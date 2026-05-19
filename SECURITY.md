# Security policy

## Token handling

b2bdbg handles raw Telegram bot tokens only at ingress. The token is extracted
from the request path (`/bot<TOKEN>/<method>`), immediately hashed with
SHA-256, and truncated to 16 hex characters. Only this hash (`TokenHash`)
is held in memory, passed to the capture layer, emitted in log lines, and
included in OTel span attributes. b2bdbg never logs the raw token, never writes
it to the trace store, and never forwards it to a telemetry backend. (For
webhook routes the raw token is configured by you and therefore lives at rest
in your YAML/environment — operator-controlled, like any service credential;
b2bdbg reads it, hashes it, and does not persist it itself.)

Tokens do appear in network traffic between your bot process and b2bdbg (just as
they would with the real Telegram API), so you should run b2bdbg on
localhost or a private network, not exposed to the internet.

## Supported versions

The latest release on the `main` branch is the only supported version.

## Reporting a vulnerability

Please do not open a public GitHub issue for security vulnerabilities.

Use **GitHub private vulnerability reporting**:
[github.com/b2bdbg/b2bdbg/security/advisories/new](https://github.com/b2bdbg/b2bdbg/security/advisories/new)
(Repository → **Security** → **Report a vulnerability**). Include a
description, reproduction steps, and potential impact.

We aim to acknowledge within 2 business days and provide an initial assessment
within 5 business days. We will coordinate a fix and a disclosure timeline with
you before any public announcement.

## Scope

In scope: anything in this repository that could expose bot tokens, allow
request tampering, enable data exfiltration, or produce incorrect telemetry
that could mislead operators.

Out of scope: issues in upstream dependencies (report those to the relevant
project), issues in your own Telegram bots, and issues that require
compromising the host machine.
