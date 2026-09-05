package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MultiTracer fans one pgx tracing callback out to several tracers.
//
// It exists because pgx has exactly ONE slot — ConnConfig.Tracer — and we have
// two things to attach: the metrics tracer and the query logger. Rather than
// merging them into one type (which would make the metrics package depend on
// the logging package for no good reason), they stay independent and this
// composes them.
//
// The subtlety is TraceQueryStart: each tracer returns a DERIVED context
// carrying its own private key, so the contexts must be CHAINED, not
// discarded. Pass the same original ctx to each and the second tracer's value
// overwrites nothing but the first one's is lost from the chain that reaches
// TraceQueryEnd — the metrics would silently stop recording.
type MultiTracer []pgx.QueryTracer

var (
	_ pgx.QueryTracer       = (MultiTracer)(nil)
	_ pgxpool.AcquireTracer = (MultiTracer)(nil)
)

func (m MultiTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	for _, t := range m {
		ctx = t.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}

func (m MultiTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	for _, t := range m {
		t.TraceQueryEnd(ctx, conn, data)
	}
}

// TraceAcquireStart / TraceAcquireEnd forward only to members that implement
// pgxpool.AcquireTracer.
//
// This matters because of HOW pgxpool discovers acquire tracing: it type-asserts
// ConnConfig.Tracer against pgxpool.AcquireTracer. Without these methods on
// MultiTracer the assertion fails, and the acquire-wait histogram — the single
// most valuable database metric in this project, per Phase 3 — silently stops
// being recorded. Wrapping a tracer is exactly where that kind of capability
// gets dropped by accident.
func (m MultiTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
	for _, t := range m {
		if at, ok := t.(pgxpool.AcquireTracer); ok {
			ctx = at.TraceAcquireStart(ctx, pool, data)
		}
	}
	return ctx
}

func (m MultiTracer) TraceAcquireEnd(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	for _, t := range m {
		if at, ok := t.(pgxpool.AcquireTracer); ok {
			at.TraceAcquireEnd(ctx, pool, data)
		}
	}
}
