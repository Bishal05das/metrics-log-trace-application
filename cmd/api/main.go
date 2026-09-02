package main

import (
	"context"
	"errors"
	"log/slog"
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
	"github.com/bishal05das/metrics-log-trace-application/internal/metrics"
	"github.com/bishal05das/metrics-log-trace-application/internal/store"
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

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	st, err := store.Open(ctx, cfg, dbMetrics)
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
		httpMetrics.Middleware,
		httpapi.Recover(log, httpMetrics.RecordPanic),
	)

	// Background goroutines get their own WaitGroup so shutdown can wait for
	// an in-flight batch to finish. Without this, a deploy can leave orders
	// stranded in `processing` with no worker owning them.
	var bg sync.WaitGroup
	if cfg.WorkerEnabled {
		w := worker.New(st, bizMetrics, log, worker.Config{
			Interval:    cfg.WorkerInterval,
			BatchSize:   cfg.WorkerBatchSize,
			FailureRate: cfg.WorkerFailureRate,
		})
		bg.Add(1)
		go func() { defer bg.Done(); w.Run(ctx) }()

		br := worker.NewBacklogRefresher(st, bizMetrics, log, cfg.BacklogInterval)
		bg.Add(1)
		go func() { defer bg.Done(); br.Run(ctx) }()
	}

	apiSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsSrv := metrics.NewServer(cfg.MetricsAddr, reg, log)

	// Buffered for 2: if both servers fail we must not leak a goroutine
	// blocked on an unbuffered send.
	errCh := make(chan error, 2)

	go func() {
		log.Info("api server listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", version)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		log.Info("metrics server listening", "addr", cfg.MetricsAddr, "path", "/metrics")
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()

	// Shutdown ORDER matters. Drain the API first so in-flight requests can
	// finish and still record their metrics; keep /metrics alive throughout so
	// a scrape landing during the drain gets real numbers. The metrics server
	// is the last thing to die.
	apiErr := apiSrv.Shutdown(shutdownCtx)

	// ctx is already cancelled by the signal, so the background loops are
	// winding down. Wait for the in-flight batch before closing the pool —
	// still with /metrics up, so a scrape during the drain sees real numbers.
	bg.Wait()

	metricsErr := metricsSrv.Shutdown(shutdownCtx)

	if err := errors.Join(apiErr, metricsErr); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
