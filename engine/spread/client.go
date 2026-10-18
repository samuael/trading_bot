package spread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/exchanges/order"
	"github.com/thrasher-corp/gocryptotrader/types"
)

var settlement = currency.USDT

// Account names used by GateIO transfers and orders.
const (
	accountSpot    = "spot"
	accountMargin  = "margin"
	accountFutures = "futures"
)

// SpotOrderRequest is an immediate-or-cancel spot order. A zero Price sends a market order, which GateIO only
// sizes in base for sells, so buys always carry a limit.
type SpotOrderRequest struct {
	Pair  currency.Pair
	Buy   bool
	Qty   float64 // base
	Price float64
	// Margin trades in the pair's isolated margin account; AutoBorrow borrows any base a sell is short of.
	Margin     bool
	AutoBorrow bool
}

// SpotFill is the executed part of a spot order.
type SpotFill struct {
	Qty      float64 // base filled, before any fee deducted in base
	AvgPrice float64
	FeeBase  float64
	FeeQuote float64
	// FeeInOtherCurrency is set when the fee was paid in GT or points rather than the traded currencies.
	FeeInOtherCurrency bool
}

// FuturesOrderRequest is an immediate-or-cancel perp order. A zero Price sends a market order.
type FuturesOrderRequest struct {
	Contract   currency.Pair
	Sell       bool
	Contracts  float64
	Price      float64
	ReduceOnly bool
}

// FuturesFill is the executed part of a perp order.
type FuturesFill struct {
	Contracts float64
	AvgPrice  float64
	FeeRate   float64 // taker rate applied by the exchange, zero when unknown
}

// MarginPair describes a spot pair that supports isolated margin borrowing.
type MarginPair struct {
	Pair      string
	MinBorrow float64 // minimum base borrow
}

// MarginAccount is the isolated margin account of one pair.
type MarginAccount struct {
	BaseAvailable  float64
	BaseBorrowed   float64
	BaseInterest   float64
	QuoteAvailable float64
	// Risk is total assets over total liabilities; zero when nothing is borrowed.
	Risk float64
}

// BaseOwed is the base borrowed plus unpaid interest.
func (m MarginAccount) BaseOwed() float64 { return m.BaseBorrowed + m.BaseInterest }

// Client is the exchange surface the strategy needs.
type Client interface {
	SpotPairs(ctx context.Context) ([]gateio.CurrencyPairDetail, error)
	Contracts(ctx context.Context) ([]gateio.FuturesContract, error)
	MarginPairs(ctx context.Context) ([]MarginPair, error)
	SpotTickers(ctx context.Context) ([]gateio.Ticker, error)
	FuturesTickers(ctx context.Context) ([]gateio.FuturesTicker, error)
	// SpotBook returns levels in base units, FuturesBook in contracts. Bids descend and asks ascend.
	SpotBook(ctx context.Context, pair currency.Pair, depth uint64) (bids, asks []Level, err error)
	FuturesBook(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, err error)
	// FuturesBookSnapshot also returns the book's update ID, to synchronise streamed updates against.
	FuturesBookSnapshot(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, id int64, err error)
	Fees(ctx context.Context, pair currency.Pair) (Fees, error)
	// BorrowRates returns the estimated hourly isolated margin interest rate per base currency.
	BorrowRates(ctx context.Context, bases []string) (map[string]float64, error)

	SpotBalance(ctx context.Context, ccy currency.Code) (available, locked float64, err error)
	FuturesAccount(ctx context.Context) (*gateio.FuturesAccount, error)
	MarginAccount(ctx context.Context, pair currency.Pair) (MarginAccount, error)
	Positions(ctx context.Context) ([]gateio.Position, error)
	// Transfer moves USDT between the "spot", "futures" and "margin" accounts. pair names the isolated margin
	// account and is ignored otherwise.
	Transfer(ctx context.Context, from, to string, pair currency.Pair, amount float64) error
	// Repay pays back base borrowed in a pair's isolated margin account. all repays the full amount owed.
	Repay(ctx context.Context, pair currency.Pair, amount float64, all bool) error

	EnsureSinglePositionMode(ctx context.Context) error
	SetLeverage(ctx context.Context, contract currency.Pair, leverage float64) error
	SpotOrder(ctx context.Context, req SpotOrderRequest) (SpotFill, error)
	FuturesOrder(ctx context.Context, req FuturesOrderRequest) (FuturesFill, error)
}

// GateIOClient implements Client on top of the gocryptotrader GateIO exchange. The few endpoints gocryptotrader
// does not wrap (isolated margin "uni" loans) are called directly with the same credentials and signing.
type GateIOClient struct {
	exch *gateio.Exchange
	http *http.Client
}

// NewGateIOClient wraps an authenticated, configured GateIO exchange. timeout bounds the requests it sends
// directly; set the exchange's own timeout with SetHTTPClientTimeout.
func NewGateIOClient(exch *gateio.Exchange, timeout time.Duration) *GateIOClient {
	return &GateIOClient{exch: exch, http: &http.Client{Timeout: timeout}}
}

func (c *GateIOClient) SpotPairs(ctx context.Context) ([]gateio.CurrencyPairDetail, error) {
	return c.exch.ListSpotCurrencyPairs(ctx)
}

func (c *GateIOClient) Contracts(ctx context.Context) ([]gateio.FuturesContract, error) {
	return c.exch.GetAllFutureContracts(ctx, settlement)
}

func (c *GateIOClient) MarginPairs(ctx context.Context) ([]MarginPair, error) {
	var resp []struct {
		CurrencyPair  string       `json:"currency_pair"`
		BaseMinBorrow types.Number `json:"base_min_borrow_amount"`
		Status        string       `json:"status"`
		DelistedTime  int64        `json:"delisted_time"`
	}
	if err := c.do(ctx, http.MethodGet, "margin/uni/currency_pairs", nil, nil, false, &resp); err != nil {
		return nil, err
	}
	out := make([]MarginPair, 0, len(resp))
	for _, p := range resp {
		if strings.EqualFold(p.Status, "enabled") && p.DelistedTime == 0 {
			out = append(out, MarginPair{Pair: strings.ToUpper(p.CurrencyPair), MinBorrow: p.BaseMinBorrow.Float64()})
		}
	}
	return out, nil
}

func (c *GateIOClient) SpotTickers(ctx context.Context) ([]gateio.Ticker, error) {
	return c.exch.GetTickers(ctx, "", "")
}

func (c *GateIOClient) FuturesTickers(ctx context.Context) ([]gateio.FuturesTicker, error) {
	return c.exch.GetFuturesTickers(ctx, settlement, currency.EMPTYPAIR)
}

func (c *GateIOClient) SpotBook(ctx context.Context, pair currency.Pair, depth uint64) (bids, asks []Level, err error) {
	ob, err := c.exch.GetOrderbook(ctx, pair.String(), "", depth, false)
	if err != nil {
		return nil, nil, err
	}
	return toLevels(ob.Bids), toLevels(ob.Asks), nil
}

func (c *GateIOClient) FuturesBook(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, err error) {
	bids, asks, _, err = c.FuturesBookSnapshot(ctx, contract, depth)
	return bids, asks, err
}

func (c *GateIOClient) FuturesBookSnapshot(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, id int64, err error) {
	ob, err := c.exch.GetFuturesOrderbook(ctx, settlement, contract.String(), "", depth, true)
	if err != nil {
		return nil, nil, 0, err
	}
	return toLevels(ob.Bids), toLevels(ob.Asks), ob.ID, nil
}

func toLevels(levels gateio.OrderbookLevels) []Level {
	out := make([]Level, len(levels))
	for i := range levels {
		out[i] = Level{Price: levels[i].Price, Amount: levels[i].Amount}
	}
	return out
}

func (c *GateIOClient) Fees(ctx context.Context, pair currency.Pair) (Fees, error) {
	f, err := c.exch.GetPersonalTradingFee(ctx, pair, settlement)
	if err != nil {
		return Fees{}, err
	}
	fees := Fees{SpotTaker: f.TakerFee.Float64(), FuturesTaker: f.FuturesTakerFee.Float64()}
	if fees.SpotTaker <= 0 || fees.FuturesTaker <= 0 {
		return Fees{}, fmt.Errorf("exchange returned unusable fee rates %+v for %s", fees, pair)
	}
	return fees, nil
}

func (c *GateIOClient) BorrowRates(ctx context.Context, bases []string) (map[string]float64, error) {
	rates := make(map[string]float64, len(bases))
	const maxPerRequest = 10
	for start := 0; start < len(bases); start += maxPerRequest {
		chunk := bases[start:min(start+maxPerRequest, len(bases))]
		codes := make([]currency.Code, len(chunk))
		for i, b := range chunk {
			codes[i] = currency.NewCode(b)
		}
		resp, err := c.exch.GetEstimatedInterestRate(ctx, codes)
		if err != nil {
			return nil, err
		}
		for k, v := range resp {
			rates[strings.ToUpper(k)] = v.Float64()
		}
	}
	return rates, nil
}

func (c *GateIOClient) SpotBalance(ctx context.Context, ccy currency.Code) (available, locked float64, err error) {
	accounts, err := c.exch.GetSpotAccounts(ctx, ccy)
	if err != nil {
		return 0, 0, err
	}
	for _, a := range accounts {
		if a.Currency.Equal(ccy) {
			return a.Available.Float64(), a.Locked.Float64(), nil
		}
	}
	return 0, 0, nil
}

func (c *GateIOClient) FuturesAccount(ctx context.Context) (*gateio.FuturesAccount, error) {
	return c.exch.QueryFuturesAccount(ctx, settlement)
}

func (c *GateIOClient) MarginAccount(ctx context.Context, pair currency.Pair) (MarginAccount, error) {
	accounts, err := c.exch.GetMarginAccountList(ctx, pair)
	if err != nil {
		return MarginAccount{}, err
	}
	for _, a := range accounts {
		if !strings.EqualFold(a.CurrencyPair, pair.String()) {
			continue
		}
		risk, _ := strconv.ParseFloat(a.Risk, 64)
		return MarginAccount{
			BaseAvailable:  a.Base.Available.Float64(),
			BaseBorrowed:   a.Base.Borrowed.Float64(),
			BaseInterest:   a.Base.Interest.Float64(),
			QuoteAvailable: a.Quote.Available.Float64(),
			Risk:           risk,
		}, nil
	}
	return MarginAccount{}, nil
}

func (c *GateIOClient) Positions(ctx context.Context) ([]gateio.Position, error) {
	return c.exch.GetAllFuturesPositionsOfUsers(ctx, settlement, true)
}

func (c *GateIOClient) Transfer(ctx context.Context, from, to string, pair currency.Pair, amount float64) error {
	arg := &gateio.TransferCurrencyParam{
		Currency: settlement,
		From:     from,
		To:       to,
		Amount:   types.Number(floorTo(amount, 2)),
	}
	if from == accountFutures || to == accountFutures {
		arg.Settle = settlement.Lower().String()
	}
	if from == accountMargin || to == accountMargin {
		arg.CurrencyPair = pair
	}
	_, err := c.exch.TransferCurrency(ctx, arg)
	return err
}

func (c *GateIOClient) Repay(ctx context.Context, pair currency.Pair, amount float64, all bool) error {
	body := map[string]any{
		"currency":      pair.Base.Upper().String(),
		"type":          "repay",
		"amount":        strconv.FormatFloat(amount, 'f', -1, 64),
		"repaid_all":    all,
		"currency_pair": pair.String(),
	}
	return c.do(ctx, http.MethodPost, "margin/uni/loans", nil, body, true, nil)
}

// do sends a REST request to an endpoint gocryptotrader does not wrap, signing it exactly as gocryptotrader does.
func (c *GateIOClient) do(ctx context.Context, method, path string, query url.Values, body any, auth bool, result any) error {
	base, err := c.exch.API.Endpoints.GetURL(exchange.RestSpot)
	if err != nil {
		return err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if auth {
		creds, err := c.exch.GetCredentials(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		var signBody any
		if payload != nil {
			signBody = json.RawMessage(payload)
		}
		sig, err := c.exch.GenerateSignature(creds.Secret, method, "/api/v4/"+path, query.Encode(), signBody, now)
		if err != nil {
			return err
		}
		req.Header.Set("KEY", creds.Key)
		req.Header.Set("TIMESTAMP", strconv.FormatInt(now.Unix(), 10))
		req.Header.Set("SIGN", sig)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if result == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, result)
}

// EnsureSinglePositionMode switches the futures account out of dual (hedge) mode, which the strategy does not
// support. GateIO only allows the switch while no positions are open.
func (c *GateIOClient) EnsureSinglePositionMode(ctx context.Context) error {
	acc, err := c.exch.QueryFuturesAccount(ctx, settlement)
	if err != nil {
		return err
	}
	if !acc.InDualMode {
		return nil
	}
	positions, err := c.Positions(ctx)
	if err != nil {
		return err
	}
	if len(positions) > 0 {
		return errors.New("futures account is in dual position mode with open positions; close them or switch to single mode manually")
	}
	// gocryptotrader's EnableOrDisableDualMode sends GET, which GateIO rejects with 405; the endpoint is POST.
	path := "futures/" + settlement.Lower().String() + "/dual_mode"
	if err := c.do(ctx, http.MethodPost, path, url.Values{"dual_mode": {"false"}}, nil, true, nil); err != nil {
		return fmt.Errorf("switch to single position mode: %w", err)
	}
	if acc, err = c.exch.QueryFuturesAccount(ctx, settlement); err != nil {
		return err
	}
	if acc.InDualMode {
		return errors.New("futures account still reports dual position mode after switching")
	}
	return nil
}

func (c *GateIOClient) SetLeverage(ctx context.Context, contract currency.Pair, leverage float64) error {
	_, err := c.exch.UpdateFuturesPositionLeverage(ctx, settlement, contract, leverage, 0)
	return err
}

func (c *GateIOClient) SpotOrder(ctx context.Context, req SpotOrderRequest) (SpotFill, error) {
	arg := &gateio.CreateOrderRequest{
		CurrencyPair: req.Pair,
		Type:         "limit",
		Account:      accountSpot,
		Side:         "sell",
		Amount:       types.Number(req.Qty),
		Price:        types.Number(req.Price),
		TimeInForce:  "ioc",
		AutoBorrow:   req.AutoBorrow,
	}
	a := asset.Spot
	if req.Margin {
		arg.Account = accountMargin
		a = asset.Margin
	}
	if req.Buy {
		arg.Side = "buy"
	}
	if req.Price == 0 {
		if req.Buy {
			return SpotFill{}, errors.New("market spot buys are not supported; they are sized in quote currency")
		}
		arg.Type = "market"
	}
	o, err := c.exch.PlaceSpotOrder(ctx, arg)
	if err != nil {
		return SpotFill{}, err
	}
	if o.Status == "open" {
		id := o.OrderID
		if o, err = c.exch.GetSpotOrder(ctx, id, req.Pair, a); err != nil {
			return SpotFill{}, fmt.Errorf("spot order %s placed but status unknown: %w", id, err)
		}
	}
	fill := SpotFill{Qty: o.Amount.Float64() - o.Left.Float64(), AvgPrice: o.AverageFillPrice.Float64()}
	if fill.Qty <= 0 {
		return SpotFill{}, nil
	}
	if fill.AvgPrice == 0 {
		fill.AvgPrice = o.FilledTotal.Float64() / fill.Qty
	}
	switch fee := o.FeeDeducted.Float64(); {
	case fee == 0:
		fill.FeeInOtherCurrency = o.PointFee.Float64() > 0 || o.GtFee != "" && o.GtFee != "0"
	case strings.EqualFold(o.FeeCurrency, req.Pair.Base.String()):
		fill.FeeBase = fee
	case strings.EqualFold(o.FeeCurrency, req.Pair.Quote.String()):
		fill.FeeQuote = fee
	default:
		fill.FeeInOtherCurrency = true
	}
	return fill, nil
}

func (c *GateIOClient) FuturesOrder(ctx context.Context, req FuturesOrderRequest) (FuturesFill, error) {
	s := &order.Submit{
		Exchange:    c.exch.GetName(),
		Pair:        req.Contract,
		AssetType:   asset.USDTMarginedFutures,
		Side:        order.Long,
		Type:        order.Limit,
		Price:       req.Price,
		Amount:      req.Contracts,
		TimeInForce: order.ImmediateOrCancel,
		ReduceOnly:  req.ReduceOnly,
	}
	if req.Sell {
		s.Side = order.Short
	}
	if req.Price == 0 {
		s.Type = order.Market
	}
	resp, err := c.exch.SubmitOrder(ctx, s)
	if err != nil {
		return FuturesFill{}, err
	}
	o, err := c.exch.GetSingleFuturesOrder(ctx, settlement, resp.OrderID)
	if err != nil {
		return FuturesFill{}, fmt.Errorf("futures order %s placed but fill unknown: %w", resp.OrderID, err)
	}
	fill := FuturesFill{Contracts: math.Abs(o.Size) - math.Abs(o.RemainingAmount), AvgPrice: o.FillPrice.Float64()}
	if fill.Contracts <= 0 {
		return FuturesFill{}, nil
	}
	if rate, err := strconv.ParseFloat(o.TakerFee, 64); err == nil {
		fill.FeeRate = rate
	}
	return fill, nil
}
