package spread

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/thrasher-corp/gocryptotrader/config"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

// capitalHeadroom keeps 2% of usable funds back for fees and price moves between sizing and filling.
const capitalHeadroom = 0.98

// Setup loads the GateIO section of a gocryptotrader config into the exchange.
func Setup(e exchange.IBotExchange, configPath string) error {
	cfg := &config.Config{}
	err := cfg.LoadConfig(configPath, true)
	if err != nil {
		return fmt.Errorf("LoadConfig() error: %w", err)
	}
	e.SetDefaults()

	eName := "GateIO"
	exchConf, err := cfg.GetExchangeConfig(eName)
	if err != nil {
		return fmt.Errorf("GetExchangeConfig(%q) error: %w", eName, err)
	}
	err = e.Setup(exchConf)
	if err != nil {
		return fmt.Errorf("Setup() error: %w", err)
	}
	return nil
}

// Engine runs the spot/perpetual spread strategy. When the perp trades at least MinEntrySpread above spot it buys
// spot and shorts the perp (carry); when it trades that far below, it sells spot borrowed on isolated margin and
// longs the perp (reverse). Entries must also clear MinNetProfit after fees and expected interest. Both legs unwind
// once the spread falls to ExitSpread. All state is owned by the Run goroutine.
type Engine struct {
	cfg        Config
	client     Client
	market     MarketData
	log        *slog.Logger
	metrics    *Metrics
	balanceLog *log.Logger
	store      Store
	now        func() time.Time

	universe     map[string]Instrument
	universeKeys []string
	positions    map[string]*Position
	blocked      map[string]bool
	fees         map[string]cachedFees
	borrowRates  map[string]cachedRate
	leverageSet  map[string]bool
	spotBids     map[string]float64
}

type cachedFees struct {
	fees Fees
	at   time.Time
}

type cachedRate struct {
	rate float64
	at   time.Time
}

// NewEngine restores persisted positions and prepares the engine. market may be nil to serve market data from
// REST; balanceLog may be nil.
func NewEngine(cfg Config, client Client, market MarketData, logger *slog.Logger, metrics *Metrics, balanceLog *log.Logger) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if market == nil {
		market = restMarket{client}
	}
	store := Store{path: cfg.StateFile}
	positions, err := store.LoadPositions()
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	return &Engine{
		cfg:         cfg,
		client:      client,
		market:      market,
		log:         logger,
		metrics:     metrics,
		balanceLog:  balanceLog,
		store:       store,
		now:         time.Now,
		universe:    make(map[string]Instrument),
		positions:   positions,
		blocked:     make(map[string]bool),
		fees:        make(map[string]cachedFees),
		borrowRates: make(map[string]cachedRate),
		leverageSet: make(map[string]bool),
		spotBids:    make(map[string]float64),
	}, nil
}

// Run trades until ctx is cancelled. Errors inside a tick are logged and retried on the next tick; only startup
// failures are returned.
func (e *Engine) Run(ctx context.Context) error {
	if !e.cfg.DryRun {
		if err := e.client.EnsureSinglePositionMode(ctx); err != nil {
			return fmt.Errorf("position mode: %w", err)
		}
	}
	// The instrument lists are several MB; keep retrying through a slow or flaky connection.
	for backoff := 5 * time.Second; ; backoff = min(backoff*2, 2*time.Minute) {
		err := e.refreshUniverse(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.log.Warn("load instruments failed, retrying", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	e.log.Info("strategy started",
		"dry_run", e.cfg.DryRun,
		"instruments", len(e.universe),
		"restored_positions", len(e.positions),
		"min_entry_spread", e.cfg.MinEntrySpread,
		"exit_spread", e.cfg.ExitSpread,
		"min_net_profit", e.cfg.MinNetProfit,
		"max_open_positions", e.cfg.MaxOpenPositions,
		"reverse", e.cfg.EnableReverse,
		"websocket", e.cfg.UseWebsocket,
	)

	tick := time.NewTicker(time.Duration(e.cfg.TickIntervalSeconds) * time.Second)
	universe := time.NewTicker(minutes(e.cfg.UniverseRefreshMinutes))
	balances := time.NewTicker(minutes(e.cfg.BalanceLogMinutes))
	defer tick.Stop()
	defer universe.Stop()
	defer balances.Stop()

	e.tick(ctx)
	e.logBalances(ctx)
	for {
		select {
		case <-ctx.Done():
			e.save()
			e.log.Info("strategy stopped", "open_positions", len(e.positions))
			return ctx.Err()
		case <-tick.C:
			e.tick(ctx)
		case <-universe.C:
			if err := e.refreshUniverse(ctx); err != nil {
				e.log.Error("refresh instruments", "err", err)
			}
		case <-balances.C:
			e.logBalances(ctx)
		}
	}
}

func (e *Engine) tick(ctx context.Context) {
	if err := e.monitor(ctx); err != nil && !errors.Is(err, context.Canceled) {
		e.log.Error("monitor positions", "err", err)
	}
	candidates, err := e.scan(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		e.log.Error("scan for entries", "err", err)
	}
	// Stream books for what is held and what may be entered next.
	books := slices.Collect(maps.Keys(e.positions))
	for _, c := range candidates {
		books = append(books, c.inst.Key())
	}
	e.market.Track(e.universeKeys, books)
	e.metrics.OpenPositions.Set(float64(len(e.positions)))
}

func (e *Engine) refreshUniverse(ctx context.Context) error {
	spotPairs, err := e.client.SpotPairs(ctx)
	if err != nil {
		return err
	}
	contracts, err := e.client.Contracts(ctx)
	if err != nil {
		return err
	}
	var marginPairs []MarginPair
	if e.cfg.EnableReverse {
		if marginPairs, err = e.client.MarginPairs(ctx); err != nil {
			e.log.Warn("margin pairs unavailable, reverse entries paused until the next refresh", "err", err)
		}
	}
	universe := buildUniverse(spotPairs, contracts, marginPairs)
	if len(universe) == 0 {
		return errors.New("no tradable spot/perpetual pairs found")
	}
	e.universe = universe
	e.universeKeys = slices.Sorted(maps.Keys(universe))
	margin := 0
	for _, inst := range universe {
		if inst.MarginEnabled {
			margin++
		}
	}
	e.log.Info("instruments refreshed", "count", len(universe), "margin_enabled", margin)
	e.market.Track(e.universeKeys, slices.Collect(maps.Keys(e.positions)))
	return nil
}

// monitor reconciles held positions with the exchange and closes those whose spread has converged.
func (e *Engine) monitor(ctx context.Context) error {
	exPositions, err := e.client.Positions(ctx)
	if err != nil {
		return err
	}
	onExchange := make(map[string]gateio.Position, len(exPositions))
	for _, p := range exPositions {
		if p.Size != 0 {
			onExchange[strings.ToUpper(p.Contract)] = p
		}
	}

	// Perp positions this engine does not own are left alone, and their instruments are not traded.
	blocked := make(map[string]bool)
	for key := range onExchange {
		if _, ok := e.positions[key]; ok {
			continue
		}
		blocked[key] = true
		if !e.blocked[key] {
			e.log.Warn("unmanaged perp position on exchange, instrument excluded from trading", "contract", key, "size", onExchange[key].Size)
		}
	}
	e.blocked = blocked

	var errs []error
	for _, key := range slices.Sorted(maps.Keys(e.positions)) {
		ex, ok := onExchange[key]
		if err := e.manage(ctx, e.positions[key], ex, ok); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) manage(ctx context.Context, pos *Position, ex gateio.Position, onExchange bool) error {
	key := pos.Inst.Key()
	dir := pos.Dir()
	var reason string
	if pos.OpenContracts() > epsilon {
		switch {
		case !onExchange:
			// The perp leg is gone: liquidated, deleveraged or closed by hand. Its fills are unknown, so futures
			// P&L is recorded as flat and the now unhedged spot leg is unwound.
			e.log.Error("perp leg missing on exchange, unwinding spot leg", "instrument", key, "expected_contracts", pos.OpenContracts())
			if dir == Carry {
				pos.ContractsBought, pos.FutBuyNotional = pos.ContractsSold, pos.FutSellNotional
			} else {
				pos.ContractsSold, pos.FutSellNotional = pos.ContractsBought, pos.FutBuyNotional
			}
			pos.State = stateClosing
			pos.CloseReason = "perp leg closed externally; futures P&L not captured"
			e.save()
		default:
			pos.Funding = ex.RealisedPNLFundingFee.Float64()
			if held := math.Abs(ex.Size); math.Abs(held-pos.OpenContracts()) > epsilon {
				e.log.Warn("perp size differs from records", "instrument", key, "exchange", held, "recorded", pos.OpenContracts())
			}
			if liq, mark := ex.LiquidationPrice.Float64(), ex.MarkPrice.Float64(); liq > 0 && mark > 0 {
				distance := (liq - mark) / mark // a short liquidates above the mark
				if dir == Reverse {
					distance = (mark - liq) / mark
				}
				if distance < e.cfg.LiquidationBuffer {
					reason = "liquidation guard"
				}
			}
		}
	}
	if dir == Reverse {
		acc, err := e.refreshMargin(ctx, pos)
		if err != nil {
			return fmt.Errorf("margin account: %w", err)
		}
		if acc.Risk > 0 && acc.Risk < e.cfg.MarginRiskFloor {
			e.log.Warn("margin account risk below floor", "instrument", key, "risk", acc.Risk)
			reason = "margin risk guard"
		}
	}

	if pos.State == stateOpen && pos.OpenContracts()-pos.HedgedContracts() > epsilon && !e.cfg.DryRun {
		if err := e.trimHedge(ctx, pos, e.cachedFees(pos.Inst)); err != nil {
			return err
		}
		if _, ok := e.positions[key]; !ok {
			return nil
		}
	}

	spotBids, spotAsks, err := e.market.SpotBook(ctx, pos.Inst.SpotPair, e.cfg.OrderbookDepth)
	if err != nil {
		return err
	}
	var futuresBids, futuresAsks []Level
	if pos.OpenContracts() > epsilon {
		if futuresBids, futuresAsks, err = e.market.FuturesBook(ctx, pos.Inst.Contract, e.cfg.OrderbookDepth); err != nil {
			return err
		}
	}
	spotSide, futuresSide, spotQty := spotBids, futuresAsks, pos.SpotExposure()
	if dir == Reverse {
		spotSide, futuresSide, spotQty = spotAsks, futuresBids, grossSpotQty(max(0, pos.SpotExposure()), e.cachedFees(pos.Inst), pos.Inst)
	}
	plan, err := planExit(dir, pos.Inst, pos.OpenContracts(), spotQty, spotSide, futuresSide, e.cfg)
	if err != nil {
		return err
	}
	if plan.Contracts > 0 {
		e.metrics.Basis.WithLabelValues(key).Set(plan.Basis)
	}

	switch {
	case pos.State == stateClosing:
		reason = pos.CloseReason
	case reason != "":
	case plan.Contracts <= epsilon:
		reason = "no perp leg"
	case plan.Basis <= e.cfg.ExitSpread:
		reason = "spread converged"
	default:
		return nil
	}
	return e.close(ctx, pos, plan, reason)
}

// scan shortlists instruments from tickers, prices them on the orderbooks and opens those that qualify. It
// returns the shortlisted candidates so their books can be streamed.
func (e *Engine) scan(ctx context.Context) ([]candidate, error) {
	spotTickers, err := e.market.SpotTickers(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range spotTickers {
		e.spotBids[strings.ToUpper(t.CurrencyPair)] = t.HighestBid.Float64()
	}
	slots := e.cfg.MaxOpenPositions - len(e.positions)
	if slots <= 0 {
		return nil, nil
	}
	futuresTickers, err := e.market.FuturesTickers(ctx)
	if err != nil {
		return nil, err
	}
	candidates := rankCandidates(e.universe, spotTickers, futuresTickers, e.cfg, func(key string) bool {
		_, held := e.positions[key]
		return held || e.blocked[key]
	})
	if len(candidates) == 0 {
		return nil, nil
	}

	funds, err := e.freeFunds(ctx)
	if err != nil {
		return candidates, err
	}
	for _, c := range candidates {
		if slots == 0 {
			break
		}
		key := c.inst.Key()
		budget := tradeBudget(funds.usable(c.dir, e.cfg), slots, e.cfg)
		if budget == 0 {
			e.log.Debug("insufficient funds for another position", "instrument", key, "direction", c.dir)
			continue
		}
		plan, err := e.evaluate(ctx, c, budget)
		if err != nil {
			e.log.Debug("candidate rejected", "instrument", key, "direction", c.dir, "ticker_spread", c.tickerBasis, "reason", err)
			continue
		}
		if e.cfg.DryRun {
			e.log.Info("dry run: would open",
				"instrument", key, "direction", plan.Direction, "contracts", plan.Contracts, "spot_qty", plan.SpotQty,
				"notional", plan.Notional, "spread", plan.Basis, "round_trip_fees", plan.Fees.RoundTrip(),
				"interest", plan.InterestCost, "expected_net", plan.ExpectedNet)
			slots--
			funds.spend(plan.Direction, plan.Notional, e.cfg)
			continue
		}
		pos, err := e.open(ctx, plan)
		if err != nil {
			e.log.Error("open position", "instrument", key, "direction", c.dir, "err", err)
			continue
		}
		if pos != nil {
			slots--
			funds.spend(plan.Direction, max(pos.SpotCost, pos.SpotProceeds), e.cfg)
		}
	}
	return candidates, nil
}

func (e *Engine) evaluate(ctx context.Context, c candidate, budget float64) (EntryPlan, error) {
	inst := c.inst
	fees, err := e.feesFor(ctx, inst)
	if err != nil {
		return EntryPlan{}, fmt.Errorf("fees: %w", err)
	}
	var rate float64
	if c.dir == Reverse {
		if rate, err = e.borrowRate(ctx, inst.Base); err != nil {
			return EntryPlan{}, fmt.Errorf("borrow rate: %w", err)
		}
	}
	spotBids, spotAsks, err := e.market.SpotBook(ctx, inst.SpotPair, e.cfg.OrderbookDepth)
	if err != nil {
		return EntryPlan{}, err
	}
	futuresBids, futuresAsks, err := e.market.FuturesBook(ctx, inst.Contract, e.cfg.OrderbookDepth)
	if err != nil {
		return EntryPlan{}, err
	}
	if c.dir == Reverse {
		return planEntry(Reverse, inst, spotBids, futuresAsks, fees, rate, budget, e.cfg)
	}
	return planEntry(Carry, inst, spotAsks, futuresBids, fees, 0, budget, e.cfg)
}

// funds tracks free USDT in the spot and futures accounts during one scan.
type funds struct {
	spot, futures float64
}

// usable is the notional a new position in dir can be funded with. Both directions hold 1x perp margin plus the
// reserve; carry pays for the coins from spot, reverse only posts margin collateral from spot.
func (f funds) usable(dir Direction, cfg Config) float64 {
	spotNeed := 1.0
	if dir == Reverse {
		spotNeed = cfg.MarginCollateralRatio
	}
	return max(0, min(f.spot/spotNeed, f.futures*(1-cfg.FuturesMarginReserve))*capitalHeadroom)
}

func (f *funds) spend(dir Direction, notional float64, cfg Config) {
	if dir == Reverse {
		f.spot -= notional * cfg.MarginCollateralRatio
	} else {
		f.spot -= notional
	}
	f.futures -= notional
}

// freeFunds returns the free USDT of both accounts, rebalancing them first if enabled.
func (e *Engine) freeFunds(ctx context.Context) (funds, error) {
	spotFree, _, err := e.client.SpotBalance(ctx, settlement)
	if err != nil {
		return funds{}, fmt.Errorf("spot balance: %w", err)
	}
	acc, err := e.client.FuturesAccount(ctx)
	if err != nil {
		return funds{}, fmt.Errorf("futures account: %w", err)
	}
	f := funds{spot: spotFree, futures: acc.Available.Float64()}

	if e.cfg.AutoRebalance {
		minTransfer := max(1, e.cfg.MinTradeNotional/2)
		amount := rebalanceAmount(f.spot, f.futures, e.cfg.FuturesMarginReserve, minTransfer)
		if e.cfg.DryRun {
			// Size as a live run would after rebalancing, without moving funds.
			f.spot -= amount
			f.futures += amount
			return f, nil
		}
		switch {
		case amount > 0:
			err = e.client.Transfer(ctx, accountSpot, accountFutures, currency.EMPTYPAIR, amount)
		case amount < 0:
			err = e.client.Transfer(ctx, accountFutures, accountSpot, currency.EMPTYPAIR, -amount)
		}
		if amount != 0 {
			if err != nil {
				e.log.Warn("rebalance accounts", "amount_to_futures", amount, "err", err)
			} else {
				e.log.Info("rebalanced accounts", "amount_to_futures", amount)
				f.spot -= amount
				f.futures += amount
			}
		}
	}
	return f, nil
}

// capitalAvailable is the notional that both legs of a carry position can fund.
func capitalAvailable(spotFree, futuresFree float64, cfg Config) float64 {
	return funds{spot: spotFree, futures: futuresFree}.usable(Carry, cfg)
}

// rebalanceAmount returns the USDT to move from spot to futures (negative for the reverse) so that spot funds equal
// futures funds after the margin reserve. Moves smaller than minTransfer return zero.
func rebalanceAmount(spotFree, futuresFree, reserve, minTransfer float64) float64 {
	targetFutures := (spotFree + futuresFree) / (2 - reserve)
	amount := targetFutures - futuresFree
	if math.Abs(amount) < minTransfer {
		return 0
	}
	return amount
}

func (e *Engine) feesFor(ctx context.Context, inst Instrument) (Fees, error) {
	key := inst.Key()
	if c, ok := e.fees[key]; ok && e.now().Sub(c.at) < minutes(e.cfg.FeeCacheMinutes) {
		return c.fees, nil
	}
	fees, err := e.client.Fees(ctx, inst.SpotPair)
	if err != nil {
		return Fees{}, err
	}
	e.fees[key] = cachedFees{fees: fees, at: e.now()}
	return fees, nil
}

// cachedFees returns the last known fees, even if stale. Used where fees only refine accounting.
func (e *Engine) cachedFees(inst Instrument) Fees {
	return e.fees[inst.Key()].fees
}

func (e *Engine) borrowRate(ctx context.Context, base string) (float64, error) {
	if c, ok := e.borrowRates[base]; ok && e.now().Sub(c.at) < minutes(e.cfg.FeeCacheMinutes) {
		return c.rate, nil
	}
	rates, err := e.client.BorrowRates(ctx, []string{base})
	if err != nil {
		return 0, err
	}
	rate, ok := rates[base]
	if !ok {
		return 0, fmt.Errorf("no borrow rate for %s", base)
	}
	e.borrowRates[base] = cachedRate{rate: rate, at: e.now()}
	return rate, nil
}

func (e *Engine) logBalances(ctx context.Context) {
	spotFree, spotLocked, err := e.client.SpotBalance(ctx, settlement)
	if err != nil {
		e.log.Error("spot balance", "err", err)
		return
	}
	acc, err := e.client.FuturesAccount(ctx)
	if err != nil {
		e.log.Error("futures account", "err", err)
		return
	}
	positionsValue := 0.0
	for _, pos := range e.positions {
		price := e.spotBids[pos.Inst.SpotPair.String()]
		if price == 0 {
			price = pos.avgEntrySpotPrice()
		}
		if pos.Dir() == Carry {
			positionsValue += pos.SpotExposure() * price
			continue
		}
		// A reverse position's margin account holds the sale proceeds and collateral against the base owed.
		m, err := e.client.MarginAccount(ctx, pos.Inst.SpotPair)
		if err != nil {
			e.log.Error("margin account", "pair", pos.Inst.SpotPair.String(), "err", err)
			return
		}
		positionsValue += m.QuoteAvailable + (m.BaseAvailable-m.BaseOwed())*price
	}
	total := spotFree + spotLocked + acc.Total.Float64() + acc.UnrealisedPnl.Float64() + positionsValue
	e.metrics.Balance.WithLabelValues("SpotPerpetualSpreadArbitrage_GateIO").Set(total)
	e.log.Info("balances", "total", total, "spot_usdt", spotFree+spotLocked, "futures_total", acc.Total.Float64(), "positions_value", positionsValue)
	if e.balanceLog != nil {
		e.balanceLog.Printf("%s,%.2f\n", e.now().Format(time.RFC3339), total)
	}
}

func (e *Engine) save() {
	if err := e.store.SavePositions(e.positions); err != nil {
		e.log.Error("save positions", "err", err)
	}
}
