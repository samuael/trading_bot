//go:build live

// Live checks against GateIO public endpoints. Run with: go test -tags live -run Live -v ./engine/spread
package spread

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func TestLivePublicMarketData(t *testing.T) {
	e := new(gateio.Exchange)
	require.NoError(t, Setup(e, "../../config_example.json"))
	require.NoError(t, e.SetHTTPClientTimeout(time.Minute))
	c := NewGateIOClient(e, time.Minute)
	ctx := t.Context()

	spotPairs, err := c.SpotPairs(ctx)
	require.NoError(t, err)
	contracts, err := c.Contracts(ctx)
	require.NoError(t, err)
	marginPairs, err := c.MarginPairs(ctx)
	require.NoError(t, err)
	universe := buildUniverse(spotPairs, contracts, marginPairs)
	require.NotEmpty(t, universe)
	t.Logf("%d spot pairs, %d contracts, %d margin pairs, %d matched instruments", len(spotPairs), len(contracts), len(marginPairs), len(universe))

	btc, ok := universe["BTC_USDT"]
	require.True(t, ok)
	t.Logf("BTC instrument: %+v", btc)
	assert.Positive(t, btc.Multiplier)
	assert.Positive(t, btc.PriceTick)
	assert.True(t, btc.MarginEnabled)

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
		t.Logf("candidate %s %s ticker spread %.4f%%", cand.inst.Key(), cand.dir, cand.tickerBasis*100)
	}
}

func TestLiveFeed(t *testing.T) {
	e := new(gateio.Exchange)
	require.NoError(t, Setup(e, "../../config_example.json"))
	require.NoError(t, e.SetHTTPClientTimeout(time.Minute))
	rest := NewGateIOClient(e, time.Minute)
	metrics := NewMetrics(prometheus.NewRegistry())
	feed := NewFeed(rest, DefaultConfig(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), metrics)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go feed.Run(ctx)

	keys := []string{"BTC_USDT", "ETH_USDT"}
	feed.Track(keys, keys)
	_, _ = feed.SpotTickers(t.Context()) // seed
	_, _ = feed.FuturesTickers(t.Context())
	time.Sleep(8 * time.Second)

	for _, key := range keys {
		pair := usdtPair(strings.TrimSuffix(key, "_USDT"))
		sb, sa, err := feed.SpotBook(t.Context(), pair, 20)
		require.NoError(t, err)
		fb, fa, err := feed.FuturesBook(t.Context(), pair, 20)
		require.NoError(t, err)
		require.NotEmpty(t, sb)
		require.NotEmpty(t, fa)
		assert.Greater(t, sa[0].Price, sb[0].Price, "spot book not crossed")
		assert.Greater(t, fa[0].Price, fb[0].Price, "futures book not crossed")
		_, ra, err := rest.FuturesBook(t.Context(), pair, 20)
		require.NoError(t, err)
		assert.InDelta(t, ra[0].Price, fa[0].Price, ra[0].Price*0.002, "streamed futures book agrees with REST")
		t.Logf("%s spot %v/%v (%d levels) perp %v/%v (%d levels)", key, sb[0].Price, sa[0].Price, len(sb), fb[0].Price, fa[0].Price, len(fb))
	}
	spot, err := feed.SpotTickers(t.Context())
	require.NoError(t, err)
	fut, err := feed.FuturesTickers(t.Context())
	require.NoError(t, err)
	t.Logf("tickers served: spot %d, futures %d", len(spot), len(fut))
	for _, kind := range [][2]string{{"book", "spot"}, {"book", "futures"}, {"tickers", "spot"}, {"tickers", "futures"}} {
		ws := testutil.ToFloat64(metrics.MarketData.WithLabelValues(kind[0], kind[1], "ws"))
		t.Logf("%s %s served from ws: %v, rest: %v", kind[1], kind[0], ws, testutil.ToFloat64(metrics.MarketData.WithLabelValues(kind[0], kind[1], "rest")))
		assert.Positive(t, ws)
	}
}
