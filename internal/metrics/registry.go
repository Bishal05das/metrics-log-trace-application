// Package metrics owns the Prometheus registry and every metric this service
// exposes.
//
// Design rule for this project: metrics are *defined* here, in one place, and
// handed to the code that updates them. They are never created ad hoc deep
// inside a handler. That way the complete list of series this binary can
// produce is auditable by reading one package.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Namespace prefixes every metric we define ourselves, giving us
// `orders_http_requests_total` rather than a bare `http_requests_total` that
// could collide with some library's idea of the same name.
//
// Metrics that come from shared collectors (go_*, process_*, promhttp_*) keep
// their conventional names — those are cross-service standards and tooling
// expects them verbatim.
const Namespace = "orders"

// New builds the registry for this process.
//
// We deliberately do NOT use prometheus.DefaultRegisterer. The default
// registry is a package-level global: any dependency you pull in can register
// metrics onto it without you knowing, MustRegister panics become
// action-at-a-distance, and tests leak state into each other. A registry you
// construct and pass explicitly is testable and has no surprises.
func New() *prometheus.Registry {
	reg := prometheus.NewRegistry()

	// --- Go runtime metrics -------------------------------------------------
	// Free, and genuinely load-bearing in production: goroutine leaks, GC
	// pressure and heap growth all show up here long before they page you.
	//
	// The base collector gives you go_goroutines, go_memstats_*, go_threads.
	// The rule sets below opt in to the newer runtime/metrics-backed series.
	// MetricsAll exists but adds a lot of series you will never query — this
	// is the first cost/benefit tradeoff of instrumentation: every metric you
	// enable costs memory here, network on scrape, and disk in Prometheus,
	// forever.
	reg.MustRegister(collectors.NewGoCollector(
		collectors.WithGoCollectorRuntimeMetrics(
			collectors.MetricsGC,        // /gc/... — pause times, cycles
			collectors.MetricsScheduler, // /sched/... — goroutine scheduling latency
		),
	))

	// --- Process metrics ----------------------------------------------------
	// Read from /proc on Linux: CPU seconds, resident memory, open file
	// descriptors, start time. process_open_fds vs process_max_fds is one of
	// the highest-value cheap alerts you can have.
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return reg
}

// BuildInfo registers the standard "info metric" pattern.
//
// The trick: a gauge whose value is always 1, carrying the interesting data in
// its labels. You never graph its value — you join on it. In PromQL:
//
//	sum(rate(orders_http_requests_total[5m])) by (instance)
//	  * on(instance) group_left(version) orders_build_info
//
// which annotates a rate with the version that produced it, so you can see a
// bad deploy in the graph. Putting `version` directly on every metric instead
// would double your series count on every release.
func BuildInfo(reg prometheus.Registerer, version, commit, goVersion, env string) {
	g := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "build_info",
			Help:      "Build metadata. Always 1; the information is in the labels.",
		},
		[]string{"version", "commit", "go_version", "env"},
	)
	reg.MustRegister(g)
	g.WithLabelValues(version, commit, goVersion, env).Set(1)
}
