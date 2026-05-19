package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// -----------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------

// recordingSink is a Sink that collects every Exchange for assertion.
type recordingSink struct {
	mu        sync.Mutex
	exchanges []*proxy.Exchange
}

func (s *recordingSink) Record(_ context.Context, ex *proxy.Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = append(s.exchanges, ex)
}

func (s *recordingSink) all() []*proxy.Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*proxy.Exchange, len(s.exchanges))
	copy(out, s.exchanges)
	return out
}

func (s *recordingSink) last() *proxy.Exchange {
	all := s.all()
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// newTestProxy builds a Proxy pointed at upstream and returns it alongside the
// recording sink.
func newTestProxy(t *testing.T, upstream string) (*proxy.Proxy, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	p, err := proxy.New(upstream, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, sink
}

// fixtureBytes reads a testdata fixture relative to the repo root.
// internal/proxy is two levels deep from the repo root.
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// -----------------------------------------------------------------------
// Transparent forwarding tests
// -----------------------------------------------------------------------

func TestTransparentForwarding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		upstreamHeader map[string]string
		httpMethod     string
		path           string
		reqBody        string
		wantStatus     int
		wantBodySubstr string
		wantHeader     map[string]string
	}{
		{
			name:           "getMe 200 forwarded transparently",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"ok":true,"result":{"id":1}}`,
			upstreamHeader: map[string]string{"Content-Type": "application/json"},
			httpMethod:     http.MethodGet,
			path:           "/bot1234567890:ABC/getMe",
			wantStatus:     http.StatusOK,
			wantBodySubstr: `"ok":true`,
			wantHeader:     map[string]string{"Content-Type": "application/json"},
		},
		{
			name:           "sendMessage POST forwarded transparently",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"ok":true,"result":{"message_id":42}}`,
			upstreamHeader: map[string]string{"Content-Type": "application/json"},
			httpMethod:     http.MethodPost,
			path:           "/botABC123:XYZ/sendMessage",
			reqBody:        `{"chat_id":1,"text":"hi"}`,
			wantStatus:     http.StatusOK,
			wantBodySubstr: `"message_id":42`,
		},
		{
			name:           "upstream 400 forwarded transparently",
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"ok":false,"error_code":400,"description":"Bad Request"}`,
			httpMethod:     http.MethodPost,
			path:           "/botTEST/sendMessage",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: `"ok":false`,
		},
		{
			name:           "upstream 404 forwarded transparently",
			upstreamStatus: http.StatusNotFound,
			upstreamBody:   `{"ok":false,"error_code":404}`,
			httpMethod:     http.MethodGet,
			path:           "/botXXX/unknownMethod",
			wantStatus:     http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.upstreamHeader {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, tc.upstreamBody)
			}))
			defer upstream.Close()

			p, _ := newTestProxy(t, upstream.URL)
			srv := httptest.NewServer(p)
			defer srv.Close()

			var reqBody io.Reader
			if tc.reqBody != "" {
				reqBody = strings.NewReader(tc.reqBody)
			}
			req, err := http.NewRequestWithContext(context.Background(), tc.httpMethod, srv.URL+tc.path, reqBody)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			respBody, _ := io.ReadAll(resp.Body)
			if tc.wantBodySubstr != "" && !strings.Contains(string(respBody), tc.wantBodySubstr) {
				t.Errorf("body %q does not contain %q", string(respBody), tc.wantBodySubstr)
			}

			for k, want := range tc.wantHeader {
				if got := resp.Header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------
// Multipart forwarding
// -----------------------------------------------------------------------

func TestMultipartForwarding(t *testing.T) {
	t.Parallel()

	var (
		mu                  sync.Mutex
		receivedContentType string
		receivedBodyLen     int
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedContentType = ct
		receivedBodyLen = len(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, _ := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	// Build a multipart body simulating a file upload (sendDocument).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", "12345"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	fw, err := mw.CreateFormFile("document", "test.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write(bytes.Repeat([]byte("x"), 512))
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot999:TOKEN/sendDocument",
		&buf,
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	mu.Lock()
	ct := receivedContentType
	bl := receivedBodyLen
	mu.Unlock()

	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("upstream received Content-Type %q, want multipart/form-data", ct)
	}
	if bl == 0 {
		t.Error("upstream received empty body for multipart request")
	}
}

// -----------------------------------------------------------------------
// Tap captures correct method + bodies
// -----------------------------------------------------------------------

func TestTapCapture(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		reqBody      string
		respBody     string
		wantMethod   string
		wantReqBody  string
		wantRespBody string
	}{
		{
			name:         "getMe captures method and response",
			path:         "/bot111:TOKEN/getMe",
			respBody:     `{"ok":true,"result":{"id":111}}`,
			wantMethod:   "getMe",
			wantRespBody: `{"ok":true,"result":{"id":111}}`,
		},
		{
			name:         "sendMessage captures method, request, and response",
			path:         "/bot222:TOKEN/sendMessage",
			reqBody:      `{"chat_id":1,"text":"hi"}`,
			respBody:     `{"ok":true,"result":{"message_id":1}}`,
			wantMethod:   "sendMessage",
			wantReqBody:  `{"chat_id":1,"text":"hi"}`,
			wantRespBody: `{"ok":true,"result":{"message_id":1}}`,
		},
		{
			name:       "getUpdates captured",
			path:       "/bot333:TOKEN/getUpdates",
			respBody:   `{"ok":true,"result":[]}`,
			wantMethod: "getUpdates",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.respBody)
			}))
			defer upstream.Close()

			p, sink := newTestProxy(t, upstream.URL)
			srv := httptest.NewServer(p)
			defer srv.Close()

			var body io.Reader
			if tc.reqBody != "" {
				body = strings.NewReader(tc.reqBody)
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+tc.path, body)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			ex := sink.last()
			if ex == nil {
				t.Fatal("no exchange recorded")
			}

			if ex.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", ex.Method, tc.wantMethod)
			}

			if tc.wantReqBody != "" {
				if !bytes.Contains(ex.ReqBody, []byte(tc.wantReqBody)) {
					t.Errorf("ReqBody = %q, want to contain %q", ex.ReqBody, tc.wantReqBody)
				}
			}

			if tc.wantRespBody != "" {
				if !bytes.Contains(ex.RespBody, []byte(tc.wantRespBody)) {
					t.Errorf("RespBody = %q, want to contain %q", ex.RespBody, tc.wantRespBody)
				}
			}

			if ex.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d, want 200", ex.StatusCode)
			}
			if ex.Duration <= 0 {
				t.Error("Duration must be positive")
			}
			if ex.Timestamp.IsZero() {
				t.Error("Timestamp must not be zero")
			}
		})
	}
}

// -----------------------------------------------------------------------
// Body cap / pass-through behaviour
// -----------------------------------------------------------------------

func TestBodyCapPassThrough(t *testing.T) {
	t.Parallel()

	const bodyCap int64 = 64 // tiny cap to test truncation
	const bodySize = 1024

	largeBody := bytes.Repeat([]byte("A"), bodySize)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got)
	}))
	defer upstream.Close()

	sink := &recordingSink{}
	p, err := proxy.NewWithOptions(upstream.URL, sink, slog.New(slog.NewTextHandler(io.Discard, nil)), bodyCap)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/sendDocument",
		bytes.NewReader(largeBody),
	)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	clientBody, _ := io.ReadAll(resp.Body)

	// Client must receive the full body (byte-transparent).
	if len(clientBody) != bodySize {
		t.Errorf("client received %d bytes, want %d (full body)", len(clientBody), bodySize)
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}

	// Captured request body must be capped.
	if int64(len(ex.ReqBody)) > bodyCap {
		t.Errorf("captured ReqBody len = %d, want <= %d (cap)", len(ex.ReqBody), bodyCap)
	}
	// Should have captured exactly bodyCap bytes (body was larger).
	if int64(len(ex.ReqBody)) != bodyCap {
		t.Errorf("captured ReqBody len = %d, want exactly %d", len(ex.ReqBody), bodyCap)
	}
}

// -----------------------------------------------------------------------
// Token redaction / hash
// -----------------------------------------------------------------------

func TestTokenHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		rawToken  string
		sameToken bool
	}{
		{
			name:      "token hashed not stored raw",
			path:      "/bot9876543210:ABCDEF_secret_token/getMe",
			rawToken:  "9876543210:ABCDEF_secret_token",
			sameToken: false,
		},
		{
			name:      "same token same hash",
			path:      "/bot111222333:SAME_TOKEN/sendMessage",
			rawToken:  "111222333:SAME_TOKEN",
			sameToken: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer upstream.Close()

			p, sink := newTestProxy(t, upstream.URL)
			srv := httptest.NewServer(p)
			defer srv.Close()

			doReq := func() *proxy.Exchange {
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+tc.path, nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("do request: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				_, _ = io.ReadAll(resp.Body)
				return sink.last()
			}

			ex1 := doReq()
			if ex1 == nil {
				t.Fatal("no exchange recorded")
			}

			if ex1.TokenHash == "" {
				t.Error("TokenHash must not be empty")
			}
			if strings.Contains(ex1.TokenHash, tc.rawToken) {
				t.Errorf("TokenHash %q contains raw token — must be hashed", ex1.TokenHash)
			}
			// Hash should be exactly 16 hex characters.
			if len(ex1.TokenHash) != 16 {
				t.Errorf("TokenHash len = %d, want 16", len(ex1.TokenHash))
			}
			for _, c := range ex1.TokenHash {
				if !strings.ContainsRune("0123456789abcdef", c) {
					t.Errorf("TokenHash %q contains non-hex character %c", ex1.TokenHash, c)
					break
				}
			}

			if tc.sameToken {
				ex2 := doReq()
				if ex2 == nil {
					t.Fatal("no exchange recorded on second request")
				}
				if ex1.TokenHash != ex2.TokenHash {
					t.Errorf("same token produced different hashes: %q vs %q", ex1.TokenHash, ex2.TokenHash)
				}
			}
		})
	}
}

func TestDifferentTokensDifferentHashes(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	paths := []string{
		"/bot111:TOKENA/getMe",
		"/bot222:TOKENB/getMe",
	}

	hashes := make([]string, 0, len(paths))
	for _, path := range paths {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		hashes = append(hashes, sink.last().TokenHash)
	}

	if hashes[0] == hashes[1] {
		t.Errorf("different tokens produced the same hash: %q", hashes[0])
	}
}

// -----------------------------------------------------------------------
// Long-poll not prematurely cancelled
// -----------------------------------------------------------------------

func TestLongPollNotCancelled(t *testing.T) {
	t.Parallel()

	const holdDuration = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(holdDuration):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case <-r.Context().Done():
			// This branch must NOT be taken in a correctly-configured proxy.
			http.Error(w, "context cancelled", http.StatusGatewayTimeout)
		}
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/getUpdates",
		strings.NewReader(`{"timeout":1}`),
	)

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("request failed (long-poll cancelled?): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (long-poll should complete)", resp.StatusCode)
	}
	if elapsed < holdDuration {
		t.Errorf("elapsed %v < hold %v — proxy may have short-circuited the long poll", elapsed, holdDuration)
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if ex.Method != "getUpdates" {
		t.Errorf("Method = %q, want getUpdates", ex.Method)
	}
}

// -----------------------------------------------------------------------
// Testdata fixture round-trips
// -----------------------------------------------------------------------

func TestFixtureRoundTrips(t *testing.T) {
	t.Parallel()

	fixtures := []struct {
		name     string
		reqFile  string
		respFile string
		path     string
		httpMeth string
	}{
		{
			name:     "getMe response fixture",
			respFile: "getme_response.json",
			path:     "/bot1:T/getMe",
			httpMeth: http.MethodGet,
		},
		{
			name:     "sendMessage request+response fixtures",
			reqFile:  "sendmessage_request.json",
			respFile: "sendmessage_response.json",
			path:     "/bot1:T/sendMessage",
			httpMeth: http.MethodPost,
		},
		{
			name:     "getUpdates response fixture",
			respFile: "getupdates_response.json",
			path:     "/bot1:T/getUpdates",
			httpMeth: http.MethodPost,
		},
	}

	for _, tc := range fixtures {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			respBody := fixtureBytes(t, tc.respFile)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(respBody)
			}))
			defer upstream.Close()

			p, sink := newTestProxy(t, upstream.URL)
			srv := httptest.NewServer(p)
			defer srv.Close()

			var reqBody io.Reader
			if tc.reqFile != "" {
				reqBody = bytes.NewReader(fixtureBytes(t, tc.reqFile))
			}

			req, _ := http.NewRequestWithContext(context.Background(), tc.httpMeth, srv.URL+tc.path, reqBody)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			got, _ := io.ReadAll(resp.Body)

			if !json.Valid(got) {
				t.Errorf("response body is not valid JSON: %s", got)
			}
			if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(respBody)) {
				t.Errorf("response body mismatch\ngot:  %s\nwant: %s", got, respBody)
			}

			ex := sink.last()
			if ex == nil {
				t.Fatal("no exchange recorded")
			}
			if ex.StatusCode != http.StatusOK {
				t.Errorf("exchange StatusCode = %d, want 200", ex.StatusCode)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Upstream error handling (502)
// -----------------------------------------------------------------------

func TestUpstreamError(t *testing.T) {
	t.Parallel()

	// Point at a server that immediately closes the TCP connection.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer upstream.Close()

	p, _ := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/bot1:T/getMe", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request itself failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on upstream error", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------
// NoopSink — must not panic
// -----------------------------------------------------------------------

func TestNoopSink(t *testing.T) {
	t.Parallel()

	sink := proxy.NoopSink()
	ex := &proxy.Exchange{
		Timestamp:  time.Now(),
		TokenHash:  "deadbeef12345678",
		Method:     "getMe",
		StatusCode: 200,
		Duration:   time.Millisecond,
	}
	// Must not panic.
	sink.Record(context.Background(), ex)
}

// -----------------------------------------------------------------------
// Exchange field coverage
// -----------------------------------------------------------------------

func TestExchangeFields(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot42:MYTOKEN/sendMessage",
		strings.NewReader(`{"chat_id":1,"text":"test"}`),
	)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}

	tokenHashOK := !strings.Contains(ex.TokenHash, "MYTOKEN")
	tokenHashMsg := fmt.Sprintf("TokenHash %q contains raw token", ex.TokenHash)
	methodMsg := fmt.Sprintf("Method = %q, want sendMessage", ex.Method)
	statusMsg := fmt.Sprintf("StatusCode = %d, want 201", ex.StatusCode)

	type check struct {
		name string
		ok   bool
		msg  string
	}
	checks := []check{
		{"Timestamp not zero", !ex.Timestamp.IsZero(), "Timestamp is zero"},
		{"TokenHash not empty", ex.TokenHash != "", "TokenHash is empty"},
		{"TokenHash not raw token", tokenHashOK, tokenHashMsg},
		{"Method correct", ex.Method == "sendMessage", methodMsg},
		{"ReqBody not nil", ex.ReqBody != nil, "ReqBody is nil"},
		{"RespBody not nil", ex.RespBody != nil, "RespBody is nil"},
		{"StatusCode correct", ex.StatusCode == http.StatusCreated, statusMsg},
		{"Duration positive", ex.Duration > 0, "Duration is not positive"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: %s", c.name, c.msg)
		}
	}
}

// -----------------------------------------------------------------------
// Body close propagation (finding 7)
// -----------------------------------------------------------------------

// trackingReadCloser wraps an io.ReadCloser and records whether Close has been
// called. Used to verify that the original ReadCloser is closed exactly once.
type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	return nil
}

// TestBodyCloseCallsOriginalRC verifies that when tapBody replaces a large body
// (i.e. the body cap is exceeded and a MultiReader is used), closing the
// replacement ReadCloser also closes the original ReadCloser.
func TestBodyCloseCallsOriginalRC(t *testing.T) {
	t.Parallel()

	// Large body that exceeds the body cap so the MultiReader path is taken.
	const bodyCap int64 = 4
	body := bytes.NewReader(bytes.Repeat([]byte("X"), 16))
	trc := &trackingReadCloser{Reader: body}

	_, replacement, truncated := proxy.TapBodyExported(trc, bodyCap)
	if !truncated {
		t.Error("tapBody should report truncated=true when the body exceeds the cap")
	}

	// Closing the replacement must close the original.
	if err := replacement.Close(); err != nil {
		t.Fatalf("replacement.Close() = %v", err)
	}
	if !trc.closed {
		t.Error("original ReadCloser was NOT closed when the replacement was closed")
	}
}

// TestBodyCloseSmallBodyClosesOriginal verifies that when the entire body fits
// within the body cap (the simple path), the original ReadCloser is closed
// in-place and the returned reader can still be read cleanly.
func TestBodyCloseSmallBodyClosesOriginal(t *testing.T) {
	t.Parallel()

	// Body smaller than the body cap — original is closed eagerly by tapBody.
	const bodyCap int64 = 1024
	body := strings.NewReader("tiny")
	trc := &trackingReadCloser{Reader: body}

	captured, replacement, truncated := proxy.TapBodyExported(trc, bodyCap)
	if truncated {
		t.Error("tapBody should report truncated=false when the body fits within the cap")
	}

	// The captured bytes should contain the full body.
	if string(captured) != "tiny" {
		t.Errorf("captured = %q, want %q", captured, "tiny")
	}
	// Original must already be closed after tapBody returns.
	if !trc.closed {
		t.Error("original ReadCloser should be closed eagerly when body fits within cap")
	}
	// The replacement is a NopCloser over a bytes.Reader; closing it must not error.
	if err := replacement.Close(); err != nil {
		t.Errorf("replacement.Close() = %v", err)
	}
}

// TestTapBodyTruncatedFlag exercises the b2b.capture.truncated source: the
// flag must be true ONLY when the body actually exceeded the cap, including
// the exact-fit boundary (a body whose length == cap is NOT truncated).
func TestTapBodyTruncatedFlag(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 8
	cases := []struct {
		name      string
		body      string
		wantTrunc bool
		wantCap   string
	}{
		{"empty", "", false, ""},
		{"under cap", "abc", false, "abc"},
		{"exactly cap", "12345678", false, "12345678"},
		{"one over cap", "123456789", true, "12345678"},
		{"far over cap", "abcdefghijklmnopqrstuvwxyz", true, "abcdefgh"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := io.NopCloser(strings.NewReader(tc.body))
			captured, replacement, truncated := proxy.TapBodyExported(rc, capBytes)
			if truncated != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", truncated, tc.wantTrunc)
			}
			if string(captured) != tc.wantCap {
				t.Errorf("captured = %q, want %q", captured, tc.wantCap)
			}
			// The replacement must always re-present the FULL original body
			// byte-for-byte regardless of truncation.
			got, err := io.ReadAll(replacement)
			if err != nil {
				t.Fatalf("ReadAll(replacement) = %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("reconstructed body = %q, want %q (full body must stream through)", got, tc.body)
			}
			if err := replacement.Close(); err != nil {
				t.Errorf("replacement.Close() = %v", err)
			}
		})
	}
}

// TestProxyHonorsConfiguredBodyCap verifies the configured body cap (the
// B2BD_BODY_CAP_BYTES wiring point) reaches the Exchange: a tiny cap makes a
// modest response body report Truncated=true, while the client still receives
// the complete body byte-for-byte.
func TestProxyHonorsConfiguredBodyCap(t *testing.T) {
	t.Parallel()

	const fullBody = `{"ok":true,"result":{"id":1,"first_name":"padpadpadpadpad"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullBody))
	}))
	defer upstream.Close()

	sink := &recordingSink{}
	// Cap far below the response size so the tapped copy is truncated.
	p, err := proxy.NewWithOptions(upstream.URL, sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 8)
	if err != nil {
		t.Fatalf("proxy.NewWithOptions: %v", err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/bot123:ABC/getMe")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Client must still see the COMPLETE body despite the tiny capture cap.
	if string(got) != fullBody {
		t.Errorf("client body = %q, want full %q (proxy must forward transparently)", got, fullBody)
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if !ex.Truncated {
		t.Error("Exchange.Truncated = false, want true (configured cap=8 < body size)")
	}
	if int64(len(ex.RespBody)) != 8 {
		t.Errorf("captured RespBody len = %d, want 8 (the configured cap)", len(ex.RespBody))
	}
}

// -----------------------------------------------------------------------
// Webhook ingress tests
// -----------------------------------------------------------------------

// webhookUpdateBody returns a minimal Telegram webhook delivery body: one
// Update carrying a message from senderID in chat chatID.
func webhookUpdateBody(chatID, senderID int64, isBot bool, text string, msgID int64) []byte {
	upd := map[string]any{
		"update_id": int64(900000001),
		"message": map[string]any{
			"message_id": msgID,
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
			"date":       1700000000,
			"from": map[string]any{
				"id":         senderID,
				"is_bot":     isBot,
				"first_name": "Sender",
			},
		},
	}
	b, _ := json.Marshal(upd)
	return b
}

// TestWebhookCapturesWithCorrectTokenHashAndForwards verifies that a webhook
// POST is captured with the TokenHash derived from the route's configured
// token (never the raw token) and is forwarded byte-transparently to the
// configured upstream bot target.
func TestWebhookCapturesWithCorrectTokenHashAndForwards(t *testing.T) {
	t.Parallel()

	const rawToken = "123456:WEBHOOK-secret-token-value"

	var gotBody []byte
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, "https://api.telegram.org")
	h, err := p.WebhookHandlerForRoute(upstream.URL+"/bot-handler", rawToken, "")
	if err != nil {
		t.Fatalf("WebhookHandlerForRoute: %v", err)
	}

	body := webhookUpdateBody(7777, 1111, true, "hello via webhook", 42)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/router", bytes.NewReader(body))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotPath != "/bot-handler" {
		t.Errorf("forwarded path = %q, want /bot-handler", gotPath)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("forwarded body mismatch:\n got %q\nwant %q", gotBody, body)
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if ex.Method != proxy.MethodWebhookIngress {
		t.Errorf("Method = %q, want %q", ex.Method, proxy.MethodWebhookIngress)
	}
	want := proxy.HashTokenExported(rawToken)
	if ex.TokenHash != want {
		t.Errorf("TokenHash = %q, want %q (derived from route token)", ex.TokenHash, want)
	}
	if ex.TokenHash == "" {
		t.Error("TokenHash must not be empty for a configured route")
	}
	if !bytes.Equal(ex.ReqBody, body) {
		t.Errorf("captured ReqBody mismatch:\n got %q\nwant %q", ex.ReqBody, body)
	}
	// The raw token must never appear in the captured exchange.
	if bytes.Contains(ex.ReqBody, []byte(rawToken)) {
		t.Error("raw token leaked into captured ReqBody")
	}
}

// TestWebhookSecretTokenEnforced verifies that, when a secret token is
// configured, a delivery with a missing/incorrect
// X-Telegram-Bot-Api-Secret-Token header is rejected with 401 and NOT
// forwarded, while a matching header is accepted.
func TestWebhookSecretTokenEnforced(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-shared-value"

	var forwarded int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, "https://api.telegram.org")
	h, err := p.WebhookHandlerForRoute(upstream.URL, "tok:abc", secret)
	if err != nil {
		t.Fatalf("WebhookHandlerForRoute: %v", err)
	}

	tests := []struct {
		name       string
		header     string
		setHeader  bool
		wantStatus int
		wantFwd    bool
	}{
		{"missing header", "", false, http.StatusUnauthorized, false},
		{"wrong header", "nope", true, http.StatusUnauthorized, false},
		{"correct header", secret, true, http.StatusOK, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forwarded = 0
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/webhook/x",
				bytes.NewReader(webhookUpdateBody(1, 2, false, "hi", 1)))
			if tc.setHeader {
				req.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.header)
			}
			h.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			gotFwd := forwarded > 0
			if gotFwd != tc.wantFwd {
				t.Errorf("forwarded = %v, want %v", gotFwd, tc.wantFwd)
			}
			if !tc.wantFwd && tc.wantStatus == http.StatusUnauthorized {
				if last := sink.last(); last != nil && last.Method == proxy.MethodWebhookIngress {
					// A rejected request must not produce a webhook exchange.
					t.Error("rejected request produced a captured exchange")
				}
			}
		})
	}
}

// TestWebhookBodyCapped verifies that an oversized webhook body is captured
// only up to the configured body cap (the existing capped-tap mechanism),
// while the full body is still forwarded transparently to the bot target.
func TestWebhookBodyCapped(t *testing.T) {
	t.Parallel()

	const capBytes = 256

	var fwdLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fwdLen = len(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	sink := &recordingSink{}
	p, err := proxy.NewWithOptions("https://api.telegram.org", sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)), capBytes)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	h, err := p.WebhookHandlerForRoute(upstream.URL, "tok:big", "")
	if err != nil {
		t.Fatalf("WebhookHandlerForRoute: %v", err)
	}

	big := bytes.Repeat([]byte("A"), capBytes*4)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/big", bytes.NewReader(big))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if fwdLen != len(big) {
		t.Errorf("forwarded %d bytes, want full %d (transparent forward)", fwdLen, len(big))
	}
	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if int64(len(ex.ReqBody)) != int64(capBytes) {
		t.Errorf("captured ReqBody = %d bytes, want capped at %d", len(ex.ReqBody), capBytes)
	}
}
