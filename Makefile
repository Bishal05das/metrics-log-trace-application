SHELL := /bin/bash

# Dedicated port block so this project never collides with anything else on
# this machine (you already run an "aegisops" stack on 3000/9090/5434).
#   API 8086 | metrics 9100 | Postgres 5440 | Prometheus 9095 | Grafana 3005
API_ADDR     ?= :8086
METRICS_ADDR ?= :9100
API_URL      ?= http://localhost:8086
METRICS_URL  ?= http://localhost:9100
DATABASE_URL ?= postgres://orders:orders@localhost:5440/orders?sslmode=disable

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

RUN_ENV := HTTP_ADDR=$(API_ADDR) METRICS_ADDR=$(METRICS_ADDR) DATABASE_URL="$(DATABASE_URL)"

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

GRAFANA_URL ?= http://localhost:3005

.PHONY: up
up: ## Start the whole stack (app + postgres + prometheus + alertmanager + grafana)
	docker compose up -d --build
	@echo "waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U orders -d orders >/dev/null 2>&1; do sleep 1; done
	@echo
	@echo "  API           $(API_URL)"
	@echo "  metrics       $(METRICS_URL)/metrics"
	@echo "  Prometheus    $(PROM_URL)"
	@echo "  Alertmanager  $(AM_URL)"
	@echo "  Grafana       $(GRAFANA_URL)"
	@echo
	@echo "  run 'make alertsink' in another terminal to watch alerts arrive"

.PHONY: infra
infra: ## Start ONLY the supporting services, for use with a host-run app (make run)
	docker compose up -d postgres prometheus alertmanager grafana
	@until docker compose exec -T postgres pg_isready -U orders -d orders >/dev/null 2>&1; do sleep 1; done
	@$(MAKE) --no-print-directory target-host

.PHONY: target-host
target-host: ## Point Prometheus at an app running on the host (make run)
	@printf '[\n  {\n    "targets": ["host.docker.internal:9100"],\n    "labels": { "service": "orders" }\n  }\n]\n' \
		> prometheus/targets/orders.json
	@echo "scrape target -> host.docker.internal:9100 (file_sd picks this up within 10s, no restart)"

.PHONY: target-container
target-container: ## Point Prometheus at the containerised app
	@printf '[\n  {\n    "targets": ["api:9100"],\n    "labels": { "service": "orders" }\n  }\n]\n' \
		> prometheus/targets/orders.json
	@echo "scrape target -> api:9100 (file_sd picks this up within 10s, no restart)"

.PHONY: logs
logs: ## Tail the app container logs (raw JSON)
	docker compose logs -f api

.PHONY: logs-pretty
logs-pretty: ## Tail the app logs, one readable line each
	@docker compose logs -f --no-log-prefix api 2>/dev/null | python3 scripts/prettylog.py

.PHONY: traces
traces: ## Render recent traces as a waterfall (TRACE=<id> for one)
	@docker compose logs api --since $(or $(SINCE),2m) --no-log-prefix 2>/dev/null \
		| python3 scripts/waterfall.py $(TRACE)

.PHONY: log-level
log-level: ## Get or set the runtime log level: make log-level L=debug
	@if [ -z "$(L)" ]; then curl -sS $(METRICS_URL)/loglevel; echo; \
	else curl -sS -X PUT "$(METRICS_URL)/loglevel?level=$(L)"; echo; fi


.PHONY: down
down: ## Stop the stack (keeps data)
	docker compose down

.PHONY: nuke
nuke: ## Stop the stack and delete the database volume
	docker compose down -v

.PHONY: run
run: ## Run the API (:8086) + metrics server (:9100)
	$(RUN_ENV) go run -ldflags '$(LDFLAGS)' ./cmd/api

.PHONY: load
load: ## Drive traffic at the API (RATE=50 DURATION=60s)
	go run ./cmd/loadgen -url $(API_URL) -rate $(or $(RATE),20) -duration $(or $(DURATION),60s)

.PHONY: saturate
saturate: ## Saturate the 10-connection pool and watch acquire-wait climb
	go run ./cmd/loadgen -url $(API_URL) -rate 150 -concurrency 128 \
		-slow 0.35 -slow-delay 250ms -duration $(or $(DURATION),40s)

.PHONY: db-metrics
db-metrics: ## Show just the database + pool metrics
	@curl -sS $(METRICS_URL)/metrics | grep -E '^orders_db' | sort

.PHONY: biz-metrics
biz-metrics: ## Show business, worker and backlog metrics
	@curl -sS $(METRICS_URL)/metrics \
		| grep -E '^orders_(created|state_transitions|processing|worker|backlog)' | sort

.PHONY: metrics
metrics: ## Dump the raw /metrics exposition output
	@curl -sS $(METRICS_URL)/metrics

.PHONY: metrics-names
metrics-names: ## List just the metric names + types currently exposed
	@curl -sS $(METRICS_URL)/metrics | grep '^# TYPE' | awk '{printf "  %-52s %s\n", $$3, $$4}' | sort

PROM_URL ?= http://localhost:9095

.PHONY: prom
prom: ## Open the Prometheus UI URL
	@echo "$(PROM_URL)/graph"

.PHONY: prom-targets
prom-targets: ## Show scrape target health
	@python3 scripts/promq.py $(PROM_URL) targets

.PHONY: prom-reload
prom-reload: ## Apply prometheus.yml changes without restarting
	@curl -sS -X POST $(PROM_URL)/-/reload && echo "reloaded"

.PHONY: prom-top
prom-top: ## Which metric names hold the most series
	@python3 scripts/promq.py $(PROM_URL) top

AM_URL ?= http://localhost:9096

.PHONY: alertsink
alertsink: ## Run the local Alertmanager webhook receiver (:9101)
	go run ./cmd/alertsink -addr :9101

.PHONY: rules
rules: ## List loaded recording + alerting rules and their state
	@python3 scripts/promq.py $(PROM_URL) rules

.PHONY: alerts
alerts: ## Show alerts currently pending or firing in Prometheus
	@python3 scripts/promq.py $(PROM_URL) alerts

.PHONY: am-alerts
am-alerts: ## Show alerts Alertmanager currently holds (post-grouping)
	@python3 scripts/promq.py $(AM_URL) amalerts

.PHONY: grafana
grafana: ## Open the Grafana URL
	@echo "$(GRAFANA_URL)"

.PHONY: check-dashboards
check-dashboards: ## Run every dashboard panel query against Prometheus
	@python3 scripts/check_dashboards.py $(PROM_URL) grafana/dashboards

.PHONY: check
check: vet test check-rules check-dashboards ## Everything CI should run

.PHONY: check-rules
check-rules: ## Validate rule files with promtool before loading them
	@docker run --rm -v $(PWD)/prometheus/rules:/rules:ro \
		--entrypoint sh prom/prometheus:v3.1.0 -c 'promtool check rules /rules/*.yml'

.PHONY: q
q: ## Run a PromQL query: make q Q='sum(rate(orders_http_requests_total[1m]))'
	@python3 scripts/promq.py $(PROM_URL) query '$(Q)'

.PHONY: psql
psql: ## Open a psql shell against the project database
	docker compose exec -it postgres psql -U orders -d orders

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: build
build: ## Compile to ./bin/api
	go build -ldflags '$(LDFLAGS)' -o bin/api ./cmd/api

.PHONY: test
test: ## Run tests
	go test ./... -count=1

.PHONY: vet
vet: ## Static checks
	go vet ./...

.PHONY: smoke
smoke: ## Hit every endpoint once
	@set -euo pipefail; \
	echo "--- healthz ---"; curl -sS $(API_URL)/healthz; echo; \
	echo "--- readyz ---";  curl -sS $(API_URL)/readyz;  echo; \
	echo "--- create ---"; \
	resp=$$(curl -sS -X POST $(API_URL)/orders \
		-H 'Content-Type: application/json' \
		-d '{"customer_id":"cust-1","amount_cents":4999,"currency":"USD"}'); \
	echo "$$resp"; \
	id=$$(echo "$$resp" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p'); \
	echo "--- get $$id ---"; curl -sS $(API_URL)/orders/$$id; echo; \
	echo "--- list ---"; curl -sS "$(API_URL)/orders?limit=5"; echo; \
	echo "--- 404 ---"; curl -sS -o /dev/null -w 'status=%{http_code}\n' $(API_URL)/orders/00000000-0000-0000-0000-000000000000; \
	echo "--- 422 ---"; curl -sS -X POST $(API_URL)/orders -H 'Content-Type: application/json' -d '{"customer_id":"","amount_cents":0,"currency":"US"}'; echo
