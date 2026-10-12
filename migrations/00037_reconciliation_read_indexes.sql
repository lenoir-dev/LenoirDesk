-- +goose Up
-- Known-state scans advance by customer key within a single tenant/Stripe
-- connection. Preserve that order without a full action-table sort.
CREATE INDEX business_actions_known_customer
 ON business_actions(tenant_id,stripe_connection_id,customer_id);

-- Due claims must also find expired leases, not only rows with a NULL token.
DROP INDEX reconcile_due;
CREATE INDEX reconcile_due ON reconcile_checkpoints(next_run_at,updated_at) INCLUDE (lease_until);
DROP INDEX reconcile_requests_due;
CREATE INDEX reconcile_requests_due ON reconcile_requests(next_run_at,created_at) INCLUDE (lease_until)
 WHERE status='pending';

CREATE INDEX business_actions_operational_state ON business_actions(status,updated_at);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'reconciliation index migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
