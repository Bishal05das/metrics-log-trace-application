package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

// Exemplars are the bridge from the metrics pillar to the traces pillar.
//
// A histogram tells you that 1% of requests took over a second. It cannot tell
// you WHICH ones, and that is where most latency investigations stall: you have
// proof a problem exists and no example of it. An exemplar attaches a trace ID
// to a single observation inside a bucket, so a spike on a p99 panel becomes a
// clickable link to one real, slow request.
//
// Mechanically an exemplar is a tiny (labels, value, timestamp) record stored
// alongside a bucket rather than as a series of its own, so it does NOT count
// against cardinality — Prometheus keeps only the most recent one per series.
// That is the whole reason this is affordable where a `trace_id` LABEL would be
// catastrophic: a label creates one permanent time series per trace, which is
// the exact unbounded-cardinality failure routeLabel exists to prevent. Same
// data, opposite cost, because of where it is stored.
//
// Three things must all be true or the exemplar silently never appears:
//
//  1. The application attaches it — this file.
//  2. The endpoint negotiates OpenMetrics — EnableOpenMetrics in server.go.
//  3. Prometheus is started with --enable-feature=exemplar-storage, otherwise
//     it parses them and throws them away. See docker-compose.yml.
//
// Nothing errors if one is missing. You just get no exemplars, which is why all
// three are worth knowing about together.

// exemplarTraceIDLabel is the conventional key. Grafana's exemplar support
// looks for "trace_id" by default; naming it anything else means the link
// column never renders.
const exemplarTraceIDLabel = "trace_id"

// traceExemplar returns the exemplar labels for ctx, or nil when there is
// nothing useful to attach.
//
// The IsSampled check is the important part. An unsampled span still has a real
// trace ID, but no backend ever received the trace — so recording it produces a
// link that leads to "trace not found". Under a 10% sample ratio that would be
// 90% of your exemplars. Only sampled traces are worth pointing at.
func traceExemplar(ctx context.Context) prometheus.Labels {
	if ctx == nil {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsSampled() {
		return nil
	}
	return prometheus.Labels{exemplarTraceIDLabel: sc.TraceID().String()}
}

// observeExemplar records an observation, with a trace exemplar when one is
// available.
//
// The type assertion is required rather than optional: prometheus.Observer has
// only Observe(float64). ExemplarObserver is a separate, optional interface
// that the concrete histogram implements but Summary does not — so asserting
// keeps this helper safe to call on any observer, degrading to a plain Observe
// instead of panicking.
func observeExemplar(ctx context.Context, o prometheus.Observer, v float64) {
	if labels := traceExemplar(ctx); labels != nil {
		if eo, ok := o.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(v, labels)
			return
		}
	}
	o.Observe(v)
}

// addExemplar increments a counter, with a trace exemplar when one is
// available.
//
// Reserved for counters that mean "something went wrong here" — panics and
// query errors. On those, the first question is always "show me one", and an
// exemplar answers it directly. Putting one on a high-rate success counter
// would be churn for no benefit: only the newest exemplar per series survives,
// so under load you would be overwriting it thousands of times a second to end
// up pointing at an arbitrary healthy request.
func addExemplar(ctx context.Context, c prometheus.Counter, v float64) {
	if labels := traceExemplar(ctx); labels != nil {
		if ea, ok := c.(prometheus.ExemplarAdder); ok {
			ea.AddWithExemplar(v, labels)
			return
		}
	}
	c.Add(v)
}
