# LenoirDesk local design demo

This is a **synthetic, disposable** workspace for visual and workflow review. It does not use Stripe, HubSpot or Slack accounts, send payments or read Stage 0 credentials.

LenoirDesk follows a billing event from Stripe through the corresponding HubSpot Contact change and, when required, a Slack notification. The interface shows which step succeeded, what is uncertain and which recovery action an operator may safely take. This local demo illustrates the workflow; it does not prove production-provider readiness.

## Start and explore

Prerequisites: Docker Desktop with Compose, Go and Python 3. From the repository root:

```sh
make demo-local
```

The command creates a dedicated `lenoirdesk_m06_demo` Compose project with PostgreSQL, NATS JetStream and Redis on loopback ports `59432`, `59422` and `59679`. It creates random synthetic database passwords and an encryption key under ignored `tmp/lenoirdesk-demo/` with mode `0700`/`0600`, applies migrations and starts the actual API, delivery worker, execution worker and local provider simulators in an integration-test process. No root `.env`, `.env.stage0.secrets`, normal development volumes or real provider credentials are read. The browser URL and a separate local simulation-control URL are printed when seeding finishes; keep the terminal open.

Open the first URL and choose **Continue to sign in**. An in-process test OIDC issuer grants a synthetic owner identity through the normal state, PKCE, nonce, session and CSRF paths. It has no password and is intentionally not the production or local Dex login. Enter **LenoirDesk Demo — Northstar Studio (synthetic)**.

Suggested tour:

1. Overview: inspect operational KPIs, the chart and recent persisted events. Try EN/RU and Dark/Light/System; preferences survive navigation and reload.
2. Connections: inspect three verified **simulation** accounts and their identity/health labels. Do not attempt OAuth reinstallation from these synthetic accounts.
3. Automation: inspect active revision #2 and historical inactive revision #1.
4. Runs: filter succeeded, pending execution, stopped retries (DLQ) and unknown, open Run Details, review the signed test webhook, CRM no-write path, Slack message and the operator-confirmed Slack decision. Another unknown Slack outcome remains for review. Use only the documented synthetic channel if resolving it; its simulated delivery does not reach a real workspace.
5. Reconciliation: inspect a persisted known-state checkpoint, an event-coverage gap and a completed synthetic historical request. Those historical records are fixture inserts; this demo does not run a real Stripe historical scan.
6. In the second control tab, first complete the pending billing event. Then trigger **temporary CRM outage**. Inspect `retry_wait` and the attempt timeline. Restore the local CRM; the normal automatic retry succeeds. Next, trigger Contact identity conflict. Inspect `needs_attention`, restore the local Contact identity, and retry the eligible CRM action in Run Details with a reason. Finish in the control tab after success.

The demo uses ordinary persisted read models and real recovery endpoints. Signed webhooks, planner, execution and operator decisions run through the application. The historical checkpoint/request rows are intentionally inserted in the isolated database because no real provider history exists. The control tab only changes local provider simulator behavior; it does not call a fake product mutation endpoint.

The initial state includes successful runs, a pending CRM action, an exhausted retry (DLQ), a CRM no-write result, an unknown Slack result, an operator-confirmed Slack result, a coverage gap and a historical request. The DLQ example is produced by an actual local provider outage and a deliberately tiny **test-only** provider-call budget; no row is forged into the dead state. During the guided scenario, the second outage becomes a scheduled retry and the Contact conflict becomes a run needing attention. The chart counts persisted runs and is not a revenue chart.

## Stop or reset

Stop the foreground Go process with Ctrl-C. Containers remain for inspection.

```sh
make demo-down   # stop only the dedicated demo containers; keep volumes
make demo-reset  # remove only dedicated demo containers, volumes and synthetic config
make demo-local  # start a fresh scenario after reset
make demo-check  # run the full synthetic scenario automatically, then clean up on PASS
```

`demo-reset` checks the fixed project name and local identity marker before deleting anything. It cannot target the ordinary `relaydesk` Compose project. If startup fails, inspect Docker's error, run `make demo-reset`, then retry. No command here should be used as evidence of live provider readiness.

## Localization and accessibility limits

English and Russian are available in the application; operational timestamps are shown in UTC with locale-specific date order. The pinned Dex password page displays English and Russian together through an official template override. Dex does not receive the application language as a template input, so it cannot mirror a selected locale. The synthetic demo bypasses Dex by design; use `make oidc-smoke` for the real local Dex journey.

Terminology used in the UI: **workspace / рабочая область**, **run / запуск**, **needs attention / требует внимания**, **waiting to retry / ожидает повтора**, **unknown outcome / результат неизвестен**, **coverage gap / пробел в покрытии**, and **reconciliation / сверка**. Status labels describe persisted states; they do not promise a provider action that has not been confirmed.

Keep screenshots in ignored `tmp/` only, and review them for test IDs or incidental browser content before sharing. This demo is for owner acceptance, not a public release or production pilot.
