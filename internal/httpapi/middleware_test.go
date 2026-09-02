package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRecoverConvertsPanicTo500 — and, critically, that onPanic fires so the
// panic counter increments.
func TestRecoverConvertsPanicTo500(t *testing.T) {
	var called int

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})

	h := Recover(discardLogger(), func(*http.Request) { called++ })(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if called != 1 {
		t.Fatalf("onPanic called %d times, want 1", called)
	}
}

// TestRecoverIgnoresErrAbortHandler — a client disconnecting mid-write makes
// net/http panic by design. Counting that as an application panic would page
// you every time someone on a train loses signal.
func TestRecoverIgnoresErrAbortHandler(t *testing.T) {
	var called int

	h := Recover(discardLogger(), func(*http.Request) { called++ })(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}),
	)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("ErrAbortHandler should be re-panicked for net/http to handle")
		}
		if called != 0 {
			t.Fatalf("ErrAbortHandler must not count as a panic, got %d", called)
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// TestChainOrder proves mw[0] is outermost — the property that keeps metrics
// outside recovery.
func TestChainOrder(t *testing.T) {
	var order []string

	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":in")
				next.ServeHTTP(w, r)
				order = append(order, name+":out")
			})
		}
	}

	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mark("outer"), mark("inner"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"outer:in", "inner:in", "handler", "inner:out", "outer:out"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("chain order = %v, want %v", order, want)
		}
	}
}
