package collections

import (
	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/kline"
)

type AssetCandles struct {
	Asset  asset.Item
	Pair   currency.Pair
	Klines *kline.Item
}

type Candles struct {
	PairCandles []AssetCandles
}
