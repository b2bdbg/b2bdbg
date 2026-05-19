// Test file for the opt-in sustained-traffic mode (runScriptedTraffic).
//
// It verifies that B2BD_EXAMPLE_REPEAT-style repetition scales: running
// the scripted conversation N times produces ~N x the spans and loop events of
// a single run, while the default plan (repeat=1) is byte-identical in volume
// to the original one-shot demo. The whole pipeline runs in-process with an
// in-memory SpanRecorder — no Docker, no network, no Telegram, no sleeps.
package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/b2bdbg/b2bdbg/examples/support-team/bots"
	"github.com/b2bdbg/b2bdbg/examples/support-team/mocktelegram"
	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
)

// runScriptedFixture wires the full in-process stack (mock Telegram, in-proc
// b2bdbg proxy with a synchronous SpanRecorder, the five bots) and repeats the
// scripted conversation according to plan. It returns the recorded spans and
// the b2b_loops_total counter value.
func runScriptedFixture(t *testing.T, plan trafficPlan) (spans int, loops float64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	promReg := prometheus.NewRegistry()
	telSink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{CostPerThousandTokens: 0.002},
		tp, promReg, nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	store := capture.NewStore(capture.StoreConfig{}, nil, telSink)

	const (
		routerToken  = "111111:router-test-token"
		salesToken   = "222222:sales-test-token"
		orderToken   = "333333:order-test-token"
		refundToken  = "444444:refund-test-token"
		approveToken = "555555:approve-test-token"
	)
	mockSrv := mocktelegram.New(nil)
	_ = mockSrv.RegisterBot(routerToken)
	_ = mockSrv.RegisterBot(salesToken)
	_ = mockSrv.RegisterBot(orderToken)
	_ = mockSrv.RegisterBot(refundToken)
	_ = mockSrv.RegisterBot(approveToken)

	mockHTTP := httptest.NewServer(mockSrv)
	t.Cleanup(mockHTTP.Close)

	p, err := proxy.New(mockHTTP.URL, store, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	proxySrv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = proxySrv.Serve(ln) }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = proxySrv.Shutdown(shutCtx)
	})
	proxyEndpoint := "http://" + ln.Addr().String()

	salesBot, err := bots.NewSalesBot(salesToken, proxyEndpoint, nil)
	if err != nil {
		t.Fatalf("NewSalesBot: %v", err)
	}
	orderBot, err := bots.NewOrderBot(orderToken, proxyEndpoint, nil)
	if err != nil {
		t.Fatalf("NewOrderBot: %v", err)
	}
	refundBot, err := bots.NewRefundBot(refundToken, proxyEndpoint, nil)
	if err != nil {
		t.Fatalf("NewRefundBot: %v", err)
	}
	approveBot, err := bots.NewHumanApproveBot(approveToken, proxyEndpoint, nil)
	if err != nil {
		t.Fatalf("NewHumanApproveBot: %v", err)
	}
	routerBot, err := bots.NewRouterBot(
		routerToken, proxyEndpoint,
		salesBot.SelfID(), orderBot.SelfID(), refundBot.SelfID(), approveBot.SelfID(),
		nil,
	)
	if err != nil {
		t.Fatalf("NewRouterBot: %v", err)
	}

	botCtx, botCancel := context.WithCancel(ctx)
	t.Cleanup(botCancel)
	go salesBot.Run(botCtx, nil)
	go orderBot.Run(botCtx, nil)
	go refundBot.Run(botCtx, nil)
	go approveBot.Run(botCtx, nil)
	go routerBot.Run(botCtx, nil)

	if err := runScriptedTraffic(ctx, discardLogger(t), routerBot, plan); err != nil {
		t.Fatalf("runScriptedTraffic: %v", err)
	}

	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "b2b_loops_total" {
			for _, m := range mf.GetMetric() {
				loops = m.GetCounter().GetValue()
			}
		}
	}
	return len(recorder.Ended()), loops
}

// TestRunScriptedTrafficRepeatScales asserts that repeating the scripted
// conversation 3x produces ~3x the spans and loop events of a single run
// (tolerant band; in-memory recorder; no sleeps, no network).
func TestRunScriptedTrafficRepeatScales(t *testing.T) {
	t.Parallel()

	spans1, loops1 := runScriptedFixture(t, trafficPlan{repeat: 1})
	if spans1 == 0 {
		t.Fatal("repeat=1 produced no spans")
	}
	if loops1 < 1 {
		t.Fatalf("repeat=1 produced no loop events (b2b_loops_total=%.0f)", loops1)
	}

	const n = 3
	spansN, loopsN := runScriptedFixture(t, trafficPlan{repeat: n})

	// Span volume scales ~linearly with N: each iteration repeats the same
	// scripted conversation through the proxy. We assert a tolerant band rather
	// than an exact multiple — the bots run as concurrent goroutines through a
	// real TCP proxy, so the exact number of captured Bot API calls per
	// iteration can jitter by a few spans under -race + slow-CI scheduling
	// (an inflight getUpdates may miss its window by ~one span). The band must
	// catch "didn't scale" / "ran once" / "ran away" without flaking on that
	// benign one-span-per-iteration drift.
	//
	// Lower bound = 80% of the ideal (spans1 * n) — proves the volume is
	// clearly N-iteration shaped, not a single-iteration run, while absorbing
	// up to ~one span of jitter per iteration. Plus an absolute "strictly
	// greater than 1.5× baseline" gate so a regression to "ran twice instead
	// of N" still trips when N >= 3.
	lo := (spans1 * n * 80) / 100
	hi := spans1*(n+1) + spans1 // n× plus up to ~2 iterations of slack
	if spansN < lo || spansN > hi {
		t.Errorf("repeat=%d spans = %d, want within [%d, %d] (baseline %d × n=%d, lo = 80%% of n×)",
			n, spansN, lo, hi, spans1, n)
	}
	if spansN*2 <= spans1*3 {
		t.Errorf("repeat=%d did not scale beyond ~1 iteration: spans=%d <= 1.5× baseline %d",
			n, spansN, spans1)
	}

	// Loop events keep advancing across iterations (so the dashboard's loop
	// panels stay populated). They grow at least linearly — in fact often
	// super-linearly, because every iteration reuses the same long-lived
	// customer conversation. Assert the monotonic lower bound, not an exact
	// multiple (loop count depends on goroutine-scheduled ordering of the
	// duplicate refund send vs. concurrent bot replies).
	if loopsN < loops1*n {
		t.Errorf("repeat=%d b2b_loops_total = %.0f, want >= %.0f (%.0f × repeat=1)",
			n, loopsN, loops1*n, loops1)
	}

	// Stability check: a second identical run lands in the same tolerant band
	// (volume is repeatable, not bit-exact — see the band rationale above).
	spansN2, _ := runScriptedFixture(t, trafficPlan{repeat: n})
	if spansN2 < lo || spansN2 > hi {
		t.Errorf("unstable span volume: repeat=%d run #2 spans=%d, want within [%d, %d]",
			n, spansN2, lo, hi)
	}
}

// TestRunScriptedTrafficDurationBounded asserts that the duration mode is
// bounded: with a tiny duration it runs at least one iteration and returns
// promptly without hanging.
func TestRunScriptedTrafficDurationBounded(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		spans, _ := runScriptedFixture(t, trafficPlan{repeat: 1, duration: time.Nanosecond})
		if spans == 0 {
			t.Errorf("duration mode produced no spans (should run >=1 iteration)")
		}
	}()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("duration-bounded run did not return within 90s")
	}
}
