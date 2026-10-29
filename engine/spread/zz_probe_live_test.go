//go:build live

package spread

import (
	"os"
	"testing"
	"time"

	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func TestZZProbeState(t *testing.T) {
	e := new(gateio.Exchange)
	if err := Setup(e, "../../config_example.json"); err != nil {
		t.Fatal(err)
	}
	e.API.AuthenticatedSupport = true
	e.SetCredentials(os.Getenv("GATEIO_API_KEY"), os.Getenv("GATEIO_API_SECRET"), "", "", "", "")
	_ = e.SetHTTPClientTimeout(time.Minute)
	ctx := t.Context()
	pair := currency.NewPairWithDelimiter("MPLX", "USDT", "_")

	pos, err := e.GetAllFuturesPositionsOfUsers(ctx, currency.USDT, true)
	t.Logf("open perp positions n=%d err=%v", len(pos), err)
	for _, p := range pos {
		t.Logf("  %s size=%v entry=%v mode=%s lev=%v", p.Contract, p.Size, p.EntryPrice, p.Mode, p.Leverage)
	}
	sp, err := e.GetSpotAccounts(ctx, currency.EMPTYCODE)
	t.Logf("spot accounts err=%v", err)
	for _, a := range sp {
		if a.Available.Float64()+a.Locked.Float64() > 0 {
			t.Logf("  spot %s avail=%v locked=%v", a.Currency, a.Available, a.Locked)
		}
	}
	fa, err := e.QueryFuturesAccount(ctx, currency.USDT)
	if err == nil {
		t.Logf("futures total=%v avail=%v dual=%v", fa.Total, fa.Available, fa.InDualMode)
	}
	m, err := e.GetMarginAccountList(ctx, currency.EMPTYPAIR)
	t.Logf("margin accounts n=%d err=%v", len(m), err)
	for _, a := range m {
		t.Logf("  %s risk=%s base=%+v quote=%+v", a.CurrencyPair, a.Risk, a.Base, a.Quote)
	}
	fo, err := e.GetFuturesOrders(ctx, pair, "finished", "", currency.USDT, 20, 0, false)
	t.Logf("MPLX futures orders n=%d err=%v", len(fo), err)
	for _, o := range fo {
		t.Logf("  %s id=%d size=%v left=%v fill=%v price=%v tif=%s reduce=%v finish=%s", o.CreateTime.Time().UTC().Format(time.RFC3339), o.ID, o.Size, o.RemainingAmount, o.FillPrice, o.OrderPrice, o.TimeInForce, o.IsReduceOnly, o.FinishAs)
	}
	for _, acct := range []asset.Item{asset.Spot, asset.Margin} {
		so, err := e.GetMySpotTradingHistory(ctx, pair, "", 1, 20, false, time.Time{}, time.Time{})
		t.Logf("MPLX spot trades (%s) n=%d err=%v", acct, len(so), err)
		for _, s := range so {
			t.Logf("  %+v", s)
		}
		break
	}
	books, err := e.GetFuturesAccountBooks(ctx, currency.USDT, 1000, time.Now().Add(-72*time.Hour), time.Now(), "")
	t.Logf("futures account book n=%d err=%v", len(books), err)
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, b := range books {
		sums[b.Type] += b.Change.Float64()
		counts[b.Type]++
	}
	for k, v := range sums {
		t.Logf("  futures book %-8s n=%3d sum=%.6f", k, counts[k], v)
	}
	if len(books) > 0 {
		t.Logf("  newest %s oldest %s", books[0].Time.Time().UTC().Format(time.RFC3339), books[len(books)-1].Time.Time().UTC().Format(time.RFC3339))
	}
	all, err := e.GetFuturesOrders(ctx, currency.EMPTYPAIR, "finished", "", currency.USDT, 100, 0, false)
	t.Logf("all finished futures orders n=%d err=%v", len(all), err)
	byContract := map[string]int{}
	for _, o := range all {
		byContract[o.Contract]++
	}
	t.Logf("  by contract: %v", byContract)
	if len(all) > 0 {
		t.Logf("  newest %s oldest %s", all[0].CreateTime.Time().UTC().Format(time.RFC3339), all[len(all)-1].CreateTime.Time().UTC().Format(time.RFC3339))
	}
	tk, err := e.GetTicker(ctx, "MON_USDT", "")
	if err == nil {
		t.Logf("MON last=%v", tk.Last)
	}
	acc, err := e.GetUsersTotalBalance(ctx, currency.USDT)
	if err == nil {
		t.Logf("total balance: %+v", acc.Total)
		for k, v := range acc.Details {
			t.Logf("  %s: %+v", k, v)
		}
	} else {
		t.Logf("total balance err=%v", err)
	}
}
