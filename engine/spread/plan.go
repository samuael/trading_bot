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

// Direction is which side of the spot/perp spread a position is on.
type Direction string

const (
	// Carry buys spot and shorts the perp, earning the premium when the perp trades above spot.
	Carry Direction = "carry"
	// Reverse sells spot borrowed on isolated margin and longs the perp, earning the discount when the perp trades
	// below spot.
	Reverse Direction = "reverse"
)

// spreadOf is the executable spread in the direction's favour, as a fraction of the spot price.
func spreadOf(dir Direction, spotPrice, futuresPrice float64) float64 {
	if dir == Reverse {
		return -basis(futuresPrice, spotPrice)
	}
	return basis(futuresPrice, spotPrice)
}

// EntryPlan is a sized, priced entry. Carry buys spot and shorts the perp; reverse sells borrowed spot and longs
// the perp.
type EntryPlan struct {
	Direction Direction
	Inst      Instrument
	Fees      Fees

	Contracts float64 // perp contracts to trade
	SpotQty   float64 // base to trade; carry buys include an allowance for a fee deducted in base

	SpotVWAP     float64
	FuturesVWAP  float64
	SpotLimit    float64 // IOC limit for the spot leg
	FuturesLimit float64 // IOC limit for the perp leg

	Basis        float64 // gross executable entry spread in the direction's favour
	InterestCost float64 // expected borrow interest over cfg.ExpectedHoldHours, reverse only
	ExpectedNet  float64 // Basis less exit target, round trip fees and interest
	Notional     float64 // USDT value of the spot leg
}

// planEntry sizes a position within budget (USDT) and the liquidity available within cfg.MaxSlippage, then
// accepts it only when the gross spread reaches cfg.MinEntrySpread and the return after round trip fees and
// expected borrow interest, assuming an exit at cfg.ExitSpread, reaches cfg.MinNetProfit.
//
// spotBook and futuresBook are the sides the entry trades against: spot asks and perp bids for carry, spot bids
// and perp asks for reverse. hourlyBorrowRate only applies to reverse.
func planEntry(dir Direction, inst Instrument, spotBook, futuresBook []Level, fees Fees, hourlyBorrowRate, budget float64, cfg Config) (EntryPlan, error) {
	plan := EntryPlan{Direction: dir, Inst: inst, Fees: fees}
	if len(spotBook) == 0 || len(futuresBook) == 0 || inst.Multiplier <= 0 {
		return plan, errNoLiquidity
	}
	carry := dir == Carry

	// A carry buy loses its fee in base, so less base is bought per USDT.
	feeAdjust := 1.0
	if carry {
		feeAdjust = 1 - fees.SpotTaker
	}
	base := budget * feeAdjust / spotBook[0].Price
	base = min(base, depthWithin(spotBook, cfg.MaxSlippage, carry)*feeAdjust)
	base = min(base, depthWithin(futuresBook, cfg.MaxSlippage, !carry)*inst.Multiplier)

	contracts := math.Floor(base/inst.Multiplier + epsilon)
	if inst.MaxContracts > 0 {
		contracts = min(contracts, inst.MaxContracts)
	}
	if contracts < max(1, inst.MinContracts) {
		return plan, errBelowMinSize
	}

	spotQty := ceilTo(contracts*inst.Multiplier, inst.SpotAmountPrecision)
	if carry {
		// Buy enough spot that the base left after a fee deducted in base still covers the perp short.
		spotQty = grossSpotQty(contracts*inst.Multiplier, fees, inst)
	}
	spot := walkBook(spotBook, spotQty)
	fut := walkBook(futuresBook, contracts)
	if spot.Qty < spotQty-epsilon || fut.Qty < contracts-epsilon {
		return plan, errNoLiquidity
	}

	plan.Contracts = contracts
	plan.SpotQty = spotQty
	plan.SpotVWAP = spot.VWAP
	plan.FuturesVWAP = fut.VWAP
	plan.Notional = spotQty * spot.VWAP
	// Rounding down to whole contracts may leave a minimum sized budget up to one contract short of the minimum.
	contractValue := inst.Multiplier * spot.VWAP
	if spotQty < inst.SpotMinBase || plan.Notional < inst.SpotMinQuote || plan.Notional+contractValue < cfg.MinTradeNotional {
		return plan, errBelowMinSize
	}
	if !carry && spotQty < inst.MinBorrow {
		return plan, errBelowMinSize
	}

	plan.Basis = spreadOf(dir, spot.VWAP, fut.VWAP)
	if !carry {
		plan.InterestCost = hourlyBorrowRate * cfg.ExpectedHoldHours
	}
	plan.ExpectedNet = expectedNetReturn(plan.Basis, cfg.ExitSpread, fees) - plan.InterestCost
	if plan.Basis < cfg.MinEntrySpread {
		return plan, fmt.Errorf("%w: %.4f%% < %.4f%%", errSpreadTooSmall, plan.Basis*100, cfg.MinEntrySpread*100)
	}
	if plan.Basis > cfg.MaxEntrySpread {
		return plan, fmt.Errorf("%w: %.4f%% > %.4f%%", errSpreadTooLarge, plan.Basis*100, cfg.MaxEntrySpread*100)
	}
	if plan.ExpectedNet < cfg.MinNetProfit {
		return plan, fmt.Errorf("%w: %.4f%% < %.4f%% (round trip fees %.4f%%, interest %.4f%%)",
			errNetTooSmall, plan.ExpectedNet*100, cfg.MinNetProfit*100, fees.RoundTrip()*100, plan.InterestCost*100)
	}

	// The perp leg goes first and must not fill worse than planned; the spot leg hedges it and gets an allowance.
	if carry {
		plan.FuturesLimit = floorToStep(fut.Worst, inst.PriceTick)
		plan.SpotLimit = ceilTo(spot.Worst*(1+cfg.HedgeSlippage), inst.SpotPricePrecision)
	} else {
		plan.FuturesLimit = ceilToStep(fut.Worst, inst.PriceTick)
		plan.SpotLimit = floorTo(spot.Worst*(1-cfg.HedgeSlippage), inst.SpotPricePrecision)
	}
	return plan, nil
}

// ExitPlan prices closing what remains of a position.
type ExitPlan struct {
	Contracts float64
	SpotQty   float64

	SpotVWAP     float64
	FuturesVWAP  float64
	SpotLimit    float64
	FuturesLimit float64

	// Basis is the executable exit spread in the position's favour. It is only meaningful while contracts remain.
	Basis float64
}

// planExit prices the remaining contracts and spot exposure against the current books. spotBook and futuresBook
// are the sides the exit trades against: spot bids and perp asks for carry, spot asks and perp bids for reverse.
// For reverse, spotQty is the base to buy back.
func planExit(dir Direction, inst Instrument, contracts, spotQty float64, spotBook, futuresBook []Level, cfg Config) (ExitPlan, error) {
	carry := dir == Carry
	plan := ExitPlan{Contracts: contracts, SpotQty: floorTo(spotQty, inst.SpotAmountPrecision)}
	if !carry {
		plan.SpotQty = ceilTo(spotQty, inst.SpotAmountPrecision)
	}
	if len(spotBook) == 0 || (contracts > 0 && len(futuresBook) == 0) {
		return plan, errNoLiquidity
	}

	spot := walkBook(spotBook, plan.SpotQty)
	if spot.Qty == 0 {
		spot = bookFill{VWAP: spotBook[0].Price, Worst: spotBook[0].Price}
	}
	plan.SpotVWAP = spot.VWAP
	if carry {
		plan.SpotLimit = floorTo(spot.Worst*(1-cfg.ExitSlippage), inst.SpotPricePrecision)
	} else {
		plan.SpotLimit = ceilTo(spot.Worst*(1+cfg.ExitSlippage), inst.SpotPricePrecision)
	}

	if contracts > 0 {
		fut := walkBook(futuresBook, contracts)
		plan.FuturesVWAP = fut.VWAP
		if carry {
			plan.FuturesLimit = ceilToStep(fut.Worst*(1+cfg.ExitSlippage), inst.PriceTick)
		} else {
			plan.FuturesLimit = floorToStep(fut.Worst*(1-cfg.ExitSlippage), inst.PriceTick)
		}
		plan.Basis = spreadOf(dir, spot.VWAP, fut.VWAP)
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
