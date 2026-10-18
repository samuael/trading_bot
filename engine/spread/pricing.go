package spread

import (
	"math"
	"strconv"
	"strings"

	"github.com/thrasher-corp/gocryptotrader/currency"
)

// epsilon absorbs float noise when rounding quantities that should land exactly on a step.
const epsilon = 1e-9

// Instrument pairs a spot market with its USDT margined perpetual and holds the trading rules of both.
type Instrument struct {
	Base     string        `json:"base"`
	SpotPair currency.Pair `json:"spot_pair"`
	Contract currency.Pair `json:"contract"`

	// Multiplier is the base currency amount represented by one contract (quanto_multiplier).
	Multiplier   float64 `json:"multiplier"`
	MinContracts float64 `json:"min_contracts"`
	MaxContracts float64 `json:"max_contracts"`
	// PriceTick is the futures price increment (order_price_round).
	PriceTick float64 `json:"price_tick"`

	SpotAmountPrecision int     `json:"spot_amount_precision"`
	SpotPricePrecision  int     `json:"spot_price_precision"`
	SpotMinBase         float64 `json:"spot_min_base"`
	SpotMinQuote        float64 `json:"spot_min_quote"`

	// MarginEnabled is set when the spot pair supports isolated margin borrowing, which reverse positions need.
	MarginEnabled bool    `json:"margin_enabled,omitempty"`
	MinBorrow     float64 `json:"min_borrow,omitempty"`
}

// Key identifies the instrument, e.g. "BTC_USDT".
func (i Instrument) Key() string { return i.Contract.String() }

// Fees holds taker fee rates as fractions. Every leg is assumed to execute as a taker.
type Fees struct {
	SpotTaker    float64
	FuturesTaker float64
}

// RoundTrip is the fee cost of opening and closing both legs, as a fraction of notional.
func (f Fees) RoundTrip() float64 { return 2 * (f.SpotTaker + f.FuturesTaker) }

// Level is one orderbook price level. Amount is in base currency for spot books and contracts for futures books.
type Level struct {
	Price  float64
	Amount float64
}

// bookFill describes the result of consuming a book for a quantity.
type bookFill struct {
	Qty   float64 // filled quantity, less than requested when the book is too thin
	VWAP  float64
	Worst float64 // price of the last level touched
}

// walkBook consumes levels in order until qty is filled.
func walkBook(levels []Level, qty float64) bookFill {
	var fill bookFill
	var cost float64
	for _, l := range levels {
		if fill.Qty >= qty-epsilon {
			break
		}
		take := math.Min(l.Amount, qty-fill.Qty)
		if take <= 0 {
			continue
		}
		fill.Qty += take
		cost += take * l.Price
		fill.Worst = l.Price
	}
	if fill.Qty > 0 {
		fill.VWAP = cost / fill.Qty
	}
	return fill
}

// depthWithin sums the amount available at prices within maxSlippage of the best level.
// Asks are bounded above by best*(1+maxSlippage), bids below by best*(1-maxSlippage).
func depthWithin(levels []Level, maxSlippage float64, asks bool) float64 {
	if len(levels) == 0 {
		return 0
	}
	limit := levels[0].Price * (1 - maxSlippage)
	if asks {
		limit = levels[0].Price * (1 + maxSlippage)
	}
	var depth float64
	for _, l := range levels {
		if (asks && l.Price > limit) || (!asks && l.Price < limit) {
			break
		}
		depth += l.Amount
	}
	return depth
}

// basis returns the perp premium over spot as a fraction of the spot price.
func basis(futuresPrice, spotPrice float64) float64 {
	if spotPrice == 0 {
		return 0
	}
	return (futuresPrice - spotPrice) / spotPrice
}

// expectedNetReturn is the return per unit notional of entering at entryBasis, exiting at exitBasis and
// paying taker fees on all four legs.
func expectedNetReturn(entryBasis, exitBasis float64, fees Fees) float64 {
	return entryBasis - exitBasis - fees.RoundTrip()
}

func floorTo(v float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Floor(v*p+epsilon) / p
}

func ceilTo(v float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Ceil(v*p-epsilon) / p
}

func floorToStep(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return roundTo(math.Floor(v/step+epsilon)*step, stepDecimals(step))
}

func ceilToStep(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return roundTo(math.Ceil(v/step-epsilon)*step, stepDecimals(step))
}

// roundTo strips float noise such as 12.340000000000002 so prices serialise cleanly.
func roundTo(v float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Round(v*p) / p
}

// stepDecimals returns the number of decimals needed to represent a step such as 0.01 or 0.00025.
func stepDecimals(step float64) int {
	s := strconv.FormatFloat(step, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}
