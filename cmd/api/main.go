package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/bishal05das/metrics-log-trace-application/internal/config"
	"github.com/bishal05das/metrics-log-trace-application/internal/domain"
	"github.com/bishal05das/metrics-log-trace-application/internal/httpapi"
	"github.com/bishal05das/metrics-log-trace-application/internal/logging"
	"github.com/bishal05das/metrics-log-trace-application/internal/metrics"
	"github.com/bishal05das/metrics-log-trace-application/internal/store"
	"github.com/bishal05das/metrics-log-trace-application/internal/tracing"
	"github.com/bishal05das/metrics-log-trace-application/internal/worker"
)

// Injected at build time via -ldflags. See the Makefile's `build` target.
// These end up as labels on orders_build_info.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log, levelVar := logging.New(logging.Config{
		Level:   cfg.LogLevel,
		Format:  logging.Format(cfg.LogFormat),
		Source:  cfg.LogSource,
		Service: "orders",
		Version: version,
		Env:     cfg.Env,
	})
	//Replace the global default logger used by the log/slog package with your custom logger.
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tracing is initialised early: it installs the global propagator, and
	// anything that creates a span before this point would get a no-op tracer.
	tp, err := tracing.Init(ctx, tracing.Config{
		Enabled:      cfg.TracingEnabled,
		Exporter:     tracing.ParseExporter(cfg.TracingExporter),
		OTLPEndpoint: cfg.TracingOTLPEndpoint,
		OTLPInsecure: cfg.TracingOTLPInsecure,
		SampleRatio:  cfg.TracingSampleRatio,
		ServiceName:  "orders",
		Version:      version,
		Env:          cfg.Env,
	}, log)
	if err != nil {
		return err
	}

	// The registry is built first and passed explicitly to everything that
	// needs to register a metric. No globals.
	reg := metrics.New()
	metrics.BuildInfo(reg, version, commit, runtime.Version(), cfg.Env)

	httpMetrics := metrics.NewHTTP(reg)
	httpMetrics.PreInit(httpapi.InstrumentedRoutes...)

	// One object serves as pgx.QueryTracer and pgxpool.AcquireTracer, so every
	// query and every connection acquisition is instrumented without any store
	// method having to remember to do it.
	dbMetrics := metrics.NewDB(reg, store.QueryNames())

	statuses := make([]string, len(domain.AllStatuses))
	for i, s := range domain.AllStatuses {
		statuses[i] = string(s)
	}
	bizMetrics := metrics.NewBusiness(reg, statuses)
	bizMetrics.PreInit(domain.SupportedCurrencies, domain.AllTransitions)

	// pgx has one tracer slot, so the metrics tracer and the query logger are
	// composed. MultiTracer also forwards the acquire-tracing interface — drop
	// that and the acquire-wait histogram silently stops recording.
	// THREE independent observers on one pgx tracer slot, none aware of the
	// others: metrics (aggregate), logs (per request), traces (where the time
	// went). MultiTracer also forwards pgxpool.AcquireTracer, without which the
	// acquire-wait histogram AND the db.acquire span both silently vanish.
	queryLogger := logging.NewQueryLogger(log, store.QueryNames(), cfg.SlowQueryThreshold)
	queryTracer := tracing.NewQueryTracer(tp.Tracer, store.QueryNames())
	tracer := store.MultiTracer{dbMetrics, queryLogger, queryTracer}

	st, err := store.Open(ctx, cfg, tracer)
	if err != nil {
		return err
	}
	defer st.Close()

	// Registered after Open because the collector closes over the live pool.
	// It reads pool.Stat() at scrape time rather than mirroring it into gauges.
	metrics.RegisterPool(reg, func() metrics.PoolStats { return st.Pool().Stat() })

	log.Info("connected to postgres", "max_conns", cfg.DBMaxConns)

	migCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := st.Migrate(migCtx); err != nil {
		return err
	}
	log.Info("migrations applied")

	// Middleware order: metrics OUTSIDE recovery, so a panic is observed as
	// the 500 that recovery turns it into.
	router := httpapi.NewRouter(
		httpapi.Deps{
			Store:   st,
			Log:     log,
			Events:  bizMetrics,
			Options: httpapi.Options{EnableDebugRoutes: cfg.Env == "dev"},
		},
		tracing.Middleware(tp.Tracer, log), // outermost: span covers everything
		httpMetrics.Middleware,             // RED metrics
		httpapi.RequestID(),                // request id on the context...
		httpapi.AccessLog(log),             // ...and trace_id, from the span above
		httpapi.Recover(log, func(r *http.Request) { // innermost: turns panic into 500
			httpMetrics.RecordPanic(r)              // count it
			tracing.RecordPanic(r, "handler panic") // and mark the span failed
		}),
	)

	apiSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsSrv := metrics.NewServer(cfg.MetricsAddr, reg, log, func(mux *http.ServeMux) {
		// Registered per-method deliberately. A bare "/loglevel" is AMBIGUOUS
		// against the admin server's "GET /" landing page — it matches more
		// methods but a narrower path, and neither pattern is strictly more
		// specific, so Go 1.22+ ServeMux panics at registration rather than
		// silently picking one. Naming the methods resolves it.
		lvl := logging.LevelHandler(levelVar, log)
		mux.Handle("GET /loglevel", lvl)
		mux.Handle("PUT /loglevel", lvl)
		mux.Handle("POST /loglevel", lvl)
	})

	// Bind BOTH sockets HERE, synchronously, before anything else starts.
	//
	// This is not the same as calling ListenAndServe inside a goroutine, and
	// the difference matters twice over:
	//
	//  1. The bind succeeds or fails on this line, so a port conflict surfaces
	//     as an ordinary startup error. The previous version logged "api server
	//     listening" and *then* called ListenAndServe, so on a conflict it
	//     announced success and died one line later — actively misleading at
	//     exactly the moment you are debugging a startup failure. A log line
	//     should never claim something that has not happened yet.
	//  2. Failing here means the worker and the backlog refresher were never
	//     started, so there are no goroutines to unwind and no chance of one
	//     querying a pool we are about to close on the way out.
	apiLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("api listener on %s: %w", cfg.HTTPAddr, err)
	}
	metricsLn, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		_ = apiLn.Close()
		return fmt.Errorf("metrics listener on %s: %w", cfg.MetricsAddr, err)
	}

	// Log the address the kernel actually gave us, not the one we asked for.
	// With ":0" — useful in tests — those differ, and the resolved one is the
	// only one anybody can connect to.
	log.Info("api server listening",
		"addr", apiLn.Addr().String(), "env", cfg.Env, "version", version)
	log.Info("metrics server listening",
		"addr", metricsLn.Addr().String(), "path", "/metrics")

	// Background goroutines get their own WaitGroup so shutdown can wait for
	// an in-flight batch to finish. Without this, a deploy can leave orders
	// stranded in `processing` with no worker owning them.
	var bg sync.WaitGroup
	if cfg.WorkerEnabled {
		w := worker.New(st, bizMetrics, log, worker.Config{
			Interval:    cfg.WorkerInterval,
			BatchSize:   cfg.WorkerBatchSize,
			FailureRate: cfg.WorkerFailureRate,
			Tracer:      tp.Tracer,
		})
		bg.Add(1)
		go func() { defer bg.Done(); w.Run(ctx) }()

		br := worker.NewBacklogRefresher(st, bizMetrics, log, cfg.BacklogInterval)
		bg.Add(1)
		go func() { defer bg.Done(); br.Run(ctx) }()
	}

	// Buffered for 2: if both servers fail we must not leak a goroutine
	// blocked on an unbuffered send.
	errCh := make(chan error, 2)

	go func() {
		if err := apiSrv.Serve(apiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api server: %w", err)
		}
	}()
	go func() {
		if err := metricsSrv.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	// Whichever branch we take, the SAME shutdown sequence runs below.
	//
	// Previously the error branch did a bare `return err`, which skipped
	// bg.Wait() entirely. Deferred calls then ran LIFO — st.Close() before
	// stop() — so the pool was closed while the worker and refresher were
	// still live, producing "closed pool" errors on the way out. Shutdown
	// ordering that is only correct on the happy path is not correct.
	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Cancel the root context so the background loops wind down. On the
	// ctx.Done() branch it is already cancelled and this is a no-op; on the
	// error branch it is what makes bg.Wait() below terminate. stop() is safe
	// to call more than once, and it is deferred as well.
	stop()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()

	// Shutdown ORDER matters. Drain the API first so in-flight requests can
	// finish and still record their metrics. Then wait for the worker's
	// current batch. Keep /metrics alive throughout, so a scrape landing
	// during the drain gets real numbers — it dies last.
	//
	// st.Close() is deferred, so the pool outlives every one of these steps.
	apiErr := apiSrv.Shutdown(shutdownCtx)
	bg.Wait()
	metricsErr := metricsSrv.Shutdown(shutdownCtx)

	// Flush LAST. Spans are batched and exported every 5s, so skipping this
	// loses the final window — exactly the spans covering whatever caused the
	// shutdown.
	traceErr := tp.Shutdown(shutdownCtx)

	if err := errors.Join(runErr, apiErr, metricsErr, traceErr); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}
