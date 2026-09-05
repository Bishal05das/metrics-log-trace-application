// Package config loads runtime configuration from the environment.
//
// Everything has a default that matches docker-compose.yml, so `make up && make
// run` works with no .env file at all.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Env is "dev" or "prod". It becomes a label on some metrics later, so it
	// must be a low-cardinality, closed set of values.
	Env string

	HTTPAddr string
	// MetricsAddr is a SEPARATE listener for /metrics. Keeping telemetry off
	// the public API port is the standard production layout — see
	// internal/metrics/server.go for why.
	MetricsAddr     string
	ShutdownTimeout time.Duration

	DatabaseURL string
	DBMaxConns  int32
	DBMinConns  int32

	LogLevel string
	// LogFormat is "json" (machine-readable, what a log pipeline needs) or
	// "text" (readable in a terminal during local development).
	LogFormat string
	// LogSource adds file:line to every record. Costs a runtime.Caller per
	// record, so it is off by default and flipped on during an investigation.
	LogSource bool
	// SlowQueryThreshold promotes a query to a WARN log line regardless of the
	// configured level. Slow queries are rare by construction, which is what
	// makes it affordable to always log them.
	SlowQueryThreshold time.Duration

	// Tracing. Defaults to the stdout exporter so traces are visible with no
	// backend running; point OTEL_EXPORTER at "otlp" once you have one.
	TracingEnabled      bool
	TracingExporter     string
	TracingOTLPEndpoint string
	TracingOTLPInsecure bool
	TracingSampleRatio  float64

	// Background worker.
	WorkerEnabled     bool
	WorkerInterval    time.Duration
	WorkerBatchSize   int
	WorkerFailureRate float64

	// How often the absolute backlog is recomputed. Deliberately decoupled
	// from the Prometheus scrape interval — see worker.BacklogRefresher.
	BacklogInterval time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Env:             env("APP_ENV", "dev"),
		HTTPAddr:        env("HTTP_ADDR", ":8087"),
		MetricsAddr:     env("METRICS_ADDR", ":9108"),
		ShutdownTimeout: 15 * time.Second,
		DatabaseURL: env("DATABASE_URL",
			"postgres://orders:orders@localhost:5440/orders?sslmode=disable"),
		LogLevel:           env("LOG_LEVEL", "info"),
		LogFormat:          env("LOG_FORMAT", "json"),
		LogSource:          env("LOG_SOURCE", "false") == "true",
		SlowQueryThreshold: 200 * time.Millisecond,

		TracingEnabled:      env("TRACING_ENABLED", "true") == "true",
		TracingExporter:     env("OTEL_EXPORTER", "stdout"),
		TracingOTLPEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		TracingOTLPInsecure: env("OTEL_EXPORTER_OTLP_INSECURE", "true") == "true",

		WorkerEnabled:   env("WORKER_ENABLED", "true") == "true",
		WorkerBatchSize: 20,
		// An 8% failure rate makes the failure path visible in a short demo.
		// A real payment processor is nearer 1-2%.
		WorkerFailureRate: 0.08,
	}

	var err error
	if cfg.TracingSampleRatio, err = envFloat("TRACING_SAMPLE_RATIO", 1.0); err != nil {
		return Config{}, err
	}
	if cfg.TracingSampleRatio < 0 || cfg.TracingSampleRatio > 1 {
		return Config{}, fmt.Errorf("config: TRACING_SAMPLE_RATIO must be 0..1, got %v", cfg.TracingSampleRatio)
	}
	if cfg.WorkerInterval, err = envDuration("WORKER_INTERVAL", 500*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.BacklogInterval, err = envDuration("BACKLOG_INTERVAL", 15*time.Second); err != nil {
		return Config{}, err
	}
	if n, err := envInt32("WORKER_BATCH_SIZE", 20); err != nil {
		return Config{}, err
	} else {
		cfg.WorkerBatchSize = int(n)
	}
	if cfg.DBMaxConns, err = envInt32("DB_MAX_CONNS", 10); err != nil {
		return Config{}, err
	}
	if cfg.DBMinConns, err = envInt32("DB_MIN_CONNS", 2); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}

	// A deliberately small pool. Ten connections is easy to saturate with a
	// load generator, which is exactly what we want in Phase 3 when we look at
	// connection-pool saturation metrics.
	if cfg.DBMaxConns < cfg.DBMinConns {
		return Config{}, fmt.Errorf("DB_MAX_CONNS (%d) < DB_MIN_CONNS (%d)", cfg.DBMaxConns, cfg.DBMinConns)
	}
	return cfg, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt32(key string, def int32) (int32, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return int32(n), nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return d, nil
}

func envFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return f, nil
}
