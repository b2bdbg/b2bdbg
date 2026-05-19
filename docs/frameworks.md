# Framework setup

b2bdbg is a transparent reverse proxy in front of the Telegram Bot API. The only
integration step is to point your bot's **Bot API base URL** at b2bdbg instead of
`https://api.telegram.org`. No other code changes.

## The base-URL shape b2bdbg expects

b2bdbg parses the request path with `pkg/botapi.ParseMethod`: the path must be

```
/bot<TOKEN>/<method>
```

i.e. the literal string `bot`, immediately followed by the raw token (no slash
between `bot` and the token), then a single `/`, then the Bot API method name.
This is exactly the path layout `https://api.telegram.org` uses, so every
mainstream client library produces it once you override the host/base URL —
the snippets below just show the *correct form of that override per library*.

Default b2bdbg address in the compose stack: `http://localhost:8080`
(host port `B2BD_PORT`, see `.env.example`).

> File-API URLs (`/file/bot<TOKEN>/<path>`, used for downloading files) are a
> different path shape; b2bdbg only correlates Bot API method calls. Pointing the
> file base URL at b2bdbg is harmless but produces no spans.

---

## python-telegram-bot (v20+)

`ApplicationBuilder.base_url(...)` is concatenated by PTB as
`f"{base_url}{token}/{method}"`, so the base URL must end with `bot`.

```python
from telegram.ext import ApplicationBuilder

app = (
    ApplicationBuilder()
    .token("YOUR_BOT_TOKEN")
    .base_url("http://localhost:8080/bot")
    .build()
)
```

- **Long-poll vs webhook**: `app.run_polling()` works with no extra config.
  For webhooks, run your bot's webhook server normally and register the webhook
  through b2bdbg's webhook ingress (see [Webhook ingress](#webhook-ingress)).
- Docs: <https://docs.python-telegram-bot.org/en/stable/telegram.ext.applicationbuilder.html>

## aiogram (v3)

aiogram v3 takes a base-URL **template** with `{token}` and `{method}`
placeholders via `TelegramAPIServer` / the `api=` argument on
`AiohttpSession`.

```python
from aiogram import Bot
from aiogram.client.session.aiohttp import AiohttpSession
from aiogram.client.telegram import TelegramAPIServer

session = AiohttpSession(
    api=TelegramAPIServer(
        base="http://localhost:8080/bot{token}/{method}",
        file="http://localhost:8080/file/bot{token}/{path}",
    )
)
bot = Bot(token="YOUR_BOT_TOKEN", session=session)
```

The shorthand `AiohttpSession(api="http://localhost:8080/bot{token}/{method}")`
also works in current aiogram v3 (string is parsed into a `TelegramAPIServer`).

- **Long-poll vs webhook**: `Dispatcher.start_polling(bot)` needs no extra
  config. For webhooks use b2bdbg's webhook ingress.
- Docs: <https://docs.aiogram.dev/en/v3.0.0/api/session.html>

## grammY (Node)

grammY configures the base URL via the client option **`apiRoot`** (default
`https://api.telegram.org`). grammY appends `/bot<token>/<method>` itself, so
pass only scheme + host.

```js
import { Bot } from "grammy";

const bot = new Bot("YOUR_BOT_TOKEN", {
  client: { apiRoot: "http://localhost:8080" },
});
```

- **Long-poll vs webhook**: `bot.start()` (long-poll) needs no extra config.
  For webhooks use b2bdbg's webhook ingress and grammY's `webhookCallback`.
- Docs: <https://grammy.dev/ref/core/apiclientoptions#apiRoot>

## Telegraf (Node)

Telegraf takes the API root via the constructor option
**`telegram.apiRoot`** (scheme + host only; Telegraf appends
`/bot<token>/<method>`).

```js
const { Telegraf } = require("telegraf");

const bot = new Telegraf("YOUR_BOT_TOKEN", {
  telegram: { apiRoot: "http://localhost:8080" },
});
```

- **Long-poll vs webhook**: `bot.launch()` (long-poll) needs no extra config.
  For webhooks use b2bdbg's webhook ingress and Telegraf's webhook callback.
- Docs: <https://telegraf.js.org/interfaces/Telegraf.Options.html>

## go-telegram-bot-api (v5)

This library takes a full **endpoint format string** with two `%s`
placeholders (token, method) via `NewBotAPIWithClient`. Use the
`/bot%s/%s` form so the resulting path matches what b2bdbg parses.

```go
import (
    "net/http"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

bot, err := tgbotapi.NewBotAPIWithClient(
    "YOUR_BOT_TOKEN",
    "http://localhost:8080/bot%s/%s",
    http.DefaultClient,
)
```

- **Long-poll vs webhook**: `bot.GetUpdatesChan(...)` (long-poll) needs no
  extra config. For webhooks use b2bdbg's webhook ingress.
- Docs: <https://pkg.go.dev/github.com/go-telegram-bot-api/telegram-bot-api/v5#NewBotAPIWithClient>

---

## Webhook ingress

For webhook-driven bots, b2bdbg receives the Telegram delivery itself and
forwards it to your bot. Configure a route in `config.yaml`:

```yaml
webhook_routes:
  - label: "router-bot"             # mounted at /webhook/router-bot
    token: "123456:AAAA..."         # used only to derive the token hash
    target: "http://localhost:4000" # your bot's own update handler
    secret_token: "shared-secret"   # optional; rejected on mismatch (401)
```

Then register the webhook with Telegram pointing at b2bdbg:

```bash
curl "https://api.telegram.org/bot<TOKEN>/setWebhook" \
  -d url="http://<b2bdbg-host>:8080/webhook/router-bot" \
  -d secret_token="shared-secret"
```

`secret_token` is optional; when set, b2bdbg rejects deliveries whose
`X-Telegram-Bot-Api-Secret-Token` header does not match. See
[docs/quickstart.md](quickstart.md#webhook-config-snippet) for the per-route
env overrides that keep the token out of YAML.

---

## Verify it is working

1. **Health**: `curl -fsS http://localhost:8080/healthz` → prints `ok`.
2. **Generate traffic**: run your bot (or `make compose-demo` for the offline
   demo bots).
3. **Trace in Jaeger**: open <http://localhost:16686>, select service **b2bdbg**,
   click **Find Traces**. Each conversation is one trace; each Bot API call is
   a span named after the method.
4. **Metrics**: open Prometheus <http://localhost:9090> and query
   `b2b_messages_total`, or the Grafana **b2b** dashboard
   (<http://localhost:3000>, admin / admin).

If you see no `telegram.bot.to` on spans, that is expected until a `getMe`
for the receiving bot has passed through b2bdbg — see
[docs/limitations.md](limitations.md#bot-to-only-resolves-after-getme).
