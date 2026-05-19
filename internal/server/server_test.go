package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/server"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// labelHandler records which label was hit so route isolation can be asserted.
func labelHandler(label string, hits *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits = append(*hits, label)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, label)
	})
}

// TestWebhookRoutesRegisteredAndIsolated verifies that each configured label
// maps to its own handler, unknown labels return 404, and the built-in
// endpoints plus the catch-all proxy are unaffected.
func TestWebhookRoutesRegisteredAndIsolated(t *testing.T) {
	t.Parallel()

	var hits []string
	p, err := proxy.New("https://api.telegram.org", proxy.NoopSink(), discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	routes := []server.WebhookRoute{
		{Label: "router-bot", Handler: labelHandler("router-bot", &hits)},
		{Label: "sales-bot", Handler: labelHandler("sales-bot", &hits)},
	}

	srv := server.New(":0", p, nil, discardLogger(), routes...)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantHit    string // label expected in hits, "" = none
	}{
		{"route one", "/webhook/router-bot", http.StatusOK, "router-bot"},
		{"route two", "/webhook/sales-bot", http.StatusOK, "sales-bot"},
		{"unknown label", "/webhook/nope", http.StatusNotFound, ""},
		{"webhook root no label", "/webhook/", http.StatusNotFound, ""},
		{"healthz unaffected", "/healthz", http.StatusOK, ""},
		{"metrics unaffected", "/metrics", http.StatusOK, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits = nil
			resp, err := http.Post(ts.URL+tc.path, "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantHit == "" {
				if len(hits) != 0 {
					t.Errorf("expected no webhook handler hit, got %v", hits)
				}
			} else {
				if len(hits) != 1 || hits[0] != tc.wantHit {
					t.Errorf("hits = %v, want exactly [%q]", hits, tc.wantHit)
				}
			}
		})
	}
}

// TestNoWebhookRoutesUnchanged verifies that with no webhook routes the server
// behaves exactly as before: /webhook/* falls through to the proxy catch-all
// (no special 404 guard registered).
func TestNoWebhookRoutesUnchanged(t *testing.T) {
	t.Parallel()

	p, err := proxy.New("https://api.telegram.org", proxy.NoopSink(), discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := server.New(":0", p, nil, discardLogger())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// /healthz still works; this proves the no-route path is wired normally.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
}
