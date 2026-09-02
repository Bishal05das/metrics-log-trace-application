package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewServer builds the admin/metrics HTTP server.
//
// Why a SECOND server on its own port instead of adding /metrics to the API:
//
//  1. Exposure. Your API port is behind a public ingress. /metrics leaks route
//     names, error codes, traffic volumes and version numbers — it is internal
//     telemetry, not a public endpoint. Separate port = separate firewall
//     rule, separate Kubernetes Service, trivially kept internal.
//  2. Isolation. If the API port is saturated or its handlers are wedged,
//     you still want scrapes to succeed — that is exactly when you most need
//     the data. A separate listener has its own accept queue.
//  3. Clean accounting. /metrics never appears in your own request metrics,
//     so a 15s scrape interval does not pollute your RPS and latency numbers.
//
// This is the standard production layout, and it costs about 30 lines.
func NewServer(addr string, reg *prometheus.Registry, log *slog.Logger) *http.Server {
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// Return HTTP 500 if a collector fails, so a broken exporter shows up
		// as a scrape failure instead of silently serving partial data.
		ErrorHandling: promhttp.HTTPErrorOnError,
		ErrorLog:      promLogger{log},

		// OpenMetrics is the IETF-track successor to the Prometheus text
		// format. Enabling it costs nothing and is required for exemplars
		// (the mechanism that links a histogram bucket to a trace ID — which
		// is how the metrics pillar connects to the traces pillar later).
		EnableOpenMetrics: true,

		// Backpressure. Scrapes are cheap, but a misconfigured Prometheus or a
		// stampede of scrapers should never be able to take the process down.
		MaxRequestsInFlight: 5,
		Timeout:             10 * time.Second,
	})

	// InstrumentMetricHandler makes the metrics endpoint monitor itself. It
	// adds promhttp_metric_handler_requests_total (by code) and
	// ..._requests_in_flight. If scrapes start failing, this is where you see
	// it from the inside.
	handler = promhttp.InstrumentMetricHandler(reg, handler)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)

	// A tiny landing page, purely so a human who opens the port knows what it
	// is. Costs nothing and saves confusion.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>orders admin</title>
<h1>orders — admin port</h1><p><a href="/metrics">/metrics</a></p>`)
	})

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// promLogger adapts slog to promhttp's tiny Logger interface.
type promLogger struct{ log *slog.Logger }

func (p promLogger) Println(v ...any) {
	p.log.Error("metrics handler", "error", fmt.Sprint(v...))
}
