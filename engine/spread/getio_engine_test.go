package spread

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/samuael/trading_bot/engine/collections"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

var e exchange.IBotExchange

func TestMain(m *testing.M) {
	e = new(gateio.Exchange)
	err := Setup(e, "../../config_example.json")
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(m.Run())
}

func TestGetFuturesAssetsFromExchange(t *testing.T) {
	t.Parallel()
	assets := GetFuturesAssetsFromExchange(e)
	for i := range assets {
		println(assets[i].String(), ",")
	}
}

func TestUpdateExchangeTradingDetails(t *testing.T) {
	t.Parallel()
	details := new(collections.TradingDetail)
	details.PerpetualInstrumentsList = make(map[collections.PairInfo]map[asset.Item]collections.InstrumentInfo)

	err := details.UpdateExchangeTradingDetails(context.Background(), e, currency.USDT, asset.Spot, asset.USDTMarginedFutures)
	require.NoError(t, err)
	assert.NotEmpty(t, details.PerpetualInstrumentsList)

	matches := details.UpdateMatches()

	for k := range matches {
		println(matches[k].SpotSymbol, matches[k].FutureSymbol, "\n\n")
		// break
	}
	println("Len: ", len(matches))
}
