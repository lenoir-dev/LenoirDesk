# Business schema

Goose migrations 00007–00021 preserve existing inbox, outbox, users, connections and runs. Older `legacy_planning` revisions still plan through the compatibility path. The migration role owns the schema; runtime and browser roles receive narrow grants and tenant RLS, and neither owns tables nor has `BYPASSRLS`.

| Migration | Change |
| --- | --- |
| 00007 | Template and environment columns; `automation_gates`, `business_actions`, run/action links, attempts, Contact links and CRM aggregate leases; immutable revision trigger and owner/operator definer functions. |
| 00008 | Atomic global claim function that creates a fenced attempt before provider calls; expired started Slack sends become `unknown`. |
| 00009 | Browser access to selected event/job metadata for run detail; raw webhook payload remains inaccessible. |
| 00010 | Audited explicit Contact link through a tenant-authorized function. |
| 00011 | Claim-function conflict-target correction compatible with PL/pgSQL output parameters. |
| 00012 | Runtime lease-release grant. |
| 00013 | Per-provider HTTP call records with request-start and response outcome. |
| 00014 | Pause/resume status propagation into runs. |
| 00015 | Extended action states, recovery metadata, canonical PostgreSQL decision audit and action outbox. |
| 00016 | Due-action scheduler, action outbox claims, generation-fenced pointer claim and repair. |
| 00017 | Operator idempotency recheck under action lock; first-attempt clock for the legacy test claimant. |
| 00018 | Provider connection cooldown for `429` and checks at schedule/claim time. |
| 00019 | Explicit pause/resume of queued and waiting actions; old pointers superseded. |
| 00020 | Aggregate-only recovery health counters available to the runtime role. |
| 00021 | Session/tenant-guarded browser reads of safe provider-call history. |

`business_actions` has a unique `(tenant_id,effect_key)` constraint. CRM effects are event/revision-scoped current-state reconciliation actions; Slack effects are invoice/account/mode/destination-scoped and deduplicate across distinct event IDs. `business_attempts` has unique `(tenant_id,action_id,attempt_number)`; each claim increments generation and supplies a lease token. `business_provider_calls` is inserted before every HTTP request and records only a safe method/path/status/request reference, never authorization headers, tokens or payload bodies. All action, link, attempt and call rows carry tenant ID and foreign-key constraints. The CRM aggregate lease key is tenant + Stripe account + mode + customer.

`contact_links` binds a stable `stripe:{account}:{mode}:{customer}` key to one HubSpot Contact in a portal. The unique `(tenant_id,hubspot_connection_id,contact_id)` constraint prevents two local customer identities from silently sharing the same Contact. Application code also reads HubSpot managed fields before writing to detect identity conflicts.

The browser role can read action and run metadata only through a current session and tenant membership. It cannot select raw webhook payload, credential ciphertext or session rows. The runtime role can execute the credential lookup function for an already selected tenant/connection but has no direct credential-table grant. Definer functions validate active membership for configuration, pause and explicit-link mutations and write audit rows.

State recovery is PostgreSQL-driven: the planner ACKs its JetStream pointer after the run/actions commit; `queue_business_actions` atomically creates a new `dispatch_generation` and `business_action_outbox` row for a due action. The executor claims only the matching queued generation and ACKs after a durable result. A lost pointer is superseded and republished after the watchdog threshold. Expired CRM attempts reread current provider state; expired Slack attempts with a started send stop as `unknown`. `dead` action rows are the canonical DLQ. `business_action_decisions` records actor, reason, old/new generation, optional idempotency key, and any explicit duplicate-risk acknowledgment. A lost PostgreSQL database or encryption key is outside these guarantees.
