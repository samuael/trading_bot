package spread

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Position states.
const (
	stateOpen    = "open"
	stateClosing = "closing"
)

// Position is one hedged spot/perp pair. It accumulates every fill so that realised profit reflects the prices,
// fees and funding actually received, including partial fills and hedge corrections.
type Position struct {
	ID       string     `json:"id"`
	Inst     Instrument `json:"instrument"`
	State    string     `json:"state"`
	OpenedAt time.Time  `json:"opened_at"`

	EntryBasis  float64 `json:"entry_basis"`
	ExpectedNet float64 `json:"expected_net"`
	ExitBasis   float64 `json:"exit_basis"`
	CloseReason string  `json:"close_reason,omitempty"`
	// CloseAttempts counts close passes; repeated misses fall back to market orders.
	CloseAttempts int `json:"close_attempts,omitempty"`

	SpotBought   float64 `json:"spot_bought"`   // base received, net of fees deducted in base
	SpotCost     float64 `json:"spot_cost"`     // USDT paid, including fees charged in USDT
	SpotSold     float64 `json:"spot_sold"`     // base sold
	SpotProceeds float64 `json:"spot_proceeds"` // USDT received, net of fees charged in USDT
	// SpotFees is every spot fee converted to USDT, for reporting. SpotFeesExternal is the part charged in
	// another currency (GT, points) that the balances above do not already reflect.
	SpotFees         float64 `json:"spot_fees"`
	SpotFeesExternal float64 `json:"spot_fees_external"`

	ContractsSold   float64 `json:"contracts_sold"`
	FutSellNotional float64 `json:"fut_sell_notional"`
	ContractsBought float64 `json:"contracts_bought"`
	FutBuyNotional  float64 `json:"fut_buy_notional"`
	FutFees         float64 `json:"fut_fees"`
	// Funding is the funding received (positive) or paid (negative) by the perp short, as reported by the exchange.
	Funding float64 `json:"funding"`
}

func newPosition(plan EntryPlan, now time.Time) *Position {
	return &Position{
		ID:          plan.Inst.Key() + "-" + strconv.FormatInt(now.UnixMilli(), 10),
		Inst:        plan.Inst,
		State:       stateOpen,
		OpenedAt:    now,
		EntryBasis:  plan.Basis,
		ExpectedNet: plan.ExpectedNet,
	}
}

// OpenContracts is the size of the perp short still held.
func (p *Position) OpenContracts() float64 { return p.ContractsSold - p.ContractsBought }

// SpotHeld is the base still held.
func (p *Position) SpotHeld() float64 { return p.SpotBought - p.SpotSold }

// hedgeTolerance treats a spot holding within 1% of a contract as covering it, absorbing fee rounding.
const hedgeTolerance = 0.01

// HedgedContracts is the number of contracts the current spot holding covers.
func (p *Position) HedgedContracts() float64 {
	return math.Floor(p.SpotHeld()/p.Inst.Multiplier + hedgeTolerance)
}

// RealizedPnL is the USDT profit of everything traded so far.
func (p *Position) RealizedPnL() float64 {
	return p.SpotProceeds - p.SpotCost + p.FutSellNotional - p.FutBuyNotional - p.FutFees - p.SpotFeesExternal + p.Funding
}

// TotalFees is every trading fee paid, in USDT.
func (p *Position) TotalFees() float64 { return p.SpotFees + p.FutFees }

func (p *Position) recordSpot(f SpotFill, buy bool, fees Fees) {
	if f.Qty <= 0 {
		return
	}
	notional := f.Qty * f.AvgPrice
	external := 0.0
	if f.FeeInOtherCurrency {
		external = notional * fees.SpotTaker
	}
	if buy {
		p.SpotBought += f.Qty - f.FeeBase
		p.SpotCost += notional + f.FeeQuote
	} else {
		p.SpotSold += f.Qty
		p.SpotProceeds += notional - f.FeeQuote - f.FeeBase*f.AvgPrice
	}
	p.SpotFees += f.FeeBase*f.AvgPrice + f.FeeQuote + external
	p.SpotFeesExternal += external
}

func (p *Position) recordFutures(f FuturesFill, sell bool, fees Fees) {
	if f.Contracts <= 0 {
		return
	}
	notional := f.Contracts * p.Inst.Multiplier * f.AvgPrice
	rate := f.FeeRate
	if rate <= 0 {
		rate = fees.FuturesTaker
	}
	if sell {
		p.ContractsSold += f.Contracts
		p.FutSellNotional += notional
	} else {
		p.ContractsBought += f.Contracts
		p.FutBuyNotional += notional
	}
	p.FutFees += notional * rate
}

// Store persists open positions so a restart resumes managing them.
type Store struct {
	path string
}

// LoadPositions reads persisted positions keyed by instrument. A missing file yields none.
func (s Store) LoadPositions() (map[string]*Position, error) {
	positions := make(map[string]*Position)
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return positions, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Position
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, p := range list {
		positions[p.Inst.Key()] = p
	}
	return positions, nil
}

// SavePositions atomically replaces the persisted positions.
func (s Store) SavePositions(positions map[string]*Position) error {
	list := make([]*Position, 0, len(positions))
	for _, p := range positions {
		list = append(list, p)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var ledgerHeader = []string{
	"closed_at", "id", "instrument", "opened_at", "close_reason", "contracts", "spot_bought", "spot_sold",
	"entry_basis", "expected_net", "exit_basis", "spot_cost", "spot_proceeds", "fut_sell_notional",
	"fut_buy_notional", "spot_fees", "fut_fees", "funding", "net_pnl", "net_return",
}

// AppendLedger writes one CSV row describing a closed position.
func AppendLedger(path string, p *Position, closedAt time.Time) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if errors.Is(statErr, fs.ErrNotExist) {
		if err := w.Write(ledgerHeader); err != nil {
			return err
		}
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }
	netReturn := 0.0
	if p.SpotCost > 0 {
		netReturn = p.RealizedPnL() / p.SpotCost
	}
	row := []string{
		closedAt.Format(time.RFC3339), p.ID, p.Inst.Key(), p.OpenedAt.Format(time.RFC3339), p.CloseReason,
		num(p.ContractsSold), num(p.SpotBought), num(p.SpotSold), num(p.EntryBasis), num(p.ExpectedNet),
		num(p.ExitBasis), num(p.SpotCost), num(p.SpotProceeds), num(p.FutSellNotional), num(p.FutBuyNotional),
		num(p.SpotFees), num(p.FutFees), num(p.Funding), num(p.RealizedPnL()), num(netReturn),
	}
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}
