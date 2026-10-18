package spread

import (
	"context"
	"encoding/csv"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/types"
)

const testKey = "ABC_USDT"

// fakeClient simulates one spot market and its perpetual. Orders fill at the touch when their limit allows it.
type fakeClient struct {
	spotBids, spotAsks       []Level
	futuresBids, futuresAsks []Level
	spotLast, futuresLast    float64
	indexPrice               float64 // zero tracks the spot price
	fundingRate              float64
	fees                     Fees

	spotFree, futuresFree float64
	position              gateio.Position
	margin                MarginAccount
	borrowRate            float64

	spotFillRatio    float64
	futuresFillRatio float64
	spotErr          error

	spotOrders    []SpotOrderRequest
	futuresOrders []FuturesOrderRequest
	transfers     []float64 // positive: spot to futures
	marginMoves   []float64 // positive: spot to margin
	repayments    []float64
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		spotBids:         deepBook(99.99),
		spotAsks:         deepBook(100),
		futuresBids:      deepBook(100.7),
		futuresAsks:      deepBook(100.71),
		spotLast:         100,
		futuresLast:      100.7,
		fees:             testFees,
		spotFree:         100,
		futuresFree:      120,
		position:         gateio.Position{Contract: testKey},
		spotFillRatio:    1,
		futuresFillRatio: 1,
		borrowRate:       0.00001,
		fundingRate:      0.0001,
	}
}

// newReverseFakeClient prices the perp about 0.7% below spot.
func newReverseFakeClient() *fakeClient {
	c := newFakeClient()
	c.spotBids, c.spotAsks = deepBook(100.7), deepBook(100.71)
	c.futuresBids, c.futuresAsks = deepBook(99.99), deepBook(100)
	c.spotLast, c.futuresLast = 100.7, 100
	c.fundingRate = -0.0001 // longs receive funding when the perp trades below spot
	return c
}

func (c *fakeClient) SpotPairs(context.Context) ([]gateio.CurrencyPairDetail, error) {
	return []gateio.CurrencyPairDetail{{
		ID: usdtPair("ABC"), Base: currency.NewCode("ABC"), Quote: currency.USDT, TradeStatus: "tradable",
		AmountPrecision: 4, PricePrecision: 2, MinQuoteAmount: 1,
	}}, nil
}

func (c *fakeClient) Contracts(context.Context) ([]gateio.FuturesContract, error) {
	return []gateio.FuturesContract{{
		Name: usdtPair("ABC"), Type: "direct", Status: "trading", QuantoMultiplier: 0.01, OrderSizeMin: 1,
		OrderSizeMax: 1_000_000, OrderPriceRound: 0.01, LeverageMin: 1,
	}}, nil
}

func (c *fakeClient) MarginPairs(context.Context) ([]MarginPair, error) {
	return []MarginPair{{Pair: testKey, MinBorrow: 0.001}}, nil
}

func (c *fakeClient) BorrowRates(context.Context, []string) (map[string]float64, error) {
	return map[string]float64{"ABC": c.borrowRate}, nil
}

func (c *fakeClient) MarginAccount(context.Context, currency.Pair) (MarginAccount, error) {
	return c.margin, nil
}

func (c *fakeClient) Repay(_ context.Context, _ currency.Pair, amount float64, all bool) error {
	if all {
		amount = c.margin.BaseOwed()
	}
	if amount > c.margin.BaseAvailable+epsilon {
		return errors.New("insufficient base to repay")
	}
	c.repayments = append(c.repayments, amount)
	c.margin.BaseAvailable -= amount
	interest := min(amount, c.margin.BaseInterest)
	c.margin.BaseInterest -= interest
	c.margin.BaseBorrowed -= amount - interest
	return nil
}

func (c *fakeClient) FuturesBookSnapshot(context.Context, currency.Pair, uint64) (bids, asks []Level, id int64, err error) {
	return c.futuresBids, c.futuresAsks, 0, nil
}

func (c *fakeClient) SpotTickers(context.Context) ([]gateio.Ticker, error) {
	return []gateio.Ticker{{
		CurrencyPair: testKey, LowestAsk: types.Number(c.spotAsks[0].Price), HighestBid: types.Number(c.spotBids[0].Price),
		Last: types.Number(c.spotLast), QuoteVolume: 1e7,
	}}, nil
}

func (c *fakeClient) FuturesTickers(context.Context) ([]gateio.FuturesTicker, error) {
	return []gateio.FuturesTicker{{
		Contract: testKey, Last: types.Number(c.futuresLast), IndexPrice: types.Number(c.index()), Volume24HQuote: 1e7, FundingRate: types.Number(c.fundingRate),
	}}, nil
}

func (c *fakeClient) index() float64 {
	if c.indexPrice != 0 {
		return c.indexPrice
	}
	return c.spotLast
}

func (c *fakeClient) SpotBook(context.Context, currency.Pair, uint64) (bids, asks []Level, err error) {
	return c.spotBids, c.spotAsks, nil
}

func (c *fakeClient) FuturesBook(context.Context, currency.Pair, uint64) (bids, asks []Level, err error) {
	return c.futuresBids, c.futuresAsks, nil
}

func (c *fakeClient) Fees(context.Context, currency.Pair) (Fees, error) { return c.fees, nil }

func (c *fakeClient) SpotBalance(context.Context, currency.Code) (available, locked float64, err error) {
	return c.spotFree, 0, nil
}

func (c *fakeClient) FuturesAccount(context.Context) (*gateio.FuturesAccount, error) {
	return &gateio.FuturesAccount{Available: types.Number(c.futuresFree), Total: types.Number(c.futuresFree)}, nil
}

func (c *fakeClient) Positions(context.Context) ([]gateio.Position, error) {
	if c.position.Size == 0 {
		return nil, nil
	}
	return []gateio.Position{c.position}, nil
}

func (c *fakeClient) Transfer(_ context.Context, from, to string, _ currency.Pair, amount float64) error {
	switch {
	case from == accountSpot && to == accountFutures:
		c.transfers = append(c.transfers, amount)
		c.spotFree, c.futuresFree = c.spotFree-amount, c.futuresFree+amount
	case from == accountFutures && to == accountSpot:
		c.transfers = append(c.transfers, -amount)
		c.spotFree, c.futuresFree = c.spotFree+amount, c.futuresFree-amount
	case from == accountSpot && to == accountMargin:
		c.marginMoves = append(c.marginMoves, amount)
		c.spotFree, c.margin.QuoteAvailable = c.spotFree-amount, c.margin.QuoteAvailable+amount
	case from == accountMargin && to == accountSpot:
		c.marginMoves = append(c.marginMoves, -amount)
		c.spotFree, c.margin.QuoteAvailable = c.spotFree+amount, c.margin.QuoteAvailable-amount
	default:
		return errors.New("unsupported transfer")
	}
	return nil
}

func (c *fakeClient) EnsureSinglePositionMode(context.Context) error { return nil }

func (c *fakeClient) SetLeverage(context.Context, currency.Pair, float64) error { return nil }

func (c *fakeClient) SpotOrder(_ context.Context, req SpotOrderRequest) (SpotFill, error) {
	c.spotOrders = append(c.spotOrders, req)
	if c.spotErr != nil {
		return SpotFill{}, c.spotErr
	}
	price := c.spotBids[0].Price
	if req.Buy {
		price = c.spotAsks[0].Price
	}
	if req.Price != 0 && ((req.Buy && price > req.Price) || (!req.Buy && price < req.Price)) {
		return SpotFill{}, nil
	}
	qty := floorTo(req.Qty*c.spotFillRatio, 4)
	if qty == 0 {
		return SpotFill{}, nil
	}
	if req.Margin {
		if req.Buy {
			c.margin.QuoteAvailable -= qty * price
			c.margin.BaseAvailable += qty - qty*c.fees.SpotTaker
			return SpotFill{Qty: qty, AvgPrice: price, FeeBase: qty * c.fees.SpotTaker}, nil
		}
		if short := qty - c.margin.BaseAvailable; short > 0 {
			if !req.AutoBorrow {
				return SpotFill{}, errors.New("insufficient base")
			}
			c.margin.BaseBorrowed += short
			c.margin.BaseAvailable += short
		}
		fee := qty * price * c.fees.SpotTaker
		c.margin.BaseAvailable -= qty
		c.margin.QuoteAvailable += qty*price - fee
		return SpotFill{Qty: qty, AvgPrice: price, FeeQuote: fee}, nil
	}
	if req.Buy {
		c.spotFree -= qty * price
		return SpotFill{Qty: qty, AvgPrice: price, FeeBase: qty * c.fees.SpotTaker}, nil
	}
	fee := qty * price * c.fees.SpotTaker
	c.spotFree += qty*price - fee
	return SpotFill{Qty: qty, AvgPrice: price, FeeQuote: fee}, nil
}

func (c *fakeClient) FuturesOrder(_ context.Context, req FuturesOrderRequest) (FuturesFill, error) {
	c.futuresOrders = append(c.futuresOrders, req)
	price := c.futuresAsks[0].Price
	if req.Sell {
		price = c.futuresBids[0].Price
	}
	if req.Price != 0 && ((!req.Sell && price > req.Price) || (req.Sell && price < req.Price)) {
		return FuturesFill{}, nil
	}
	filled := math.Floor(req.Contracts * c.futuresFillRatio)
	if req.ReduceOnly {
		filled = min(filled, math.Abs(c.position.Size))
	}
	if req.Sell {
		c.position.Size -= filled
	} else {
		c.position.Size += filled
	}
	return FuturesFill{Contracts: filled, AvgPrice: price, FeeRate: c.fees.FuturesTaker}, nil
}

func newTestEngine(t *testing.T, c *fakeClient, mutate func(*Config)) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DryRun = false
	cfg.AutoRebalance = false
	cfg.StateFile = filepath.Join(dir, "positions.json")
	cfg.LedgerFile = filepath.Join(dir, "trades.csv")
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := NewEngine(cfg, c, nil, slog.New(slog.DiscardHandler), NewMetrics(prometheus.NewRegistry()), nil)
	require.NoError(t, err)
	require.NoError(t, e.refreshUniverse(t.Context()))
	return e
}

// converge moves both markets so the exit basis is about 0.05% and the ticker shows no new entry.
func (c *fakeClient) converge() {
	c.spotBids, c.spotAsks = deepBook(100.5), deepBook(100.51)
	c.futuresBids, c.futuresAsks = deepBook(100.54), deepBook(100.55)
	c.spotLast, c.futuresLast = 100.5, 100.55
}

func ledgerRows(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	var rows []map[string]string
	for _, r := range records[1:] {
		row := make(map[string]string)
		for i, h := range records[0] {
			row[h] = r[i]
		}
		rows = append(rows, row)
	}
	return rows
}

func TestEngineOpensAndClosesHedgedPosition(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	require.Len(t, e.positions, 1)
	pos := e.positions[testKey]
	// Usable capital min(100, 120*0.9)*0.98 = 98 split over 10 slots buys 9 contracts of 0.01 base.
	assert.Equal(t, 9.0, pos.OpenContracts())
	assert.Equal(t, -9.0, c.position.Size)
	assert.Equal(t, pos.OpenContracts(), pos.HedgedContracts(), "spot covers the short after its fee")
	require.Len(t, c.futuresOrders, 1)
	assert.True(t, c.futuresOrders[0].Sell, "perp leg goes first")
	require.Len(t, c.spotOrders, 1)
	assert.Equal(t, 0.0901, c.spotOrders[0].Qty)

	restored, err := Store{path: e.cfg.StateFile}.LoadPositions()
	require.NoError(t, err)
	require.Contains(t, restored, testKey, "positions survive a restart")
	assert.Equal(t, 9.0, restored[testKey].OpenContracts())

	e.tick(t.Context())
	assert.Len(t, e.positions, 1, "basis still wide, position held")

	c.converge()
	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Zero(t, c.position.Size)

	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "spread converged", rows[0]["close_reason"])
	assert.Equal(t, "carry", rows[0]["direction"])
	pnl, err := strconv.ParseFloat(rows[0]["net_pnl"], 64)
	require.NoError(t, err)
	// About 0.7% entry less 0.05% exit less 0.3% fees on roughly 9 USDT.
	assert.InDelta(t, 0.03, pnl, 0.01)
	fees, err := strconv.ParseFloat(rows[0]["spot_fees"], 64)
	require.NoError(t, err)
	assert.Positive(t, fees)
}

func TestEngineTrimsPartialSpotHedge(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotFillRatio = 0.5
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	require.Len(t, e.positions, 1)
	pos := e.positions[testKey]
	assert.Equal(t, 4.0, pos.OpenContracts(), "contracts the half filled spot cannot cover are bought back")
	assert.Equal(t, -4.0, c.position.Size)
	require.Len(t, c.futuresOrders, 2)
	assert.True(t, c.futuresOrders[1].ReduceOnly)
	assert.Equal(t, 5.0, c.futuresOrders[1].Contracts)
}

func TestEngineUnwindsWhenSpotHedgeFails(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotErr = errors.New("spot rejected")
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Zero(t, c.position.Size, "perp short bought back")
	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "hedge failed", rows[0]["close_reason"])
}

func TestEngineRequiresNetProfitAfterFees(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.fees = Fees{SpotTaker: 0.002, FuturesTaker: 0.0005}
	c.futuresBids, c.futuresLast = deepBook(100.55), 100.55
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Empty(t, c.futuresOrders, "0.55%% gross clears the minimum spread but not the fees")
}

func TestEngineDryRunPlacesNoOrders(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotFree, c.futuresFree = 0, 200 // dry run sizes as if rebalanced, but moves nothing
	e := newTestEngine(t, c, func(cfg *Config) { cfg.DryRun = true; cfg.AutoRebalance = true })

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Empty(t, c.futuresOrders)
	assert.Empty(t, c.spotOrders)
	assert.Empty(t, c.transfers)
}

func TestEngineLeavesUnmanagedPositionsAlone(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.position.Size = 5
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Empty(t, c.futuresOrders)
	assert.True(t, e.blocked[testKey])
}

func TestEngineLiquidationGuard(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MaxOpenPositions = 1 })
	e.tick(t.Context())
	require.Len(t, e.positions, 1)

	c.position.MarkPrice, c.position.LiquidationPrice = 100, 105
	e.tick(t.Context())
	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "liquidation guard", rows[0]["close_reason"])
}

func TestEngineSellsSpotWhenPerpLegDisappears(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MaxOpenPositions = 1 })
	e.tick(t.Context())
	require.Len(t, e.positions, 1)

	c.position.Size = 0
	c.futuresLast = 100
	e.tick(t.Context())
	assert.Empty(t, e.positions)
	last := c.spotOrders[len(c.spotOrders)-1]
	assert.False(t, last.Buy, "unhedged spot sold")
	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0]["close_reason"], "perp leg closed externally")
}

func TestEngineRebalancesAccounts(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotFree, c.futuresFree = 0, 200
	e := newTestEngine(t, c, func(cfg *Config) { cfg.AutoRebalance = true })

	e.tick(t.Context())
	require.Len(t, c.transfers, 1)
	assert.InDelta(t, -94.74, c.transfers[0], 0.01, "futures to spot")
	assert.Len(t, e.positions, 1, "the rebalanced funds open the position")
}

func TestEngineClosesWithMarketOrdersAfterRepeatedMisses(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MaxOpenPositions = 1 })
	e.tick(t.Context())
	require.Len(t, e.positions, 1)

	c.converge()
	c.futuresFillRatio = 0 // IOC limits keep missing
	for range marketCloseAfter + 1 {
		e.tick(t.Context())
	}
	last := c.futuresOrders[len(c.futuresOrders)-1]
	assert.Zero(t, last.Price, "falls back to a market order")
	assert.True(t, last.ReduceOnly)
}

func TestEngineSkipsPerpTrackingAnotherAsset(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.indexPrice = 80 // same ticker, different underlying
	e := newTestEngine(t, c, nil)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders)

	c = newFakeClient()
	c.futuresBids, c.futuresLast = deepBook(105), 105
	e = newTestEngine(t, c, nil)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders, "a 5%% basis exceeds max_entry_spread")
}

// convergeReverse moves both markets so the reverse exit spread is about 0.05% and no new entry shows.
func (c *fakeClient) convergeReverse() {
	c.spotBids, c.spotAsks = deepBook(100.29), deepBook(100.3)
	c.futuresBids, c.futuresAsks = deepBook(100.25), deepBook(100.26)
	c.spotLast, c.futuresLast = 100.3, 100.25
}

func TestEngineOpensAndClosesReversePosition(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	require.Len(t, e.positions, 1)
	pos := e.positions[testKey]
	assert.Equal(t, Reverse, pos.Dir())
	// Usable min(100/0.5, 120*0.9)*0.98 = 105.84 over 10 slots sells 0.1 base and longs 10 contracts.
	assert.Equal(t, 10.0, pos.OpenContracts())
	assert.Equal(t, 10.0, c.position.Size, "perp leg is long")
	assert.False(t, c.futuresOrders[0].Sell)
	require.Len(t, c.spotOrders, 1)
	assert.True(t, c.spotOrders[0].Margin)
	assert.True(t, c.spotOrders[0].AutoBorrow)
	assert.False(t, c.spotOrders[0].Buy)
	assert.InDelta(t, 0.1, c.margin.BaseBorrowed, 1e-12)
	require.Len(t, c.marginMoves, 1)
	assert.InDelta(t, 5.04, c.marginMoves[0], 1e-9, "half the notional posted as collateral")
	assert.Equal(t, pos.OpenContracts(), pos.HedgedContracts())

	c.margin.BaseInterest = 0.0001 // a day of interest accrues
	e.tick(t.Context())
	assert.Len(t, e.positions, 1, "spread still wide")
	assert.InDelta(t, 0.0001, pos.InterestBase, 1e-12)

	c.convergeReverse()
	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Zero(t, c.position.Size)
	assert.Zero(t, c.margin.BaseOwed(), "loan and interest repaid")
	require.NotEmpty(t, c.repayments)
	assert.Less(t, c.marginMoves[len(c.marginMoves)-1], 0.0, "collateral returned to spot")

	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "reverse", rows[0]["direction"])
	assert.Equal(t, "spread converged", rows[0]["close_reason"])
	pnl, err := strconv.ParseFloat(rows[0]["net_pnl"], 64)
	require.NoError(t, err)
	// About 0.7% entry less 0.05% exit, 0.3% fees and the interest bought back, on roughly 10 USDT.
	assert.InDelta(t, 0.0148, pnl, 0.002)
	interest, err := strconv.ParseFloat(rows[0]["interest_base"], 64)
	require.NoError(t, err)
	assert.InDelta(t, 0.0001, interest, 1e-9)
}

func TestEngineReverseNeedsMarginPair(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.EnableReverse = false })
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders, "reverse disabled, and the carry spread is negative")
}

func TestEngineReverseSkipsPayingFunding(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	c.fundingRate = 0.0001
	e := newTestEngine(t, c, nil)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders, "a long would pay positive funding")
}

func TestEngineReverseIncludesInterest(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	c.borrowRate = 0.0002 // 0.48% over the expected 24 hour hold
	e := newTestEngine(t, c, nil)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders)
}

func TestEngineReverseUnwindsWhenSpotSellFails(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	c.spotErr = errors.New("margin sell rejected")
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	assert.Zero(t, c.position.Size, "perp long sold back")
	assert.Zero(t, c.margin.QuoteAvailable, "collateral returned")
	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "hedge failed", rows[0]["close_reason"])
}

func TestEngineReverseMarginRiskGuard(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MaxOpenPositions = 1 })
	e.tick(t.Context())
	require.Len(t, e.positions, 1)
	borrowed := e.positions[testKey].SpotSold

	c.margin.Risk = 1.2
	e.tick(t.Context())
	rows := ledgerRows(t, e.cfg.LedgerFile)
	require.Len(t, rows, 1)
	assert.Equal(t, "margin risk guard", rows[0]["close_reason"])
	require.NotEmpty(t, c.repayments, "the loan was repaid before the slot was reused")
	assert.InDelta(t, borrowed, c.repayments[0], 1e-9)
}

func TestEngineRestoresLegacyCarryPositions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "positions.json")
	legacy := `[{"id":"ABC_USDT-1","instrument":{"base":"ABC","spot_pair":"ABC_USDT","contract":"ABC_USDT","multiplier":0.01},
		"state":"open","contracts_sold":9,"spot_bought":0.09}]`
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
	positions, err := Store{path: path}.LoadPositions()
	require.NoError(t, err)
	require.Contains(t, positions, testKey)
	assert.Equal(t, Carry, positions[testKey].Dir())
	assert.Equal(t, 9.0, positions[testKey].OpenContracts())
}
