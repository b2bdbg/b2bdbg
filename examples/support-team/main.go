// Command support-team runs the support-team demo.
//
// It has two modes, selected by environment variables:
//
//   - In-process mode (default, used by `make example`): starts an in-process
//     mock Telegram server, an in-process b2bdbg proxy (using the project's
//     internal packages), and five deterministic bots, then injects two
//     scripted customer tasks. The proxy exports OpenTelemetry spans to stdout
//     (human-readable JSON) unless B2BD_OTEL_ENDPOINT is set, in which case it
//     exports OTLP/gRPC. Prometheus metrics live in a private in-process
//     registry. Nothing leaves the process; no Docker is involved.
//
//   - External-proxy mode (used by `docker compose --profile demo`): set
//     B2BD_EXAMPLE_PROXY_BASE_URL to the base URL of an already-running b2bdbg
//     proxy (e.g. http://b2bdbg:8080). This mode does NOT start an in-process
//     proxy or telemetry pipeline. Instead it serves the in-process mock
//     Telegram backend on a real TCP listener (address from
//     B2BD_EXAMPLE_MOCK_ADDR, default :8081) and points all bots at the
//     external proxy. The external b2bdbg is configured (via
//     B2BD_TELEGRAM_BASE_URL) to use this mock as its upstream, so every
//     bot↔bot Bot API call flows through the composed b2bdbg — producing spans
//     in the composed Jaeger and metrics scraped by the composed Prometheus —
//     while still requiring no real Telegram tokens and no internet access.
//
// Both modes run the scripted conversation once by default. Three opt-in
// environment variables (read once in run) generate sustained traffic — useful
// for keeping the Grafana dashboard's rate panels populated for screenshots:
//
//   - B2BD_EXAMPLE_REPEAT   integer N (default 1): replay the scripted
//     conversation N times against the same bots/proxy.
//   - B2BD_EXAMPLE_INTERVAL Go duration (default 0): delay between repeats.
//   - B2BD_EXAMPLE_DURATION Go duration (default unset): loop the scripted
//     conversation until this wall-clock time elapses; takes precedence over
//     B2BD_EXAMPLE_REPEAT.
//
// With none of these set the demo's behaviour is byte-identical to before:
// exactly one scripted run, then exit.
//
// Usage:
//
//	make example
//	  — or —
//	go run ./examples/support-team/
//
// No real Telegram tokens or internet connection are required in either mode.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/b2bdbg/b2bdbg/examples/support-team/bots"
	"github.com/b2bdbg/b2bdbg/examples/support-team/mocktelegram"
	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
)

// Environment variables understood by the demo. They are read once, in run,
// and threaded through explicitly — no globals are mutated.
const (
	// envProxyBaseURL, when set, switches the demo to external-proxy mode:
	// the bots route their Bot API calls through this base URL (e.g.
	// "http://b2bdbg:8080") instead of an in-process proxy, and no in-process
	// proxy or telemetry pipeline is created.
	envProxyBaseURL = "B2BD_EXAMPLE_PROXY_BASE_URL"

	// envMockAddr is the TCP listen address for the mock Telegram backend in
	// external-proxy mode (default ":8081"). The external b2bdbg must be pointed
	// at this address via B2BD_TELEGRAM_BASE_URL.
	envMockAddr = "B2BD_EXAMPLE_MOCK_ADDR"

	// envOTelEndpoint selects the OTLP/gRPC exporter for the in-process proxy
	// in in-process mode. Ignored in external-proxy mode (the external b2bdbg
	// owns telemetry there).
	envOTelEndpoint = "B2BD_OTEL_ENDPOINT"

	// defaultMockAddr is the mock listen address used when envMockAddr is
	// unset in external-proxy mode.
	defaultMockAddr = ":8081"

	// envRepeat sets how many times the scripted multi-bot conversation is
	// replayed (positive integer). Default 1 = the original one-shot behaviour;
	// the bots, proxy and telemetry pipeline are created once and reused, so
	// each repeat keeps the Prometheus counters and OTel traces advancing.
	// This is the opt-in knob for generating sustained traffic for Grafana
	// screenshots. Ignored when envDuration is set.
	envRepeat = "B2BD_EXAMPLE_REPEAT"

	// envInterval is an optional delay (Go duration, e.g. "2s") inserted
	// between scripted-conversation repeats. Default 0 = no delay. The wait is
	// context-cancellable.
	envInterval = "B2BD_EXAMPLE_INTERVAL"

	// envDuration, when set to a positive Go duration (e.g. "10m"), runs the
	// scripted conversation in a loop until that wall-clock time has elapsed,
	// then exits cleanly. It takes precedence over envRepeat. Default unset =
	// honour envRepeat (which itself defaults to 1).
	envDuration = "B2BD_EXAMPLE_DURATION"

	// defaultRepeat preserves the original one-shot behaviour when envRepeat is
	// unset or invalid.
	defaultRepeat = 1
)

// trafficPlan describes how many times the scripted conversation is replayed
// and how the demo paces / bounds those repeats. The zero value
// (repeat=0, treated as defaultRepeat) reproduces the original one-shot run.
type trafficPlan struct {
	// repeat is the number of scripted-conversation iterations. Always >= 1.
	repeat int
	// interval is the delay inserted between iterations (0 = none).
	interval time.Duration
	// duration, when > 0, overrides repeat: iterate until this elapses.
	duration time.Duration
}

// trafficPlanFromEnv reads the opt-in sustained-traffic knobs once. Invalid or
// unset values fall back to the original one-shot behaviour (repeat=1), so the
// default (no env) is byte-identical to before this knob existed.
func trafficPlanFromEnv(logger *slog.Logger) trafficPlan {
	plan := trafficPlan{repeat: defaultRepeat}

	if v := os.Getenv(envRepeat); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			plan.repeat = n
		} else {
			logger.Warn("ignoring invalid "+envRepeat+" (want integer >= 1)",
				slog.String("value", v))
		}
	}
	if v := os.Getenv(envInterval); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			plan.interval = d
		} else {
			logger.Warn("ignoring invalid "+envInterval+" (want Go duration >= 0)",
				slog.String("value", v))
		}
	}
	if v := os.Getenv(envDuration); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			plan.duration = d
		} else {
			logger.Warn("ignoring invalid "+envDuration+" (want positive Go duration)",
				slog.String("value", v))
		}
	}
	return plan
}

// botTokens holds the five deterministic, fake bot tokens used by the demo.
// They are arbitrary strings: the mock assigns each a stable identity.
type botTokens struct {
	router  string
	sales   string
	order   string
	refund  string
	approve string
}

func defaultBotTokens() botTokens {
	return botTokens{
		router:  "111111:router-fake-token",
		sales:   "222222:sales-fake-token",
		order:   "333333:order-fake-token",
		refund:  "444444:refund-fake-token",
		approve: "555555:approve-fake-token",
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	proxyBaseURL := os.Getenv(envProxyBaseURL)
	var err error
	if proxyBaseURL != "" {
		mockAddr := os.Getenv(envMockAddr)
		if mockAddr == "" {
			mockAddr = defaultMockAddr
		}
		err = runDemoExternalProxy(ctx, logger, proxyBaseURL, mockAddr)
	} else {
		err = runDemoInProcess(ctx, logger)
	}
	if err != nil {
		logger.Error("demo failed", slog.Any("error", err))
		return 1
	}
	logger.Info("demo completed successfully")
	return 0
}

// runDemoInProcess wires the mock, an in-process b2bdbg proxy, telemetry and the
// five bots together and runs the scripted conversation. This is the default
// `make example` path: fully offline, spans to stdout (or OTLP if
// B2BD_OTEL_ENDPOINT is set on the in-process proxy).
func runDemoInProcess(ctx context.Context, logger *slog.Logger) error {
	tokens := defaultBotTokens()

	// -----------------------------------------------------------------------
	// 1. Start mock Telegram (random port via httptest)
	// -----------------------------------------------------------------------
	mockSrv := mocktelegram.New(logger)
	registerBots(mockSrv, tokens)

	mockHTTP := httptest.NewServer(mockSrv)
	defer mockHTTP.Close()
	logger.Info("mock telegram started", slog.String("url", mockHTTP.URL))

	// -----------------------------------------------------------------------
	// 2. Start b2bdbg proxy pointed at the mock
	// -----------------------------------------------------------------------
	otelEndpoint := os.Getenv(envOTelEndpoint) // empty → stdout exporter

	cfg := config.Config{
		ListenAddr:      "localhost:0", // random port
		TelegramBaseURL: mockHTTP.URL,
		OTelEndpoint:    otelEndpoint,
		LogLevel:        "info",
		ShutdownTimeout: 5 * time.Second,
	}

	// The example has no build-time version; pass "" and let buildResource
	// record "dev" in service.version.
	tp, err := telemetry.NewTracerProvider(ctx, cfg, "")
	if err != nil {
		return fmt.Errorf("tracer provider: %w", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = telemetry.ShutdownTracerProvider(shutCtx, tp, logger)
	}()

	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector())

	telSink, err := telemetry.NewSink(cfg, telemetry.SinkConfig{}, tp, promReg, logger)
	if err != nil {
		return fmt.Errorf("telemetry sink: %w", err)
	}

	// Wire a BotRegistry so the demo's getMe responses populate id↔hash and
	// the parser can resolve telegram.bot.to on sendMessage spans. Without
	// this, the in-process zero-setup demo would always log
	// b2b.bot.to.resolution=unknown_getme_not_seen for bot→bot sends — the
	// composed/external-proxy paths already do this; bring the offline demo
	// in line.
	reg := capture.NewBotRegistry(0)
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, logger, reg, telSink)

	p, err := proxy.New(cfg.TelegramBaseURL, store, logger)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	// Listen on a random port.
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	proxyAddr := ln.Addr().String()
	proxySrv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() { _ = proxySrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxySrv.Shutdown(shutCtx)
	}()

	proxyEndpoint := "http://" + proxyAddr
	logger.Info("b2bdbg proxy started", slog.String("addr", proxyAddr))

	return runConversation(ctx, logger, tokens, proxyEndpoint)
}

// runDemoExternalProxy serves the mock Telegram backend on a real TCP listener
// and routes the bots through an already-running, external b2bdbg proxy. It
// creates no in-process proxy and no telemetry pipeline: the external b2bdbg
// captures the traffic and exports it (in the compose demo, to Jaeger and
// Prometheus). The mock still runs in-process, so no real Telegram or internet
// is required.
//
// The external b2bdbg must be configured with B2BD_TELEGRAM_BASE_URL pointing at
// http://<this-container>:<mockAddr-port> so its upstream is this mock.
func runDemoExternalProxy(
	ctx context.Context,
	logger *slog.Logger,
	proxyBaseURL string,
	mockAddr string,
) error {
	tokens := defaultBotTokens()

	// -----------------------------------------------------------------------
	// 1. Start mock Telegram on a real, addressable TCP listener
	// -----------------------------------------------------------------------
	mockSrv := mocktelegram.New(logger)
	registerBots(mockSrv, tokens)

	ln, err := net.Listen("tcp", mockAddr)
	if err != nil {
		return fmt.Errorf("mock listen %q: %w", mockAddr, err)
	}
	mockHTTP := &http.Server{
		Handler:           mockSrv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() { _ = mockHTTP.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mockHTTP.Shutdown(shutCtx)
	}()
	logger.Info(
		"mock telegram started (external-proxy mode)",
		slog.String("addr", ln.Addr().String()),
	)
	logger.Info(
		"bots will route through external b2bdbg",
		slog.String("proxy_base_url", proxyBaseURL),
	)

	return runConversation(ctx, logger, tokens, proxyBaseURL)
}

// registerBots pre-registers the five fake tokens so the mock assigns stable
// bot IDs before the first getMe.
func registerBots(mockSrv *mocktelegram.Server, t botTokens) {
	_ = mockSrv.RegisterBot(t.router)
	_ = mockSrv.RegisterBot(t.sales)
	_ = mockSrv.RegisterBot(t.order)
	_ = mockSrv.RegisterBot(t.refund)
	_ = mockSrv.RegisterBot(t.approve)
}

// runConversation constructs the five bots pointed at proxyEndpoint, runs them,
// and injects the two scripted customer tasks. proxyEndpoint is a base URL such
// as "http://host:port" with no trailing slash; the bots append /bot<token>/...
func runConversation(
	ctx context.Context,
	logger *slog.Logger,
	tokens botTokens,
	proxyEndpoint string,
) error {
	salesBot, err := bots.NewSalesBot(tokens.sales, proxyEndpoint, logger)
	if err != nil {
		return fmt.Errorf("sales bot: %w", err)
	}
	orderBot, err := bots.NewOrderBot(tokens.order, proxyEndpoint, logger)
	if err != nil {
		return fmt.Errorf("order bot: %w", err)
	}
	refundBot, err := bots.NewRefundBot(tokens.refund, proxyEndpoint, logger)
	if err != nil {
		return fmt.Errorf("refund bot: %w", err)
	}
	approveBot, err := bots.NewHumanApproveBot(tokens.approve, proxyEndpoint, logger)
	if err != nil {
		return fmt.Errorf("approve bot: %w", err)
	}

	routerBot, err := bots.NewRouterBot(
		tokens.router,
		proxyEndpoint,
		salesBot.SelfID(),
		orderBot.SelfID(),
		refundBot.SelfID(),
		approveBot.SelfID(),
		logger,
	)
	if err != nil {
		return fmt.Errorf("router bot: %w", err)
	}

	// Run specialist bots in background goroutines.
	botCtx, botCancel := context.WithCancel(ctx)
	defer botCancel()

	go salesBot.Run(botCtx, nil)
	go orderBot.Run(botCtx, nil)
	go refundBot.Run(botCtx, nil)
	go approveBot.Run(botCtx, nil)
	go routerBot.Run(botCtx, nil)

	plan := trafficPlanFromEnv(logger)
	return runScriptedTraffic(ctx, logger, routerBot, plan)
}

// scriptedTasks are the two customer tasks injected per iteration. The first is
// a refund > $100, which deterministically triggers the human-approve hop and
// the intentional duplicate-send loop (b2b.loop.depth > 0 / b2b_loops_total).
var scriptedTasks = []string{
	"I need a refund for order #99, $150",
	"What are your order tracking options?",
}

// runScriptedTraffic replays the scripted customer tasks against an
// already-running routerBot according to plan.
//
// The default plan (repeat == 1, duration == 0, interval == 0) injects the two
// tasks exactly once with a 15s task deadline — byte-identical to the original
// one-shot behaviour, so `make example`, the integration tests and
// compose-smoke timing are unchanged.
//
// When plan.duration > 0 the conversation is replayed in a loop until that
// wall-clock time elapses; otherwise it is replayed plan.repeat times. Either
// way the loop is bounded, deterministic and cancellable: it stops early if ctx
// is done, and the per-iteration task deadline is independent of ctx's deadline.
func runScriptedTraffic(
	ctx context.Context,
	logger *slog.Logger,
	routerBot *bots.RouterBot,
	plan trafficPlan,
) error {
	// Fictional customer chat ID (just an integer; the customer is not a bot in
	// this demo so there is no bot to receive the reply — that is fine, the
	// Router sends and the proxy captures the span).
	const customerChatID = int64(9001)

	var deadline time.Time
	if plan.duration > 0 {
		deadline = time.Now().Add(plan.duration)
		logger.Info("sustained-traffic mode: looping scripted conversation until duration elapses",
			slog.Duration("duration", plan.duration),
			slog.Duration("interval", plan.interval))
	} else if plan.repeat > 1 {
		logger.Info("sustained-traffic mode: repeating scripted conversation",
			slog.Int("repeat", plan.repeat),
			slog.Duration("interval", plan.interval))
	}

	for iter := 1; ; iter++ {
		if ctx.Err() != nil {
			logger.Info("context cancelled — stopping scripted traffic",
				slog.Int("completed_iterations", iter-1))
			return nil
		}

		// Per-iteration task deadline, derived from ctx so cancellation still
		// propagates, but with its own 15s bound (matching the original).
		taskCtx, taskCancel := context.WithTimeout(ctx, 15*time.Second)
		for i, task := range scriptedTasks {
			logger.Info("injecting task",
				slog.Int("iteration", iter),
				slog.Int("n", i+1),
				slog.String("task", task))
			reply, err := routerBot.HandleTask(taskCtx, customerChatID, task)
			if err != nil {
				logger.Warn("task failed",
					slog.Int("iteration", iter), slog.Int("n", i+1), slog.Any("error", err))
			} else {
				logger.Info("task complete",
					slog.Int("iteration", iter), slog.Int("n", i+1), slog.String("reply", reply))
			}
		}
		taskCancel()

		// Decide whether to continue.
		if plan.duration > 0 {
			if time.Now().After(deadline) {
				break
			}
		} else if iter >= plan.repeat {
			break
		}

		if plan.interval > 0 {
			select {
			case <-ctx.Done():
				logger.Info("context cancelled during interval — stopping scripted traffic",
					slog.Int("completed_iterations", iter))
				return nil
			case <-time.After(plan.interval):
			}
		}
	}

	logger.Info("all tasks complete — spans flushed to exporter")
	return nil
}
