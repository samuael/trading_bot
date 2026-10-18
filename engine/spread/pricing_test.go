package spread

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWalkBook(t *testing.T) {
	t.Parallel()
	asks := []Level{{Price: 100, Amount: 1}, {Price: 101, Amount: 2}, {Price: 102, Amount: 5}}

	f := walkBook(asks, 2)
	assert.InDelta(t, 2, f.Qty, 1e-12)
	assert.InDelta(t, 100.5, f.VWAP, 1e-12)
	assert.InDelta(t, 101, f.Worst, 1e-12)

	f = walkBook(asks, 20)
	assert.InDelta(t, 8, f.Qty, 1e-12, "thin book fills partially")
	assert.InDelta(t, 102, f.Worst, 1e-12)

	assert.Zero(t, walkBook(nil, 1).Qty)
}

func TestDepthWithin(t *testing.T) {
	t.Parallel()
	asks := []Level{{Price: 100, Amount: 1}, {Price: 100.2, Amount: 2}, {Price: 100.3, Amount: 5}}
	assert.InDelta(t, 3, depthWithin(asks, 0.002, true), 1e-12)
	bids := []Level{{Price: 100, Amount: 1}, {Price: 99.8, Amount: 2}, {Price: 99.7, Amount: 5}}
	assert.InDelta(t, 3, depthWithin(bids, 0.002, false), 1e-12)
	assert.Zero(t, depthWithin(nil, 0.002, true))
}

func TestRounding(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 1.2345, floorTo(1.23459, 4))
	assert.Equal(t, 1.2346, ceilTo(1.23451, 4))
	assert.Equal(t, 1.2345, ceilTo(1.2345, 4), "exact values stay put")
	assert.Equal(t, 123.45, floorToStep(123.456, 0.01))
	assert.Equal(t, 123.5, ceilToStep(123.451, 0.05))
	assert.Equal(t, 0.12375, floorToStep(0.12399, 0.00025))
	assert.Equal(t, 5, stepDecimals(0.00025))
	assert.Equal(t, 0, stepDecimals(5))
}

func TestBasisAndExpectedReturn(t *testing.T) {
	t.Parallel()
	assert.InDelta(t, 0.007, basis(100.7, 100), 1e-12)
	fees := Fees{SpotTaker: 0.001, FuturesTaker: 0.0005}
	assert.InDelta(t, 0.003, fees.RoundTrip(), 1e-12)
	assert.InDelta(t, 0.003, expectedNetReturn(0.007, 0.001, fees), 1e-12)
}

func testInstrument() Instrument {
	return Instrument{
		Base:                "ABC",
		SpotPair:            usdtPair("ABC"),
		Contract:            usdtPair("ABC"),
		Multiplier:          0.01,
		MinContracts:        1,
		MaxContracts:        1_000_000,
		PriceTick:           0.01,
		SpotAmountPrecision: 4,
		SpotPricePrecision:  2,
		SpotMinQuote:        1,
	}
}

var testFees = Fees{SpotTaker: 0.001, FuturesTaker: 0.0005}

func deepBook(price float64) []Level { return []Level{{Price: price, Amount: 1_000_000}} }

func TestPlanEntryAccepts(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	plan, err := planEntry(Carry, testInstrument(), deepBook(100), deepBook(100.7), testFees, 0, 50, cfg)
	require.NoError(t, err)

	assert.Equal(t, 49.0, plan.Contracts, "50 USDT less the fee allowance buys 0.4995 base, 49 whole contracts")
	assert.Equal(t, 0.4905, plan.SpotQty, "0.49 base grossed up for a fee deducted in base")
	assert.GreaterOrEqual(t, plan.SpotQty*(1-testFees.SpotTaker), plan.Contracts*0.01, "net spot covers the short")
	assert.InDelta(t, 0.007, plan.Basis, 1e-12)
	assert.InDelta(t, 0.003, plan.ExpectedNet, 1e-12)
	assert.InDelta(t, 49.05, plan.Notional, 1e-9)
	assert.Equal(t, 100.7, plan.FuturesLimit)
	assert.Equal(t, 100.2, plan.SpotLimit, "spot hedge gets the hedge slippage allowance")
}

func TestPlanEntryRejects(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	inst := testInstrument()

	_, err := planEntry(Carry, inst, deepBook(100), deepBook(100.45), testFees, 0, 50, cfg)
	assert.ErrorIs(t, err, errSpreadTooSmall, "0.45%% is below the 0.5%% gross minimum")

	_, err = planEntry(Carry, inst, deepBook(100), deepBook(104), testFees, 0, 50, cfg)
	assert.ErrorIs(t, err, errSpreadTooLarge)

	highFees := Fees{SpotTaker: 0.002, FuturesTaker: 0.0005}
	_, err = planEntry(Carry, inst, deepBook(100), deepBook(100.55), highFees, 0, 50, cfg)
	assert.ErrorIs(t, err, errNetTooSmall, "0.55%% gross less 0.1%% exit and 0.5%% fees is negative")

	_, err = planEntry(Carry, inst, deepBook(100), deepBook(100.7), testFees, 0, 0.5, cfg)
	assert.ErrorIs(t, err, errBelowMinSize)

	_, err = planEntry(Carry, inst, nil, deepBook(100.7), testFees, 0, 50, cfg)
	assert.ErrorIs(t, err, errNoLiquidity)
}

func TestPlanEntryAtMinimumBudget(t *testing.T) {
	t.Parallel()
	// SAND-like market: 1 base per contract at about 0.06 USDT. A 5 USDT budget, the configured minimum, rounds down
	// to 82 contracts worth 4.98 USDT, which must still be accepted.
	inst := testInstrument()
	inst.Multiplier, inst.SpotAmountPrecision, inst.SpotPricePrecision, inst.PriceTick = 1, 2, 5, 0.00001
	inst.MinBorrow = 1
	cfg := DefaultConfig()
	plan, err := planEntry(Reverse, inst, deepBook(0.06073), deepBook(0.06030), testFees, 0, cfg.MinTradeNotional, cfg)
	require.NoError(t, err)
	assert.Equal(t, 82.0, plan.Contracts)
	assert.Less(t, plan.Notional, cfg.MinTradeNotional)

	_, err = planEntry(Reverse, inst, deepBook(0.06073), deepBook(0.06030), testFees, 0, cfg.MinTradeNotional/2, cfg)
	assert.ErrorIs(t, err, errBelowMinSize, "a position well under the minimum is still rejected")
}

func TestPlanEntrySizesToLiquidity(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.MinTradeNotional = 1
	asks := []Level{{Price: 100, Amount: 0.1}, {Price: 101, Amount: 100}}
	plan, err := planEntry(Carry, testInstrument(), asks, deepBook(100.7), testFees, 0, 1000, cfg)
	require.NoError(t, err)
	assert.Equal(t, 9.0, plan.Contracts, "only 0.1 base sits within max slippage of the best ask")
}

func TestPlanExit(t *testing.T) {
	t.Parallel()
	plan, err := planExit(Carry, testInstrument(), 49, 0.49, deepBook(100.5), deepBook(100.55), DefaultConfig())
	require.NoError(t, err)
	assert.InDelta(t, 0.000497, plan.Basis, 1e-6)
	assert.Equal(t, 0.49, plan.SpotQty)
	assert.Equal(t, 100.29, plan.SpotLimit)
	assert.Equal(t, 100.76, plan.FuturesLimit)

	_, err = planExit(Carry, testInstrument(), 49, 0.49, nil, deepBook(100.55), DefaultConfig())
	assert.True(t, errors.Is(err, errNoLiquidity))
}

func TestPlanEntryReverse(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	inst := testInstrument()
	inst.MinBorrow = 0.001
	// Sell spot at the 100.7 bid, buy the perp at the 100 ask.
	plan, err := planEntry(Reverse, inst, deepBook(100.7), deepBook(100), testFees, 0.00001, 50, cfg)
	require.NoError(t, err)
	assert.Equal(t, 49.0, plan.Contracts, "no fee allowance on a sell: 50/100.7 = 0.4965 base")
	assert.Equal(t, 0.49, plan.SpotQty)
	assert.InDelta(t, 0.7/100.7, plan.Basis, 1e-12)
	assert.InDelta(t, 0.00024, plan.InterestCost, 1e-12, "hourly rate over 24 hours")
	assert.InDelta(t, plan.Basis-0.001-0.003-0.00024, plan.ExpectedNet, 1e-12)
	assert.Equal(t, 100.0, plan.FuturesLimit)
	assert.Equal(t, 100.49, plan.SpotLimit)

	_, err = planEntry(Reverse, inst, deepBook(100.7), deepBook(100), testFees, 0.0002, 50, cfg)
	assert.ErrorIs(t, err, errNetTooSmall, "0.48%% expected interest eats the spread")
}

func TestPlanExitReverse(t *testing.T) {
	t.Parallel()
	plan, err := planExit(Reverse, testInstrument(), 49, 0.4905, deepBook(100.3), deepBook(100.25), DefaultConfig())
	require.NoError(t, err)
	assert.InDelta(t, 0.05/100.3, plan.Basis, 1e-12)
	assert.Equal(t, 100.51, plan.SpotLimit, "buy back limit above the ask")
	assert.Equal(t, 100.04, plan.FuturesLimit, "perp sell limit below the bid")
}

func TestTradeBudget(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	assert.Equal(t, 10.0, tradeBudget(100, 10, cfg), "funds split over free slots")
	assert.Equal(t, 5.0, tradeBudget(30, 10, cfg), "small balances still open minimum sized positions")
	assert.Zero(t, tradeBudget(4, 10, cfg))
	assert.Zero(t, tradeBudget(100, 0, cfg))
	cfg.MaxTradeNotional = 8
	assert.Equal(t, 8.0, tradeBudget(100, 10, cfg))
}

func TestRebalanceAmount(t *testing.T) {
	t.Parallel()
	amount := rebalanceAmount(0, 100, 0.1, 1)
	assert.InDelta(t, -47.368, amount, 1e-3, "move funds to spot")
	spot, fut := -amount, 100+amount
	assert.InDelta(t, spot, fut*0.9, 1e-9, "both legs fund the same notional afterwards")
	assert.Zero(t, rebalanceAmount(47, 53, 0.1, 1))
	assert.InDelta(t, capitalAvailable(spot, fut, DefaultConfig()), spot*capitalHeadroom, 1e-9)
	assert.InDelta(t, fut*0.9*capitalHeadroom, funds{spot: spot, futures: fut}.usable(Reverse, DefaultConfig()), 1e-9,
		"reverse only posts half the notional from spot, so futures margin binds")
}

func TestPositionAccounting(t *testing.T) {
	t.Parallel()
	inst := testInstrument()
	pos := newPosition(EntryPlan{Inst: inst, Basis: 0.01}, time.Unix(0, 0))

	pos.recordFutures(FuturesFill{Contracts: 100, AvgPrice: 101, FeeRate: 0.0005}, true, testFees)
	pos.recordSpot(SpotFill{Qty: 1.0011, AvgPrice: 100, FeeBase: 0.0010011}, true, testFees)
	assert.Equal(t, 100.0, pos.HedgedContracts())
	assert.InDelta(t, 1.0000989, pos.SpotExposure(), 1e-9)

	pos.recordFutures(FuturesFill{Contracts: 100, AvgPrice: 100.6}, false, testFees)
	pos.recordSpot(SpotFill{Qty: 1, AvgPrice: 100.5, FeeQuote: 0.1005}, false, testFees)
	pos.Funding = 0.02
	assert.Zero(t, pos.OpenContracts())

	spotPnL := (100.5 - 0.1005) - 100.11
	futPnL := 101 - 100.6 - (101+100.6)*0.0005
	assert.InDelta(t, spotPnL+futPnL+0.02, pos.RealizedPnL(), 1e-9)
	assert.InDelta(t, 0.10011+0.1005+(101+100.6)*0.0005, pos.TotalFees(), 1e-9)
}

func TestReversePositionAccounting(t *testing.T) {
	t.Parallel()
	pos := newPosition(EntryPlan{Direction: Reverse, Inst: testInstrument()}, time.Unix(0, 0))
	pos.recordFutures(FuturesFill{Contracts: 100, AvgPrice: 100, FeeRate: 0.0005}, false, testFees)
	pos.recordSpot(SpotFill{Qty: 1, AvgPrice: 100.7, FeeQuote: 0.1007}, false, testFees)
	assert.Equal(t, 100.0, pos.OpenContracts())
	assert.Equal(t, 1.0, pos.SpotExposure(), "1 base owed")
	assert.Equal(t, 100.0, pos.HedgedContracts())

	pos.InterestBase = 0.001
	assert.InDelta(t, 1.001, pos.SpotExposure(), 1e-12, "interest adds to what must be bought back")
	pos.recordSpot(SpotFill{Qty: 1.0021, AvgPrice: 100.3, FeeBase: 0.0010021}, true, testFees)
	pos.InterestPaid, pos.InterestBase = 0.001, 0
	assert.LessOrEqual(t, pos.SpotExposure(), 0.0, "repaid with a little dust left over")
	pos.recordFutures(FuturesFill{Contracts: 100, AvgPrice: 100.25}, true, testFees)
	assert.Zero(t, pos.OpenContracts())

	spotPnL := (100.7 - 0.1007) - 1.0021*100.3
	futPnL := 100.25 - 100 - (100+100.25)*0.0005
	assert.InDelta(t, spotPnL+futPnL, pos.RealizedPnL(), 1e-9)
}

func TestPositionExternalFees(t *testing.T) {
	t.Parallel()
	pos := newPosition(EntryPlan{Inst: testInstrument()}, time.Unix(0, 0))
	pos.recordSpot(SpotFill{Qty: 1, AvgPrice: 100, FeeInOtherCurrency: true}, true, testFees)
	assert.Equal(t, 1.0, pos.SpotExposure(), "fees paid in GT leave the base untouched")
	assert.InDelta(t, -0.1, pos.RealizedPnL()+pos.SpotCost, 1e-12, "but still count against profit")
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, DefaultConfig().Validate())
	cfg := DefaultConfig()
	cfg.ExitSpread = cfg.MinEntrySpread
	assert.Error(t, cfg.Validate())
}
