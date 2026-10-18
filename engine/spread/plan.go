package spread

import (
	"errors"
	"fmt"
	"math"
)

var (
	errNoLiquidity    = errors.New("insufficient orderbook liquidity")
	errBelowMinSize   = errors.New("position below exchange or configured minimum size")
	errSpreadTooSmall = errors.New("entry spread below minimum")
	errSpreadTooLarge = errors.New("entry spread above maximum, markets likely track different assets")
	errNetTooSmall    = errors.New("expected net profit below minimum")
)

// EntryPlan is a sized, priced cash and carry entry: buy spot, short the perpetual.
type EntryPlan struct {
	Inst Instrument
	Fees Fees

	Contracts float64 // perp contracts to short
	SpotQty   float64 // base to buy, including an allowance for a fee deducted in base

	SpotVWAP     float64
	FuturesVWAP  float64
	SpotLimit    float64 // IOC limit for the spot buy
	FuturesLimit float64 // IOC limit for the perp sell

	Basis       float64 // gross executable entry basis
	ExpectedNet float64 // Basis less exit target and round trip fees
	Notional    float64 // USDT spent on the spot leg
}

// planEntry sizes a position within budget (USDT) and the liquidity available within cfg.MaxSlippage, then
// accepts it only when the gross basis reaches cfg.MinEntrySpread and the return after round trip fees, assuming
// an exit at cfg.ExitSpread, reaches cfg.MinNetProfit.
func planEntry(inst Instrument, spotAsks, futuresBids []Level, fees Fees, budget float64, cfg Config) (EntryPlan, error) {
	plan := EntryPlan{Inst: inst, Fees: fees}
	if len(spotAsks) == 0 || len(futuresBids) == 0 || inst.Multiplier <= 0 {
		return plan, errNoLiquidity
	}

	base := budget * (1 - fees.SpotTaker) / spotAsks[0].Price
	base = min(base, depthWithin(spotAsks, cfg.MaxSlippage, true)*(1-fees.SpotTaker))
	base = min(base, depthWithin(futuresBids, cfg.MaxSlippage, false)*inst.Multiplier)

	contracts := math.Floor(base/inst.Multiplier + epsilon)
	if inst.MaxContracts > 0 {
		contracts = min(contracts, inst.MaxContracts)
	}
	if contracts < max(1, inst.MinContracts) {
		return plan, errBelowMinSize
	}

	// Buy enough spot that the base left after a fee deducted in base still covers the perp short.
	spotQty := grossSpotQty(contracts*inst.Multiplier, fees, inst)
	spot := walkBook(spotAsks, spotQty)
	fut := walkBook(futuresBids, contracts)
	if spot.Qty < spotQty-epsilon || fut.Qty < contracts-epsilon {
		return plan, errNoLiquidity
	}

	plan.Contracts = contracts
	plan.SpotQty = spotQty
	plan.SpotVWAP = spot.VWAP
	plan.FuturesVWAP = fut.VWAP
	plan.Notional = spotQty * spot.VWAP
	if spotQty < inst.SpotMinBase || plan.Notional < inst.SpotMinQuote || plan.Notional < cfg.MinTradeNotional {
		return plan, errBelowMinSize
	}

	plan.Basis = basis(fut.VWAP, spot.VWAP)
	plan.ExpectedNet = expectedNetReturn(plan.Basis, cfg.ExitSpread, fees)
	if plan.Basis < cfg.MinEntrySpread {
		return plan, fmt.Errorf("%w: %.4f%% < %.4f%%", errSpreadTooSmall, plan.Basis*100, cfg.MinEntrySpread*100)
	}
	if plan.Basis > cfg.MaxEntrySpread {
		return plan, fmt.Errorf("%w: %.4f%% > %.4f%%", errSpreadTooLarge, plan.Basis*100, cfg.MaxEntrySpread*100)
	}
	if plan.ExpectedNet < cfg.MinNetProfit {
		return plan, fmt.Errorf("%w: %.4f%% < %.4f%% (round trip fees %.4f%%)", errNetTooSmall, plan.ExpectedNet*100, cfg.MinNetProfit*100, fees.RoundTrip()*100)
	}

	// The perp leg goes first and must not fill worse than planned; the spot leg hedges it and gets an allowance.
	plan.FuturesLimit = floorToStep(fut.Worst, inst.PriceTick)
	plan.SpotLimit = ceilTo(spot.Worst*(1+cfg.HedgeSlippage), inst.SpotPricePrecision)
	return plan, nil
}

// ExitPlan prices closing what remains of a position: sell spot, buy back the perpetual.
type ExitPlan struct {
	Contracts float64
	SpotQty   float64

	SpotVWAP     float64
	FuturesVWAP  float64
	SpotLimit    float64
	FuturesLimit float64

	// Basis is the executable exit basis. It is only meaningful while contracts remain.
	Basis float64
}

// planExit prices the remaining contracts and spot holding against the current books.
func planExit(inst Instrument, contracts, spotHeld float64, spotBids, futuresAsks []Level, cfg Config) (ExitPlan, error) {
	plan := ExitPlan{Contracts: contracts, SpotQty: floorTo(spotHeld, inst.SpotAmountPrecision)}
	if len(spotBids) == 0 || (contracts > 0 && len(futuresAsks) == 0) {
		return plan, errNoLiquidity
	}

	spot := walkBook(spotBids, plan.SpotQty)
	if spot.Qty == 0 {
		spot = bookFill{VWAP: spotBids[0].Price, Worst: spotBids[0].Price}
	}
	plan.SpotVWAP = spot.VWAP
	plan.SpotLimit = floorTo(spot.Worst*(1-cfg.ExitSlippage), inst.SpotPricePrecision)

	if contracts > 0 {
		fut := walkBook(futuresAsks, contracts)
		plan.FuturesVWAP = fut.VWAP
		plan.FuturesLimit = ceilToStep(fut.Worst*(1+cfg.ExitSlippage), inst.PriceTick)
		plan.Basis = basis(fut.VWAP, spot.VWAP)
	}
	return plan, nil
}

// grossSpotQty is the spot amount to buy so that base remains after a taker fee deducted in base.
func grossSpotQty(base float64, fees Fees, inst Instrument) float64 {
	return ceilTo(base/(1-fees.SpotTaker), inst.SpotAmountPrecision)
}

// tradeBudget splits usable capital evenly over the free position slots, so each new position is as large as
// funds allow while leaving room for the remaining slots. It returns zero when no viable position fits.
func tradeBudget(usable float64, slots int, cfg Config) float64 {
	if slots <= 0 || usable < cfg.MinTradeNotional {
		return 0
	}
	budget := max(usable/float64(slots), cfg.MinTradeNotional)
	if cfg.MaxTradeNotional > 0 {
		budget = min(budget, cfg.MaxTradeNotional)
	}
	return budget
}
