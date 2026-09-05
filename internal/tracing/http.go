package tracing

import (
	"log/slog"
	"net/http"
	"strings"

	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/bishal05das/metrics-log-trace-application/internal/logging"
)

// Middleware starts a server span for every request.
//
// Three things happen here, and each is load-bearing:
//
//  1. EXTRACT. The incoming `traceparent` header is parsed, so a span created
//     here becomes a CHILD of the caller's span rather than a new root. Skip
//     this and every service produces its own disconnected trace — the most
//     common way a distributed trace ends up broken.
//  2. START. A server span wraps the whole request.
//  3. JOIN. The trace ID is put on the logging context, so every log line for
//     this request carries trace_id. That is the bridge between two pillars:
//     find a slow trace, get its ID, and pull the exact log lines — or the
//     reverse.
func Middleware(tracer trace.Tracer, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health probes fire every few seconds forever. Tracing them is
			// pure cost — the same exclusion as metrics and logs.
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}

			// 1. EXTRACT the upstream context from the request headers.
			ctx := otel.GetTextMapPropagator().Extract(
				r.Context(), propagation.HeaderCarrier(r.Header))

			// 2. START the span. The name is provisional: r.Pattern is not
			// populated until ServeMux matches, which happens inside
			// next.ServeHTTP — the same constraint the metrics middleware has.
			// It is corrected below, once the route is known.
			ctx, span := tracer.Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(r.Method),
					semconv.URLPath(r.URL.Path),
					semconv.URLScheme(scheme(r)),
					semconv.UserAgentOriginal(r.UserAgent()),
					semconv.ServerAddress(r.Host),
				),
			)
			defer span.End()

			// 3. JOIN to the logs. Only when the span is actually recording —
			// an unsampled span has a valid ID but no data behind it, and
			// logging a trace_id that leads nowhere wastes an investigation.
			sc := span.SpanContext()
			if sc.IsValid() {
				ctx = logging.WithAttrs(ctx,
					slog.String("trace_id", sc.TraceID().String()),
					slog.String("span_id", sc.SpanID().String()),
				)
				// Return it to the client too, so a support ticket can carry
				// the trace ID as well as the request ID.
				w.Header().Set("X-Trace-Id", sc.TraceID().String())
			}

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			inner := r.WithContext(ctx)
			next.ServeHTTP(rec, inner)
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

			// Now that ServeMux has matched, rename the span to the route
			// TEMPLATE. Span names are a low-cardinality dimension in every
			// tracing backend — naming spans after raw paths is the same
			// cardinality mistake as a URL-valued metric label, one system over.
			route := routeTemplate(r)
			span.SetName(r.Method + " " + route)
			span.SetAttributes(
				semconv.HTTPRoute(route),
				semconv.HTTPResponseStatusCode(rec.status),
			)

			// Only 5xx marks the span as failed. A 404 is a correct answer to
			// a wrong question; marking it an error makes every backend's
			// error-rate view meaningless — the same reasoning as the log
			// severity mapping.
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			} else {
				span.SetStatus(codes.Ok, "")
			}
		})
	}
}

// RecordPanic marks the active span as failed. Wired into the recovery
// middleware so a panic is visible in the trace, not just the logs.
func RecordPanic(r *http.Request, v any) {
	span := trace.SpanFromContext(r.Context())
	if !span.IsRecording() {
		return
	}
	span.AddEvent("panic", trace.WithAttributes(
		attribute.String("panic.value", truncate(v)),
	))
	span.SetStatus(codes.Error, "handler panic")
}

// TraceID returns the current trace ID, or "" when not tracing.
func TraceID(r *http.Request) string {
	sc := trace.SpanContextFromContext(r.Context())
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func routeTemplate(r *http.Request) string {
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

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		return v
	}
	return "http"
}

func truncate(v any) string {
	s := strings.TrimSpace(strings.SplitN(fmt.Sprint(v), "\n", 2)[0])
	if len(s) > 256 {
		return s[:256] + "…"
	}
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status, r.wroteHeader = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
