I rewrote the bot as a real two-leg spread trade with your four settings built in. Everything builds and vets clean, and the 24 offline tests pass, including under `-race`. A separate test against GateIO's live public data also passed. No live orders have been placed yet. The changes are staged but not committed.

## How it trades
Every 5 seconds the bot does two things.

**It checks open positions.** It compares them with the exchange and pulls in funding payments. It closes a position when the basis you'd actually get on exit (perp ask vs spot bid) drops to **0.1% or less**.

**It looks for new entries.** One ticker call per market shortlists pairs. It then reads the real order books for the best 10 and opens a position only if both of these hold:
- the **gross spread is at least 0.5%** (perp bid vs spot ask, using the prices you'd actually fill at), and
- `spread − 0.1% exit − fees on all four legs` is **at least `min_net_profit`**, which defaults to 0.1%.

Your fee rates come from GateIO's personal-fee endpoint and are cached for an hour.

**Sizing ("as far as funds allow"):** free capital is split evenly across the open slots, up to 10 positions. Each position is limited by:
- what both accounts can fund: spot pays for the coins, futures holds 1x margin plus a 10% reserve;
- order-book depth within 0.2% of the best price;
- a $5 minimum.

**Execution:** the perp short goes in first as an IOC limit order. The spot buy then hedges whatever filled. If the spot order fills only partly or fails, the extra short is bought back immediately. Closes keep both legs matched. If a close misses its limits three times, it switches to market orders.

## Things you should know
- **Same-name, different-asset markets.** The live test showed `OPENAI_USDT` with a 74% "spread". The perp and the spot token almost certainly track different things. I added two guards: the perp's index price must be within 0.5% of the spot price, and any spread above 3% is rejected. Both cases are now filtered out.
- **Fees can eat the 0.5% spread.** Your fee rates determine how much of each spread you keep. For example, if round-trip taker fees are 0.3%, the bot needs about a 0.5% spread before it trades.
- **Other safety rules:**
  - If the perp is within 15% of its liquidation price, the position is closed.
  - If the perp leg disappears (liquidated or closed by hand), the spot leg is sold.
  - Positions the bot didn't open, such as anything left over from the old bot, are never touched, and that pair isn't traded.
  - The futures account must be in single-position mode. The bot switches it automatically only if no positions are open.
- **Automatic transfers.** With `auto_rebalance` on, the bot moves USDT between your spot and futures accounts so both sides can be funded. It's on by default; set it to `false` if you'd rather move funds yourself.
- **Library version.** gocryptotrader is now pinned in [go.mod](go.mod) to the upstream commit your fork's `master` matches. The old code no longer compiled against any available version.

## Running it
```sh
export GATEIO_API_KEY=... GATEIO_API_SECRET=...
go build -o trading_bot . && ./trading_bot -v     # -v logs why candidates were rejected
```
[strategy.json](strategy.json) ships with **`"dry_run": true`**: the bot logs the trades it would make without placing any. Run it like that first, then set it to `false`.

Each closed position adds a row to `trades.csv` with entry and exit spreads, fees, funding and net profit. Open positions are saved to `positions.json`, so a restart picks them up again. Balance logging to `profit_log.csv` and Prometheus on `:2112` work as before, with new series for P&L, fees and funding.

## What changed
- **Replaced:** the old engine, the `collections` package and the candle-history selection. The new code is in [engine/spread/](engine/spread/): `plan.go`, `pricing.go` and `position.go` hold the math, `executor.go` places orders, `engine.go` runs the loop, and `client.go` talks to GateIO.
- **Removed:** the committed 26 MB binary is no longer tracked, and [.gitignore](.gitignore) is fixed.
- **Live test:** run it with `go test -tags live -run Live -v ./engine/spread`.

**Not done:**
- Trading when the perp is cheaper than spot (sell spot, long the perp) is still out, as agreed.
- Prices come from REST polling every 5 seconds rather than WebSocket feeds.
- The order-placing code has only been tested against the fake exchange. The first live run with keys will be its first real check.