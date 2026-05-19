//go:build telegram_e2e
// +build telegram_e2e

// Package main (test file) — OPT-IN, MANUAL real-Telegram end-to-end test.
//
// This file is fenced behind the `telegram_e2e` build tag, so the default
// `go test ./...` / `go build ./...` never compile or run it: it is excluded
// from the standard suite entirely. It is the AUTOMATED equivalent of the
// manual runbook in .dev/real-telegram-smoke.md and is the ONLY validation
// path that exercises b2bdbg (including the proxy gzip-decode-for-capture path)
// against the *real* Telegram Bot API rather than the in-process mock.
//
// It is deliberately NOT run in CI: it needs two real BotFather tokens, makes
// live API calls and is subject to Telegram rate limits. Run it locally with:
//
//	export B2BD_E2E_BOT_TOKEN_A='111111:AAA...'
//	export B2BD_E2E_BOT_TOKEN_B='222222:BBB...'
//	export B2BD_E2E_CHAT_ID='123456789'        # a chat both bots are in,
//	                                            # or bot B's numeric id
//	make test-telegram                          # or:
//	go test -tags telegram_e2e -race -run TestTelegramE2E ./examples/support-team/
//
// Even WITH the tag, if any required env var is unset the test t.Skip()s with
// a clear message (defence in depth) so a tagged-but-token-less run is a SKIP,
// never a failure and never a real API call.
//
// Token safety: the raw token is never logged, printed, or asserted against by
// value other than to assert its ABSENCE. Where a bot identity must be shown,
// only the proxy's existing 16-hex SHA-256 token-hash form is used. The test
// additionally asserts no raw token text appears in any recorded span name,
// span attribute, or captured body.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
)

// realTelegramBaseURL is the real upstream the in-process proxy points at for
// this test. This is the whole point: traffic flows bot → in-process b2bdbg →
// api.telegram.org, so the capture/telemetry pipeline (and the proxy's
// gzip/deflate decode-for-capture path) is validated against responses the
// real Telegram edge actually returns, not a mock.
const realTelegramBaseURL = "https://api.telegram.org"

// e2e env contract — read ONCE, in readE2EEnv. Documented in
// docs/e2e-testing.md and mirrored from .dev/real-telegram-smoke.md.
const (
	// envE2ETokenA / envE2ETokenB are two real BotFather tokens (bots A and B).
	envE2ETokenA = "B2BD_E2E_BOT_TOKEN_A"
	envE2ETokenB = "B2BD_E2E_BOT_TOKEN_B"

	// envE2EChatID is a chat/group id both bots are in (or bot B's numeric id
	// for a direct bot-to-bot send). Kept as a string: it may be a negative
	// group id or a positive user/bot id.
	envE2EChatID = "B2BD_E2E_CHAT_ID"

	// envE2ETimeout optionally overrides the overall test deadline (Go
	// duration, e.g. "90s"). Defaults to defaultE2ETimeout.
	envE2ETimeout = "B2BD_E2E_TIMEOUT"

	defaultE2ETimeout = 60 * time.Second
)

// e2eEnv is the parsed, validated environment contract.
type e2eEnv struct {
	tokenA  string
	tokenB  string
	chatID  string
	timeout time.Duration
}

// readE2EEnv reads the env contract exactly once and reports whether it is
// complete. When incomplete it returns ok=false plus a human-readable reason
// listing the missing variable(s); the caller t.Skip()s on that. It never
// returns or logs a token value.
func readE2EEnv() (e2eEnv, bool, string) {
	env := e2eEnv{
		tokenA:  strings.TrimSpace(os.Getenv(envE2ETokenA)),
		tokenB:  strings.TrimSpace(os.Getenv(envE2ETokenB)),
		chatID:  strings.TrimSpace(os.Getenv(envE2EChatID)),
		timeout: defaultE2ETimeout,
	}

	var missing []string
	if env.tokenA == "" {
		missing = append(missing, envE2ETokenA)
	}
	if env.tokenB == "" {
		missing = append(missing, envE2ETokenB)
	}
	if env.chatID == "" {
		missing = append(missing, envE2EChatID)
	}
	if len(missing) > 0 {
		return e2eEnv{}, false, fmt.Sprintf(
			"set %s/%s and %s to run the real-Telegram e2e test (missing: %s)",
			envE2ETokenA, envE2ETokenB, envE2EChatID, strings.Join(missing, ", "),
		)
	}

	if v := strings.TrimSpace(os.Getenv(envE2ETimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			env.timeout = d
		}
	}
	return env, true, ""
}

// TestTelegramE2E drives a real bot-to-bot exchange through an in-process b2bdbg
// pipeline whose upstream is the real Telegram Bot API, then makes tolerant
// assertions about the spans the pipeline produced.
//
// Wiring (mirrors the in-process examples, upstream swapped for real Telegram):
//   - tracetest.SpanRecorder (in-memory; NOT a Jaeger container)
//   - telemetry.NewSink + capture.NewStoreWithRegistry (so telegram.bot.to can
//     resolve once both getMe responses have been observed)
//   - proxy.New(https://api.telegram.org) on a loopback listener
//   - two real go-telegram-bot-api/v5 clients pointed at the local proxy
//
// Exchange: getMe×2 (implicit, on client construction, through the proxy) →
// bot A sendMessage → bot B getUpdates (polled with a bounded deadline).
func TestTelegramE2E(t *testing.T) {
	env, ok, skipReason := readE2EEnv()
	if !ok {
		t.Skip(skipReason)
	}

	ctx, cancel := context.WithTimeout(context.Background(), env.timeout)
	defer cancel()

	// --- In-process capture + telemetry pipeline ----------------------------
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = tp.Shutdown(shutCtx)
	})

	telSink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{},
		tp,
		prometheus.NewRegistry(),
		discardLogger(t),
	)
	if err != nil {
		t.Fatalf("telemetry.NewSink: %v", err)
	}

	// A real BotRegistry so the two getMe responses (one per bot, issued
	// automatically when the SDK client is constructed) let the capture layer
	// resolve telegram.bot.to for the A→B edge — exactly as the runbook relies
	// on routing both bots' getMe through b2bdbg.
	reg := capture.NewBotRegistry(0)
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, discardLogger(t), reg, telSink)

	// --- In-process proxy pointed at the REAL Telegram upstream -------------
	p, err := proxy.New(realTelegramBaseURL, store, discardLogger(t))
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	proxyEndpoint, stopProxy, err := startLoopbackProxy(p)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(stopProxy)

	// --- Two real bot clients, routed through the in-process proxy ----------
	// NewBotAPIWithAPIEndpoint performs getMe on construction; pointing the
	// endpoint at the proxy means BOTH getMe round-trips flow through b2bdbg and
	// populate the registry (step 4a of the runbook), all against real
	// Telegram. The %s/%s template matches bots.newBot and the runbook.
	apiTemplate := proxyEndpoint + "/bot%s/%s"

	botA, err := tgbotapi.NewBotAPIWithAPIEndpoint(env.tokenA, apiTemplate)
	if err != nil {
		t.Fatalf("construct bot A (getMe through proxy → real Telegram): %v", err)
	}
	botA.Debug = false

	botB, err := tgbotapi.NewBotAPIWithAPIEndpoint(env.tokenB, apiTemplate)
	if err != nil {
		t.Fatalf("construct bot B (getMe through proxy → real Telegram): %v", err)
	}
	botB.Debug = false

	// --- Drive a real exchange: A sends, B long-polls for it ---------------
	chatID, err := parseChatID(env.chatID)
	if err != nil {
		t.Fatalf("parse %s: %v", envE2EChatID, err)
	}

	marker := fmt.Sprintf("b2bdbg-e2e %d", time.Now().UnixNano())
	sent, err := botA.Send(tgbotapi.NewMessage(chatID, marker))
	if err != nil {
		t.Fatalf("bot A sendMessage through proxy → real Telegram: %v", err)
	}
	t.Logf("bot A sent message id=%d to chat=%d", sent.MessageID, chatID)

	// Bot B long-polls getUpdates through the proxy until it observes our
	// marker or the context deadline elapses. No fixed sleeps: each poll uses
	// a bounded server-side long-poll timeout and we loop until ctx is done.
	if recv := pollForMarker(ctx, t, botB, marker); recv {
		t.Log("bot B received bot A's message via getUpdates (A↔B edge driven)")
	} else {
		// Not fatal: bot B may not be a member of the chat, or privacy mode
		// may suppress the update. The getUpdates spans still went through the
		// real upstream and are still asserted on below.
		t.Log("bot B did not observe the marker before deadline " +
			"(privacy mode / membership?) — getMe+sendMessage spans still asserted")
	}

	// Give the synchronous SpanRecorder pipeline a brief, bounded chance to
	// drain any in-flight getUpdates exchange before we read spans.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	<-drainCtx.Done()

	// --- Tolerant assertions over real, non-deterministic traffic ----------
	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded — no traffic reached the in-process pipeline")
	}

	methods := distinctAttrValues(spans, "telegram.method")
	t.Logf("observed telegram.method span attrs: %v", methods)
	if !containsAny(methods, "getMe", "sendMessage", "getUpdates") {
		t.Errorf("want >=1 span with telegram.method in {getMe,sendMessage,getUpdates}, got %v",
			methods)
	}

	assertSendMessageHasFrom(t, spans)
	assertBotToResolutionCoherent(t, spans)
	assertCaptureParsedAgainstRealTelegram(t, spans)
	assertNoRawToken(t, spans, env.tokenA, env.tokenB)
}

// assertSendMessageHasFrom asserts the sendMessage span carries a non-empty
// telegram.bot.from (the sending bot's token hash). Tolerant: only asserts
// when a sendMessage span exists (it always should, since bot A's send is the
// one deterministic write we issue).
func assertSendMessageHasFrom(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, s := range spans {
		if attrString(s, "telegram.method") != "sendMessage" {
			continue
		}
		if from := attrString(s, "telegram.bot.from"); from == "" {
			t.Errorf("sendMessage span has empty telegram.bot.from (want the sender's token hash)")
		} else {
			t.Logf("sendMessage span telegram.bot.from=%s (hash, not raw token)", from)
		}
		return
	}
	t.Log("no sendMessage span found to assert telegram.bot.from on (tolerated)")
}

// assertBotToResolutionCoherent asserts that, where present,
// b2b.bot.to.resolution is one of the closed enum values and that
// telegram.bot.to is non-empty IFF the resolution is "resolved". It does not
// require resolution to be "resolved": that depends on bot B's getMe having
// been observed AND bot A sending to bot B's numeric id, which is environment
// dependent. It only requires the invariant to hold.
func assertBotToResolutionCoherent(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	valid := map[string]bool{
		string(capture.ResolutionResolved):            true,
		string(capture.ResolutionUnknownGetMeNotSeen): true,
		string(capture.ResolutionNonBotChat):          true,
		string(capture.ResolutionStringChatID):        true,
		"":                                            true, // getMe and other non-message spans carry no resolution
	}
	for _, s := range spans {
		res := attrString(s, "b2b.bot.to.resolution")
		if !valid[res] {
			t.Errorf("b2b.bot.to.resolution=%q is not a valid enum value", res)
			continue
		}
		to := attrString(s, "telegram.bot.to")
		resolved := res == string(capture.ResolutionResolved)
		if resolved && to == "" {
			t.Errorf("resolution=resolved but telegram.bot.to is empty (must be non-empty)")
		}
		if !resolved && to != "" {
			t.Errorf("telegram.bot.to=%q non-empty but resolution=%q (must be 'resolved')",
				to, res)
		}
	}
}

// assertCaptureParsedAgainstRealTelegram is the gzip-against-real-Telegram
// angle. The proxy decompresses ONLY the captured copy of any
// Content-Encoding: gzip/deflate response so the capture layer can parse it;
// the forwarded bytes are untouched. We cannot read the raw upstream headers
// from here, so we assert the OBSERVABLE consequence: real Telegram responses
// were parsed coherently end to end — at least one method-bearing span exists,
// and b2b.capture.truncated is a sane boolean on every span (real Bot API
// responses for these tiny calls are far under the 1 MiB cap, so a truncated
// getMe/sendMessage span would indicate a decode/tap regression).
func assertCaptureParsedAgainstRealTelegram(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	var methodBearing, truncatedSmall int
	for _, s := range spans {
		m := attrString(s, "telegram.method")
		if m != "" {
			methodBearing++
		}
		if attrBool(s, "b2b.capture.truncated") &&
			(m == "getMe" || m == "sendMessage") {
			truncatedSmall++
		}
	}
	if methodBearing == 0 {
		t.Error("no method-bearing spans — real Telegram responses were not parsed " +
			"(possible gzip decode-for-capture regression against real Telegram)")
	}
	if truncatedSmall > 0 {
		t.Errorf("%d small getMe/sendMessage span(s) flagged b2b.capture.truncated — "+
			"a tiny real Telegram response should never truncate; "+
			"possible decode-for-capture regression", truncatedSmall)
	}
	t.Logf("capture parsed real Telegram OK: %d method-bearing spans, "+
		"%d small-call truncations (want 0)", methodBearing, truncatedSmall)
}

// assertNoRawToken asserts that neither raw token value appears in any
// recorded span name, span attribute value, or attribute key. This guards the
// invariant that only the 16-hex SHA-256 token-hash is ever serialized.
func assertNoRawToken(t *testing.T, spans []sdktrace.ReadOnlySpan, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		for _, s := range spans {
			if strings.Contains(s.Name(), tok) {
				t.Errorf("RAW TOKEN LEAK: a span name contains a raw bot token")
				return
			}
			for _, a := range s.Attributes() {
				if strings.Contains(string(a.Key), tok) ||
					strings.Contains(a.Value.Emit(), tok) {
					t.Errorf("RAW TOKEN LEAK: span attribute %q carries a raw bot token",
						a.Key)
					return
				}
			}
		}
	}
	t.Log("no raw token found in any recorded span name/attribute (only token-hashes)")
}

// pollForMarker long-polls getUpdates through the proxy until an incoming
// message text contains marker, ctx is done, or a hard error occurs. It
// returns true only when the marker was observed. Each poll uses a bounded
// server-side timeout so there are no fixed client-side sleeps.
func pollForMarker(ctx context.Context, t *testing.T, bot *tgbotapi.BotAPI, marker string) bool {
	t.Helper()
	offset := 0
	for {
		if ctx.Err() != nil {
			return false
		}
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = 3 // seconds of server-side long-poll per request
		updates, err := bot.GetUpdates(u)
		if err != nil {
			// A transient error (rate limit, deadline) is not fatal to the
			// test: the getUpdates span was still produced and is asserted on.
			if ctx.Err() != nil {
				return false
			}
			t.Logf("getUpdates through proxy returned %v (continuing until deadline)", err)
			continue
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			if upd.Message != nil && strings.Contains(upd.Message.Text, marker) {
				return true
			}
		}
	}
}

// containsAny reports whether haystack contains at least one of wants.
func containsAny(haystack []string, wants ...string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, w := range wants {
		if set[w] {
			return true
		}
	}
	return false
}

// parseChatID coerces the B2BD_E2E_CHAT_ID env value to int64. Telegram chat
// ids are integers (negative for groups/channels, positive for users/bots).
func parseChatID(s string) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &id); err != nil {
		return 0, fmt.Errorf("chat id %q must be an integer: %w", s, err)
	}
	if id == 0 {
		return 0, errors.New("chat id must be non-zero")
	}
	return id, nil
}

// startLoopbackProxy serves p on a fresh loopback TCP port and returns the
// base URL ("http://127.0.0.1:<port>") plus a cleanup func that shuts the
// server down with a bounded grace period. It mirrors how the in-process
// examples stand the proxy up on a real listener.
func startLoopbackProxy(p http.Handler) (baseURL string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen loopback: %w", err)
	}
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	stop = func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
	return "http://" + ln.Addr().String(), stop, nil
}

// attrString returns the string value of the named span attribute, or "".
func attrString(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// attrBool returns the bool value of the named span attribute, or false.
func attrBool(s sdktrace.ReadOnlySpan, key string) bool {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsBool()
		}
	}
	return false
}
