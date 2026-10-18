//go:build livetrade

// Live round trip with real money: opens one minimum sized carry position on DOGE_USDT through the engine's own
// executor, verifies both legs on the exchange, closes it and verifies the account is flat. Entry thresholds are
// relaxed so the test does not wait for a real opportunity; it costs the round trip fees and spread (cents).
//
//	GATEIO_API_KEY=... GATEIO_API_SECRET=... go test -tags livetrade -run TestLiveTradeRoundTrip -v -timeout 15m ./engine/spread
package spread

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func TestLiveTradeRoundTrip(t *testing.T) {
	key, secret := os.Getenv("GATEIO_API_KEY"), os.Getenv("GATEIO_API_SECRET")
	require.True(t, key != "" && secret != "", "GATEIO_API_KEY and GATEIO_API_SECRET must be set")
	exch := new(gateio.Exchange)
	require.NoError(t, Setup(exch, "../../config_example.json"))
	exch.API.AuthenticatedSupport = true
	exch.SetCredentials(key, secret, "", "", "", "")
	require.NoError(t, exch.SetHTTPClientTimeout(time.Minute))
	client := NewGateIOClient(exch, time.Minute)

	dir := os.Getenv("LIVETRADE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	cfg := DefaultConfig()
	cfg.DryRun = false
	cfg.UseWebsocket = false
	cfg.MaxOpenPositions = 1
	cfg.MaxTradeNotional = 5
	cfg.StateFile = filepath.Join(dir, "positions.json")
	cfg.LedgerFile = filepath.Join(dir, "trades.csv")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, err := NewEngine(cfg, client, nil, logger, NewMetrics(prometheus.NewRegistry()), nil)
	require.NoError(t, err)
	ctx := context.Background()

	const instKey = "DOGE_USDT"
	base := currency.NewCode("DOGE")
	logBalances := func(stage string) {
		usdt, _, err := client.SpotBalance(ctx, settlement)
		require.NoError(t, err)
		doge, _, err := client.SpotBalance(ctx, base)
		require.NoError(t, err)
		acc, err := client.FuturesAccount(ctx)
		require.NoError(t, err)
		t.Logf("[%s] spot USDT=%.6f spot DOGE=%.4f futures available=%.6f total=%.6f dual_mode=%v",
			stage, usdt, doge, acc.Available.Float64(), acc.Total.Float64(), acc.InDualMode)
	}
	perpSize := func() float64 {
		positions, err := client.Positions(ctx)
		require.NoError(t, err)
		for _, p := range positions {
			if strings.EqualFold(p.Contract, instKey) {
				return p.Size
			}
		}
		return 0
	}

	logBalances("start")
	require.Zero(t, perpSize(), "refusing to run with an existing DOGE perp position")
	require.NoError(t, client.EnsureSinglePositionMode(ctx))
	require.NoError(t, eng.refreshUniverse(ctx))
	inst, ok := eng.universe[instKey]
	require.True(t, ok)
	t.Logf("instrument %+v", inst)

	f, err := eng.freeFunds(ctx) // rebalances spot and futures for real
	require.NoError(t, err)
	logBalances("after rebalance")
	budget := tradeBudget(f.usable(Carry, cfg), 1, cfg)
	require.Positive(t, budget, "not enough funds for a minimum position")
	fees, err := eng.feesFor(ctx, inst)
	require.NoError(t, err)
	_, spotAsks, err := client.SpotBook(ctx, inst.SpotPair, cfg.OrderbookDepth)
	require.NoError(t, err)
	futuresBids, _, err := client.FuturesBook(ctx, inst.Contract, cfg.OrderbookDepth)
	require.NoError(t, err)

	testCfg := cfg
	testCfg.MinEntrySpread, testCfg.MinNetProfit = -0.01, -1 // accept today's spread whatever it is
	plan, err := planEntry(Carry, inst, spotAsks, futuresBids, fees, 0, budget, testCfg)
	require.NoError(t, err)
	t.Logf("plan: %d contracts, %.2f DOGE spot, notional %.4f USDT, spread %.4f%%, fees %+v, spot limit %v, perp limit %v",
		int(plan.Contracts), plan.SpotQty, plan.Notional, plan.Basis*100, fees, plan.SpotLimit, plan.FuturesLimit)

	// Whatever happens below, leave the account flat.
	closeAll := func(reason string, market bool) {
		for attempt := 0; attempt < 8; attempt++ {
			pos, ok := eng.positions[instKey]
			if !ok {
				return
			}
			if market {
				pos.CloseAttempts = max(pos.CloseAttempts, marketCloseAfter)
			}
			spotBids, _, err := client.SpotBook(ctx, inst.SpotPair, cfg.OrderbookDepth)
			require.NoError(t, err)
			_, futuresAsks, err := client.FuturesBook(ctx, inst.Contract, cfg.OrderbookDepth)
			require.NoError(t, err)
			exit, err := planExit(Carry, inst, pos.OpenContracts(), pos.SpotExposure(), spotBids, futuresAsks, cfg)
			require.NoError(t, err)
			t.Logf("close attempt %d: %v contracts, %.4f DOGE, exit spread %.4f%%", attempt+1, exit.Contracts, exit.SpotQty, exit.Basis*100)
			if err := eng.close(ctx, pos, exit, reason); err != nil {
				t.Logf("close attempt %d: %v", attempt+1, err)
			}
		}
	}
	defer func() {
		if _, open := eng.positions[instKey]; open {
			t.Log("cleanup: flattening at market")
			closeAll("live test cleanup", true)
		}
	}()

	pos, err := eng.open(ctx, plan)
	require.NoError(t, err)
	require.NotNil(t, pos, "perp leg did not fill at its limit")
	t.Logf("opened: contracts sold %v at notional %.6f, spot bought %.4f DOGE for %.6f USDT, fees spot %.6f perp %.6f",
		pos.ContractsSold, pos.FutSellNotional, pos.SpotBought, pos.SpotCost, pos.SpotFees, pos.FutFees)
	logBalances("open")

	assert.Equal(t, pos.OpenContracts(), pos.HedgedContracts(), "spot covers the short")
	assert.Equal(t, -pos.OpenContracts(), perpSize(), "exchange perp matches the record")
	doge, _, err := client.SpotBalance(ctx, base)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, doge+1e-9, pos.SpotExposure(), "exchange spot holds the hedge")

	closeAll("live round trip test", false)
	_, stillOpen := eng.positions[instKey]
	require.False(t, stillOpen, "position did not close")
	assert.Zero(t, perpSize(), "perp flat")
	logBalances("closed")

	rows := ledgerRows(t, cfg.LedgerFile)
	require.Len(t, rows, 1)
	for _, k := range ledgerHeader {
		t.Logf("ledger %-18s %s", k, rows[0][k])
	}
	assert.Less(t, math.Abs(pos.RealizedPnL()), 0.5, "a minimum round trip costs cents")
}
