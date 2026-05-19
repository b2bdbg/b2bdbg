# syntax=docker/dockerfile:1.7

# ---------------------------------------------------------------------------
# Stage 1 — module cache (separate layer for faster rebuilds)
# ---------------------------------------------------------------------------
FROM golang:1.25-bookworm AS deps

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download -x

# ---------------------------------------------------------------------------
# Stage 2 — build the static binary
# ---------------------------------------------------------------------------
FROM deps AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build \
      -trimpath \
      -ldflags "-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.date=${BUILD_DATE}" \
      -o /bin/b2bdbg ./cmd/b2bdbg

# ---------------------------------------------------------------------------
# Stage 3 — distroless runtime (no shell, no libc, non-root uid 65532)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /bin/b2bdbg /b2bdbg

# Proxy + admin/metrics/health all share one port (see server.go).
EXPOSE 8080

# Built-in healthcheck using the binary itself — no shell or wget needed in
# the distroless image.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/b2bdbg", "healthcheck"]

USER nonroot:nonroot

ENTRYPOINT ["/b2bdbg"]
