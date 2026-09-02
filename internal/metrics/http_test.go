package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestHTTP gives each test its own registry. This is the payoff of not
// using prometheus.DefaultRegisterer: tests are isolated and can run in
// parallel without duplicate-registration panics.
func newTestHTTP(t *testing.T) (*HTTP, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return NewHTTP(reg), reg
}

// testMux mirrors the real route table closely enough to exercise r.Pattern.
func testMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"x"}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestRouteLabelIsTemplateNotPath is the test that actually protects you.
// Three requests to three different order IDs must produce ONE time series.
func TestRouteLabelIsTemplateNotPath(t *testing.T) {
	m, reg := newTestHTTP(t)
	h := m.Middleware(testMux())

	for _, id := range []string{"aaa", "bbb", "ccc"} {
		do(h, http.MethodGet, "/orders/"+id)
	}

	got := testutil.ToFloat64(m.requests.WithLabelValues("/orders/{id}", "GET", "200", "2xx"))
	if got != 3 {
		t.Fatalf("want 3 observations on the templated route, got %v", got)
	}

	// The real assertion: three distinct IDs produced ONE series, not three.
	// Had we labelled with r.URL.Path this would be 3, and unbounded in prod.
	if n := testutil.CollectAndCount(reg, "orders_http_requests_total"); n != 1 {
		t.Fatalf("expected 1 request series, got %d — cardinality leak", n)
	}
}

// TestUnmatchedPathsCollapse guards the DoS vector: attacker-controlled URLs
// must not each become a series.
func TestUnmatchedPathsCollapse(t *testing.T) {
	m, _ := newTestHTTP(t)
	h := m.Middleware(testMux())

	for _, p := range []string{"/aaaa", "/bbbb", "/cccc", "/x/y/z"} {
		do(h, http.MethodGet, p)
	}

	got := testutil.ToFloat64(m.requests.WithLabelValues(routeUnmatched, "GET", "404", "4xx"))
	if got != 4 {
		t.Fatalf("want 4 requests folded into %q, got %v", routeUnmatched, got)
	}
}

func TestStatusAndClassLabels(t *testing.T) {
	m, _ := newTestHTTP(t)
	h := m.Middleware(testMux())

	do(h, http.MethodPost, "/orders")
	do(h, http.MethodGet, "/orders/missing")

	if got := testutil.ToFloat64(m.requests.WithLabelValues("/orders", "POST", "201", "2xx")); got != 1 {
		t.Errorf("201/2xx: got %v", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("/orders/{id}", "GET", "404", "4xx")); got != 1 {
		t.Errorf("404/4xx: got %v", got)
	}
}

// TestHealthEndpointsAreExcluded — probes must not pollute traffic metrics.
func TestHealthEndpointsAreExcluded(t *testing.T) {
	m, reg := newTestHTTP(t)
	h := m.Middleware(testMux())

	for i := 0; i < 10; i++ {
		do(h, http.MethodGet, "/healthz")
	}

	if n := testutil.CollectAndCount(reg, "orders_http_requests_total"); n != 0 {
		t.Fatalf("health probes created %d series; want 0", n)
	}
}

// TestImplicitStatusOK covers a handler that Writes without WriteHeader.
func TestImplicitStatusOK(t *testing.T) {
	m, _ := newTestHTTP(t)
	h := m.Middleware(testMux())

	do(h, http.MethodGet, "/orders/aaa")

	if got := testutil.ToFloat64(m.requests.WithLabelValues("/orders/{id}", "GET", "200", "2xx")); got != 1 {
		t.Fatalf("implicit 200 not recorded: got %v", got)
	}
}

func TestResponseSizeRecorded(t *testing.T) {
	m, reg := newTestHTTP(t)
	h := m.Middleware(testMux())

	do(h, http.MethodGet, "/orders/aaa") // body is `{"ok":true}` = 11 bytes

	sum, count := histogramTotals(t, reg, "orders_http_response_size_bytes")
	if count != 1 || sum != 11 {
		t.Fatalf("want one 11-byte observation, got count=%d sum=%v", count, sum)
	}
}

// TestLatencyBucketsHaveSubMillisecondResolution guards the bucket choice.
// Our p50 is well under 1ms; if every request lands in the first bucket the
// histogram cannot describe the latency we actually care about.
func TestLatencyBucketsHaveSubMillisecondResolution(t *testing.T) {
	if LatencyBuckets[0] > 0.001 {
		t.Fatalf("first latency bucket is %vs — too coarse for this service", LatencyBuckets[0])
	}
	// An SLO threshold must sit exactly on a boundary, never be interpolated.
	var has250ms bool
	for _, b := range LatencyBuckets {
		if b == 0.25 {
			has250ms = true
		}
	}
	if !has250ms {
		t.Fatal("no le=0.25 boundary; a 250ms SLO would be interpolated")
	}
}

// histogramTotals reads _sum and _count for the first series of a histogram.
func histogramTotals(t *testing.T, reg *prometheus.Registry, name string) (float64, uint64) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h.GetSampleCount() == 0 {
				continue
			}
			return h.GetSampleSum(), h.GetSampleCount()
		}
	}
	t.Fatalf("no observations found for %s", name)
	return 0, 0
}

func TestPreInitCreatesZeroValuedSeries(t *testing.T) {
	m, reg := newTestHTTP(t)
	m.PreInit([3]string{http.MethodPost, "/orders", "201"})

	// The series must exist with value 0 before any traffic, so that rate()
	// has a baseline and alerts can match it.
	if n := testutil.CollectAndCount(reg, "orders_http_requests_total"); n != 1 {
		t.Fatalf("want 1 pre-initialised series, got %d", n)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("/orders", "POST", "201", "2xx")); got != 0 {
		t.Fatalf("pre-initialised series should be 0, got %v", got)
	}
}

// TestPreInitMatchesRealSuccessCodes is the regression test for the dead-series
// bug: pre-initialising POST /orders with 200 when the handler returns 201
// leaves a flat-zero series beside the real traffic.
func TestPreInitMatchesRealSuccessCodes(t *testing.T) {
	m, reg := newTestHTTP(t)
	m.PreInit([3]string{http.MethodPost, "/orders", "201"})

	h := m.Middleware(testMux())
	do(h, http.MethodPost, "/orders") // testMux returns 201

	// Exactly one series: the pre-initialised one, now carrying the traffic.
	if n := testutil.CollectAndCount(reg, "orders_http_requests_total"); n != 1 {
		t.Fatalf("pre-init code does not match the handler's; got %d series, want 1", n)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("/orders", "POST", "201", "2xx")); got != 1 {
		t.Fatalf("want 1, got %v", got)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		100: "1xx", 200: "2xx", 201: "2xx", 301: "3xx",
		400: "4xx", 404: "4xx", 499: "4xx", 500: "5xx", 503: "5xx",
		0: "unknown", 700: "unknown",
	}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// TestInFlightReturnsToZero catches the classic leaked-gauge bug: an early
// return path that increments but never decrements.
func TestInFlightReturnsToZero(t *testing.T) {
	m, _ := newTestHTTP(t)
	h := m.Middleware(testMux())

	do(h, http.MethodGet, "/orders/aaa")
	do(h, http.MethodGet, "/healthz") // skipped path — must not leak either

	if got := testutil.ToFloat64(m.inFlight); got != 0 {
		t.Fatalf("in-flight gauge leaked: %v", got)
	}
}
