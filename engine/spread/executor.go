package spread

import (
	"context"
	"fmt"
	"time"
)

const (
	// orderTimeout bounds a multi-leg order sequence. Sequences ignore shutdown so a leg is never left unhedged
	// because the process was asked to stop between two orders.
	orderTimeout = 30 * time.Second
	// marketCloseAfter switches a close that keeps missing its IOC limits to market orders.
	marketCloseAfter = 3
	leverage         = 1
)

// open executes an entry plan. The perp short goes first at a limit no worse than planned; the spot buy then hedges
// whatever filled. Any perp exposure the spot fill does not cover is bought back straight away. It returns nil
// without error when the perp leg did not fill.
func (e *Engine) open(ctx context.Context, plan EntryPlan) (*Position, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orderTimeout)
	defer cancel()

	inst := plan.Inst
	key := inst.Key()
	if !e.leverageSet[key] {
		if err := e.client.SetLeverage(ctx, inst.Contract, leverage); err != nil {
			return nil, fmt.Errorf("set leverage: %w", err)
		}
		e.leverageSet[key] = true
	}

	fut, err := e.client.FuturesOrder(ctx, FuturesOrderRequest{
		Contract:  inst.Contract,
		Sell:      true,
		Contracts: plan.Contracts,
		Price:     plan.FuturesLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("perp leg: %w", err)
	}
	if fut.Contracts == 0 {
		e.log.Info("perp leg did not fill, skipping", "instrument", key, "limit", plan.FuturesLimit)
		return nil, nil
	}

	pos := newPosition(plan, e.now())
	pos.recordFutures(fut, true, plan.Fees)
	e.positions[key] = pos
	e.save()

	spot, err := e.client.SpotOrder(ctx, SpotOrderRequest{
		Pair:  inst.SpotPair,
		Buy:   true,
		Qty:   grossSpotQty(fut.Contracts*inst.Multiplier, plan.Fees, inst),
		Price: plan.SpotLimit,
	})
	if err != nil {
		e.log.Error("spot hedge failed, unwinding perp leg", "instrument", key, "err", err)
	}
	pos.recordSpot(spot, true, plan.Fees)
	e.save()
	e.metrics.Trades.WithLabelValues("open").Inc()

	if err := e.trimHedge(ctx, pos, plan.Fees); err != nil {
		e.log.Error("position is not fully hedged, retrying next tick", "instrument", key, "err", err)
	}
	if _, ok := e.positions[key]; !ok {
		return nil, nil
	}
	e.log.Info("opened position",
		"instrument", key,
		"contracts", pos.OpenContracts(),
		"spot", pos.SpotHeld(),
		"spot_cost", pos.SpotCost,
		"planned_basis", plan.Basis,
		"filled_basis", basis(pos.FutSellNotional/(pos.ContractsSold*inst.Multiplier), spot.AvgPrice),
		"expected_net", plan.ExpectedNet,
	)
	return pos, nil
}

// trimHedge buys back perp contracts the spot holding does not cover. When no hedged contracts remain it sells any
// spot bought and closes the record.
func (e *Engine) trimHedge(ctx context.Context, pos *Position, fees Fees) error {
	defer e.save()
	if excess := pos.OpenContracts() - pos.HedgedContracts(); excess > epsilon {
		fill, err := e.client.FuturesOrder(ctx, FuturesOrderRequest{
			Contract:   pos.Inst.Contract,
			Contracts:  excess,
			ReduceOnly: true,
		})
		if err != nil {
			return fmt.Errorf("buy back unhedged contracts: %w", err)
		}
		pos.recordFutures(fill, false, fees)
		if fill.Contracts < excess-epsilon {
			return fmt.Errorf("bought back %v of %v unhedged contracts", fill.Contracts, excess)
		}
	}
	if pos.OpenContracts() > epsilon {
		return nil
	}

	pos.State = stateClosing
	pos.CloseReason = "hedge failed"
	if qty := floorTo(pos.SpotHeld(), pos.Inst.SpotAmountPrecision); sellable(pos.Inst, qty, pos.avgEntrySpotPrice()) {
		fill, err := e.client.SpotOrder(ctx, SpotOrderRequest{Pair: pos.Inst.SpotPair, Qty: qty})
		if err != nil {
			return fmt.Errorf("sell unhedged spot: %w", err)
		}
		pos.recordSpot(fill, false, fees)
	}
	e.finalize(pos)
	return nil
}

// close works a position down towards flat: buy back perp contracts, then sell the spot they no longer hedge.
// Partial fills leave the position in the closing state and the next tick continues.
func (e *Engine) close(ctx context.Context, pos *Position, plan ExitPlan, reason string) error {
	key := pos.Inst.Key()
	if e.cfg.DryRun {
		e.log.Info("dry run: would close", "instrument", key, "reason", reason, "exit_basis", plan.Basis)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orderTimeout)
	defer cancel()
	defer e.save()

	if pos.State != stateClosing {
		pos.State = stateClosing
		pos.CloseReason = reason
		pos.ExitBasis = plan.Basis
		e.save()
	}
	pos.CloseAttempts++
	useMarket := pos.CloseAttempts > marketCloseAfter
	fees := e.cachedFees(pos.Inst)

	if plan.Contracts > 0 {
		req := FuturesOrderRequest{Contract: pos.Inst.Contract, Contracts: plan.Contracts, Price: plan.FuturesLimit, ReduceOnly: true}
		if useMarket {
			req.Price = 0
		}
		fill, err := e.client.FuturesOrder(ctx, req)
		if err != nil {
			return fmt.Errorf("buy back perp: %w", err)
		}
		pos.recordFutures(fill, false, fees)
	}

	// Sell only the spot the remaining short no longer needs, so the position stays hedged between partial fills.
	qty := pos.SpotHeld() - pos.OpenContracts()*pos.Inst.Multiplier
	qty = floorTo(qty, pos.Inst.SpotAmountPrecision)
	if sellable(pos.Inst, qty, plan.SpotVWAP) {
		req := SpotOrderRequest{Pair: pos.Inst.SpotPair, Qty: qty, Price: plan.SpotLimit}
		if useMarket {
			req.Price = 0
		}
		fill, err := e.client.SpotOrder(ctx, req)
		if err != nil {
			return fmt.Errorf("sell spot: %w", err)
		}
		pos.recordSpot(fill, false, fees)
	}

	if pos.OpenContracts() <= epsilon && !sellable(pos.Inst, floorTo(pos.SpotHeld(), pos.Inst.SpotAmountPrecision), plan.SpotVWAP) {
		e.finalize(pos)
	}
	return nil
}

// finalize records a flat position in the ledger and stops tracking it. Spot dust below the exchange minimum
// stays in the account.
func (e *Engine) finalize(pos *Position) {
	key := pos.Inst.Key()
	if err := AppendLedger(e.cfg.LedgerFile, pos, e.now()); err != nil {
		e.log.Error("write ledger", "instrument", key, "err", err)
	}
	e.metrics.RealizedPnL.Add(pos.RealizedPnL())
	e.metrics.Fees.Add(pos.TotalFees())
	e.metrics.Funding.Add(pos.Funding)
	e.metrics.Trades.WithLabelValues("close").Inc()
	e.metrics.Basis.DeleteLabelValues(key)
	e.log.Info("closed position",
		"instrument", key,
		"reason", pos.CloseReason,
		"entry_basis", pos.EntryBasis,
		"exit_basis", pos.ExitBasis,
		"fees", pos.TotalFees(),
		"funding", pos.Funding,
		"net_pnl", pos.RealizedPnL(),
		"spot_dust", pos.SpotHeld(),
	)
	delete(e.positions, key)
	e.save()
}

// sellable reports whether qty meets the spot minimums at price.
func sellable(inst Instrument, qty, price float64) bool {
	return qty > 0 && qty >= inst.SpotMinBase && qty*price >= inst.SpotMinQuote
}

func (p *Position) avgEntrySpotPrice() float64 {
	if p.SpotBought <= 0 {
		return 0
	}
	return p.SpotCost / p.SpotBought
}
