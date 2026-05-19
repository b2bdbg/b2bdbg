# Architecture

## Component overview

```
  Bot processes (any language / framework)
      │
      │  HTTP  (base URL = http://<host>:8080)
      │  Path: /{bot<TOKEN>}/{method}
      │
      ▼
  ┌────────────────────────────────────────────────────┐
  │  internal/server  (net/http, :8080)                │
  │                                                    │
  │  GET  /healthz    → 200 "ok"                       │
  │  GET  /metrics    → Prometheus text format         │
  │  ALL  /           → proxy.Proxy handler            │
  └────────────────┬───────────────────────────────────┘
                   │
                   ▼
  ┌────────────────────────────────────────────────────┐
  │  internal/proxy  (Proxy)                           │
  │                                                    │
  │  • net/http/httputil.ReverseProxy to upstream      │
  │    (default: https://api.telegram.org)             │
  │  • Extracts token from path, immediately hashes    │
  │    it (SHA-256, 16 hex chars = TokenHash)          │
  │  • Taps request + response bodies into []byte      │
  │    (capped at 1 MiB; oversized bodies still        │
  │    forwarded transparently)                        │
  │  • Builds proxy.Exchange per call                  │
  │  • Calls proxy.Sink.Record(ctx, *Exchange)         │
  └────────────────┬───────────────────────────────────┘
                   │  proxy.Sink interface
                   ▼
  ┌────────────────────────────────────────────────────┐
  │  internal/capture  (Store)                         │
  │                                                    │
  │  implements proxy.Sink                             │
  │                                                    │
  │  Record():                                         │
  │    1. parseExchange → ParsedMessage                │
  │       – sendMessage: req body chat_id/text,        │
  │         resp body message_id                       │
  │       – getUpdates: resp body first message        │
  │       – generic: best-effort req body chat_id      │
  │    2. Correlation key = (chat_id, thread_id)       │
  │       → Conversation (LRU, max 10 000 entries,     │
  │         TTL 30 min)                                │
  │    3. Loop detection: sliding window of 20         │
  │       recent (from, to, FNV-64a text hash) tuples, │
  │       TTL 5 min. Match → LoopDepth > 0             │
  │    4. Append Event to Conversation                 │
  │    5. Call capture.Listener.OnEvent for each       │
  │       registered listener                          │
  └────────────────┬───────────────────────────────────┘
                   │  capture.Listener interface
                   ▼
  ┌────────────────────────────────────────────────────┐
  │  internal/telemetry  (Sink)                        │
  │                                                    │
  │  implements capture.Listener                       │
  │                                                    │
  │  OnEvent():                                        │
  │    OTel:                                           │
  │      – traceMap: per-conversation context index    │
  │        (sync.Mutex on heap; safe to copy)          │
  │      – First event → new trace; subsequent events  │
  │        → child spans under same TraceID            │
  │      – Span name = Bot API method name             │
  │      – Set 9 span attributes (see span-schema.md)  │
  │      – Export via OTLP/gRPC or stdout exporter     │
  │    Prometheus:                                     │
  │      – b2b_messages_total{method} Inc()            │
  │      – b2b_loops_total Inc() when LoopDepth > 0    │
  │      – b2b_spam_ratio Set(loops/total)             │
  │      – b2b_token_cost_usd Add(cost) when rate > 0  │
  └────────────────┬───────────────────────────────────┘
                   │
          ┌────────┴────────┐
          ▼                 ▼
       Jaeger           Prometheus
    (OTLP/gRPC)       (scrapes /metrics)
          │                 │
          └────────┬────────┘
                   ▼
               Grafana
```

## Data contracts

### proxy.Exchange

The struct that crosses the proxy→capture boundary. Key fields:

| Field | Type | Notes |
|---|---|---|
| `Timestamp` | `time.Time` | UTC time the upstream response was received |
| `TokenHash` | `string` | 16-char SHA-256 hex prefix of the raw token |
| `Method` | `string` | Bot API method name parsed from path |
| `ReqBody` | `[]byte` | Up to 1 MiB of the outgoing request body |
| `RespBody` | `[]byte` | Up to 1 MiB of the upstream response body |
| `StatusCode` | `int` | HTTP status from the upstream |
| `Duration` | `time.Duration` | Round-trip time |

Raw tokens are never placed in an Exchange field.

### proxy.Sink

```go
type Sink interface {
    Record(ctx context.Context, ex *Exchange)
}
```

Implemented by `capture.Store`. Called once per proxied request, synchronously
on the proxy response path. Must not block for significant time.

### capture.Listener

```go
type Listener interface {
    OnEvent(ctx context.Context, conv *Conversation, ev Event)
}
```

Implemented by `telemetry.Sink`. Called inside `Store.Record` after the
event has been correlated and appended. The Store lock is NOT held during
the call. Listeners must be fast and non-blocking.

## Correlation key

Conversations are keyed by `(chat_id, thread_id)` — all bot activity within
the same Telegram chat and optional message thread is grouped into one
conversation and one OTel trace. The set of participating bots is tracked
inside the Conversation itself and grows as new bots appear.

## Loop detection

For each event the store maintains a per-conversation sliding window of
`(from, to, FNV-64a text hash)` tuples. Default window: 20 entries, 5-minute
TTL. A new event that matches a prior tuple is flagged with a positive
`LoopDepth` (distance back to the matching entry) and causes
`b2b_loops_total` to increment.

## Memory bounds

The conversation store is an LRU cache backed by a doubly-linked list and a
hash map (keyed by `ConvKey`). Defaults:

- Max 10 000 concurrent conversations.
- TTL eviction: 30 minutes after the last event.

Conversations evicted from the store emit an eviction callback that the
telemetry Sink handles via `OnEvict`, which deletes the corresponding
`traceMap` entry. This means `traceMap` is strictly bounded by the store's
LRU/TTL policy — it never accumulates stale entries.
