---
name: Trace missing a bot-to-bot edge
about: A hop between two bots is not showing telegram.bot.to / the A→B edge
labels: missing-edge
---

Before filing: most missing edges are explained by
[docs/limitations.md](../../docs/limitations.md#bot-to-only-resolves-after-getme)
— `telegram.bot.to` only resolves after the recipient bot's `getMe` has passed
through b2bdbg.

## b2b.bot.to.resolution value

<!-- The value of the b2b.bot.to.resolution span attribute on the affected
     span. One of: resolved | unknown_getme_not_seen | non_bot_chat |
     string_chat_id -->

## Was getMe called through the proxy?

<!-- Did the *recipient* bot make a getMe call with its Bot API base URL
     pointed at b2bdbg? yes / no / not sure -->

## Method involved

<!-- The Bot API method on the span (sendMessage, forwardMessage, copyMessage,
     getUpdates, webhookIngress, …) and span name. -->

## /debug/registry output (if enabled)

<!-- If you ran with debug_endpoints / B2BD_DEBUG_ENDPOINTS=true, paste:
       curl -fsS http://localhost:8080/debug/registry
     It lists id↔token-hash mappings + count (never raw tokens). Leave blank
     if debug endpoints are disabled. -->

```
```

## Span / log excerpt

<!-- The relevant span (with attributes) and any b2bdbg log lines. Redact real
     bot tokens. -->

```
```

## Additional context

<!-- Long-poll or webhook ingress? Chat type (DM / group / channel)?
     String @username chat_id? -->
