package logging

import (
	"testing"

	"github.com/bishal05das/metrics-log-trace-application/internal/store"
)

// TestQueryLoggerNamesMatchStoreConstants is the regression test for a bug that
// only showed up in a running system: NewQueryLogger normalised the SQL it
// received but not the map KEYS, so every lookup missed and every query logged
// as "other".
//
// It runs against the real store.QueryNames() rather than a fixture, so adding
// a query without naming it, or reformatting a constant, is caught here.
func TestQueryLoggerNamesMatchStoreConstants(t *testing.T) {
	names := store.QueryNames()
	if len(names) == 0 {
		t.Fatal("store.QueryNames() is empty")
	}

	q := NewQueryLogger(Discard(), names, 0)

	for sql, want := range names {
		got, ok := q.names[normalise(sql)]
		if !ok {
			t.Errorf("query %q (%s) does not resolve — the map keys are not normalised", want, sql)
			continue
		}
		if got != want {
			t.Errorf("normalise collision: %q resolved to %q, want %q", sql, got, want)
		}
	}
}

// TestNormaliseIsFormatInsensitive — the SQL constants are indented across
// several lines; the tracer receives them verbatim.
func TestNormaliseIsFormatInsensitive(t *testing.T) {
	multiline := "\n\t\tSELECT id::text, customer_id\n\t\tFROM orders\n\t\tWHERE id = $1::uuid"
	oneLine := "SELECT id::text, customer_id FROM orders WHERE id = $1::uuid"

	if got := normalise(multiline); got != oneLine {
		t.Errorf("normalise(multiline) = %q, want %q", got, oneLine)
	}
	if got := normalise(oneLine); got != oneLine {
		t.Errorf("normalise(oneLine) = %q, want %q", got, oneLine)
	}
}

// TestUnknownQueryIsBounded — an unregistered query must never put raw SQL into
// a log field that someone might later promote to an indexed label.
func TestUnknownQueryIsBounded(t *testing.T) {
	q := NewQueryLogger(Discard(), store.QueryNames(), 0)

	adhoc := "SELECT * FROM orders WHERE customer_id = 'cust-8123'"
	if _, ok := q.names[normalise(adhoc)]; ok {
		t.Fatal("an ad-hoc query should not resolve to a name")
	}
}
