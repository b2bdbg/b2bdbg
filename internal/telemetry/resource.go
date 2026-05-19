package telemetry

import (
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// buildResource returns the OTel resource that is attached to every span
// produced by this service.
//
// version is the build's semantic version (set by ldflags from main.version
// and threaded in via [NewTracerProvider]); passing it through here means
// release telemetry carries the actual tag in service.version instead of a
// hardcoded placeholder. An empty version falls back to "dev" so unit tests
// and library callers that did not wire a version still get a sensible value.
func buildResource(version string) *resource.Resource {
	if version == "" {
		version = "dev"
	}
	r, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("b2bdbg"),
			semconv.ServiceVersionKey.String(version),
		),
	)
	return r
}
