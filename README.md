# Production Metrics with Go and PostgreSQL

A working orders service instrumented the way a real production service is, built
in phases so each layer's reasoning is visible.

```
Go app  ──/metrics──>  Prometheus  ──>  Alertmanager  ──>  webhook sink
  │  │                     │
  │  └── JSON logs ─> stdout        └──>  Grafana
  └── PostgreSQL
```

Two pillars so far: **metrics** (complete, with alerting and dashboards) and
**logs** (structured, correlated, emitted to stdout). No log aggregation backend
yet — the app writes JSON to stdout, which is the whole contract; shipping it
somewhere is a separate concern deliberately left for later.

## Quick start

```bash
make up          # whole stack: app, postgres, prometheus, alertmanager,
                 # grafana, elasticsearch, logstash, kibana
make load        # drive synthetic traffic
make alertsink   # in another terminal: watch alerts arrive

make logs-pretty            # tail the app logs, one readable line each
make log-level              # read the current level
make log-level L=debug      # raise it at runtime, no restart

make elk-health             # is the log pipeline actually moving?
make logs-es Q='level:ERROR'      # query shipped logs from the terminal
make logs-trace T=<trace_id>      # every log line for one trace
make elk-mapping            # how Elasticsearch actually mapped each field

make trace-health           # are spans reaching Jaeger, and is storage taking them?
make traces                 # slowest traces in the last hour
make trace T=<trace_id>     # one trace as a waterfall
make jaeger-clean           # delete trace indices older than 3 days
```

| | URL |
|---|---|
| API | http://localhost:8086 |
| App metrics | http://localhost:9100/metrics |
| Prometheus | http://localhost:9095 |
| Alertmanager | http://localhost:9096 |
| Grafana | http://localhost:3005 |
| Kibana | http://localhost:5602 |
| Elasticsearch | http://localhost:9201 |
| Jaeger | http://localhost:16687 |

Jaeger's ports are shifted off its defaults (16686/4317/4318) because another
stack on this machine holds them; the app reaches the collector as `jaeger:4318`
on the compose network, where the container port is unchanged.

`make up` now starts a JVM stack (Elasticsearch, Logstash, Kibana) alongside the
rest. Heaps are capped deliberately low — 512m ES, 256m Logstash — so the whole
thing fits in roughly 2 GB. `make elk` starts just those three if you want the
metrics stack on its own first.

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
internal/logging   slog setup, request-ID correlation, redaction, sampling, level switch
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

**8 — Logs.** The second pillar, application side only. A `ContextHandler`
attaches request-scoped attributes to every record, so one `X-Request-Id` ties
the HTTP line, every database query it ran, and any error together — with no
call site knowing the ID exists. Secrets are redacted centrally in
`ReplaceAttr`, because relying on developers to remember is how credentials
reach a log store. Log level is changeable at runtime on the admin port: the
moment you want debug logs is during an incident, and a restart destroys the
state you were trying to observe.

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

## Logging

One structured line per request, plus DB queries at debug:

```json
{"level":"INFO","msg":"http request","service":"orders","method":"POST",
 "route":"/orders","status":201,"duration_ms":1.089,"bytes":220,
 "request_id":"85655692c3909e573c0bf72b3be3037e"}
```

- **Correlation.** `X-Request-Id` is honoured from upstream (sanitised first —
  the header is attacker-controlled) or generated, echoed to the client, and
  attached to every log line for that request via the context.
- **Severity follows fault.** 5xx is our bug (`ERROR`); 4xx is the client's
  mistake (`WARN`). Logging 4xx at error fills the error stream with people
  typing bad UUIDs.
- **`route` is the matched template**, never the raw path — the same cardinality
  discipline as the metrics label, and the same value, so you can pivot between
  the two.
- **Health probes are excluded**, same as in metrics.
- **Redaction** happens in one place, `ReplaceAttr`; `PII()` pseudonymises
  identifiers so you can still correlate a customer's events without storing who
  they are.
- **Use the `*Context` variants.** `log.Info()` passes `context.Background()` and
  silently loses correlation. Do not use `WithGroup` on this logger — it nests
  `request_id` inside the group.
- **Sampling is a cost control, in the application.** At most `LOG_SAMPLE_N`
  (default 50) records per distinct message per `LOG_SAMPLE_INTERVAL` (default
  1s); WARN and above are never sampled. Dropped lines are counted and the count
  is attached to the next surviving record as `sampled_dropped`, so suppression
  is never silent. Set `LOG_SAMPLE_N=0` to disable.

  Measured under load: 2,999 requests produced 1,668 indexed access-log lines
  plus 1,372 recorded drops — 45% suppressed, none of it invisible.

- **Log volume is a METRIC, not just a log field.** `orders_log_records_total`
  and `orders_log_records_dropped_total` (both by `level`) are counted in the
  application *before* sampling. That placement is the point: `sampled_dropped`
  only exists once a document has been written, shipped, parsed and indexed, so
  the moment shipping breaks you lose the number describing your log volume
  along with the logs. A counter upstream of all of it survives, and makes
  volume alertable with the same `rate()` as everything else.

  `internal/logging` never imports `internal/metrics` — it declares a
  `Recorder` interface and `metrics.Logs` satisfies it, the same consumer-side
  pattern as `metrics.PoolStats` and `worker.Store`.

## Log aggregation (ELK)

```
app (JSON → stdout)
  → Docker json-file driver
    → Logstash  (tail, parse, drop foreign lines, retype)
      → Elasticsearch  (orders-logs-YYYY.MM.DD, 7d ILM)
        → Kibana (explore)  +  Grafana (correlate beside metrics)
```

Why tail files rather than use Docker's `gelf` driver: those drivers *replace*
`json-file`, so `docker logs` returns nothing and every local habit built on
`make logs-pretty` / `make traces` dies with it. The cost is that Logstash runs
as root to read `/var/lib/docker/containers`. In production this is Filebeat as
a DaemonSet with the same mount.

Two UIs, deliberately:

| | Use it for |
|---|---|
| **Kibana** (`make kibana`) | exploring. Four provisioned saved searches — errors and warnings, slow requests, database queries, background worker — and an "Orders — Logs" dashboard, all imported from `kibana/saved-objects.ndjson` |
| **Grafana** (RED dashboard, Logs row) | correlating. A logs panel sharing the dashboard's time range, so the lines explaining a latency spike are already on screen for that exact window |

Kibana's saved objects are provisioned from a file in git for the same reason
the Grafana dashboards are: a view that exists only in Kibana's saved-object
index cannot be reviewed or diffed and vanishes with the container.

Things that matter more than they look:

- **Apply the index template before the first document.** A mapping ES invents
  by guessing cannot be changed without reindexing. `scripts/elk_init.sh` is
  idempotent and runs from `make up`.
- **`keyword`, not `text`, for everything except `message`.** ES maps strings as
  `text` *plus* a `.keyword` subfield by default — double storage. A
  `dynamic_template` turns that off; `message` keeps its subfield because "which
  message is flooding my index?" is a question you actually ask. `stack` is
  stored but not indexed: you read stack traces, you never search them.
- **Prometheus watches the log pipeline, not Elasticsearch.** A store cannot
  alert you that it stopped being written to. `OrdersLogShippingStopped` fires on
  `orders-logs-*` document count flat *while the service is serving traffic* —
  the `and` clause is what stops it crying wolf on an idle service overnight.
- **`start_position => "end"`.** `beginning` backfills every byte of every
  matched log file; measured here at 4.5M lines read to keep 1,020.
- **Comments go in `_meta`, never in `properties`.** Elasticsearch parses every
  key under `properties` as a field mapping, so a explanatory string there fails
  the whole template with `Expected map for property [fields]` — and `curl -sf`
  hides it unless you check the exit code.

## Traces (Jaeger)

```
app (OTLP/HTTP) → Jaeger collector → Elasticsearch (jaeger-span-*)
                                       ↓
                            Jaeger UI  +  Grafana
```

Jaeger stores into the **same Elasticsearch as the logs**: one storage system,
one thing to operate and back up. The cost is that trace indices compete with
log indices for the same heap, which is why traces are kept for 3 days against
the logs' 7 — spans are far bulkier per useful question answered.

Switching from the stdout exporter was not only about getting a UI. Those
pretty-printed spans were going to stderr, into the same Docker log files
Logstash tails: **Logstash's read rate dropped from ~45,000 lines/s to 0/s.**
Almost all of the log pipeline's load was the traces pillar shouting into it.

### The three links between pillars

| From | To | Mechanism |
|---|---|---|
| metrics → traces | exemplar diamond on any latency histogram | `exemplarTraceIdDestinations` → `datasourceUid: jaeger` |
| logs → traces | `trace_id` is clickable on every log line | Elasticsearch datasource `dataLinks` |
| traces → logs | a span opens the log lines written during it | `tracesToLogsV2`, ±1s window |
| traces → metrics | a slow span opens that route's rate and p99 | `tracesToMetrics`, joined on `http.route` |

All four join on values the application already emits. Nothing in the backends
is doing correlation work — `tracing.Middleware` puts `trace_id` on the logging
context, `routeLabel` gives metrics and traces the same route template, and
`internal/metrics/exemplar.go` attaches the trace ID to histogram observations.

### Retention

Jaeger creates its own composable index templates, and composable templates do
not merge — a second template matching `jaeger-span-*` would replace Jaeger's
mappings rather than add a lifecycle policy to them. So retention is the
official index cleaner (`make jaeger-clean`, run on a schedule), which is how
Jaeger-on-Elasticsearch is actually operated. `ES_USE_ILM=true` is the
alternative and additionally needs rollover aliases and an init job.

## Not built here

- **A deadman's switch for alert delivery.** `alertmanager_notifications_failed_total`
  has no alert on it, and an alert about the delivery path is circular anyway —
  that one needs an external watchdog.
- **Tail-based sampling.** `TRACING_SAMPLE_RATIO` is head-based: the decision is
  made at the root span, before anything is known about how the request turned
  out. Keeping 100% of *errors and slow requests* and 1% of the rest needs a
  collector that buffers whole traces — the OpenTelemetry Collector's
  `tailsamplingprocessor` between the app and Jaeger.
- **Span metrics.** Jaeger is configured with `METRICS_STORAGE_TYPE=prometheus`,
  but the RED-metrics-from-spans view needs the collector's `spanmetrics`
  connector to be generating them.
