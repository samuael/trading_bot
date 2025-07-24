package collections

import (
	"context"

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

func (detail *TradingDetail) UpdateExchangeTradingDetails(ctx context.Context, exch exchange.IBotExchange, settlement currency.Code, assets ...asset.Item) error {
	for i := range assets {
		pairs, err := exch.GetAvailablePairs(assets[i])
		if err != nil {
			return err
		}
		pairFormat, err := exch.GetPairFormat(assets[i], true)
		if err != nil {
			return err
		}
		for _, p := range pairs {
			if p.Quote != settlement {
				continue
			}
			if detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] == nil {
				detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}] = make(map[asset.Item]InstrumentInfo)
			}
			detail.PerpetualInstrumentsList[PairInfo{p.Base, p.Quote}][assets[i]] = InstrumentInfo{
				Symbol: pairFormat.Format(p),
				Perpetual: func() bool {
					if assets[i] == asset.Spot {
						return true
					}
					isPerp, err := exch.(*gateio.Exchange).IsPerpetualFutureCurrency(assets[i], p.Format(pairFormat))
					if err != nil {
						return false
					}
					return isPerp
				}(),
			}
		}
	}
	return nil
}

type MatchInfo struct {
	Base  currency.Code
	Quote currency.Code

	SpotSymbol   string
	FutureSymbol string

	FuturesDetail kline.Item
	SpotCandles   kline.Item
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
