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
	fees                     Fees

	spotFree, futuresFree float64
	position              gateio.Position

	spotFillRatio    float64
	futuresFillRatio float64
	spotErr          error

	spotOrders    []SpotOrderRequest
	futuresOrders []FuturesOrderRequest
	transfers     []float64
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
	}
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

func (c *fakeClient) SpotTickers(context.Context) ([]gateio.Ticker, error) {
	return []gateio.Ticker{{
		CurrencyPair: testKey, LowestAsk: types.Number(c.spotAsks[0].Price), HighestBid: types.Number(c.spotBids[0].Price),
		Last: types.Number(c.spotLast), QuoteVolume: 1e7,
	}}, nil
}

func (c *fakeClient) FuturesTickers(context.Context) ([]gateio.FuturesTicker, error) {
	return []gateio.FuturesTicker{{
		Contract: testKey, Last: types.Number(c.futuresLast), IndexPrice: types.Number(c.index()), Volume24HQuote: 1e7, FundingRate: 0.0001,
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

func (c *fakeClient) Transfer(_ context.Context, from, _ string, amount float64) error {
	if from == "futures" {
		amount = -amount
	}
	c.transfers = append(c.transfers, amount)
	c.spotFree -= amount
	c.futuresFree += amount
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
	e, err := NewEngine(cfg, c, slog.New(slog.DiscardHandler), NewMetrics(prometheus.NewRegistry()), nil)
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
	assert.Equal(t, "basis converged", rows[0]["close_reason"])
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
