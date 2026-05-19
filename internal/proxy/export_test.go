// export_test.go exposes internal functions for use in the proxy_test package.
// This file is only compiled during `go test`.
package proxy

import (
	"io"
	"net/url"
)

// RedactURLExported is a test-only export of the unexported redactURL function.
// It is used by proxy_test to verify that bot tokens are redacted from URLs
// before they reach error logs, including when the token sits behind a path
// prefix.
func RedactURLExported(u *url.URL) string {
	return redactURL(u)
}

// TapBodyExported is a test-only export of the unexported tapBody function.
// It is used by proxy_test to verify body-close propagation and the
// truncation flag.
func TapBodyExported(rc io.ReadCloser, capBytes int64) ([]byte, io.ReadCloser, bool) {
	return tapBody(rc, capBytes)
}

// HashTokenExported is a test-only export of the unexported hashToken function.
// It is used by proxy_test to verify that a webhook exchange carries the correct
// TokenHash without needing to re-implement the hashing logic.
func HashTokenExported(token string) string {
	return hashToken(token)
}
