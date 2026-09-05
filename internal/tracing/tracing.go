// Package tracing is the third observability pillar.
//
// Where the other two stop:
//
//	METRICS  "p99 latency on POST /orders is 2.4s"      — something is wrong
//	LOGS     "request abc123 took 2.4s"                 — which request
//	TRACES   "of those 2.4s, 2.3s was spent waiting for a database connection,
//	          inside a call made from the order handler"  — WHERE THE TIME WENT
//
// A trace is a tree of SPANS. Each span is one unit of work with a start time,
// a duration, attributes, and a parent. The tree reassembles into a waterfall
// that shows exactly which step was slow — and, across services, which service
// was slow.
//
// The single most important property is CONTEXT PROPAGATION. A trace ID is
// created at the edge, carried through every function call in a
// context.Context, and passed to the next service over the `traceparent` HTTP
// header. Break that chain anywhere and the trace silently splits into two
// unrelated fragments, which is the tracing equivalent of losing a request ID.
package tracing

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Exporter selects where finished spans go.
type Exporter string

const (
	// ExporterStdout prints spans as JSON. No backend required, which is why
	// it is the default here — you can read a real trace without running
	// anything extra. Far too verbose for production.
	ExporterStdout Exporter = "stdout"

	// ExporterOTLP ships to a collector or backend (Tempo, Jaeger) over
	// OTLP/HTTP. Configure the endpoint and this is production-ready.
	ExporterOTLP Exporter = "otlp"

	// ExporterNone installs a no-op provider. Instrumentation stays in the
	// code and costs almost nothing; nothing is recorded or exported.
	ExporterNone Exporter = "none"
)

type Config struct {
	Enabled  bool
	Exporter Exporter

	// OTLPEndpoint is host:port, e.g. "tempo:4318". Empty means the OTel
	// default (localhost:4318).
	OTLPEndpoint string
	OTLPInsecure bool

	// SampleRatio is the fraction of ROOT traces recorded, 0.0-1.0.
	//
	// This is the cost dial, and traces are the most expensive pillar per
	// event: a trace is many spans, each with attributes and timings. At real
	// volume nobody samples at 1.0 — 1-10% is typical, with errors and slow
	// requests always kept via a tail sampler in the collector.
	//
	// Sampling is PARENT-BASED (see newSampler): if an upstream service
	// already decided to sample a trace, we honour that decision. Otherwise
	// half a distributed trace goes missing, which is worse than not sampling
	// it at all — you get a waterfall with holes and draw the wrong conclusion.
	SampleRatio float64

	ServiceName string
	Version     string
	Env         string
}

// Provider owns the tracer provider and its shutdown.
type Provider struct {
	tp     *sdktrace.TracerProvider
	Tracer trace.Tracer
	log    *slog.Logger
}

// Init configures the global OTel pipeline and returns a Provider.
//
// It sets globals — otel.SetTracerProvider and otel.SetTextMapPropagator —
// which sits awkwardly beside the "no globals" discipline used for the
// Prometheus registry. The difference is that OTel's instrumentation libraries
// are built around those globals: any third-party package that creates spans
// looks them up there, and refusing to set them means those spans are silently
// dropped. The Tracer is still passed explicitly to our own code.
func Init(ctx context.Context, cfg Config, log *slog.Logger) (*Provider, error) {
	// Always install the W3C propagator, even when disabled. A service that
	// does not RECORD traces should still PASS THROUGH the trace context it
	// receives, or it becomes a hole in someone else's trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C `traceparent` / `tracestate`
		propagation.Baggage{},      // W3C `baggage`
	))

	if !cfg.Enabled || cfg.Exporter == ExporterNone {
		otel.SetTracerProvider(noop.NewTracerProvider())
		log.Info("tracing disabled", "propagation", "enabled (pass-through)")
		return &Provider{Tracer: noop.NewTracerProvider().Tracer(cfg.ServiceName), log: log}, nil
	}

	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// The resource describes WHO is emitting these spans. It is attached to
	// every span and is what lets a backend group a waterfall by service.
	//
	// resource.Merge REFUSES to combine resources with different schema URLs,
	// and resource.Default() carries the SDK's own semconv version. Import a
	// different semconv package here and Init fails at STARTUP with
	// "conflicting Schema URL" — which is what v1.26.0 did against an SDK on
	// v1.43.0. Keep this import pinned to the SDK's version.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
			semconv.DeploymentEnvironmentNameKey.String(cfg.Env),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// Batching, not one export per span. Exporting synchronously would put
		// a network call on the request path — the observability layer must
		// never be able to slow down or fail the thing it observes.
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(newSampler(cfg.SampleRatio)),
	)

	otel.SetTracerProvider(tp)

	// Errors from the SDK itself — a failing exporter, a dropped batch — go
	// here. Without this they are discarded and your traces stop arriving with
	// no indication why.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Error("otel error", "error", err)
	}))

	log.Info("tracing enabled",
		"exporter", string(cfg.Exporter),
		"sample_ratio", cfg.SampleRatio,
		"endpoint", cfg.OTLPEndpoint)

	return &Provider{tp: tp, Tracer: tp.Tracer(cfg.ServiceName), log: log}, nil
}

// newSampler builds a parent-based sampler.
//
// ParentBased means: if the incoming request already carries a sampling
// decision, honour it. Only make a fresh decision for a ROOT span — a request
// that started here. That is what keeps a distributed trace whole; every
// service independently deciding would give you a tree with random branches
// missing.
func newSampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio <= 0:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case ratio >= 1:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Exporter {
	case ExporterOTLP:
		opts := []otlptracehttp.Option{}
		if cfg.OTLPEndpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.OTLPEndpoint))
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp exporter: %w", err)
		}
		return exp, nil

	default:
		// Spans go to STDERR, not stdout.
		//
		// stdout carries the structured application logs, and a log pipeline
		// parses every line as JSON. Span JSON has a completely different shape
		// and would be ingested as malformed log records. Keeping the two
		// streams separate means `make logs` stays readable and neither
		// pipeline has to know about the other.
		return stdouttrace.New(
			stdouttrace.WithWriter(stderrWriter()),
			stdouttrace.WithPrettyPrint(),
		)
	}
}

func stderrWriter() io.Writer { return os.Stderr }

// Shutdown flushes any spans still in the batch queue.
//
// This MUST be called, and it must be called before the process exits. Spans
// are buffered and exported every 5s or 512 spans, so skipping this loses the
// last few seconds of traces — which are exactly the spans covering whatever
// caused the shutdown.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp == nil {
		return nil
	}
	if err := p.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("tracing: shutdown: %w", err)
	}
	p.log.Info("tracing flushed")
	return nil
}

// ParseExporter maps a config string onto an Exporter, defaulting to stdout.
func ParseExporter(s string) Exporter {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "otlp":
		return ExporterOTLP
	case "none", "":
		return ExporterNone
	default:
		return ExporterStdout
	}
}
