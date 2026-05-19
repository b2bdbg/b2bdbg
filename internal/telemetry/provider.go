package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/b2bdbg/b2bdbg/internal/config"
)

// NewTracerProvider constructs an OTel [sdktrace.TracerProvider].
//
//   - If cfg.OTelEndpoint is non-empty, an OTLP/gRPC exporter is created
//     targeting that endpoint. The connection is attempted lazily.
//   - Otherwise a stdout exporter is used (human-readable; no external deps).
//
// version is recorded as the OTel resource attribute service.version, so
// release telemetry carries the real build tag from main.version. Callers
// without a meaningful version may pass "" and "dev" is recorded instead.
//
// The caller is responsible for calling Shutdown on the returned provider
// before process exit to flush buffered spans.
func NewTracerProvider(ctx context.Context, cfg config.Config, version string) (*sdktrace.TracerProvider, error) {
	var exporter sdktrace.SpanExporter
	var err error

	if cfg.OTelEndpoint != "" {
		exporter, err = otlptracegrpc.New(
			ctx,
			otlptracegrpc.WithEndpoint(cfg.OTelEndpoint),
			otlptracegrpc.WithInsecure(), // TLS handled at the collector level
		)
		if err != nil {
			return nil, fmt.Errorf("telemetry: create OTLP exporter: %w", err)
		}
	} else {
		// Fallback: pretty-print spans to stdout so the binary is usable with no
		// external services. This is still useful during development.
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("telemetry: create stdout exporter: %w", err)
		}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(buildResource(version)),
	)
	return tp, nil
}

// ShutdownTracerProvider flushes and stops tp, honouring the deadline in ctx.
// Errors from ForceFlush and Shutdown are both surfaced; only the first
// non-nil error is returned.
func ShutdownTracerProvider(ctx context.Context, tp *sdktrace.TracerProvider, logger *slog.Logger) error {
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := tp.ForceFlush(flushCtx); err != nil {
		logger.Warn("telemetry: tracer force-flush", slog.Any("error", err))
	}

	if err := tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: tracer shutdown: %w", err)
	}
	return nil
}
