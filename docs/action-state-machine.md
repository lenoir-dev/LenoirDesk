# Business action state machine

The authoritative application transition table is `internal/business/action_state.go`; `TestActionTransitionTable` checks every state/reason pair. PostgreSQL predicates, row locks, lease constraints and `decide_business_action()` enforce the concurrent boundary. The immutable `effect_key` identifies one external effect throughout retries and resolutions.

| Operation | Source → target | Guard and durable changes | External request | New dispatch generation | Dependent actions | Audit |
| --- | --- | --- | --- | --- | --- | --- |
| Plan | new → `pending` | Fixed revision, effect key and dependency committed with run | No | No | Wait | Webhook/planning history |
| Schedule | `pending`/due `retry_wait` → `queued` | Pause, dependency and cooldown checks; action outbox row | No | Yes | Wait | Outbox row |
| Claim | `queued` → `running` | Exact pointer generation; connection, pause, cooldown and aggregate lease; attempt and fence persisted | After commit only | No | Wait | Attempt row |
| Finish | `running` → `succeeded`, `retry_wait`, `blocked_auth`, `blocked_config`, `paused`, `unknown`, `needs_attention`, `dead` | Token/generation CAS; attempt outcome, safe error, next due and run summary committed | The bounded request already occurred, if any | No; scheduler increments on later retry | Eligible only after `succeeded` | Attempt and provider-call rows |
| Pause | `pending`/`queued`/`retry_wait` → `paused` | Supersede queued pointer, keep revision and due | No new write | On resume and schedule | Wait | Actor audit |
| Resume | `paused` → `pending`/`retry_wait` | Retain not-yet-due retry when applicable | No | On schedule | Wait | Actor audit |
| Manual retry | `dead`/`needs_attention`/`blocked_auth`/`blocked_config` → `pending` | Owner/operator, reason, expected generation, row lock, optional idempotency key; reset recovery budget while retaining attempts/effect/revision | Only after later claim | On schedule | Wait | Decision and actor audit |
| Confirm unknown | `unknown` → `succeeded` | Slack only; original channel, valid timestamp, reason and actor; marked `operator` confirmed | No | No | Eligible | Decision and actor audit |
| Leave unknown | `unknown` → `unknown` | Record reason and advance CAS generation | No | No | Wait | Decision and actor audit |
| Risk retry | `unknown` → `pending` | Slack only; explicit duplicate-risk acknowledgment and reason; preserve effect/revision | Only after later claim; duplicate is possible | On schedule | Wait | Decision and actor audit |
| Cancel/supersede | `pending`/`queued`/`retry_wait`/`paused` → `canceled`/`superseded` | Defined table transition; no public operator endpoint | No | No | No | Future implementation must audit |

No generic retry is available for `running`, `succeeded` or `unknown`. Unknown does not expire into success. A run succeeds only if *all* linked actions are `succeeded`; a successful CRM step with unresolved Slack stays non-successful. `confirmation_source='operator'` on an unknown resolution is visible in history and is not an automated read of Slack.

An expired lease is a local claim failure, not proof that an external request failed. A recorded, unresolved Slack send becomes `unknown`; a CRM action rereads current Stripe and HubSpot state before another write. Fencing protects PostgreSQL commits, while provider-side effects can still arrive late. The watchdog never creates a second effect key.
