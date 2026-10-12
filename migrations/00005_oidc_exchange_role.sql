-- +goose Up
-- Only the OIDC exchange component may create users and rotate sessions.
-- The webhook/worker credential must not be able to mint browser identities.
GRANT USAGE ON SCHEMA public TO relaydesk_auth;
GRANT EXECUTE ON FUNCTION oidc_begin(bytea,bytea,bytea,bytea,timestamptz),
    oidc_consume(bytea),oidc_user(text,text,text),
    session_rotate(bytea,bytea,uuid,timestamptz) TO relaydesk_auth;
REVOKE EXECUTE ON FUNCTION oidc_begin(bytea,bytea,bytea,bytea,timestamptz),
    oidc_consume(bytea),oidc_user(text,text,text),
    session_rotate(bytea,bytea,uuid,timestamptz) FROM relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'OIDC exchange role migration is intentionally irreversible';
END $$;
-- +goose StatementEnd
