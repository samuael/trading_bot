package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samuael/trading_bot/engine/spread"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func main() {
	exchangeConfig := flag.String("exchange-config", "config_example.json", "gocryptotrader config holding the GateIO exchange settings")
	strategyConfig := flag.String("strategy-config", "strategy.json", "strategy parameters; defaults are used for missing fields")
	metricsAddr := flag.String("metrics-addr", ":2113", "address serving Prometheus metrics")
	profitLog := flag.String("profit-log", "profit_log.csv", "file receiving a total balance line every balance_log_minutes")
	verbose := flag.Bool("v", false, "log rejected candidates and other debug detail")
	httpTimeout := flag.Duration("http-timeout", time.Minute, "REST request timeout; full market lists are several MB")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg, err := spread.LoadConfig(*strategyConfig)
	if err != nil {
		log.Fatal(err)
	}

	apiKey, apiSecret := os.Getenv("GATEIO_API_KEY"), os.Getenv("GATEIO_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		log.Fatal("GATEIO_API_KEY and GATEIO_API_SECRET must be set")
	}

	e := new(gateio.Exchange)
	if err := spread.Setup(e, *exchangeConfig); err != nil {
		log.Fatal(err)
	}
	e.API.AuthenticatedSupport = true
	e.API.AuthenticatedWebsocketSupport = true
	e.SetCredentials(apiKey, apiSecret, "", "", "", "")
	if err := e.SetHTTPClientTimeout(*httpTimeout); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := e.ValidateAPICredentials(ctx, asset.Spot); err != nil {
		log.Fatal(err)
	}

	f, err := os.OpenFile(*profitLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	metrics := spread.NewMetrics(prometheus.DefaultRegisterer)
	go startMetricsServer(*metricsAddr)

	client := spread.NewGateIOClient(e, *httpTimeout)
	var market spread.MarketData // nil serves market data from REST
	if cfg.UseWebsocket {
		feed := spread.NewFeed(client, cfg, logger, metrics)
		go feed.Run(ctx)
		market = feed
	}

	engine, err := spread.NewEngine(cfg, client, market, logger, metrics, log.New(f, "", log.LstdFlags))
	if err != nil {
		log.Fatal(err)
	}
	if err := engine.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func startMetricsServer(addr string) {
	http.Handle("/metrics", promhttp.Handler())
	log.Println("Prometheus metrics listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
