package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/bishal05das/metrics-log-trace-application/internal/logging"
)

// recorder gives a real SDK tracer whose finished spans can be inspected —
// the tracing equivalent of a Prometheus test registry.
func recorder(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return tp.Tracer("test"), sr
}

func testMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	return mux
}

// TestSpanIsNamedAfterRouteTemplate — span names are a low-cardinality
// dimension in every backend. Naming spans after raw paths is the Phase 2
// cardinality mistake, one system over.
func TestSpanIsNamedAfterRouteTemplate(t *testing.T) {
	tr, sr := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	for _, id := range []string{"aaa", "bbb", "ccc"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/"+id, nil))
	}

	spans := sr.Ended()
	if len(spans) != 3 {
		t.Fatalf("want 3 spans, got %d", len(spans))
	}
	for _, s := range spans {
		if s.Name() != "GET /orders/{id}" {
			t.Errorf("span name = %q, want the route template", s.Name())
		}
	}
}

func TestUnmatchedRoutesCollapse(t *testing.T) {
	tr, sr := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope-"+strings.Repeat("x", 30), nil))

	if got := sr.Ended()[0].Name(); got != "GET unmatched" {
		t.Errorf("span name = %q, want 'GET unmatched'", got)
	}
}

// TestIncomingTraceparentIsHonoured is THE distributed-tracing property. Fail
// this and every service produces its own disconnected trace.
func TestIncomingTraceparentIsHonoured(t *testing.T) {
	tr, sr := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	const upstreamTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodGet, "/orders/abc", nil)
	req.Header.Set("traceparent", "00-"+upstreamTrace+"-00f067aa0ba902b7-01")

	h.ServeHTTP(httptest.NewRecorder(), req)

	span := sr.Ended()[0]
	if got := span.SpanContext().TraceID().String(); got != upstreamTrace {
		t.Fatalf("trace id = %s, want the upstream id %s — the trace was split",
			got, upstreamTrace)
	}
	if !span.Parent().IsValid() {
		t.Error("span has no parent; it was created as a root instead of a child")
	}
}

func TestRootSpanCreatedWithoutUpstream(t *testing.T) {
	tr, sr := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/abc", nil))

	span := sr.Ended()[0]
	if span.Parent().IsValid() {
		t.Error("expected a root span when no traceparent is present")
	}
	if !span.SpanContext().TraceID().IsValid() {
		t.Error("root span has no trace id")
	}
}

// TestOnly5xxMarksSpanFailed — a 404 is a correct answer to a wrong question.
// Marking it an error makes a backend's error-rate view meaningless.
func TestOnly5xxMarksSpanFailed(t *testing.T) {
	cases := map[string]bool{"/orders/abc": false, "/orders/missing": false, "/orders/boom": true}

	for path, wantErr := range cases {
		tr, sr := recorder(t)
		h := Middleware(tr, logging.Discard())(testMux())
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

		got := sr.Ended()[0].Status().Code.String() == "Error"
		if got != wantErr {
			t.Errorf("%s: span error = %v, want %v", path, got, wantErr)
		}
	}
}

// TestTraceIDReachesTheLogs is the logs<->traces join. Without it you can find
// a slow trace and have no way to reach its log lines.
func TestTraceIDReachesTheLogs(t *testing.T) {
	tr, _ := recorder(t)

	buf := &bytes.Buffer{}
	log := slog.New(&logging.ContextHandler{
		Handler: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.InfoContext(r.Context(), "inside the handler")
	})
	Middleware(tr, logging.Discard())(inner).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("no log line: %v", err)
	}
	id, ok := rec["trace_id"].(string)
	if !ok || len(id) != 32 {
		t.Fatalf("trace_id missing or malformed in the log record: %v", rec)
	}
	if _, ok := rec["span_id"]; !ok {
		t.Error("span_id missing from the log record")
	}
}

func TestTraceIDReturnedToClient(t *testing.T) {
	tr, _ := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/abc", nil))

	if got := rec.Header().Get("X-Trace-Id"); len(got) != 32 {
		t.Errorf("X-Trace-Id = %q, want a 32-char trace id", got)
	}
}

// TestHealthProbesAreNotTraced — probes fire forever and say nothing. Same
// exclusion as metrics and logs.
func TestHealthProbesAreNotTraced(t *testing.T) {
	tr, sr := recorder(t)
	h := Middleware(tr, logging.Discard())(testMux())

	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("health probes produced %d spans, want 0", n)
	}
}

// TestSamplerIsParentBased — if an upstream service already decided to sample,
// we must honour it, or a distributed trace comes back with holes.
func TestSamplerIsParentBased(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(newSampler(0)), // never sample our own roots
	)
	defer tp.Shutdown(context.Background())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	h := Middleware(tp.Tracer("t"), logging.Discard())(testMux())

	// No upstream decision -> not sampled.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/a", nil))
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("ratio 0 recorded %d root spans, want 0", n)
	}

	// Upstream says "sampled" (flag 01) -> we must honour it.
	req := httptest.NewRequest(http.MethodGet, "/orders/b", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if n := len(sr.Ended()); n != 1 {
		t.Fatalf("upstream sampling decision ignored: recorded %d spans, want 1", n)
	}
}

func TestParseExporter(t *testing.T) {
	cases := map[string]Exporter{
		"stdout": ExporterStdout, "STDOUT": ExporterStdout,
		"otlp": ExporterOTLP, "  otlp  ": ExporterOTLP,
		"none": ExporterNone, "": ExporterNone,
		"nonsense": ExporterStdout,
	}
	for in, want := range cases {
		if got := ParseExporter(in); got != want {
			t.Errorf("ParseExporter(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestDisabledStillPropagates — a service that does not RECORD traces must
// still PASS THROUGH the context it receives, or it becomes a hole in someone
// else's trace.
func TestDisabledStillPropagates(t *testing.T) {
	p, err := Init(context.Background(), Config{Enabled: false, ServiceName: "orders"}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if p.Tracer == nil {
		t.Fatal("disabled provider returned a nil tracer")
	}
	if otel.GetTextMapPropagator() == nil {
		t.Fatal("propagator not installed while tracing is disabled")
	}

	carrier := propagation.MapCarrier{}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		TraceFlags: trace.FlagsSampled,
	}))
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	if carrier.Get("traceparent") == "" {
		t.Fatal("traceparent was not injected — downstream traces would break")
	}
}

// Shutdown must be safe on a disabled provider.
func TestShutdownOnDisabledProvider(t *testing.T) {
	p, _ := Init(context.Background(), Config{Enabled: false, ServiceName: "orders"}, logging.Discard())
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown on a no-op provider: %v", err)
	}
}

// TestInitEnabledSucceeds is the regression test for a startup crash that every
// other test in this file missed.
//
// They all call Init with Enabled:false, which returns a no-op provider before
// building a resource. The enabled path calls resource.Merge, which REFUSES to
// combine resources with different schema URLs — and semconv v1.26.0 against an
// SDK on v1.43.0 produced exactly that, so the binary compiled, every test
// passed, and the container crash-looped on startup with
// "conflicting Schema URL".
//
// The lesson generalises: a constructor that only ever runs in its disabled
// form is not tested.
func TestInitEnabledSucceeds(t *testing.T) {
	for _, exp := range []Exporter{ExporterStdout, ExporterNone} {
		p, err := Init(context.Background(), Config{
			Enabled:     true,
			Exporter:    exp,
			SampleRatio: 1.0,
			ServiceName: "orders",
			Version:     "test",
			Env:         "test",
		}, logging.Discard())
		if err != nil {
			t.Fatalf("Init(exporter=%s) failed: %v", exp, err)
		}
		if p.Tracer == nil {
			t.Fatalf("Init(exporter=%s) returned a nil tracer", exp)
		}
		if err := p.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown(exporter=%s): %v", exp, err)
		}
	}
}

// TestInitEnabledProducesUsableSpans goes one step further: a provider that
// constructs cleanly but records nothing would still pass the test above.
func TestInitEnabledProducesUsableSpans(t *testing.T) {
	p, err := Init(context.Background(), Config{
		Enabled: true, Exporter: ExporterStdout, SampleRatio: 1.0,
		ServiceName: "orders", Version: "test", Env: "test",
	}, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	_, span := p.Tracer.Start(context.Background(), "probe")
	sc := span.SpanContext()
	span.End()

	if !sc.IsValid() || !sc.IsSampled() {
		t.Fatalf("span context invalid or unsampled at ratio 1.0: %+v", sc)
	}
}
