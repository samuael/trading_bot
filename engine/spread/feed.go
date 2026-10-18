package spread

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/exchanges/orderbook"
	"github.com/thrasher-corp/gocryptotrader/types"
)

// MarketData serves tickers and orderbooks to the engine.
type MarketData interface {
	SpotTickers(ctx context.Context) ([]gateio.Ticker, error)
	FuturesTickers(ctx context.Context) ([]gateio.FuturesTicker, error)
	SpotBook(ctx context.Context, pair currency.Pair, depth uint64) (bids, asks []Level, err error)
	FuturesBook(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, err error)
	// Track names the instruments to stream tickers for and the subset to stream orderbooks for.
	Track(universe, books []string)
}

// restMarket serves market data straight from REST.
type restMarket struct{ Client }

func (restMarket) Track(_, _ []string) {}

const (
	spotWebsocketURL    = "wss://api.gateio.ws/ws/v4/"
	futuresWebsocketURL = "wss://fx-ws.gateio.ws/v4/ws/usdt"

	// tickerReseed refreshes the streamed ticker cache from REST in the background, covering markets that rarely
	// push updates.
	tickerReseed = time.Minute
	// tickersPerSubscription chunks the ticker subscriptions for the whole universe.
	tickersPerSubscription = 100
	// resyncCooldown spaces REST snapshot requests for a futures book that keeps losing sync.
	resyncCooldown = 2 * time.Second
	// maxPendingUpdates bounds the updates buffered while a futures book waits for its snapshot.
	maxPendingUpdates = 2000
	// maxBookLevels bounds each side of an incrementally maintained book.
	maxBookLevels = 200
)

var errBookGap = errors.New("futures orderbook update sequence gap")

// Feed streams GateIO tickers and orderbooks over websockets and falls back to REST for any request the streams
// cannot serve: a connection is down, a book is not yet synchronised or has gone quiet for too long.
//
// Spot books use the limited-level snapshot channel, so every push replaces the book. Futures books use the
// incremental channel, synchronised against a REST snapshot by update ID and rebuilt whenever a gap appears.
// IOC limit orders protect against acting on a stale price either way: a stale quote simply fails to fill.
type Feed struct {
	rest    Client
	cfg     Config
	log     *slog.Logger
	metrics *Metrics
	spot    *wsConn
	futures *wsConn
	resync  chan string
	// cooldown spaces REST snapshot requests for a futures book that keeps losing sync.
	cooldown time.Duration

	mu             sync.Mutex
	universe       []string
	watched        map[string]bool
	ctx            context.Context // Run's context, for background work
	spotTickers    tickerCache[gateio.Ticker]
	futuresTickers tickerCache[gateio.FuturesTicker]
	spotBooks      map[string]*snapshotBook
	futuresBooks   map[string]*syncBook
}

// stamped records when a cached value was received, so a REST reseed never overwrites a newer streamed value.
type stamped[T any] struct {
	v  T
	at time.Time
}

func values[T any](m map[string]stamped[T]) []T {
	out := make([]T, 0, len(m))
	for _, s := range m {
		out = append(out, s.v)
	}
	return out
}

type snapshotBook struct {
	bids, asks []Level
	at         time.Time
}

// syncBook is an incrementally maintained futures book. Amounts are in contracts.
type syncBook struct {
	bids, asks  map[float64]float64
	lastID      int64
	synced      bool
	syncing     bool
	scheduled   bool // a resync is waiting out the cooldown
	lastAttempt time.Time
	pending     []futuresUpdate
	at          time.Time
}

type futuresUpdate struct {
	first, last int64
	bids, asks  []Level
}

// NewFeed creates a feed for the production GateIO endpoints. Call Run to start streaming.
func NewFeed(rest Client, cfg Config, logger *slog.Logger, metrics *Metrics) *Feed {
	return newFeed(rest, cfg, logger, metrics, spotWebsocketURL, futuresWebsocketURL)
}

func newFeed(rest Client, cfg Config, logger *slog.Logger, metrics *Metrics, spotURL, futuresURL string) *Feed {
	f := &Feed{
		rest:           rest,
		cfg:            cfg,
		log:            logger,
		metrics:        metrics,
		resync:         make(chan string, 256),
		cooldown:       resyncCooldown,
		watched:        make(map[string]bool),
		spotTickers:    newTickerCache[gateio.Ticker](),
		futuresTickers: newTickerCache[gateio.FuturesTicker](),
		spotBooks:      make(map[string]*snapshotBook),
		futuresBooks:   make(map[string]*syncBook),
	}
	f.spot = newWSConn("spot", spotURL, "spot.ping", logger, f.handleSpot, func(up bool) { f.connState("spot", up) })
	f.futures = newWSConn("futures", futuresURL, "futures.ping", logger, f.handleFutures, func(up bool) { f.connState("futures", up) })
	return f
}

// Run streams until ctx is cancelled.
func (f *Feed) Run(ctx context.Context) {
	f.mu.Lock()
	f.ctx = ctx
	f.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); f.spot.run(ctx) }()
	go func() { defer wg.Done(); f.futures.run(ctx) }()
	go func() { defer wg.Done(); f.syncWorker(ctx) }()
	wg.Wait()
}

func (f *Feed) connState(name string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	f.metrics.WSConnected.WithLabelValues(name).Set(v)
	if up {
		return
	}
	// Updates are lost while disconnected, so streamed books must be rebuilt.
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "spot" {
		for key := range f.spotBooks {
			f.spotBooks[key] = &snapshotBook{}
		}
		return
	}
	for key := range f.futuresBooks {
		f.futuresBooks[key] = newSyncBook()
	}
}

func newSyncBook() *syncBook {
	return &syncBook{bids: make(map[float64]float64), asks: make(map[float64]float64)}
}

// Track sets the instruments streamed. Orderbooks are only streamed for books, typically held positions and the
// current entry candidates; tickers are streamed for the whole universe.
func (f *Feed) Track(universe, books []string) {
	universe = slices.Sorted(slices.Values(universe))
	books = slices.Compact(slices.Sorted(slices.Values(books)))

	f.mu.Lock()
	f.universe = universe
	watched := make(map[string]bool, len(books))
	for _, key := range books {
		watched[key] = true
		if f.spotBooks[key] == nil {
			f.spotBooks[key] = &snapshotBook{}
		}
		if f.futuresBooks[key] == nil {
			f.futuresBooks[key] = newSyncBook()
		}
	}
	for key := range f.watched {
		if !watched[key] {
			delete(f.spotBooks, key)
			delete(f.futuresBooks, key)
		}
	}
	f.watched = watched
	f.mu.Unlock()

	spotLevels, futuresLevels := wsLevels(f.cfg.OrderbookDepth)
	var spotSubs, futuresSubs []wsSub
	for chunk := range slices.Chunk(universe, tickersPerSubscription) {
		spotSubs = append(spotSubs, wsSub{Channel: "spot.tickers", Payload: chunk})
		futuresSubs = append(futuresSubs, wsSub{Channel: "futures.tickers", Payload: chunk})
	}
	for _, key := range books {
		spotSubs = append(spotSubs, wsSub{Channel: "spot.order_book", Payload: []string{key, spotLevels, "100ms"}})
		futuresSubs = append(futuresSubs, wsSub{Channel: "futures.order_book_update", Payload: []string{key, "100ms", futuresLevels}})
	}
	f.spot.set(spotSubs)
	f.futures.set(futuresSubs)
}

// wsLevels picks the smallest streamed depth GateIO offers that covers depth.
func wsLevels(depth uint64) (spot, futures string) {
	spot, futures = "100", "100"
	for _, l := range []uint64{5, 10, 20, 50, 100} {
		if l >= depth {
			spot = strconv.FormatUint(l, 10)
			break
		}
	}
	for _, l := range []uint64{20, 50, 100} {
		if l >= depth {
			futures = strconv.FormatUint(l, 10)
			break
		}
	}
	return spot, futures
}

func (f *Feed) count(kind, market, source string) {
	f.metrics.MarketData.WithLabelValues(kind, market, source).Inc()
}

func (f *Feed) bookMaxAge() time.Duration {
	return time.Duration(f.cfg.WSBookMaxAgeSeconds) * time.Second
}

// SpotTickers serves streamed tickers. It blocks on REST only while the stream is down or has nothing cached;
// otherwise a background REST reseed every tickerReseed covers markets that rarely push updates.
func (f *Feed) SpotTickers(ctx context.Context) ([]gateio.Ticker, error) {
	return serveTickers(ctx, f, f.spot, &f.spotTickers, f.rest.SpotTickers,
		func(t gateio.Ticker) string { return strings.ToUpper(t.CurrencyPair) }, "spot")
}

// FuturesTickers serves streamed tickers, as SpotTickers.
func (f *Feed) FuturesTickers(ctx context.Context) ([]gateio.FuturesTicker, error) {
	return serveTickers(ctx, f, f.futures, &f.futuresTickers, f.rest.FuturesTickers,
		func(t gateio.FuturesTicker) string { return strings.ToUpper(t.Contract) }, "futures")
}

// tickerCache holds streamed tickers by instrument, plus REST reseed bookkeeping.
type tickerCache[T any] struct {
	items   map[string]stamped[T]
	seeded  time.Time
	seeding bool
}

func newTickerCache[T any]() tickerCache[T] {
	return tickerCache[T]{items: make(map[string]stamped[T])}
}

func serveTickers[T any](ctx context.Context, f *Feed, conn *wsConn, cache *tickerCache[T], fetch func(context.Context) ([]T, error), key func(T) string, market string) ([]T, error) {
	f.mu.Lock()
	if conn.healthy() && len(cache.items) > 0 {
		out := values(cache.items)
		if time.Since(cache.seeded) >= tickerReseed && !cache.seeding {
			cache.seeding = true
			go func() {
				reseedCtx, cancel := context.WithTimeout(f.runContext(), tickerReseed)
				defer cancel()
				if _, err := reseedTickers(reseedCtx, f, cache, fetch, key); err != nil {
					f.log.Debug("ticker reseed", "market", market, "err", err)
				}
			}()
		}
		f.mu.Unlock()
		f.count("tickers", market, "ws")
		return out, nil
	}
	f.mu.Unlock()
	f.count("tickers", market, "rest")
	return reseedTickers(ctx, f, cache, fetch, key)
}

// reseedTickers merges a REST snapshot into the cache without overwriting anything streamed after the request.
func reseedTickers[T any](ctx context.Context, f *Feed, cache *tickerCache[T], fetch func(context.Context) ([]T, error), key func(T) string) ([]T, error) {
	requested := time.Now()
	tickers, err := fetch(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	cache.seeding = false
	if err != nil {
		return nil, err
	}
	for _, t := range tickers {
		k := key(t)
		if cache.items[k].at.Before(requested) {
			cache.items[k] = stamped[T]{v: t, at: requested}
		}
	}
	cache.seeded = requested
	return values(cache.items), nil
}

func (f *Feed) runContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

// SpotBook serves the streamed snapshot when it is live and fresh, otherwise REST.
func (f *Feed) SpotBook(ctx context.Context, pair currency.Pair, depth uint64) (bids, asks []Level, err error) {
	f.mu.Lock()
	b := f.spotBooks[pair.String()]
	if f.spot.healthy() && b != nil && !b.at.IsZero() && time.Since(b.at) <= f.bookMaxAge() {
		bids, asks = topLevels(b.bids, depth), topLevels(b.asks, depth)
		f.mu.Unlock()
		f.count("book", "spot", "ws")
		return bids, asks, nil
	}
	f.mu.Unlock()
	f.count("book", "spot", "rest")
	return f.rest.SpotBook(ctx, pair, depth)
}

// FuturesBook serves the streamed book when it is synchronised, live and fresh, otherwise REST.
func (f *Feed) FuturesBook(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, err error) {
	f.mu.Lock()
	b := f.futuresBooks[contract.String()]
	if f.futures.healthy() && b != nil && b.synced && time.Since(b.at) <= f.bookMaxAge() {
		bids, asks = b.levels(depth)
		f.mu.Unlock()
		f.count("book", "futures", "ws")
		return bids, asks, nil
	}
	f.mu.Unlock()
	f.count("book", "futures", "rest")
	return f.rest.FuturesBook(ctx, contract, depth)
}

func topLevels(levels []Level, depth uint64) []Level {
	return slices.Clone(levels[:min(len(levels), int(depth))])
}

func (f *Feed) handleSpot(m wsMessage, received time.Time) {
	switch m.Channel {
	case "spot.tickers":
		var t gateio.WsTicker
		if err := json.Unmarshal(m.Result, &t); err != nil {
			f.log.Debug("spot ticker", "err", err)
			return
		}
		key := strings.ToUpper(t.CurrencyPair.String())
		f.mu.Lock()
		f.spotTickers.items[key] = stamped[gateio.Ticker]{at: received, v: gateio.Ticker{
			CurrencyPair: key,
			Last:         t.Last,
			LowestAsk:    t.LowestAsk,
			HighestBid:   t.HighestBid,
			BaseVolume:   t.BaseVolume,
			QuoteVolume:  t.QuoteVolume,
		}}
		f.mu.Unlock()
	case "spot.order_book":
		var s gateio.WsOrderbookSnapshot
		if err := json.Unmarshal(m.Result, &s); err != nil {
			f.log.Debug("spot orderbook", "err", err)
			return
		}
		key := strings.ToUpper(s.CurrencyPair.String())
		f.mu.Lock()
		if b := f.spotBooks[key]; b != nil {
			b.bids, b.asks, b.at = convertLevels(s.Bids), convertLevels(s.Asks), received
		}
		f.mu.Unlock()
	}
}

func convertLevels(in orderbook.LevelsArrayPriceAmount) []Level {
	out := make([]Level, len(in))
	for i, l := range in {
		out[i] = Level{Price: l.Price, Amount: l.Amount}
	}
	return out
}

func (f *Feed) handleFutures(m wsMessage, received time.Time) {
	switch m.Channel {
	case "futures.tickers":
		var ts []gateio.WsFutureTicker
		if err := json.Unmarshal(m.Result, &ts); err != nil {
			f.log.Debug("futures ticker", "err", err)
			return
		}
		f.mu.Lock()
		for _, t := range ts {
			key := strings.ToUpper(t.Contract.String())
			funding, _ := strconv.ParseFloat(t.FundingRate, 64)
			settleVolume, _ := strconv.ParseFloat(t.Volume24HSettle, 64)
			f.futuresTickers.items[key] = stamped[gateio.FuturesTicker]{at: received, v: gateio.FuturesTicker{
				Contract:        key,
				Last:            t.Last,
				MarkPrice:       t.MarkPrice,
				IndexPrice:      t.IndexPrice,
				FundingRate:     types.Number(funding),
				Volume24HQuote:  t.Volume24HQuote,
				Volume24HSettle: types.Number(settleVolume),
			}}
		}
		f.mu.Unlock()
	case "futures.order_book_update":
		var u gateio.WsFuturesAndOptionsOrderbookUpdate
		if err := json.Unmarshal(m.Result, &u); err != nil {
			f.log.Debug("futures orderbook", "err", err)
			return
		}
		f.onFuturesUpdate(strings.ToUpper(u.ContractName.String()), futuresUpdate{
			first: u.FirstUpdatedID,
			last:  u.LastUpdatedID,
			bids:  futuresLevels(u.Bids),
			asks:  futuresLevels(u.Asks),
		}, received)
	}
}

func futuresLevels(in []gateio.Level) []Level {
	out := make([]Level, len(in))
	for i, l := range in {
		out[i] = Level{Price: l.Price.Float64(), Amount: l.Size}
	}
	return out
}

func (f *Feed) onFuturesUpdate(key string, u futuresUpdate, received time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.futuresBooks[key]
	if b == nil {
		return
	}
	b.at = received
	if b.synced {
		if err := b.apply(u); err == nil {
			return
		}
		f.log.Debug("futures book lost sync, resynchronising", "contract", key)
		b.synced = false
		b.pending = nil
	}
	b.pending = append(b.pending, u)
	if len(b.pending) > maxPendingUpdates {
		b.pending = b.pending[len(b.pending)-maxPendingUpdates:]
	}
	f.requestResync(key, b)
}

// requestResync queues a REST snapshot for b, or schedules one for when the cooldown since the last attempt ends.
// Callers hold f.mu.
func (f *Feed) requestResync(key string, b *syncBook) {
	if b.syncing || b.scheduled {
		return
	}
	if wait := f.cooldown - time.Since(b.lastAttempt); wait > 0 {
		b.scheduled = true
		time.AfterFunc(wait, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			b.scheduled = false
			if f.futuresBooks[key] == b && !b.synced {
				f.requestResync(key, b)
			}
		})
		return
	}
	select {
	case f.resync <- key:
		b.syncing = true
		b.lastAttempt = time.Now()
	default:
	}
}

// syncWorker fetches REST snapshots for futures books that need one and replays buffered updates onto them.
func (f *Feed) syncWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-f.resync:
			contract, err := currency.NewPairDelimiter(key, currency.UnderscoreDelimiter)
			if err != nil {
				continue
			}
			bids, asks, id, err := f.rest.FuturesBookSnapshot(ctx, contract, f.cfg.OrderbookDepth)
			f.mu.Lock()
			if b := f.futuresBooks[key]; b != nil {
				b.syncing = false
				if err != nil {
					f.log.Debug("futures book snapshot", "contract", key, "err", err)
				} else {
					b.load(bids, asks, id)
				}
			}
			f.mu.Unlock()
		}
	}
}

// load installs a snapshot and replays the buffered updates that follow it.
func (b *syncBook) load(bids, asks []Level, id int64) {
	b.bids, b.asks = make(map[float64]float64, len(bids)), make(map[float64]float64, len(asks))
	for _, l := range bids {
		b.bids[l.Price] = l.Amount
	}
	for _, l := range asks {
		b.asks[l.Price] = l.Amount
	}
	b.lastID = id
	pending := b.pending
	b.pending = nil
	for _, u := range pending {
		if err := b.apply(u); err != nil {
			b.synced = false
			return
		}
	}
	b.synced = true
}

// apply adds one update. Updates already covered by the book are ignored; one that skips IDs is a gap.
func (b *syncBook) apply(u futuresUpdate) error {
	if u.last <= b.lastID {
		return nil
	}
	if u.first > b.lastID+1 {
		return errBookGap
	}
	set := func(side map[float64]float64, levels []Level) {
		for _, l := range levels {
			if l.Amount == 0 {
				delete(side, l.Price)
			} else {
				side[l.Price] = l.Amount
			}
		}
	}
	set(b.bids, u.bids)
	set(b.asks, u.asks)
	b.lastID = u.last
	b.prune()
	return nil
}

// prune drops levels far from the touch that the limited-depth stream no longer maintains.
func (b *syncBook) prune() {
	if len(b.bids) > maxBookLevels {
		bids, _ := b.levels(maxBookLevels)
		b.bids = make(map[float64]float64, len(bids))
		for _, l := range bids {
			b.bids[l.Price] = l.Amount
		}
	}
	if len(b.asks) > maxBookLevels {
		_, asks := b.levels(maxBookLevels)
		b.asks = make(map[float64]float64, len(asks))
		for _, l := range asks {
			b.asks[l.Price] = l.Amount
		}
	}
}

// levels returns the best depth levels per side: bids descending, asks ascending.
func (b *syncBook) levels(depth uint64) (bids, asks []Level) {
	collect := func(side map[float64]float64, desc bool) []Level {
		out := make([]Level, 0, len(side))
		for p, a := range side {
			out = append(out, Level{Price: p, Amount: a})
		}
		slices.SortFunc(out, func(x, y Level) int {
			if desc {
				return cmp.Compare(y.Price, x.Price)
			}
			return cmp.Compare(x.Price, y.Price)
		})
		return out[:min(len(out), int(depth))]
	}
	return collect(b.bids, true), collect(b.asks, false)
}
