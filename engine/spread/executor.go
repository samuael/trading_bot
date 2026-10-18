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
	// aggressiveBuySlippage prices spot buys that must fill. GateIO sizes market buys in quote, so a buy back of an
	// exact base amount is sent as a limit this far through the ask.
	aggressiveBuySlippage = 0.03
	leverage              = 1
)

// open executes an entry plan. The perp leg goes first at a limit no worse than planned; the spot leg then hedges
// whatever filled. Any perp exposure the spot fill does not cover is unwound straight away. It returns nil without
// error when the perp leg did not fill.
func (e *Engine) open(ctx context.Context, plan EntryPlan) (*Position, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orderTimeout)
	defer cancel()

	inst := plan.Inst
	key := inst.Key()
	carry := plan.Direction == Carry
	if !e.leverageSet[key] {
		if err := e.client.SetLeverage(ctx, inst.Contract, leverage); err != nil {
			return nil, fmt.Errorf("set leverage: %w", err)
		}
		e.leverageSet[key] = true
	}
	if !carry {
		if err := e.ensureCollateral(ctx, inst, plan.Notional*e.cfg.MarginCollateralRatio); err != nil {
			return nil, fmt.Errorf("margin collateral: %w", err)
		}
	}

	fut, err := e.client.FuturesOrder(ctx, FuturesOrderRequest{
		Contract:  inst.Contract,
		Sell:      carry,
		Contracts: plan.Contracts,
		Price:     plan.FuturesLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("perp leg: %w", err)
	}
	if fut.Contracts == 0 {
		e.log.Info("perp leg did not fill, skipping", "instrument", key, "direction", plan.Direction, "limit", plan.FuturesLimit)
		return nil, nil
	}

	pos := newPosition(plan, e.now())
	pos.recordFutures(fut, carry, plan.Fees)
	e.positions[key] = pos
	e.save()

	req := SpotOrderRequest{Pair: inst.SpotPair, Buy: carry, Price: plan.SpotLimit}
	if carry {
		req.Qty = grossSpotQty(fut.Contracts*inst.Multiplier, plan.Fees, inst)
	} else {
		req.Qty = ceilTo(fut.Contracts*inst.Multiplier, inst.SpotAmountPrecision)
		req.Margin = true
		req.AutoBorrow = true
	}
	spot, err := e.client.SpotOrder(ctx, req)
	if err != nil {
		e.log.Error("spot hedge failed, unwinding perp leg", "instrument", key, "err", err)
	}
	pos.recordSpot(spot, carry, plan.Fees)
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
		"direction", pos.Dir(),
		"contracts", pos.OpenContracts(),
		"spot_exposure", pos.SpotExposure(),
		"notional", spot.Qty*spot.AvgPrice,
		"planned_spread", plan.Basis,
		"filled_spread", spreadOf(plan.Direction, spot.AvgPrice, fut.AvgPrice),
		"expected_net", plan.ExpectedNet,
	)
	return pos, nil
}

// ensureCollateral tops the pair's isolated margin account up to need USDT from the spot account.
func (e *Engine) ensureCollateral(ctx context.Context, inst Instrument, need float64) error {
	acc, err := e.client.MarginAccount(ctx, inst.SpotPair)
	if err != nil {
		return err
	}
	if short := ceilTo(need-acc.QuoteAvailable, 2); short >= 0.01 {
		if err := e.client.Transfer(ctx, accountSpot, accountMargin, inst.SpotPair, short); err != nil {
			return err
		}
		e.log.Info("moved collateral to margin account", "pair", inst.SpotPair.String(), "usdt", short)
	}
	return nil
}

// trimHedge unwinds perp contracts the spot exposure does not cover. When no hedged contracts remain it flattens
// the spot leg and closes the record.
func (e *Engine) trimHedge(ctx context.Context, pos *Position, fees Fees) error {
	defer e.save()
	if excess := pos.OpenContracts() - pos.HedgedContracts(); excess > epsilon {
		reduce := !pos.perpIsShort()
		fill, err := e.client.FuturesOrder(ctx, FuturesOrderRequest{
			Contract:   pos.Inst.Contract,
			Sell:       reduce,
			Contracts:  excess,
			ReduceOnly: true,
		})
		if err != nil {
			return fmt.Errorf("unwind unhedged contracts: %w", err)
		}
		pos.recordFutures(fill, reduce, fees)
		if fill.Contracts < excess-epsilon {
			return fmt.Errorf("unwound %v of %v unhedged contracts", fill.Contracts, excess)
		}
	}
	if pos.OpenContracts() > epsilon {
		return nil
	}

	pos.State = stateClosing
	pos.CloseReason = "hedge failed"
	price := pos.avgEntrySpotPrice()
	if err := e.reduceSpot(ctx, pos, 0, 0, price, fees); err != nil {
		return fmt.Errorf("flatten unhedged spot: %w", err)
	}
	if e.spotFlat(pos, price) {
		e.finalize(ctx, pos)
	}
	return nil
}

// close works a position down towards flat: unwind perp contracts, then trade away the spot exposure they no
// longer hedge. Partial fills leave the position in the closing state and the next tick continues.
func (e *Engine) close(ctx context.Context, pos *Position, plan ExitPlan, reason string) error {
	key := pos.Inst.Key()
	if e.cfg.DryRun {
		e.log.Info("dry run: would close", "instrument", key, "direction", pos.Dir(), "reason", reason, "exit_spread", plan.Basis)
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
		reduce := !pos.perpIsShort()
		req := FuturesOrderRequest{Contract: pos.Inst.Contract, Sell: reduce, Contracts: plan.Contracts, Price: plan.FuturesLimit, ReduceOnly: true}
		if useMarket {
			req.Price = 0
		}
		fill, err := e.client.FuturesOrder(ctx, req)
		if err != nil {
			return fmt.Errorf("unwind perp: %w", err)
		}
		pos.recordFutures(fill, reduce, fees)
	}

	// Trade only the spot the remaining perp leg no longer hedges, so the position stays hedged between fills.
	limit := plan.SpotLimit
	if useMarket {
		limit = 0
	}
	if err := e.reduceSpot(ctx, pos, pos.OpenContracts()*pos.Inst.Multiplier, limit, plan.SpotVWAP, fees); err != nil {
		return err
	}
	if pos.OpenContracts() <= epsilon && e.spotFlat(pos, plan.SpotVWAP) {
		e.finalize(ctx, pos)
	}
	return nil
}

// reduceSpot trades the spot leg until its exposure is down to target base. Carry sells held base; reverse buys
// base back in the margin account and repays the loan. A zero limit trades at market (carry) or at an aggressive
// limit through the ask (reverse).
func (e *Engine) reduceSpot(ctx context.Context, pos *Position, target, limit, price float64, fees Fees) error {
	inst := pos.Inst
	if pos.Dir() == Carry {
		qty := floorTo(pos.SpotExposure()-target, inst.SpotAmountPrecision)
		if !sellable(inst, qty, price) {
			return nil
		}
		fill, err := e.client.SpotOrder(ctx, SpotOrderRequest{Pair: inst.SpotPair, Qty: qty, Price: limit})
		if err != nil {
			return fmt.Errorf("sell spot: %w", err)
		}
		pos.recordSpot(fill, false, fees)
		return nil
	}

	if _, err := e.refreshMargin(ctx, pos); err != nil {
		return fmt.Errorf("margin account: %w", err)
	}
	if need := pos.SpotExposure() - target; need > epsilon {
		if limit == 0 {
			_, asks, err := e.market.SpotBook(ctx, inst.SpotPair, e.cfg.OrderbookDepth)
			if err != nil {
				return err
			}
			if len(asks) == 0 {
				return errNoLiquidity
			}
			limit = ceilTo(asks[0].Price*(1+aggressiveBuySlippage), inst.SpotPricePrecision)
			price = asks[0].Price
		}
		// Exchange minimums can force buying slightly more than owed; the surplus stays as dust.
		qty := max(grossSpotQty(need, fees, inst), minSpotQty(inst, price))
		fill, err := e.client.SpotOrder(ctx, SpotOrderRequest{Pair: inst.SpotPair, Buy: true, Qty: qty, Price: limit, Margin: true})
		if err != nil {
			return fmt.Errorf("buy back spot: %w", err)
		}
		pos.recordSpot(fill, true, fees)
	}
	return e.repay(ctx, pos)
}

// minSpotQty is the smallest spot order the exchange accepts at price.
func minSpotQty(inst Instrument, price float64) float64 {
	q := inst.SpotMinBase
	if price > 0 {
		q = max(q, inst.SpotMinQuote/price*1.01)
	}
	return ceilTo(q, inst.SpotAmountPrecision)
}

// refreshMargin reads the margin account and records the interest owed.
func (e *Engine) refreshMargin(ctx context.Context, pos *Position) (MarginAccount, error) {
	acc, err := e.client.MarginAccount(ctx, pos.Inst.SpotPair)
	if err != nil {
		return acc, err
	}
	pos.InterestBase = acc.BaseInterest
	return acc, nil
}

// repay pays the margin loan down with the base held in the margin account.
func (e *Engine) repay(ctx context.Context, pos *Position) error {
	acc, err := e.refreshMargin(ctx, pos)
	if err != nil {
		return fmt.Errorf("margin account: %w", err)
	}
	owed := acc.BaseOwed()
	if owed <= 0 || acc.BaseAvailable <= 0 {
		return nil
	}
	all := acc.BaseAvailable >= owed
	amount := floorTo(min(acc.BaseAvailable, owed), 8)
	if err := e.client.Repay(ctx, pos.Inst.SpotPair, amount, all); err != nil {
		return fmt.Errorf("repay: %w", err)
	}
	// GateIO settles interest before principal.
	paidInterest := min(amount, acc.BaseInterest)
	if all {
		paidInterest = acc.BaseInterest
	}
	pos.InterestPaid += paidInterest
	pos.InterestBase = acc.BaseInterest - paidInterest
	return nil
}

// spotFlat reports whether nothing tradable is left on the spot leg.
func (e *Engine) spotFlat(pos *Position, price float64) bool {
	if pos.Dir() == Carry {
		return !sellable(pos.Inst, floorTo(pos.SpotExposure(), pos.Inst.SpotAmountPrecision), price)
	}
	return pos.SpotExposure() <= epsilon
}

// finalize records a flat position in the ledger and stops tracking it. Spot dust below the exchange minimum
// stays in the account; a reverse position's margin collateral returns to the spot account.
func (e *Engine) finalize(ctx context.Context, pos *Position) {
	key := pos.Inst.Key()
	if pos.Dir() == Reverse {
		e.releaseCollateral(ctx, pos.Inst)
	}
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
		"direction", pos.Dir(),
		"reason", pos.CloseReason,
		"entry_spread", pos.EntryBasis,
		"exit_spread", pos.ExitBasis,
		"fees", pos.TotalFees(),
		"funding", pos.Funding,
		"interest_base", pos.InterestPaid+pos.InterestBase,
		"net_pnl", pos.RealizedPnL(),
		"spot_dust", pos.SpotExposure(),
	)
	delete(e.positions, key)
	e.save()
}

// releaseCollateral returns the USDT in a fully repaid margin account to the spot account.
func (e *Engine) releaseCollateral(ctx context.Context, inst Instrument) {
	acc, err := e.client.MarginAccount(ctx, inst.SpotPair)
	if err != nil {
		e.log.Warn("margin account", "pair", inst.SpotPair.String(), "err", err)
		return
	}
	if acc.BaseOwed() > 0 {
		e.log.Warn("margin loan not fully repaid, collateral left in margin account", "pair", inst.SpotPair.String(), "owed", acc.BaseOwed())
		return
	}
	if amount := floorTo(acc.QuoteAvailable, 2); amount >= 0.01 {
		if err := e.client.Transfer(ctx, accountMargin, accountSpot, inst.SpotPair, amount); err != nil {
			e.log.Warn("return margin collateral", "pair", inst.SpotPair.String(), "err", err)
		}
	}
}

// sellable reports whether qty meets the spot minimums at price.
func sellable(inst Instrument, qty, price float64) bool {
	return qty > 0 && qty >= inst.SpotMinBase && qty*price >= inst.SpotMinQuote
}

func (p *Position) avgEntrySpotPrice() float64 {
	if p.Dir() == Reverse {
		if p.SpotSold <= 0 {
			return 0
		}
		return p.SpotProceeds / p.SpotSold
	}
	if p.SpotBought <= 0 {
		return 0
	}
	return p.SpotCost / p.SpotBought
}
