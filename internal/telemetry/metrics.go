package telemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics holds all Prometheus collectors registered by the telemetry sink.
// Field names match the metric names documented in the span schema in
// docs/span-schema.md exactly.
type metrics struct {
	// b2b_messages_total counts correlated capture *events*, not upstream HTTP
	// calls. One increment per parsed/correlated event: a single getUpdates
	// response carrying N updates increments by N, and a call that produces no
	// correlated event (e.g. getMe, or an unparseable body) does not increment
	// at all. The `method` label is the Bot API method that produced the event.
	messagesTotal *prometheus.CounterVec

	// b2b_loops_total counts detected bot↔bot message loops.
	loopsTotal prometheus.Counter

	// b2b_spam_ratio is a gauge tracking the rolling fraction of exchanged
	// messages that are part of a detected loop within the current process run.
	// It is recomputed as loops_total / messages_total on each update.
	spamRatio prometheus.Gauge

	// b2b_token_cost_usd accumulates estimated LLM token cost in USD.
	tokenCostUSD prometheus.Counter
}

// newMetrics creates and registers all collectors into reg.
// It returns an error if any registration fails.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		messagesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "b2b_messages_total",
			Help: "Total correlated capture events, partitioned by Bot API method (one per parsed update; a batched getUpdates yields N).",
		}, []string{"method"}),

		loopsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "b2b_loops_total",
			Help: "Total number of bot↔bot message loops detected.",
		}),

		spamRatio: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "b2b_spam_ratio",
			Help: "Rolling ratio of looping messages to total messages (0.0–1.0).",
		}),

		tokenCostUSD: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "b2b_token_cost_usd",
			Help: "Estimated cumulative LLM token cost in USD (heuristic: chars/4 tokens × rate).",
		}),
	}

	collectors := []prometheus.Collector{
		m.messagesTotal,
		m.loopsTotal,
		m.spamRatio,
		m.tokenCostUSD,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("telemetry: register metric: %w", err)
		}
	}

	return m, nil
}
