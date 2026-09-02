package httpapi

import (
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all we can do is record it.
		slog.Error("write json response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg, field string) {
	writeJSON(w, status, errorBody{Code: code, Message: msg, Field: field})
}
