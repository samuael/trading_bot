package spread

import (
	"context"

	// "github.com/shazbert/tradingcli/pkg/collection"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	// "github.com/thrasher-corp/gocryptotrader/exchanges/orderbook"
	// "github.com/thrasher-corp/gocryptotrader/exchanges/orderbook"
)

var point = currency.NewCode("POINT")

// Toggle defines flag toggles for the strategy
type Toggle struct {
	// Verbose will make all logs verbose include open and close debug logs
	Verbose bool
	// AllowOpen and AllowClose are used to toggle the ability to open and close from the generated trading signals.
	// This is useful for testing and debugging.
	AllowOpen  bool
	AllowClose bool
	// InitialRunComplete signals the initial run has completed.
	InitialRunComplete bool
	// Halts execution after initial run. Useful for testing.
	InitialRunOnly bool
}

// Run manages the engine for the strategy
func Run(ctx context.Context, strategyID string, toggle *Toggle, exch exchange.IBotExchange) {
	// hourTimer := time.NewTimer(0)
	// twentyFourHourTimer := time.NewTimer(time.Second)
	// tenSecondTimer := time.NewTimer(time.Second * 10)
	// websocketSignals := make(chan collection.FilteredSignal, 1)

	// leverage := collection.NewLeverageTracker()
	// positions := collection.NewPositionsTracker()
	// portfolio := collection.NewPortfolioTracker()
	// prices := collection.NewPriceTracker()
	// details := collection.NewTradingDetailsTracker()
	// matches := collection.NewMatchesTracker()
	// candles := collection.NewCandlesTracker()
	// stats := collection.NewStatisticsTracker()
	// fees := collection.NewFeeTracker()

	// settlement := currency.USDT
	// exclude := []currency.Code{point}

	// signalProcessMetric := collection.NewLatencyMetric("strategy", "signal processor", time.Minute)

	// var tradeableAssets collection.ExchangeTradeableAssets
	// for {
	// 	select {
	// 	case <-hourTimer.C:
	// 		hourTimer.Reset(time.Until(time.Now().Truncate(time.Hour).Add(time.Hour)))
	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "running hourly sync")
	// 		}

	// 		if toggle.InitialRunComplete {
	// 			// When starting the application the pairs are initially updated, every hour after that we can flush the
	// 			// pairs.
	// 			if toggle.Verbose {
	// 				log.Debugf(collection.Strategy, "updating pairs")
	// 			}

	// 			if err := exch.UpdateTradablePairs(ctx, false); err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update pairs: %v", err)
	// 			}
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating contract details")
	// 		}

	// 		fAsset, err := GetFuturesAssetFromExchange(exch)
	// 		if err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to get futures asset: %v", err)
	// 		}

	// 		if err := details.UpdateExchangeTradingDetails(ctx, exch, asset.Spot, fAsset); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update contract details: %v", err)
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating matches")
	// 		}
	// 		mainOp := [2]collection.TradeableOption{{Exchange: exch, Asset: asset.Spot}, {Exchange: exch, Asset: fAsset}}
	// 		if err := matches.UpdateMatches(details, mainOp, toggle.Verbose); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update matches: %v", err)
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "connection and subscribing websocket")
	// 		}
	// 		if err := collection.WebsocketConnectOrFlushSubs(exch, websocketSignals, collection.GetSubscriptionFilterHookByExchange(exch)); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to connect and subscribe to websocket: %v", err)
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating candles")
	// 		}

	// 		tradeableAssets = matches.GetExchangeTradeableAssets()
	// 		if err := candles.UpdateCandles(ctx, tradeableAssets); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update candles: %v", err)
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating stats")
	// 		}
	// 		if err := stats.UpdateStats(candles, matches); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update stats: %v", err)
	// 		}

	// 		tn := time.Now()
	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "Setting leverage")
	// 		}
	// 		if err := leverage.SetLeverage(ctx, matches); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to set leverage: %v", err)
	// 		}
	// 		fmt.Println("Time taken to set leverage:", time.Since(tn))

	// 		// TODO: Weekly candles for leverage calculations

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating fees")
	// 		}

	// 		if err := fees.UpdateFees(ctx, exch, asset.Spot, fAsset); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update fees: %v", err)
	// 		}

	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "Updating balances")
	// 		}
	// 		if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 			if !toggle.InitialRunComplete {
	// 				panic(err)
	// 			}
	// 			log.Errorf(collection.Strategy, "failed to update balances: %v", err)
	// 		} else {
	// 			portfolioValue, err := portfolio.GetTotalValue()
	// 			if err != nil {
	// 				goto checkRun // cannot rebalance without correct balances.
	// 			}
	// 			collection.SetTotalValueMetric(strategyID, portfolioValue)
	// 			log.Debugf(collection.Strategy, "portfolio value: %v", portfolioValue)

	// 			if isUnified, err := collection.IsUnifiedAccount(ctx, exch); err != nil && !isUnified {
	// 				if toggle.Verbose {
	// 					log.Debugf(collection.Strategy, "checking account balances for transfer potential")
	// 				}
	// 				err = RebalanceBetweenAccounts(ctx, currency.USDT, exch, portfolio)
	// 				if err != nil {
	// 					log.Errorf(collection.Strategy, "failed to rebalance between accounts: %v", err)
	// 				}
	// 			}
	// 		}

	// 	checkRun:
	// 		if toggle.InitialRunOnly {
	// 			log.Debugln(collection.Strategy, "Initial run complete")
	// 			return
	// 		}
	// 		if !toggle.InitialRunComplete {
	// 			change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
	// 			if err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update positions: %v", err)
	// 				panic(err)
	// 			}
	// 			change.Summary("Initial Start")
	// 		}
	// 		toggle.InitialRunComplete = true
	// 	case <-twentyFourHourTimer.C:
	// 		twentyFourHourTimer.Reset(time.Until(time.Now().Truncate(24 * time.Hour).Add(24 * time.Hour)))
	// 		if !toggle.AllowOpen || !toggle.AllowClose {
	// 			continue
	// 		}
	// 		balances, err := portfolio.GetSpotBalances(exch, []currency.Code{settlement})
	// 		if err != nil {
	// 			log.Errorf(collection.Strategy, "failed to get spot balances: %v", err)
	// 			continue
	// 		}

	// 		var sweep []currency.Code
	// 		for _, balance := range balances {
	// 			price, err := prices.FetchOrderbookPriceWithFallback(ctx, exch, currency.NewPair(balance.Currency, settlement), asset.Spot, true)
	// 			if err != nil {
	// 				log.Errorf(collection.Strategy, "failed to fetch price for %v: %v", balance.Currency, err)
	// 				continue
	// 			}
	// 			if price == 0 {
	// 				log.Warnf(collection.Strategy, "price for %v is zero, skipping sweep", balance.Currency)
	// 				continue
	// 			}
	// 			if balance.Available*price >= 3 {
	// 				continue
	// 			}
	// 			sweep = append(sweep, balance.Currency)
	// 		}

	// 		if len(sweep) == 0 {
	// 			continue
	// 		}

	// 		if err := exch.(*gateio.Gateio).ConvertSmallBalances(request.WithVerbose(ctx), sweep...); err != nil {
	// 			log.Errorf(collection.Strategy, "failed to convert small balances: %v", err)
	// 			continue
	// 		}

	// 		if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 			log.Errorf(collection.Strategy, "failed to update balances: %v", err)
	// 			continue
	// 		}

	// 		portfolioValue, err := portfolio.GetTotalValue()
	// 		if err != nil {
	// 			log.Errorf(collection.Strategy, "failed to get account value: %v", err)
	// 			continue
	// 		}
	// 		collection.SetTotalValueMetric(strategyID, portfolioValue)
	// 	case <-tenSecondTimer.C:
	// 		// TODO: Break up into hedge detection (10s only walk the items which has a state change) and close detection (1min only when a position hasn't been checked by ob signal).
	// 		// TODO: When a position is updated via websocket, we can defer an individual check for a second to allow for the exchange to update.
	// 		// This will reduce the walk time across the entire list if its not needed. This could potentially lead to issues but test.
	// 		tn := time.Now()
	// 		tenSecondTimer.Reset(time.Until(tn.Truncate(time.Second).Add(time.Second * 10)))
	// 		if toggle.Verbose {
	// 			log.Debugf(collection.Strategy, "updating positions")
	// 		}

	// 		if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 			log.Errorf(collection.Strategy, "failed to update portfolio: %v", err)
	// 			continue
	// 		}
	// 		change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
	// 		if err != nil {
	// 			log.Errorf(collection.Strategy, "failed to update positions: %v", err)
	// 			continue
	// 		}
	// 		change.Summary("Close checker")

	// 		reducedPositions := CheckAllPositionsForClose(ctx, strategyID, positions, toggle, prices, details, matches, settlement, exclude, portfolio, prices, details)
	// 		if len(reducedPositions.Positions) > 0 {
	// 			changes := positions.UpdateHedgedPositions(reducedPositions.Positions)
	// 			changes.Summary("Check all positions close")
	// 		}
	// 		if reducedPositions.PortfolioUpdate {
	// 			if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update portfolio: %v", err)
	// 			} else {
	// 				portfolioValue, err := portfolio.GetTotalValue()
	// 				if err != nil {
	// 					log.Errorf(collection.Strategy, "failed to get portfolio value: %v", err)
	// 				} else {
	// 					collection.SetTotalValueMetric(strategyID, portfolioValue)
	// 				}
	// 			}
	// 		}
	// 		if reducedPositions.PositionsUpdate {
	// 			change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
	// 			if err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update positions: %v", err)
	// 			} else {
	// 				change.Summary("Check all positions close required change")
	// 			}
	// 		}
	// 	case data := <-websocketSignals:
	// 		tn := time.Now()
	// 		d, ok := data.Data.(*orderbook.Depth)
	// 		if !ok {
	// 			log.Errorf(collection.Strategy, "failed to type assert data: %v", data)
	// 			continue
	// 		}

	// 		obKey := d.Key()
	// 		k1 := collection.ExchangePairKey{Exchange: data.Exchange, Base: obKey.Base, Quote: obKey.Quote, Asset: obKey.Asset}
	// 		position := positions.HedgedPosition(k1)
	// 		if position != nil {
	// 			reduce := CheckSignalPositionReduce(ctx, strategyID, position, positions, toggle, prices, details, matches, settlement, exclude, portfolio, prices, details)
	// 			if reduce.Position != nil {
	// 				change := positions.UpdateHedgedPosition(reduce.Position)
	// 				change.Summary("Websocket Position Reduce")
	// 				time.Sleep(time.Second * 2) // TODO: Remove this, its for debugging.
	// 				os.Exit(0)                  // TODO: Remove this, its for debugging.
	// 			}
	// 			if reduce.PortfolioUpdate {
	// 				if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 					log.Errorf(collection.Strategy, "failed to update portfolio: %v", err)
	// 				} else {
	// 					portfolioValue, err := portfolio.GetTotalValue()
	// 					if err != nil {
	// 						log.Errorf(collection.Strategy, "failed to get portfolio value: %v", err)
	// 					} else {
	// 						collection.SetTotalValueMetric(strategyID, portfolioValue)
	// 					}
	// 				}
	// 			}
	// 			if reduce.PositionsUpdate {
	// 				change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
	// 				if err != nil {
	// 					log.Errorf(collection.Strategy, "failed to update positions: %v", err)
	// 				} else {
	// 					change.Summary("Websocket Position Reduce Required Change")
	// 				}
	// 			}

	// 			if reduce.PortfolioUpdate || reduce.PositionsUpdate || reduce.Position != nil {
	// 				signalProcessMetric.SetLatency(time.Since(tn))
	// 				continue // No need to check for increase if we have reduced/ changed the position.
	// 			}
	// 		}

	// 		increase, err := CheckSignalPositionIncrease(ctx, strategyID, k1, portfolio, position, positions, toggle, prices, details, matches, fees, settlement, exclude, prices, details)
	// 		if err != nil {
	// 			log.Warnf(collection.Strategy, "system level reset detected flushing system: %v", err)
	// 			hourTimer.Reset(0) // Force a flush of pairs as this should be a delisting issue.
	// 		}
	// 		if increase.Position != nil {
	// 			// TODO: If theoretical position is used, might need to add a delay so that we don't close the position before the exchange has updated.
	// 			// There might be an ADL issue here if opened at an incorrect book state or high leverage.
	// 			change := positions.UpdateHedgedPosition(increase.Position)
	// 			change.Summary("Websocket Position Increase")
	// 		}
	// 		if increase.PortfolioUpdate {
	// 			if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update balances: %v", err)
	// 			} else {
	// 				portfolioValue, err := portfolio.GetTotalValue()
	// 				if err != nil {
	// 					log.Errorf(collection.Strategy, "failed to get account value: %v", err)
	// 				} else {
	// 					collection.SetTotalValueMetric(strategyID, portfolioValue)
	// 				}
	// 			}
	// 		}
	// 		if increase.PositionsUpdate {
	// 			change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
	// 			if err != nil {
	// 				log.Errorf(collection.Strategy, "failed to update positions: %v", err)
	// 			} else {
	// 				change.Summary("Websocket Position Increase Required Change")
	// 			}
	// 		}

	// 		signalProcessMetric.SetLatency(time.Since(tn))
	// 	}
	// }
}
