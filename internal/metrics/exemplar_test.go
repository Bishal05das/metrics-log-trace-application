package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/trace"
)

const testTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// sampledCtx returns a context carrying a SAMPLED span context, without needing
// a real TracerProvider.
func sampledCtx(t *testing.T, sampled bool) context.Context {
	t.Helper()

	tid, err := trace.TraceIDFromHex(testTraceID)
	if err != nil {
		t.Fatalf("bad test trace id: %v", err)
	}
	sid, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("bad test span id: %v", err)
	}

	var flags trace.TraceFlags
	if sampled {
		flags = trace.FlagsSampled
	}
	return trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tid, SpanID: sid, TraceFlags: flags,
		}))
}

// findExemplar returns the first exemplar attached to any bucket of the named
// histogram, or nil.
func findExemplar(t *testing.T, reg *prometheus.Registry, name string) *dto.Exemplar {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				if b.Exemplar != nil {
					return b.Exemplar
				}
			}
		}
	}
	return nil
}

func TestHistogramCarriesTraceExemplar(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHTTP(reg)

	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil).
		WithContext(sampledCtx(t, true))
	req.Pattern = "GET /orders"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	ex := findExemplar(t, reg, "orders_http_request_duration_seconds")
	if ex == nil {
		t.Fatal("no exemplar on the duration histogram; " +
			"metrics can no longer link to traces")
	}

	var got string
	for _, l := range ex.GetLabel() {
		if l.GetName() == exemplarTraceIDLabel {
			got = l.GetValue()
		}
	}
	if got != testTraceID {
		t.Errorf("exemplar trace_id = %q, want %q", got, testTraceID)
	}
}

// An unsampled span still HAS a trace ID, but no backend ever received the
// trace. Recording it produces a link to "trace not found" — under a 10% sample
// ratio, for 90% of exemplars.
func TestUnsampledSpanProducesNoExemplar(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHTTP(reg)

	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil).
		WithContext(sampledCtx(t, false))
	req.Pattern = "GET /orders"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if ex := findExemplar(t, reg, "orders_http_request_duration_seconds"); ex != nil {
		t.Errorf("unsampled span produced an exemplar: %v", ex.GetLabel())
	}
}

// A request with no span at all must still record the observation. Tracing is
// optional (TRACING_ENABLED=false), and losing latency metrics because of that
// would be a severe regression.
func TestNoSpanStillObserves(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHTTP(reg)

	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Pattern = "GET /orders"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "orders_http_request_duration_seconds" {
			continue
		}
		if n := f.GetMetric()[0].GetHistogram().GetSampleCount(); n != 1 {
			t.Fatalf("sample count = %d, want 1", n)
		}
		return
	}
	t.Fatal("duration histogram missing entirely")
}

// The exemplar helpers must tolerate a nil context rather than panicking — they
// sit on the hot path of every request and every query.
func TestExemplarHelpersTolerateNilContext(t *testing.T) {
	if labels := traceExemplar(nil); labels != nil {
		t.Errorf("traceExemplar(nil) = %v, want nil", labels)
	}

	h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "t"})
	observeExemplar(nil, h, 1) //nolint:staticcheck // nil context is the case under test

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "c"})
	addExemplar(nil, c, 1) //nolint:staticcheck // nil context is the case under test
}
