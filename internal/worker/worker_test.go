package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- fakes -----------------------------------------------------------------

type fakeStore struct {
	mu       sync.Mutex
	pending  []domain.Order
	marked   map[uuid.UUID]domain.Status
	claimErr error
	markErr  error
}

func newFakeStore(orders ...domain.Order) *fakeStore {
	return &fakeStore{pending: orders, marked: map[uuid.UUID]domain.Status{}}
}

func (f *fakeStore) ClaimPendingOrders(_ context.Context, limit int) ([]domain.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	n := min(limit, len(f.pending))
	out := f.pending[:n]
	f.pending = f.pending[n:]
	return out, nil
}

func (f *fakeStore) MarkOrder(_ context.Context, id uuid.UUID, status domain.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.marked[id] = status
	return nil
}

type recordedRun struct {
	result string
	size   int
}

type fakeMetrics struct {
	mu          sync.Mutex
	transitions map[[2]string]int
	processed   map[string]int
	runs        []recordedRun
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{
		transitions: map[[2]string]int{},
		processed:   map[string]int{},
	}
}

func (f *fakeMetrics) Transition(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions[[2]string{from, to}]++
}

func (f *fakeMetrics) OrderProcessed(outcome string, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processed[outcome]++
}

func (f *fakeMetrics) WorkerRun(result string, size int, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, recordedRun{result, size})
}

func order() domain.Order {
	return domain.Order{
		ID: uuid.New(), CustomerID: "c1", Status: domain.StatusPending,
		AmountCents: 1000, Currency: "USD", CreatedAt: time.Now(),
	}
}

// --- tests -----------------------------------------------------------------

func TestRunOnceRecordsFullTransitionChain(t *testing.T) {
	o1, o2 := order(), order()
	st := newFakeStore(o1, o2)
	m := newFakeMetrics()

	w := New(st, m, testLogger(), Config{BatchSize: 10}).
		WithProcessor(func(context.Context, domain.Order) (domain.Status, error) {
			return domain.StatusPaid, nil
		})

	w.runOnce(context.Background())

	if got := m.transitions[[2]string{"pending", "processing"}]; got != 2 {
		t.Errorf("pending→processing = %d, want 2", got)
	}
	if got := m.transitions[[2]string{"processing", "paid"}]; got != 2 {
		t.Errorf("processing→paid = %d, want 2", got)
	}
	if got := m.processed["paid"]; got != 2 {
		t.Errorf("processed paid = %d, want 2", got)
	}
	if st.marked[o1.ID] != domain.StatusPaid || st.marked[o2.ID] != domain.StatusPaid {
		t.Error("orders were not persisted as paid")
	}
}

// TestEmptyTickIsItsOwnResult — "the worker is keeping up" and "the worker has
// stopped receiving work" look identical if you only count successes.
func TestEmptyTickIsItsOwnResult(t *testing.T) {
	m := newFakeMetrics()
	w := New(newFakeStore(), m, testLogger(), Config{})

	w.runOnce(context.Background())

	if len(m.runs) != 1 || m.runs[0].result != "empty" {
		t.Fatalf("want one 'empty' run, got %+v", m.runs)
	}
	if m.runs[0].size != 0 {
		t.Errorf("empty run batch size = %d, want 0", m.runs[0].size)
	}
}

// TestClaimFailureIsRecorded — every exit path must record a run, or the
// heartbeat has holes and a failing worker looks like a dead one.
func TestClaimFailureIsRecorded(t *testing.T) {
	st := newFakeStore()
	st.claimErr = errors.New("connection refused")
	m := newFakeMetrics()

	New(st, m, testLogger(), Config{}).runOnce(context.Background())

	if len(m.runs) != 1 || m.runs[0].result != "error" {
		t.Fatalf("want one 'error' run, got %+v", m.runs)
	}
}

func TestProcessorErrorMarksFailed(t *testing.T) {
	o := order()
	st := newFakeStore(o)
	m := newFakeMetrics()

	w := New(st, m, testLogger(), Config{}).
		WithProcessor(func(context.Context, domain.Order) (domain.Status, error) {
			return "", errors.New("gateway timeout")
		})

	w.runOnce(context.Background())

	if st.marked[o.ID] != domain.StatusFailed {
		t.Errorf("order status = %q, want failed", st.marked[o.ID])
	}
	if got := m.transitions[[2]string{"processing", "failed"}]; got != 1 {
		t.Errorf("processing→failed = %d, want 1", got)
	}
}

// TestConflictIsNotCountedAsTransition — another replica finalised it first.
// Expected under concurrency; counting it would double-count the outcome.
func TestConflictIsNotCountedAsTransition(t *testing.T) {
	st := newFakeStore(order())
	st.markErr = domain.ErrConflict
	m := newFakeMetrics()

	w := New(st, m, testLogger(), Config{}).
		WithProcessor(func(context.Context, domain.Order) (domain.Status, error) {
			return domain.StatusPaid, nil
		})

	w.runOnce(context.Background())

	if got := m.transitions[[2]string{"processing", "paid"}]; got != 0 {
		t.Errorf("a conflicting mark must not record a transition, got %d", got)
	}
	if got := m.processed["paid"]; got != 0 {
		t.Errorf("a conflicting mark must not record processing, got %d", got)
	}
}

// TestOutcomeIsPersistedEvenWhenContextCancelled is the shutdown-safety test.
// Without context.WithoutCancel in processOne, a SIGTERM mid-batch leaves
// orders stranded in `processing` with no worker owning them — a real
// background-job leak that only shows up during deploys.
func TestOutcomeIsPersistedEvenWhenContextCancelled(t *testing.T) {
	o := order()
	st := newFakeStore(o)
	m := newFakeMetrics()

	ctx, cancel := context.WithCancel(context.Background())

	w := New(st, m, testLogger(), Config{}).
		WithProcessor(func(context.Context, domain.Order) (domain.Status, error) {
			cancel() // shutdown lands mid-processing
			return domain.StatusPaid, nil
		})

	w.runOnce(ctx)

	if st.marked[o.ID] != domain.StatusPaid {
		t.Fatalf("outcome lost on shutdown: order is %q, want paid", st.marked[o.ID])
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	m := newFakeMetrics()
	w := New(newFakeStore(), m, testLogger(), Config{Interval: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
