// Command b2bdbg is the Telegram bot-to-bot debugger proxy.
//
// It sits in front of the Telegram Bot API, captures bot ↔ bot conversations,
// and emits OpenTelemetry traces and Prometheus metrics.
//
// Usage:
//
//	b2bdbg [flags]
//	b2bdbg healthcheck [--listen <addr>]
//
// Subcommands:
//
//	healthcheck      perform an HTTP GET to /healthz and exit 0 on success
//
// Flags:
//
//	--config         path to YAML config file (default: config.yaml)
//	--listen         TCP listen address (overrides config)
//	--telegram-url   upstream Telegram base URL (overrides config)
//	--otel-endpoint  OTLP gRPC endpoint (overrides config)
//	--log-level      log level: debug|info|warn|error (overrides config)
//	--version        print version and exit
//	--help           print usage and exit
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/config"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/server"
	"github.com/b2bdbg/b2bdbg/internal/telemetry"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	// Dispatch subcommands before flag parsing so that `b2bdbg healthcheck`
	// works without touching the main flag set.
	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}
	os.Exit(run())
}

// runHealthcheck implements the `b2bdbg healthcheck` subcommand.
//
// It resolves the listen address from (in order):
//  1. the --listen flag passed to the subcommand
//  2. the B2BD_LISTEN_ADDR environment variable
//  3. the YAML config file at the default (or --config) path
//  4. the built-in default (:8080)
//
// It performs an HTTP GET to http://<resolved-addr>/healthz with a 2-second
// timeout and exits 0 only when the response status is 200 and the body
// contains "ok". All other outcomes produce a non-zero exit code.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("b2bdbg healthcheck", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: b2bdbg healthcheck [--listen <addr>] [--config <path>]\n")
		fmt.Fprintf(os.Stderr, "\nPerforms an HTTP GET to the local /healthz endpoint and exits 0 on success.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	flagListen := fs.String("listen", "", "TCP listen address (overrides config / env)")
	flagConfig := fs.String("config", "config.yaml", "path to YAML config file")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve address using the same precedence as the server: flag > env > yaml > default.
	cfgPath := *flagConfig
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgPath = ""
	}
	cfg, err := config.Load(cfgPath, config.Overrides{
		ListenAddr: *flagListen,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: config load: %v\n", err)
		return 1
	}

	addr := listenAddrToHostPort(cfg.ListenAddr)
	url := "http://" + addr + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: build request: %v\n", err)
		return 1
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: GET %s: %v\n", url, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: read body: %v\n", err)
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: status %d\n", resp.StatusCode)
		return 1
	}

	if !strings.Contains(string(body), "ok") {
		fmt.Fprintf(os.Stderr, "b2bdbg healthcheck: unexpected body: %q\n", string(body))
		return 1
	}

	return 0
}

// listenAddrToHostPort converts a listen address such as ":8080" (no host) to
// "127.0.0.1:8080" so that an HTTP client can use it. Addresses that already
// include a host are returned unchanged.
func listenAddrToHostPort(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not a valid host:port — return as-is and let the HTTP client fail.
		return addr
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// run is the real entry point, returning an exit code so main can call os.Exit
// after deferred cleanup has run.
func run() int {
	// --- flags ---------------------------------------------------------------
	fs := flag.NewFlagSet("b2bdbg", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "b2bdbg — Telegram bot-to-bot debugger proxy\n\n")
		fmt.Fprintf(os.Stderr, "Usage: b2bdbg [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment variables: B2BD_LISTEN_ADDR, B2BD_TELEGRAM_BASE_URL,\n")
		fmt.Fprintf(os.Stderr, "  B2BD_OTEL_ENDPOINT, B2BD_LOG_LEVEL, B2BD_SHUTDOWN_TIMEOUT,\n")
		fmt.Fprintf(os.Stderr, "  B2BD_COST_PER_K_TOKENS, B2BD_BODY_CAP_BYTES\n")
	}

	var (
		flagConfig      = fs.String("config", "config.yaml", "path to YAML config file")
		flagListen      = fs.String("listen", "", "TCP listen address (overrides config)")
		flagTelegramURL = fs.String("telegram-url", "", "upstream Telegram base URL (overrides config)")
		flagOTel        = fs.String("otel-endpoint", "", "OTLP gRPC endpoint (overrides config)")
		flagLogLevel    = fs.String("log-level", "", "log level: debug|info|warn|error (overrides config)")
		flagVersion     = fs.Bool("version", false, "print version and exit")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag.ContinueOnError already printed the error.
		return 2
	}

	if *flagVersion {
		fmt.Printf("b2bdbg %s\n", version)
		return 0
	}

	// --- config --------------------------------------------------------------
	// If the user-supplied config path does not exist, fall back silently so
	// that the binary works with pure env vars / flags out of the box.
	cfgPath := *flagConfig
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgPath = ""
	}

	cfg, err := config.Load(cfgPath, config.Overrides{
		ListenAddr:      *flagListen,
		TelegramBaseURL: *flagTelegramURL,
		OTelEndpoint:    *flagOTel,
		LogLevel:        *flagLogLevel,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: config load: %v\n", err)
		return 1
	}
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: config invalid: %v\n", err)
		return 1
	}

	// --- logging -------------------------------------------------------------
	logger := buildLogger(cfg.LogLevel)
	logger.Info("b2bdbg starting", slog.String("version", version))

	// --- signal-aware context ------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- telemetry -----------------------------------------------------------
	tp, err := telemetry.NewTracerProvider(ctx, cfg, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: tracer provider: %v\n", err)
		return 1
	}

	promReg := prometheus.NewRegistry()
	// Register the default Go runtime collectors into our isolated registry.
	promReg.MustRegister(collectors.NewGoCollector())
	promReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	telSink, err := telemetry.NewSink(cfg, telemetry.SinkConfig{
		CostPerThousandTokens: cfg.CostPerKTokens,
	}, tp, promReg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: telemetry sink: %v\n", err)
		return 1
	}

	// --- capture store (implements proxy.Sink) --------------------------------
	// BotRegistry tracks bot user ID ↔ token hash mappings so that
	// telegram.bot.to can be set when a sendMessage targets a known bot.
	botReg := capture.NewBotRegistry(0) // 0 = use default cap (1 000)
	store := capture.NewStoreWithRegistry(capture.StoreConfig{}, logger, botReg, telSink)

	// --- proxy ---------------------------------------------------------------
	p, err := proxy.NewWithOptions(cfg.TelegramBaseURL, store, logger, cfg.BodyCapBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: proxy init: %v\n", err)
		return 1
	}
	logger.Info("proxy configured",
		slog.String("upstream", cfg.TelegramBaseURL),
		slog.Int64("body_cap_bytes", cfg.BodyCapBytes))

	// --- webhook ingress routes ---------------------------------------------
	// Build one server.WebhookRoute per configured webhook_routes entry. The
	// raw token is hashed inside the proxy handler and never retained here.
	// No-op when no routes are configured: poll-only behaviour is unchanged.
	webhookRoutes, err := buildWebhookRoutes(cfg, p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "b2bdbg: webhook routes: %v\n", err)
		return 1
	}
	if len(webhookRoutes) > 0 {
		logger.Info("webhook ingress enabled", slog.Int("routes", len(webhookRoutes)))
	}

	// --- server --------------------------------------------------------------
	metricsHandler := promhttp.HandlerFor(promReg, promhttp.HandlerOpts{})
	srvOpts := server.Options{WebhookRoutes: webhookRoutes}
	if cfg.DebugEndpoints {
		// Opt-in, local-only introspection so operators can see why
		// telegram.bot.to is empty. Exposes only id↔hash + counts (no tokens).
		srvOpts.DebugRegistry = botReg
		logger.Warn("debug endpoints enabled — GET /debug/registry exposes bot id↔hash mappings; bind to a trusted/loopback interface")
	}
	srv := server.NewWithOptions(cfg.ListenAddr, p, metricsHandler, logger, srvOpts)

	// Run the server in a goroutine so we can wait for a signal.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Block until a signal fires or the server dies on its own.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			logger.Error("server error", slog.Any("error", err))
			return 1
		}
		return 0
	}

	// Graceful shutdown with the configured timeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.Any("error", err))
		return 1
	}

	// Drain the server goroutine.
	if err := <-errCh; err != nil {
		logger.Error("server exit error", slog.Any("error", err))
		return 1
	}

	// Flush and stop the tracer provider before exiting.
	if err := telemetry.ShutdownTracerProvider(shutdownCtx, tp, logger); err != nil {
		logger.Error("tracer shutdown", slog.Any("error", err))
	}

	logger.Info("b2bdbg stopped cleanly")
	return 0
}

// buildWebhookRoutes converts the configured webhook_routes into
// server.WebhookRoute values, building one proxy webhook handler per route.
//
// The raw bot token is passed to the proxy only long enough to derive its
// hash; it is never logged or retained by this function. An empty config
// produces a nil slice so poll-only deployments are entirely unaffected.
func buildWebhookRoutes(cfg config.Config, p *proxy.Proxy) ([]server.WebhookRoute, error) {
	if len(cfg.WebhookRoutes) == 0 {
		return nil, nil
	}
	routes := make([]server.WebhookRoute, 0, len(cfg.WebhookRoutes))
	for _, r := range cfg.WebhookRoutes {
		h, err := p.WebhookHandlerForRoute(r.Target, r.Token, r.SecretToken)
		if err != nil {
			return nil, fmt.Errorf("webhook route %q: %w", r.Label, err)
		}
		routes = append(routes, server.WebhookRoute{
			Label:   r.Label,
			Handler: h,
		})
	}
	return routes, nil
}

// buildLogger constructs a structured slog.Logger at the requested level,
// writing JSON to stdout.
func buildLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
