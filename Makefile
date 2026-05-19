## b2bdbg — Makefile
##
## Targets:
##   build            compile the binary to bin/b2bdbg
##   test             run all tests with -race
##   test-telegram    OPT-IN / LOCAL ONLY: real two-bot end-to-end test against
##                    the REAL Telegram Bot API. Excluded from `test` via the
##                    `telegram_e2e` build tag. Needs real BotFather tokens via
##                    B2BD_E2E_BOT_TOKEN_A, B2BD_E2E_BOT_TOKEN_B and
##                    B2BD_E2E_CHAT_ID; makes live, rate-limited API calls. NOT
##                    run in CI. Skips cleanly if the env vars are unset.
##   lint             run golangci-lint
##   run              build and run with --help
##   tidy             go mod tidy
##   docker-build     build the Docker image (tag: b2bdbg:<VERSION>)
##   compose-up       docker compose up --build --detach
##   compose-down     docker compose down (removes containers, keeps volumes)
##   compose-demo     run the offline demo bots through the composed b2bdbg
##                    (traces in compose Jaeger, metrics in compose Grafana)
##   compose-demo-traffic
##                    like compose-demo but the bots repeat the scripted
##                    conversation continuously (B2BD_EXAMPLE_DURATION) so the
##                    Grafana dashboard rate panels stay populated — use this
##                    before taking dashboard screenshots
##   compose-smoke    BEFORE RELEASE: bring up the default stack, drive demo
##                    traffic, assert Jaeger trace + Prometheus metric, tear
##                    down. Exits non-zero on any unmet check.
##   release-snapshot goreleaser --snapshot --clean (local dry-run, no push)
##   release-check    BEFORE TAGGING: go test -race, lint, compose-smoke and a
##                    goreleaser snapshot, in order, failing fast. Requires
##                    Docker (compose-smoke shells docker). Human pre-release
##                    gate; does not push or tag.

MODULE  := github.com/b2bdbg/b2bdbg
BIN     := bin/b2bdbg
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test test-telegram lint run tidy docker-build compose-up compose-down compose-demo compose-demo-traffic compose-smoke release-snapshot release-check example example-test

all: build

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/b2bdbg

test:
	go test -race -timeout 60s ./...

# OPT-IN / LOCAL ONLY. Real two-bot end-to-end test against the REAL Telegram
# Bot API. It is fenced behind the `telegram_e2e` build tag so it is excluded
# from `make test` / `go test ./...` entirely. It needs THREE env vars with
# real, secret credentials (never commit them):
#   B2BD_E2E_BOT_TOKEN_A   a real BotFather token (bot A)
#   B2BD_E2E_BOT_TOKEN_B   a real BotFather token (bot B)
#   B2BD_E2E_CHAT_ID       a chat both bots are in, or bot B's numeric id
# Optional: B2BD_E2E_TIMEOUT (Go duration, default 60s).
# It makes live, rate-limited Telegram API calls and is NOT for CI. With the
# tag set but the env vars unset it SKIPs cleanly (no failure, no API call).
# See docs/e2e-testing.md.
test-telegram:
	go test -tags telegram_e2e -race -run TestTelegramE2E ./examples/support-team/

lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "installing golangci-lint v2.12.2 (.golangci.yml is v2 schema)..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2; \
	fi
	golangci-lint run ./...

run: build
	$(BIN) --help

tidy:
	go mod tidy

## ── Example ──────────────────────────────────────────────────────────────

# Run the offline support-team demo (no real Telegram, no internet required).
# Spans are written to stdout; set B2BD_OTEL_ENDPOINT to forward to Jaeger.
example:
	go run ./examples/support-team/

# Run only the integration test for the example (faster than the full suite).
example-test:
	go test -race -timeout 120s -v ./examples/support-team/...

## ── Docker / Compose ─────────────────────────────────────────────────────

docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(DATE) \
		-t b2bdbg:$(VERSION) \
		-t b2bdbg:latest \
		.

compose-up:
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(DATE) \
	docker compose up --build --detach

compose-down:
	docker compose down

# Run the REAL offline compose demo: brings up the full stack plus the
# support-team-demo bots, which route bot↔bot traffic through the composed
# b2bdbg so spans appear in the composed Jaeger and metrics in the composed
# Grafana. B2BD_TELEGRAM_BASE_URL points the composed b2bdbg at the demo's
# in-process mock Telegram (offline; no real tokens, no internet).
#   Jaeger UI:  http://localhost:16686  (service: b2bdbg)
#   Grafana:    http://localhost:3000   (admin / admin; b2b dashboard)
compose-demo:
	B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081 \
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(DATE) \
	docker compose --profile demo up --build

# Like compose-demo, but the demo bots repeat the scripted conversation
# continuously for B2BD_EXAMPLE_DURATION (default 15m) with a short pause
# between repeats, so the Grafana dashboard's rate panels keep showing data.
# Open Grafana (http://localhost:3000, admin/admin) → "bot-to-bot debugger
# (b2bdbg)" dashboard while this runs, then capture screenshots. Stop with
# Ctrl-C / `make compose-down` when done. Does NOT change compose-demo or
# compose-smoke (those leave these env vars unset → one-shot behaviour).
#   Override window:  make compose-demo-traffic DEMO_DURATION=30m DEMO_INTERVAL=3s
DEMO_DURATION ?= 15m
DEMO_INTERVAL ?= 5s
compose-demo-traffic:
	B2BD_TELEGRAM_BASE_URL=http://support-team-demo:8081 \
	B2BD_EXAMPLE_DURATION=$(DEMO_DURATION) \
	B2BD_EXAMPLE_INTERVAL=$(DEMO_INTERVAL) \
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(DATE) \
	docker compose --profile demo up --build

# Pre-release gate. Brings up the DEFAULT compose stack (no demo profile),
# waits for b2bdbg /healthz, drives the demo profile to generate real bot<->bot
# traffic, then asserts the trace landed in Jaeger and b2b_messages_total is
# scraped by Prometheus. Tears the whole project down on exit. FAILS (non-zero)
# on any unmet check. Run this before tagging a release.
compose-smoke:
	bash scripts/compose-smoke.sh

## ── Release ──────────────────────────────────────────────────────────────

release-snapshot:
	@if ! command -v goreleaser >/dev/null 2>&1; then \
		echo "installing goreleaser..."; \
		go install github.com/goreleaser/goreleaser/v2@latest; \
	fi
	goreleaser release --snapshot --clean

# Human "before tagging a release" gate. Runs, in order, failing fast:
#   1. go test ./... -race
#   2. make lint
#   3. make compose-smoke   (requires Docker — it shells `docker compose`)
#   4. goreleaser release --snapshot --clean
# Does NOT push or tag; it only verifies the tree is releasable. Run it, then
# `git tag vX.Y.Z` by hand. Needs Docker available for step 3.
release-check:
	go test ./... -race
	$(MAKE) lint
	$(MAKE) compose-smoke
	@if ! command -v goreleaser >/dev/null 2>&1; then \
		echo "installing goreleaser..."; \
		go install github.com/goreleaser/goreleaser/v2@latest; \
	fi
	goreleaser release --snapshot --clean
