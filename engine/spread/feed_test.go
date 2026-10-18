package spread

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/types"
)

// fakeREST serves the REST fallback for feed tests. Methods the feed does not use panic via the nil Client.
type fakeREST struct {
	Client
	mu         sync.Mutex
	snapshotID int64
	snapshots  int
}

func (r *fakeREST) SpotTickers(context.Context) ([]gateio.Ticker, error) {
	return []gateio.Ticker{{CurrencyPair: testKey, Last: 100, LowestAsk: 100.1, HighestBid: 100}}, nil
}

func (r *fakeREST) FuturesTickers(context.Context) ([]gateio.FuturesTicker, error) {
	return []gateio.FuturesTicker{{Contract: testKey, Last: 100, FundingRate: 0.0001}}, nil
}

func (r *fakeREST) SpotBook(context.Context, currency.Pair, uint64) (bids, asks []Level, err error) {
	return []Level{{Price: 100, Amount: 1}}, []Level{{Price: 100.1, Amount: 1}}, nil
}

func (r *fakeREST) FuturesBook(context.Context, currency.Pair, uint64) (bids, asks []Level, err error) {
	return []Level{{Price: 99.9, Amount: 10}}, []Level{{Price: 100, Amount: 10}}, nil
}

func (r *fakeREST) FuturesBookSnapshot(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, id int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots++
	bids, asks, _ = r.FuturesBook(ctx, contract, depth)
	return bids, asks, r.snapshotID, nil
}

func (r *fakeREST) setSnapshotID(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshotID = id
}

func (r *fakeREST) snapshotCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshots
}

// wsServer imitates a GateIO websocket endpoint: it acknowledges requests and pushes what the test sends.
type wsServer struct {
	srv   *httptest.Server
	mu    sync.Mutex // guards conns, reqs and all writes
	conns []*websocket.Conn
	reqs  []wsRequest
}

func newWSServer(t *testing.T) *wsServer {
	t.Helper()
	s := &wsServer{}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		for {
			var req wsRequest
			if err := c.ReadJSON(&req); err != nil {
				return
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, req)
			reply := map[string]any{"time": req.Time, "channel": req.Channel, "event": req.Event, "result": map[string]string{"status": "success"}}
			if strings.HasSuffix(req.Channel, ".ping") {
				reply = map[string]any{"time": req.Time, "channel": strings.Replace(req.Channel, "ping", "pong", 1)}
			}
			_ = c.WriteJSON(reply)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *wsServer) url() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") }

func (s *wsServer) push(t *testing.T, channel string, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.WriteJSON(map[string]any{"time": time.Now().Unix(), "channel": channel, "event": "update", "result": json.RawMessage(raw)})
	}
}

func (s *wsServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *wsServer) subscriptions(channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if r.Channel == channel && r.Event == "subscribe" {
			n++
		}
	}
	return n
}

type feedHarness struct {
	feed          *Feed
	rest          *fakeREST
	spot, futures *wsServer
	metrics       *Metrics
}

func newFeedHarness(t *testing.T) *feedHarness {
	t.Helper()
	h := &feedHarness{rest: &fakeREST{snapshotID: 100}, spot: newWSServer(t), futures: newWSServer(t)}
	h.metrics = NewMetrics(prometheus.NewRegistry())
	h.feed = newFeed(h.rest, DefaultConfig(), slog.New(slog.DiscardHandler), h.metrics, h.spot.url(), h.futures.url())
	h.feed.cooldown = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.feed.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	h.feed.Track([]string{testKey}, []string{testKey})
	require.Eventually(t, func() bool {
		return h.spot.subscriptions("spot.order_book") == 1 && h.futures.subscriptions("futures.order_book_update") == 1
	}, 5*time.Second, 5*time.Millisecond)
	return h
}

var testPair = usdtPair("ABC")

func (h *feedHarness) spotBest(t *testing.T) float64 {
	t.Helper()
	bids, _, err := h.feed.SpotBook(t.Context(), testPair, 20)
	require.NoError(t, err)
	return bids[0].Price
}

func (h *feedHarness) futuresBook(t *testing.T) (bids, asks []Level) {
	t.Helper()
	bids, asks, err := h.feed.FuturesBook(t.Context(), testPair, 20)
	require.NoError(t, err)
	return bids, asks
}

func futuresUpdateMsg(first, last int64, bids, asks [][2]float64) map[string]any {
	levels := func(in [][2]float64) []map[string]any {
		out := make([]map[string]any, len(in))
		for i, l := range in {
			out[i] = map[string]any{"p": types.Number(l[0]), "s": l[1]}
		}
		return out
	}
	return map[string]any{"t": time.Now().UnixMilli(), "s": testKey, "U": first, "u": last, "b": levels(bids), "a": levels(asks)}
}

func TestFeedSpotBookStreamsAndFallsBack(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)
	assert.Equal(t, 100.0, h.spotBest(t), "REST until the first snapshot arrives")

	h.spot.push(t, "spot.order_book", map[string]any{
		"t": time.Now().UnixMilli(), "lastUpdateId": 1, "s": testKey,
		"bids": [][2]string{{"101", "2"}}, "asks": [][2]string{{"101.1", "3"}},
	})
	require.Eventually(t, func() bool { return h.spotBest(t) == 101 }, 3*time.Second, 5*time.Millisecond)
	assert.Positive(t, testutil.ToFloat64(h.metrics.MarketData.WithLabelValues("book", "spot", "ws")))

	h.spot.dropAll()
	require.Eventually(t, func() bool { return h.spotBest(t) == 100 }, 3*time.Second, 5*time.Millisecond, "REST while disconnected")
	require.Eventually(t, func() bool { return h.spot.subscriptions("spot.order_book") >= 2 }, 5*time.Second, 10*time.Millisecond,
		"subscriptions restored after reconnecting")
}

func TestFeedFuturesBookSynchronises(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)

	// The first update triggers a REST snapshot (id 100); the update follows it and is applied on top.
	h.futures.push(t, "futures.order_book_update", futuresUpdateMsg(101, 102,
		[][2]float64{{99.95, 5}}, [][2]float64{{100, 0}, {100.05, 7}}))
	require.Eventually(t, func() bool {
		_, asks := h.futuresBook(t)
		return asks[0].Price == 100.05
	}, 3*time.Second, 5*time.Millisecond)
	bids, asks := h.futuresBook(t)
	assert.Equal(t, []Level{{Price: 99.95, Amount: 5}, {Price: 99.9, Amount: 10}}, bids)
	assert.Equal(t, []Level{{Price: 100.05, Amount: 7}}, asks, "the 100 ask was removed")
	assert.Equal(t, 1, h.rest.snapshotCount())

	h.futures.push(t, "futures.order_book_update", futuresUpdateMsg(103, 103, nil, [][2]float64{{100.05, 4}}))
	require.Eventually(t, func() bool {
		_, asks := h.futuresBook(t)
		return asks[0].Amount == 4
	}, 3*time.Second, 5*time.Millisecond, "contiguous updates apply")

	// A gap invalidates the book: REST serves it until a snapshot lines up with the stream again.
	h.futures.push(t, "futures.order_book_update", futuresUpdateMsg(110, 111, nil, [][2]float64{{100.2, 1}}))
	require.Eventually(t, func() bool {
		_, asks := h.futuresBook(t)
		return asks[0].Price == 100 && h.rest.snapshotCount() >= 2
	}, 3*time.Second, 5*time.Millisecond)

	h.rest.setSnapshotID(111)
	time.Sleep(20 * time.Millisecond) // let the resync cooldown pass
	h.futures.push(t, "futures.order_book_update", futuresUpdateMsg(112, 112, [][2]float64{{99.98, 2}}, nil))
	require.Eventually(t, func() bool {
		bids, _ := h.futuresBook(t)
		return bids[0].Price == 99.98
	}, 3*time.Second, 5*time.Millisecond, "resynchronised")
}

func TestFeedTickers(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)
	require.Eventually(t, func() bool {
		return h.spot.subscriptions("spot.tickers") == 1 && h.futures.subscriptions("futures.tickers") == 1
	}, 5*time.Second, 5*time.Millisecond)

	spot, err := h.feed.SpotTickers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 100.0, spot[0].Last.Float64(), "seeded from REST")
	_, err = h.feed.FuturesTickers(t.Context())
	require.NoError(t, err)

	h.spot.push(t, "spot.tickers", map[string]string{
		"currency_pair": testKey, "last": "101", "lowest_ask": "101.1", "highest_bid": "101", "quote_volume": "5000",
	})
	h.futures.push(t, "futures.tickers", []map[string]string{{
		"contract": testKey, "last": "99", "funding_rate": "-0.0002", "index_price": "100", "mark_price": "99.9",
		"volume_24h_quote": "1000",
	}})
	require.Eventually(t, func() bool {
		spot, _ := h.feed.SpotTickers(t.Context())
		fut, _ := h.feed.FuturesTickers(t.Context())
		return len(spot) == 1 && spot[0].Last.Float64() == 101 && len(fut) == 1 && fut[0].Last.Float64() == 99
	}, 3*time.Second, 5*time.Millisecond, "streamed updates replace the REST seed")
	fut, err := h.feed.FuturesTickers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, -0.0002, fut[0].FundingRate.Float64())
	assert.Equal(t, 100.0, fut[0].IndexPrice.Float64())
	assert.Equal(t, 1000.0, fut[0].Volume24HQuote.Float64())
}

func TestFeedReseedKeepsNewerStreamedTickers(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)
	h.feed.handleFutures(wsMessage{Channel: "futures.tickers", Result: json.RawMessage(`[{"contract":"ABC_USDT","last":"99"}]`)},
		time.Now().Add(time.Hour)) // streamed after the reseed request below starts
	fut, err := h.feed.FuturesTickers(t.Context())
	require.NoError(t, err)
	require.Len(t, fut, 1)
	assert.Equal(t, 99.0, fut[0].Last.Float64(), "the REST value (100) is older and must not replace it")
}

func TestFeedReseedsTickersInBackground(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)
	_, err := h.feed.SpotTickers(t.Context()) // initial synchronous seed
	require.NoError(t, err)
	require.Eventually(t, h.feed.spot.healthy, 3*time.Second, 5*time.Millisecond)

	h.feed.mu.Lock()
	h.feed.spotTickers.seeded = time.Now().Add(-2 * tickerReseed)
	h.feed.mu.Unlock()
	rest := testutil.ToFloat64(h.metrics.MarketData.WithLabelValues("tickers", "spot", "rest"))
	_, err = h.feed.SpotTickers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, rest, testutil.ToFloat64(h.metrics.MarketData.WithLabelValues("tickers", "spot", "rest")),
		"a stale cache is still served from the stream")
	require.Eventually(t, func() bool {
		h.feed.mu.Lock()
		defer h.feed.mu.Unlock()
		return time.Since(h.feed.spotTickers.seeded) < time.Minute && !h.feed.spotTickers.seeding
	}, 3*time.Second, 5*time.Millisecond, "reseeded in the background")
}

func TestFeedStopsStreamingUntrackedBooks(t *testing.T) {
	t.Parallel()
	h := newFeedHarness(t)
	h.feed.Track([]string{testKey}, nil)
	require.Eventually(t, func() bool {
		h.spot.mu.Lock()
		defer h.spot.mu.Unlock()
		for _, r := range h.spot.reqs {
			if r.Channel == "spot.order_book" && r.Event == "unsubscribe" {
				return true
			}
		}
		return false
	}, 3*time.Second, 5*time.Millisecond)
	h.spot.push(t, "spot.order_book", map[string]any{"s": testKey, "bids": [][2]string{{"105", "1"}}, "asks": [][2]string{{"105.1", "1"}}})
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 100.0, h.spotBest(t), "untracked books are served from REST")
}

func TestSyncBook(t *testing.T) {
	t.Parallel()
	b := newSyncBook()
	b.pending = []futuresUpdate{
		{first: 90, last: 95, bids: []Level{{Price: 1, Amount: 1}}}, // already in the snapshot
		{first: 96, last: 101, asks: []Level{{Price: 3, Amount: 2}}},
	}
	b.load([]Level{{Price: 2, Amount: 1}}, []Level{{Price: 4, Amount: 1}}, 100)
	require.True(t, b.synced)
	bids, asks := b.levels(10)
	assert.Equal(t, []Level{{Price: 2, Amount: 1}}, bids, "stale update ignored")
	assert.Equal(t, []Level{{Price: 3, Amount: 2}, {Price: 4, Amount: 1}}, asks)

	assert.ErrorIs(t, b.apply(futuresUpdate{first: 103, last: 104}), errBookGap)
	require.NoError(t, b.apply(futuresUpdate{first: 102, last: 102, asks: []Level{{Price: 3, Amount: 0}}}))
	_, asks = b.levels(10)
	assert.Equal(t, []Level{{Price: 4, Amount: 1}}, asks)

	gap := newSyncBook()
	gap.pending = []futuresUpdate{{first: 105, last: 106}}
	gap.load(nil, nil, 100)
	assert.False(t, gap.synced, "a snapshot older than the buffered stream cannot be used")

	for i := range maxBookLevels + 50 {
		b.bids[float64(i)] = 1
	}
	b.prune()
	assert.Len(t, b.bids, maxBookLevels)
	top, _ := b.levels(1)
	assert.Equal(t, float64(maxBookLevels+49), top[0].Price, "pruning keeps the best levels")
}

func TestWSLevels(t *testing.T) {
	t.Parallel()
	spot, futures := wsLevels(20)
	assert.Equal(t, "20", spot)
	assert.Equal(t, "20", futures)
	spot, futures = wsLevels(8)
	assert.Equal(t, "10", spot)
	assert.Equal(t, "20", futures)
}
