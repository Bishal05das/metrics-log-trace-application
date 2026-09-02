package metrics

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// DBLatencyBuckets: queries are an order of magnitude faster than requests, so
// they need their own, finer boundaries. Reusing LatencyBuckets would put every
// healthy query in the first bucket — the same mistake DefBuckets makes for
// HTTP, one scale down.
var DBLatencyBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// AcquireWaitBuckets: on a healthy pool this is microseconds — you are just
// taking an idle connection off a channel. Anything above ~1ms means you
// waited for someone else to finish. The low boundaries exist so that "we are
// starting to queue" is visible long before it becomes "we are timing out".
var AcquireWaitBuckets = []float64{
	0.00001, 0.0001, 0.00025, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5,
}

// DB instruments database access.
//
// It implements BOTH pgx.QueryTracer and pgxpool.AcquireTracer. pgxpool
// type-asserts whatever you set as ConnConfig.Tracer against the acquire and
// release tracer interfaces, so a single object can serve both roles.
//
// Using a tracer rather than hand-written Observe() calls in each store method
// is the production choice for one reason: you cannot forget it. A colleague
// adding a new query next year gets metrics automatically. Manual
// instrumentation is correct exactly until the first person is in a hurry.
type DB struct {
	// Coarse label set (query, status) with status ∈ {ok, error} — same
	// principle as the HTTP histogram: keep bucketed metrics cheap.
	queryDuration *prometheus.HistogramVec

	// Detailed breakdown lives on the counter, where a label costs one series
	// rather than seventeen.
	queryErrors *prometheus.CounterVec

	acquireWait   prometheus.Histogram
	acquireErrors prometheus.Counter

	// names maps normalised SQL text to a short, bounded label value.
	names map[string]string
}

// NewDB builds the database metrics.
//
// names maps SQL text to a label value; see store.QueryNames(). Anything not in
// the map is labelled "other" — never the raw SQL. Two reasons: SQL text is
// enormous for a label, and any query built by string concatenation would
// produce an unbounded label set. "other" showing up in your dashboard is a
// visible, harmless prompt to go name the new query.
func NewDB(reg prometheus.Registerer, names map[string]string) *DB {
	normalised := make(map[string]string, len(names))
	for sql, name := range names {
		normalised[normaliseSQL(sql)] = name
	}

	d := &DB{
		names: normalised,

		queryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:                       Namespace,
				Subsystem:                       "db",
				Name:                            "query_duration_seconds",
				Help:                            "Database query latency in seconds, by named query.",
				Buckets:                         DBLatencyBuckets,
				NativeHistogramBucketFactor:     1.1,
				NativeHistogramMaxBucketNumber:  100,
				NativeHistogramMinResetDuration: time.Hour,
			},
			[]string{"query", "status"},
		),

		queryErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: Namespace,
				Subsystem: "db",
				Name:      "query_errors_total",
				Help:      "Database query errors by named query and error kind.",
			},
			[]string{"query", "kind"},
		),

		// THE signal that separates two problems with opposite fixes:
		//
		//   query_duration high, acquire_wait low  → the database is slow.
		//                                            Fix the query or the index.
		//   query_duration low,  acquire_wait high → you are out of connections.
		//                                            Raise the pool, or find
		//                                            what is holding them.
		//
		// Without this metric both look identical from the outside ("the API
		// got slow") and teams routinely spend a day optimising a query that
		// was never the problem.
		acquireWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: "db_pool",
			Name:      "acquire_wait_seconds",
			Help:      "Time spent waiting to acquire a connection from the pool.",
			Buckets:   AcquireWaitBuckets,
		}),

		acquireErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "db_pool",
			Name:      "acquire_errors_total",
			Help:      "Failed connection acquisitions (timeout, cancellation, pool closed).",
		}),
	}

	reg.MustRegister(d.queryDuration, d.queryErrors, d.acquireWait, d.acquireErrors)
	return d
}

// --- pgx.QueryTracer -------------------------------------------------------

type queryCtxKey struct{}

type queryCtxVal struct {
	name  string
	start time.Time
}

func (d *DB) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryCtxKey{}, queryCtxVal{
		name:  d.queryName(data.SQL),
		start: time.Now(),
	})
}

func (d *DB) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	v, ok := ctx.Value(queryCtxKey{}).(queryCtxVal)
	if !ok {
		return // Start was never called; nothing meaningful to record.
	}

	status := "ok"
	if isRealError(data.Err) {
		status = "error"
	}
	d.queryDuration.WithLabelValues(v.name, status).Observe(time.Since(v.start).Seconds())

	if status == "error" {
		d.queryErrors.WithLabelValues(v.name, errorKind(data.Err)).Inc()
	}
}

// --- pgxpool.AcquireTracer -------------------------------------------------

type acquireCtxKey struct{}

func (d *DB) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireCtxKey{}, time.Now())
}

func (d *DB) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	start, ok := ctx.Value(acquireCtxKey{}).(time.Time)
	if !ok {
		return
	}
	d.acquireWait.Observe(time.Since(start).Seconds())
	if data.Err != nil {
		d.acquireErrors.Inc()
	}
}

// --- label derivation ------------------------------------------------------

func (d *DB) queryName(sql string) string {
	if name, ok := d.names[normaliseSQL(sql)]; ok {
		return name
	}
	return "other"
}

// normaliseSQL collapses whitespace so that a query constant written across
// several indented lines matches regardless of formatting.
func normaliseSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

// isRealError distinguishes failures from ordinary outcomes.
//
// pgx.ErrNoRows means "that row does not exist", which for GET /orders/{id} is
// a perfectly successful query answering a legitimate question. Counting it as
// a database error would make your error rate track how often clients ask for
// missing things — and you would page someone for a crawler hitting stale URLs.
func isRealError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, pgx.ErrNoRows)
}

// errorKind produces a bounded label.
//
// PostgreSQL SQLSTATE codes are a CLOSED SET defined by the SQL standard and
// the Postgres docs — roughly 250 five-character codes, and any one service
// will realistically produce a handful. That makes them safe as a label value,
// and far more actionable than a boolean: 23505 (unique violation) is a
// client bug, 40001 (serialization failure) means retry, 53300 (too many
// connections) is an infrastructure problem. Three completely different
// responses that "error" alone cannot distinguish.
func errorKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		// Usually the client hung up. Not your fault, but worth seeing.
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "other"
}

// --- pool collector --------------------------------------------------------

// RegisterPool exposes pgxpool.Stat() as metrics via a custom Collector.
//
// This is the most reusable pattern in the project, so it is worth
// understanding rather than copying.
//
// The naive approach is a set of gauges plus a goroutine that Set()s them every
// few seconds. That is worse in three ways: the values are stale by up to the
// tick interval, you have a goroutine and a ticker to leak, and your gauges can
// silently diverge from the real pool if an update path is missed.
//
// A Collector inverts it. Collect() is called BY the registry, during the
// scrape, and reads the source of truth right then. No background work, no
// staleness, no duplicated state — the pool remains the only place the number
// lives. Use this pattern any time you need to expose state owned by something
// else: a cache, a work queue, a third-party client.
func RegisterPool(reg prometheus.Registerer, stat func() PoolStats) {
	reg.MustRegister(&poolCollector{stat: stat})
}

// PoolStats is the subset of *pgxpool.Stat this collector reads.
//
// Declaring the interface here, on the CONSUMER side, rather than importing
// the concrete type is idiomatic Go and buys two things: this package does not
// depend on the driver for the collector, and — the practical win —
// *pgxpool.Stat has unexported fields and no constructor, so without an
// interface the collector could not be unit-tested at all.
type PoolStats interface {
	AcquiredConns() int32
	IdleConns() int32
	ConstructingConns() int32
	MaxConns() int32

	AcquireCount() int64
	AcquireDuration() time.Duration
	EmptyAcquireCount() int64
	EmptyAcquireWaitTime() time.Duration
	CanceledAcquireCount() int64

	NewConnsCount() int64
	MaxLifetimeDestroyCount() int64
	MaxIdleDestroyCount() int64
}

type poolCollector struct {
	stat func() PoolStats
}

// USE method for a resource: Utilisation, Saturation, Errors.
var (
	descPoolConns = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "connections"),
		"Connections in the pool by state.",
		[]string{"state"}, nil,
	)
	descPoolMax = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "max_connections"),
		"Configured maximum pool size. Utilisation = acquired / this.",
		nil, nil,
	)
	descPoolAcquires = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "acquires_total"),
		"Total successful connection acquisitions.",
		nil, nil,
	)
	descPoolAcquireSeconds = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "acquire_seconds_total"),
		"Cumulative time spent acquiring connections.",
		nil, nil,
	)
	descPoolEmptyAcquires = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "empty_acquires_total"),
		"Acquisitions that had to wait because the pool was empty. Saturation.",
		nil, nil,
	)
	descPoolEmptyWaitSeconds = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "empty_acquire_wait_seconds_total"),
		"Cumulative time blocked on an empty pool.",
		nil, nil,
	)
	descPoolCanceledAcquires = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "canceled_acquires_total"),
		"Acquisitions abandoned because the caller's context ended first.",
		nil, nil,
	)
	descPoolNewConns = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "new_connections_total"),
		"Connections opened since start. Churn costs a TCP+TLS+auth round trip each.",
		nil, nil,
	)
	descPoolDestroyed = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "db_pool", "connections_destroyed_total"),
		"Connections closed, by reason.",
		[]string{"reason"}, nil,
	)
)

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPoolConns
	ch <- descPoolMax
	ch <- descPoolAcquires
	ch <- descPoolAcquireSeconds
	ch <- descPoolEmptyAcquires
	ch <- descPoolEmptyWaitSeconds
	ch <- descPoolCanceledAcquires
	ch <- descPoolNewConns
	ch <- descPoolDestroyed
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stat()
	if s == nil {
		// The pool is closed or not yet built. Emitting nothing is correct:
		// Prometheus marks the series stale rather than recording a false zero.
		return
	}

	g := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	cnt := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	// Utilisation.
	g(descPoolConns, float64(s.AcquiredConns()), "acquired")
	g(descPoolConns, float64(s.IdleConns()), "idle")
	g(descPoolConns, float64(s.ConstructingConns()), "constructing")
	g(descPoolMax, float64(s.MaxConns()))

	// Saturation.
	cnt(descPoolAcquires, float64(s.AcquireCount()))
	cnt(descPoolAcquireSeconds, s.AcquireDuration().Seconds())
	cnt(descPoolEmptyAcquires, float64(s.EmptyAcquireCount()))
	cnt(descPoolEmptyWaitSeconds, s.EmptyAcquireWaitTime().Seconds())

	// Errors.
	cnt(descPoolCanceledAcquires, float64(s.CanceledAcquireCount()))

	// Lifecycle.
	cnt(descPoolNewConns, float64(s.NewConnsCount()))
	cnt(descPoolDestroyed, float64(s.MaxLifetimeDestroyCount()), "max_lifetime")
	cnt(descPoolDestroyed, float64(s.MaxIdleDestroyCount()), "max_idle")
}
