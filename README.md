# Production Metrics with Go and PostgreSQL

A working orders service instrumented the way a real production service is, built
in phases so each layer's reasoning is visible.

```
Go app  ──/metrics──>  Prometheus  ──>  Alertmanager  ──>  webhook sink
  │                        │
  └── PostgreSQL           └──────────>  Grafana
```

## Quick start

```bash
make up          # whole stack: app, postgres, prometheus, alertmanager, grafana
make load        # drive synthetic traffic
make alertsink   # in another terminal: watch alerts arrive
```

| | URL |
|---|---|
| API | http://localhost:8086 |
| App metrics | http://localhost:9100/metrics |
| Prometheus | http://localhost:9095 |
| Alertmanager | http://localhost:9096 |
| Grafana | http://localhost:3005 |

Ports are deliberately non-default so this stack never collides with anything
else on the machine.

### Developing against the stack

```bash
make infra              # supporting services only, scrape target -> host
make run                # app on the host, fast edit-rebuild loop
make target-container   # switch Prometheus back to the containerised app
```

`make target-host` / `make target-container` rewrite
`prometheus/targets/orders.json`. Prometheus watches that file via `file_sd` and
picks up the change within 10s — no restart, no reload call.

### Checks

```bash
make check   # vet + tests + promtool rule validation + dashboard panel queries
```

`make check-dashboards` runs every panel query against Prometheus. A panel that
silently returns nothing is the most common observability failure — someone
renames a metric and the graph looks exactly like an idle system.

## Layout

```
cmd/api            service entrypoint: two HTTP servers, worker, graceful shutdown
cmd/loadgen        open-loop traffic generator, incl. pool-saturation mode
cmd/alertsink      local Alertmanager webhook receiver
internal/domain    Order, Status, validation — no DB or HTTP imports
internal/store     pgxpool, migrations, named SQL constants
internal/httpapi   routes, handlers, middleware chain
internal/metrics   registry, RED, database, business metrics — all metrics live here
internal/worker    background processor + cached-backlog refresher
prometheus/        scrape config, recording rules, alert rules, file_sd targets
alertmanager/      routing, grouping, inhibition
grafana/           provisioned datasource + 4 dashboards
```

## What each phase established

**0 — The application.** Six decisions made here exist only to make metrics
possible later: stdlib `ServeMux` so `r.Pattern` yields route *templates*;
`Status` as a closed set; stable error `code` separate from free-text `message`;
every SQL statement a named constant; liveness and readiness as different things;
a deliberately small connection pool.

**1 — `/metrics`.** The pull model: a text file your process regenerates on every
scrape. Counter / gauge / histogram / summary, and why histograms aggregate while
summaries do not. A custom registry rather than the global default. `/metrics` on
its own port.

**2 — RED.** Rate, Errors, Duration. The `route` label is a **cardinality
firewall** — 200 attacker-chosen URLs collapse to one series. Bucket boundaries
chosen deliberately: `DefBuckets` would have put 99.6% of traffic in the first
bucket. A label derived from another label (`status_class` from `code`) costs
zero extra series; an independent one multiplies.

**3 — USE.** The database pool as a resource. A custom `Collector` reads
`pgxpool.Stat()` at scrape time rather than mirroring it into gauges. The
experiment: under saturation, query time stayed *flat* while acquire wait rose
**104,648×**. The database was never slow. Without `acquire_wait` you would spend
a day optimising the wrong thing.

**4 — Business metrics.** Flow (transition counters) and level (cached gauges),
and why you need both. Expensive gauges are refreshed out-of-band, never at scrape
time — and any metric computed out-of-band must publish its own freshness.
Alert on queue *age*, not length.

**5 — Prometheus.** `relabel_configs` (targets) vs `metric_relabel_configs`
(samples), and that the latter is per-job. Native histograms discard classic
buckets by default. `rate` vs `irate` vs `increase`; rate before sum;
`histogram_quantile` needs `by (le)`; vector matching and `ignoring()`.

**6 — Rules and alerting.** Recording rules give one definition shared by
dashboards, alerts and SLO reports. `or vector(0)` is the wrong NaN guard — it
tests for a *missing series*, not a NaN *value*, and creates a phantom labelless
series; `clamp_min` on the denominator is correct. Alert on symptoms, not causes.
`up == 0` and `absent()` because threshold alerts cannot fire without data.
Multi-window multi-burn-rate SLO alerts. Alertmanager grouping, routing and
inhibition turned 5 firing alerts into **1 delivered notification**.

**7 — Dashboards.** Provisioned as code, not clicked into a UI. Multi-stage
distroless image: **15.1 MB**, non-root, no shell.

## Metric inventory

| Metric | Type | Labels |
|---|---|---|
| `orders_http_requests_total` | counter | route, method, code, status_class |
| `orders_http_request_duration_seconds` | histogram | route, method, status_class |
| `orders_http_response_size_bytes` | histogram | route, method |
| `orders_http_requests_in_flight` | gauge | — |
| `orders_http_panics_total` | counter | route |
| `orders_db_query_duration_seconds` | histogram | query, status |
| `orders_db_query_errors_total` | counter | query, kind (SQLSTATE) |
| `orders_db_pool_acquire_wait_seconds` | histogram | — |
| `orders_db_pool_connections` | gauge | state |
| `orders_created_total` | counter | currency |
| `orders_created_value_cents_total` | counter | currency |
| `orders_state_transitions_total` | counter | from, to |
| `orders_processing_duration_seconds` | histogram | outcome |
| `orders_worker_runs_total` | counter | result |
| `orders_backlog_orders` | gauge | status |
| `orders_backlog_oldest_age_seconds` | gauge | — |
| `orders_backlog_last_refresh_timestamp_seconds` | gauge | — |
| `orders_build_info` | gauge | version, commit, go_version, env |

Plus Go runtime and process collectors.

## The rules worth carrying to other projects

1. **A time series is a metric name plus its exact label set.** Every cardinality
   disaster is someone forgetting this.
2. **Never label with user-controlled input** unless it is validated against a
   closed set at the edge. URLs and JSON fields are attacker-controlled.
3. **Histograms are where cardinality goes.** One label costs `len(buckets)+2`
   series per combination.
4. **Put a bucket boundary exactly on your SLO threshold**, or you are reporting
   an interpolated guess.
5. **Utilisation is not saturation.** 100% utilised with zero wait is healthy.
6. **Read state at scrape time when it is cheap and local**; refresh out-of-band
   when it is expensive or remote — and publish the refresh timestamp.
7. **Alert on symptoms, not causes**, and always have one alert for the metric
   disappearing entirely.
8. **Read your raw `/metrics` output.** It tells you when your instrumentation is
   lying, which it did three times while building this.

## Fault injection

`GET /debug/slow?d=250ms` holds a pooled connection for the given duration. Dev
only (`APP_ENV=dev`), because an unauthenticated "hold a database connection"
endpoint is a denial-of-service primitive.

```bash
make saturate   # 150 rps, 35% slow queries -> pool exhaustion, alerts fire
```

## Not built here

Logs and traces — the other two pillars. The service uses `log/slog` with JSON
output and the metrics layer already emits OpenMetrics with exemplar support,
which is the hook tracing would attach to.
