package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
	"github.com/b2bdbg/b2bdbg/internal/server"
)

// TestDebugRegistryDisabledReturns404 verifies that with debug endpoints OFF
// (the default — Options.DebugRegistry nil) GET /debug/registry is never
// registered and the path is rejected (it falls through to the proxy
// catch-all, which 404s a non-/bot path).
func TestDebugRegistryDisabledReturns404(t *testing.T) {
	t.Parallel()

	p, err := proxy.New("https://api.telegram.org", proxy.NoopSink(), discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := server.NewWithOptions(":0", p, nil, discardLogger(), server.Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/debug/registry")
	if err != nil {
		t.Fatalf("GET /debug/registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when debug endpoints disabled", resp.StatusCode)
	}
}

// TestDebugRegistryEnabledReturnsJSON verifies that when debug endpoints are
// opted in, GET /debug/registry returns the id↔hash entries as JSON, with the
// hashes already written to spans and never any raw token.
func TestDebugRegistryEnabledReturnsJSON(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)
	reg.Register(1001, "hashbot1")
	reg.Register(2002, "hashbot2")

	p, err := proxy.New("https://api.telegram.org", proxy.NoopSink(), discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := server.NewWithOptions(":0", p, nil, discardLogger(), server.Options{
		DebugRegistry: reg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/debug/registry")
	if err != nil {
		t.Fatalf("GET /debug/registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body, _ := io.ReadAll(resp.Body)

	var got struct {
		Count   int                     `json:"count"`
		Entries []capture.RegistryEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, body)
	}
	if got.Count != 2 || len(got.Entries) != 2 {
		t.Fatalf("count=%d entries=%d, want 2/2", got.Count, len(got.Entries))
	}
	// Insertion order is preserved.
	if got.Entries[0].BotID != 1001 || got.Entries[0].TokenHash != "hashbot1" {
		t.Errorf("entry[0] = %+v, want {1001 hashbot1}", got.Entries[0])
	}
	if got.Entries[1].BotID != 2002 || got.Entries[1].TokenHash != "hashbot2" {
		t.Errorf("entry[1] = %+v, want {2002 hashbot2}", got.Entries[1])
	}
}

// TestDebugRegistryRejectsNonGet verifies non-GET methods are rejected so the
// endpoint is strictly read-only.
func TestDebugRegistryRejectsNonGet(t *testing.T) {
	t.Parallel()

	reg := capture.NewBotRegistry(0)
	p, err := proxy.New("https://api.telegram.org", proxy.NoopSink(), discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := server.NewWithOptions(":0", p, nil, discardLogger(), server.Options{DebugRegistry: reg})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/debug/registry", "application/json", http.NoBody)
	if err != nil {
		t.Fatalf("POST /debug/registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for POST", resp.StatusCode)
	}
}
