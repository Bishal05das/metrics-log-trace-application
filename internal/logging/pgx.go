package logging

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueryLogger logs every database query, at DEBUG on success and ERROR on
// failure. It implements pgx.QueryTracer, the same interface metrics.DB uses.
//
// Why debug and not info: at 70 rps this service runs roughly 200 queries a
// second. Logging all of them at info costs far more than the metrics that
// already describe them in aggregate, and buries everything else. They are
// switched on for the minutes you need them, via the runtime level endpoint on
// the admin port, and switched off again.
//
// The pairing to understand:
//
//	orders_db_query_duration_seconds  tells you insert_order got slow.
//	these log lines                   tell you WHICH insert, for which request.
//
// The metric is always on and costs almost nothing. The log is off until the
// metric tells you where to look. That is the correct relationship between the
// two pillars, and it is why "just log everything and compute metrics from
// logs" is the expensive answer.
type QueryLogger struct {
	log *slog.Logger

	// SlowThreshold promotes a slow query to WARN regardless of level, so it is
	// recorded even when debug logging is off. A query crossing this line is
	// rare by construction, which is what makes it affordable to always log.
	SlowThreshold time.Duration

	// names maps SQL text to a short label, shared with the metrics tracer so
	// a log line and a metric sample agree on what to call a query.
	names map[string]string
}

func NewQueryLogger(log *slog.Logger, names map[string]string, slowThreshold time.Duration) *QueryLogger {
	if slowThreshold <= 0 {
		slowThreshold = 200 * time.Millisecond
	}

	// The KEYS must be normalised too, not just the lookup.
	//
	// store.QueryNames() returns the raw SQL constants, which are written
	// across indented lines. Normalising only the incoming SQL and not the map
	// means every lookup misses and every query logs as "other" — which is
	// exactly what happened the first time this ran. metrics.NewDB does the
	// same normalisation; the two must agree or a log line and a metric sample
	// will disagree about what to call the same query.
	normalised := make(map[string]string, len(names))
	for sql, name := range names {
		normalised[normalise(sql)] = name
	}

	return &QueryLogger{log: log, names: normalised, SlowThreshold: slowThreshold}
}

type queryLogKey struct{}

type queryLogVal struct {
	name  string
	start time.Time
}

func (q *QueryLogger) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name, ok := q.names[normalise(data.SQL)]
	if !ok {
		name = "other"
	}
	return context.WithValue(ctx, queryLogKey{}, queryLogVal{name: name, start: time.Now()})
}

func (q *QueryLogger) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	v, ok := ctx.Value(queryLogKey{}).(queryLogVal)
	if !ok {
		return
	}
	elapsed := time.Since(v.start)

	// NOTE: data.Args is deliberately NOT logged. Query arguments are the most
	// reliable place to find personal data, card numbers and tokens in a log
	// store — you would be logging the exact values someone submitted. The
	// named query plus the request ID is enough to identify what ran; if you
	// need the row, look it up by the ID.
	attrs := []slog.Attr{
		slog.String("query", v.name),
		slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
	}

	switch {
	case data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows):
		q.log.LogAttrs(ctx, slog.LevelError, "db query failed",
			append(attrs, slog.String("error", data.Err.Error()))...)

	case elapsed >= q.SlowThreshold:
		q.log.LogAttrs(ctx, slog.LevelWarn, "slow db query",
			append(attrs, slog.Duration("threshold", q.SlowThreshold))...)

	default:
		q.log.LogAttrs(ctx, slog.LevelDebug, "db query",
			append(attrs, slog.String("tag", data.CommandTag.String()))...)
	}
}

// normalise mirrors metrics.normaliseSQL so both tracers derive the same name
// from the same constant, regardless of how it is indented in source.
func normalise(sql string) string {
	fields := make([]byte, 0, len(sql))
	space := true
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !space {
				fields = append(fields, ' ')
				space = true
			}
			continue
		}
		fields = append(fields, c)
		space = false
	}
	return string(trimTrailingSpace(fields))
}

func trimTrailingSpace(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}
