# ADR 0002: Durable external effects and ambiguous outcomes

Status: Accepted. The current implementation includes durable ingress, planning and the external-effect state machine. Date: 2026-10-10.

## Context

PostgreSQL commit, JetStream ACK, HubSpot write, and Slack send cannot share one atomic transaction. Broker redelivery does not establish exactly-once behavior in an external API.

## Decision

PostgreSQL owns the obligation and result. Ingress saves inbox + planning job + outbox in one transaction before acknowledging Stripe. The outbox may publish a pointer more than once; planner validates generation and creates one run. Later executor actions must persist an attempt before an external call, confirm the result before success, and ACK broker delivery after durable state transition.

HubSpot state updates use stable identity and read/compare recovery after an ambiguous response. Slack notification uses an invoice/destination effect key. If a send may have succeeded but no `channel`/`ts` was stored, mark it `unknown`; require verification rather than blind automatic replay.

## Consequences

The first slice demonstrates durable acceptance and idempotent planning. It does not claim CRM/Slack delivery, automatic business retries, or end-to-end exactly-once. PostgreSQL restore can resurrect uncertainty about already sent effects and must quarantine them before replay.

Evidence: [Stripe delivery](https://docs.stripe.com/webhooks#event-delivery-behaviors), [NATS JetStream](https://docs.nats.io/concepts/jetstream), [Slack post response](https://docs.slack.dev/reference/methods/chat.postMessage/).
