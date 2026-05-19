// Package server provides the combined HTTP server for b2bdbg.
//
// It exposes:
//   - /healthz — liveness probe
//   - /metrics — Prometheus metrics (provided by the caller via metricsHandler)
//   - /webhook/<label> — inbound Telegram webhook ingress (one per configured route)
//   - /{bot<TOKEN>}/{method} — transparent reverse proxy to the Telegram Bot API
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// WebhookRoute binds a URL-safe label to its pre-built ingress handler.
//
// The label maps to the path /webhook/<label>; Handler is the proxy webhook
// handler for that route (already wired with the route's token hash, target
// URL and optional secret token). The raw bot token never appears here.
type WebhookRoute struct {
	// Label is the URL-safe route name. The handler is mounted at
	// /webhook/<Label>.
	Label string

	// Handler serves inbound Telegram webhook POSTs for this route.
	Handler http.Handler
}

// RegistrySnapshotProvider supplies the bot id↔token-hash mappings for the
// local-only GET /debug/registry endpoint. *capture.BotRegistry satisfies it.
// The returned slice must never contain raw tokens.
type RegistrySnapshotProvider interface {
	Snapshot() []capture.RegistryEntry
}

// Options carries optional, opt-in server features. The zero value is safe and
// reproduces the exact poll-only behaviour of [New].
type Options struct {
	// WebhookRoutes registers one inbound Telegram webhook endpoint per entry
	// at /webhook/<label>. Nil/empty → poll-only behaviour.
	WebhookRoutes []WebhookRoute

	// DebugRegistry, when non-nil, enables GET /debug/registry which serves the
	// id↔hash mappings as JSON. It MUST only be wired when the operator has
	// explicitly enabled debug endpoints (config.DebugEndpoints); when nil the
	// route is not registered and any request to it returns 404.
	DebugRegistry RegistrySnapshotProvider
}

// Server is the combined admin/health/proxy HTTP server.
// Construct via New; do not copy after first use.
type Server struct {
	addr   string
	logger *slog.Logger
	mux    *http.ServeMux
	srv    *http.Server
}

// New constructs a Server bound to addr, using logger for structured output.
// p is the proxy handler that will serve all /{bot<TOKEN>}/{method} paths.
// metricsHandler is the handler for the /metrics endpoint; when nil a
// placeholder response is used.
//
// webhookRoutes registers one inbound Telegram webhook endpoint per entry at
// /webhook/<label>; pass nil or an empty slice for poll-only deployments
// (behaviour is then identical to before webhook support existed).
func New(addr string, p *proxy.Proxy, metricsHandler http.Handler, logger *slog.Logger, webhookRoutes ...WebhookRoute) *Server {
	return NewWithOptions(addr, p, metricsHandler, logger, Options{WebhookRoutes: webhookRoutes})
}

// NewWithOptions is like [New] but accepts an [Options] value to enable opt-in
// features (webhook routes, the local-only debug registry endpoint). The zero
// Options value reproduces the exact poll-only behaviour of [New].
func NewWithOptions(addr string, p *proxy.Proxy, metricsHandler http.Handler, logger *slog.Logger, opts Options) *Server {
	mux := http.NewServeMux()

	s := &Server{
		addr:   addr,
		logger: logger,
		mux:    mux,
	}

	s.registerRoutes(p, metricsHandler, opts)

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout must be long enough for Telegram long-poll connections
		// (Telegram timeout param max is 50 s; we give generous headroom).
		IdleTimeout: 120 * time.Second,
	}

	return s
}

// registerRoutes wires the built-in endpoints, webhook ingress routes, and the
// proxy onto the mux.
//
// Path precedence is handled by http.ServeMux longest-pattern matching:
//   - /healthz and /metrics are exact patterns.
//   - /webhook/<label> is an exact pattern per configured route; it never
//     collides with /healthz, /metrics or / and shadows the catch-all proxy.
//   - /webhook/ is a subtree guard so an unknown label returns 404 instead of
//     falling through to the Bot-API proxy.
//   - /debug/registry is registered ONLY when opts.DebugRegistry is non-nil
//     (operator opted in via config); otherwise it is never registered and
//     falls through to the proxy catch-all, which 404s a non-bot path.
//   - / handles every other path (/{bot<TOKEN>}/{method}) via the proxy,
//     unchanged from poll-only behaviour.
func (s *Server) registerRoutes(p *proxy.Proxy, metricsHandler http.Handler, opts Options) {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	if metricsHandler != nil {
		s.mux.Handle("/metrics", metricsHandler)
	} else {
		s.mux.HandleFunc("/metrics", s.handleMetricsPlaceholder)
	}

	if len(opts.WebhookRoutes) > 0 {
		// Subtree guard: any /webhook/... path with no exact route match
		// returns 404 rather than leaking to the Bot-API proxy.
		s.mux.HandleFunc("/webhook/", s.handleWebhookNotFound)
		for _, wr := range opts.WebhookRoutes {
			pattern := "/webhook/" + wr.Label
			s.mux.Handle(pattern, wr.Handler)
			s.logger.Info("webhook route registered",
				slog.String("label", wr.Label),
				slog.String("path", pattern))
		}
	}

	// Local-only debug introspection — opt-in only. When DebugRegistry is nil
	// the route is never registered (zero overhead) and the path 404s.
	if opts.DebugRegistry != nil {
		reg := opts.DebugRegistry
		s.mux.HandleFunc("/debug/registry", func(w http.ResponseWriter, r *http.Request) {
			s.handleDebugRegistry(w, r, reg)
		})
		s.logger.Info("debug endpoint registered (opt-in)",
			slog.String("path", "/debug/registry"))
	}

	// All other paths (/{bot<TOKEN>}/{method}) are handled by the proxy.
	s.mux.Handle("/", p)
}

// handleDebugRegistry serves the bot id↔token-hash mappings as JSON. It only
// ever exposes the SHA-256 hash prefixes already written to spans/logs plus a
// count — never a raw token. It is reachable only when the operator explicitly
// enabled debug endpoints (config.DebugEndpoints); otherwise it is never
// registered. Only GET is accepted.
func (s *Server) handleDebugRegistry(w http.ResponseWriter, r *http.Request, reg RegistrySnapshotProvider) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries := reg.Snapshot()
	body := struct {
		Count   int                     `json:"count"`
		Entries []capture.RegistryEntry `json:"entries"`
	}{
		Count:   len(entries),
		Entries: entries,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Warn("debug registry: encode response", slog.Any("error", err))
	}
}

// handleWebhookNotFound responds 404 for /webhook/<unknown-label> requests so
// that an unconfigured label is rejected rather than proxied upstream.
func (s *Server) handleWebhookNotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintln(w, "unknown webhook route")
}

// Handler returns the server's root http.Handler (the configured mux).
// It is primarily useful for in-process testing with httptest, where binding
// a real socket is unnecessary.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Start begins listening and serving in the foreground.
// It returns when the server has stopped (after Shutdown or a fatal error).
// A nil return value means the server was cleanly shut down.
// The ctx parameter is reserved for future use (e.g. pre-start checks).
func (s *Server) Start(_ context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.addr, err)
	}

	s.logger.Info("admin server listening", slog.String("addr", ln.Addr().String()))

	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: serve: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests, honouring the deadline in ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("admin server shutting down")
	if err := s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	return nil
}

// handleHealthz responds 200 OK with a plain-text body.
// It is used as a liveness probe by container orchestrators.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// handleMetricsPlaceholder serves /metrics when no Prometheus handler was
// supplied (telemetry disabled). When telemetry is enabled, New receives a
// promhttp handler and this fallback is not registered.
func (s *Server) handleMetricsPlaceholder(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "# metrics disabled (no telemetry sink configured)")
}
