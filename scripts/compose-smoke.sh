#!/usr/bin/env bash
# compose-smoke.sh — pre-release gate for the b2bdbg compose stack.
#
# Invoked by `make compose-smoke`. Do not run this against a stack you care
# about: it brings the compose project up and tears it all down (incl. volumes)
# on exit.
#
# What it does, in order:
#   1. `docker compose up -d`  (DEFAULT stack only — no demo profile)
#   2. Poll b2bdbg /healthz until 200 "ok" or timeout
#   3. Run the demo profile to generate real bot<->bot traffic
#      (B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081, like compose-demo)
#   4. Assert Jaeger knows the `b2bdbg` service AND has >=1 trace for it
#   5. Assert the Prometheus `b2bdbg` target is up AND b2b_messages_total exists
#   6. Tear everything down (trap on EXIT)
#
# Any unmet check exits non-zero. Pure POSIX-ish bash + curl + jq.

set -euo pipefail

# --- ports (must match .env.example defaults / docker-compose.yml) -----------
B2BD_PORT="${B2BD_PORT:-8080}"
JAEGER_UI_PORT="${JAEGER_UI_PORT:-16686}"
PROMETHEUS_PORT="${PROMETHEUS_PORT:-9090}"

B2BD_URL="http://localhost:${B2BD_PORT}"
JAEGER_URL="http://localhost:${JAEGER_UI_PORT}"
PROM_URL="http://localhost:${PROMETHEUS_PORT}"

HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-90}"   # seconds to wait for b2bdbg /healthz
TRAFFIC_TIMEOUT="${TRAFFIC_TIMEOUT:-120}" # seconds for the demo run to finish
ASSERT_TIMEOUT="${ASSERT_TIMEOUT:-90}"   # seconds to wait for traces/metrics

log() { printf '[compose-smoke] %s\n' "$*" >&2; }
fail() { printf '[compose-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || fail "required tool not found: $1"; }
need docker
need curl
need jq

cleanup() {
  log "tearing down compose project (including demo profile + volumes)"
  docker compose --profile demo down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# --- 1. bring up the FULL stack with b2bdbg pointed at the mock upstream ----
#
# Critical: B2BD_TELEGRAM_BASE_URL must be set BEFORE the initial `up` so the
# b2bdbg container is created with the mock as its upstream from the start.
# (Setting it only on a later `up support-team-demo` worked by accident — it
# made compose recreate b2bdbg because its interpolated env changed — but is
# fragile: if b2bdbg's interpolated env happened to match, it would not be
# recreated and would still point at real Telegram. Bringing the full stack
# up under the demo profile in one shot makes the upstream deterministic.)
export B2BD_TELEGRAM_BASE_URL="http://support-team-demo:8081"
log "docker compose --profile demo up -d (full stack, b2bdbg upstream = ${B2BD_TELEGRAM_BASE_URL})"
docker compose --profile demo up -d --build

# --- 2. wait for b2bdbg /healthz ----------------------------------------------
log "waiting up to ${HEALTH_TIMEOUT}s for ${B2BD_URL}/healthz"
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until body="$(curl -fsS --max-time 5 "${B2BD_URL}/healthz" 2>/dev/null)" \
      && printf '%s' "$body" | grep -q "ok"; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    docker compose ps >&2 || true
    fail "b2bdbg /healthz did not return 200 \"ok\" within ${HEALTH_TIMEOUT}s"
  fi
  sleep 3
done
log "b2bdbg is healthy"

# --- 3. demo container is already up and generating traffic ------------------
# support-team-demo `restart: "no"` runs the deterministic scripted demo once
# (the support-team scenario) and exits 0. The wait loop below polls until
# it has exited (or the traffic timeout fires).
log "demo container is generating bot<->bot traffic via the composed b2bdbg"

log "waiting up to ${TRAFFIC_TIMEOUT}s for the demo run to complete"
deadline=$(( $(date +%s) + TRAFFIC_TIMEOUT ))
until [ "$(docker inspect -f '{{.State.Status}}' support-team-demo 2>/dev/null || echo missing)" = "exited" ]; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    docker compose logs support-team-demo >&2 || true
    fail "demo container did not finish within ${TRAFFIC_TIMEOUT}s"
  fi
  sleep 3
done
demo_rc="$(docker inspect -f '{{.State.ExitCode}}' support-team-demo 2>/dev/null || echo 1)"
[ "$demo_rc" = "0" ] || fail "demo container exited non-zero (code ${demo_rc})"
log "demo traffic generated"

# --- 4. assert Jaeger has the b2bdbg service + a trace -------------------------
log "asserting Jaeger knows service b2bdbg and has a trace"
deadline=$(( $(date +%s) + ASSERT_TIMEOUT ))
while :; do
  services="$(curl -fsS --max-time 5 "${JAEGER_URL}/api/services" 2>/dev/null || echo '{}')"
  if printf '%s' "$services" | jq -e '.data // [] | index("b2bdbg")' >/dev/null 2>&1; then
    traces="$(curl -fsS --max-time 5 "${JAEGER_URL}/api/traces?service=b2bdbg&limit=1" 2>/dev/null || echo '{}')"
    if printf '%s' "$traces" | jq -e '(.data // []) | length > 0' >/dev/null 2>&1; then
      log "Jaeger has >=1 trace for service b2bdbg"
      break
    fi
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "Jaeger did not show service b2bdbg with a trace within ${ASSERT_TIMEOUT}s"
  fi
  sleep 3
done

# --- 5. assert Prometheus target up + b2b_messages_total present -------------
log "asserting Prometheus b2bdbg target is up and b2b_messages_total exists"
deadline=$(( $(date +%s) + ASSERT_TIMEOUT ))
while :; do
  targets="$(curl -fsS --max-time 5 "${PROM_URL}/api/v1/targets" 2>/dev/null || echo '{}')"
  target_up="$(printf '%s' "$targets" \
    | jq -r '[.data.activeTargets[]? | select(.labels.job=="b2bdbg") | .health] | index("up") // empty' 2>/dev/null || true)"
  if [ -n "$target_up" ]; then
    q="$(curl -fsS --max-time 5 "${PROM_URL}/api/v1/query?query=b2b_messages_total" 2>/dev/null || echo '{}')"
    if printf '%s' "$q" | jq -e '.status=="success" and ((.data.result // []) | length > 0)' >/dev/null 2>&1; then
      log "Prometheus b2bdbg target is up and b2b_messages_total has samples"
      break
    fi
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "Prometheus b2bdbg target up + b2b_messages_total not satisfied within ${ASSERT_TIMEOUT}s"
  fi
  sleep 3
done

log "ALL CHECKS PASSED — stack is release-ready"
# cleanup runs via the EXIT trap
