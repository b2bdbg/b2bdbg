# support-team example

An offline, fully reproducible demo of **bot-to-bot** traffic flowing through
the b2bdbg proxy.  No real Telegram account, no internet connection, no secrets.

## What it shows

Five deterministic bots run in a single process:

| Bot | Role |
|---|---|
| `router` | Receives a customer task, classifies it, dispatches to a specialist |
| `sales` | Handles sales enquiries |
| `order` | Handles order-status requests |
| `refund` | Handles refund requests, echoes the dollar amount |
| `human-approve` | Simulates a human-in-the-loop approval gate |

**Conversation flow**

```
customer task → router → specialist (sales / order / refund)
                            ↓ reply
              router ←──────┘
              (if refund > $100) → human-approve → router
              router → customer
```

**Loop scenario** – for refund tasks, `router` intentionally sends the same
message twice (simulating a retry bug).  The proxy's loop detector flags the
duplicate as `b2b.loop.depth > 0` and increments `b2b_loops_total`.

All Bot API calls travel through b2bdbg so every hop produces an OTel span with
the attributes from the span schema in `docs/span-schema.md`:

OTel span attributes:

```
telegram.bot.from   — SHA-256 hash of the sending bot's token (16 hex chars)
telegram.bot.to     — hash of the receiving bot
telegram.method     — "sendMessage" / "getUpdates" / …
telegram.chat.id    — numeric chat identifier
b2b.loop.depth      — > 0 when the proxy detected a loop
```

Prometheus metrics (scraped at `/metrics`):

```
b2b_messages_total  — counter, partitioned by method
b2b_loops_total     — counter
```

## Two ways to run

This example has two modes, selected by environment variables (read once in
`run()` in `main.go`):

| Mode | Trigger | Proxy | Telemetry sink |
|---|---|---|---|
| **In-process** (default) | no env | an in-process b2bdbg proxy it starts itself | stdout spans (or OTLP if `B2BD_OTEL_ENDPOINT` is set), private Prometheus registry |
| **External-proxy** | `B2BD_EXAMPLE_PROXY_BASE_URL` set | an already-running external b2bdbg | owned by that external b2bdbg |

In external-proxy mode the example creates **no** proxy or telemetry of its
own: it serves the in-process mock Telegram on a real TCP listener
(`B2BD_EXAMPLE_MOCK_ADDR`, default `:8081`) and routes every bot through the
external proxy. The compose `demo` profile uses this mode.

## Quick start — local, fully offline, no Docker (`make example`)

```bash
# From the repo root:
make example
```

The driver starts everything in **one process** — mock Telegram, an
in-process b2bdbg proxy, and the five bots — injects two scripted tasks, prints
span JSON to **stdout**, then exits.  Takes about one second. Nothing reaches a
composed Jaeger/Prometheus/Grafana in this mode.

To run only the integration tests (also offline, -race clean):

```bash
make example-test
# or
go test -race -timeout 120s ./examples/support-team/...
```

## Docker Compose — traces in the composed Jaeger + Grafana

From the repo root:

```bash
make compose-demo
```

This is `docker compose --profile demo up --build` with
`B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081`. It builds a small,
separate demo image (`Dockerfile.demo` — the main b2bdbg image is not bloated)
and starts the five bots in external-proxy mode:

- bots route through the composed `b2bdbg`
  (`B2BD_EXAMPLE_PROXY_BASE_URL=http://b2bdbg:8080`);
- this container serves the mock Telegram on `:8081`, which the composed b2bdbg
  uses as its upstream (`B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081`).

Open Jaeger at <http://localhost:16686> and search for service `b2bdbg`:
a multi-hop trace with spans from each bot in the support conversation.

Open Grafana at <http://localhost:3000> (admin / admin), b2b dashboard —
`b2b_messages_total` and `b2b_loops_total` have advanced.

> **`make compose-demo` is a single ~6 s burst.** The cumulative *stat* panels
> (Total messages proxied, Loop events detected, Estimated token cost) and the
> Spam ratio gauge will show data, but the per-second **rate** timeseries decay
> back to zero shortly after the burst, so a screenshot taken later looks empty.

### Sustained traffic for Grafana screenshots

To keep every panel — including the rate timeseries — populated, run the demo
in **sustained-traffic mode** instead of the one-shot:

```bash
make compose-demo-traffic
# optional window override:
make compose-demo-traffic DEMO_DURATION=30m DEMO_INTERVAL=3s
```

This repeats the scripted conversation continuously for `DEMO_DURATION`
(default 15m) with `DEMO_INTERVAL` (default 5s) between repeats — long enough
to open Grafana and capture screenshots while traffic is live.

While it runs, open the **bot-to-bot debugger (b2bdbg)** dashboard
(<http://localhost:3000>, admin / admin; default range `now-15m`, auto-refresh
`5s`). With live traffic you should see:

| Panel | What it shows |
|---|---|
| Messages / sec by method | non-zero `sendMessage` / `getUpdates` rate lines |
| Total messages proxied | a steadily climbing cumulative count |
| Loop events detected | a non-zero count (the refund task's duplicate send) |
| Spam ratio | the loop/message ratio gauge, in the green/orange band |
| Loop rate vs message rate | both rate lines non-zero |
| Estimated token cost | `$0.00` unless `B2BD_COST_PER_K_TOKENS` is set on b2bdbg |
| Token cost rate | flat at 0 unless a cost rate is configured |
| Traces — b2bdbg service | live multi-hop traces from the Jaeger datasource |

Stop it with `Ctrl-C`, then `make compose-down` to tear the stack down.
`make compose-demo` and `make compose-smoke` are unaffected: they leave the
sustained-traffic env vars unset, so the demo still runs exactly once.

The sustained-traffic knobs are opt-in environment variables read once in
`run()` (see `main.go`): `B2BD_EXAMPLE_REPEAT` (integer N, default `1` =
one-shot), `B2BD_EXAMPLE_INTERVAL` (delay between repeats), and
`B2BD_EXAMPLE_DURATION` (loop until this wall-clock duration elapses;
takes precedence over `B2BD_EXAMPLE_REPEAT`). With none set the demo behaves
exactly as before.

> **Note:** the demo uses fake tokens (`111111:router-fake-token`, etc.) and
> the built-in mock Telegram backend.  No real credentials are required and no
> traffic reaches api.telegram.org. The plain `docker compose up -d` does not
> build or start the demo — it is gated behind the `demo` profile.

## Architecture

In-process mode (`make example` / integration test):

```
┌──────────────────────────────────────────────────────┐
│  one process                                         │
│  mocktelegram  ←──  in-proc b2bdbg  ←──  bots          │
│  (in-memory        (capture +         (router,       │
│   queues)           telemetry→stdout)  sales, …)     │
└──────────────────────────────────────────────────────┘
```

External-proxy mode (`make compose-demo`):

```
  support-team-demo container          b2bdbg container
  ┌───────────────────────┐           ┌──────────────┐
  │ bots ───Bot API────────┼──────────▶│ b2bdbg proxy   │
  │ mocktelegram :8081 ◀────┼───────────┤ (capture +   │
  └───────────────────────┘  upstream  │  telemetry)  │
                                        └──────┬───────┘
                                     OTLP→jaeger / /metrics→prometheus
```

The mock Telegram server (`mocktelegram/`) is an `http.Handler` that
implements `getMe`, `getUpdates`, `sendMessage` (and no-op `setWebhook` /
`deleteWebhook`) entirely in memory.  It accepts
`application/x-www-form-urlencoded` requests (the format used by
`go-telegram-bot-api/v5`) and signals waiting `getUpdates` callers
immediately when a new message arrives.
