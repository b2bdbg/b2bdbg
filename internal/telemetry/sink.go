package telemetry

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
)

// -----------------------------------------------------------------------
// Token/cost estimation
// -----------------------------------------------------------------------

// DefaultCostPerThousandTokens is the default estimated cost per 1 000 tokens
// in USD. Zero means no cost is accumulated unless the caller configures a
// non-zero rate via [SinkConfig].
//
// This is a deliberately conservative default. Users should set a rate matching
// their actual model's pricing.
const DefaultCostPerThousandTokens = 0.0

// estimateTokens returns a rough token count using the heuristic chars/4.
// This is documented as an approximation only; do not use for billing.
func estimateTokens(text string) int64 {
	if len(text) == 0 {
		return 0
	}
	n := int64(len(text)) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// estimateCost returns the estimated cost in USD for a given token count.
func estimateCost(tokens int64, ratePerK float64) float64 {
	if ratePerK <= 0 {
		return 0
	}
	return float64(tokens) / 1000.0 * ratePerK
}

// -----------------------------------------------------------------------
// SinkConfig
// -----------------------------------------------------------------------

// SinkConfig carries the telemetry-specific configuration that is not already
// covered by [config.Config].
type SinkConfig struct {
	// CostPerThousandTokens is the per-1 000-token cost in USD used for the
	// b2b_token_cost_usd metric and b2b.cost.usd.est span attribute.
	// Default: 0.0 (no cost accumulated).
	CostPerThousandTokens float64
}

// -----------------------------------------------------------------------
// Sink
// -----------------------------------------------------------------------

// Sink implements [capture.Listener]. It converts correlated [capture.Event]s
// into OTel spans and Prometheus metric updates.
//
// Construct via [NewSink]. Wire the returned [capture.Listener] into a
// [capture.Store] as a listener; then pass the store to [proxy.New] as the
// [proxy.Sink].
type Sink struct {
	tracer     trace.Tracer
	m          *metrics
	sinkCfg    SinkConfig
	log        *slog.Logger
	traceIndex traceMap

	// running counters for spam-ratio computation
	totalMessages atomic.Int64
	totalLoops    atomic.Int64
}

// NewSink constructs a [Sink] that wires capture events to OTel and Prometheus.
//
//   - cfg is used for OTel endpoint configuration (already applied when building
//     the TracerProvider) and is accepted here for future extension.
//   - tp is the [trace.TracerProvider] from [NewTracerProvider].
//   - reg is the Prometheus registry; all b2b metrics are registered into it.
//   - logger may be nil (a discard logger is used).
//
// NewSink returns an error only if metric registration fails.
func NewSink(
	_ config.Config,
	sinkCfg SinkConfig,
	tp trace.TracerProvider,
	reg prometheus.Registerer,
	logger *slog.Logger,
) (*Sink, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	m, err := newMetrics(reg)
	if err != nil {
		return nil, err
	}

	return &Sink{
		tracer:     tp.Tracer("b2bdbg"),
		m:          m,
		sinkCfg:    sinkCfg,
		log:        logger,
		traceIndex: newTraceMap(),
	}, nil
}

// OnEvict implements [capture.EvictionListener]. It is called by the
// [capture.Store] when a conversation is evicted from the LRU/TTL cache.
// It removes the corresponding traceMap entry to prevent unbounded growth.
func (s *Sink) OnEvict(convID string) {
	s.traceIndex.delete(convID)
}

// TraceMapLen returns the number of active conversation contexts held in the
// traceMap. It is intended for testing only.
func (s *Sink) TraceMapLen() int {
	return s.traceIndex.len()
}

// OnEvent implements [capture.Listener]. It is called synchronously by the
// [capture.Store] after each event is correlated.
//
// OnEvent creates (or continues) an OTel span for the conversation and updates
// all Prometheus metrics. It must be fast and non-blocking.
func (s *Sink) OnEvent(_ context.Context, conv *capture.Conversation, ev capture.Event) {
	pm := ev.ParsedMessage

	// ---- Prometheus --------------------------------------------------------
	method := pm.Method
	if method == "" {
		method = "unknown"
	}
	s.m.messagesTotal.WithLabelValues(method).Inc()

	total := s.totalMessages.Add(1)

	var loops int64
	if ev.LoopDepth > 0 {
		s.m.loopsTotal.Inc()
		loops = s.totalLoops.Add(1)
	} else {
		loops = s.totalLoops.Load()
	}

	if total > 0 {
		s.m.spamRatio.Set(float64(loops) / float64(total))
	}

	tokens := estimateTokens(pm.Text)
	cost := estimateCost(tokens, s.sinkCfg.CostPerThousandTokens)
	if cost > 0 {
		s.m.tokenCostUSD.Add(cost)
	}

	// ---- OTel span ---------------------------------------------------------
	// Span name = Bot API method name.
	spanName := pm.Method
	if spanName == "" {
		spanName = "unknown"
	}

	// Start the span under this conversation's trace. The first event creates
	// and atomically reserves the root context (under the traceMap lock), so
	// concurrent first-events for the same conversation cannot create
	// divergent root traces. Subsequent events become children of that root.
	_, span := s.startConversationSpan(conv.ID, spanName, pm.Timestamp)

	span.SetAttributes(
		attribute.String("telegram.bot.from", pm.FromBot),
		attribute.String("telegram.bot.to", pm.ToBot),
		attribute.String("b2b.bot.to.resolution", string(pm.Resolution)),
		attribute.String("telegram.method", pm.Method),
		attribute.Int64("telegram.chat.id", pm.ChatID),
		attribute.Int64("telegram.msg.id", pm.MessageID),
		attribute.Int64("telegram.text.len", int64(pm.TextLen)),
		attribute.Int("b2b.loop.depth", ev.LoopDepth),
		attribute.Int64("b2b.tokens.est", tokens),
		attribute.Float64("b2b.cost.usd.est", cost),
		// True only when the proxy body tap actually hit the cap for this
		// exchange, so the fields above may be parsed from an incomplete body.
		attribute.Bool("b2b.capture.truncated", pm.Truncated),
	)

	// Inbound-only: Telegram Update.update_id. Outbound sends carry none, so
	// emit only when present (> 0) and never fake a zero.
	if pm.UpdateID > 0 {
		span.SetAttributes(attribute.Int64("telegram.update.id", pm.UpdateID))
	}

	// Coarse media classification, only when the message carried recognised
	// media (kept consistent with MediaKey by mediaKindOf).
	if pm.MediaKind != "" {
		span.SetAttributes(attribute.String("telegram.media.kind", pm.MediaKind))
	}

	if pm.StatusCode >= 400 {
		span.SetStatus(codes.Error, "upstream error")
	}

	span.End(trace.WithTimestamp(pm.Timestamp.Add(pm.Duration)))
}

// startConversationSpan starts the OTel span for an event in conversation id
// and guarantees every span for that conversation shares one trace.
//
// The first event for a conversation creates the root span and reserves its
// context while still holding the traceMap lock. A concurrent first-event for
// the same conversation blocks on that lock and, on acquisition, observes the
// reserved context and becomes a child — so two simultaneous first-events can
// no longer produce two divergent root traces (the get→start→store window in
// the previous implementation). The lock is held across tracer.Start only for
// the (rare) first event per conversation; steady-state events take the lock
// only to read the parent and start their span outside it.
func (s *Sink) startConversationSpan(id, name string, startTS time.Time) (context.Context, trace.Span) {
	tm := &s.traceIndex
	tm.mu.Lock()
	if parent, ok := tm.index[id]; ok {
		tm.mu.Unlock()
		return s.tracer.Start(parent, name, trace.WithTimestamp(startTS))
	}
	ctx, span := s.tracer.Start(context.Background(), name, trace.WithTimestamp(startTS))
	tm.index[id] = ctx
	tm.mu.Unlock()
	return ctx, span
}

// -----------------------------------------------------------------------
// traceMap — per-conversation OTel context index
// -----------------------------------------------------------------------

// traceMap stores the root context for each active conversation so that all
// spans within a conversation are nested under the same trace.
//
// Conversations are keyed by their ID string. The map is never unbounded in
// practice because it mirrors the [capture.Store] LRU (conversations that
// are evicted from the store will not produce new events).
//
// Thread-safety is achieved by an embedded *sync.Mutex held on a pointer
// receiver so that govet's copylocks analyser does not trigger when the
// containing Sink is constructed (Sink holds traceMap by value, but traceMap
// itself stores the mutex on the heap via a pointer).
type traceMap struct {
	mu    *sync.Mutex
	index map[string]context.Context
}

func newTraceMap() traceMap {
	return traceMap{
		mu:    &sync.Mutex{},
		index: make(map[string]context.Context),
	}
}

// Note: parent-context lookup and first-event reservation are done atomically
// in Sink.startConversationSpan (which holds tm.mu across the decision), so
// traceMap no longer exposes separate get/setIfAbsent accessors — splitting
// them allowed a get→start→store race that could fork one conversation into
// multiple root traces.

func (tm *traceMap) delete(id string) {
	tm.mu.Lock()
	delete(tm.index, id)
	tm.mu.Unlock()
}

func (tm *traceMap) len() int {
	tm.mu.Lock()
	n := len(tm.index)
	tm.mu.Unlock()
	return n
}
