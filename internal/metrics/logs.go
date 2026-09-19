package metrics

import "github.com/prometheus/client_golang/prometheus"

// Logs measures the LOGGING pillar from inside the metrics pillar.
//
// This is the same principle as scraping Elasticsearch: a pillar cannot report
// its own death. `sampled_dropped` already rides along inside log documents,
// but that number only exists once a document has been written, shipped,
// parsed and indexed — so the moment shipping breaks, the measurement of your
// log volume breaks with it, and you lose the number exactly when it would have
// told you something.
//
// These two counters sit upstream of all of that. They cost an atomic increment
// each, they survive Elasticsearch being down, and they make log volume
// alertable with the same `rate()` you use for everything else:
//
//	# what fraction of records are we discarding?
//	sum(rate(orders_log_records_dropped_total[5m]))
//	  / clamp_min(sum(rate(orders_log_records_total[5m])), 1e-9)
//
//	# did someone leave DEBUG on after an incident?
//	sum(rate(orders_log_records_total{level="DEBUG"}[5m])) > 0
type Logs struct {
	records *prometheus.CounterVec // level
	dropped *prometheus.CounterVec // level
}

// logLevels is the closed set slog produces. Enumerated so both counters exist
// at zero from the first scrape — a CounterVec with no children emits nothing
// at all, and a ratio whose denominator is absent returns no data rather than
// zero.
var logLevels = []string{"DEBUG", "INFO", "WARN", "ERROR"}

func NewLogs(reg prometheus.Registerer) *Logs {
	l := &Logs{
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "log",
			Name:      "records_total",
			Help:      "Log records emitted by the application, by level, counted before sampling.",
		}, []string{"level"}),

		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "log",
			Name:      "records_dropped_total",
			Help:      "Log records discarded by the sampler, by level. Always 0 for WARN and above.",
		}, []string{"level"}),
	}

	reg.MustRegister(l.records, l.dropped)

	for _, lvl := range logLevels {
		l.records.WithLabelValues(lvl)
		l.dropped.WithLabelValues(lvl)
	}
	return l
}

// LogRecord and LogRecordDropped satisfy logging.Recorder.
//
// `level` is the only label, and that is a deliberate limit. Sampling is keyed
// by MESSAGE, so a message label would be the natural thing to add — and it
// would be a cardinality bomb: log messages are supposed to be static strings,
// but one caller interpolating an order ID into a message would create a series
// per order, permanently. The bounded four-value level label answers the
// operational question ("how much, and is DEBUG on?"); Elasticsearch answers
// the detailed one ("which message?"), which is exactly the division of labour
// between the two pillars.
func (l *Logs) LogRecord(level string) { l.records.WithLabelValues(level).Inc() }

func (l *Logs) LogRecordDropped(level string) { l.dropped.WithLabelValues(level).Inc() }
