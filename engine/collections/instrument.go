package collections

import (
	"context"
	"strings"

	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/exchanges/kline"
)

// type CurrencyList struct {
// 	CurrencyToAssetMap map[currency.Pair] asset.Item
// }

type InstrumentInfo struct {
	Symbol    string
	Perpetual bool
	Kline     kline.Item
}

type PairInfo struct {
	Base  currency.Code
	Quote currency.Code
}

// TradingDetail holds exchange currency pairs and relevant details.
type TradingDetail struct {
	Exchange                 string
	PerpetualInstrumentsList map[PairInfo]map[asset.Item]InstrumentInfo
	Matches                  map[PairInfo]map[asset.Item]InstrumentInfo
}

func (detail *TradingDetail) UpdateExchangeTradingDetails(ctx context.Context, exch exchange.IBotExchange, settlement currency.Code, spotPairs []gateio.CurrencyPairDetail, contracts []gateio.FuturesContract) error {
	err := exch.UpdateTradablePairs(ctx, true)
	if err != nil {
		return err
	}

	spotCount := 0
	futuresCount := 0

	pairFormat, err := exch.GetPairFormat(asset.Spot, true)
	if err != nil {
		return err
	}

	for _, sp := range spotPairs {
		if (sp.Type != "" && !strings.EqualFold(sp.Type, "normal")) || !sp.DelistingTime.Time().IsZero() || (sp.TradeStatus != "" && !strings.EqualFold(sp.TradeStatus, "tradable")) {
			continue
		}
		if !strings.EqualFold(settlement.String(), currency.NewCode(sp.Quote).String()) {
			continue
		}
		p, err := exch.MatchSymbolWithAvailablePairs(sp.ID /* sp.Base+"_"+sp.Quote */, asset.Spot, true)
		if err != nil {
			return err
		}
		p = p.Format(pairFormat)

		if detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] == nil {
			detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] = make(map[asset.Item]InstrumentInfo)
		}

		spotCount += 1
		detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}][asset.Spot] = InstrumentInfo{
			Symbol:    sp.ID,
			Perpetual: true,
		}
	}

	futuresFormat, err := exch.GetPairFormat(asset.USDTMarginedFutures, true)
	if err != nil {
		return err
	}
	var average int64
	var count int64
	for _, contract := range contracts {
		if contract.InDelisting || (contract.Type != "" && !strings.EqualFold(contract.Type, "direct")) || contract.LeverageMin > 1 {
			continue
		}
		pairString := strings.ToUpper(contract.Name)
		if !exch.(*gateio.Exchange).IsValidPairString(pairString) {
			continue
		}
		p, err := currency.NewPairFromString(pairString)
		if err != nil {
			return err
		}
		// if count > 0 {
		// 	if contract.TradeSize < int64(math.Ceil(float64(average/count)*0.3)) {
		// 		continue
		// 	}
		// }
		average += contract.TradeSize
		count += 1
		p = p.Format(futuresFormat)
		if detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] == nil {
			detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] = make(map[asset.Item]InstrumentInfo)
		}
		futuresCount += 1
		detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}][asset.USDTMarginedFutures] = InstrumentInfo{
			Symbol:    contract.Name,
			Perpetual: true,
		}
	}
	println("Spot Count: ", spotCount, " Futures Count: ", futuresCount, "\n")
	return nil
}

type MatchInfo struct {
	Base  currency.Code
	Quote currency.Code

	SpotSymbol   string
	FutureSymbol string

	FuturesCandles *kline.Item
	SpotCandles    *kline.Item

	Spread     float64
	LastSpread float64

	Eligible bool
	Diff     float64
}

func (detail *TradingDetail) UpdateMatches() []MatchInfo {
	matches := make([]MatchInfo, 0, len(detail.PerpetualInstrumentsList))
	for b := range detail.PerpetualInstrumentsList {
		if len(detail.PerpetualInstrumentsList[b]) == 2 {
			valid := true
			for a := range detail.PerpetualInstrumentsList[b] {
				valid = valid && detail.PerpetualInstrumentsList[b][a].Perpetual
			}
			if valid {
				matches = append(matches, MatchInfo{
					Base:         b.Base,
					Quote:        b.Quote,
					SpotSymbol:   detail.PerpetualInstrumentsList[b][asset.Spot].Symbol,
					FutureSymbol: detail.PerpetualInstrumentsList[b][asset.USDTMarginedFutures].Symbol,
				})
			}
		}
	}
	return matches
}
