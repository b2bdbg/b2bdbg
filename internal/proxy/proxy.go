// Package proxy implements the transparent reverse proxy to the Telegram Bot API.
//
// It intercepts all bot ↔ Telegram traffic, taps request and response bodies
// for capture, and supports two ingestion modes:
//
// # Bot ingestion modes
//
// Long-polling: the bot calls getUpdates through this proxy as its API base URL.
// The proxy forwards the request to the configured upstream with extended timeouts
// so legitimate long-poll connections are not prematurely cancelled.
//
// Webhook ingress: Telegram POSTs updates to /webhook/<label> on this proxy.
// Each label is pre-configured with a bot token (hashed on arrival) and a target
// URL. The proxy validates the optional X-Telegram-Bot-Api-Secret-Token header,
// captures the exchange with the correct TokenHash, and forwards the body to the
// configured target. All capture/telemetry pipeline steps work identically to
// long-polling ingress.
//
// # Token security
//
// Raw bot tokens are never stored, logged, or returned. The token extracted from
// the request path (long-poll) or supplied via config (webhook) is immediately
// hashed (SHA-256, truncated to 16 hex characters) and only the hash appears in
// [Exchange] and log output.
package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/b2bdbg/b2bdbg/pkg/botapi"
)

// DefaultBodyCapBytes is the default maximum number of request/response body
// bytes that are buffered into [Exchange]. Bodies larger than this limit are
// still forwarded transparently but only the first DefaultBodyCapBytes bytes
// appear in the Exchange.
const DefaultBodyCapBytes = 1 << 20 // 1 MiB

// MethodWebhookIngress is the synthetic [Exchange.Method] value used for
// inbound Telegram webhook deliveries. It is not a real Bot API method; the
// capture layer routes exchanges carrying this method through the same
// per-update resolution as getUpdates so webhook and long-poll ingestion
// behave identically.
const MethodWebhookIngress = "webhookIngress"

// Sink receives a completed Exchange after each proxied request.
// Implementations must be safe for concurrent use; Record must not block the
// proxy response path (use a channel or goroutine if necessary).
type Sink interface {
	// Record delivers ex to the sink. ctx carries the request's context and may
	// already be cancelled by the time Record is called; implementations should
	// use a background context for any work that must outlive the request.
	Record(ctx context.Context, ex *Exchange)
}

// Exchange captures the salient fields of one proxied Bot API call.
// It is the contract between the proxy (step 2) and the capture/telemetry
// layer (step 3). All fields are safe to read after [Sink.Record] is called.
//
// Raw bot tokens are never stored here. TokenHash is a stable, short SHA-256
// hex digest that uniquely identifies the bot without revealing the secret.
type Exchange struct {
	// Timestamp is when the proxy received the upstream response (UTC).
	Timestamp time.Time

	// TokenHash is the first 16 hex characters of SHA-256(raw_token).
	// Use this to group exchanges by bot without storing the secret.
	TokenHash string

	// Method is the Telegram Bot API method name parsed from the request path
	// (e.g. "getMe", "sendMessage", "getUpdates"). Empty if parsing fails.
	Method string

	// ReqBody holds up to BodyCapBytes of the outgoing request body.
	// Nil for GET requests or when the request body is empty.
	ReqBody []byte

	// ReqContentType is the Content-Type header of the request body, used by
	// the capture layer to choose the correct decoder (JSON,
	// x-www-form-urlencoded or multipart/form-data). It is a single header
	// value, never the raw token, and is empty when absent.
	ReqContentType string

	// RespBody holds up to BodyCapBytes of the upstream response body.
	// Nil when the response body is empty.
	RespBody []byte

	// Truncated is true when at least one of the tapped request/response body
	// copies hit the body cap and was cut short, so the captured copy is an
	// incomplete prefix of the real body and any parsing/loop detection done
	// from it may be incomplete. It is false only when every tapped copy was
	// captured in full (or there was nothing to tap). The full body is always
	// still forwarded transparently regardless of this flag.
	Truncated bool

	// StatusCode is the HTTP status code returned by the upstream.
	StatusCode int

	// Duration is the round-trip time from when the proxy forwarded the request
	// to when the full response was received.
	Duration time.Duration
}

// noopSink is the default Sink implementation. It discards all exchanges.
type noopSink struct{}

func (noopSink) Record(_ context.Context, _ *Exchange) {}

// NoopSink returns a [Sink] that discards every exchange. Use it when no
// capture layer is wired in yet.
func NoopSink() Sink { return noopSink{} }

// Proxy is a transparent reverse proxy that forwards /{bot<TOKEN>}/{method}
// requests to the configured upstream Telegram Bot API base URL and taps
// request/response bodies for capture.
//
// Construct via [New]; do not copy after first use.
type Proxy struct {
	upstream     *url.URL
	sink         Sink
	logger       *slog.Logger
	bodyCapBytes int64
	rp           *httputil.ReverseProxy
}

// New constructs a [Proxy] that forwards requests to the upstream URL.
//
// upstreamRaw must be a valid absolute URL (e.g. "https://api.telegram.org").
// sink receives every completed exchange; pass [NoopSink]() (or nil) when no
// capture is needed. logger is used for structured error output; pass nil to
// discard it silently. Both nil inputs are normalized in the constructor, so
// the proxy never nil-panics on the error/record paths.
//
// New panics only if upstreamRaw cannot be parsed as a URL; callers should
// have already validated the URL via [config.Validate].
func New(upstreamRaw string, sink Sink, logger *slog.Logger) (*Proxy, error) {
	return NewWithOptions(upstreamRaw, sink, logger, DefaultBodyCapBytes)
}

// NewWithOptions is like [New] but allows callers to override the body capture
// cap. bodyCapBytes == 0 disables body capture entirely.
func NewWithOptions(upstreamRaw string, sink Sink, logger *slog.Logger, bodyCapBytes int64) (*Proxy, error) {
	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse upstream URL %q: %w", upstreamRaw, err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("proxy: upstream URL %q must be an absolute http/https URL", upstreamRaw)
	}

	// Normalize optional collaborators so the error/record paths can never
	// nil-panic. Callers (examples, tests) legitimately pass nil to mean
	// "no logging" / "no capture".
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if sink == nil {
		sink = NoopSink()
	}

	p := &Proxy{
		upstream:     upstream,
		sink:         sink,
		logger:       logger,
		bodyCapBytes: bodyCapBytes,
	}

	p.rp = &httputil.ReverseProxy{
		Director:       p.director,
		Transport:      proxyTransport(),
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}

	return p, nil
}

// ServeHTTP implements http.Handler. It proxies /{bot<TOKEN>}/{method} requests
// transparently to the upstream Telegram Bot API.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Tap the request body before passing to the reverse proxy.
	var reqBody []byte
	var reqTruncated bool
	if r.Body != nil && r.Body != http.NoBody {
		reqBody, r.Body, reqTruncated = tapBody(r.Body, p.bodyCapBytes)
	}

	// Attach tap state to the request context so modifyResponse can read it.
	ts := &tapState{
		start:        time.Now(),
		reqBody:      reqBody,
		reqCT:        r.Header.Get("Content-Type"),
		reqTruncated: reqTruncated,
		method:       "",
		token:        "",
	}
	method, token := botapi.ParseMethod(r.URL.Path)
	ts.method = method
	ts.token = token

	r = r.WithContext(withTapState(r.Context(), ts))
	p.rp.ServeHTTP(w, r)
}

// WebhookHandlerForRoute returns an http.Handler for one webhook ingress route.
//
// target is the bot server URL that will receive the forwarded update. token is
// the raw bot token for this route; it is hashed immediately and the raw value
// is never retained after this call returns. secretToken, when non-empty, is
// the value that must appear in the X-Telegram-Bot-Api-Secret-Token header;
// requests that fail this check receive a 401 response and are not forwarded.
// Pass an empty secretToken to skip the header check.
//
// The returned handler is safe for concurrent use.
func (p *Proxy) WebhookHandlerForRoute(target, token, secretToken string) (http.Handler, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("proxy: webhook target URL %q: %w", target, err)
	}

	wh := &webhookProxy{
		target:       targetURL,
		tokenHash:    hashToken(token),
		secretToken:  secretToken,
		sink:         p.sink,
		logger:       p.logger,
		bodyCapBytes: p.bodyCapBytes,
		transport:    proxyTransport(),
	}
	return wh, nil
}

// director rewrites the incoming request so it is addressed to the upstream
// Telegram Bot API. It preserves the path (/{bot<TOKEN>}/{method}) exactly.
func (p *Proxy) director(r *http.Request) {
	r.URL.Scheme = p.upstream.Scheme
	r.URL.Host = p.upstream.Host

	// Ensure the upstream path prefix is honoured when the upstream URL has a
	// non-root path (uncommon but supported for test doubles).
	if p.upstream.Path != "" && p.upstream.Path != "/" {
		r.URL.Path = strings.TrimRight(p.upstream.Path, "/") + r.URL.Path
	}

	// Clear the RequestURI — httputil requires this.
	r.RequestURI = ""

	// Propagate the host header to match what Telegram expects.
	r.Host = p.upstream.Host
}

// modifyResponse taps the response body, builds an Exchange, and delivers it
// to the sink. The response is always forwarded byte-transparently regardless
// of tap errors.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	ts, ok := tapStateFrom(resp.Request.Context())
	if !ok {
		// Should not happen; log and continue.
		p.logger.Warn("proxy: no tap state in response context")
		return nil
	}

	// Tap the response body. tapBody replaces resp.Body with a reader over the
	// original, still-encoded bytes, so the client always receives the
	// byte-exact upstream response regardless of what we do to the captured
	// copy below.
	var respBody []byte
	var respTruncated bool
	if resp.Body != nil {
		respBody, resp.Body, respTruncated = tapBody(resp.Body, p.bodyCapBytes)
	}

	// Telegram (and fronting CDNs) may return a Content-Encoding: gzip/deflate
	// body. Go's Transport only auto-decompresses when it added Accept-Encoding
	// itself; a bot framework that sets its own Accept-Encoding makes the proxy
	// receive a still-compressed body. Decompress ONLY the captured copy (never
	// the forwarded bytes) so the capture layer can parse it. An unsupported or
	// undecodable encoding is not fatal: parsing is skipped and the exchange is
	// flagged truncated so the gap is observable, not silent.
	respBody, respTruncated = decodeCapturedBody(
		respBody, resp.Header.Get("Content-Encoding"), respTruncated, p.bodyCapBytes)

	ex := &Exchange{
		Timestamp:      time.Now().UTC(),
		TokenHash:      hashToken(ts.token),
		Method:         ts.method,
		ReqBody:        ts.reqBody,
		ReqContentType: ts.reqCT,
		RespBody:       respBody,
		Truncated:      ts.reqTruncated || respTruncated,
		StatusCode:     resp.StatusCode,
		Duration:       time.Since(ts.start),
	}

	p.sink.Record(resp.Request.Context(), ex)
	return nil
}

// errorHandler logs upstream errors and returns a 502 to the client, redacting
// any token that may appear in the URL.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// Redact the path before logging to ensure the token is not emitted.
	safeURL := redactURL(r.URL)
	p.logger.Error(
		"proxy: upstream error",
		slog.String("method", r.Method),
		slog.String("path", safeURL),
		slog.Any("error", err),
	)
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// -----------------------------------------------------------------------
// Webhook ingress handler
// -----------------------------------------------------------------------

// secretTokenHeader is the HTTP header name that Telegram sets on webhook
// deliveries when the bot registered a secret_token via setWebhook.
const secretTokenHeader = "X-Telegram-Bot-Api-Secret-Token"

// webhookProxy handles inbound Telegram webhook POST requests, taps them, and
// forwards them to the configured bot target URL.
type webhookProxy struct {
	target *url.URL

	// tokenHash is the pre-computed SHA-256 hash of the route's bot token.
	// It is set once at construction time; the raw token is never retained.
	tokenHash string

	// secretToken is the optional shared secret that must match the
	// X-Telegram-Bot-Api-Secret-Token request header.  Empty means skip check.
	secretToken string

	sink         Sink
	logger       *slog.Logger
	bodyCapBytes int64
	transport    http.RoundTripper
}

// ServeHTTP forwards the inbound Telegram webhook POST to the bot's target URL.
//
// Security checks performed before forwarding:
//  1. If secretToken is configured, the X-Telegram-Bot-Api-Secret-Token header
//     must match exactly; mismatches return 401 without forwarding.
//  2. The raw bot token never appears in the URL, logs, or error messages.
func (wh *webhookProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Validate the optional shared secret before reading the body.
	if wh.secretToken != "" {
		got := r.Header.Get(secretTokenHeader)
		if got != wh.secretToken {
			wh.logger.Warn("proxy: webhook: secret-token mismatch — request rejected",
				slog.String("remote_addr", r.RemoteAddr))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Read and tap the incoming body.
	var reqBody []byte
	var reqTruncated bool
	if r.Body != nil && r.Body != http.NoBody {
		reqBody, r.Body, reqTruncated = tapBody(r.Body, wh.bodyCapBytes)
	}

	// Build the forwarded request.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, wh.target.String(), r.Body)
	if err != nil {
		wh.logger.Error("proxy: webhook: build request", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Copy headers (skip hop-by-hop).
	copyHeaders(outReq.Header, r.Header)
	outReq.Header.Set("X-Forwarded-For", r.RemoteAddr)

	resp, err := wh.transport.RoundTrip(outReq)
	if err != nil {
		wh.logger.Error("proxy: webhook: forward", slog.Any("error", err))
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Tap the response body. The forwarded copy below is the original,
	// still-encoded stream; only the captured copy is decompressed for parsing.
	var respBody []byte
	var respTruncated bool
	respBody, resp.Body, respTruncated = tapBody(resp.Body, wh.bodyCapBytes)

	// Copy response back to client (byte-exact, before any decompress-for-parse).
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		wh.logger.Warn("proxy: webhook: copy response body", slog.Any("error", err))
	}

	// Decompress the captured copy only, bounded by the body cap.
	respBody, respTruncated = decodeCapturedBody(
		respBody, resp.Header.Get("Content-Encoding"), respTruncated, wh.bodyCapBytes)

	ex := &Exchange{
		Timestamp:      time.Now().UTC(),
		TokenHash:      wh.tokenHash, // derived from the configured route token
		Method:         MethodWebhookIngress,
		ReqBody:        reqBody,
		ReqContentType: r.Header.Get("Content-Type"),
		RespBody:       respBody,
		Truncated:      reqTruncated || respTruncated,
		StatusCode:     resp.StatusCode,
		Duration:       time.Since(start),
	}
	wh.sink.Record(r.Context(), ex)
}

// -----------------------------------------------------------------------
// Body tap helpers
// -----------------------------------------------------------------------

// tapBody reads up to capBytes from rc into a buffer and returns the buffered
// bytes, a replacement ReadCloser that re-presents the full original stream
// (buffered prefix + remaining tail) to the next consumer, and whether the
// captured copy was truncated.
//
// truncated is true only when the body actually exceeded capBytes so the
// returned bytes are a strict prefix of the real body; it is false when the
// whole body fit (or capBytes <= 0, or buffering failed). It is never a guess —
// it reflects whether one extra byte was readable past the cap.
//
// If capBytes is 0, no buffering is performed and nil is returned for the bytes.
// The original rc is always fully accessible via the returned ReadCloser. The
// returned ReadCloser's Close method always closes rc exactly once.
func tapBody(rc io.ReadCloser, capBytes int64) (captured []byte, replacement io.ReadCloser, truncated bool) {
	if capBytes <= 0 {
		return nil, rc, false
	}

	// Read up to capBytes+1 so a body that exactly fills the cap is
	// distinguished from one that overflows it: if the extra byte is present
	// the body was genuinely truncated relative to capBytes.
	lr := io.LimitReader(rc, capBytes+1)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(lr); err != nil {
		// If buffering fails, pass the original stream through untapped.
		return nil, rc, false
	}

	full := buf.Bytes()

	if int64(len(full)) > capBytes {
		// The body exceeded the cap. Keep only capBytes for the captured copy
		// but re-present the full prefix plus the untapped tail downstream.
		captured = full[:capBytes]
		overflow := full[capBytes:] // the probe byte(s) we read past the cap
		// The custom closer ensures rc is closed when the replacement is closed.
		reconstructed := io.MultiReader(
			bytes.NewReader(captured),
			bytes.NewReader(overflow),
			rc,
		)
		return captured, &closingReader{Reader: reconstructed, closer: rc}, true
	}

	// We read the entire body within the cap; no tail to chain.
	// Close rc now (no more data to read from it).
	captured = full
	_ = rc.Close()
	return captured, io.NopCloser(bytes.NewReader(captured)), false
}

// decodeCapturedBody decompresses the already-tapped (and already body-capped)
// response copy according to the Content-Encoding header so the capture layer
// can parse it. It never touches the bytes forwarded to the client — the
// caller has already replaced resp.Body with a reader over the original,
// still-encoded stream, so transparency is preserved unconditionally.
//
// Behaviour by encoding (case-insensitive, comma-list aware — only the single
// outermost encoding is handled, which is what Telegram/CDNs use):
//
//   - "", "identity":                 returned unchanged.
//   - "gzip" / "x-gzip":              gunzipped.
//   - "deflate":                      inflated (zlib first, raw DEFLATE fallback).
//   - anything else (e.g. "br"):      parsing is skipped — the original capped
//     bytes are returned and truncated is forced true so the unparseable body
//     is observable on the span, not silently empty.
//
// Decompression is bounded by capBytes (a second time): the decompressed output
// is read through an io.LimitReader so a decompression bomb cannot exhaust
// memory. If the captured copy was itself truncated at the cap the compressed
// stream is incomplete; decompression then yields a best-effort prefix and any
// trailing error is tolerated (the bytes decoded so far are still useful and
// truncated is already true). On any hard decode failure the original capped
// bytes are returned with truncated=true so nothing is silently dropped.
//
// capBytes <= 0 means body capture is disabled; the input is returned as-is.
func decodeCapturedBody(captured []byte, contentEncoding string, truncated bool, capBytes int64) (out []byte, outTruncated bool) {
	if len(captured) == 0 || capBytes <= 0 {
		return captured, truncated
	}

	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	// A header like "gzip, identity" — take the outermost (first) token.
	if i := strings.IndexByte(enc, ','); i >= 0 {
		enc = strings.TrimSpace(enc[:i])
	}

	switch enc {
	case "", "identity":
		return captured, truncated
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(captured))
		if err != nil {
			// Not a valid gzip stream (or a truncated header): skip parsing
			// but make the gap observable.
			return captured, true
		}
		defer func() { _ = zr.Close() }()
		return readBounded(zr, captured, truncated, capBytes)
	case "deflate":
		// RFC 7230 "deflate" is zlib-wrapped DEFLATE, but some servers send
		// raw DEFLATE. Try zlib first, fall back to raw flate.
		if zr, err := zlib.NewReader(bytes.NewReader(captured)); err == nil {
			defer func() { _ = zr.Close() }()
			return readBounded(zr, captured, truncated, capBytes)
		}
		fr := flate.NewReader(bytes.NewReader(captured))
		defer func() { _ = fr.Close() }()
		return readBounded(fr, captured, truncated, capBytes)
	default:
		// Unknown/unsupported encoding (e.g. br): do not crash, do not parse
		// garbage. Flag truncated so the empty parse is observable.
		return captured, true
	}
}

// readBounded reads up to capBytes from r (a decompressor over the captured
// copy). On a clean decode the decompressed bytes are returned with the
// original truncated flag. If the decompressed output would exceed capBytes the
// result is capped and truncated is forced true (decompression-bomb guard). A
// read error (e.g. an incomplete stream because the captured copy was cut at
// the body cap) is tolerated: whatever was decoded is returned with
// truncated=true; if nothing decoded, the original compressed bytes are kept so
// the exchange is never silently emptied.
func readBounded(r io.Reader, original []byte, truncated bool, capBytes int64) ([]byte, bool) {
	// Read capBytes+1 so we can detect overflow past the cap.
	lr := io.LimitReader(r, capBytes+1)
	dec, err := io.ReadAll(lr)
	if int64(len(dec)) > capBytes {
		return dec[:capBytes], true
	}
	if err != nil {
		if len(dec) == 0 {
			// Hard failure with nothing usable: keep the original capped
			// (compressed) bytes but flag it so it is not a silent empty.
			return original, true
		}
		// Partial decode (likely an incomplete stream from a capped capture):
		// the prefix is still parseable best-effort.
		return dec, true
	}
	return dec, truncated
}

// closingReader wraps an io.Reader and closes an underlying io.Closer when its
// own Close is called. This is used to ensure the original ReadCloser is always
// closed exactly once even when the body is reconstructed with io.MultiReader.
type closingReader struct {
	io.Reader
	closer io.Closer
}

func (c *closingReader) Close() error {
	return c.closer.Close()
}

// -----------------------------------------------------------------------
// Token hashing and redaction
// -----------------------------------------------------------------------

// hashToken returns the first 16 hex characters of SHA-256(token).
// An empty token produces an empty hash so callers can distinguish
// "no token found" from a real hash.
func hashToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:8]) // 8 bytes = 16 hex chars
}

// botTokenInPath matches a Telegram bot-token path segment
// (`bot<digits>:<token>`) wherever it appears in the path — not only as the
// first segment. A self-hosted Bot API upstream may sit behind a path prefix
// (e.g. /prefix/bot<TOKEN>/method), so anchoring to the first segment would
// let the token escape redaction into error logs.
var botTokenInPath = regexp.MustCompile(`bot\d+:[A-Za-z0-9_-]+`)

// redactURL returns a string representation of u with any bot token in the
// path replaced by "bot<redacted>" so it is safe to include in log output.
// Redaction is positional-independent: it matches the token segment anywhere
// in the path, including behind an upstream path prefix.
func redactURL(u *url.URL) string {
	safe := *u
	safe.Path = botTokenInPath.ReplaceAllString(u.Path, "bot<redacted>")
	// RawPath (if set) is what String() actually emits for the path; keep it
	// consistent so the raw, un-redacted form cannot leak via the encoded path.
	if u.RawPath != "" {
		safe.RawPath = botTokenInPath.ReplaceAllString(u.RawPath, "bot<redacted>")
	}
	safe.RawQuery = ""
	return safe.String()
}

// -----------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------

// hopByHopHeaders lists headers that must not be forwarded between hops.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeaders copies src into dst, skipping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[k] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// proxyTransport returns an http.RoundTripper tuned for proxying to Telegram.
//
// Key decisions:
//   - DialContext timeout: 10 s — fast-fail on DNS/TCP problems.
//   - TLSHandshakeTimeout: 10 s — ditto for TLS.
//   - ResponseHeaderTimeout: 0 — disabled; long-poll getUpdates hold connections
//     for up to the Telegram poll_timeout (max 50 s); killing the response header
//     wait would prematurely cancel those requests.
//   - IdleConnTimeout: 90 s — keep-alive connections to api.telegram.org.
//   - No overall http.Client.Timeout — the request context controls deadline.
func proxyTransport() http.RoundTripper {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // must be 0 for long-poll compatibility
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		ForceAttemptHTTP2:     true,
	}
}

// -----------------------------------------------------------------------
// Context key for tap state
// -----------------------------------------------------------------------

// tapStateKey is an unexported context key type to avoid collisions.
type tapStateKey struct{}

// tapState carries per-request tap data through the ReverseProxy Director →
// ModifyResponse pipeline via the request context.
type tapState struct {
	start        time.Time
	reqBody      []byte
	reqCT        string
	reqTruncated bool
	method       string
	token        string
}

func withTapState(ctx context.Context, ts *tapState) context.Context {
	return context.WithValue(ctx, tapStateKey{}, ts)
}

func tapStateFrom(ctx context.Context) (*tapState, bool) {
	ts, ok := ctx.Value(tapStateKey{}).(*tapState)
	return ts, ok
}
