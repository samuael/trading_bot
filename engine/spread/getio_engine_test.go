package spread

import (
	"context"
	"encoding/json"
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
	"github.com/thrasher-corp/gocryptotrader/exchanges/kline"
	"github.com/thrasher-corp/gocryptotrader/exchanges/order"
)

var e exchange.IBotExchange

func TestMain(m *testing.M) {
	e = new(gateio.Exchange)
	err := Setup(e, "../../config_example.json")
	if err != nil {
		log.Fatal(err)
	}
	e.(*gateio.Exchange).API.AuthenticatedSupport = true

	e.(*gateio.Exchange).Websocket.SetCanUseAuthenticatedEndpoints(true)

	e.(*gateio.Exchange).SetCredentials("", "", "", "", "", "")
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

	err := e.UpdateTradablePairs(context.Background(), true)
	require.NoError(t, err)

	spotPairsList, err := e.(*gateio.Exchange).ListSpotCurrencyPairs(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, spotPairsList)

	contracts, err := e.(*gateio.Exchange).GetAllFutureContracts(context.Background(), currency.USDT)
	require.NoError(t, err)

	err = details.UpdateExchangeTradingDetails(context.Background(), e, currency.USDT, spotPairsList, contracts)
	require.NoError(t, err)
	assert.NotEmpty(t, details.PerpetualInstrumentsList)

	_, err = UpdateStats(nil)
	require.Error(t, err)

	matches := details.UpdateMatches()
	require.NotEmpty(t, matches)

	_, err = UpdateStats(matches)
	require.ErrorIs(t, err, kline.ErrInsufficientCandleData)

	err = GetCandlesOfMatches(e.(*gateio.Exchange), matches)
	require.NoError(t, err)

	newmatched, err := UpdateStats(matches)
	require.NoError(t, err)
	assert.NotNil(t, newmatched)
}

func TestSortMatches(t *testing.T) {
	t.Parallel()
	matches := []collections.MatchInfo{
		{Diff: 9, Eligible: true},
		{Diff: 6, Eligible: true},
		{Diff: 45, Eligible: true},
		{Diff: 2, Eligible: true},
		{Diff: 7, Eligible: true},
		{Diff: 5, Eligible: true},
	}
	sorted := SortMatches(matches, 3)
	require.Len(t, sorted, 3)
	val, _ := json.Marshal(sorted)
	println(string(val))
}

func TestGetAssets(t *testing.T) {
	t.Parallel()
	spotAccount, err := e.(*gateio.Exchange).GetSpotAccounts(context.Background(), currency.USDT)
	require.NoError(t, err)

	val, _ := json.Marshal(spotAccount)
	println(string(val))

	futuresAccount, err := e.(*gateio.Exchange).QueryFuturesAccount(context.Background(), currency.USDT)
	require.NoError(t, err)

	val, _ = json.Marshal(futuresAccount)
	println(string(val))
}

func TestSubmitOrder(t *testing.T) {
	t.Parallel()
	e.(*gateio.Exchange).Verbose = true
	_, err := e.(*gateio.Exchange).SubmitOrder(context.Background(), &order.Submit{
		Exchange:    e.GetName(),
		Pair:        currency.NewPair(currency.BTC, currency.USDT),
		TimeInForce: order.FillOrKill,
		Type:        order.Market,
		AssetType:   asset.USDTMarginedFutures,
		Side:        order.Short,
		Amount:      50,
	})
	require.NoError(t, err)
}

func TestGetPositions(t *testing.T) {
	t.Parallel()
	allPositions, err := e.(*gateio.Exchange).GetAllFuturesPositionsOfUsers(context.Background(), currency.USDT, true)
	require.NoError(t, err)

	val, _ := json.Marshal(allPositions)
	println(string(val))
}

func TestCancelOrder(t *testing.T) {
	t.Parallel()
	e.(*gateio.Exchange).Verbose = true
	allPositions, err := e.(*gateio.Exchange).GetAllFuturesPositionsOfUsers(context.Background(), currency.USDT, true)
	require.NoError(t, err)

	println("Positions length: ", len(allPositions))

	for p := range allPositions {
		// println("Position with ID: ", allPositions[p].Contract, allPositions[p].Leverage)
		contract, err := currency.NewPairFromString(allPositions[p].Contract)
		require.NoError(t, err)

		var autoSize string
		if allPositions[p].Mode == "dual_long" {
			autoSize = "close_long"
		} else {
			autoSize = "close_short"
		}
		_, err = e.(*gateio.Exchange).PlaceFuturesOrder(context.Background(), &gateio.ContractOrderCreateParams{
			Contract:      contract,
			Size:          0,
			ClosePosition: false,
			// IsClose:       true,
			ReduceOnly:  true,
			TimeInForce: "ioc",
			Settle:      currency.USDT,
			AutoSize:    autoSize,
			// SelfTradePreventionAction: "",
		})
		require.NoError(t, err)
		// assert.Equal(t, true, result.)
	}
}

func TestUpdateFuturesPositionLeverage(t *testing.T) {
	t.Parallel()
	e.(*gateio.Exchange).Verbose = true
	position, err := e.(*gateio.Exchange).UpdateFuturesPositionLeverage(context.Background(), currency.USDT, currency.Pair{Base: currency.NewCode("SHM"), Quote: currency.USDT, Delimiter: currency.UnderscoreDelimiter}, 0, 1)
	require.NoError(t, err)
	assert.NotNil(t, position)
}

func TestGetFuturesOrderbook(t *testing.T) {
	t.Parallel()
	e.(*gateio.Exchange).Verbose = true
	result, err := e.(*gateio.Exchange).GetFuturesOrderbook(context.Background(), currency.USDT, "BTC_USDT", "10", 10, false)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestGetOrderbook(t *testing.T) {
	t.Parallel()
	e.(*gateio.Exchange).Verbose = true
	result, err := e.(*gateio.Exchange).GetOrderbook(context.Background(), "BTC_USDT", "", 0, false)
	require.NoError(t, err)
	assert.NotNil(t, result)
}
