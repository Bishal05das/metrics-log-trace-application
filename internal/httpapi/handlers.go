package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
	"github.com/bishal05das/metrics-log-trace-application/internal/store"
)

// OrderEvents receives business events from the HTTP layer.
//
// Declared here, on the consumer side, so this package does not import the
// metrics package. The same pattern as metrics.PoolStats and worker.Store —
// and the payoff is the same: handlers can be tested with a two-line fake
// instead of a Prometheus registry.
type OrderEvents interface {
	OrderCreated(currency string, amountCents int64)
}

type API struct {
	store  *store.Store
	log    *slog.Logger
	events OrderEvents
}

func NewAPI(s *store.Store, log *slog.Logger, events OrderEvents) *API {
	if events == nil {
		events = nopEvents{}
	}
	return &API{store: s, log: log, events: events}
}

type nopEvents struct{}

func (nopEvents) OrderCreated(string, int64) {}

const maxBodyBytes = 64 << 10 // 64 KiB

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	var in domain.NewOrder

	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidJSON, "request body is not valid JSON for an order", "")
		return
	}

	if err := in.Validate(); err != nil {
		var ve domain.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusUnprocessableEntity, codeValidation, ve.Reason, ve.Field)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, codeValidation, err.Error(), "")
		return
	}

	order := in.Build()
	if err := a.store.CreateOrder(r.Context(), order); err != nil {
		a.log.ErrorContext(r.Context(), "create order failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "could not create order", "")
		return
	}

	// Recorded only after the commit succeeds. Emitting the business event
	// before the write would inflate created_total with orders that do not
	// exist, and business metrics are trusted precisely because they match
	// what is in the database.
	a.events.OrderCreated(order.Currency, order.AmountCents)

	w.Header().Set("Location", "/orders/"+order.ID.String())
	writeJSON(w, http.StatusCreated, order)
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadID, "order id must be a UUID", "id")
		return
	}

	order, err := a.store.GetOrder(r.Context(), id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no order with that id", "")
		return
	case err != nil:
		a.log.ErrorContext(r.Context(), "get order failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "could not read order", "")
		return
	}
	writeJSON(w, http.StatusOK, order)
}

type listResponse struct {
	Orders []domain.Order `json:"orders"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, err := intParam(q.Get("limit"), 20)
	if err != nil || limit < 1 || limit > 100 {
		writeError(w, http.StatusBadRequest, codeBadQuery, "limit must be an integer between 1 and 100", "limit")
		return
	}
	offset, err := intParam(q.Get("offset"), 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, codeBadQuery, "offset must be a non-negative integer", "offset")
		return
	}

	var status domain.Status
	if s := q.Get("status"); s != "" {
		status = domain.Status(s)
		if !status.Valid() {
			writeError(w, http.StatusBadRequest, codeBadQuery, "unknown status", "status")
			return
		}
	}

	orders, err := a.store.ListOrders(r.Context(), status, limit, offset)
	if err != nil {
		a.log.ErrorContext(r.Context(), "list orders failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "could not list orders", "")
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Orders: orders, Limit: limit, Offset: offset})
}

// healthz is a liveness probe: "is this process running?" It must never touch
// a dependency, or a database blip will get your pods restarted.
func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is a readiness probe: "can this process serve traffic right now?"
// This one *does* check the database, because a pod that cannot reach Postgres
// should be pulled out of the load balancer rather than restarted.
func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		a.log.WarnContext(r.Context(), "readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "database unreachable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// slowQuery holds a pooled database connection for a requested duration.
// Dev-only; registered by Options.EnableDebugRoutes.
//
// The point is to make the connection pool saturate on demand: with 10
// connections, sustaining more than 10 concurrent 200ms queries forces callers
// to queue, and orders_db_pool_acquire_wait_seconds starts to climb while
// orders_db_query_duration_seconds stays flat. That divergence is the whole
// lesson of pool metrics, and it is much easier to believe once you have
// watched it happen.
func (a *API) slowQuery(w http.ResponseWriter, r *http.Request) {
	d, err := time.ParseDuration(cmp.Or(r.URL.Query().Get("d"), "200ms"))
	if err != nil || d < 0 || d > 5*time.Second {
		writeError(w, http.StatusBadRequest, codeBadQuery, "d must be a duration between 0 and 5s", "d")
		return
	}

	if err := a.store.Sleep(r.Context(), d); err != nil {
		// Context cancellation here is expected under saturation: the client
		// gave up while queued for a connection.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, codeUnavailable, "request abandoned while waiting for a connection", "")
			return
		}
		a.log.ErrorContext(r.Context(), "slow query failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "slow query failed", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"slept": d.String()})
}

func intParam(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	return strconv.Atoi(raw)
}
