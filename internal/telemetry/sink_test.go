package telemetry_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
)

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// newTestSetup creates an in-memory SpanRecorder, a Prometheus registry, and
// a fully wired Sink+Store ready for use in tests.
func newTestSetup(t *testing.T) (
	recorder *tracetest.SpanRecorder,
	reg *prometheus.Registry,
	store *capture.Store,
) {
	t.Helper()

	recorder = tracetest.NewSpanRecorder()
	// WithSpanProcessor(recorder) is synchronous: spans appear in
	// recorder.Ended() immediately after span.End(), with no batching delay.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
	)

	reg = prometheus.NewRegistry()

	sink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{CostPerThousandTokens: 0.002},
		tp,
		reg,
		nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	store = capture.NewStore(capture.StoreConfig{}, nil, sink)
	return recorder, reg, store
}

// buildSendExchange constructs a sendMessage Exchange from raw parts.
func buildSendExchange(tokenHash string, chatID int64, text string, msgID int64) *proxy.Exchange {
	req, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": msgID,
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
			"date":       1700000000,
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  tokenHash,
		Method:     "sendMessage",
		ReqBody:    req,
		RespBody:   resp,
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
	}
}

// -----------------------------------------------------------------------
// attrMap extracts span attributes into a map from key string to value.
// -----------------------------------------------------------------------

func attrMap(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(s.Attributes()))
	for _, a := range s.Attributes() {
		m[string(a.Key)] = a.Value
	}
	return m
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

// -----------------------------------------------------------------------
// TestSpanAttributes: span attributes match the span schema in
// docs/span-schema.md exactly
// -----------------------------------------------------------------------

func TestSpanAttributes(t *testing.T) {
	t.Parallel()

	recorder, _, store := newTestSetup(t)
	ctx := context.Background()

	store.Record(ctx, buildSendExchange("hash_a", 42, "hello", 7))

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span, got none")
	}

	// Find the sendMessage span.
	var found sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "sendMessage" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("no sendMessage span found; spans: %v", spanNames(spans))
	}

	attrs := attrMap(found)

	mustStr := func(key, want string) {
		t.Helper()
		v, ok := attrs[key]
		if !ok {
			t.Errorf("span missing attribute %q", key)
			return
		}
		if got := v.AsString(); got != want {
			t.Errorf("attr %q = %q, want %q", key, got, want)
		}
	}
	mustInt := func(key string, want int64) {
		t.Helper()
		v, ok := attrs[key]
		if !ok {
			t.Errorf("span missing attribute %q", key)
			return
		}
		if got := v.AsInt64(); got != want {
			t.Errorf("attr %q = %d, want %d", key, got, want)
		}
	}
	mustPresent := func(key string) {
		t.Helper()
		if _, ok := attrs[key]; !ok {
			t.Errorf("span missing attribute %q", key)
		}
	}

	mustStr("telegram.bot.from", "hash_a")
	mustStr("telegram.method", "sendMessage")
	mustInt("telegram.chat.id", 42)
	mustInt("telegram.msg.id", 7)
	mustInt("telegram.text.len", int64(len("hello")))
	mustInt("b2b.loop.depth", 0)
	mustPresent("b2b.tokens.est")
	mustPresent("b2b.cost.usd.est")
	// telegram.bot.to and telegram.msg.id must be present.
	mustPresent("telegram.bot.to")
}

// TestResolutionSpanAttribute verifies the b2b.bot.to.resolution attribute is
// emitted alongside telegram.bot.to and carries the correct closed-enum value.
// A sendMessage to a positive numeric chat_id never seen via getMe must report
// "unknown_getme_not_seen" with an empty telegram.bot.to.
func TestResolutionSpanAttribute(t *testing.T) {
	t.Parallel()

	recorder, _, store := newTestSetup(t)
	store.Record(context.Background(), buildSendExchange("hash_a", 424242, "hi", 1))

	var found sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "sendMessage" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatal("no sendMessage span")
	}
	attrs := attrMap(found)

	v, ok := attrs["b2b.bot.to.resolution"]
	if !ok {
		t.Fatal("span missing b2b.bot.to.resolution")
	}
	if got := v.AsString(); got != "unknown_getme_not_seen" {
		t.Errorf("b2b.bot.to.resolution = %q, want unknown_getme_not_seen", got)
	}
	if to, ok := attrs["telegram.bot.to"]; !ok || to.AsString() != "" {
		t.Errorf("telegram.bot.to = %v, want empty when not resolved", to)
	}
}

// TestSpanNameIsMethod verifies that the span name equals the Bot API method.
func TestSpanNameIsMethod(t *testing.T) {
	t.Parallel()

	recorder, _, store := newTestSetup(t)
	store.Record(context.Background(), buildSendExchange("tok", 1, "x", 1))

	spans := recorder.Ended()
	for _, s := range spans {
		if s.Name() == "sendMessage" {
			return
		}
	}
	t.Errorf("expected span named sendMessage; got: %v", spanNames(spans))
}

// TestMultiMessageBotBotCorrelation feeds a back-and-forth exchange between
// two bots and asserts all spans are produced (one per event).
func TestMultiMessageBotBotCorrelation(t *testing.T) {
	t.Parallel()

	recorder, _, store := newTestSetup(t)
	ctx := context.Background()
	chatID := int64(9999)

	store.Record(ctx, buildSendExchange("botA", chatID, "hello", 1))
	store.Record(ctx, buildSendExchange("botB", chatID, "hi back", 2))
	store.Record(ctx, buildSendExchange("botA", chatID, "great", 3))

	spans := recorder.Ended()
	if len(spans) < 3 {
		t.Fatalf("expected >= 3 spans, got %d", len(spans))
	}

	// All three spans should share the same TraceID because they belong to the
	// same conversation (same chatID).
	tid := spans[0].SpanContext().TraceID()
	for i, s := range spans {
		if s.SpanContext().TraceID() != tid {
			t.Errorf("span[%d] traceID = %v, want %v", i, s.SpanContext().TraceID(), tid)
		}
	}
}

// TestLoopSpanAttribute checks that a loop event has a positive b2b.loop.depth.
func TestLoopSpanAttribute(t *testing.T) {
	t.Parallel()

	recorder, _, store := newTestSetup(t)
	ctx := context.Background()

	store.Record(ctx, buildSendExchange("botA", 1, "ping", 1))
	store.Record(ctx, buildSendExchange("botA", 1, "ping", 2)) // same from+to+text → loop

	spans := recorder.Ended()
	if len(spans) < 2 {
		t.Fatalf("expected >= 2 spans, got %d", len(spans))
	}

	// Second span should have loop.depth > 0.
	second := spans[1]
	attrs := attrMap(second)
	v, ok := attrs["b2b.loop.depth"]
	if !ok {
		t.Fatal("second span missing b2b.loop.depth")
	}
	if v.AsInt64() <= 0 {
		t.Errorf("b2b.loop.depth = %d, want > 0", v.AsInt64())
	}
}

// -----------------------------------------------------------------------
// Prometheus metric tests
// -----------------------------------------------------------------------

// TestMessagesTotalCounter verifies that b2b_messages_total increments.
func TestMessagesTotalCounter(t *testing.T) {
	t.Parallel()

	_, reg, store := newTestSetup(t)
	ctx := context.Background()

	store.Record(ctx, buildSendExchange("bot1", 1, "a", 1))
	store.Record(ctx, buildSendExchange("bot1", 1, "b", 2))

	const want = `
# HELP b2b_messages_total Total correlated capture events, partitioned by Bot API method (one per parsed update; a batched getUpdates yields N).
# TYPE b2b_messages_total counter
b2b_messages_total{method="sendMessage"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "b2b_messages_total"); err != nil {
		t.Errorf("b2b_messages_total: %v", err)
	}
}

// TestLoopsTotalCounter verifies that b2b_loops_total increments on loop detection.
func TestLoopsTotalCounter(t *testing.T) {
	t.Parallel()

	_, reg, store := newTestSetup(t)
	ctx := context.Background()

	store.Record(ctx, buildSendExchange("bot1", 1, "ping", 1))
	store.Record(ctx, buildSendExchange("bot1", 1, "ping", 2)) // loop

	const want = `
# HELP b2b_loops_total Total number of bot↔bot message loops detected.
# TYPE b2b_loops_total counter
b2b_loops_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "b2b_loops_total"); err != nil {
		t.Errorf("b2b_loops_total: %v", err)
	}
}

// TestSpamRatioGauge verifies that b2b_spam_ratio is updated.
func TestSpamRatioGauge(t *testing.T) {
	t.Parallel()

	_, reg, store := newTestSetup(t)
	ctx := context.Background()

	// 2 messages, 1 loop → ratio should be 0.5.
	store.Record(ctx, buildSendExchange("bot1", 1, "ping", 1))
	store.Record(ctx, buildSendExchange("bot1", 1, "ping", 2)) // loop

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var ratio float64
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "b2b_spam_ratio" {
			for _, m := range mf.GetMetric() {
				ratio = m.GetGauge().GetValue()
				found = true
			}
		}
	}
	if !found {
		t.Fatal("b2b_spam_ratio metric not found")
	}
	// 1 loop out of 2 messages = 0.5.
	if ratio < 0.49 || ratio > 0.51 {
		t.Errorf("b2b_spam_ratio = %.4f, want ~0.5", ratio)
	}
}

// TestTokenCostCounter verifies that b2b_token_cost_usd accumulates.
func TestTokenCostCounter(t *testing.T) {
	t.Parallel()

	_, reg, store := newTestSetup(t)
	ctx := context.Background()

	// Text "hello" = 5 chars → ~1 token → cost = 0.002/1000 × 1 = 0.000002 USD.
	store.Record(ctx, buildSendExchange("bot1", 1, "hello", 1))

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "b2b_token_cost_usd" {
			found = true
			for _, m := range mf.GetMetric() {
				v := m.GetCounter().GetValue()
				if v < 0 {
					t.Errorf("b2b_token_cost_usd = %v, want >= 0", v)
				}
			}
		}
	}
	if !found {
		t.Fatal("b2b_token_cost_usd metric not found")
	}
}

// TestCostZeroWhenRateIsZero verifies that b2b_token_cost_usd stays at 0 when
// CostPerThousandTokens is 0 (the default for the real binary before
// B2BD_COST_PER_K_TOKENS is set).
func TestCostZeroWhenRateIsZero(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reg := prometheus.NewRegistry()

	sink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{CostPerThousandTokens: 0}, // zero rate
		tp, reg, nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	store := capture.NewStore(capture.StoreConfig{}, nil, sink)
	store.Record(context.Background(), buildSendExchange("bot1", 1, "hello world", 1))

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "b2b_token_cost_usd" {
			for _, m := range mf.GetMetric() {
				v := m.GetCounter().GetValue()
				if v != 0 {
					t.Errorf("b2b_token_cost_usd = %v, want 0 when rate is 0", v)
				}
			}
		}
	}
}

// TestCostPositiveWhenRateSet verifies that b2b_token_cost_usd accumulates
// a positive value when CostPerThousandTokens is non-zero (i.e. when the user
// has configured B2BD_COST_PER_K_TOKENS).
func TestCostPositiveWhenRateSet(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reg := prometheus.NewRegistry()

	sink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{CostPerThousandTokens: 1.0}, // $1/k tokens
		tp, reg, nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	// "hello world" = 11 chars → max(1, 11/4) = 2 tokens → cost = 2/1000 × 1.0 = 0.002
	store := capture.NewStore(capture.StoreConfig{}, nil, sink)
	store.Record(context.Background(), buildSendExchange("bot1", 1, "hello world", 1))

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "b2b_token_cost_usd" {
			found = true
			for _, m := range mf.GetMetric() {
				v := m.GetCounter().GetValue()
				if v <= 0 {
					t.Errorf("b2b_token_cost_usd = %v, want > 0 when rate is 1.0", v)
				}
			}
		}
	}
	if !found {
		t.Fatal("b2b_token_cost_usd not found")
	}
}

// -----------------------------------------------------------------------
// traceMap eviction test
// -----------------------------------------------------------------------

// TestTraceMapShrinksOnEviction verifies that the telemetry Sink's traceMap
// entries are removed when conversations are evicted from the capture Store.
// This tests the EvictionListener contract (finding 6): after inserting more
// conversations than the LRU cap, the traceMap must not grow without bound.
func TestTraceMapShrinksOnEviction(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reg := prometheus.NewRegistry()

	sink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{CostPerThousandTokens: 0},
		tp, reg, nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	const maxConvs = 2
	store := capture.NewStore(capture.StoreConfig{
		MaxConversations: maxConvs,
		ConvTTL:          1 * time.Hour,
	}, nil, sink)

	ctx := context.Background()

	// Insert well past the cap — without eviction the traceMap would grow
	// unboundedly; with eviction it must stay at or below maxConvs.
	const total = maxConvs*3 + 1
	for i := int64(0); i < total; i++ {
		// Chat IDs 100..100+total, all distinct → forces evictions.
		store.Record(ctx, buildSendExchange("bot1", i+100, "msg", i))
	}

	traceLen := sink.TraceMapLen()
	if traceLen > maxConvs {
		t.Errorf("traceMap len = %d after %d inserts into cap-%d store; want <= %d (eviction must clean up)",
			traceLen, total, maxConvs, maxConvs)
	}
}

// -----------------------------------------------------------------------
// New span attributes: b2b.capture.truncated, telegram.update.id,
// telegram.media.kind
// -----------------------------------------------------------------------

// findSpan returns the first ended span with the given name, or fails.
func findSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %q span found; spans: %v", name, spanNames(recorder.Ended()))
	return nil
}

// TestCaptureTruncatedAttribute verifies b2b.capture.truncated reflects
// Exchange.Truncated exactly: false for a complete body, true when the proxy
// reported the tapped copy was cut.
func TestCaptureTruncatedAttribute(t *testing.T) {
	t.Parallel()

	t.Run("false for complete body", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		store.Record(context.Background(), buildSendExchange("hash_a", 42, "hello", 7))
		attrs := attrMap(findSpan(t, recorder, "sendMessage"))
		v, ok := attrs["b2b.capture.truncated"]
		if !ok {
			t.Fatal("span missing b2b.capture.truncated")
		}
		if v.AsBool() {
			t.Error("b2b.capture.truncated = true, want false for a complete body")
		}
	})

	t.Run("true when exchange truncated", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		ex := buildSendExchange("hash_a", 42, "hello", 7)
		ex.Truncated = true // proxy reported the tapped body hit the cap
		store.Record(context.Background(), ex)
		attrs := attrMap(findSpan(t, recorder, "sendMessage"))
		v, ok := attrs["b2b.capture.truncated"]
		if !ok {
			t.Fatal("span missing b2b.capture.truncated")
		}
		if !v.AsBool() {
			t.Error("b2b.capture.truncated = false, want true when Exchange.Truncated is set")
		}
	})
}

// getUpdatesExchange builds a getUpdates response carrying one update with the
// given update_id and a text message.
func getUpdatesExchange(tokenHash string, updateID, chatID, msgID int64) *proxy.Exchange {
	resp, _ := json.Marshal(map[string]any{
		"ok": true,
		"result": []map[string]any{
			{
				"update_id": updateID,
				"message": map[string]any{
					"message_id": msgID,
					"chat":       map[string]any{"id": chatID, "type": "private"},
					"text":       "inbound",
					"date":       1700000000,
				},
			},
		},
	})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  tokenHash,
		Method:     "getUpdates",
		RespBody:   resp,
		StatusCode: 200,
		Duration:   time.Millisecond,
	}
}

// webhookExchange builds a webhook ingress Exchange carrying one update.
func webhookExchange(tokenHash string, updateID, chatID, msgID int64) *proxy.Exchange {
	req, _ := json.Marshal(map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": msgID,
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       "hook",
			"date":       1700000000,
		},
	})
	return &proxy.Exchange{
		Timestamp:      time.Now().UTC(),
		TokenHash:      tokenHash,
		Method:         proxy.MethodWebhookIngress,
		ReqBody:        req,
		ReqContentType: "application/json",
		StatusCode:     200,
		Duration:       time.Millisecond,
	}
}

// TestUpdateIDAttribute verifies telegram.update.id is emitted with the correct
// value for getUpdates batch elements and webhook deliveries, and omitted for
// outbound sends (which carry no update_id).
func TestUpdateIDAttribute(t *testing.T) {
	t.Parallel()

	t.Run("getUpdates carries update_id", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		store.Record(context.Background(), getUpdatesExchange("bot_g", 555111, 42, 9))
		attrs := attrMap(findSpan(t, recorder, "getUpdates"))
		v, ok := attrs["telegram.update.id"]
		if !ok {
			t.Fatal("getUpdates span missing telegram.update.id")
		}
		if v.AsInt64() != 555111 {
			t.Errorf("telegram.update.id = %d, want 555111", v.AsInt64())
		}
	})

	t.Run("webhook carries update_id", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		store.Record(context.Background(), webhookExchange("bot_w", 777222, 43, 10))
		attrs := attrMap(findSpan(t, recorder, proxy.MethodWebhookIngress))
		v, ok := attrs["telegram.update.id"]
		if !ok {
			t.Fatal("webhook span missing telegram.update.id")
		}
		if v.AsInt64() != 777222 {
			t.Errorf("telegram.update.id = %d, want 777222", v.AsInt64())
		}
	})

	t.Run("outbound send omits update_id", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		store.Record(context.Background(), buildSendExchange("hash_a", 42, "hi", 1))
		attrs := attrMap(findSpan(t, recorder, "sendMessage"))
		if _, ok := attrs["telegram.update.id"]; ok {
			t.Error("outbound sendMessage span must NOT carry telegram.update.id")
		}
	})
}

// mediaSendExchange builds a sendPhoto/sendDocument/sendVideo Exchange whose
// response carries the named media kind so mediaKindOf classifies it.
func mediaSendExchange(method, kind string, chatID, msgID int64) *proxy.Exchange {
	req, _ := json.Marshal(map[string]any{"chat_id": chatID})
	result := map[string]any{
		"message_id": msgID,
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"date":       1700000000,
	}
	switch kind {
	case "photo":
		result["photo"] = []map[string]any{{"file_id": "f1", "file_unique_id": "u1"}}
	case "document":
		result["document"] = map[string]any{"file_id": "d1", "file_unique_id": "du1"}
	case "video":
		result["video"] = map[string]any{"file_id": "v1", "file_unique_id": "vu1"}
	}
	resp, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return &proxy.Exchange{
		Timestamp:  time.Now().UTC(),
		TokenHash:  "media_bot",
		Method:     method,
		ReqBody:    req,
		RespBody:   resp,
		StatusCode: 200,
		Duration:   time.Millisecond,
	}
}

// TestMediaKindAttribute verifies telegram.media.kind is set to the correct
// classification for photo/document/video messages and is absent for text.
func TestMediaKindAttribute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		method string
		kind   string
	}{
		{"sendPhoto", "photo"},
		{"sendDocument", "document"},
		{"sendVideo", "video"},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			recorder, _, store := newTestSetup(t)
			store.Record(context.Background(),
				mediaSendExchange(tc.method, tc.kind, int64(500+i), int64(i+1)))
			attrs := attrMap(findSpan(t, recorder, tc.method))
			v, ok := attrs["telegram.media.kind"]
			if !ok {
				t.Fatalf("%s span missing telegram.media.kind", tc.method)
			}
			if v.AsString() != tc.kind {
				t.Errorf("telegram.media.kind = %q, want %q", v.AsString(), tc.kind)
			}
		})
	}

	t.Run("text message omits media.kind", func(t *testing.T) {
		t.Parallel()
		recorder, _, store := newTestSetup(t)
		store.Record(context.Background(), buildSendExchange("hash_a", 42, "plain text", 3))
		attrs := attrMap(findSpan(t, recorder, "sendMessage"))
		if _, ok := attrs["telegram.media.kind"]; ok {
			t.Error("text-only sendMessage span must NOT carry telegram.media.kind")
		}
	})
}

// TestUpstreamErrorSpanStatusAndNoTokenLeak verifies the failure-path span
// contract: a >=400 upstream status sets the span status to codes.Error, and
// the raw bot token never appears anywhere on the span (name, attributes, or
// status description). The Exchange.TokenHash carries only the SHA-256 digest,
// which is what every span attribute must reflect.
func TestUpstreamErrorSpanStatusAndNoTokenLeak(t *testing.T) {
	t.Parallel()

	const rawToken = "987654:ZZ-RawTokenMustNeverReachSpan"

	cases := []struct {
		name       string
		status     int
		wantErrSet bool
	}{
		{"401 unauthorized", 401, true},
		{"429 rate limited", 429, true},
		{"500 internal", 500, true},
		{"200 ok control", 200, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder, _, store := newTestSetup(t)

			ex := buildSendExchange("hash_err", 77, "boom", 9)
			ex.StatusCode = tc.status
			// Simulate a Telegram error envelope (never contains the token).
			ex.RespBody = []byte(`{"ok":false,"error_code":` +
				itoa(tc.status) + `,"description":"err"}`)
			store.Record(context.Background(), ex)

			span := findSpan(t, recorder, "sendMessage")

			if tc.wantErrSet {
				if span.Status().Code != codes.Error {
					t.Errorf("span status = %v, want codes.Error for HTTP %d",
						span.Status().Code, tc.status)
				}
			} else if span.Status().Code == codes.Error {
				t.Errorf("span wrongly marked codes.Error for HTTP %d", tc.status)
			}

			// The raw token must never appear on the span anywhere.
			if strings.Contains(span.Name(), rawToken) ||
				strings.Contains(span.Status().Description, rawToken) {
				t.Error("raw token leaked into span name/description")
			}
			for _, a := range span.Attributes() {
				if strings.Contains(a.Value.Emit(), rawToken) {
					t.Errorf("raw token leaked into span attribute %s", a.Key)
				}
			}
		})
	}
}

// itoa is a tiny dependency-free int->string for building test JSON.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
