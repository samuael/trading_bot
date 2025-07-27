package main

import (
	"context"
	"log"
	"sync"

	"github.com/samuael/trading_bot/engine/spread"
	"github.com/thrasher-corp/gocryptotrader/exchanges/gateio"
)

func main() {
	e := new(gateio.Exchange)
	err := spread.Setup(e, "config_example.json")
	if err != nil {
		log.Fatal(err)
	}

	// if apiKey != "" && apiSecret != "" {
	e.API.AuthenticatedSupport = true
	e.API.AuthenticatedWebsocketSupport = true
	// sam
	e.SetCredentials("", "", "", "", "", "")
	// }
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go spread.RunGateIO(context.Background(), &spread.Toggle{}, e, wg)
	wg.Wait()
}
