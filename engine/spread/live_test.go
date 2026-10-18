//go:build live

// Live checks against GateIO public endpoints. Run with: go test -tags live -run Live -v ./engine/spread
package spread

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func TestLivePublicMarketData(t *testing.T) {
	e := new(gateio.Exchange)
	require.NoError(t, Setup(e, "../../config_example.json"))
	c := NewGateIOClient(e)
	ctx := t.Context()

	spotPairs, err := c.SpotPairs(ctx)
	require.NoError(t, err)
	contracts, err := c.Contracts(ctx)
	require.NoError(t, err)
	universe := buildUniverse(spotPairs, contracts)
	require.NotEmpty(t, universe)
	t.Logf("%d spot pairs, %d contracts, %d matched instruments", len(spotPairs), len(contracts), len(universe))

	btc, ok := universe["BTC_USDT"]
	require.True(t, ok)
	t.Logf("BTC instrument: %+v", btc)
	assert.Positive(t, btc.Multiplier)
	assert.Positive(t, btc.PriceTick)

	spotBids, spotAsks, err := c.SpotBook(ctx, btc.SpotPair, 10)
	require.NoError(t, err)
	futuresBids, futuresAsks, err := c.FuturesBook(ctx, btc.Contract, 10)
	require.NoError(t, err)
	require.NotEmpty(t, spotBids)
	require.NotEmpty(t, futuresAsks)
	assert.Greater(t, spotAsks[0].Price, spotBids[0].Price)
	assert.Greater(t, futuresAsks[0].Price, futuresBids[0].Price)
	t.Logf("BTC spot %v/%v perp %v/%v, entry basis %.4f%%", spotBids[0].Price, spotAsks[0].Price,
		futuresBids[0].Price, futuresAsks[0].Price, basis(futuresBids[0].Price, spotAsks[0].Price)*100)

	spotTickers, err := c.SpotTickers(ctx)
	require.NoError(t, err)
	futuresTickers, err := c.FuturesTickers(ctx)
	require.NoError(t, err)
	cfg := DefaultConfig()
	cfg.MinEntrySpread = 0.002 // show something even on a quiet market
	cfg.ExitSpread = 0
	for _, cand := range rankCandidates(universe, spotTickers, futuresTickers, cfg, func(string) bool { return false }) {
		t.Logf("candidate %s ticker basis %.4f%%", cand.inst.Key(), cand.tickerBasis*100)
	}
}
