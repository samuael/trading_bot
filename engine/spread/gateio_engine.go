package spread

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	nlog "log"

	"github.com/prometheus/client_golang/prometheus"
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

func RunGateIO(ctx context.Context, toggle *Toggle, exch *gateio.Exchange, logger *nlog.Logger, strategyBalance *prometheus.GaugeVec, wg *sync.WaitGroup) {
	defer wg.Done()
	// hourTimer := time.NewTicker(time.Hour)
	// twentyFourHourTimer := time.NewTimer(time.Second)
	tenMinuteTimer := time.NewTimer(time.Minute * 10)
	twentySecondsTicker := time.NewTicker(time.Second * 20)
	fiveMinTicker := time.NewTicker(time.Minute * 5)
	sixHourTimer := time.NewTimer(0)

	settlement := currency.USDT

	var matches []collections.MatchInfo
	var selectedMatchLock sync.Mutex
	var selectedMatches []collections.MatchInfo

	tradesPerHour := 0

	strategy := log.ExchangeSys

	details := new(collections.TradingDetail)
	details.PerpetualInstrumentsList = make(map[collections.PairInfo]map[asset.Item]collections.InstrumentInfo)

	var spotPairsList []gateio.CurrencyPairDetail
	var contracts []gateio.FuturesContract

	var allPositions []gateio.Position

	usdtmFuturesFormat, err := exch.GetPairFormat(asset.USDTMarginedFutures, true)
	if err != nil {
		panic(err)
	}

	for {
		select {
		case <-sixHourTimer.C:
			log.Debugf(strategy, "\nLOOP INTERVAL 6 HOURS: Updating tradable pairs and filtering contracts")
			sixHourTimer.Reset(time.Until(time.Now().Truncate(time.Hour).Add(time.Hour * 6)))
			var err error
			spotPairsList, err = exch.ListSpotCurrencyPairs(ctx)
			if err != nil {
				log.Errorf(strategy, "%v, fatal", err)
			}

			contracts, err = exch.GetAllFutureContracts(ctx, settlement)
			if err != nil {
				log.Errorf(strategy, "%v, fatal", err)
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

			matches = details.UpdateMatches()
			log.Debugf(strategy, "Matches length: %d", len(matches))
			err = GetCandlesOfMatches(exch, matches)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}

			selectedMatchLock.Lock()
			selectedMatches, err = UpdateStats(matches)
			selectedMatchLock.Unlock()
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
			}
			log.Debugf(strategy, "\nSelected Matche Pairs Length: %d", len(selectedMatches))
			tenMinuteTimer.Reset(0)
		case <-tenMinuteTimer.C:
			log.Debugf(strategy, "\nLOOP INTERVAL 10 Mins: Placing orders")
			if tradesPerHour != 0 {
				strategyBalance.WithLabelValues("Trades Per Hour").Set(float64(tradesPerHour))
				tradesPerHour = 0
			}
			tenMinuteTimer.Reset(time.Until(time.Now().Truncate(time.Minute * 10).Add(time.Minute * 10)))
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
					panic(err)
				}
			}

			if toggle.Verbose {
				log.Debugf(strategy, "updating contract details")
			}

			selectedMatchLock.Lock()
			log.Debugf(strategy, "Selected Matches: %d", len(selectedMatches))

			err := UpdateStat(exch, selectedMatches, settlement)
			selectedMatchLock.Unlock()
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
				panic(err)
			}

			selectedMatchLock.Lock()
			for a := range selectedMatches {
				println(selectedMatches[a].SpotSymbol, selectedMatches[a].Spread, selectedMatches[a].LastSpread, selectedMatches[a].Eligible)
			}
			selectedMatchLock.Unlock()

			// Get open positions and
			allPositions, err = exch.GetAllFuturesPositionsOfUsers(context.Background(), currency.USDT, true)
			if err != nil {
				log.Errorf(strategy, "failed to update positions: %v", err)
				panic(err)
			}

			log.Debugf(strategy, "All Positions: %d", len(allPositions))
			// Check if there is an already open position and skip if there is an open position.
			if len(allPositions) > 0 {
				continue
			}

			// sort based on their diff value.
			selectedMatchLock.Lock()
			filteredMatches := SortMatches(selectedMatches, 1)
			selectedMatchLock.Unlock()

			// get account balance information.
			futuresAccount, err := exch.QueryFuturesAccount(context.Background(), currency.USDT)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
				panic(err)
			}

			// Save the 10 percent for margin.
			futuresAccount.Available -= futuresAccount.Available * .1

			for s := range filteredMatches {
				if futuresAccount.Available.Float64() < 5 {
					log.Debugf(strategy, "Available Balance Below Min: %f", futuresAccount.Available.Float64())
					break
				}
				var nextBalance float64
				if futuresAccount.Available.Float64() >= 5 {
					nextBalance = 5.00
					futuresAccount.Available -= 5
				} else {
					nextBalance = futuresAccount.Available.Float64()
					futuresAccount.Available = 0
				}

				contract := currency.Pair{Base: filteredMatches[s].Base, Quote: filteredMatches[s].Quote}.Format(usdtmFuturesFormat)
				switch {
				case filteredMatches[s].Spread > 0 && filteredMatches[s].LastSpread > 0,
					filteredMatches[s].Spread < 0 && filteredMatches[s].LastSpread > 0:
					for c := range allPositions {
						if allPositions[c].Contract == contract.String() && allPositions[c].Size > 0 {
							err := ClosePosition(exch, contract, "close_long")
							if err != nil {
								log.Errorf(strategy, "failed to update positions: %v", err)
								panic(err)
							}
						}
					}
					// position
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        contract,
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Short,
						Amount:      nextBalance,
						Leverage:    1,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
						panic(err)
					}
					tradesPerHour += 1
					// orderDetails = append(orderDetails, result)
				case filteredMatches[s].Spread > 0 && filteredMatches[s].LastSpread < 0,
					filteredMatches[s].Spread < 0 && filteredMatches[s].LastSpread < 0:
					for c := range allPositions {
						if allPositions[c].Contract == contract.String() && allPositions[c].Size < 0 {
							err := ClosePosition(exch, contract, "close_short")
							if err != nil {
								log.Errorf(strategy, "failed to update positions: %v", err)
								panic(err)
							}
						}
					}
					_, err := exch.SubmitOrder(context.Background(), &order.Submit{
						Exchange:    exch.GetName(),
						Pair:        currency.Pair{Base: filteredMatches[s].Base, Quote: filteredMatches[s].Quote},
						TimeInForce: order.FillOrKill,
						Type:        order.Market,
						AssetType:   asset.USDTMarginedFutures,
						Side:        order.Long,
						Amount:      nextBalance,
						Leverage:    1,
					})
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
						panic(err)
					}
					tradesPerHour += 1
					// orderDetails = append(orderDetails, result)
				}
				// setting the leverages.
				_, err = exch.UpdateFuturesPositionLeverage(context.Background(), settlement, contract, 1, 1)
				if err != nil {
					log.Errorf(strategy, "error: %v", err)
					continue
				}
				// break
			}
		case <-fiveMinTicker.C:
			log.Debugf(strategy, "\nLOOP INTERVAL 5 Mins: Updating balance information")
			// Update stats of selected instruments.
			if len(selectedMatches) < 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var err error
					selectedMatchLock.Lock()
					selectedMatches, err = UpdateStats(matches)
					selectedMatchLock.Unlock()
					if err != nil {
						log.Errorf(strategy, "error: %v", err)
						panic(err)
					}

					log.Debugf(strategy, "Selected Pairs Length: %d", len(selectedMatches))
				}()
			}

			// log the balance
			futuresAccount, err := exch.QueryFuturesAccount(context.Background(), currency.USDT)
			if err != nil {
				log.Errorf(strategy, "error: %v", err)
				panic(err)
			}

			log.Debugf(strategy, "FuturesAccount.Available: %f", futuresAccount.Available.Float64())

			strategyBalance.WithLabelValues("SpotPerpetualSpreadArbitrage_GateIO").Set(futuresAccount.Total.Float64())
			logger.Printf("%s,%.2f\n", time.Now().Format(time.RFC3339), futuresAccount.Total.Float64())
		case <-twentySecondsTicker.C:
			log.Debugf(strategy, "\nLOOP INTERVAL 20 SECONDS: Closing active and non-eligible positions")
			var err error
			allPositions, err = exch.GetAllFuturesPositionsOfUsers(context.Background(), currency.USDT, true)
			if err != nil {
				log.Errorf(strategy, "failed to update positions: %v", err)
				panic(err)
			}

			// Update stats for only open positions
			for p := range allPositions {
				err = UpdateStat(exch, selectedMatches, settlement, allPositions[p].Contract)
				if err != nil {
					log.Errorf(strategy, "failed to update positions: %v", err)
					panic(err)
				}
			}

			// Close positions if not eligible and not found under the list of selected pairs.
			for p := range allPositions {
				found := false
				contract, err := currency.NewPairFromString(allPositions[p].Contract)
				if err != nil {
					log.Errorf(strategy, "failed to update positions: %v", err)
					continue
				}

				var autoSize string
				contract = contract.Format(usdtmFuturesFormat)
				if allPositions[p].Mode == "dual_long" {
					autoSize = "close_long"
				} else {
					autoSize = "close_short"
				}
				for s := range selectedMatches {
					if allPositions[p].Contract != selectedMatches[s].FutureSymbol {
						continue
					}
					found = true
					if !selectedMatches[s].Eligible {
						err := ClosePosition(exch, contract, autoSize)
						if err != nil {
							log.Errorf(strategy, "failed to update positions: %v", err)
							panic(err)
						}
						tradesPerHour += 1
					}
				}
				if !found {
					err := ClosePosition(exch, contract, autoSize)
					if err != nil {
						log.Errorf(strategy, "failed to update positions: %v", err)
						panic(err)
					}
					tradesPerHour += 1
				}
			}
		}
	}
}

func ClosePosition(exch *gateio.Exchange, contract currency.Pair, autoSize string) error {
	_, err := exch.PlaceFuturesOrder(context.Background(), &gateio.ContractOrderCreateParams{
		Contract:      contract,
		Size:          0,
		ClosePosition: false,
		// IsClose:       true,
		ReduceOnly:  true,
		TimeInForce: "ioc",
		Settle:      currency.USDT,
		AutoSize:    autoSize,
		// SelfTradePreventionAction: "",
	})
	return err
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

func UpdateStat(exch *gateio.Exchange, matches []collections.MatchInfo, settlementCcy currency.Code, contracts ...string) error {
	for m := range matches {
		if len(contracts) > 0 {
			if !slices.Contains(contracts, matches[m].FutureSymbol) {
				continue
			}
		}
		intervalString, err := gateio.GetIntervalString(kline.ThirtyMin)
		if err != nil {
			return err
		}
		sBooks, err := exch.GetOrderbook(context.Background(), matches[m].SpotSymbol, intervalString, 10, true)
		if err != nil {
			return err
		}
		exch.Verbose = true
		fBooks, err := exch.GetFuturesOrderbook(context.Background(), settlementCcy, matches[m].FutureSymbol, intervalString, 1, true)
		if err != nil {
			return err
		}
		var sAverage float64
		var sAverageSize float64
		for a := range sBooks.Asks {
			sAverage += sBooks.Asks[a].Price.Float64() * sBooks.Asks[a].Amount.Float64()
			sAverageSize += sBooks.Asks[a].Amount.Float64()
		}
		for b := range sBooks.Bids {
			sAverage += sBooks.Bids[b].Price.Float64() * sBooks.Bids[b].Amount.Float64()
			sAverageSize += sBooks.Bids[b].Amount.Float64()
		}
		sAverage = sAverage / (float64(len(sBooks.Bids)+len(sBooks.Asks)) * sAverageSize)

		var faverage float64
		var faverageSize float64
		for a := range fBooks.Asks {
			faverage += fBooks.Asks[a].Price.Float64() * fBooks.Asks[a].Amount.Float64()
			faverageSize += fBooks.Asks[a].Amount.Float64()
		}
		for b := range fBooks.Bids {
			faverage += (fBooks.Bids[b].Price.Float64() * fBooks.Bids[b].Amount.Float64())
			faverageSize += fBooks.Bids[b].Amount.Float64()
		}
		faverage = faverage / (float64(len(fBooks.Bids)+len(fBooks.Asks)) * faverageSize)
		matches[m].LastSpread = (faverage - sAverage)

		diff := matches[m].Spread - matches[m].LastSpread
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
		err = exch.SetPairs([]currency.Pair{{Base: matches[i].Base, Quote: matches[i].Quote}}, asset.Spot, true)
		if err != nil {
			panic(err)
		}
		err = exch.SetPairs([]currency.Pair{{Base: matches[i].Base, Quote: matches[i].Quote}}, asset.USDTMarginedFutures, true)
		if err != nil {
			panic(err)
		}
		matches[i].FuturesCandles, err = exch.GetHistoricCandles(context.Background(), currency.Pair{Base: matches[i].Base, Quote: matches[i].Quote}, asset.USDTMarginedFutures, kline.OneHour, time.Now().Add(-200*time.Hour), time.Now())
		if err != nil {
			return err
		} else if matches[i].FuturesCandles == nil {
			panic("Nil pointer for matches[i].FuturesCandles")
		}
		if len(matches[i].FuturesCandles.Candles) < 100 {
			continue
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
