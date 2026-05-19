# Span schema and Prometheus metrics

All values are copied directly from the source code in `internal/telemetry/`.

## OTel spans

One span is produced per Bot API call that passes through the proxy and can be
correlated (requires a non-zero `chat_id` and a `from` bot token hash).

**Span name** = Bot API method name (e.g. `sendMessage`, `getUpdates`).
Falls back to `"unknown"` if the method cannot be parsed.

**Tracer name** = `"b2bdbg"`.

**Trace grouping:** all events within the same `(chat_id, thread_id)` pair
share one `TraceID`. The first event starts the trace; subsequent events
are children of the first span.

### Span attributes

Source: `internal/telemetry/sink.go`, `OnEvent()`.

| Attribute key | OTel type | Go value | Description |
|---|---|---|---|
| `telegram.bot.from` | `string` | `pm.FromBot` | 16-char SHA-256 hex prefix of the calling bot's token |
| `telegram.bot.to` | `string` | `pm.ToBot` | 16-char SHA-256 hex prefix of the recipient bot's token. Non-empty **iff** `b2b.bot.to.resolution` is `"resolved"`; never faked otherwise |
| `b2b.bot.to.resolution` | `string` | `string(pm.Resolution)` | Closed enum explaining why `telegram.bot.to` is/isn't set (see below) |
| `telegram.method` | `string` | `pm.Method` | Bot API method name |
| `telegram.chat.id` | `int64` | `pm.ChatID` | Telegram chat identifier |
| `telegram.msg.id` | `int64` | `pm.MessageID` | Telegram message identifier |
| `telegram.text.len` | `int64` | `int64(pm.TextLen)` | Byte length of the message text |
| `b2b.loop.depth` | `int` | `ev.LoopDepth` | 0 = no loop detected; positive = cycle depth (distance back to the matching prior entry) |
| `b2b.tokens.est` | `int64` | `tokens` | Estimated token count: `max(1, len(text)/4)` |
| `b2b.cost.usd.est` | `float64` | `cost` | Estimated cost in USD: `tokens/1000 × SinkConfig.CostPerThousandTokens`; 0 when rate is 0 |
| `b2b.capture.truncated` | `bool` | `pm.Truncated` | `true` when the parsed fields may be incomplete: either the proxy body tap hit the body cap for this exchange, **or** the response used a `Content-Encoding` the proxy cannot decode (e.g. `br`) or an incomplete compressed stream, so parsing was skipped/partial (`proxy.Exchange.Truncated`, `proxy.decodeCapturedBody`). Always emitted (`false` when the body was captured and decoded in full). The full body is still forwarded transparently, byte-exact. |
| `telegram.update.id` | `int64` | `pm.UpdateID` | Telegram `Update.update_id` for **inbound** events (a `getUpdates` batch element or a webhook delivery). Emitted **only when `> 0`**; omitted for outbound sends (which carry no `update_id`) and never faked as `0`. |
| `telegram.media.kind` | `string` | `pm.MediaKind` | Coarse media classification — one of `photo`, `document`, `video` — derived alongside `MediaKey` (`capture.mediaKindOf`, same recognition order as `mediaKeyOf`). Emitted **only when non-empty**; absent for text-only messages and media kinds not modelled. |

### `b2b.bot.to.resolution` values

Source: `internal/telemetry/sink.go`, `OnEvent()` (the value is `pm.Resolution`).
This is a closed enum; `telegram.bot.to` is non-empty **only** for `"resolved"`:

| Value | Meaning |
|---|---|
| `resolved` | `telegram.bot.to` is set: the recipient is a known bot seen via a prior `getMe` |
| `unknown_getme_not_seen` | Numeric `chat_id` that could be a bot, but no `getMe` for it has been seen (registry miss); `telegram.bot.to` is empty |
| `non_bot_chat` | Recipient is a human / group / channel — not applicable; `telegram.bot.to` is empty |
| `string_chat_id` | `chat_id` was an `@username`/channel string, so no bot hash can be derived; `telegram.bot.to` is empty |

### Span status

When `pm.StatusCode >= 400` the span status is set to `codes.Error` with
description `"upstream error"`.

### Token / cost estimation

The estimation is a deliberate approximation only; do not use for billing.

```
tokens = max(1, len(text_bytes) / 4)
cost   = tokens / 1000.0 × CostPerThousandTokens
```

`CostPerThousandTokens` defaults to `0.0`. Set `cost_per_k_tokens` in
`config.yaml` (or the `B2BD_COST_PER_K_TOKENS` environment variable) to a
non-zero value if you want the cost counter to accumulate.

### Body capture cap

The number of request/response body bytes parsed for the attributes above is
bounded by the configurable body cap (`config.Config.BodyCapBytes`, YAML
`body_cap_bytes`, env `B2BD_BODY_CAP_BYTES`). It defaults to
`proxy.DefaultBodyCapBytes` = `1 << 20` (1 MiB) and must be `>= 1024` bytes
(`config.MinBodyCapBytes`; `Validate` rejects `0`, negative, and too-small
values). When a body exceeds the cap, `b2b.capture.truncated` is set `true`.
A `gzip`/`deflate` response is decompressed (tapped copy only, bounded again
by the cap) before parsing; an undecodable encoding such as `br` also sets
`b2b.capture.truncated` rather than producing a silent empty span. The
complete body is always still forwarded transparently, byte-exact, regardless
of the cap or any decompress-for-parse step.

---

## Prometheus metrics

Source: `internal/telemetry/metrics.go`, `newMetrics()`.

All metrics are registered into an isolated `prometheus.Registry` (not the
default global registry). They are served at `http://<host>:8080/metrics`.

### b2b_messages_total

```
# HELP b2b_messages_total Total correlated capture events, partitioned by Bot API method (one per parsed update; a batched getUpdates yields N).
# TYPE b2b_messages_total counter
```

| Field | Value |
|---|---|
| Type | `CounterVec` |
| Labels | `method` (Bot API method name; `"unknown"` if unparsed) |
| Increment | +1 per correlated `OnEvent` (one per parsed update). A batched `getUpdates` of N updates → +N; a call with no correlated event (e.g. `getMe`) → +0. Not upstream HTTP-call count |

### b2b_loops_total

```
# HELP b2b_loops_total Total number of bot↔bot message loops detected.
# TYPE b2b_loops_total counter
```

| Field | Value |
|---|---|
| Type | `Counter` |
| Labels | none |
| Increment | +1 when `ev.LoopDepth > 0` |

### b2b_spam_ratio

```
# HELP b2b_spam_ratio Rolling ratio of looping messages to total messages (0.0–1.0).
# TYPE b2b_spam_ratio gauge
```

| Field | Value |
|---|---|
| Type | `Gauge` |
| Labels | none |
| Value | `b2b_loops_total / b2b_messages_total` (recomputed on every event) |
| Notes | Ratio is for the current process run only; resets on restart |

### b2b_token_cost_usd

```
# HELP b2b_token_cost_usd Estimated cumulative LLM token cost in USD (heuristic: chars/4 tokens × rate).
# TYPE b2b_token_cost_usd counter
```

| Field | Value |
|---|---|
| Type | `Counter` |
| Labels | none |
| Increment | `cost` per event (0 when rate is 0) |

---

## Standard Go / process metrics

The following standard collectors are also registered into the same isolated
registry by `cmd/b2bdbg/main.go`:

- `go_*` — Go runtime metrics (`collectors.NewGoCollector`)
- `process_*` — process-level metrics (`collectors.NewProcessCollector`)
