// Package telemetry sets up OpenTelemetry tracing (OTLP/gRPC exporter) and the
// Prometheus metrics registry for b2bdbg.
//
// # OTel tracer
//
// Use NewTracerProvider to build a *sdktrace.TracerProvider backed by an
// OTLP/gRPC exporter (when OTelEndpoint is set) or a stdout exporter (when
// the endpoint is empty, so the binary works with zero external dependencies).
//
// # Prometheus metrics
//
// NewSink creates a dedicated prometheus.Registry and registers the following
// collectors:
//
//   - b2b_messages_total  (counter, label: method)
//   - b2b_loops_total     (counter)
//   - b2b_spam_ratio      (gauge)
//   - b2b_token_cost_usd  (counter)
//
// # Span attributes (see the span schema in docs/span-schema.md)
//
//   - telegram.bot.from   string  token-hash of the calling bot
//   - telegram.bot.to     string  token-hash of the recipient bot
//   - b2b.bot.to.resolution string why telegram.bot.to is/ isn't set; one of
//     the closed enum: "resolved" (bot.to set, recipient is a known bot from a
//     prior getMe), "unknown_getme_not_seen" (numeric chat_id that could be a
//     bot but no getMe for it has been seen — registry miss),
//     "non_bot_chat" (recipient is a human/group/channel — not applicable), or
//     "string_chat_id" (chat_id was an @username/channel string, so no bot
//     hash can be derived). telegram.bot.to is non-empty iff this is
//     "resolved"; it is never faked for any other value.
//   - telegram.method     string  Bot API method name
//   - telegram.chat.id    int64   Telegram chat identifier
//   - telegram.msg.id     int64   Telegram message identifier
//   - telegram.text.len   int64   byte length of the message text
//   - b2b.loop.depth      int     0 = no loop; > 0 = cycle depth
//   - b2b.tokens.est      int64   estimated token count (chars/4)
//   - b2b.cost.usd.est    float64 estimated cost (tokens/1000 × rate)
//   - b2b.capture.truncated bool  true iff the body tap actually hit the
//     configurable body cap for this exchange (so parsed fields may be
//     incomplete); always emitted. The full body is still forwarded.
//   - telegram.update.id  int64   inbound Update.update_id (getUpdates element
//     / webhook); emitted only when > 0, omitted for outbound sends.
//   - telegram.media.kind string  photo|document|video for media messages
//     (consistent with the media key); omitted for text-only messages.
package telemetry
