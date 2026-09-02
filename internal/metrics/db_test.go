package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The SQL constants in the store are written across indented lines. The tracer
// receives them verbatim, so name lookup must survive that formatting.
const multilineSQL = `
		SELECT id::text, customer_id
		FROM orders
		WHERE id = $1::uuid`

func TestQueryNameIsBoundedAndFormatInsensitive(t *testing.T) {
	d := NewDB(prometheus.NewRegistry(), map[string]string{
		multilineSQL: "get_order_by_id",
	})

	// Same query, reformatted on one line — must still resolve.
	oneLine := "SELECT id::text, customer_id FROM orders WHERE id = $1::uuid"
	if got := d.queryName(oneLine); got != "get_order_by_id" {
		t.Errorf("reformatted SQL: got %q, want get_order_by_id", got)
	}
	if got := d.queryName(multilineSQL); got != "get_order_by_id" {
		t.Errorf("original SQL: got %q, want get_order_by_id", got)
	}

	// THE important case: an unregistered query must never become a label.
	// If this returned the SQL text, a query built by string concatenation
	// would produce unbounded cardinality.
	adhoc := "SELECT * FROM orders WHERE customer_id = 'cust-8123'"
	if got := d.queryName(adhoc); got != "other" {
		t.Errorf("unregistered SQL leaked into a label: %q", got)
	}
}

func TestIsRealErrorIgnoresNoRows(t *testing.T) {
	// "No such order" is a successful query answering a legitimate question.
	// Counting it as a database error makes your error rate track how often
	// clients request missing things.
	if isRealError(pgx.ErrNoRows) {
		t.Error("pgx.ErrNoRows must not count as a database error")
	}
	if isRealError(fmt.Errorf("wrapped: %w", pgx.ErrNoRows)) {
		t.Error("wrapped ErrNoRows must not count as a database error")
	}
	if isRealError(nil) {
		t.Error("nil is not an error")
	}
	if !isRealError(errors.New("connection reset")) {
		t.Error("a real failure must count")
	}
}

func TestErrorKindUsesSQLSTATE(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped canceled", fmt.Errorf("query: %w", context.Canceled), "canceled"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"unique violation", &pgconn.PgError{Code: "23505"}, "23505"},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, "40001"},
		{"too many connections", &pgconn.PgError{Code: "53300"}, "53300"},
		{"unknown", errors.New("boom"), "other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errorKind(c.err); got != c.want {
				t.Errorf("errorKind = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTraceQueryRecordsDurationAndStatus drives the tracer directly, the same
// way pgx would.
func TestTraceQueryRecordsDurationAndStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	d := NewDB(reg, map[string]string{"SELECT 1": "select_one"})

	// Success.
	ctx := d.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	d.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	// ErrNoRows — still status "ok", and no error counted.
	ctx = d.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	d.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})

	// A real failure.
	ctx = d.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	d.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505"}})

	if n := testutil.CollectAndCount(reg, "orders_db_query_duration_seconds"); n != 2 {
		t.Errorf("want 2 duration series (ok + error), got %d", n)
	}
	if got := testutil.ToFloat64(d.queryErrors.WithLabelValues("select_one", "23505")); got != 1 {
		t.Errorf("unique-violation counter = %v, want 1", got)
	}
	// ErrNoRows must not have incremented any error series.
	if n := testutil.CollectAndCount(reg, "orders_db_query_errors_total"); n != 1 {
		t.Errorf("want exactly 1 error series, got %d", n)
	}
}

// TestTraceQueryEndWithoutStart guards against a panic if the context is lost.
func TestTraceQueryEndWithoutStart(t *testing.T) {
	reg := prometheus.NewRegistry()
	d := NewDB(reg, nil)

	d.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{})

	if n := testutil.CollectAndCount(reg, "orders_db_query_duration_seconds"); n != 0 {
		t.Errorf("End without Start should record nothing, got %d series", n)
	}
}

func TestTraceAcquireRecordsWait(t *testing.T) {
	reg := prometheus.NewRegistry()
	d := NewDB(reg, nil)

	ctx := d.TraceAcquireStart(context.Background(), nil, pgxpool.TraceAcquireStartData{})
	d.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{})

	ctx = d.TraceAcquireStart(context.Background(), nil, pgxpool.TraceAcquireStartData{})
	d.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: context.DeadlineExceeded})

	if got := testutil.ToFloat64(d.acquireErrors); got != 1 {
		t.Errorf("acquire errors = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(reg, "orders_db_pool_acquire_wait_seconds"); n != 1 {
		t.Errorf("want 1 acquire-wait histogram, got %d", n)
	}
}

// fakeStats implements PoolStats — the reason the collector takes an interface
// rather than *pgxpool.Stat, which cannot be constructed outside the driver.
type fakeStats struct{}

func (fakeStats) AcquiredConns() int32                { return 7 }
func (fakeStats) IdleConns() int32                    { return 3 }
func (fakeStats) ConstructingConns() int32            { return 1 }
func (fakeStats) MaxConns() int32                     { return 10 }
func (fakeStats) AcquireCount() int64                 { return 1234 }
func (fakeStats) AcquireDuration() time.Duration      { return 2500 * time.Millisecond }
func (fakeStats) EmptyAcquireCount() int64            { return 42 }
func (fakeStats) EmptyAcquireWaitTime() time.Duration { return 1500 * time.Millisecond }
func (fakeStats) CanceledAcquireCount() int64         { return 5 }
func (fakeStats) NewConnsCount() int64                { return 11 }
func (fakeStats) MaxLifetimeDestroyCount() int64      { return 2 }
func (fakeStats) MaxIdleDestroyCount() int64          { return 4 }

func TestPoolCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterPool(reg, func() PoolStats { return fakeStats{} })

	want := `
# HELP orders_db_pool_connections Connections in the pool by state.
# TYPE orders_db_pool_connections gauge
orders_db_pool_connections{state="acquired"} 7
orders_db_pool_connections{state="constructing"} 1
orders_db_pool_connections{state="idle"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "orders_db_pool_connections"); err != nil {
		t.Error(err)
	}

	if got := testutil.CollectAndCount(reg, "orders_db_pool_empty_acquires_total"); got != 1 {
		t.Errorf("empty_acquires series: got %d", got)
	}
}

// TestPoolCollectorHandlesNilStats — a scrape landing after the pool closes
// must not panic and take the metrics endpoint down with it.
func TestPoolCollectorHandlesNilStats(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterPool(reg, func() PoolStats { return nil })

	if n := testutil.CollectAndCount(reg, "orders_db_pool_connections"); n != 0 {
		t.Errorf("want 0 series from a nil pool, got %d", n)
	}
}

func TestDBLatencyBucketsAreFinerThanHTTP(t *testing.T) {
	// Queries are an order of magnitude faster than requests. Reusing the HTTP
	// buckets would land every healthy query in the first bucket.
	if DBLatencyBuckets[0] >= LatencyBuckets[0] {
		t.Fatalf("db first bucket %v must be finer than http first bucket %v",
			DBLatencyBuckets[0], LatencyBuckets[0])
	}
}
