package spread

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the Prometheus series the strategy publishes.
type Metrics struct {
	Balance       *prometheus.GaugeVec
	OpenPositions prometheus.Gauge
	Basis         *prometheus.GaugeVec
	RealizedPnL   prometheus.Gauge
	Fees          prometheus.Counter
	Funding       prometheus.Gauge
	Trades        *prometheus.CounterVec
}

// NewMetrics creates and registers the strategy metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Balance: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bot_strategy_total_balance_usd",
			Help: "Total balance per strategy in USD",
		}, []string{"strategy"}),
		OpenPositions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "bot_spread_open_positions",
			Help: "Hedged spot/perp positions currently held",
		}),
		Basis: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bot_spread_exit_basis",
			Help: "Executable exit basis of each held position",
		}, []string{"instrument"}),
		RealizedPnL: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "bot_spread_realized_pnl_usdt",
			Help: "Net profit of positions closed since start, after fees and funding",
		}),
		Fees: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bot_spread_fees_usdt_total",
			Help: "Trading fees of positions closed since start",
		}),
		Funding: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "bot_spread_funding_usdt",
			Help: "Funding received by positions closed since start",
		}),
		Trades: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bot_spread_positions_total",
			Help: "Positions opened and closed since start",
		}, []string{"action"}),
	}
	reg.MustRegister(m.Balance, m.OpenPositions, m.Basis, m.RealizedPnL, m.Fees, m.Funding, m.Trades)
	return m
}
