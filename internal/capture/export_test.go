// export_test.go exposes internal capture functions for use in the
// capture_test (black-box) package. This file is only compiled during
// `go test`.
package capture

import "github.com/b2bdbg/b2bdbg/internal/proxy"

// ParseExchangeExported is a test-only export of the unexported parseExchange.
// It is used by the fuzz target to exercise the full single-exchange parse
// path with arbitrary bodies and content types.
func ParseExchangeExported(ex *proxy.Exchange, reg *BotRegistry) ParsedMessage {
	return parseExchange(ex, reg)
}

// ParseGetUpdatesAllExported is a test-only export of parseGetUpdatesAll, the
// batch getUpdates response parser.
func ParseGetUpdatesAllExported(ex *proxy.Exchange, reg *BotRegistry) []ParsedMessage {
	return parseGetUpdatesAll(ex, reg)
}

// ParseWebhookUpdateExported is a test-only export of parseWebhookUpdate, the
// inbound webhook body parser.
func ParseWebhookUpdateExported(ex *proxy.Exchange, reg *BotRegistry) []ParsedMessage {
	return parseWebhookUpdate(ex, reg)
}

// DecodeOutboundBodyExported is a test-only export of decodeOutboundBody, the
// wire-encoding-agnostic outbound request decoder (JSON / urlencoded /
// multipart).
func DecodeOutboundBodyExported(ex *proxy.Exchange) (any, bool) {
	f, ok := decodeOutboundBody(ex)
	return f, ok
}
