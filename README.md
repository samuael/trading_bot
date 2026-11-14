# GateIO spot/perpetual spread bot

Buys spot and shorts the USDT perpetual of the same coin when the perp trades far enough above spot ("carry"), then unwinds both legs once the gap closes. The position is hedged, so it profits from the gap closing and from funding paid to shorts, not from price moves. Reverse trading (sell borrowed spot, long the perp) exists but is **off** in [strategy.json](strategy.json); see below.

## How it trades
Every 5 seconds the bot:

1. **Checks open positions.** It reconciles them with the exchange, records funding, and closes a position once the executable exit spread (perp ask vs spot bid) is at or below `exit_spread` (0.05%).
2. **Looks for entries.** Tickers shortlist up to `max_candidates` pairs, and the real order books then price each one. A pair qualifies when all of these hold:
   - the gross executable spread (perp bid vs spot ask) is between `min_entry_spread` (0.8%) and `max_entry_spread` (2.5%);
   - `spread − exit_spread − taker fees on all four legs ≥ min_net_profit` (0.3%). Fees come from your account's fee tier;
   - funding is at least `min_funding_rate`, so the short is paid while it waits;
   - the perp index is within 0.5% of spot, so both markets track the same asset;
   - both markets trade at least 250k USDT a day.
3. **Waits for the spread to persist.** A qualifying pair is only entered after it has qualified continuously for `min_spread_persist_seconds` (10s). Brief spikes are usually the perp moving ahead of spot, and they vanish before the spot leg can fill.

**Execution.** The perp short goes first as an IOC limit. The spot buy then hedges what filled, with an allowance of up to half the expected profit, so a slightly moved market still hedges at a profit. Unhedged contracts are bought back immediately. Closes keep both legs matched and fall back to market orders after three missed limits.

**Sizing.** Free capital is split across free slots (`max_open_positions`: 5). Each position is between 10 and 40 USDT and limited by order-book depth within 0.2% of the best price.

## Safeguards
- **Hedge-failure cooldown.** If the spot hedge misses and the perp has to be unwound, that pair is paused for `hedge_fail_cooldown_minutes` (60). The pause doubles with each consecutive failure, up to 24h.
- **Circuit breaker.** After `max_hedge_failures_per_hour` (3) failed hedges, all new entries stop for an hour.
- **Idle margin sweep.** At startup and every instrument refresh, USDT sitting in isolated margin accounts with no loan and no managed position goes back to spot (`sweep_idle_margin`). Accounts with an unpaid loan, or holding coins, are logged for you to clean up by hand.
- **Other guards.**
  - A position whose perp is within 15% of liquidation is closed.
  - If the perp leg disappears, the spot leg is sold.
  - Perp positions the bot didn't open are never touched, and that pair isn't traded.
  - The futures account must be in single-position mode.

## Running it
```sh
export GATEIO_API_KEY=... GATEIO_API_SECRET=...
go build -o trading_bot .
./trading_bot -dry-run -v   # live data, no orders or transfers; -v logs why candidates were rejected
./trading_bot               # trades for real when strategy.json has "dry_run": false
```
Run **only one instance per account**. Each instance owns the positions in its own `positions.json` and treats every other perp position as unmanaged. Two instances would compete for the same balances.

State and output:
- `positions.json` holds open positions, so a restart resumes them.
- `trades.csv` gets one row per closed position: entry and exit spread, fees, funding, net P&L.
- `profit_log.csv` gets the total balance every 5 minutes.
- Prometheus listens on `:2113`. `bot_spread_hedge_failures_total` should stay near zero.

Tests: `go test -race ./...` runs offline. `go test -tags live -run Live -v ./engine/spread` checks public market data.

## Why reverse trading is off
Reverse entries sell coins borrowed on isolated margin. On the thin pairs where the perp trades below spot, the borrow usually fails or costs a lot. Each failed attempt left collateral in a margin account, and the bot used to retry every 5 seconds. The cooldown now stops the retries, but the borrow costs and failure rate still make reverse a net loser on GateIO alts. Set `enable_reverse` to `true` only if you have a reason to.

## What to expect
Real carry spreads of 0.8% or more on liquid GateIO pairs are rare and short-lived; most of the time the bot will sit idle. That's deliberate. At 0.3% round-trip fees, smaller spreads don't leave enough after slippage, and trading them is what lost money before. Profit comes from occasional dislocations plus funding while positions are held. Expect few trades and small, positive results per trade, not steady daily income.
