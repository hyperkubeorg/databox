// middleware.go — the HTTP server span middleware. Route naming is left to
// the caller (pkg/server passes the gorilla/mux route template) so span
// names stay low-cardinality.
package telemetry

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// MiddlewareConfig tunes the per-request span middleware.
type MiddlewareConfig struct {
	// RouteName returns the low-cardinality route template for a matched
	// request (NOT the raw path). Empty string falls back to the method.
	RouteName func(*http.Request) string
	// Skip marks requests that must never be traced (raft hot path).
	Skip func(*http.Request) bool
	// JoinOnly marks requests that are traced only when the caller
	// propagated a sampled trace context — they join existing traces but
	// never root new ones (keeps /internal/* volume near zero).
	JoinOnly func(*http.Request) bool
}

// Middleware returns an http middleware that starts a server span per
// request, extracts inbound W3C trace context, and records method, route,
// and status. When tracing is disabled the only per-request cost is one
// atomic load and two predicate calls.
func Middleware(cfg MiddlewareConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !Enabled() || (cfg.Skip != nil && cfg.Skip(r)) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := ExtractHTTP(r.Context(), r.Header)
			if cfg.JoinOnly != nil && cfg.JoinOnly(r) {
				if sc := trace.SpanContextFromContext(ctx); !sc.IsValid() || !sc.IsSampled() {
					next.ServeHTTP(w, r)
					return
				}
			}
			route := ""
			if cfg.RouteName != nil {
				route = cfg.RouteName(r)
			}
			name := r.Method
			if route != "" {
				name = r.Method + " " + route
			}
			ctx, span := Tracer().Start(ctx, name,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.route", route),
				))
			defer span.End()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))
			span.SetAttributes(attribute.Int("http.status_code", rec.status))
			if rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}

// statusRecorder captures the response status. It forwards Flush so
// streaming handlers (NDJSON watch) keep working, and exposes Unwrap for
// http.ResponseController.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
