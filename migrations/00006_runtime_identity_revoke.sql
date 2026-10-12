-- +goose Up
-- Existing Mission 01 runtime credentials must be limited to webhook/worker
-- operations after browser and identity logins are provisioned.
REVOKE EXECUTE ON FUNCTION session_current(bytea),session_revoke(bytea),
    auth_session_user(bytea),tenant_authorize(bytea,uuid,text),
    workspace_create(bytea,text,text),
    membership_change(bytea,uuid,uuid,text,text,text),
    my_workspaces(bytea,integer,timestamptz,uuid),tenant_members(bytea,uuid)
    FROM relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'runtime identity privilege revocation is intentionally irreversible';
END $$;
-- +goose StatementEnd
