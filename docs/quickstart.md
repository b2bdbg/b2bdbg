# Quickstart

## Prerequisites

- Docker and Docker Compose v2
- Ports 8080, 16686, 9090, 3000 free on the host

## 1. Start the full stack

```bash
cp .env.example .env   # adjust ports or passwords if needed
docker compose up -d
```

Wait until all services are healthy (usually < 30 s):

```bash
docker compose ps
```

All four services should show `healthy`:

| Container | Host URL | Purpose |
|---|---|---|
| `b2bdbg` | http://localhost:8080 | Proxy + `/metrics` + `/healthz` |
| `jaeger` | http://localhost:16686 | Trace UI |
| `prometheus` | http://localhost:9090 | Metrics scraper |
| `grafana` | http://localhost:3000 | Dashboard (admin / admin) |

## 2. Point a bot at the proxy

Change only the Bot API base URL. No other code changes.

### python-telegram-bot (v20+)

```python
from telegram.ext import ApplicationBuilder

app = (
    ApplicationBuilder()
    .token("YOUR_BOT_TOKEN")
    .base_url("http://localhost:8080/bot")
    .build()
)
```

### aiogram (v3)

```python
from aiogram import Bot
from aiogram.client.session.aiohttp import AiohttpSession

session = AiohttpSession(api="http://localhost:8080/bot{token}/{method}")
bot = Bot(token="YOUR_BOT_TOKEN", session=session)
```

### go-telegram-bot-api (v5)

```go
import tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

bot, err := tgbotapi.NewBotAPIWithClient(
    "YOUR_BOT_TOKEN",
    "http://localhost:8080/bot%s/%s",
    http.DefaultClient,
)
```

After wiring, all Bot API calls from your bot flow through b2bdbg. Spans appear
in Jaeger under service `b2bdbg`. Metrics appear in Prometheus / Grafana.

## 3. See the demo bots in the compose Jaeger/Grafana (offline)

To watch a real multi-bot conversation flow through the **composed** stack with
no real tokens and no internet, run the `demo` profile:

```bash
make compose-demo
```

This is `docker compose --profile demo up --build` with
`B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081`. It builds a small,
separate demo image (`Dockerfile.demo`) and starts the five support-team bots
in external-proxy mode: every bot↔bot Bot API call routes through the composed
`b2bdbg`, and the demo container serves an in-process mock Telegram on `:8081`
that the composed b2bdbg uses as its upstream.

Equivalent without Make:

```bash
B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081 \
  docker compose --profile demo up --build
```

The plain `docker compose up -d` does not build or start the demo (it is gated
behind the `demo` profile).

`make compose-demo` is a single ~6 s burst, so the Grafana **rate** panels
decay back to zero soon after. For dashboard screenshots, run sustained
traffic instead — it repeats the scripted conversation for ~15 minutes so
every panel stays populated (the dashboard defaults to a `now-15m` window with
5 s auto-refresh):

```bash
make compose-demo-traffic   # Ctrl-C to stop; make compose-down to tear down
```

`make compose-demo` and `make compose-smoke` are unaffected (they leave the
sustained-traffic env vars unset, so the demo still runs exactly once).

## 4. See traces in Jaeger

After running either your own bots (Step 2) or the compose demo (Step 3):

1. Open http://localhost:16686
2. Select service **b2bdbg**
3. Click **Find Traces**

Each conversation appears as one trace; each Bot API call is a child span. The
demo conversation includes one span with `b2b.loop.depth > 0` (a simulated
retry bug in the refund flow). Metrics appear in Prometheus
(http://localhost:9090) and the Grafana **b2b** dashboard
(http://localhost:3000, admin / admin): `b2b_messages_total` and
`b2b_loops_total` advance.

## 4a. Zero-setup, no-Docker demo (`make example`)

`make example` is a **separate, in-process** path. It runs the five bots, an
in-process b2bdbg proxy, and an in-memory mock Telegram entirely in one process —
no Docker involved:

```bash
make example
```

Spans are written to **stdout** as JSON (method names `sendMessage` /
`getUpdates`, including one with `b2b.loop.depth > 0`). This in-process proxy
keeps its own private Prometheus registry; it does **not** feed the composed
Jaeger/Prometheus/Grafana. Set `B2BD_OTEL_ENDPOINT` to forward those stdout
spans to an OTLP/gRPC collector instead.

To run only the integration test (also offline, race-clean):

```bash
make example-test
```

## 5. Logs

```bash
docker compose logs -f b2bdbg
```

Structured JSON on stdout. Set `B2BD_LOG_LEVEL=debug` in `.env` for verbose
output including every tap.

## 6. Graceful shutdown

```bash
docker compose down
```

b2bdbg drains in-flight requests for up to the configured `shutdown_timeout`
(default 15 s) before exiting.

## Configuration reference

See the [configuration table in README.md](../README.md#configuration) or
`config.example.yaml`.

## Ingestion modes

- **Long-polling** — bots call `getUpdates` through the proxy. Both the request
  and the upstream response are tapped.
- **Webhook ingress** — Telegram POSTs updates to `/webhook/<label>` on the
  proxy; b2bdbg captures each update and forwards it to the route's configured
  bot server. Webhook deliveries flow through the identical
  capture/telemetry pipeline as long-polling (same A→B edge, loop detection,
  conversation correlation, span and Prometheus metrics).

### Webhook config snippet

Add to `config.yaml` (or `config.example.yaml` as a template):

```yaml
webhook_routes:
  - label: "router-bot"             # → POST /webhook/router-bot
    token: "123456:AAAA..."         # used only to derive the token hash
    target: "http://localhost:4000" # the bot's own update handler
    secret_token: "shared-secret"   # optional; reject mismatched deliveries
```

Then register the webhook with Telegram, pointing at b2bdbg:

```bash
curl "https://api.telegram.org/bot<TOKEN>/setWebhook" \
  -d url="http://<b2bdbg-host>:8080/webhook/router-bot" \
  -d secret_token="shared-secret"
```

Keep the token out of the YAML file with the per-route env override
`B2BD_WEBHOOK_TOKEN_ROUTER_BOT` (and `B2BD_WEBHOOK_SECRET_ROUTER_BOT` for the
secret). The raw token is hashed on arrival and never logged; only the
URL-safe label appears in the path.

**Not supported:** trace replay/retention (roadmap) and MTProto clients
(Pyrogram, Telethon, TDLib). The proxy intercepts Bot API HTTP traffic only.
