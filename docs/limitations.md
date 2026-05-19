# Limitations

Honest, specific scope boundaries. Every entry below is backed by a code
symbol so you can verify it yourself. Read this before you rely on b2bdbg for
anything load-bearing.

## Bot API HTTP only — MTProto is not intercepted

b2bdbg is a reverse proxy that parses the Bot API path
`/bot<TOKEN>/<method>` (`pkg/botapi.ParseMethod`) and forwards to an HTTP
upstream (`internal/proxy.Proxy.director`, default
`https://api.telegram.org` from `config.defaults`). It only sees traffic a
client sends to that base URL.

**MTProto clients are not intercepted.** Pyrogram, Telethon, TDLib and any
other MTProto userbot/library talk the binary MTProto protocol directly to
Telegram's data centres, not the HTTP Bot API, so nothing reaches b2bdbg and no
spans are produced for them. There is no MTProto support and none is planned in
this repo.

- Backed by: `pkg/botapi.ParseMethod`, `internal/proxy.Proxy.director`,
  `internal/config.defaults` (`TelegramBaseURL = https://api.telegram.org`).

## `telegram.bot.to` only resolves after a `getMe` {#bot-to-only-resolves-after-getme}

The receiving-bot hash (`telegram.bot.to`) is only populated when b2bdbg has
already learned that bot's numeric Telegram user ID. That mapping is learned
**only from a `getMe` response that passed through the proxy**
(`internal/capture.parseGetMe` → `BotRegistry.Register`); it is then looked up
by numeric chat_id (`BotRegistry.HashForID`). If the recipient bot never made a
`getMe` call through b2bdbg, b2bdbg cannot know which token-hash that chat_id
corresponds to, and `telegram.bot.to` is left empty — never faked.

Every message-bearing span carries `b2b.bot.to.resolution` (the closed
`capture.Resolution` enum) explaining exactly why. `telegram.bot.to` is
non-empty **iff** the value is `resolved`:

| `b2b.bot.to.resolution` | Enum constant | Meaning / what to do |
|---|---|---|
| `resolved` | `ResolutionResolved` | `telegram.bot.to` is set; recipient is a known bot (its `getMe` was seen). Nothing to do. |
| `unknown_getme_not_seen` | `ResolutionUnknownGetMeNotSeen` | Numeric **positive** chat_id that could be a bot, but no `getMe` for that id was observed (registry miss). **Actionable**: route the recipient bot's `getMe` through b2bdbg (point that bot at the proxy too). |
| `non_bot_chat` | `ResolutionNonBotChat` | Recipient is a human / group / channel (negative or non-bot id). Resolution not applicable. |
| `string_chat_id` | `ResolutionStringChatID` | chat_id was an `@username` / channel string, so no numeric id and no bot hash can be derived. The hop is still traced via a deterministic synthetic negative chat_id (`capture.syntheticChatID`). |

For inbound updates (long-poll / webhook) the edge is resolved only when the
message *sender* is a known bot (`capture.parseInboundUpdate` →
`classifyInboundResolution`); otherwise it is `non_bot_chat`.

- Backed by: `capture.parseGetMe`, `capture.BotRegistry.Register`,
  `capture.BotRegistry.HashForID`, `capture.Resolution` and its constants,
  `capture.classifyOutboundResolution`, `capture.classifyInboundResolution`,
  `capture.syntheticChatID`.

## Body capture is capped (default ~1 MiB, tunable)

b2bdbg buffers at most `config.Config.BodyCapBytes` bytes of each request and
response body for telemetry (`proxy.tapBody`). This is **configurable** via the
`body_cap_bytes` YAML key / `B2BD_BODY_CAP_BYTES` env var; it defaults to
`proxy.DefaultBodyCapBytes` = `1 << 20` (1 MiB) and must be `>= 1024` bytes
(`config.MinBodyCapBytes`; `config.Validate` rejects `0`, negative, and
too-small values with a clear error). **The full body is still forwarded
transparently** — the cap only bounds what is parsed for span attributes. For
payloads larger than the configured cap (huge inline keyboards, large
`getUpdates` batches, big media JSON), fields past the boundary may be missing
from the parsed message, so text length, chat_id, message_id or media key can
be partial or absent for that exchange. When this happens the
`b2b.capture.truncated` span attribute is set `true` so the incompleteness is
observable rather than silent (it is `true` **only** when the tapped copy was
actually cut). The proxy still streams the complete body upstream/downstream
byte-for-byte (`tapBody` chains the untapped tail via `io.MultiReader`).

- Backed by: `config.Config.BodyCapBytes`, `config.MinBodyCapBytes`,
  `proxy.DefaultBodyCapBytes` (`1 << 20`), `proxy.tapBody`,
  `proxy.Exchange.Truncated`.

## Compressed responses: gzip/deflate parsed, brotli skipped

Telegram and fronting CDNs may return responses with `Content-Encoding:
gzip` or `deflate`. Go's `http.Transport` only transparently decompresses
when **it** added the `Accept-Encoding` header; a bot framework that sets
its own `Accept-Encoding` makes the proxy receive a still-compressed body.
The capture layer decompresses **only the tapped copy** for parsing
(`proxy.decodeCapturedBody`), bounded a second time by the body cap so a
decompression bomb cannot exhaust memory. **The bytes forwarded to the
client are never altered** — the client always receives the byte-exact,
still-encoded upstream response.

`gzip` (incl. `x-gzip`) and `deflate` (zlib-wrapped and raw DEFLATE) are
decompressed and parsed. Any other encoding — notably **brotli (`br`)** —
is **not** decompressed: parsing for that exchange is skipped and
`b2b.capture.truncated` is set `true` so the gap is observable, not a
silent empty span. If the captured (capped) copy is itself an incomplete
compressed stream the decoder returns its best-effort prefix and likewise
flags `b2b.capture.truncated`.

- Backed by: `proxy.decodeCapturedBody`, `proxy.readBounded`,
  `proxy.Exchange.Truncated`, `compress/gzip`, `compress/flate`,
  `compress/zlib`.

## Token cost is a chars/4 *estimate*, not a bill

`b2b.tokens.est` and `b2b.cost.usd.est` (and the `b2b_token_cost_usd` counter)
are a deliberate heuristic: `tokens = max(1, len(text_bytes) / 4)`
(`telemetry.estimateTokens`), `cost = tokens / 1000 × CostPerThousandTokens`
(`telemetry.estimateCost`). The byte length is of the message text **or the
caption** for media (`capture.parseInboundUpdate` text/caption fallback). This
is not real tokenization, not your model's tokenizer, and not a billing
figure. The rate defaults to `0` (`telemetry.DefaultCostPerThousandTokens`,
`config.CostPerKTokens`), so cost is `0` until you set
`cost_per_k_tokens` / `B2BD_COST_PER_K_TOKENS`.

- Backed by: `telemetry.estimateTokens`, `telemetry.estimateCost`,
  `telemetry.DefaultCostPerThousandTokens`, `config.Config.CostPerKTokens`.

## Loop detection covers text + caption + media, not arbitrary equivalence

Loop detection hashes a `(from, to, FNV-64a hash of text)` tuple per event
over a bounded sliding window. The "text" includes the caption for media
messages, and media hand-offs are also keyed by a stable
`file_unique_id`/`file_id` (`capture.mediaKeyOf`, `ParsedMessage.MediaKey`).
It does **not** detect semantically-equivalent-but-not-byte-identical loops:
reworded text, edited messages whose content changed, structured-payload
equivalence, or two different media files with the same meaning are not
flagged. `firstMessage` does fold `edited_message` / `channel_post` into the
same path, but loop matching is still exact on the hashed text/media key, not
content equivalence.

- Backed by: `capture.textHash` (FNV-64a), `capture.mediaKeyOf`,
  `capture.ParsedMessage.MediaKey`, `capture.firstMessage`. (Window/TTL
  defaults — 20 entries, 5 min — are documented in
  [docs/architecture.md](architecture.md#loop-detection).)

## `b2b_spam_ratio` is process-run totals, not a sliding window

`b2b_spam_ratio` is recomputed every event as
`totalLoops / totalMessages` using two process-lifetime atomic counters
(`telemetry.Sink.totalLoops`, `telemetry.Sink.totalMessages`), set via
`Sink.OnEvent`. It is **cumulative since process start**, not a rolling
rate; it resets to zero on restart and a long-running clean process will
trend toward 0 even during a burst of loops.

- Backed by: `telemetry.Sink.totalLoops`, `telemetry.Sink.totalMessages`,
  `telemetry.Sink.OnEvent` (`s.m.spamRatio.Set(loops/total)`).

## Conversation correlation key is `(chat_id, thread_id)`

All activity within the same Telegram chat and optional message thread is
grouped into one conversation and one trace, keyed by `(chat_id, thread_id)`
(see [docs/architecture.md](architecture.md#correlation-key)). Cross-chat
hand-offs are *not* automatically stitched into one trace: if bot A in chat X
triggers bot B which writes to chat Y, those are two conversations / two
traces. `thread_id` is taken from `message_thread_id`
(`capture.ParsedMessage.ThreadID`); a forum topic change therefore starts a
distinct conversation.

- Backed by: `capture.ParsedMessage.ThreadID`,
  `capture.decodeOutbound*` (`message_thread_id` extraction),
  architecture.md correlation-key section.

## Webhook secret-token is optional

For webhook ingress routes, `secret_token` /
`config.WebhookRoute.SecretToken` is **optional**. When empty, b2bdbg performs
**no** authentication on inbound `/webhook/<label>` deliveries — anyone who can
reach the port can inject updates that get forwarded to your bot target. The
check, when configured, is an exact match on the
`X-Telegram-Bot-Api-Secret-Token` header (`webhookProxy.ServeHTTP`, 401 on
mismatch). Set a secret and run b2bdbg on a trusted network.

- Backed by: `config.WebhookRoute.SecretToken`, `proxy.webhookProxy.ServeHTTP`
  (`if wh.secretToken != ""` … `http.StatusUnauthorized`).

## Replay / retention / policy / firewall / hosted are NOT in this repo

This OSS repo is capture + telemetry only. Trace replay, retention/storage,
policy/firewall/rate-limiting and hosted multi-tenant operation are **not
implemented here**. They are a separate, planned commercial product with no
code in this tree and no link until it is real. Long-polling **and** webhook
ingestion both remain free in this OSS core. (Confirmed: there is no replay,
retention, firewall or policy package — `internal/` is `capture`, `config`,
`proxy`, `server`, `telemetry` only.)

- Backed by: README "Supported / not supported"; absence of any such symbol in
  `internal/`.

## Other scope notes (verified against code)

- **No persistence.** Conversations live only in an in-memory LRU with TTL
  eviction; nothing is written to disk. The OTel trace-context index
  (`telemetry.traceMap`) is evicted in lock-step via `Sink.OnEvict`. Restart =
  clean slate.
- **Synchronous capture.** `proxy.Sink.Record` is called on the proxy response
  path and `capture.Listener.OnEvent` runs inside it
  (`telemetry.Sink.OnEvent`); a slow listener slows responses. Listeners are
  documented as required to be fast/non-blocking.
- **`getMe` produces no span.** `getMe` carries no chat_id, so
  `capture.parseGetMe` only populates the registry; the event is dropped by the
  Store (ChatID == 0) and you will not see a `getMe` span — only its
  side-effect on later `telegram.bot.to` resolution.
- **`/debug/registry` is opt-in and exposes hashes only.** Registered only when
  `config.DebugEndpoints` is true (`server.registerRoutes`,
  `cmd/b2bdbg` wiring). It returns id↔hash + count
  (`capture.BotRegistry.Snapshot`, `server.handleDebugRegistry`) — never raw
  tokens — and 404s with zero overhead when disabled. Bind to loopback/trusted
  interface when enabled.
- **`telegram.update.id` is inbound-only.** It is emitted only for `getUpdates`
  batch elements and webhook deliveries and only when `> 0`; outbound sends
  carry no `update_id` so the attribute is omitted, never faked as `0`
  (`capture.parseInboundUpdate` → `ParsedMessage.UpdateID`,
  `telemetry.Sink.OnEvent`).
- **`telegram.media.kind` is coarse and media-only.** It is one of `photo` /
  `document` / `video`, derived by `capture.mediaKindOf` using the same
  recognition order as `mediaKeyOf`, and is omitted for text-only messages and
  for media kinds not modelled (audio, voice, sticker, etc. — these may still
  carry a `MediaKey` but no `kind`).
- **`/metrics` may be a stub.** When no telemetry sink is configured the
  endpoint returns a plain "metrics disabled" body
  (`server.handleMetricsPlaceholder`). The composed stack always wires the
  Prometheus handler, so this only affects custom deployments.
