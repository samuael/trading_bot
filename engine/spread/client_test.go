package spread

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

type recordedRequest struct {
	method, path, query, body string
	header                    http.Header
}

// newTestGateIOClient points a GateIO client at a local server answering with handler.
func newTestGateIOClient(t *testing.T, handler func(w http.ResponseWriter, r recordedRequest)) (*GateIOClient, *gateio.Exchange, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body), header: r.Header.Clone()}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		handler(w, rec)
	}))
	t.Cleanup(srv.Close)

	exch := new(gateio.Exchange)
	require.NoError(t, Setup(exch, "../../config_example.json"))
	exch.API.AuthenticatedSupport = true
	exch.SetCredentials("test-key", "test-secret", "", "", "", "")
	require.NoError(t, exch.API.Endpoints.SetRunningURL(exchange.RestSpot.String(), srv.URL+"/api/v4/"))
	return NewGateIOClient(exch, 5*time.Second), exch, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

// assertSigned checks a request carries a valid GateIO v4 signature.
func assertSigned(t *testing.T, exch *gateio.Exchange, r recordedRequest) {
	t.Helper()
	assert.Equal(t, "test-key", r.header.Get("KEY"))
	ts, err := strconv.ParseInt(r.header.Get("TIMESTAMP"), 10, 64)
	require.NoError(t, err)
	var body any
	if r.body != "" {
		body = json.RawMessage(r.body)
	}
	want, err := exch.GenerateSignature("test-secret", r.method, r.path, r.query, body, time.Unix(ts, 0))
	require.NoError(t, err)
	assert.Equal(t, want, r.header.Get("SIGN"))
}

func TestEnsureSinglePositionModeUsesPost(t *testing.T) {
	t.Parallel()
	dual := true
	client, exch, requests := newTestGateIOClient(t, func(w http.ResponseWriter, r recordedRequest) {
		switch r.path {
		case "/api/v4/futures/usdt/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"currency": "USDT", "total": "14.37", "in_dual_mode": dual})
		case "/api/v4/futures/usdt/positions":
			_, _ = w.Write([]byte("[]"))
		case "/api/v4/futures/usdt/dual_mode":
			if r.method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			dual = false
			_ = json.NewEncoder(w).Encode(map[string]any{"in_dual_mode": false})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	require.NoError(t, client.EnsureSinglePositionMode(t.Context()))
	var toggle *recordedRequest
	for _, r := range requests() {
		if r.path == "/api/v4/futures/usdt/dual_mode" {
			toggle = &r
		}
	}
	require.NotNil(t, toggle)
	assert.Equal(t, http.MethodPost, toggle.method)
	assert.Equal(t, "dual_mode=false", toggle.query)
	assertSigned(t, exch, *toggle)
}

func TestRepayRequest(t *testing.T) {
	t.Parallel()
	client, exch, requests := newTestGateIOClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		w.WriteHeader(http.StatusNoContent)
	})
	require.NoError(t, client.Repay(t.Context(), usdtPair("SAND"), 82.5, true))

	reqs := requests()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, http.MethodPost, r.method)
	assert.Equal(t, "/api/v4/margin/uni/loans", r.path)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &body))
	assert.Equal(t, map[string]any{
		"currency": "SAND", "type": "repay", "amount": "82.5", "repaid_all": true, "currency_pair": "SAND_USDT",
	}, body)
	assertSigned(t, exch, r)
}

func TestSignedRequestSurfacesErrors(t *testing.T) {
	t.Parallel()
	client, _, _ := newTestGateIOClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"label":"INVALID_PARAM_VALUE","message":"bad amount"}`))
	})
	err := client.Repay(t.Context(), usdtPair("SAND"), 1, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INVALID_PARAM_VALUE")
}

func TestFuturesOrderPartialIOCFill(t *testing.T) {
	t.Parallel()
	client, exch, requests := newTestGateIOClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = w.Write([]byte(`{"id":42,"contract":"PARTI_USDT","size":10,"left":4,"price":"0.1234","fill_price":"0.1233",` +
			`"tkfr":"0.0005","tif":"ioc","status":"finished","finish_as":"ioc"}`))
	})
	fill, err := client.FuturesOrder(t.Context(), FuturesOrderRequest{Contract: usdtPair("PARTI"), Contracts: 10, Price: 0.1234})
	require.NoError(t, err)
	assert.Equal(t, FuturesFill{Contracts: 6, AvgPrice: 0.1233, FeeRate: 0.0005}, fill)

	reqs := requests()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, http.MethodPost, r.method)
	assert.Equal(t, "/api/v4/futures/usdt/orders", r.path)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &body))
	assert.Equal(t, map[string]any{
		"contract": "PARTI_USDT", "size": 10.0, "iceberg": 0.0, "price": "0.1234", "tif": "ioc",
	}, body)
	assertSigned(t, exch, r)
}

func TestFuturesOrderMarketSell(t *testing.T) {
	t.Parallel()
	client, _, requests := newTestGateIOClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = w.Write([]byte(`{"id":43,"size":-5,"left":0,"fill_price":"2.5","status":"finished","finish_as":"filled"}`))
	})
	fill, err := client.FuturesOrder(t.Context(), FuturesOrderRequest{Contract: usdtPair("SAND"), Sell: true, Contracts: 5, ReduceOnly: true})
	require.NoError(t, err)
	assert.Equal(t, 5.0, fill.Contracts)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(requests()[0].body), &body))
	assert.Equal(t, -5.0, body["size"])
	assert.Equal(t, "0", body["price"])
	assert.Equal(t, true, body["reduce_only"])
}
