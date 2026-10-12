-- +goose Up
-- A newly accepted event may share only a CRM action that has not started.
-- Keep this lookup bounded for hot Customers without changing the durable
-- per-event run or the independent per-invoice Slack effect identity.
CREATE INDEX business_actions_pending_crm_aggregate ON business_actions
 (tenant_id,automation_revision_id,stripe_connection_id,customer_id,created_at,id)
 WHERE kind='crm_projection' AND status IN ('pending','queued');

-- +goose Down
DROP INDEX business_actions_pending_crm_aggregate;
