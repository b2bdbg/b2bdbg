package capture_test

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/capture"
	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// gzipSeed compresses b with gzip for use as a fuzz seed (a body with a gzip
// header but that is then parsed as JSON — exercises the "gzip header but
// non-JSON content" robustness path inside the parser).
func gzipSeed(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// FuzzParseExchange drives the Bot API body-parsing entrypoint with arbitrary
// method names, content types and request/response bodies. It is the fuzz
// target required by the launch-hardening plan.
//
// Invariant under test: parsing ANY input — valid, truncated, non-UTF8, empty,
// deeply nested, wrong content type, gzip-header-but-plain-body — must never
// panic and must respect the parser's bounds (no unbounded allocation; the
// parser only ever reads the bytes it is handed, which the proxy already caps).
// The body cap is the proxy's responsibility (verified in proxy tests); this
// target additionally guards the cap by passing only bounded inputs and
// asserting the parser does not amplify them.
//
// Run as a normal unit test it executes the seed corpus deterministically
// (good for CI). To fuzz for real:
//
//	GOWORK=off go test ./internal/capture/ -run=^$ -fuzz=FuzzParseExchange -fuzztime=60s
func FuzzParseExchange(f *testing.F) {
	// --- Valid fixtures (JSON, urlencoded, multipart) -----------------------
	jsonReq := []byte(`{"chat_id":12345,"text":"hello","message_thread_id":7}`)
	jsonResp := []byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":12345,"type":"private"},"text":"hello"}}`)
	getUpdatesResp := []byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,` +
		`"chat":{"id":9,"type":"private"},"text":"hi","from":{"id":5,"is_bot":true}}}]}`)
	urlencoded := []byte(`chat_id=999&text=urlencoded+body&message_thread_id=3`)
	multipartBody := "--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"chat_id\"\r\n\r\n555\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"text\"\r\n\r\nmultipart text\r\n" +
		"--BOUNDARY--\r\n"

	seeds := []struct {
		method string
		ct     string
		req    []byte
		resp   []byte
	}{
		// Valid, well-formed.
		{"sendMessage", "application/json", jsonReq, jsonResp},
		{"sendMessage", "application/x-www-form-urlencoded", urlencoded, jsonResp},
		{"sendPhoto", "multipart/form-data; boundary=BOUNDARY", []byte(multipartBody), jsonResp},
		{"getUpdates", "", nil, getUpdatesResp},
		{"getMe", "", nil, []byte(`{"ok":true,"result":{"id":777,"is_bot":true}}`)},
		{
			proxy.MethodWebhookIngress, "application/json",
			[]byte(`{"update_id":9,"message":{"message_id":3,"chat":{"id":4,"type":"private"},"text":"wh"}}`), nil,
		},
		{
			"forwardMessage", "application/json",
			[]byte(`{"chat_id":1,"from_chat_id":2}`), jsonResp,
		},
		// Adversarial / edge.
		{"sendMessage", "application/json", []byte(`{"chat_id":1,"text":`), nil}, // truncated JSON
		{"sendMessage", "application/json", []byte{0xff, 0xfe, 0x00, 0x01}, nil}, // non-UTF8
		{"sendMessage", "application/json", []byte{}, []byte{}},                  // empty
		{
			"sendMessage", "application/json",
			bytes.Repeat([]byte(`{"a":`), 2000), nil,
		}, // deeply nested-ish
		{"sendMessage", "text/plain", jsonReq, jsonResp},            // wrong content type
		{"getUpdates", "application/json", nil, gzipSeed(jsonResp)}, // gzip header bytes as body
		{"sendMessage", "multipart/form-data; boundary=", []byte("garbage"), nil},
		{"", "", nil, nil}, // empty method
		{"unknownMethod", "application/json", []byte(`{"chat_id":"@chan","text":"x"}`), nil},
		{
			"getUpdates", "application/json", nil,
			[]byte(`{"ok":true,"result":[` + string(bytes.Repeat([]byte(`{},`), 500)) + `{}]}`),
		},
	}
	for _, s := range seeds {
		f.Add(s.method, s.ct, s.req, s.resp)
	}

	f.Fuzz(func(t *testing.T, method, ct string, reqBody, respBody []byte) {
		// Bound the inputs the way the proxy body cap would; the fuzzer can
		// otherwise generate arbitrarily large slices and we only care that
		// the parser does not AMPLIFY a bounded input.
		const bodyCap = 1 << 20
		if len(reqBody) > bodyCap {
			reqBody = reqBody[:bodyCap]
		}
		if len(respBody) > bodyCap {
			respBody = respBody[:bodyCap]
		}

		ex := &proxy.Exchange{
			Timestamp:      time.Unix(0, 0).UTC(),
			TokenHash:      "fuzzbot",
			Method:         method,
			ReqBody:        reqBody,
			ReqContentType: ct,
			RespBody:       respBody,
			StatusCode:     200,
			Duration:       time.Millisecond,
		}

		reg := capture.NewBotRegistry(0)

		// Invariant 1: parseExchange never panics for any input.
		pm := capture.ParseExchangeExported(ex, reg)

		// Invariant 2: derived lengths are coherent (no negative / overflow,
		// TextLen matches Text). A mismatch would indicate corruption.
		if pm.TextLen != len(pm.Text) {
			t.Fatalf("TextLen %d != len(Text) %d", pm.TextLen, len(pm.Text))
		}
		if pm.TextLen < 0 {
			t.Fatalf("negative TextLen %d", pm.TextLen)
		}
		// Invariant 3: parsed text is never larger than the (bounded) inputs
		// it could have come from — the parser must not allocate text out of
		// nothing (bounds / no-amplification check).
		if len(pm.Text) > len(reqBody)+len(respBody) {
			t.Fatalf("parsed Text len %d exceeds input %d (amplification)",
				len(pm.Text), len(reqBody)+len(respBody))
		}

		// The batch / webhook entrypoints must also never panic for any input.
		_ = capture.ParseGetUpdatesAllExported(ex, reg)
		_ = capture.ParseWebhookUpdateExported(ex, reg)
		_, _ = capture.DecodeOutboundBodyExported(ex)
	})
}
