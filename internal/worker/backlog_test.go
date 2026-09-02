package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
)

type fakeBacklogStore struct {
	counts map[domain.Status]int64
	oldest time.Duration
	err    error
}

func (f *fakeBacklogStore) CountOrdersByStatus(context.Context) (map[domain.Status]int64, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.counts, nil
}

func (f *fakeBacklogStore) OldestPendingAge(context.Context) (time.Duration, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.oldest, nil
}

type fakeBacklogMetrics struct {
	mu       sync.Mutex
	counts   map[string]int64
	oldest   time.Duration
	sets     int
	failures int
}

func (f *fakeBacklogMetrics) SetBacklog(counts map[string]int64, oldest time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts, f.oldest, f.sets = counts, oldest, f.sets+1
}

func (f *fakeBacklogMetrics) BacklogRefreshFailed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures++
}

func TestRefreshPublishesCountsAndAge(t *testing.T) {
	st := &fakeBacklogStore{
		counts: map[domain.Status]int64{domain.StatusPending: 42, domain.StatusPaid: 7},
		oldest: 90 * time.Second,
	}
	m := &fakeBacklogMetrics{}

	NewBacklogRefresher(st, m, testLogger(), time.Minute).refresh(context.Background())

	if m.counts["pending"] != 42 || m.counts["paid"] != 7 {
		t.Errorf("counts = %v", m.counts)
	}
	if m.oldest != 90*time.Second {
		t.Errorf("oldest = %v, want 90s", m.oldest)
	}
}

// TestRefreshFailureLeavesPreviousValues is the important one.
//
// On error we must NOT publish zeros. A zeroed backlog reads as "the queue
// cleared", which is the most dangerous possible lie for this metric — it
// turns a database outage into a green dashboard. The honest signal is the
// stale value plus a stale refresh timestamp.
func TestRefreshFailureLeavesPreviousValues(t *testing.T) {
	st := &fakeBacklogStore{counts: map[domain.Status]int64{domain.StatusPending: 500}}
	m := &fakeBacklogMetrics{}
	r := NewBacklogRefresher(st, m, testLogger(), time.Minute)

	r.refresh(context.Background())
	if m.counts["pending"] != 500 {
		t.Fatalf("setup failed: %v", m.counts)
	}

	st.err = errors.New("database unreachable")
	r.refresh(context.Background())

	if m.failures != 1 {
		t.Errorf("failures = %d, want 1", m.failures)
	}
	if m.sets != 1 {
		t.Errorf("SetBacklog called %d times; a failed refresh must not publish", m.sets)
	}
	if m.counts["pending"] != 500 {
		t.Errorf("previous value was overwritten on failure: %v", m.counts)
	}
}

// TestRunRefreshesImmediately — waiting a full interval before the first
// refresh means the gauges read "no data" on the first scrapes after a deploy.
func TestRunRefreshesImmediately(t *testing.T) {
	st := &fakeBacklogStore{counts: map[domain.Status]int64{domain.StatusPending: 1}}
	m := &fakeBacklogMetrics{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go NewBacklogRefresher(st, m, testLogger(), time.Hour).Run(ctx)

	deadline := time.After(time.Second)
	for {
		m.mu.Lock()
		sets := m.sets
		m.mu.Unlock()
		if sets > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("no refresh happened before the first tick")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
