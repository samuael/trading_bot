package spread

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock lets a test move the engine's time forward.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func withClock(e *Engine) *fakeClock {
	clock := &fakeClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	e.now = clock.now
	return clock
}

func TestEngineCoolsDownAfterHedgeFailure(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotErr = errors.New("spot rejected")
	e := newTestEngine(t, c, func(cfg *Config) { cfg.HedgeFailCooldownMinutes = 60; cfg.MaxHedgeFailuresPerHour = 0 })
	clock := withClock(e)

	e.tick(t.Context())
	require.Len(t, c.futuresOrders, 2, "perp opened and unwound once")

	for range 10 {
		clock.advance(5 * time.Second)
		e.tick(t.Context())
	}
	assert.Len(t, c.futuresOrders, 2, "the failed instrument is not retried every tick")

	clock.advance(time.Hour)
	e.tick(t.Context())
	require.Len(t, c.futuresOrders, 4, "retried once the cooldown ends")

	clock.advance(time.Hour + time.Minute)
	e.tick(t.Context())
	assert.Len(t, c.futuresOrders, 4, "a second consecutive failure doubles the pause")
	clock.advance(time.Hour)
	e.tick(t.Context())
	assert.Len(t, c.futuresOrders, 6)

	c.spotErr = nil
	clock.advance(5 * time.Hour)
	e.tick(t.Context())
	require.Len(t, e.positions, 1, "a working hedge opens normally")
	assert.Zero(t, e.hedgeFails[testKey], "success resets the failure count")
}

func TestEngineCircuitBreakerPausesAllEntries(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.spotErr = errors.New("spot rejected")
	e := newTestEngine(t, c, func(cfg *Config) { cfg.HedgeFailCooldownMinutes = 0; cfg.MaxHedgeFailuresPerHour = 2 })
	clock := withClock(e)

	e.tick(t.Context())
	clock.advance(5 * time.Second)
	e.tick(t.Context())
	require.Len(t, c.futuresOrders, 4, "two failed entries")
	assert.True(t, e.pausedUntil.After(clock.now()))

	clock.advance(30 * time.Minute)
	e.tick(t.Context())
	assert.Len(t, c.futuresOrders, 4, "no entries while the breaker is open")

	c.spotErr = nil
	clock.advance(31 * time.Minute)
	e.tick(t.Context())
	assert.Len(t, e.positions, 1, "entries resume after the pause")
}

func TestEngineWaitsForSpreadToPersist(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MinSpreadPersistSeconds = 10 })
	clock := withClock(e)

	e.tick(t.Context())
	clock.advance(5 * time.Second)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders, "a spread seen for 5s is not acted on")

	// The spread collapses, so the clock restarts.
	wide := c.futuresBids
	c.futuresBids, c.futuresLast = deepBook(100.1), 100.1
	clock.advance(5 * time.Second)
	e.tick(t.Context())
	c.futuresBids, c.futuresLast = wide, 100.7
	clock.advance(5 * time.Second)
	e.tick(t.Context())
	clock.advance(5 * time.Second)
	e.tick(t.Context())
	assert.Empty(t, c.futuresOrders, "an interrupted spread has to qualify again")

	clock.advance(5 * time.Second)
	e.tick(t.Context())
	assert.Len(t, e.positions, 1, "opened once the spread held for 10s")
}

func TestEngineSweepsIdleMarginCollateral(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.margin.QuoteAvailable = 4.37
	e := newTestEngine(t, c, nil)
	e.sweepIdleMargin(t.Context())
	assert.Equal(t, []float64{-4.37}, c.marginMoves, "collateral returned to spot")
	assert.InDelta(t, 104.37, c.spotFree, 1e-9)

	c = newFakeClient()
	c.margin = MarginAccount{QuoteAvailable: 4, BaseBorrowed: 0.01, BaseAvailable: -0.0001}
	e = newTestEngine(t, c, nil)
	e.sweepIdleMargin(t.Context())
	assert.Empty(t, c.marginMoves, "collateral behind an unpaid loan stays")

	c = newFakeClient()
	c.margin.QuoteAvailable = 4
	e = newTestEngine(t, c, func(cfg *Config) { cfg.DryRun = true })
	e.sweepIdleMargin(t.Context())
	assert.Empty(t, c.marginMoves, "dry run moves nothing")
	f, err := e.freeFunds(t.Context())
	require.NoError(t, err)
	assert.InDelta(t, 104, f.spot, 1e-9, "but sizes as if the collateral were back in spot")

	c = newFakeClient()
	c.margin.QuoteAvailable = 4
	e = newTestEngine(t, c, func(cfg *Config) { cfg.SweepIdleMargin = false })
	e.sweepIdleMargin(t.Context())
	assert.Empty(t, c.marginMoves)
}

func TestEngineSweepSkipsManagedReversePosition(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	e := newTestEngine(t, c, func(cfg *Config) { cfg.MaxOpenPositions = 1 })
	e.tick(t.Context())
	require.Len(t, e.positions, 1)
	moves := len(c.marginMoves)
	e.sweepIdleMargin(t.Context())
	assert.Len(t, c.marginMoves, moves, "collateral of a held position is not touched")
}

func TestEngineReverseReturnsCollateralWhenPerpMisses(t *testing.T) {
	t.Parallel()
	c := newReverseFakeClient()
	c.futuresFillRatio = 0
	e := newTestEngine(t, c, nil)

	e.tick(t.Context())
	assert.Empty(t, e.positions)
	require.Len(t, c.marginMoves, 2)
	assert.Positive(t, c.marginMoves[0], "collateral posted")
	assert.InDelta(t, -c.marginMoves[0], c.marginMoves[1], 1e-9, "and returned")
	assert.Zero(t, c.margin.QuoteAvailable)
}

func TestHedgeAllowance(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	assert.InDelta(t, 0.002, hedgeAllowance(0.003, cfg), 1e-12, "small edges keep the configured allowance")
	assert.InDelta(t, 0.0035, hedgeAllowance(0.007, cfg), 1e-12, "half the expected net return")
	assert.InDelta(t, maxHedgeAllowance, hedgeAllowance(0.05, cfg), 1e-12, "capped")
}
