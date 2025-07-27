package spread

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/samuael/trading_bot/engine/collections"
	"github.com/thrasher-corp/gocryptotrader/config"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
	"github.com/thrasher-corp/gocryptotrader/exchanges/kline"
	"github.com/thrasher-corp/gocryptotrader/exchanges/order"
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

func RunGateIO(ctx context.Context, toggle *Toggle, exch *gateio.Exchange, wg *sync.WaitGroup) {
	defer wg.Done()
	hourTimer := time.NewTimer(0)
	// twentyFourHourTimer := time.NewTimer(time.Second)
	tenSecondTimer := time.NewTimer(time.Second * 10)
	settlement := currency.USDT

	var selectedMatches []collections.MatchInfo
	// var orderDetails = []*order.SubmitResponse{}
	// exclude := []currency.Code{point}
	// matches := collections.MatchInfo{}

	strategy := &log.SubLogger{}

	details := new(collections.TradingDetail)
	details.PerpetualInstrumentsList = make(map[collections.PairInfo]map[asset.Item]collections.InstrumentInfo)

	spotPairsList, err := exch.ListSpotCurrencyPairs(ctx)
	if err != nil {
		log.Errorf(strategy, "%v, fatal", err)
	}

	contracts, err := exch.GetAllFutureContracts(ctx, settlement)
	if err != nil {
		log.Errorf(strategy, "%v, fatal", err)
	}

	var allPositions []gateio.Position

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

			if err := details.UpdateExchangeTradingDetails(ctx, exch, settlement, spotPairsList, contracts); err != nil {
				if !toggle.InitialRunComplete {
					panic(err)
				}
				log.Errorf(strategy, "failed to update contract details: %v", err)
			}

			if toggle.Verbose {
				log.Debugf(strategy, "updating matches")
			}

			matches := details.UpdateMatches()
			println("Matches length: ", len(matches), "\n")
			err = GetCandlesOfMatches(exch, matches)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}

			selectedMatches, err = UpdateStats(matches)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}

			println("selectedMatches: ", len(selectedMatches))
			err = UpdateStat(exch, selectedMatches)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}
			for a := range selectedMatches {
				println(selectedMatches[a].SpotSymbol, selectedMatches[a].Spread, selectedMatches[a].LastSpread, selectedMatches[a].Eligible)
			}

			// sort based on their diff value.
			selectedMatches = SortMatches(selectedMatches, 1)

			// get account balance information.

			// spotAccount, err := exch.GetSpotAccounts(context.Background(), currency.USDT)
			// if err != nil {
			// 	log.Errorf(strategy, "error: %v", err)
			// }

			futuresAccount, err := exch.QueryFuturesAccount(context.Background(), currency.USDT)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}

			// Save the 10 percent for margin.
			futuresAccount.Available -= futuresAccount.Available * .1

			for s := range selectedMatches {
				if futuresAccount.Available < 10 {
					break
				}
				var nextBalance float64
				if futuresAccount.Available > 50 {
					nextBalance = 50.00
					futuresAccount.Available -= 50
				} else {
					nextBalance = futuresAccount.Available.Float64()
					futuresAccount.Available = 0
				}
				switch {
				case selectedMatches[s].Spread > 0 && selectedMatches[s].LastSpread > 0:
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        currency.Pair{Base: selectedMatches[s].Base, Quote: selectedMatches[s].Quote},
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Short,
						Amount:      nextBalance,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
					}
					// orderDetails = append(orderDetails, result)
				case selectedMatches[s].Spread > 0 && selectedMatches[s].LastSpread < 0:
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        currency.Pair{Base: selectedMatches[s].Base, Quote: selectedMatches[s].Quote},
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Long,
						Amount:      nextBalance,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
					}
					// orderDetails = append(orderDetails, result)
				case selectedMatches[s].Spread < 0 && selectedMatches[s].LastSpread < 0:
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        currency.Pair{Base: selectedMatches[s].Base, Quote: selectedMatches[s].Quote},
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Long,
						Amount:      nextBalance,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
					}
					// orderDetails = append(orderDetails, result)
				case selectedMatches[s].Spread < 0 && selectedMatches[s].LastSpread > 0:
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        currency.Pair{Base: selectedMatches[s].Base, Quote: selectedMatches[s].Quote},
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Short,
						Amount:      nextBalance,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
					}
					// orderDetails = append(orderDetails, result)
				}
				// break
			}
		case <-tenSecondTimer.C:
			println("ticking ...")
			allPositions, err = exch.GetAllFuturesPositionsOfUsers(context.Background(), currency.USDT, true)
			if err != nil {
				log.Errorf(strategy, "failed to update positions: %v", err)
				panic(err)
			}

			err = UpdateStat(exch, selectedMatches)
			if err != nil {
				log.Errorf(strategy, "failed to update positions: %v", err)
				panic(err)
			}

			for s := range selectedMatches {
				for p := range allPositions {
					if allPositions[p].Contract == selectedMatches[s].FutureSymbol && !selectedMatches[s].Eligible {
						err := exch.CancelOrder(context.Background(), &order.Cancel{
							AssetType: asset.USDTMarginedFutures,
							OrderID:   allPositions[p].Contract,
						})
						if err != nil {
							log.Errorf(strategy, "failed to update positions: %v", err)
							panic(err)
						}
					}
				}
			}
		}
	}
}

func SortMatches(matches []collections.MatchInfo, leng int) []collections.MatchInfo {
	var eligible []collections.MatchInfo
	for _, m := range matches {
		if m.Eligible {
			eligible = append(eligible, m)
		}
	}

	// Sort by Diff descending
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].Diff > eligible[j].Diff
	})

	// Return top `leng` elements, or all if fewer
	if len(eligible) < leng {
		return eligible
	}
	return eligible[:leng]
}

func UpdateStat(exch *gateio.Exchange, matches []collections.MatchInfo) error {
	for m := range matches {
		sTicker, err := exch.UpdateTicker(context.Background(), currency.Pair{Base: matches[m].Base, Quote: matches[m].Quote}, asset.Spot)
		if err != nil {
			return err
		}
		fTicker, err := exch.UpdateTicker(context.Background(), currency.Pair{Base: matches[m].Base, Quote: matches[m].Quote}, asset.USDTMarginedFutures)
		if err != nil {
			return err
		}
		matches[m].LastSpread = (fTicker.Last - sTicker.Last) / sTicker.Last
		diff := math.Abs(matches[m].Spread - matches[m].LastSpread)
		matches[m].Eligible = diff > 0.005
		matches[m].Diff = diff
	}
	return nil
}

func UpdateStats(matches []collections.MatchInfo) ([]collections.MatchInfo, error) {
	if len(matches) == 0 {
		return nil, errors.New("no matche found")
	}
	if matches[0].SpotCandles == nil {
		return nil, kline.ErrInsufficientCandleData
	}

	selectedStats := []collections.MatchInfo{}

	for i := range matches {
		if matches[i].SpotCandles == nil {
			continue
		}
		if len(matches[i].SpotCandles.Candles) == 0 {
			continue
		}
		lastTime := matches[i].SpotCandles.Candles[0].Time

		if len(matches[i].SpotCandles.Candles) != len(matches[i].FuturesCandles.Candles) {
			return nil, errors.New("futures and spot candles length difference")
		}

		totalSpread := 0.0
		for c := range matches[i].SpotCandles.Candles {
			if matches[i].SpotCandles.Candles[c].Time.Before(lastTime) || !matches[i].SpotCandles.Candles[c].Time.Equal(matches[i].FuturesCandles.Candles[c].Time) {
				break
			}
			lastTime = matches[i].SpotCandles.Candles[c].Time
			totalSpread += (matches[i].FuturesCandles.Candles[c].Close - matches[i].SpotCandles.Candles[c].Close) / matches[i].SpotCandles.Candles[c].Close
		}
		matches[i].Spread = totalSpread / float64(len(matches[i].FuturesCandles.Candles))
		if math.Abs(matches[i].Spread) > 0.0045 { // if the spread is greater than 0.45% in the last candles
			selectedStats = append(selectedStats, matches[i])
		}
	}
	return selectedStats, nil
}

func GetCandlesOfMatches(exch *gateio.Exchange, matches []collections.MatchInfo) error {
	var err error
	for i := range matches {
		exch.SetPairs([]currency.Pair{{Base: matches[i].Base, Quote: matches[i].Quote}}, asset.Spot, true)
		exch.SetPairs([]currency.Pair{{Base: matches[i].Base, Quote: matches[i].Quote}}, asset.USDTMarginedFutures, true)
		matches[i].FuturesCandles, err = exch.GetHistoricCandles(context.Background(), currency.Pair{Base: matches[i].Base, Quote: matches[i].Quote}, asset.USDTMarginedFutures, kline.OneHour, time.Now().Add(-200*time.Hour), time.Now())
		if err != nil {
			return err
		} else if matches[i].FuturesCandles == nil {
			panic("Nil pointer for matches[i].FuturesCandles")
		}
		matches[i].SpotCandles, err = exch.GetHistoricCandles(context.Background(), currency.Pair{Base: matches[i].Base, Quote: matches[i].Quote}, asset.Spot, kline.OneHour, time.Now().Add(-200*time.Hour), time.Now())
		if err != nil {
			return err
		} else if matches[i].SpotCandles == nil {
			panic("Nil pointer for matches[i].SpotCandles")
		}
	}
	return nil
}
