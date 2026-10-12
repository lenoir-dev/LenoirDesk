-- +goose Up
GRANT DELETE ON crm_aggregate_leases TO relaydesk_runtime;
-- +goose Down
REVOKE DELETE ON crm_aggregate_leases FROM relaydesk_runtime;
