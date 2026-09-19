package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ProcessingBuckets: order processing is a business operation measured in tens
// of milliseconds to seconds, not microseconds. A third distinct scale in this
// project — HTTP, database, and now business work — which is exactly why
// "just reuse the default buckets" is never the right answer.
var ProcessingBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// Business holds product-level metrics: the ones a product owner reads, not an
// SRE.
//
// These matter operationally for a reason that is easy to miss. Technical
// metrics tell you the system is up; business metrics tell you it is WORKING.
// A deploy that breaks payment authorisation can leave every RED metric
// perfectly green — 200s, fast, no errors — while `transitions{to="paid"}`
// drops to zero. That is the outage your customers notice and your dashboards
// do not, unless you built this layer.
type Business struct {
	created      *prometheus.CounterVec // currency
	createdValue *prometheus.CounterVec // currency

	// The primary backlog signal: FLOW, not level.
	//
	// Counters of state transitions are restart-safe, aggregate correctly
	// across replicas, and let you derive arrival rate, completion rate and
	// failure ratio in PromQL. What they cannot give you is the absolute
	// number of orders sitting in a state right now — see the backlog gauges.
	transitions *prometheus.CounterVec // from, to

	processing *prometheus.HistogramVec // outcome

	workerRuns     *prometheus.CounterVec // result
	workerPanics   *prometheus.CounterVec // stage
	workerBatch    prometheus.Histogram
	workerDuration prometheus.Histogram

	// The complementary LEVEL signal, refreshed on a schedule rather than at
	// scrape time. See RefreshBacklog.
	backlog        *prometheus.GaugeVec // status
	backlogOldest  prometheus.Gauge
	backlogRefresh prometheus.Gauge
	backlogErrors  prometheus.Counter

	// The closed set of statuses, so SetBacklog can zero the ones absent from
	// a result. Passed in rather than imported, keeping this package free of a
	// domain dependency.
	knownStatuses []string
}

func NewBusiness(reg prometheus.Registerer, knownStatuses []string) *Business {
	b := &Business{
		knownStatuses: knownStatuses,
		created: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "created_total",
			Help:      "Orders created, by currency.",
		}, []string{"currency"}),

		// Money as a counter. rate() over it gives revenue per second, which
		// is a metric your business actually recognises — and one of the best
		// outage detectors you will ever have, because it goes to zero for
		// failure modes that no technical metric notices.
		//
		// Integer cents, never floats: a float64 counter accumulating currency
		// loses precision, and Prometheus stores float64 regardless, so the
		// discipline of counting the smallest unit matters.
		createdValue: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "created_value_cents_total",
			Help:      "Cumulative value of created orders, in cents, by currency.",
		}, []string{"currency"}),

		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "state_transitions_total",
			Help:      "Order state transitions.",
		}, []string{"from", "to"}),

		processing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "processing_duration_seconds",
			Help:      "Time to process one order, by outcome.",
			Buckets:   ProcessingBuckets,

			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  100,
			NativeHistogramMinResetDuration: time.Hour,
		}, []string{"outcome"}),

		workerRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "worker",
			Name:      "runs_total",
			Help:      "Worker loop iterations by result.",
		}, []string{"result"}),

		// The background counterpart to orders_http_panics_total.
		//
		// An HTTP panic is contained: the recovery middleware turns it into a
		// 500 and the process lives on. A panic in a background goroutine has
		// no such net — it unwinds past Run() and takes the whole process down,
		// or (once recovery is added, as it now is) silently kills one batch.
		//
		// Neither is visible in any other metric. A dead worker produces no
		// errors and no latency, it just stops, which is precisely the failure
		// mode this package's doc comment warns about. Counting panics by stage
		// separates "one order is poison" from "the claim query is broken".
		workerPanics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "worker",
			Name:      "panics_total",
			Help:      "Panics recovered in background goroutines, by stage.",
		}, []string{"stage"}),

		workerBatch: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: "worker",
			Name:      "batch_size",
			Help:      "Orders claimed per iteration. Consistently hitting the max means you are falling behind.",
			Buckets:   []float64{0, 1, 2, 5, 10, 20, 50, 100},

			// Deliberately the ONE histogram here with no native buckets. This
			// measures a small bounded integer capped at WORKER_BATCH_SIZE, so
			// the eight explicit boundaries already describe every value it can
			// take. Exponential buckets solve a wide-dynamic-range problem this
			// metric does not have, and would only add cost.
		}),

		workerDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: "worker",
			Name:      "batch_duration_seconds",
			Help:      "Wall time of one worker iteration.",
			Buckets:   ProcessingBuckets,

			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  100,
			NativeHistogramMinResetDuration: time.Hour,
		}),

		backlog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "backlog",
			Name:      "orders",
			Help:      "Orders currently in each state. Refreshed periodically, NOT at scrape time.",
		}, []string{"status"}),

		// Queue LENGTH tells you how much work is waiting. Queue AGE tells you
		// how long the oldest item has waited, which is what a customer
		// actually experiences. A queue of 10,000 that drains in a second is
		// fine; a queue of 3 where the oldest is an hour old is an incident.
		// Alert on age.
		backlogOldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "backlog",
			Name:      "oldest_age_seconds",
			Help:      "Age of the oldest pending order.",
		}),

		// Staleness tracking for the cached gauges above.
		//
		// A cached gauge that silently stops updating is WORSE than no gauge:
		// it shows a plausible, frozen number and your dashboard looks calm
		// while the backlog grows. Exporting the refresh timestamp lets you
		// alert on the metric's own freshness:
		//
		//   time() - orders_backlog_last_refresh_timestamp_seconds > 60
		//
		// Any metric you compute out-of-band needs this. It is the price of
		// not querying at scrape time.
		backlogRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "backlog",
			Name:      "last_refresh_timestamp_seconds",
			Help:      "Unix time of the last successful backlog refresh. Alert if this goes stale.",
		}),

		backlogErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "backlog",
			Name:      "refresh_errors_total",
			Help:      "Failed backlog refreshes.",
		}),
	}

	reg.MustRegister(
		b.created, b.createdValue, b.transitions, b.processing,
		b.workerRuns, b.workerPanics, b.workerBatch, b.workerDuration,
		b.backlog, b.backlogOldest, b.backlogRefresh, b.backlogErrors,
	)
	return b
}

// PreInitBusiness creates the series that must exist before the first event.
//
// The failure transitions matter most here. An alert on
// `rate(orders_state_transitions_total{to="failed"}[5m])` cannot fire against a
// series that does not exist, so without this the alert is silently dead until
// the first failure — the exact moment you need it to already be working.
func (b *Business) PreInit(currencies []string, transitions [][2]string) {
	for _, c := range currencies {
		b.created.WithLabelValues(c)
		b.createdValue.WithLabelValues(c)
	}
	for _, t := range transitions {
		b.transitions.WithLabelValues(t[0], t[1])
	}
	for _, outcome := range []string{"paid", "failed"} {
		b.processing.WithLabelValues(outcome)
	}
	for _, r := range []string{"ok", "empty", "error"} {
		b.workerRuns.WithLabelValues(r)
	}

	// Same reasoning as orders_http_panics_total: an alert of the form
	// `increase(...panics_total[5m]) > 0` cannot match a series that does not
	// exist, and a CounterVec with no children emits nothing at all — not even
	// its HELP and TYPE lines. Without this the panic alert is silently dead
	// until the first panic, which is the one moment it must already work.
	for _, stage := range WorkerStages {
		b.workerPanics.WithLabelValues(stage)
	}
}

// WorkerStages are the recovery points in the background goroutines. Kept here
// so the pre-initialised series and the call sites in internal/worker cannot
// drift apart — a stage that panics but was never pre-initialised still gets
// counted, it just appears in the exposition for the first time at the worst
// possible moment.
var WorkerStages = []string{"batch", "process_order", "backlog_refresh"}

// OrderCreated satisfies httpapi.OrderEvents.
func (b *Business) OrderCreated(currency string, amountCents int64) {
	b.created.WithLabelValues(currency).Inc()
	b.createdValue.WithLabelValues(currency).Add(float64(amountCents))
	b.Transition("new", "pending")
}

func (b *Business) Transition(from, to string) {
	b.transitions.WithLabelValues(from, to).Inc()
}

// OrderProcessed and WorkerRun take a context solely to carry the trace
// exemplar.
//
// Background work is where exemplars pay for themselves most. A worker span is
// a ROOT span in its own trace — there is no request to search by, no user
// complaining, and no obvious way to find the trace for the one order that took
// thirty seconds. The exemplar on this histogram is the only path from "the p99
// is bad" to that specific trace.
func (b *Business) OrderProcessed(ctx context.Context, outcome string, d time.Duration) {
	observeExemplar(ctx, b.processing.WithLabelValues(outcome), d.Seconds())
}

func (b *Business) WorkerRun(ctx context.Context, result string, batchSize int, d time.Duration) {
	b.workerRuns.WithLabelValues(result).Inc()
	b.workerBatch.Observe(float64(batchSize))
	observeExemplar(ctx, b.workerDuration, d.Seconds())
}

// WorkerPanic records a recovered panic in a background goroutine.
func (b *Business) WorkerPanic(stage string) {
	b.workerPanics.WithLabelValues(stage).Inc()
}

// SetBacklog publishes the cached level metrics.
func (b *Business) SetBacklog(counts map[string]int64, oldest time.Duration) {
	// Explicitly zero every known status. Without this, a status that drops to
	// zero keeps its last non-zero value forever, because nothing in the map
	// tells the gauge to move.
	for _, s := range b.knownStatuses {
		b.backlog.WithLabelValues(s).Set(float64(counts[s]))
	}
	b.backlogOldest.Set(oldest.Seconds())
	b.backlogRefresh.Set(float64(time.Now().Unix()))
}

func (b *Business) BacklogRefreshFailed() { b.backlogErrors.Inc() }
