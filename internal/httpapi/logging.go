package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bishal05das/metrics-log-trace-application/internal/logging"
)

// RequestID assigns every request a correlation identifier and puts it on the
// context, where logging.ContextHandler picks it up for every subsequent log
// line — handler, store, worker callback, all of them.
//
// It honours an inbound X-Request-Id so the same ID spans service hops, but
// SANITISES it first: the header is attacker-controlled, and an unvalidated
// value is both a log-injection vector and an unbounded string that someone
// will eventually promote to an indexed label.
//
// The ID is echoed back on the response. That turns "my order failed" from a
// support ticket into a single log query.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := logging.SanitizeRequestID(r.Header.Get(logging.RequestIDHeader))
			if !ok {
				id = logging.NewRequestID()
			}

			// Set on the response BEFORE calling the handler: once a handler
			// writes a status line the headers are flushed, and anything added
			// afterwards is silently discarded.
			w.Header().Set(logging.RequestIDHeader, id)

			ctx := logging.WithRequestID(r.Context(), id)
			inner := r.WithContext(ctx)
			next.ServeHTTP(w, inner)
			// Propagate the routing result back to the CALLER's request.
			//
			// r.WithContext returns a COPY. ServeMux sets Pattern on the copy it is
			// handed, so any middleware ABOVE this one is left holding a request whose
			// Pattern is still empty — and every route label derived from it becomes
			// "unmatched". This silently broke the metrics route label the moment the
			// request-ID middleware was introduced: 1333 real requests all labelled
			// unmatched, with the per-route series pinned at zero.
			//
			// Copying the field back restores the invariant ServeMux established. Safe:
			// it runs after next.ServeHTTP returns, on one goroutine, and ServeMux
			// mutates this same field in place.
			r.Pattern = inner.Pattern
		})
	}
}

// AccessLog writes one structured line per request.
//
// Deliberately ONE line, at the end, rather than a "started"/"finished" pair.
// Two lines double the volume and cost to tell you nothing extra, and they can
// be split across a log rotation, leaving orphans that look like hung requests.
//
// It reads r.Pattern for the route, exactly like the metrics middleware, so a
// log line and a metric sample for the same request agree on the label. If they
// disagreed you could not pivot from a spiking metric to the logs behind it,
// which is the entire point of running both.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health probes fire every few seconds forever and say nothing.
			// Excluded for the same reason they are excluded from metrics — but
			// note the asymmetry: a FAILING probe is worth a line, and readyz
			// already logs its own warning when the database check fails.
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			route := routePattern(r)

			// Severity follows who is at fault, which is what makes an
			// error-level filter useful. 5xx is our bug and must be visible;
			// 4xx is the client's mistake and is normal traffic — logging it at
			// error would fill the error stream with people typing bad UUIDs.
			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}

			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", rec.status),
				// Milliseconds as a float: readable in a log viewer, and still
				// numeric, so a log query can filter on `duration_ms > 250`
				// rather than parsing a formatted duration string.
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.Int64("bytes", rec.written),
				slog.String("remote_addr", clientIP(r)),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// routePattern mirrors metrics.routeLabel: the matched TEMPLATE, never the raw
// path. Duplicated rather than shared because httpapi does not import metrics —
// the same consumer-side-interface discipline used everywhere else here.
// TestAccessLogRouteIsTemplateNotPath keeps this half honest; the metrics half
// is covered by TestRouteLabelIsTemplateNotPath.
func routePattern(r *http.Request) string {
	p := r.Pattern
	if p == "" {
		return "unmatched"
	}
	if i := strings.LastIndex(p, " "); i >= 0 {
		p = p[i+1:]
	}
	if p == "" || p == "/" {
		return "unmatched"
	}
	return p
}

// clientIP prefers the left-most X-Forwarded-For entry.
//
// SECURITY NOTE: X-Forwarded-For is client-supplied and trivially spoofed. It
// is safe to log, and NOT safe to use for authorisation, rate limiting or
// blocking unless a proxy you control overwrites it. Trusting it for anything
// that grants access is a well-worn way to be bypassed.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, ok := strings.Cut(r.RemoteAddr, ":"); ok {
		return host
	}
	return r.RemoteAddr
}

// statusRecorder captures status and size for the access log.
//
// A second recorder nested inside the metrics one. Slightly redundant, and the
// alternative — one shared recorder — would mean the logging layer importing
// the metrics package or vice versa. Two small structs cost less than that
// coupling.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
