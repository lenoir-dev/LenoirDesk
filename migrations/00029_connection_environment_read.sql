-- +goose Up
GRANT SELECT (execution_environment) ON connections TO relaydesk_app;
-- +goose Down
REVOKE SELECT (execution_environment) ON connections FROM relaydesk_app;
