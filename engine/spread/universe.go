package spread

import (
	"math"
	"sort"
	"strings"

	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

// prefilterFactor admits candidates whose ticker basis is slightly under the entry threshold, because tickers use
// the perp last trade price while entries are decided on executable orderbook prices.
const prefilterFactor = 0.8

func usdtPair(base string) currency.Pair {
	return currency.NewPairWithDelimiter(strings.ToUpper(base), settlement.String(), currency.UnderscoreDelimiter)
}

// buildUniverse joins tradable USDT spot pairs with live USDT margined perpetuals of the same base, and marks the
// pairs that support isolated margin borrowing.
func buildUniverse(spotPairs []gateio.CurrencyPairDetail, contracts []gateio.FuturesContract, marginPairs []MarginPair) map[string]Instrument {
	margin := make(map[string]MarginPair, len(marginPairs))
	for _, m := range marginPairs {
		margin[m.Pair] = m
	}
	spotByBase := make(map[string]gateio.CurrencyPairDetail)
	for _, sp := range spotPairs {
		if !sp.Quote.Equal(settlement) ||
			!strings.EqualFold(sp.TradeStatus, "tradable") ||
			(sp.Type != "" && !strings.EqualFold(sp.Type, "normal")) ||
			sp.STTag ||
			!sp.DelistingTime.Time().IsZero() {
			continue
		}
		spotByBase[sp.Base.Upper().String()] = sp
	}

	universe := make(map[string]Instrument)
	for _, c := range contracts {
		if c.Delisting ||
			(c.Status != "" && !strings.EqualFold(c.Status, "trading")) ||
			(c.Type != "" && !strings.EqualFold(c.Type, "direct")) ||
			!c.DelistingTime.Time().IsZero() ||
			c.QuantoMultiplier.Float64() <= 0 ||
			c.LeverageMin.Float64() > 1 ||
			!c.Name.Quote.Equal(settlement) {
			continue
		}
		base := c.Name.Base.Upper().String()
		sp, ok := spotByBase[base]
		if !ok {
			continue
		}
		inst := Instrument{
			Base:                base,
			SpotPair:            usdtPair(base),
			Contract:            usdtPair(base),
			Multiplier:          c.QuantoMultiplier.Float64(),
			MinContracts:        c.OrderSizeMin.Float64(),
			MaxContracts:        c.OrderSizeMax.Float64(),
			PriceTick:           c.OrderPriceRound.Float64(),
			SpotAmountPrecision: int(sp.AmountPrecision),
			SpotPricePrecision:  int(sp.PricePrecision),
			SpotMinBase:         sp.MinBaseAmount.Float64(),
			SpotMinQuote:        sp.MinQuoteAmount.Float64(),
		}
		if m, ok := margin[inst.SpotPair.String()]; ok {
			inst.MarginEnabled = true
			inst.MinBorrow = m.MinBorrow
		}
		universe[inst.Key()] = inst
	}
	return universe
}

type candidate struct {
	inst        Instrument
	dir         Direction
	tickerBasis float64
}

// rankCandidates uses one spot and one futures ticker snapshot to shortlist instruments worth an orderbook check,
// best ticker spread first. Carry candidates trade a perp above spot; reverse candidates, when enabled, a perp
// below spot on a margin enabled pair. skip excludes instruments already held or blocked.
func rankCandidates(universe map[string]Instrument, spot []gateio.Ticker, futures []gateio.FuturesTicker, cfg Config, skip func(key string) bool) []candidate {
	spotByPair := make(map[string]gateio.Ticker, len(spot))
	for _, t := range spot {
		spotByPair[strings.ToUpper(t.CurrencyPair)] = t
	}

	var out []candidate
	for _, ft := range futures {
		key := strings.ToUpper(ft.Contract)
		inst, ok := universe[key]
		if !ok || skip(key) {
			continue
		}
		st, ok := spotByPair[inst.SpotPair.String()]
		if !ok {
			continue
		}
		ask, bid, last := st.LowestAsk.Float64(), st.HighestBid.Float64(), ft.Last.Float64()
		if ask <= 0 || bid <= 0 || last <= 0 {
			continue
		}
		futuresVolume := ft.Volume24HQuote.Float64()
		if futuresVolume == 0 {
			futuresVolume = ft.Volume24HSettle.Float64()
		}
		if st.QuoteVolume.Float64() < cfg.MinSpotVolume24h || futuresVolume < cfg.MinFuturesVolume24h {
			continue
		}
		// A perp whose index does not track this spot market references a different asset with the same ticker.
		mid := (ask + bid) / 2
		if index := ft.IndexPrice.Float64(); index <= 0 || math.Abs(index-mid)/mid > cfg.MaxIndexDeviation {
			continue
		}

		c := candidate{inst: inst, dir: Carry, tickerBasis: basis(last, ask)}
		funding := ft.FundingRate.Float64()
		if cfg.EnableReverse && inst.MarginEnabled {
			if reverse := spreadOf(Reverse, bid, last); reverse > c.tickerBasis {
				c = candidate{inst: inst, dir: Reverse, tickerBasis: reverse}
				funding = -funding
			}
		}
		if c.tickerBasis < cfg.MinEntrySpread*prefilterFactor || c.tickerBasis > cfg.MaxEntrySpread || funding < cfg.MinFundingRate {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tickerBasis > out[j].tickerBasis })
	if len(out) > cfg.MaxCandidates {
		out = out[:cfg.MaxCandidates]
	}
	return out
}
