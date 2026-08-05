// Package tracing configures OpenTelemetry.
//
// See docs/design/12-observability-and-audit.md section 3.
//
// Tracing is a side channel: a collector being unreachable drops spans and
// must never affect a transfer. When disabled — or when the exporter cannot be
// built — Init returns a no-op provider and a nil-safe shutdown, so no caller
// needs to guard against tracing being off.
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
)

// Config selects the exporter endpoint and sampling.
type Config struct {
	Enabled     bool
	Endpoint    string  // host:port for OTLP/gRPC
	SampleRatio float64 // 0..1; errors are always sampled by the caller
}

// Shutdown flushes pending spans. Always non-nil, so callers can defer it
// unconditionally.
type Shutdown func(context.Context) error

// Init installs a global tracer provider and the W3C propagator.
//
// The propagator is installed even when tracing is disabled: trace context must
// still be carried Coordinator -> worker -> registry so that enabling tracing
// later does not require touching call sites.
func Init(ctx context.Context, cfg Config, component string) (trace.TracerProvider, Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if !cfg.Enabled || cfg.Endpoint == "" {
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return tp, func(context.Context) error { return nil }, nil
	}

	info := version.Get(component)
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("softwaregateway-"+component),
		semconv.ServiceVersion(info.Version),
		attribute.String("service.commit", info.Commit),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("build otel resource: %w", err)
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build otlp exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// ParentBased so a sampled transfer yields a complete trace across
		// processes rather than a scattering of unrelated spans.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)

	return tp, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}
