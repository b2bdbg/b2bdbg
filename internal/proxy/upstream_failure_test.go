package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// The raw token used across the failure-path tests. Every assertion below
// verifies this exact string never escapes into a client-visible error body,
// the structured log, or anywhere derived from the failing path.
const failTok = "123456:AAH-SuperSecretBotTokenDoNotLeak"

// bufLogger returns a slog.Logger writing JSON into a synchronised buffer plus
// the buffer so the test can assert what was (not) logged on the failure path.
func bufLogger() (*slog.Logger, *syncBuffer) {
	sb := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(sb, &slog.HandlerOptions{Level: slog.LevelDebug})), sb
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestUpstreamFailureMatrix exercises the upstream failure modes and asserts a
// sane client status, no panic/hang, and — critically — that the raw bot token
// never appears in the client-visible error body or the structured log on any
// failure path.
func TestUpstreamFailureMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the upstream base URL the proxy should target and an
		// optional cleanup func.
		setup func() (upstreamURL string, cleanup func())
		// wantStatus is the status the client must observe. 0 means "client
		// transport error is acceptable" (e.g. forced timeout where the proxy
		// itself never replies because the context is cancelled).
		wantStatus int
		// clientTimeout bounds the client so a hung proxy fails fast.
		clientTimeout time.Duration
	}{
		{
			name: "connection refused",
			setup: func() (string, func()) {
				// Bind then immediately close so the port is dead.
				s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := s.URL
				s.Close()
				return url, func() {}
			},
			wantStatus:    http.StatusBadGateway,
			clientTimeout: 5 * time.Second,
		},
		{
			name: "connection reset mid-flight",
			setup: func() (string, func()) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hj, ok := w.(http.Hijacker)
					if !ok {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
				}))
				return s.URL, s.Close
			},
			wantStatus:    http.StatusBadGateway,
			clientTimeout: 5 * time.Second,
		},
		{
			name: "upstream 5xx is forwarded",
			setup: func() (string, func()) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					// A real Telegram 5xx never echoes the token; assert the
					// proxy forwards the upstream status verbatim.
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`)
				}))
				return s.URL, s.Close
			},
			wantStatus:    http.StatusBadGateway,
			clientTimeout: 5 * time.Second,
		},
		{
			name: "upstream returns garbage body with 200",
			setup: func() (string, func()) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte{0x00, 0xff, 0x01, 0xfe, 0x7f})
				}))
				return s.URL, s.Close
			},
			wantStatus:    http.StatusOK, // transparent: garbage is forwarded as-is
			clientTimeout: 5 * time.Second,
		},
		{
			name: "upstream returns empty body with 200",
			setup: func() (string, func()) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				return s.URL, s.Close
			},
			wantStatus:    http.StatusOK,
			clientTimeout: 5 * time.Second,
		},
		{
			name: "unresolvable host (DNS failure)",
			setup: func() (string, func()) {
				return "http://b2bdbg-nonexistent-host.invalid", func() {}
			},
			wantStatus:    http.StatusBadGateway,
			clientTimeout: 15 * time.Second,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstreamURL, cleanup := tc.setup()
			defer cleanup()

			logger, logBuf := bufLogger()
			sink := &recordingSink{}
			p, err := proxy.New(upstreamURL, sink, logger)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}
			srv := httptest.NewServer(p)
			defer srv.Close()

			client := &http.Client{Timeout: tc.clientTimeout}
			req, _ := http.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				srv.URL+"/bot"+failTok+"/sendMessage",
				strings.NewReader(`{"chat_id":1,"text":"hi"}`),
			)

			resp, err := client.Do(req)
			if err != nil {
				// A transport error is only acceptable if we did not require a
				// specific status. We must still never hang (the client
				// timeout bounds that) and must not have leaked the token.
				if tc.wantStatus != 0 {
					t.Fatalf("client error (proxy hung or crashed?): %v", err)
				}
			} else {
				body, _ := io.ReadAll(resp.Body)
				if cerr := resp.Body.Close(); cerr != nil {
					t.Errorf("response body close: %v", cerr)
				}
				if tc.wantStatus != 0 && resp.StatusCode != tc.wantStatus {
					t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
				}
				// Token must never appear in the client-visible body.
				if bytes.Contains(body, []byte(failTok)) {
					t.Errorf("raw token leaked into client error body: %q", body)
				}
			}

			// Token must never appear in the structured log on the failure
			// path (redactURL must have been applied).
			if strings.Contains(logBuf.String(), failTok) {
				t.Errorf("raw token leaked into log output:\n%s", logBuf.String())
			}
			// If an exchange was recorded it must carry the HASH, never the
			// raw token, anywhere.
			for _, ex := range sink.all() {
				if strings.Contains(ex.TokenHash, failTok) {
					t.Error("raw token leaked into Exchange.TokenHash")
				}
				if bytes.Contains(ex.RespBody, []byte(failTok)) {
					t.Error("raw token leaked into Exchange.RespBody")
				}
			}
		})
	}
}

// TestUpstreamTimeoutNotHang verifies that when the request context is
// cancelled (client gives up) the proxy does not hang or panic: the goroutine
// returns and the upstream connection is torn down. It also proves the proxy
// itself does not impose a short ResponseHeaderTimeout that would kill a slow
// upstream prematurely (see TestLongPollHeldRequestSurvives below).
func TestUpstreamTimeoutNotHang(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // client/proxy gave up — expected here
		case <-time.After(10 * time.Second):
		}
		close(released)
		_ = w
	}))
	defer upstream.Close()

	logger, logBuf := bufLogger()
	sink := &recordingSink{}
	p, err := proxy.New(upstream.URL, sink, logger)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(
		ctx, http.MethodPost,
		srv.URL+"/bot"+failTok+"/getMe", nil,
	)
	_, err = http.DefaultClient.Do(req)
	if err == nil {
		t.Fatal("expected client error on context timeout")
	}

	// The upstream handler must observe context cancellation promptly (proves
	// the proxy propagated cancel and is not hanging).
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not released — proxy hung instead of cancelling")
	}

	if strings.Contains(logBuf.String(), failTok) {
		t.Errorf("raw token leaked into log on timeout path:\n%s", logBuf.String())
	}
}

// TestLongPollHeldRequestSurvives proves the proxy's own transport config
// (ResponseHeaderTimeout: 0) does NOT prematurely cancel a legitimately slow
// long-poll getUpdates that holds the connection well past any short internal
// timeout before replying.
func TestLongPollHeldRequestSurvives(t *testing.T) {
	t.Parallel()

	const hold = 2 * time.Second // well under CI limits, well over any short timeout

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(hold):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case <-r.Context().Done():
			// Must NOT happen: a correctly configured proxy does not cancel a
			// slow long-poll on its own.
			http.Error(w, "premature cancel", http.StatusGatewayTimeout)
		}
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(
		context.Background(), http.MethodPost,
		srv.URL+"/bot1:T/getUpdates",
		strings.NewReader(`{"timeout":30}`),
	)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("long-poll request failed (proxy killed it?): %v", err)
	}
	elapsed := time.Since(start)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — proxy prematurely cancelled the long-poll", resp.StatusCode)
	}
	if elapsed < hold {
		t.Errorf("elapsed %v < hold %v — long-poll was short-circuited", elapsed, hold)
	}
	if ex := sink.last(); ex == nil || ex.Method != "getUpdates" {
		t.Errorf("expected a getUpdates exchange to be captured")
	}
}
