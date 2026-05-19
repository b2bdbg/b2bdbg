// Package main (test file) covers the external-proxy mode of the support-team
// demo, used by `docker compose --profile demo`.
//
// In that mode the demo must NOT create its own proxy or telemetry: it serves
// the in-process mock Telegram on a real TCP listener and routes every bot's
// Bot API call through an externally-running b2bdbg proxy. This test stands up a
// real external b2bdbg proxy (on a real localhost listener, with an in-memory
// span recorder), points the demo at it via B2BD_EXAMPLE_PROXY_BASE_URL, and
// asserts that the spans land in the EXTERNAL proxy — proving the wiring the
// compose demo depends on.
package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// TestExternalProxyMode asserts that runDemoExternalProxy routes all bot↔bot
// traffic through the external proxy passed via B2BD_EXAMPLE_PROXY_BASE_URL and
// creates no in-process proxy/telemetry of its own.
func TestExternalProxyMode(t *testing.T) {
	t.Parallel()

	// Skipped under -short — see TestSupportTeamIntegration for the rationale
	// (example/demo path, sloppy reply-matching flakes under -race on CI).
	if testing.Short() {
		t.Skip("demo integration test; run locally without -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- An EXTERNAL b2bdbg proxy with a synchronous span recorder ------------
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	telSink, err := telemetry.NewSink(
		config.Config{},
		telemetry.SinkConfig{},
		tp,
		prometheus.NewRegistry(),
		nil,
	)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	// The external proxy's upstream is the mock the demo will serve. The demo
	// chooses a free port for the mock; we pre-bind that here and hand the
	// resolved URL to the external proxy as its B2BD_TELEGRAM_BASE_URL
	// equivalent, exactly as docker-compose wires b2bdbg → support-team-demo.
	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve mock port: %v", err)
	}
	mockAddr := mockLn.Addr().String()
	// Close it immediately; the demo re-binds the same address. A brief race
	// window is acceptable in a single-process test on loopback.
	_ = mockLn.Close()
	mockUpstreamURL := "http://" + mockAddr

	store := capture.NewStore(capture.StoreConfig{}, nil, telSink)
	p, err := proxy.New(mockUpstreamURL, store, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("external proxy listen: %v", err)
	}
	extSrv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = extSrv.Serve(extLn) }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = extSrv.Shutdown(shutCtx)
	})
	externalProxyBaseURL := "http://" + extLn.Addr().String()

	// --- Run the demo in external-proxy mode --------------------------------
	if err := runDemoExternalProxy(ctx, discardLogger(t), externalProxyBaseURL, mockAddr); err != nil {
		t.Fatalf("runDemoExternalProxy: %v", err)
	}

	// --- The EXTERNAL proxy must have captured the conversation -------------
	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans in the external proxy — bots did not route through it")
	}
	fromBots := distinctAttrValues(spans, "telegram.bot.from")
	if len(fromBots) < 3 {
		t.Errorf("want ≥3 distinct telegram.bot.from values via external proxy, got %d: %v",
			len(fromBots), fromBots)
	}
}

// TestRunSelectsExternalProxyMode asserts that run() dispatches to
// external-proxy mode when B2BD_EXAMPLE_PROXY_BASE_URL is set: the demo serves
// its mock on B2BD_EXAMPLE_MOCK_ADDR and routes through the external proxy,
// exiting 0 without creating an in-process proxy.
func TestRunSelectsExternalProxyMode(t *testing.T) {
	// Skipped under -short — see TestSupportTeamIntegration for the rationale.
	if testing.Short() {
		t.Skip("demo integration test; run locally without -short")
	}
	// Not parallel: mutates process environment.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	telSink, err := telemetry.NewSink(config.Config{}, telemetry.SinkConfig{}, tp, prometheus.NewRegistry(), nil)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve mock port: %v", err)
	}
	mockAddr := mockLn.Addr().String()
	mockUpstreamURL := "http://" + mockAddr
	_ = mockLn.Close()

	store := capture.NewStore(capture.StoreConfig{}, nil, telSink)
	p, err := proxy.New(mockUpstreamURL, store, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("external proxy listen: %v", err)
	}
	extSrv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = extSrv.Serve(extLn) }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = extSrv.Shutdown(shutCtx)
	})

	t.Setenv(envProxyBaseURL, "http://"+extLn.Addr().String())
	t.Setenv(envMockAddr, mockAddr)

	if code := run(); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}

	if len(recorder.Ended()) == 0 {
		t.Fatal("run() did not route traffic through the external proxy")
	}
}

// discardLogger returns a logger that drops output, keeping test logs quiet
// while still exercising the real code path.
func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
