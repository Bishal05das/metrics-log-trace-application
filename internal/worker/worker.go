// Package worker processes pending orders in the background.
//
// It exists in this project for a specific reason: everything instrumented so
// far has been request-driven, and RED only applies to things that receive
// requests. A background loop has a different shape — nobody is waiting on the
// other end of a socket, so if it stops, no error rate rises and no latency
// panel moves. It just quietly stops working.
//
// That makes a worker the place where "what do I measure?" actually requires
// thought, and where the answer is usually: throughput, backlog, and liveness.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
)

// Store is the narrow slice of the database the worker needs.
//
// Consumer-side interface again (same reasoning as metrics.PoolStats): it
// keeps this package testable without a running Postgres, which is what makes
// the retry and failure paths practical to cover.
type Store interface {
	ClaimPendingOrders(ctx context.Context, limit int) ([]domain.Order, error)
	MarkOrder(ctx context.Context, id uuid.UUID, status domain.Status) error
}

// Metrics is what the worker reports. Also an interface, so tests can assert
// on calls rather than scraping a registry.
type Metrics interface {
	Transition(from, to string)
	OrderProcessed(outcome string, d time.Duration)
	WorkerRun(result string, batchSize int, d time.Duration)
}

// Processor decides an order's fate. Injectable so tests are deterministic —
// a random failure rate in a unit test is a flaky test.
type Processor func(ctx context.Context, o domain.Order) (domain.Status, error)

type Config struct {
	Interval  time.Duration
	BatchSize int

	// FailureRate and Latency drive the default simulated processor.
	FailureRate float64
	MinLatency  time.Duration
	MaxLatency  time.Duration

	// Tracer is optional. Background work produces ROOT spans — there is no
	// incoming request to be a child of — so each batch becomes its own trace.
	// That is what makes "why did this order take 40 seconds to process?"
	// answerable at all; the HTTP trace ended when the order was accepted.
	Tracer trace.Tracer
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 500 * time.Millisecond
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.MinLatency <= 0 {
		c.MinLatency = 5 * time.Millisecond
	}
	if c.MaxLatency <= c.MinLatency {
		c.MaxLatency = c.MinLatency + 45*time.Millisecond
	}
	if c.Tracer == nil {
		c.Tracer = noop.NewTracerProvider().Tracer("worker")
	}
	return c
}

type Worker struct {
	store   Store
	metrics Metrics
	log     *slog.Logger
	cfg     Config
	process Processor
}

func New(s Store, m Metrics, log *slog.Logger, cfg Config) *Worker {
	cfg = cfg.withDefaults()
	w := &Worker{store: s, metrics: m, log: log, cfg: cfg}
	w.process = w.simulatePayment
	return w
}

// WithProcessor overrides the payment simulation. Used by tests.
func (w *Worker) WithProcessor(p Processor) *Worker {
	w.process = p
	return w
}

// Run loops until ctx is cancelled. It returns only after the in-flight batch
// finishes, so a shutdown never leaves orders stranded in `processing`.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	w.log.Info("worker started",
		"interval", w.cfg.Interval, "batch_size", w.cfg.BatchSize)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

// runOnce claims a batch and processes it.
//
// Every exit path records a WorkerRun. That completeness is the point: the
// counter's rate() is the worker's HEARTBEAT, and a heartbeat with holes in it
// is indistinguishable from a worker that died.
func (w *Worker) runOnce(ctx context.Context) {
	start := time.Now()

	ctx, span := w.cfg.Tracer.Start(ctx, "worker.batch", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()

	orders, err := w.store.ClaimPendingOrders(ctx, w.cfg.BatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a failure
		}
		w.log.ErrorContext(ctx, "claim pending orders failed", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "claim failed")
		w.metrics.WorkerRun("error", 0, time.Since(start))
		return
	}

	if len(orders) == 0 {
		// An idle tick is a distinct, healthy result — not an error, and not
		// nothing. Separating "empty" from "ok" lets you tell "the worker is
		// keeping up" apart from "the worker has stopped receiving work",
		// which look identical if you only count successes.
		span.SetAttributes(attribute.Int("batch.size", 0))
		w.metrics.WorkerRun("empty", 0, time.Since(start))
		return
	}

	span.SetAttributes(attribute.Int("batch.size", len(orders)))

	// The claim already committed pending → processing for the whole batch, so
	// record those transitions up front rather than per-order below.
	for range orders {
		w.metrics.Transition(string(domain.StatusPending), string(domain.StatusProcessing))
	}

	for _, o := range orders {
		w.processOne(ctx, o)
	}

	w.metrics.WorkerRun("ok", len(orders), time.Since(start))
}

func (w *Worker) processOne(ctx context.Context, o domain.Order) {
	start := time.Now()

	ctx, span := w.cfg.Tracer.Start(ctx, "worker.process_order",
		trace.WithAttributes(attribute.String("order.id", o.ID.String())))
	defer span.End()

	// The detached write context is built FIRST, because every exit path below
	// needs it — including the cancellation path.
	//
	// The claim already committed this order into `processing`, so from here on
	// this worker owns a row that no one else will touch. Returning without
	// writing something strands it there permanently. A `context.WithoutCancel`
	// write is the only way to finish the job once the parent context is gone,
	// and it has to be reachable from the cancellation branch — an earlier
	// version created it below that branch, which meant the one shutdown path
	// it existed to protect was the one path that skipped it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	outcome, err := w.process(ctx, o)

	if err != nil && ctx.Err() != nil {
		// Shutdown interrupted the attempt. The work genuinely did not happen,
		// so the order goes BACK to `pending` to be picked up after restart.
		//
		// Marking it `failed` would be a lie — it would claim a payment failed
		// that was never attempted, and it is a terminal state, so the order
		// would never be retried. Releasing is the honest, recoverable choice.
		if relErr := w.store.MarkOrder(writeCtx, o.ID, domain.StatusPending); relErr != nil {
			w.log.Error("releasing interrupted order failed",
				"order_id", o.ID, "error", relErr)
			return
		}
		w.log.Info("released interrupted order back to pending", "order_id", o.ID)
		span.SetStatus(codes.Error, "interrupted; released to pending")
		w.metrics.Transition(string(domain.StatusProcessing), string(domain.StatusPending))
		return
	}

	if err != nil {
		w.log.ErrorContext(ctx, "processing order failed", "order_id", o.ID, "error", err)
		outcome = domain.StatusFailed
	}

	if err := w.store.MarkOrder(writeCtx, o.ID, outcome); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Another worker got there first. Expected under concurrency, not
			// an error worth alerting on.
			w.log.DebugContext(ctx, "order already finalised", "order_id", o.ID)
			return
		}
		w.log.ErrorContext(ctx, "marking order failed", "order_id", o.ID, "error", err)
		return
	}

	span.SetAttributes(attribute.String("order.outcome", string(outcome)))
	if outcome == domain.StatusFailed {
		span.SetStatus(codes.Error, "payment failed")
	}
	w.metrics.Transition(string(domain.StatusProcessing), string(outcome))
	w.metrics.OrderProcessed(string(outcome), time.Since(start))
}

// simulatePayment stands in for a real payment gateway call.
func (w *Worker) simulatePayment(ctx context.Context, _ domain.Order) (domain.Status, error) {
	spread := w.cfg.MaxLatency - w.cfg.MinLatency
	delay := w.cfg.MinLatency + time.Duration(rand.Int63n(int64(spread)))

	select {
	case <-ctx.Done():
		return domain.StatusPending, ctx.Err()
	case <-time.After(delay):
	}

	if rand.Float64() < w.cfg.FailureRate {
		return domain.StatusFailed, nil
	}
	return domain.StatusPaid, nil
}
