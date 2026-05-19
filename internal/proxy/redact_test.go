// Regression test for the URL-redaction path that feeds upstream-error logs.
//
// The previous redactor only matched a token that appeared as the FIRST path
// segment (`/bot<TOKEN>/method`). A self-hosted Bot API upstream that sits
// behind a path prefix produces `/<prefix>/bot<TOKEN>/method` — the previous
// redactor missed that case and the raw token escaped into the structured
// "proxy: upstream error" log. The test asserts that the new positional-
// independent redactor strips the token wherever it lands in the path.
package proxy_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

func TestRedactURL(t *testing.T) {
	const token = "123456789:AAEbCdEfGhIjKlMnOpQrStUvWxYz0123456"
	cases := []struct {
		name string
		raw  string
	}{
		{"default-no-prefix", "https://api.telegram.org/bot" + token + "/sendMessage"},
		{"with-path-prefix", "https://self-hosted.example.com/prefix/bot" + token + "/sendMessage"},
		{"nested-prefix", "https://self-hosted.example.com/api/v1/bot" + token + "/getUpdates"},
		{"trailing-query-dropped", "https://api.telegram.org/bot" + token + "/sendMessage?weird=1"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			got := proxy.RedactURLExported(u)
			if strings.Contains(got, token) {
				t.Fatalf("redacted URL still contains raw token: %s", got)
			}
			// String()'s path encoding turns `<` `>` into %3C %3E, so accept
			// either the literal sentinel or its percent-encoded form. The
			// invariant we care about is "token absent, redaction marker
			// present"; the exact spelling is cosmetic.
			if !strings.Contains(strings.ToLower(got), "redacted") {
				t.Fatalf("redacted URL missing redaction marker: %s", got)
			}
		})
	}
}
