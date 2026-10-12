# RelayDesk pilot operations

These procedures are for the single-node test/pilot deployment. They do not establish high availability. Inspect the tenant-scoped Reconciliation and Runs screens before changing work. Keep provider payloads, tokens, local `.env*`, backup dumps, and encryption keys out of tickets and shared logs. Readiness (`/health/ready`) checks dependencies; business progress requires the operational metrics and run history.

Start the optional observability profile only with a local ignored `.env.observability.secrets` containing `RELAYDESK_GRAFANA_ADMIN_PASSWORD=<random local value>` and explicit outbound clearance in the separate local environment. Check `docker compose -f compose.yaml -f deploy/observability/compose.yaml --env-file .env --env-file .env.observability.secrets config -q` before `up`. Prometheus is on loopback port 59091, Grafana on 53000, and the worker metrics endpoint on 59090. Alert rule `runbook` annotations point to the headings below. The worker's `RELAYDESK_OUTBOUND_EXECUTION_ENABLED` defaults to `false`; only set it to `true` after verifying the database is not in restore recovery and the account bindings are correct.

## PostgreSQL unavailable

Symptom: `/health/ready` fails, webhook acceptance returns 503, `RelayDeskDatabaseObservationStale` fires. Confirm `docker compose ps postgres` and `docker compose exec -T postgres pg_isready -U postgres -d relaydesk`; inspect local PostgreSQL logs without exporting credential-bearing lines. The API rejects new webhook work; the worker cannot claim actions and does not safely infer that a timed-out external request failed. Restore the database service or use the restore procedure below. Do not acknowledge webhooks, force action success, or restart outbound execution from an older backup. Verify readiness, a fresh metrics sample (`sample_age_seconds < 90`), outbox age recovery, and tenant run progress. A failed external request can remain `unknown` and need review.

## NATS unavailable

Symptom: outbox age rises, `RelayDeskBrokerUnavailable` or `RelayDeskOutboxDelayed` fires. Confirm `docker compose ps nats`, JetStream health at the loopback monitoring port 58222, and the tenant run's published state. PostgreSQL retains the obligation; the relay retries and the repair loop reissues fenced pointers. Restore NATS, then verify broker metric is 1 and outbox age declines. Do not delete outbox rows or manually publish guessed payloads. A lost broker volume can lengthen recovery but should not erase database obligations.

## Redis unavailable

Symptom: provider calls report quota/limiter unavailability and `relaydesk_provider_events_total{kind="redis_outage"}` rises. Confirm `docker compose ps redis` and `docker compose exec -T redis redis-cli ping`. The write limiter, quota and breaker fail closed; ordinary retries wait. Restore Redis and verify retries resume at bounded rate. Do not bypass limits or increase provider scopes. A Redis data loss can reset cooldown state; watch 429s after recovery.

## Stripe webhook delivery failures

Symptom: Stripe reports failed deliveries or webhook rejection rate rises. Check destination URL, signed delivery diagnostic code, account/mode binding, API version, time synchronization (`make clock-preflight`), and Stripe connection health. The API commits inbox/job/outbox before 2xx; invalid signatures and wrong accounts remain rejected. Repair the destination or secret using the approved connection flow, then redeliver a signed test event and confirm durable inbox and run. Do not disable signature or account verification. Missed events require Events API permission and reconciliation coverage; a 403 is a gap, not success.

## HubSpot prolonged outage

Symptom: CRM actions enter retry/blocked/dead and due age rises. Confirm HubSpot connection health, provider status, quota metrics, and one tenant's run history. Safe retry uses the persisted effect key and current Stripe read/HubSpot compare. Restore authentication or provider availability; use operator retry for dead actions only after cause removal. Do not overwrite unmanaged Contact fields or blindly recreate an archived/merged Contact. Verify current managed properties and projection hash; archived identity may still require explicit mapping repair.

## Slack unknown outcome

Symptom: action is `unknown` or `RelayDeskUnknownActionAged` fires. Inspect the private destination channel using authorized access and correlate invoice, channel and time with the action's sanitized run reference. Automatic replay is disabled. If the message exists, use the audited `confirm_unknown` decision with exact channel and Slack timestamp; otherwise an operator may choose `risk_retry` with duplicate-risk acknowledgment. Do not reset the action row or resend from a terminal. Verify the audited decision and run status. Slack cannot provide a universal exactly-once guarantee after a lost response.

## OAuth token expired or revoked

Symptom: connection is `blocked_auth`, refresh-failure counter rises, or `RelayDeskAuthFailures` fires. Confirm tenant, provider account ID, granted scopes and connection health through the UI. Refresh/retry stops on revoked credentials; reconnect the same expected account through the guided flow. Do not copy tokens into logs or silently attach another portal/workspace. Verify read-only identity binding and then one controlled run. Existing unknown effects still require separate review.

## Provider rate-limit exhaustion

Symptom: 429 events, quota waits and overdue actions rise. Inspect per-provider status class and bounded retry category, plus Redis availability. The quota and breaker defer work without sleeping indefinitely in worker slots. Wait for provider cooldown, lower load or adjust a documented local budget after measuring. Do not bypass Redis or send bursts from manual scripts. Verify 429 rate falls and due age declines; recovery can be slower than a provider's published limit.

## Reconciliation coverage gap

Symptom: Reconciliation screen shows `gap_code`, `RelayDeskCoverageGap` fires or event lag exceeds one hour. Confirm Stripe Events Read permission, account/mode, API version, checkpoint window and the provider's 30-day event history boundary. The checkpoint does not advance across a failed page. Restore permission or quota, then use bounded operator historical reconciliation within the available window; use known-customer current-state scan for older gaps. Do not mark a gap scanned or send historical invoice notifications. Verify checkpoint progress, discovered inbox rows, CRM convergence and the gap clearing. Current-state repair cannot reconstruct all missing historical business events.

## Large tenant backlog

Symptom: one tenant's due/outbox age rises while others progress. Compare tenant operational status and connection health; check global queue utilization and Redis quota waits. Fair bounded claims and fixed page sizes prevent one reconciliation request from monopolizing execution. Fix the blocking connection, pause that tenant's automation if necessary, then resume after review. Do not widen worker concurrency or replay all events in one batch. Verify other tenants still advance and this tenant's age declines; a provider-specific quota may dominate throughput.

## Storage pressure

Symptom: `RelayDeskStoragePressure` fires or PostgreSQL disk approaches capacity. Confirm disk free space, database size metric, and retention batch counts. The hourly bounded retention job redacts eligible raw payloads and removes terminal history while preserving unfinished work and effect tombstones. Add storage or run a documented bounded retention batch with the configured minima; secure older synthetic drill dumps separately. Do not delete inbox/effect keys, unfinished actions, or WAL files manually. Verify free space and fresh backup integrity. Retained identity tombstones and active evidence still consume storage.

## Worker crash or restart

Symptom: worker `up=0`, `relaydesk_worker_outbound_enabled=0`, expired leases or stalled actions. Confirm `docker compose ps worker`, process exit, external restore witness, database gate and outbound environment clearance. The metrics listener remains available while the worker is deliberately idle, so `up=1` alone does not prove processing. PostgreSQL leases and outbox survive restart; watchdog repairs fenced pointers, ambiguous Slack attempts become `unknown`. Restart only after checking that this is not a restored database. Do not clear leases or manually acknowledge NATS messages. Verify `up=1`, `relaydesk_worker_outbound_enabled=1`, `expired_leases=0`, outbox age declines and no unknown action was silently retried. Already transmitted HTTP requests may finish after a crash.

## Database restore

Use [the restore procedure](../backup-restore.md). Stop all worker instances first. Restore into an isolated target, enter recovery mode, quarantine uncertain Slack actions, validate encryption key and account bindings, use audited decisions, and only then clear the gate and enable outbound execution. Do not restore over the primary local developer volume or start a worker with the ordinary `.env` before clearance. Verify gate status, unknown count, CRM current-state convergence and run progress. The backup may omit external effects performed after its boundary.

## Encryption key rotation and recovery

Symptom: credentials cannot be opened after restore or key version is missing. Confirm only the key version and authorized key source, never the key bytes in logs. Recover the same master key from the separate approved secret store before starting outbound execution; use the existing connection flow to replace credentials if key recovery is impossible. Do not put the key in a database dump, Git, shell history or a shared report. Verify a controlled read-only connection identity check and one test-mode run. Pilot in-place bulk re-encryption/zero-downtime rotation is not implemented; a production KMS rotation procedure remains a release gate.

## Tenant deletion and data retention

Symptom: a workspace is scheduled for deletion or a privacy retention request is received. Confirm tenant identity and all unfinished/unknown actions, then prevent new inbound work and pause automation before removing credentials. The retention batch enforces 7/30/90-day minima and preserves effect identity records. Do not erase the only durable obligation or shared provider evidence while an external result is uncertain. Verify work is terminal, raw payloads are redacted, and required dedup/effect tombstones remain. Full tenant erasure with legal holds and cross-provider deletion is not yet implemented and needs a separate policy decision.
