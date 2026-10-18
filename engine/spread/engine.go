package spread

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/thrasher-corp/gocryptotrader/config"
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

// Engine runs the spot/perpetual basis strategy: buy spot and short the perpetual when the perp trades at least
// MinEntrySpread above spot and the trade clears MinNetProfit after fees, then unwind both legs once the basis
// falls to ExitSpread. All state is owned by the Run goroutine.
type Engine struct {
	cfg        Config
	client     Client
	log        *slog.Logger
	metrics    *Metrics
	balanceLog *log.Logger
	store      Store
	now        func() time.Time

	universe    map[string]Instrument
	positions   map[string]*Position
	blocked     map[string]bool
	fees        map[string]cachedFees
	leverageSet map[string]bool
	spotBids    map[string]float64
}

type cachedFees struct {
	fees Fees
	at   time.Time
}

// NewEngine restores persisted positions and prepares the engine. balanceLog may be nil.
func NewEngine(cfg Config, client Client, logger *slog.Logger, metrics *Metrics, balanceLog *log.Logger) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store := Store{path: cfg.StateFile}
	positions, err := store.LoadPositions()
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	return &Engine{
		cfg:         cfg,
		client:      client,
		log:         logger,
		metrics:     metrics,
		balanceLog:  balanceLog,
		store:       store,
		now:         time.Now,
		universe:    make(map[string]Instrument),
		positions:   positions,
		blocked:     make(map[string]bool),
		fees:        make(map[string]cachedFees),
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
	if err := e.refreshUniverse(ctx); err != nil {
		return fmt.Errorf("load instruments: %w", err)
	}
	e.log.Info("strategy started",
		"dry_run", e.cfg.DryRun,
		"instruments", len(e.universe),
		"restored_positions", len(e.positions),
		"min_entry_spread", e.cfg.MinEntrySpread,
		"exit_spread", e.cfg.ExitSpread,
		"min_net_profit", e.cfg.MinNetProfit,
		"max_open_positions", e.cfg.MaxOpenPositions,
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
	if err := e.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		e.log.Error("scan for entries", "err", err)
	}
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
	universe := buildUniverse(spotPairs, contracts)
	if len(universe) == 0 {
		return errors.New("no tradable spot/perpetual pairs found")
	}
	e.universe = universe
	e.log.Info("instruments refreshed", "count", len(universe))
	return nil
}

// monitor reconciles held positions with the exchange and closes those whose basis has converged.
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

	keys := make([]string, 0, len(e.positions))
	for key := range e.positions {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var errs []error
	for _, key := range keys {
		ex, ok := onExchange[key]
		if err := e.manage(ctx, e.positions[key], ex, ok); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) manage(ctx context.Context, pos *Position, ex gateio.Position, onExchange bool) error {
	key := pos.Inst.Key()
	var reason string
	if pos.OpenContracts() > epsilon {
		switch {
		case !onExchange:
			// The short is gone: liquidated, deleveraged or closed by hand. Its fills are unknown, so futures P&L is
			// recorded as flat and the now unhedged spot is sold.
			e.log.Error("perp leg missing on exchange, selling spot leg", "instrument", key, "expected_contracts", pos.OpenContracts())
			pos.ContractsBought = pos.ContractsSold
			pos.FutBuyNotional = pos.FutSellNotional
			pos.State = stateClosing
			pos.CloseReason = "perp leg closed externally; futures P&L not captured"
			e.save()
		default:
			pos.Funding = ex.RealisedPNLFundingFee.Float64()
			if held := math.Abs(ex.Size); math.Abs(held-pos.OpenContracts()) > epsilon {
				e.log.Warn("perp size differs from records", "instrument", key, "exchange", held, "recorded", pos.OpenContracts())
			}
			if liq, mark := ex.LiquidationPrice.Float64(), ex.MarkPrice.Float64(); liq > 0 && mark > 0 && (liq-mark)/mark < e.cfg.LiquidationBuffer {
				reason = "liquidation guard"
			}
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

	spotBids, _, err := e.client.SpotBook(ctx, pos.Inst.SpotPair, e.cfg.OrderbookDepth)
	if err != nil {
		return err
	}
	var futuresAsks []Level
	if pos.OpenContracts() > epsilon {
		if _, futuresAsks, err = e.client.FuturesBook(ctx, pos.Inst.Contract, e.cfg.OrderbookDepth); err != nil {
			return err
		}
	}
	plan, err := planExit(pos.Inst, pos.OpenContracts(), pos.SpotHeld(), spotBids, futuresAsks, e.cfg)
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
		reason = "basis converged"
	default:
		return nil
	}
	return e.close(ctx, pos, plan, reason)
}

// scan shortlists instruments from tickers, prices them on the orderbooks and opens those that qualify.
func (e *Engine) scan(ctx context.Context) error {
	spotTickers, err := e.client.SpotTickers(ctx)
	if err != nil {
		return err
	}
	for _, t := range spotTickers {
		e.spotBids[strings.ToUpper(t.CurrencyPair)] = t.HighestBid.Float64()
	}
	slots := e.cfg.MaxOpenPositions - len(e.positions)
	if slots <= 0 {
		return nil
	}
	futuresTickers, err := e.client.FuturesTickers(ctx)
	if err != nil {
		return err
	}
	candidates := rankCandidates(e.universe, spotTickers, futuresTickers, e.cfg, func(key string) bool {
		_, held := e.positions[key]
		return held || e.blocked[key]
	})
	if len(candidates) == 0 {
		return nil
	}

	usable, err := e.usableCapital(ctx)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		budget := tradeBudget(usable, slots, e.cfg)
		if budget == 0 {
			e.log.Debug("insufficient funds for another position", "usable", usable)
			return nil
		}
		key := c.inst.Key()
		plan, err := e.evaluate(ctx, c.inst, budget)
		if err != nil {
			e.log.Debug("candidate rejected", "instrument", key, "ticker_basis", c.tickerBasis, "reason", err)
			continue
		}
		if e.cfg.DryRun {
			e.log.Info("dry run: would open",
				"instrument", key, "contracts", plan.Contracts, "spot_qty", plan.SpotQty, "notional", plan.Notional,
				"basis", plan.Basis, "round_trip_fees", plan.Fees.RoundTrip(), "expected_net", plan.ExpectedNet)
			slots--
			usable -= plan.Notional
			continue
		}
		pos, err := e.open(ctx, plan)
		if err != nil {
			e.log.Error("open position", "instrument", key, "err", err)
			continue
		}
		if pos != nil {
			slots--
			usable -= pos.SpotCost
		}
	}
	return nil
}

func (e *Engine) evaluate(ctx context.Context, inst Instrument, budget float64) (EntryPlan, error) {
	fees, err := e.feesFor(ctx, inst)
	if err != nil {
		return EntryPlan{}, fmt.Errorf("fees: %w", err)
	}
	_, spotAsks, err := e.client.SpotBook(ctx, inst.SpotPair, e.cfg.OrderbookDepth)
	if err != nil {
		return EntryPlan{}, err
	}
	futuresBids, _, err := e.client.FuturesBook(ctx, inst.Contract, e.cfg.OrderbookDepth)
	if err != nil {
		return EntryPlan{}, err
	}
	return planEntry(inst, spotAsks, futuresBids, fees, budget, e.cfg)
}

// usableCapital returns the notional both accounts can fund for new positions, rebalancing them first if enabled.
func (e *Engine) usableCapital(ctx context.Context) (float64, error) {
	spotFree, _, err := e.client.SpotBalance(ctx, settlement)
	if err != nil {
		return 0, fmt.Errorf("spot balance: %w", err)
	}
	acc, err := e.client.FuturesAccount(ctx)
	if err != nil {
		return 0, fmt.Errorf("futures account: %w", err)
	}
	futuresFree := acc.Available.Float64()

	if e.cfg.AutoRebalance && !e.cfg.DryRun {
		minTransfer := max(1, e.cfg.MinTradeNotional/2)
		amount := rebalanceAmount(spotFree, futuresFree, e.cfg.FuturesMarginReserve, minTransfer)
		switch {
		case amount > 0:
			err = e.client.Transfer(ctx, "spot", "futures", amount)
		case amount < 0:
			err = e.client.Transfer(ctx, "futures", "spot", -amount)
		}
		if amount != 0 {
			if err != nil {
				e.log.Warn("rebalance accounts", "amount_to_futures", amount, "err", err)
			} else {
				e.log.Info("rebalanced accounts", "amount_to_futures", amount)
				spotFree -= amount
				futuresFree += amount
			}
		}
	}
	return capitalAvailable(spotFree, futuresFree, e.cfg), nil
}

// capitalAvailable is the notional that both legs can fund: spot pays for the coins, futures holds 1x margin plus
// the configured reserve.
func capitalAvailable(spotFree, futuresFree float64, cfg Config) float64 {
	return max(0, min(spotFree, futuresFree*(1-cfg.FuturesMarginReserve))*capitalHeadroom)
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
	held := 0.0
	for _, pos := range e.positions {
		price := e.spotBids[pos.Inst.SpotPair.String()]
		if price == 0 {
			price = pos.avgEntrySpotPrice()
		}
		held += pos.SpotHeld() * price
	}
	total := spotFree + spotLocked + acc.Total.Float64() + acc.UnrealisedPnl.Float64() + held
	e.metrics.Balance.WithLabelValues("SpotPerpetualSpreadArbitrage_GateIO").Set(total)
	e.log.Info("balances", "total", total, "spot_usdt", spotFree+spotLocked, "futures_total", acc.Total.Float64(), "spot_holdings", held)
	if e.balanceLog != nil {
		e.balanceLog.Printf("%s,%.2f\n", e.now().Format(time.RFC3339), total)
	}
}

func (e *Engine) save() {
	if err := e.store.SavePositions(e.positions); err != nil {
		e.log.Error("save positions", "err", err)
	}
}
