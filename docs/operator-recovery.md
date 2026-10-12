# Local recovery runbook

The canonical record is PostgreSQL. JetStream messages are generation-fenced pointers; deleting a broker message is not deleting an action. Run all commands from the repository root. The commands below use local simulators.

## Reproduce the HubSpot outage

```sh
make up
make clock-preflight
RELAYDESK_TEST_ENV_FILE=.env go test -tags integration -count=1 -v -run '^TestMission04QueuedOutageRetryAndRestart$' ./tests/integration
```

The test accepts a signed `invoice.paid` envelope, stores a run and CRM/Slack actions, makes HubSpot return 503, observes `retry_wait` with `next_attempt_at` and Slack still `pending`, restarts the business worker, restores HubSpot, and asserts one Contact write, one Slack message and final `succeeded`. `TestMission04QueuedBusinessAndPointerReplay` verifies action outbox publication and the two committed claims. `TestBusinessPaidInvoice`, `TestBusinessDuplicateDeliveryAndInvoiceDedup` and `TestBusinessOutOfOrderConvergence` cover duplicate event/current-state behavior; run all with `make test-integration` for the regression gate. The simulator creates no external account resources.

## Reproduce Slack uncertainty separately

```sh
RELAYDESK_TEST_ENV_FILE=.env go test -tags integration -count=1 -v -run '^TestMission04UnknownOperatorDecision$|^TestMission04RiskAcknowledgedSlackResend$' ./tests/integration
```

The first test sends one Slack message but loses the response. It confirms `unknown` persists, ordinary retry fails, and an owner can record a channel/timestamp observation without another send. This is `operator` confirmation, not provider verification. The second test explicitly acknowledges duplicate risk and proves that a repeated send can create a second message; its audit row retains the same effect key.

## Operational decisions

| Observed state | Action |
| --- | --- |
| HubSpot 5xx / `retry_wait` | Check `next_attempt_at` and connection cooldown. Let PostgreSQL reschedule; do not click Retry while automatic recovery is pending. CRM rereads current Stripe and HubSpot state. |
| `dead` | Inspect safe error class, first/last failure, attempt timeline and connection health. Repair the cause, then owner/operator supplies a reason through Retry. This reopens the same action/effect. |
| `blocked_auth` | Re-authorize or repair the connection; coordinated OAuth refresh can recover valid rotating grants, but an invalid or revoked grant requires owner intervention. Retry only after the connection is valid. |
| `blocked_config` / `needs_attention` | Correct scope, Contact identity or mapping deliberately. Avoid replaying an old webhook snapshot. Retry the affected action only. |
| Slack `unknown` | Check the exact configured destination manually. If a message is observed, enter its channel and timestamp and mark it operator-confirmed. If uncertain, leave unresolved. Risky retry requires explicit duplicate-risk acknowledgment and can duplicate the message. |
| NATS unavailable | Leave API ingress up while PostgreSQL is healthy. Outbox remains unpublished; restore NATS and watch unpublished age and run status. Do not delete outbox rows. |
| Worker crash / expired lease | Restart worker; watchdog runs about every 30 seconds. A started Slack send with no committed response becomes `unknown`; CRM reruns read/compare. Monitor expired lease and stalled-action counters. |
| Paused automation | Resume through the existing owner/operator pause control after checking the cause. Queued pointers from before pause are superseded; revision and due time are retained. |

The UI filters `/ui/tenants/{tenant}/runs?status=dead`, `?status=unknown` and `?status=needs_attention`. Run details show attempts, safe provider-call status and request IDs, operator decisions, and an unknown-resolution form. The mapping screen reads a proposed Contact and records a reason before binding. Viewer access is read-only. API clients use `POST /v1/tenants/{tenant}/actions/{action}/retry` or `/resolve` with an active session, `X-CSRF-Token`, nonempty reason and `expected_generation`. Repeat submissions should supply the same `Idempotency-Key`; a stale conflicting decision receives HTTP 409. Never put credentials or raw provider bodies in a reason field.

Every 30 seconds the worker logs aggregate-only `business recovery health`: due and unpublished counts/age, active and expired leases, stalled/dead actions, unknown age, 24-hour retry count, repaired count and scan latency. These logs contain no tenant, customer or invoice labels. The provider-call timeline is available only through tenant-authorized history.
