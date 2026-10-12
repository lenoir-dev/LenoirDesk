-- +goose Up
-- Stable order for bounded tenant-scoped audit history queries.
GRANT SELECT (id) ON audit_log TO relaydesk_app;
-- +goose Down
REVOKE SELECT (id) ON audit_log FROM relaydesk_app;
