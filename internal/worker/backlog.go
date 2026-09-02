package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
)

// BacklogStore is the read side used by the refresher.
type BacklogStore interface {
	CountOrdersByStatus(ctx context.Context) (map[domain.Status]int64, error)
	OldestPendingAge(ctx context.Context) (time.Duration, error)
}

// BacklogMetrics is the write side.
type BacklogMetrics interface {
	SetBacklog(counts map[string]int64, oldest time.Duration)
	BacklogRefreshFailed()
}

// BacklogRefresher periodically publishes the absolute backlog.
//
// This is the answer to "how do I expose a level, not a flow?", and the design
// deserves the argument behind it.
//
// The obvious approach is a custom Collector that runs the COUNT at scrape
// time, exactly like the pool collector in Phase 3. That is right for the pool
// — reading pgxpool.Stat() is a few atomic loads — and wrong here, because
// Collect() would run `SELECT count(*) ... GROUP BY status` against production
// Postgres on every scrape. The consequences compound:
//
//   - Every Prometheus replica scraping you multiplies the query load.
//   - A slow query blocks the whole /metrics response, so ALL your metrics go
//     missing exactly when the database is struggling and you need them most.
//   - Scrape timeouts turn into gaps in unrelated series.
//   - You cannot control the frequency: it is whatever the scrape interval is,
//     across however many scrapers.
//
// So we decouple: our own ticker, our own interval, results cached in gauges.
// The cost is staleness, and the price of staleness is that you must EXPORT
// the staleness — hence last_refresh_timestamp_seconds. A cached gauge with no
// freshness signal is worse than no gauge, because a frozen plausible number
// reads as calm.
//
// The rule: read state at scrape time when it is cheap and local; refresh
// out-of-band when it is expensive or remote, and publish the refresh time.
type BacklogRefresher struct {
	store    BacklogStore
	metrics  BacklogMetrics
	log      *slog.Logger
	interval time.Duration
	timeout  time.Duration
}

func NewBacklogRefresher(s BacklogStore, m BacklogMetrics, log *slog.Logger, interval time.Duration) *BacklogRefresher {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &BacklogRefresher{
		store:    s,
		metrics:  m,
		log:      log,
		interval: interval,
		// Bounded so a wedged database cannot pile up refresh goroutines.
		timeout: 5 * time.Second,
	}
}

func (b *BacklogRefresher) Run(ctx context.Context) {
	// Refresh once immediately so the gauges are populated from the first
	// scrape rather than after a full interval of "no data".
	b.refresh(ctx)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	b.log.Info("backlog refresher started", "interval", b.interval)

	for {
		select {
		case <-ctx.Done():
			b.log.Info("backlog refresher stopped")
			return
		case <-ticker.C:
			b.refresh(ctx)
		}
	}
}

func (b *BacklogRefresher) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	counts, err := b.store.CountOrdersByStatus(ctx)
	if err != nil {
		if ctx.Err() == nil || errorsIsRealFailure(ctx) {
			b.log.ErrorContext(ctx, "backlog count failed", "error", err)
		}
		b.metrics.BacklogRefreshFailed()
		return
	}

	oldest, err := b.store.OldestPendingAge(ctx)
	if err != nil {
		b.log.ErrorContext(ctx, "oldest pending age failed", "error", err)
		b.metrics.BacklogRefreshFailed()
		return
	}

	// On failure we deliberately leave the previous gauge values in place
	// rather than zeroing them. Zeroing would look like "the backlog cleared",
	// which is the most dangerous possible lie for this metric. The stale
	// value plus a stale refresh timestamp is honest: the number is old, and
	// the freshness alert will say so.
	byName := make(map[string]int64, len(counts))
	for status, n := range counts {
		byName[string(status)] = n
	}
	b.metrics.SetBacklog(byName, oldest)
}

// errorsIsRealFailure suppresses log noise during shutdown, when the parent
// context is already cancelled.
func errorsIsRealFailure(ctx context.Context) bool {
	return ctx.Err() != context.Canceled
}
