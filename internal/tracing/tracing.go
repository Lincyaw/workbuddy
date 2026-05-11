// Package tracing wires OpenTelemetry tracing for workbuddy components.
//
// Behavior:
//   - If OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is unset,
//     Init installs a no-op TracerProvider so Tracer() callers stay cheap.
//   - Transport is chosen by OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" (default) or
//     "http/protobuf" / "http". All other OTEL_* env vars are honored by the
//     upstream exporter (endpoint, headers, insecure, timeout, etc.).
//   - service.name defaults to "workbuddy-<role>" but is overridden by
//     OTEL_SERVICE_NAME when set.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// InstrumentationName is the tracer name used for all workbuddy-emitted spans.
const InstrumentationName = "github.com/Lincyaw/workbuddy"

// ShutdownFunc flushes pending spans and tears down the provider.
type ShutdownFunc func(context.Context) error

// noopShutdown is returned when tracing is disabled.
func noopShutdown(context.Context) error { return nil }

var (
	initMu      sync.Mutex
	initialized atomic.Bool
)

// Init initializes the global TracerProvider and propagator for the given role
// (e.g. "coordinator", "worker", "serve", "supervisor"). It returns a shutdown
// func that flushes pending spans; callers should defer it.
//
// When no OTLP endpoint is configured, it installs a no-op provider and returns
// a no-op shutdown. Errors initializing the exporter are returned without
// installing a provider, so the caller may decide whether to fail or fall back.
func Init(ctx context.Context, role string) (ShutdownFunc, error) {
	initMu.Lock()
	defer initMu.Unlock()
	if initialized.Load() {
		// Subsequent callers (e.g. `serve` runs coordinator + worker in-process)
		// share the first provider. Return a no-op so each caller may still
		// defer shutdown without tearing down the live provider.
		return noopShutdown, nil
	}
	if !Enabled() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetTextMapPropagator(defaultPropagator())
		initialized.Store(true)
		return noopShutdown, nil
	}

	exp, err := newExporter(ctx)
	if err != nil {
		return noopShutdown, fmt.Errorf("tracing: build exporter: %w", err)
	}

	res, err := buildResource(ctx, role)
	if err != nil {
		// Resource detection should not be fatal; fall back to a minimal resource.
		res = resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(defaultServiceName(role)),
		)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(defaultPropagator())

	shutdown := func(shutdownCtx context.Context) error {
		if _, ok := shutdownCtx.Deadline(); !ok {
			var cancel context.CancelFunc
			shutdownCtx, cancel = context.WithTimeout(shutdownCtx, 5*time.Second)
			defer cancel()
		}
		return tp.Shutdown(shutdownCtx)
	}
	initialized.Store(true)
	return shutdown, nil
}

// Enabled reports whether any OTLP traces endpoint is configured.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func newExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	proto := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")))
	if proto == "" {
		proto = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	switch proto {
	case "", "grpc":
		return otlptrace.New(ctx, otlptracegrpc.NewClient())
	case "http/protobuf", "http", "http/proto":
		return otlptrace.New(ctx, otlptracehttp.NewClient())
	default:
		return nil, fmt.Errorf("unsupported OTEL_EXPORTER_OTLP_PROTOCOL %q (want grpc|http/protobuf)", proto)
	}
}

func buildResource(ctx context.Context, role string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(defaultServiceName(role)),
		attribute.String("workbuddy.role", role),
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithAttributes(attrs...),
	)
}

func defaultServiceName(role string) string {
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		return v
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return "workbuddy"
	}
	return "workbuddy-" + role
}

func defaultPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// Tracer returns the workbuddy tracer from the global provider. It is safe to
// call before Init: a no-op tracer is returned until a provider is installed.
func Tracer() trace.Tracer {
	return otel.Tracer(InstrumentationName)
}

// Start is a convenience wrapper around Tracer().Start.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, span
}
