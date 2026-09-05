package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// routeUnmatched is the bucket for any request that did not match a declared
// route. This single constant is the most important line in this file — see
// routeLabel below for why.
const routeUnmatched = "unmatched"

// LatencyBuckets are the histogram boundaries for request duration, in seconds.
//
// prometheus.DefBuckets (.005 → 10) is the library default and it is WRONG for
// a service like this one. Our measured p50 is under 1ms, so with DefBuckets
// every single successful request lands in the first bucket (le="0.005") and
// histogram_quantile can tell you nothing except "somewhere between 0 and 5ms".
// You would have a latency panel that is technically correct and completely
// useless.
//
// Rules for choosing buckets:
//   - Cover your *current* p50 with at least 2-3 buckets below it, so you can
//     see improvements as well as regressions.
//   - Put a boundary exactly on your SLO threshold. If your SLO is "99% under
//     250ms", you need le="0.25" to exist or you are interpolating your own
//     compliance number.
//   - Extend past your timeout so slow requests are distinguishable from each
//     other rather than all piling into +Inf.
//   - Every bucket is a time series per label combination. Be deliberate.
//
// These 14 span 500µs to 10s, with 0.25 and 1 as future SLO thresholds.
var LatencyBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// ResponseSizeBuckets are powers of four from 64B to 1MiB. Response size
// matters because "the API got slow" is often really "someone requested
// limit=100 and we now serialise 40x more JSON".
var ResponseSizeBuckets = []float64{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576}

// HTTP holds the RED metrics: Rate, Errors, Duration.
//
// RED is the standard model for request-driven services (its counterpart, USE
// — Utilisation, Saturation, Errors — is for resources, and we apply that to
// the connection pool in Phase 3). Rate and Errors both come off the same
// counter: errors are just a filtered rate.
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	respSize *prometheus.HistogramVec
	inFlight prometheus.Gauge
	panics   *prometheus.CounterVec

	// skip holds paths excluded from request metrics.
	skip map[string]bool
}

func NewHTTP(reg prometheus.Registerer) *HTTP {
	h := &HTTP{
		// Rate + Errors.
		//
		// Labels: route, method, code, status_class.
		//
		// `status_class` is fully DERIVED from `code` — 404 always implies
		// 4xx. That means it creates no new label combinations and therefore
		// costs ZERO extra time series. It is pure query convenience:
		// `status_class="5xx"` reads better than `code=~"5.."` and avoids a
		// regex matcher in your hot alerting rules.
		//
		// This is worth internalising: a label that is a *function* of another
		// label is free. A label that is *independent* multiplies.
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: Namespace,
				Subsystem: "http",
				Name:      "requests_total",
				Help:      "Total HTTP requests by route, method and response status.",
			},
			[]string{"route", "method", "code", "status_class"},
		),

		// Duration.
		//
		// Labels: route, method, status_class — note NO `code` here.
		//
		// On a histogram every label combination costs len(buckets)+2 series,
		// so 16 series each. `code` has ~6 values against status_class's 3, so
		// using it here would double this metric for information you almost
		// never need ("what is the p99 latency of 403s specifically?").
		//
		// We keep status_class because you genuinely do need to exclude errors
		// from a latency SLO — failures are often fast, and letting them into
		// the histogram makes your p99 look better during an outage.
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: Namespace,
				Subsystem: "http",
				Name:      "request_duration_seconds",
				Help:      "HTTP request latency in seconds.",
				Buckets:   LatencyBuckets,

				// Native histograms (Prometheus 2.40+, stable in 3.x) store
				// exponential buckets computed automatically, which removes
				// the bucket-choosing problem entirely at far lower storage
				// cost. Setting this emits BOTH representations: classic
				// buckets over the text format, native ones if the scraper
				// negotiates protobuf. Costs nothing to leave on.
				NativeHistogramBucketFactor:     1.1,
				NativeHistogramMaxBucketNumber:  100,
				NativeHistogramMinResetDuration: time.Hour,
			},
			[]string{"route", "method", "status_class"},
		),

		respSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: Namespace,
				Subsystem: "http",
				Name:      "response_size_bytes",
				Help:      "HTTP response body size in bytes.",
				Buckets:   ResponseSizeBuckets,
			},
			[]string{"route", "method"},
		),

		// Saturation — concurrent requests being served right now.
		//
		// This gauge is deliberately UNLABELLED, and the reason is a genuine
		// constraint rather than laziness. ServeMux populates r.Pattern only
		// when it matches, which happens *inside* next.ServeHTTP. An outer
		// middleware must increment before that call, when the route is still
		// unknown. To label in-flight by route you must instrument each
		// handler at registration time instead. Global in-flight answers the
		// question that actually matters ("are we near capacity?"), so we take
		// the simple version.
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "http",
			Name:      "requests_in_flight",
			Help:      "Requests currently being served.",
		}),

		panics: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: Namespace,
				Subsystem: "http",
				Name:      "panics_total",
				Help:      "Handler panics recovered, by route.",
			},
			[]string{"route"},
		),

		// Kubernetes hits liveness/readiness probes every few seconds. Left in
		// the metrics they would dominate your request rate, drag your latency
		// percentiles down (they are trivially fast), and make a real traffic
		// drop invisible behind constant probe volume.
		//
		// /metrics itself needs no exclusion here — it lives on a separate
		// server that this middleware never wraps. That is the payoff of the
		// two-listener design from Phase 1.
		skip: map[string]bool{"/healthz": true, "/readyz": true},
	}

	reg.MustRegister(h.requests, h.duration, h.respSize, h.inFlight, h.panics)
	return h
}

// PreInit creates the series for known route/method pairs at value zero.
//
// Why bother: a counter that has never been incremented does not exist in the
// exposition output at all. Worse for a *Vec — a CounterVec with no children
// emits nothing whatsoever, not even its # HELP and # TYPE lines. `rate()` over
// a series that appears for the first time returns nothing, dashboards show
// "No data" instead of a flat zero line, and — worst — an alert like
// `increase(...panics...) > 0` cannot match a series that is absent, so a
// brand-new failure mode stays silent exactly when you need it loudest.
//
// That is not hypothetical: orders_http_panics_total was missing from /metrics
// entirely until it was added to the loop below, because no handler had ever
// panicked and nothing else created a child.
//
// Each spec is {method, route, success code}. The success code must be the one
// the handler actually returns — 201 for a create, not 200 — or you get a
// permanently-zero series next to the real one.
//
// You cannot pre-init everything: you do not know in advance which error codes
// will occur. For those, the guard belongs in PromQL — but NOT `or vector(0)`,
// which substitutes a LABELLESS series that matches no `by (job)` aggregation.
// Use `clamp_min` on a denominator, or `or <same aggregation> * 0` to keep the
// labels. See the note at the top of prometheus/rules/recording.yml.
func (h *HTTP) PreInit(specs ...[3]string) {
	for _, s := range specs {
		method, route, code := s[0], s[1], s[2]

		n, err := strconv.Atoi(code)
		if err != nil {
			panic("metrics: PreInit success code " + code + " is not numeric")
		}
		class := statusClass(n)

		h.requests.WithLabelValues(route, method, code, class)
		h.duration.WithLabelValues(route, method, class)
		h.respSize.WithLabelValues(route, method)
		h.panics.WithLabelValues(route)
	}

	// The catch-all handler can panic too, and its label value never appears in
	// the route table above.
	h.panics.WithLabelValues(routeUnmatched)
}

// Middleware records RED metrics for every request it wraps.
func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.skip[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		//How many requests are currently being processed.
		h.inFlight.Inc()
		defer h.inFlight.Dec()

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		// r.Pattern is only populated once ServeMux has matched, which it does
		// in place on this same *http.Request — so it is readable here, after
		// the call, but was empty before it.
		elapsed := time.Since(start).Seconds()
		route := routeLabel(r)
		class := statusClass(rec.status)
		code := strconv.Itoa(rec.status)

		h.requests.WithLabelValues(route, r.Method, code, class).Inc()
		h.duration.WithLabelValues(route, r.Method, class).Observe(elapsed)
		h.respSize.WithLabelValues(route, r.Method).Observe(float64(rec.written))
	})
}

// RecordPanic is called by the recovery middleware.
func (h *HTTP) RecordPanic(r *http.Request) {
	h.panics.WithLabelValues(routeLabel(r)).Inc()
}

// routeLabel converts a request into a bounded label value.
//
// THIS IS THE CARDINALITY FIREWALL. Without it, labelling with r.URL.Path
// means every distinct URL becomes a permanent time series, and since URLs are
// attacker-controlled, anyone who can reach your service can create unlimited
// series in your Prometheus by requesting /aaaa, /aaab, /aaac... That is not a
// theoretical concern; it is a real and frequently-exploited way to take down
// a monitoring system, and it takes down monitoring for every OTHER service
// sharing that Prometheus too.
//
// We use r.Pattern (the matched route template, e.g. "GET /orders/{id}") and
// collapse everything unrecognised into a single "unmatched" bucket. The label
// set is therefore bounded by the number of routes we declared in code, which
// is a number we control.
//
// The method prefix is stripped because `method` is already its own label;
// carrying it twice would be redundant.
func routeLabel(r *http.Request) string {
	p := r.Pattern
	if p == "" {
		// No match at all, or a 405 from method-mismatch handling.
		return routeUnmatched
	}
	if i := strings.LastIndex(p, " "); i >= 0 {
		p = p[i+1:]
	}
	if p == "" || p == "/" {
		// Our catch-all handler. Every unknown path lands here, so it must not
		// carry the real path.
		return routeUnmatched
	}
	return p
}

func statusClass(code int) string {
	switch {
	// The < 100 guard must come first. Without it a zero value — which is what
	// you get if a handler hijacks the connection or the recorder is never
	// written to — silently reports as "2xx", turning a broken request into a
	// success in your dashboards.
	case code < 100:
		return "unknown"
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	case code < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// responseRecorder captures the status code and body size, neither of which
// http.ResponseWriter exposes after the fact.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return // net/http already warns about this; do not corrupt our metric too.
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		// An implicit 200: a handler that calls Write without WriteHeader.
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.NewResponseController reach the real ResponseWriter, so
// Flush, Hijack and deadline control keep working through this wrapper. Before
// Go 1.20 you had to hand-implement every optional interface, and forgetting
// one silently broke SSE and WebSocket upgrades — a classic middleware bug.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
