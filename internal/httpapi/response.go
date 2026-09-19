package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the single error shape every endpoint returns.
//
// `Code` is a stable, closed set of short strings. It exists so that clients
// can branch on it — and so that, in Phase 2, we can put it on a metric label
// without blowing up cardinality. Never put `Message` on a label: it is free
// text and would create a new time series per distinct message.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

const (
	codeInvalidJSON   = "invalid_json"
	codeValidation    = "validation_failed"
	codeNotFound      = "not_found"
	codeBadID         = "invalid_id"
	codeBadQuery      = "invalid_query_param"
	codeInternal      = "internal_error"
	codeUnavailable   = "unavailable"
	codeMethodUnknown = "not_found"
)

// writeJSON takes a context for one reason: the error path below.
//
// It used to call the package-level slog.Error, which passes
// context.Background() — so ContextHandler had nothing to attach and this
// single line came out with no request_id and no trace_id. It was the only
// error in the whole request path you could not correlate, and it fires exactly
// when a response is failing to reach the client, which is when you most want
// to know which request it was.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all we can do is record it.
		slog.ErrorContext(ctx, "write json response", "error", err)
	}
}

func writeError(ctx context.Context, w http.ResponseWriter, status int, code, msg, field string) {
	writeJSON(ctx, w, status, errorBody{Code: code, Message: msg, Field: field})
}
