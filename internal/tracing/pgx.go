package tracing

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// QueryTracer creates a CHILD SPAN for every database query and for every
// connection acquisition.
//
// This is the payoff of the whole design. It is the third implementation of
// pgx.QueryTracer in this project, alongside metrics.DB and
// logging.QueryLogger, and it plugs into the same store.MultiTracer without any
// of them knowing about each other:
//
//	metrics.DB          -> how long did queries take, in aggregate
//	logging.QueryLogger -> which query, for which request
//	tracing.QueryTracer -> where the time went inside ONE request
//
// Separating acquire from execute is what makes the Phase 3 lesson visible in a
// single waterfall rather than requiring you to correlate two metrics:
//
//	POST /orders ────────────────────────────────── 2.40s
//	  └─ db.acquire ──────────────────────────────  2.30s   ← the problem
//	  └─ db.query insert_order ─                    0.08s
//
// You do not need to know which metric to look at. The shape of the trace tells
// you immediately.
type QueryTracer struct {
	tracer trace.Tracer

	// names maps SQL text to a short label, shared with the other two tracers
	// so a span, a log line and a metric sample all call a query the same
	// thing. Span names are low-cardinality in every backend — the raw SQL must
	// never become one.
	names map[string]string
}

func NewQueryTracer(tracer trace.Tracer, names map[string]string) *QueryTracer {
	normalised := make(map[string]string, len(names))
	for sql, name := range names {
		normalised[normalise(sql)] = name
	}
	return &QueryTracer{tracer: tracer, names: normalised}
}

var (
	_ pgx.QueryTracer       = (*QueryTracer)(nil)
	_ pgxpool.AcquireTracer = (*QueryTracer)(nil)
)

type querySpanKey struct{}

func (q *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name, ok := q.names[normalise(data.SQL)]
	if !ok {
		name = "other"
	}

	ctx, span := q.tracer.Start(ctx, "db.query "+name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(name),
			attribute.String("db.query.name", name),
		),
	)

	// data.Args is NOT recorded, for the same reason the query logger omits it:
	// arguments are where personal data, card numbers and tokens live. Spans go
	// to a backend that is usually queryable by anyone on the team.
	//
	// The SQL text itself is also omitted. It is large, it is identical for
	// every execution of a named query, and the name already identifies it.

	return context.WithValue(ctx, querySpanKey{}, span)
}

func (q *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok {
		return
	}
	defer span.End()

	if data.CommandTag.String() != "" {
		span.SetAttributes(attribute.Int64("db.rows_affected", data.CommandTag.RowsAffected()))
	}

	if data.Err == nil || errors.Is(data.Err, pgx.ErrNoRows) {
		// ErrNoRows is a successful query that found nothing — marking it an
		// error would light up every "no such order" lookup in red, the same
		// classification decision the metrics layer makes.
		return
	}

	span.RecordError(data.Err)
	span.SetStatus(codes.Error, "query failed")

	// SQLSTATE is a closed set and highly actionable: 23505 is a client bug,
	// 40001 means retry, 53300 is infrastructure.
	var pgErr *pgconn.PgError
	if errors.As(data.Err, &pgErr) {
		span.SetAttributes(
			attribute.String("db.response.status_code", pgErr.Code),
			attribute.String("db.postgres.severity", pgErr.Severity),
		)
	}
}

type acquireSpanKey struct{}

// TraceAcquireStart/End produce the span that makes pool saturation obvious.
//
// Without it, a request that waited two seconds for a connection shows as a
// slow HTTP span with a fast query inside and an unexplained two-second gap.
// With it, the gap has a name.
func (q *QueryTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	ctx, span := q.tracer.Start(ctx, "db.acquire",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.DBSystemNamePostgreSQL),
	)
	return context.WithValue(ctx, acquireSpanKey{}, span)
}

func (q *QueryTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	span, ok := ctx.Value(acquireSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	defer span.End()

	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, "acquire failed")
	}
}

// normalise collapses whitespace so an indented SQL constant matches the map
// key regardless of formatting. Mirrors metrics.normaliseSQL and
// logging.normalise — all three must agree or the same query gets three
// different names across the three pillars.
func normalise(sql string) string {
	out := make([]byte, 0, len(sql))
	space := true
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; c {
		case ' ', '\t', '\n', '\r':
			if !space {
				out = append(out, ' ')
				space = true
			}
		default:
			out = append(out, c)
			space = false
		}
	}
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}
