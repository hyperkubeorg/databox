package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDisabledNoOverhead: with no OTLP endpoint configured Init must be a
// no-op — no goroutines, spans non-recording, middleware pass-through with
// no traceparent generated. (Runs first: later tests install a real global
// provider.)
func TestDisabledNoOverhead(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	before := runtime.NumGoroutine()
	shutdown := Init("databox-test", testLogger())
	if Enabled() {
		t.Fatal("Enabled() = true without an endpoint")
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("Init leaked goroutines: %d -> %d", before, after)
	}
	// Global provider stays no-op: spans are non-recording.
	_, span := Tracer().Start(context.Background(), "test")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Fatal("expected a non-recording span when disabled")
	}
	span.End()

	// Middleware passes through untouched and injects nothing.
	var gotTraceparent string
	h := Middleware(MiddlewareConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/kv/x", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rr.Code)
	}
	if gotTraceparent != "" {
		t.Fatalf("traceparent generated while disabled: %q", gotTraceparent)
	}

	// InjectHTTP must not add headers while disabled.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	InjectHTTP(context.Background(), req)
	if req.Header.Get("traceparent") != "" {
		t.Fatal("InjectHTTP wrote headers while disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestEnabledRoundTrip: Init against a fake OTLP/HTTP collector, then a
// traceparent round trip through the middleware — the extracted context
// must carry the caller's trace ID, the span must record, InjectHTTP must
// re-emit the same trace, and shutdown must flush to the collector.
func TestEnabledRoundTrip(t *testing.T) {
	var exported atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/traces") {
			exported.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL) // http:// = insecure
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_always_on")

	shutdown := Init("databox-test", testLogger())
	if !Enabled() {
		t.Fatal("Enabled() = false with an endpoint set")
	}

	const inTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	var handlerSC trace.SpanContext
	var injected string
	mw := Middleware(MiddlewareConfig{
		RouteName: func(*http.Request) string { return "/api/v1/kv/{key}" },
		Skip:      func(r *http.Request) bool { return r.URL.Path == "/internal/raft" },
		JoinOnly:  func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/internal/") },
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerSC = trace.SpanContextFromContext(r.Context())
		out := httptest.NewRequest(http.MethodPost, "https://peer/internal/propose", nil)
		InjectHTTP(r.Context(), out)
		injected = out.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/kv/foo", nil)
	req.Header.Set("traceparent", "00-"+inTrace+"-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !handlerSC.IsValid() || !handlerSC.IsSampled() {
		t.Fatalf("handler span context invalid/unsampled: %+v", handlerSC)
	}
	if got := handlerSC.TraceID().String(); got != inTrace {
		t.Fatalf("trace ID not propagated: got %s want %s", got, inTrace)
	}
	if !strings.Contains(injected, inTrace) {
		t.Fatalf("InjectHTTP traceparent %q does not carry trace %s", injected, inTrace)
	}

	// Skip: raft path must not get a span even when enabled.
	var raftSC trace.SpanContext
	raftH := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raftSC = trace.SpanContextFromContext(r.Context())
	}))
	rreq := httptest.NewRequest(http.MethodPost, "/internal/raft", nil)
	rreq.Header.Set("traceparent", "00-"+inTrace+"-00f067aa0ba902b7-01")
	raftH.ServeHTTP(httptest.NewRecorder(), rreq)
	if raftSC.IsValid() {
		t.Fatal("/internal/raft was traced despite Skip")
	}

	// JoinOnly: other /internal/* without a sampled parent roots no span.
	var internalSC trace.SpanContext
	iH := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalSC = trace.SpanContextFromContext(r.Context())
	}))
	iH.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/internal/propose", nil))
	if internalSC.IsValid() {
		t.Fatal("/internal/propose rooted a span without a sampled parent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown flush: %v", err)
	}
	if exported.Load() == 0 {
		t.Fatal("no spans were exported to the collector on shutdown")
	}
	if Enabled() {
		t.Fatal("Enabled() still true after shutdown")
	}
}
