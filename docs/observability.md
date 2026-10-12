# Local observability profile

The optional profile in `deploy/observability/compose.yaml` adds a loopback-only Prometheus scrape (59091), Grafana (53000) and Tempo (53200). The worker exposes `/metrics` on 59090 when `RELAYDESK_METRICS_ADDR` is set, including while outbound execution is disabled. `relaydesk_worker_outbound_enabled=0` distinguishes an intentionally idle worker from working business processing, and `RelayDeskOutboundDisabled` alerts on that state. Grafana provisions the seven-panel **RelayDesk Operations** dashboard and Prometheus/Tempo data sources. The worker can export sanitized spans to Tempo via OTLP/HTTP. This profile is for a controlled local or staging environment; it is not a production monitoring deployment. [Prometheus alert rules](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/) require an Alertmanager or another notification layer for paging; this profile evaluates and displays alerts but does not send pages. Tempo's single-binary OTLP receiver configuration follows [Grafana's local deployment guide](https://grafana.com/docs/tempo/latest/set-up-for-tracing/setup-tempo/deploy/locally/linux/).

Create a private ignored `.env.observability.secrets` (mode `0600`) with one random `RELAYDESK_GRAFANA_ADMIN_PASSWORD` value. Do not put it in Git or a report. Validate the overlay with:

```sh
docker compose --env-file .env --env-file .env.observability.secrets -f compose.yaml -f deploy/observability/compose.yaml config -q
```

Only enable the worker after checking the database `restore_safety_gate`, the provider account bindings and `RELAYDESK_OUTBOUND_EXECUTION_ENABLED=true` in that **isolated** deployment. Running the profile against a Stage 0 database with pending real actions could make provider calls. To view only the monitoring services without starting a worker, use `docker compose ... up -d --no-deps prometheus tempo grafana`; Prometheus will correctly show `up=0` until a safe worker is present. Use the same options for `down` to preserve or remove only the chosen profile's volumes according to the local policy.

The OTel exporter uses fixed `provider`, `status_class`, `kind`, `category`, and `measure` values. It never labels a metric with tenant, customer, invoice, connection or token. `relaydesk_operational_value` carries aggregate PostgreSQL ages/counts refreshed every thirty seconds; `sample_age_seconds` exposes stale database observations even while the HTTP API responds. `up` and `broker_connected` distinguish worker and NATS outages. Webhook acceptance/rejection/duplicate counters, provider request status/latency, business action outcomes, CRM sync duration, OAuth refresh and 429 events complement the database gauges. Detailed IDs remain in tenant-authorized run history, not in time-series labels.

`alerts.yml` has thirteen thresholds with a `for` duration and runbook annotation, including the outbound-disabled signal. Test configuration and PromQL behavior without starting LenoirDesk or touching a provider:

```sh
docker run --rm -v "$PWD/deploy/observability:/work:ro" -w /work --entrypoint /bin/promtool prom/prometheus:v3.14.0 check rules alerts.yml
docker run --rm -v "$PWD/deploy/observability:/work:ro" -w /work --entrypoint /bin/promtool prom/prometheus:v3.14.0 test rules alerts.test.yml
```

The integration fault test inserts a synthetic ten-minute-old outbox row, observes PostgreSQL's actual age, scrapes the OTel Prometheus endpoint, then removes the injected outbox row. A wrong simulated HubSpot credential produces a persisted `blocked_auth` action and a sanitized provider metric. The rule test confirms `RelayDeskOutboxDelayed` is pending at four minutes and firing at six minutes under the synthetic series; it also checks coverage-gap, stale-database, persistent-auth and outbound-disabled alerts. These checks validate instrumentation and rule semantics, not a continuous production alert delivery path. The seventh Grafana panel's live rendering and an end-to-end trace query remain unverified.

Thresholds are initial pilot values: outbox/due age five minutes, discovery lag one hour, unknown age fifteen minutes, dead age one hour, database size two GiB. Tune them from measured service levels and tenant load, not by disabling a noisy failure signal. Provider quotas and external outages can make a healthy process operationally degraded. The API `/health/ready` remains dependency readiness, not a promise that business synchronization is current.
