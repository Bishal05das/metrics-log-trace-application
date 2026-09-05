package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type ctxKey struct{}

// WithAttrs returns a context carrying the given attributes, merged with any
// already present.
//
// This is the mechanism behind correlation. A middleware puts request_id on the
// context once; every log call anywhere downstream — handler, store, worker —
// emits it automatically, because ContextHandler reads it back out. No call
// site has to know the request ID exists, and none of them can forget it.
//
// The alternative is threading a *slog.Logger through every function signature.
// That works, and it is what people do before they discover this, but it means
// every function that might log needs a logger parameter, and one that forgets
// silently breaks the correlation chain.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	existing := Attrs(ctx)

	// Copy rather than append in place. Contexts are shared across goroutines,
	// and appending to a slice held by a parent context is a data race that
	// shows up as attributes randomly appearing on the wrong log lines.
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)

	return context.WithValue(ctx, ctxKey{}, merged)
}

// Attrs returns the attributes carried by ctx, or nil.
func Attrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

const (
	// RequestIDKey is the log FIELD name.
	//
	// A field, never an indexed label. Whichever log store this eventually
	// ships to, a request ID is unique per request — indexing it would create
	// one index entry per request and destroy the store exactly the way a UUID
	// label destroys Prometheus. It stays queryable as a field; it must never
	// be promoted to a label.
	RequestIDKey = "request_id"

	// RequestIDHeader is both read and written, so the ID survives across
	// service hops and is returned to the client. A user reporting "my order
	// failed" can hand you this string, and it is the fastest possible path
	// from a complaint to the exact log lines.
	RequestIDHeader = "X-Request-Id"
)

type requestIDKey struct{}

// WithRequestID stores the id both as a log attribute and as a directly
// retrievable value.
func WithRequestID(ctx context.Context, id string) context.Context {
	ctx = context.WithValue(ctx, requestIDKey{}, id)
	return WithAttrs(ctx, slog.String(RequestIDKey, id))
}

// RequestID returns the request ID, or "" if there is none.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// NewRequestID returns a 128-bit random hex identifier.
//
// 16 bytes rather than a UUID: same collision resistance, no dependency, and
// the hex form is the same shape as a W3C trace-id — which matters because in
// the tracing phase this field becomes the join key between logs and traces.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable, but a missing request ID must
		// never take down a request. Degrade instead.
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// SanitizeRequestID validates a client-supplied ID before it is trusted.
//
// The header is attacker-controlled. Two things go wrong without this:
//
//   - LOG INJECTION. A value containing a newline and a crafted JSON object
//     can forge an entire extra log line. Our JSON encoder escapes it, so the
//     risk here is lower than with a text logger — but log pipelines are full
//     of components that re-parse, and defence at the boundary is cheap.
//   - CARDINALITY. An unbounded ID is fine as a field. It would be fatal as a
//     label, and someone will eventually promote this field to a label.
//
// Anything not [A-Za-z0-9._-] or longer than 128 chars is rejected and a fresh
// ID is generated instead.
func SanitizeRequestID(v string) (string, bool) {
	if v == "" || len(v) > 128 {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return "", false
		}
	}
	return v, true
}
