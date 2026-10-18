package spread

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Config holds the tunable parameters of the spot/perpetual basis strategy.
// All ratios are fractions: 0.005 means 0.5%.
type Config struct {
	// DryRun evaluates and logs trades without placing orders or moving funds.
	DryRun bool `json:"dry_run"`

	// MinEntrySpread is the minimum gross executable basis (perp bid vs spot ask) required to open.
	MinEntrySpread float64 `json:"min_entry_spread"`
	// MaxEntrySpread rejects bases so wide they almost always mean the spot and perp track different assets.
	MaxEntrySpread float64 `json:"max_entry_spread"`
	// MaxIndexDeviation requires the perp index price to sit within this fraction of the spot price, proving both
	// markets reference the same asset.
	MaxIndexDeviation float64 `json:"max_index_deviation"`
	// ExitSpread closes a position once the executable exit basis (perp ask vs spot bid) is at or below it.
	ExitSpread float64 `json:"exit_spread"`
	// MinNetProfit is the minimum expected return after round trip taker fees, assuming exit at ExitSpread.
	MinNetProfit float64 `json:"min_net_profit"`

	// MaxOpenPositions caps concurrently held spot/perp pairs.
	MaxOpenPositions int `json:"max_open_positions"`
	// MinTradeNotional is the smallest position (USDT) worth opening.
	MinTradeNotional float64 `json:"min_trade_notional"`
	// MaxTradeNotional caps a single position (USDT). Zero means no cap beyond available funds.
	MaxTradeNotional float64 `json:"max_trade_notional"`
	// FuturesMarginReserve is the fraction of futures available balance kept aside as extra margin.
	FuturesMarginReserve float64 `json:"futures_margin_reserve"`
	// AutoRebalance transfers USDT between spot and futures accounts so both legs can be funded.
	AutoRebalance bool `json:"auto_rebalance"`

	// MaxSlippage limits how deep into either book a position may be sized, relative to the best price.
	MaxSlippage float64 `json:"max_slippage"`
	// HedgeSlippage is the extra price allowance on the spot leg that hedges an already filled perp leg.
	HedgeSlippage float64 `json:"hedge_slippage"`
	// ExitSlippage is the price allowance on both legs when closing.
	ExitSlippage float64 `json:"exit_slippage"`
	// LiquidationBuffer force closes a position when the mark price is within this fraction of the liquidation price.
	LiquidationBuffer float64 `json:"liquidation_buffer"`

	// MinFundingRate skips contracts whose current funding rate is below it (a short pays funding when negative).
	MinFundingRate float64 `json:"min_funding_rate"`
	// MinSpotVolume24h and MinFuturesVolume24h filter out illiquid markets (quote volume, USDT).
	MinSpotVolume24h    float64 `json:"min_spot_volume_24h"`
	MinFuturesVolume24h float64 `json:"min_futures_volume_24h"`
	// MaxCandidates limits how many ticker-ranked pairs get a full orderbook check per tick.
	MaxCandidates int `json:"max_candidates"`
	// OrderbookDepth is the number of levels requested per book.
	OrderbookDepth uint64 `json:"orderbook_depth"`

	TickIntervalSeconds    int `json:"tick_interval_seconds"`
	UniverseRefreshMinutes int `json:"universe_refresh_minutes"`
	BalanceLogMinutes      int `json:"balance_log_minutes"`
	FeeCacheMinutes        int `json:"fee_cache_minutes"`

	// StateFile persists open positions across restarts; LedgerFile receives one CSV row per closed position.
	StateFile  string `json:"state_file"`
	LedgerFile string `json:"ledger_file"`
}

// DefaultConfig returns the default strategy parameters.
func DefaultConfig() Config {
	return Config{
		DryRun:                 true,
		MinEntrySpread:         0.005,
		MaxEntrySpread:         0.03,
		MaxIndexDeviation:      0.005,
		ExitSpread:             0.001,
		MinNetProfit:           0.001,
		MaxOpenPositions:       10,
		MinTradeNotional:       5,
		FuturesMarginReserve:   0.1,
		AutoRebalance:          true,
		MaxSlippage:            0.002,
		HedgeSlippage:          0.002,
		ExitSlippage:           0.002,
		LiquidationBuffer:      0.15,
		MinFundingRate:         0,
		MinSpotVolume24h:       50_000,
		MinFuturesVolume24h:    50_000,
		MaxCandidates:          10,
		OrderbookDepth:         20,
		TickIntervalSeconds:    5,
		UniverseRefreshMinutes: 360,
		BalanceLogMinutes:      5,
		FeeCacheMinutes:        60,
		StateFile:              "positions.json",
		LedgerFile:             "trades.csv",
	}
}

// LoadConfig reads a JSON strategy config on top of the defaults. An empty path returns the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read strategy config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse strategy config: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate checks the config for values that would make the strategy unsafe or inert.
func (c Config) Validate() error {
	var errs []error
	if c.MinEntrySpread <= 0 {
		errs = append(errs, errors.New("min_entry_spread must be positive"))
	}
	if c.MaxEntrySpread <= c.MinEntrySpread {
		errs = append(errs, errors.New("max_entry_spread must be above min_entry_spread"))
	}
	if c.MaxIndexDeviation <= 0 {
		errs = append(errs, errors.New("max_index_deviation must be positive"))
	}
	if c.ExitSpread >= c.MinEntrySpread {
		errs = append(errs, errors.New("exit_spread must be below min_entry_spread"))
	}
	if c.MaxOpenPositions <= 0 {
		errs = append(errs, errors.New("max_open_positions must be positive"))
	}
	if c.MinTradeNotional <= 0 {
		errs = append(errs, errors.New("min_trade_notional must be positive"))
	}
	if c.MaxTradeNotional != 0 && c.MaxTradeNotional < c.MinTradeNotional {
		errs = append(errs, errors.New("max_trade_notional must be zero or at least min_trade_notional"))
	}
	if c.FuturesMarginReserve < 0 || c.FuturesMarginReserve >= 1 {
		errs = append(errs, errors.New("futures_margin_reserve must be in [0, 1)"))
	}
	for name, v := range map[string]float64{"max_slippage": c.MaxSlippage, "hedge_slippage": c.HedgeSlippage, "exit_slippage": c.ExitSlippage} {
		if v < 0 || v >= 0.05 {
			errs = append(errs, fmt.Errorf("%s must be in [0, 0.05)", name))
		}
	}
	if c.MaxCandidates <= 0 || c.OrderbookDepth == 0 {
		errs = append(errs, errors.New("max_candidates and orderbook_depth must be positive"))
	}
	if c.TickIntervalSeconds <= 0 || c.UniverseRefreshMinutes <= 0 || c.BalanceLogMinutes <= 0 || c.FeeCacheMinutes <= 0 {
		errs = append(errs, errors.New("intervals must be positive"))
	}
	if c.StateFile == "" || c.LedgerFile == "" {
		errs = append(errs, errors.New("state_file and ledger_file must be set"))
	}
	return errors.Join(errs...)
}

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }
