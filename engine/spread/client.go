package spread

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thrasher-corp/gocryptotrader/currency"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/exchanges/order"
	"github.com/thrasher-corp/gocryptotrader/types"
)

var settlement = currency.USDT

// SpotOrderRequest is an immediate-or-cancel spot order. A zero Price sends a market order, which this strategy
// only uses to sell.
type SpotOrderRequest struct {
	Pair  currency.Pair
	Buy   bool
	Qty   float64 // base
	Price float64
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

// Client is the exchange surface the strategy needs.
type Client interface {
	SpotPairs(ctx context.Context) ([]gateio.CurrencyPairDetail, error)
	Contracts(ctx context.Context) ([]gateio.FuturesContract, error)
	SpotTickers(ctx context.Context) ([]gateio.Ticker, error)
	FuturesTickers(ctx context.Context) ([]gateio.FuturesTicker, error)
	// SpotBook returns levels in base units, FuturesBook in contracts. Bids descend and asks ascend.
	SpotBook(ctx context.Context, pair currency.Pair, depth uint64) (bids, asks []Level, err error)
	FuturesBook(ctx context.Context, contract currency.Pair, depth uint64) (bids, asks []Level, err error)
	Fees(ctx context.Context, pair currency.Pair) (Fees, error)

	SpotBalance(ctx context.Context, ccy currency.Code) (available, locked float64, err error)
	FuturesAccount(ctx context.Context) (*gateio.FuturesAccount, error)
	Positions(ctx context.Context) ([]gateio.Position, error)
	// Transfer moves USDT between the "spot" and "futures" accounts.
	Transfer(ctx context.Context, from, to string, amount float64) error

	EnsureSinglePositionMode(ctx context.Context) error
	SetLeverage(ctx context.Context, contract currency.Pair, leverage float64) error
	SpotOrder(ctx context.Context, req SpotOrderRequest) (SpotFill, error)
	FuturesOrder(ctx context.Context, req FuturesOrderRequest) (FuturesFill, error)
}

// GateIOClient implements Client on top of the gocryptotrader GateIO exchange.
type GateIOClient struct {
	exch *gateio.Exchange
}

// NewGateIOClient wraps an authenticated, configured GateIO exchange.
func NewGateIOClient(exch *gateio.Exchange) *GateIOClient { return &GateIOClient{exch: exch} }

func (c *GateIOClient) SpotPairs(ctx context.Context) ([]gateio.CurrencyPairDetail, error) {
	return c.exch.ListSpotCurrencyPairs(ctx)
}

func (c *GateIOClient) Contracts(ctx context.Context) ([]gateio.FuturesContract, error) {
	return c.exch.GetAllFutureContracts(ctx, settlement)
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
	ob, err := c.exch.GetFuturesOrderbook(ctx, settlement, contract.String(), "", depth, false)
	if err != nil {
		return nil, nil, err
	}
	return toLevels(ob.Bids), toLevels(ob.Asks), nil
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

func (c *GateIOClient) Positions(ctx context.Context) ([]gateio.Position, error) {
	return c.exch.GetAllFuturesPositionsOfUsers(ctx, settlement, true)
}

func (c *GateIOClient) Transfer(ctx context.Context, from, to string, amount float64) error {
	_, err := c.exch.TransferCurrency(ctx, &gateio.TransferCurrencyParam{
		Currency: settlement,
		From:     from,
		To:       to,
		Amount:   types.Number(floorTo(amount, 2)),
		Settle:   settlement.Lower().String(),
	})
	return err
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
	_, err = c.exch.EnableOrDisableDualMode(ctx, settlement, false)
	return err
}

func (c *GateIOClient) SetLeverage(ctx context.Context, contract currency.Pair, leverage float64) error {
	_, err := c.exch.UpdateFuturesPositionLeverage(ctx, settlement, contract, leverage, 0)
	return err
}

func (c *GateIOClient) SpotOrder(ctx context.Context, req SpotOrderRequest) (SpotFill, error) {
	arg := &gateio.CreateOrderRequest{
		CurrencyPair: req.Pair,
		Type:         "limit",
		Account:      "spot",
		Side:         "sell",
		Amount:       types.Number(req.Qty),
		Price:        types.Number(req.Price),
		TimeInForce:  "ioc",
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
		if o, err = c.exch.GetSpotOrder(ctx, id, req.Pair, asset.Spot); err != nil {
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
