// Package domain holds the core types. It deliberately imports nothing from
// the database or HTTP layers, so the business rules stay independent of both.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is a closed set. Keeping it closed is what makes it safe to use as a
// Prometheus label later — five possible values, not five million.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusPaid       Status = "paid"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

// AllStatuses is used to pre-initialise metric label combinations so a series
// exists (at zero) before the first event of that kind ever happens.
var AllStatuses = []Status{
	StatusPending, StatusProcessing, StatusPaid, StatusFailed, StatusCancelled,
}

// AllTransitions is the closed set of legal state changes. Pre-initialising
// these means an alert on `to="failed"` matches a real series from startup,
// rather than being silently dead until the first failure occurs.
var AllTransitions = [][2]string{
	{"new", string(StatusPending)},
	{string(StatusPending), string(StatusProcessing)},
	{string(StatusProcessing), string(StatusPaid)},
	{string(StatusProcessing), string(StatusFailed)},
	{string(StatusPending), string(StatusCancelled)},
}

// SupportedCurrencies is an allowlist, and it exists for TWO reasons.
//
// The API reason: accepting arbitrary three-letter strings as currency is bad
// input validation.
//
// The metrics reason, which is the one that will bite you: `currency` is a
// label on orders_created_total. A label value that comes from client input
// and is only length-checked is an OPEN SET — a client can send any of 17,576
// three-letter combinations and each one permanently creates a new time
// series. That is the Phase 2 cardinality attack again, arriving through a
// JSON body instead of a URL.
//
// The rule: any label value derived from user input must be validated against
// a closed set at the edge, or mapped to a catch-all before it reaches a
// metric. Never both trust it and label with it.
var SupportedCurrencies = []string{"USD", "EUR", "GBP", "INR", "JPY"}

func CurrencySupported(c string) bool {
	for _, s := range SupportedCurrencies {
		if s == c {
			return true
		}
	}
	return false
}

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusProcessing, StatusPaid, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

type Order struct {
	ID          uuid.UUID `json:"id"`
	CustomerID  string    `json:"customer_id"`
	Status      Status    `json:"status"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

var (
	ErrNotFound = errors.New("order not found")
	ErrConflict = errors.New("order conflict")
)

// NewOrder validates input and builds a pending order.
type NewOrder struct {
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// ValidationError carries a field-level reason so the HTTP layer can return a
// stable, machine-readable error code. Stable error codes matter for metrics:
// we will count errors by reason, and the reason set must stay small.
type ValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e ValidationError) Error() string { return e.Field + ": " + e.Reason }

func (n NewOrder) Validate() error {
	switch {
	case n.CustomerID == "":
		return ValidationError{Field: "customer_id", Reason: "required"}
	case len(n.CustomerID) > 64:
		return ValidationError{Field: "customer_id", Reason: "too_long"}
	case n.AmountCents <= 0:
		return ValidationError{Field: "amount_cents", Reason: "must_be_positive"}
	case n.AmountCents > 100_000_00:
		return ValidationError{Field: "amount_cents", Reason: "exceeds_limit"}
	case len(n.Currency) != 3:
		return ValidationError{Field: "currency", Reason: "must_be_iso4217"}
	case !CurrencySupported(n.Currency):
		return ValidationError{Field: "currency", Reason: "unsupported_currency"}
	}
	return nil
}

func (n NewOrder) Build() Order {
	now := time.Now().UTC()
	return Order{
		ID:          uuid.New(),
		CustomerID:  n.CustomerID,
		Status:      StatusPending,
		AmountCents: n.AmountCents,
		Currency:    n.Currency,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}
