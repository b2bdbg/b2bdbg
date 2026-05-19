package proxy_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gzipBytes returns b compressed with gzip.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// flateBytes returns b compressed with raw DEFLATE (Content-Encoding: deflate).
func flateBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate new: %v", err)
	}
	if _, err := fw.Write(b); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

// TestGzipEncodedResponseStillParsed is the regression test for the silent
// capture-empty bug: a real bot framework sends its own Accept-Encoding: gzip,
// so Go's transport does NOT auto-decompress and the proxy taps still-compressed
// bytes. Before the fix the capture copy is gzip and JSON parsing yields nothing
// (Method captured but RespBody is unparseable). The client must always receive
// the byte-exact compressed upstream response (transparency is non-negotiable).
func TestGzipEncodedResponseStillParsed(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"ok":true,"result":[{"update_id":7,"message":{"message_id":42,` +
		`"chat":{"id":999,"type":"private"},"text":"hello from gzip","date":1700000000}}]}`)
	compressed := gzipBytes(t, plain)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	// A real bot client sets its own Accept-Encoding: gzip. This is the trigger:
	// Go's Transport only auto-decompresses when IT added the header itself.
	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/getUpdates",
		strings.NewReader(`{"timeout":1}`),
	)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	bodyBack, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// --- Transparency: the client must receive the byte-exact compressed body.
	// Go's client auto-decompresses only when IT set Accept-Encoding; here the
	// test set it explicitly so resp.Body is the raw upstream bytes.
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding header not forwarded; got %q",
			resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(bodyBack, compressed) {
		t.Fatalf("client did not receive byte-exact compressed body: got %d bytes, want %d",
			len(bodyBack), len(compressed))
	}

	// --- Capture must still parse the decompressed JSON.
	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if ex.Method != "getUpdates" {
		t.Errorf("Method = %q, want getUpdates", ex.Method)
	}

	// The captured RespBody must be JSON-parseable (decompressed). Before the
	// fix this fails: ex.RespBody holds gzip bytes and json.Unmarshal errors,
	// so the whole capture/telemetry pipeline silently produces zero messages.
	var parsed struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				MessageID int64  `json:"message_id"`
				Text      string `json:"text"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(ex.RespBody, &parsed); err != nil {
		t.Fatalf("captured RespBody not JSON-parseable (still gzip?): %v\nbytes: %q",
			err, ex.RespBody)
	}
	if !parsed.OK || len(parsed.Result) != 1 {
		t.Fatalf("decompressed JSON missing data: ok=%v result=%d",
			parsed.OK, len(parsed.Result))
	}
	if parsed.Result[0].Message.Text != "hello from gzip" {
		t.Errorf("text = %q, want %q", parsed.Result[0].Message.Text, "hello from gzip")
	}
}

// TestDeflateEncodedResponseStillParsed mirrors the gzip case for
// Content-Encoding: deflate (raw DEFLATE, as RFC 7230 / zlib clients use).
func TestDeflateEncodedResponseStillParsed(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"ok":true,"result":{"message_id":5,` +
		`"chat":{"id":12,"type":"private"},"text":"deflate body"}}`)
	compressed := flateBytes(t, plain)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "deflate")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/sendMessage",
		strings.NewReader(`{"chat_id":12,"text":"deflate body"}`),
	)
	req.Header.Set("Accept-Encoding", "deflate")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	bodyBack, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !bytes.Equal(bodyBack, compressed) {
		t.Fatalf("client did not receive byte-exact deflate body")
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	var parsed struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(ex.RespBody, &parsed); err != nil {
		t.Fatalf("captured RespBody not JSON-parseable (still deflate?): %v", err)
	}
	if !parsed.OK || parsed.Result.Text != "deflate body" {
		t.Fatalf("decompressed JSON wrong: %+v", parsed)
	}
}

// TestUnknownEncodingSkipsParsingGracefully verifies an unsupported encoding
// (e.g. br) does NOT crash, does NOT corrupt forwarding, and marks the captured
// exchange as Truncated so the un-parseable body is observable, not silent.
func TestUnknownEncodingSkipsParsingGracefully(t *testing.T) {
	t.Parallel()

	// Arbitrary bytes labelled as brotli; the proxy cannot decode br and must
	// neither panic nor attempt to parse garbage as JSON.
	fakeBr := []byte{0x1b, 0xff, 0x00, 0x42, 0x10, 0x99}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fakeBr)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/getMe",
		nil,
	)
	req.Header.Set("Accept-Encoding", "br")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	bodyBack, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (forwarding must not break)", resp.StatusCode)
	}
	if !bytes.Equal(bodyBack, fakeBr) {
		t.Fatalf("client did not receive byte-exact br body")
	}

	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if !ex.Truncated {
		t.Error("unknown encoding should mark Truncated=true so it is observable")
	}
}

// TestUncompressedResponseUnchanged is the negative control: a plain
// uncompressed response must behave exactly as before (no false truncation,
// fully parseable).
func TestUncompressedResponseUnchanged(t *testing.T) {
	t.Parallel()

	plain := `{"ok":true,"result":{"message_id":1,"chat":{"id":3,"type":"private"},"text":"plain"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, plain)
	}))
	defer upstream.Close()

	p, sink := newTestProxy(t, upstream.URL)
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/bot1:T/sendMessage",
		strings.NewReader(`{"chat_id":3,"text":"plain"}`),
	)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	bodyBack, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if string(bodyBack) != plain {
		t.Fatalf("body mismatch: got %q", bodyBack)
	}
	ex := sink.last()
	if ex == nil {
		t.Fatal("no exchange recorded")
	}
	if ex.Truncated {
		t.Error("uncompressed small body must not be marked Truncated")
	}
	if !bytes.Equal(ex.RespBody, []byte(plain)) {
		t.Errorf("RespBody = %q, want %q", ex.RespBody, plain)
	}
}
