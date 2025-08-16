package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samuael/trading_bot/engine/spread"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

var (
	strategyBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "bot_strategy_total_balance_usd",
			Help: "Total balance per strategy in USD",
		},
		[]string{"strategy"}, // <-- label for your strategy name
	)
)

func init() {
	prometheus.MustRegister(strategyBalance)
}

func main() {
	e := new(gateio.Exchange)
	err := spread.Setup(e, "config_example.json")
	if err != nil {
		log.Fatal(err)
	}

	e.API.AuthenticatedSupport = true
	e.API.AuthenticatedWebsocketSupport = true
	e.SetCredentials("", "", "", "", "", "")

	f, err := os.OpenFile("profit_log.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		err := f.Close()
		if err != nil {
			println(err.Error())
		}
	}()

	logger := log.New(f, "", log.LstdFlags)

	go startMetricsServer()

	wg := new(sync.WaitGroup)
	wg.Add(1)
	go spread.RunGateIO(context.Background(), &spread.Toggle{}, e, logger, strategyBalance, wg)
	wg.Wait()
}

func startMetricsServer() {
	http.Handle("/metrics", promhttp.Handler())
	log.Println("Prometheus metrics listening on :2112")
	log.Fatal(http.ListenAndServe(":2112", nil))
}
