# Production rollout boundary

RelayDesk's verified provider identity and delivery contracts apply to the named **test** accounts only. `RELAYDESK_PROVIDER_MODE=test` is prohibited when `RELAYDESK_ENV=production`; there is no supported live-provider execution mode. This document is a gate list, not an instruction to enable production by changing an environment variable.

| Gate | Acceptance evidence before real customer data or live provider accounts | Decision owner |
| --- | --- | --- |
| Stripe distribution and authorization | Choose an approved account-authorization model; document minimum account, customer, subscription, invoice and event-read grants; verify live-mode account binding, versioned event parsing and key rotation without unrestricted merchant secrets. | Product + Security |
| HubSpot installation and distribution | Confirm app distribution terms, approved scopes, portal-bound OAuth, Free-tier property contracts, property ownership and uninstall/revocation behavior in a separate production-like tenant. | Product + Integrations |
| Slack distribution | Decide private versus distributed app policy, workspace/channel authorization, bot removal, message retention and duplicate-risk operator procedure. | Product + Security |
| Encryption and secrets | Replace the local master key with KMS envelope encryption, versioned online rotation, audited break-glass recovery, centralized secret injection and revocation drills. | Security + Operations |
| Public transport | Managed TLS for public API/webhook ingress, origin/redirect enforcement, network egress allowlists, verified DB/NATS/Redis TLS and WAF/body-size controls; test proxy forwarded-header trust. | Platform + Security |
| Backups and recovery | Select off-host encrypted storage, backup schedule and retention; keep age/KMS identity separate; automate restore verification, external witness/fencing, provider-side uncertainty review and measured RPO/RTO at expected data volume. | Operations |
| Tenant privacy | Approved processing terms, data residency, retention schedule, export and deletion workflow for contacts, payloads, logs, metrics and backups; legal holds and evidence of erasure. | Legal/Privacy + Product |
| Availability and capacity | Select HA topology for PostgreSQL, NATS and Redis, isolated worker deployments, distributed recovery authority, quota budgets and load/chaos results meeting agreed SLOs. | Platform + Product |
| Customer operations | Define on-call/alert delivery, runbook ownership, support access/RBAC, incident communication, error budget, audit retention and regular provider contract checks. | Operations + Support |
| Deployment and rollback | Reproducible image provenance, signed artifacts, hosted CI, staging promotion, reversible application rollout, migration compatibility, canary, rollback rehearsals and explicit production approval. | Platform + Security |

Provisional targets in the architecture (webhook p95 below 500 ms, healthy-provider CRM completion 99% within 60 seconds, RPO at most 15 minutes and RTO at most 60 minutes) remain **targets**, not service commitments. The current local load and restore drills cannot establish them for production. No production deployment or live-mode onboarding is implied by this repository.
