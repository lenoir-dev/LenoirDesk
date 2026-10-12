# ADR 0003: Invoice notifications and reconciliation evidence

Status: Accepted for the first release candidate by the product owner on 2026-10-10.

## Decision

The first durable acceptance of a Stripe event fixes its source, automation revision and planning obligation. A supported, signed, in-window `invoice.paid` webhook can create one Slack action after CRM confirmation. Event discovery, bounded historical backfill and known-customer state repair never authorize a Slack send. A signed delivery arriving after discovery of the same event is a duplicate and does not change the persisted source or create a second run/action. An invoice older than 90 days is historical even if its first delivery is signed. An already processed invoice fact keeps its original invoice/destination effect key and result. A previously suppressed notification stays suppressed.

This is conservative because Stripe Events API listing proves an event exists, but its timing relative to a possibly delayed signed webhook does not establish an operator-approved notification window. It avoids historical floods and accidental replay after restore. It also means a sufficiently recent paid invoice found by discovery first may never notify Slack. The Reconciliation screen and owner guide disclose this business tradeoff. The original promise of one notification for a paid invoice is therefore **not fully met** in this race. The product owner explicitly accepted this exception for the first release candidate; operators must treat it as a coverage gap, not a sent notification.

## Future alternatives requiring a separate decision

1. Permit a recent discovered invoice to notify immediately. This needs an explicit recency bound and operational approval for delayed notifications. The existing stable effect key can prevent independent duplicates, but an unknown Slack result still requires operator review.
2. Keep discovery silent, then promote a suppressed invoice only upon a later signed webhook. This requires an atomic promotion protocol under the tenant/invoice effect key, a defined promotion deadline, immutable evidence of signed arrival, and concurrent acceptance tests. A duplicate inbox insert alone cannot safely perform that promotion.

For either change, preserve the existing effect key, do not auto-retry unknown sends, and run migration and race tests before changing the contract. In this release candidate, the conservative behavior is intentional, not evidence of successful notification coverage.
