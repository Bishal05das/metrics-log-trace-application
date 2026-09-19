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
			// skip helath check
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := otel.GetTextMapPropagator().Extract(
				r.Context(), propagation.HeaderCarrier(r.Header))

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

			sc := span.SpanContext() //get trace id and span id
			if sc.IsValid() {
				ctx = logging.WithAttrs(ctx,
					slog.String("trace_id", sc.TraceID().String()),
					slog.String("span_id", sc.SpanID().String()),
				)
				// Return it to the client too, so a support ticket can carry
				// the trace ID as well as the request ID.
				w.Header().Set("X-Trace-Id", sc.TraceID().String()) //send trace id to client
			}

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			inner := r.WithContext(ctx)
			next.ServeHTTP(rec, inner)
			
			r.Pattern = inner.Pattern  //Incoming:GET /orders/12345.But the route is:GET /orders/{id} You want:http.route=/orders/{id}

			// Before:

			// Span name:
			// GET

			// After:

			// Span name:
			// GET /orders/{id}
			route := routeTemplate(r)
			span.SetName(r.Method + " " + route)
			span.SetAttributes(
				semconv.HTTPRoute(route),
				semconv.HTTPResponseStatusCode(rec.status),
			)

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
