package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Middleware is the standard decorator shape.
type Middleware func(http.Handler) http.Handler

// chain applies middleware so that mw[0] is the OUTERMOST wrapper.
//
// Order is not cosmetic. The metrics middleware must sit outside the recovery
// middleware: recovery converts a panic into a real 500 response, and only
// then can the metrics layer observe a 500 rather than a request that appears
// to have returned 200. Reverse them and every panic is silently recorded as a
// success.
func chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// Recover turns a handler panic into a 500 instead of killing the process.
//
// onPanic is called so the metrics layer can count it. A panic counter is one
// of the few metrics worth alerting on at `> 0`: any panic in production is a
// bug, and unlike a log line it cannot be lost to sampling or a full disk.
func Recover(log *slog.Logger, onPanic func(*http.Request)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				// A client disconnecting mid-write makes net/http panic with
				// ErrAbortHandler by design. It is not a bug and must not be
				// counted as one, or a flaky mobile network becomes a page.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				if onPanic != nil {
					onPanic(r)
				}
				log.ErrorContext(r.Context(), "handler panic",
					"panic", rec,
					"method", r.Method,
					"pattern", r.Pattern,
					"stack", string(debug.Stack()),
				)

				// Best-effort: if the handler already wrote a header this is a
				// no-op, which is fine.
				writeError(w, http.StatusInternalServerError, codeInternal, "internal error", "")
			}()

			next.ServeHTTP(w, r)
		})
	}
}
