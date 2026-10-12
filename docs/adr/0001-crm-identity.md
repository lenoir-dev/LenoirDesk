# ADR 0001: Stable CRM identity

Status: Accepted for design; provider test gate pending. Date: 2026-10-10.

## Context

Stripe Customer IDs and HubSpot Contact IDs serve different systems. Email can change, can belong to an existing Contact, and is not proof that two records represent the same person. Retries after an uncertain HTTP response can create duplicate Contacts if identity is based on a search followed by create.

## Decision

For each tenant and Stripe account/mode, define one stable `relaydesk_customer_key` derived from external account and Customer ID. Provision it as a unique Contact property. Keep an explicit local mapping to HubSpot Contact ID. Existing contacts are linked by deliberate operator action; email is only a candidate for review. With creation enabled, use HubSpot upsert by the stable unique property. On conflict/merge/archive, stop and request mapping resolution.

RelayDesk owns only `relaydesk_*` billing fields. It does not write standard lifecycle stage or unrelated salesperson fields. One tracked subscription per Customer is supported in v1; multiple matching subscriptions require attention.

## Consequences

Retries can use the same external key and reconcile the observed Contact. This does not prove that downstream HubSpot workflows fire only once. Schema creation and custom unique upsert must be validated on an authorized test portal before the connector is implemented.

Evidence: [HubSpot Contacts and upsert](https://developers.hubspot.com/docs/api-reference/legacy/crm/objects/contacts/guide#upsert-contacts).
