// Package telemetry wires OpenTelemetry tracing (§19, §23).
//
// Configuration is entirely through the standard OTel environment
// variables — there is deliberately no config-file section yet:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT         OTLP collector base URL (e.g.
//	                                    http://otel-collector:4318). Unset =
//	                                    tracing disabled: a no-op provider,
//	                                    no goroutines, no network attempts.
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  per-signal override (full URL incl.
//	                                    /v1/traces); also enables tracing.
//	OTEL_SERVICE_NAME                   overrides the service name passed
//	                                    to Init.
//	OTEL_TRACES_SAMPLER[_ARG]           e.g. parentbased_traceidratio / 0.1
//	                                    (default: parentbased_always_on).
//	OTEL_EXPORTER_OTLP_HEADERS          collector auth headers.
//	OTEL_RESOURCE_ATTRIBUTES            extra resource attributes.
//	OTEL_SDK_DISABLED=true              force-disable even with an endpoint.
//
// Spans are exported over OTLP/HTTP (a plain http:// endpoint URL selects
// an insecure connection, https:// a TLS one). Trace context propagates as
// W3C traceparent/tracestate plus baggage.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// enabled reports whether a real (exporting) tracer provider is installed.
// The middleware and InjectHTTP fast-path on this single atomic load, so a
// node without an OTLP endpoint pays nothing per request.
var enabled atomic.Bool

// Enabled reports whether tracing was configured (an OTLP endpoint is set
// and Init succeeded).
func Enabled() bool { return enabled.Load() }

// Init installs the global tracer provider and W3C propagators. serviceName
// is the default service.name (OTEL_SERVICE_NAME overrides it). When no
// OTLP endpoint is configured the globals stay no-op and the returned
// shutdown does nothing. The returned shutdown flushes buffered spans; call
// it on process exit with a bounded context.
func Init(serviceName string, logger *slog.Logger) func(context.Context) error {
	// Propagators are set unconditionally: extraction/injection of W3C
	// headers is allocation-cheap and keeps behavior uniform.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	noop := func(context.Context) error { return nil }
	if !configured() {
		return noop
	}
	exp, err := otlptracehttp.New(context.Background()) // endpoint & TLS from env
	if err != nil {
		logger.Warn("opentelemetry disabled: exporter setup failed", "error", err)
		return noop
	}
	res := resource.Default() // includes OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		if merged, err := resource.Merge(res,
			resource.NewSchemaless(attribute.String("service.name", serviceName))); err == nil {
			res = merged
		}
	}
	tp := sdktrace.NewTracerProvider( // sampler comes from OTEL_TRACES_SAMPLER
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	enabled.Store(true)
	logger.Info("opentelemetry tracing enabled",
		"endpoint", firstEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"),
		"service", serviceName)
	return func(ctx context.Context) error {
		enabled.Store(false)
		return tp.Shutdown(ctx)
	}
}

// configured reports whether the environment asks for tracing at all.
func configured() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	return firstEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Tracer returns the process tracer (no-op until Init enables tracing).
func Tracer() trace.Tracer { return otel.Tracer("github.com/hyperkubeorg/databox") }

// InjectHTTP injects the current trace context (W3C traceparent + baggage)
// into an outbound request. A no-op when tracing is disabled or ctx carries
// no span — safe to sprinkle on every internal RPC call site.
func InjectHTTP(ctx context.Context, req *http.Request) {
	if !Enabled() {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// ExtractHTTP returns ctx extended with any trace context found in h.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(h))
}
