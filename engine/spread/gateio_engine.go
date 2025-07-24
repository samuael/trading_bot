package spread

import (
	"context"
	"fmt"
	"time"

	"github.com/samuael/trading_bot/engine/collections"
	"github.com/thrasher-corp/gocryptotrader/config"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/log"
)

func Setup(e exchange.IBotExchange, configPath string) error {
	cfg := &config.Config{}
	err := cfg.LoadConfig(configPath, true)
	if err != nil {
		return fmt.Errorf("LoadConfig() error: %w", err)
	}
	e.SetDefaults()

	eName := "GateIO"
	exchConf, err := cfg.GetExchangeConfig(eName)
	if err != nil {
		return fmt.Errorf("GetExchangeConfig(%q) error: %w", eName, err)
	}
	err = e.Setup(exchConf)
	if err != nil {
		return fmt.Errorf("Setup() error: %w", err)
	}
	return nil
}

func GetFuturesAssetsFromExchange(exch exchange.IBotExchange) []asset.Item {
	assetTypes := exch.GetBase().Config.CurrencyPairs.GetAssetTypes(true)
	assetTypeList := []asset.Item{}
	for i := range assetTypes {
		if assetTypes[i].IsFutures() {
			assetTypeList = append(assetTypeList, assetTypes[i])
		}
	}
	return assetTypeList
}

func RunGateIO(ctx context.Context, toggle *Toggle, exch *gateio.Exchange) {
	hourTimer := time.NewTimer(0)
	twentyFourHourTimer := time.NewTimer(time.Second)
	tenSecondTimer := time.NewTimer(time.Second * 10)

	settlement := currency.USDT
	// exclude := []currency.Code{point}

	// matches := collections.MatchInfo{}

	strategy := &log.SubLogger{}

	details := new(collections.TradingDetail)
	details.PerpetualInstrumentsList = make(map[collections.PairInfo]map[asset.Item]collections.InstrumentInfo)

	for {
		select {
		case <-hourTimer.C:
			hourTimer.Reset(time.Until(time.Now().Truncate(time.Hour).Add(time.Hour)))
			if toggle.Verbose {
				log.Debugf(strategy, "running hourly sync")
			}

			if toggle.InitialRunComplete {
				// When starting the application the pairs are initially updated, every hour after that we can flush the
				// pairs.
				if toggle.Verbose {
					log.Debugf(strategy, "updating pairs")
				}

				if err := exch.UpdateTradablePairs(ctx, false); err != nil {
					log.Errorf(strategy, "failed to update pairs: %v", err)
				}
			}

			if toggle.Verbose {
				log.Debugf(strategy, "updating contract details")
			}

			// fAsset := GetFuturesAssetsFromExchange(exch)

			if err := details.UpdateExchangeTradingDetails(ctx, exch, settlement, asset.Spot, asset.USDTMarginedFutures); err != nil {
				if !toggle.InitialRunComplete {
					panic(err)
				}
				log.Errorf(strategy, "failed to update contract details: %v", err)
			}

			if toggle.Verbose {
				log.Debugf(strategy, "updating matches")
			}
			// matches := details.UpdateMatches()

			// (exch.Get)
			// 	mainOp := [2]collection.TradeableOption{{Exchange: exch, Asset: asset.Spot}, {Exchange: exch, Asset: fAsset}}
			// 	if err := matches.UpdateMatches(details, mainOp, toggle.Verbose); err != nil {
			// 		if !toggle.InitialRunComplete {
			// 			panic(err)
			// 		}
			// 		log.Errorf(strategy, "failed to update matches: %v", err)
			// 	}

			// 	if toggle.Verbose {
			// 		log.Debugf(strategy, "connection and subscribing websocket")
			// 	}
			// 	if err := collection.WebsocketConnectOrFlushSubs(exch, websocketSignals, collection.GetSubscriptionFilterHookByExchange(exch)); err != nil {
			// 		if !toggle.InitialRunComplete {
			// 			panic(err)
			// 		}
			// 		log.Errorf(strategy, "failed to connect and subscribe to websocket: %v", err)
			// 	}

			// 	if toggle.Verbose {
			// 		log.Debugf(strategy, "updating candles")
			// 	}

			// spotPairs, err := exch.ListSpotCurrencies(ctx)
			// if err != nil {
			// 	panic(err)
			// }
		// 	tradeableAssets = matches.GetExchangeTradeableAssets()
		// 	if err := candles.UpdateCandles(ctx, tradeableAssets); err != nil {
		// 		if !toggle.InitialRunComplete {
		// 			panic(err)
		// 		}
		// 		log.Errorf(strategy, "failed to update candles: %v", err)
		// 	}

		// 	if toggle.Verbose {
		// 		log.Debugf(strategy, "updating stats")
		// 	}
		// 	if err := stats.UpdateStats(candles, matches); err != nil {
		// 		if !toggle.InitialRunComplete {
		// 			panic(err)
		// 		}
		// 		log.Errorf(strategy, "failed to update stats: %v", err)
		// 	}

		// 	tn := time.Now()
		// 	if toggle.Verbose {
		// 		log.Debugf(strategy, "Setting leverage")
		// 	}
		// 	if err := leverage.SetLeverage(ctx, matches); err != nil {
		// 		if !toggle.InitialRunComplete {
		// 			panic(err)
		// 		}
		// 		log.Errorf(strategy, "failed to set leverage: %v", err)
		// 	}
		// 	fmt.Println("Time taken to set leverage:", time.Since(tn))

		// 	// TODO: Weekly candles for leverage calculations

		// 	if toggle.Verbose {
		// 		log.Debugf(strategy, "updating fees")
		// 	}

		// 	if err := fees.UpdateFees(ctx, exch, asset.Spot, fAsset); err != nil {
		// 		if !toggle.InitialRunComplete {
		// 			panic(err)
		// 		}
		// 		log.Errorf(strategy, "failed to update fees: %v", err)
		// 	}

		// 	if toggle.Verbose {
		// 		log.Debugf(strategy, "Updating balances")
		// 	}
		// 	if err := portfolio.UpdatePortfolio(ctx, settlement, exclude, prices, tradeableAssets, toggle.Verbose); err != nil {
		// 		if !toggle.InitialRunComplete {
		// 			panic(err)
		// 		}
		// 		log.Errorf(strategy, "failed to update balances: %v", err)
		// 	} else {
		// 		portfolioValue, err := portfolio.GetTotalValue()
		// 		if err != nil {
		// 			goto checkRun // cannot rebalance without correct balances.
		// 		}
		// 		collection.SetTotalValueMetric(strategyID, portfolioValue)
		// 		log.Debugf(strategy, "portfolio value: %v", portfolioValue)

		// 		if isUnified, err := collection.IsUnifiedAccount(ctx, exch); err != nil && !isUnified {
		// 			if toggle.Verbose {
		// 				log.Debugf(strategy, "checking account balances for transfer potential")
		// 			}
		// 			err = RebalanceBetweenAccounts(ctx, currency.USDT, exch, portfolio)
		// 			if err != nil {
		// 				log.Errorf(strategy, "failed to rebalance between accounts: %v", err)
		// 			}
		// 		}
		// 	}

		// checkRun:
		// 	if toggle.InitialRunOnly {
		// 		log.Debugln(strategy, "Initial run complete")
		// 		return
		// 	}
		// 	if !toggle.InitialRunComplete {
		// 		change, err := positions.UpdatePositions(ctx, settlement, exclude, portfolio, prices, details, matches, tradeableAssets, toggle.Verbose)
		// 		if err != nil {
		// 			log.Errorf(strategy, "failed to update positions: %v", err)
		// 			panic(err)
		// 		}
		// 		change.Summary("Initial Start")
		// 	}
		// 	toggle.InitialRunComplete = true
		case <-twentyFourHourTimer.C:

		case <-tenSecondTimer.C:
		}
	}

}
