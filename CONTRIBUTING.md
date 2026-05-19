# Contributing

## Build and test

The build requires Go 1.25+ (matches `go.mod`, the CI workflow, and the
`golang:1.25-bookworm` builder image) and works with no internet access once
the module cache is warm.

```bash
make build        # compiles bin/b2bdbg
make test         # go test -race -timeout 60s ./...
make lint         # golangci-lint run ./...
make example      # offline support-team demo
make example-test # integration test for the example
```

### Constrained environments

If `/tmp` or the default GOCACHE is on a small or read-only partition, set:

```bash
export GOTMPDIR=/path/to/writable/tmp
export GOCACHE=/path/to/writable/cache
export GOMODCACHE=/path/to/writable/modcache
```

Add `GOWORK=off` when the parent directory contains a `go.work` file that
does not include this module:

```bash
GOWORK=off go test ./...
```

### Linting

`golangci-lint` v2.x is required. The project config is `.golangci.yml`.
The enabled linters are: `govet`, `staticcheck`, `errcheck`, `revive`, and
the `gofumpt` formatter. Install once:

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

## Pull request expectations

- Keep each PR focused on one logical change.
- All tests must pass including `-race`: `make test`.
- Lint must be clean: `make lint`.
- New behaviour must have unit tests. Table-driven tests preferred.
- Test fixtures go in `testdata/`.
- No new global state. Constructor injection only.
- Wrap errors with `%w`; use `log/slog` for structured logging.

## Commit style

Loosely conventional:

```
feat: add webhook ingress path
fix: bound loop-window memory on high-churn convs
docs: add span-schema reference
test: add race test for traceMap
refactor: replace channel-mutex with sync.Mutex
chore: bump golangci-lint to v2.12
```

Subject line ≤ 72 characters. Body only when the "why" is not obvious from
the diff.

## Adding a new Bot API method parser

1. Add a `parseXxx` function in `internal/capture/parse.go` following the
   pattern of `parseSendMessage`.
2. Add the method name to the `switch` in `parseExchange`.
3. Add a fixture JSON response to `testdata/` and a table row in
   `internal/capture/capture_test.go`.

## License

By contributing you agree your changes will be licensed under Apache-2.0.
