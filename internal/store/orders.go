package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
)

// Every query below is written as a named constant. That is not just style:
// in Phase 3 we attach a `query` label to the DB duration histogram, and the
// label value is the constant's name. One constant == one label value keeps
// that metric's cardinality fixed and auditable.

const (
	qInsertOrder = `
		INSERT INTO orders (id, customer_id, status, amount_cents, currency, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`

	qGetOrderByID = `
		SELECT id::text, customer_id, status, amount_cents, currency, created_at, updated_at
		FROM orders
		WHERE id = $1::uuid`

	qListOrders = `
		SELECT id::text, customer_id, status, amount_cents, currency, created_at, updated_at
		FROM orders
		WHERE ($1::text IS NULL OR status = $1::text)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`

	qCountOrdersByStatus = `
		SELECT status, count(*) FROM orders GROUP BY status`

	// Claim a batch of pending orders for this worker.
	//
	// FOR UPDATE SKIP LOCKED is the standard "queue in Postgres" pattern: rows
	// already locked by another worker are skipped rather than waited on, so N
	// replicas can drain the same table concurrently without blocking each
	// other or double-processing. The UPDATE ... RETURNING makes claiming and
	// reading a single atomic round trip.
	qClaimPending = `
		UPDATE orders SET status = 'processing', updated_at = now()
		WHERE id IN (
			SELECT id FROM orders
			WHERE status = 'pending'
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, customer_id, status, amount_cents, currency, created_at, updated_at`

	// The status guard makes this idempotent: a retry after a partial failure
	// cannot move an order out of a terminal state.
	qMarkOrderStatus = `
		UPDATE orders SET status = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'processing'`

	qOldestPendingAge = `
		SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at))), 0)
		FROM orders WHERE status = 'pending'`

	// Fault injection, dev only. Used to saturate the connection pool on
	// demand so that pool metrics can be observed doing something.
	qSleep = `SELECT pg_sleep($1)`
)

// QueryNames maps SQL text to the short label used on database metrics.
//
// This map IS the cardinality budget for orders_db_query_duration_seconds:
// the label can only ever take one of these values, plus "other". Adding a
// query without adding it here is not a silent failure — it shows up as
// "other" in the dashboard, which is a visible prompt rather than an outage.
//
// Never label with the SQL text itself. It is enormous for a label value, and
// any query assembled by string concatenation would produce an unbounded set.
func QueryNames() map[string]string {
	return map[string]string{
		qInsertOrder:         "insert_order",
		qGetOrderByID:        "get_order_by_id",
		qListOrders:          "list_orders",
		qCountOrdersByStatus: "count_orders_by_status",
		qClaimPending:        "claim_pending_orders",
		qMarkOrderStatus:     "mark_order_status",
		qOldestPendingAge:    "oldest_pending_age",
		qSleep:               "sleep",
	}
}

// ClaimPendingOrders atomically moves up to limit pending orders into
// processing and returns them.
func (s *Store) ClaimPendingOrders(ctx context.Context, limit int) ([]domain.Order, error) {
	rows, err := s.pool.Query(ctx, qClaimPending, limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim pending: %w", err)
	}
	defer rows.Close()

	out := make([]domain.Order, 0, limit)
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan claimed order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkOrder moves an order out of processing into a terminal state.
func (s *Store) MarkOrder(ctx context.Context, id uuid.UUID, status domain.Status) error {
	tag, err := s.pool.Exec(ctx, qMarkOrderStatus, id.String(), string(status))
	if err != nil {
		return fmt.Errorf("store: mark order: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Someone else already moved it, or it was never claimed by us.
		return domain.ErrConflict
	}
	return nil
}

// OldestPendingAge returns how long the oldest pending order has been waiting,
// or zero if there are none.
func (s *Store) OldestPendingAge(ctx context.Context) (time.Duration, error) {
	var seconds float64
	if err := s.pool.QueryRow(ctx, qOldestPendingAge).Scan(&seconds); err != nil {
		return 0, fmt.Errorf("store: oldest pending age: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// Sleep runs pg_sleep on a pooled connection, holding it for the duration.
// Dev-only fault injection: it is the simplest way to make a 10-connection
// pool saturate on demand.
func (s *Store) Sleep(ctx context.Context, d time.Duration) error {
	_, err := s.pool.Exec(ctx, qSleep, d.Seconds())
	if err != nil {
		return fmt.Errorf("store: sleep: %w", err)
	}
	return nil
}

func (s *Store) CreateOrder(ctx context.Context, o domain.Order) error {
	_, err := s.pool.Exec(ctx, qInsertOrder,
		o.ID.String(), o.CustomerID, string(o.Status),
		o.AmountCents, o.Currency, o.CreatedAt, o.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: create order: %w", err)
	}
	return nil
}

func (s *Store) GetOrder(ctx context.Context, id uuid.UUID) (domain.Order, error) {
	row := s.pool.QueryRow(ctx, qGetOrderByID, id.String())
	o, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("store: get order: %w", err)
	}
	return o, nil
}

// ListOrders returns a page of orders. status may be empty to mean "any".
func (s *Store) ListOrders(ctx context.Context, status domain.Status, limit, offset int) ([]domain.Order, error) {
	var statusArg *string
	if status != "" {
		v := string(status)
		statusArg = &v
	}

	rows, err := s.pool.Query(ctx, qListOrders, statusArg, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list orders: %w", err)
	}
	defer rows.Close()

	out := make([]domain.Order, 0, limit)
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan order: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list orders: %w", err)
	}
	return out, nil
}

// CountOrdersByStatus backs a gauge in Phase 4: the number of orders currently
// sitting in each state. This is a "state of the world" number, which is why it
// will be a gauge and not a counter.
func (s *Store) CountOrdersByStatus(ctx context.Context) (map[domain.Status]int64, error) {
	rows, err := s.pool.Query(ctx, qCountOrdersByStatus)
	if err != nil {
		return nil, fmt.Errorf("store: count orders: %w", err)
	}
	defer rows.Close()

	out := make(map[domain.Status]int64, len(domain.AllStatuses))
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: scan count: %w", err)
		}
		out[domain.Status(status)] = n
	}
	return out, rows.Err()
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanOrder(s scanner) (domain.Order, error) {
	var (
		o     domain.Order
		idStr string
		st    string
	)
	err := s.Scan(&idStr, &o.CustomerID, &st, &o.AmountCents, &o.Currency, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return domain.Order{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return domain.Order{}, fmt.Errorf("parse id %q: %w", idStr, err)
	}
	o.ID = id
	o.Status = domain.Status(st)
	return o, nil
}
