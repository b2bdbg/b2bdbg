# Real-Telegram end-to-end test (opt-in, local only)

b2bdbg's default test suite (`make test` / `go test ./...`) is fully offline: it
drives bots through an in-process mock Telegram. That proves the
capture/telemetry pipeline but never exercises b2bdbg against the **real**
Telegram Bot API.

`make test-telegram` is the one automated test that does. It is the automated
counterpart of the manual runbook kept locally under `.dev/`: it stands the
b2bdbg capture + telemetry pipeline up **in-process** with an in-memory span
recorder, points the proxy at the real `https://api.telegram.org`, constructs
two real bot clients through that proxy, and drives a real exchange.

It is **opt-in and local only**:

- It is fenced behind the `telegram_e2e` Go build tag, so it is **excluded
  from `go test ./...` / `make test` entirely** — the default suite is
  byte-unaffected.
- It is **not run in CI**: it needs real BotFather tokens and makes live,
  rate-limited API calls.
- Even with the build tag set, it **`t.Skip`s cleanly** when the env vars are
  unset — a tagged-but-token-less run is a SKIP, never a failure, and makes no
  network call.

## Prerequisites

1. Two bots from [@BotFather](https://t.me/BotFather) (`/newbot` twice). Call
   them bot **A** and bot **B**.
2. A chat both can use. Either:
   - Put A and B in the same group and use that group's id as the chat id; or
   - Use bot B's numeric user id as the chat id for a direct A→B send.
3. For the group case, disable BotFather privacy mode so A's message is
   visible to B's `getUpdates`: BotFather → `/setprivacy` → select the bot →
   **Disable**. (This mirrors the note in the local `.dev` runbook.)

Tokens are credentials. Never paste them into commits, issues, screenshots, or
this repo. b2bdbg hashes tokens (SHA-256, first 16 hex) and never logs or exports
the raw value, and the test asserts no raw token appears in any recorded span —
but the token still lives in your shell env. Use a scratch shell and `unset`
when done.

## Environment contract

| Variable | Required | Meaning |
|---|---|---|
| `B2BD_E2E_BOT_TOKEN_A` | yes | Real BotFather token, bot A (the sender). |
| `B2BD_E2E_BOT_TOKEN_B` | yes | Real BotFather token, bot B (the receiver/poller). |
| `B2BD_E2E_CHAT_ID` | yes | Integer chat id both bots are in, or bot B's numeric id. |
| `B2BD_E2E_TIMEOUT` | no | Overall deadline, Go duration. Default `60s`. |

If any required variable is empty the test skips with:
`set B2BD_E2E_BOT_TOKEN_A/B and B2BD_E2E_CHAT_ID to run the real-Telegram e2e test`.

## Running it

```bash
export B2BD_E2E_BOT_TOKEN_A='111111:AAA...'
export B2BD_E2E_BOT_TOKEN_B='222222:BBB...'
export B2BD_E2E_CHAT_ID='123456789'
make test-telegram
# equivalently:
# go test -tags telegram_e2e -race -run TestTelegramE2E ./examples/support-team/
unset B2BD_E2E_BOT_TOKEN_A B2BD_E2E_BOT_TOKEN_B B2BD_E2E_CHAT_ID
```

You can keep these in a local, gitignored `.env.e2e` for convenience
(`.env.*` is already gitignored — do not commit it, and do not commit a real
one to the repo).

## What it does

1. In-memory `tracetest.SpanRecorder` (no Jaeger container), a real
   `telemetry.Sink`, and `capture.NewStoreWithRegistry` (so the bot-to-bot
   recipient can resolve once both `getMe`s are seen).
2. An in-process b2bdbg proxy on loopback whose upstream is the real
   `https://api.telegram.org`.
3. Two real `go-telegram-bot-api/v5` clients pointed at the local proxy. Each
   client issues `getMe` through the proxy on construction, populating the
   registry.
4. Bot A `sendMessage`s a unique marker through the proxy to the configured
   chat; bot B long-polls `getUpdates` through the proxy (bounded deadline, no
   fixed sleeps) and optionally observes it.

## What it asserts (tolerant — the real API is non-deterministic)

- At least one span has `telegram.method` in `{getMe, sendMessage,
  getUpdates}`.
- The `sendMessage` span has a non-empty `telegram.bot.from`.
- Wherever `b2b.bot.to.resolution` is present it is one of the closed enum
  values, and `telegram.bot.to` is non-empty **iff** the resolution is
  `resolved` (it does not require `resolved`, since that depends on chat
  topology).
- Real Telegram responses were parsed coherently: there are method-bearing
  spans and no tiny `getMe`/`sendMessage` response is flagged
  `b2b.capture.truncated`. This is the **only validation of the proxy's
  gzip/deflate decode-for-capture path against responses real Telegram
  actually returns** (the offline suite only ever sees the mock).
- No raw token value appears in any recorded span name or attribute — only the
  16-hex SHA-256 token-hash.

It deliberately does **not** assert exact span counts or ordering, and a
non-fatal "bot B never saw the message" (privacy mode / membership) still
passes because the `getMe`+`sendMessage` spans were still produced through the
real upstream.
