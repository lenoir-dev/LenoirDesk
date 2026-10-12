-- +goose Up
-- Browser queries use a separate login. Its table grants are read-only and its
-- RLS predicates require both transaction-local tenant context and a live
-- session whose user is an active member of that tenant.
GRANT USAGE ON SCHEMA public TO relaydesk_app;
GRANT SELECT ON tenants, runs TO relaydesk_app;
GRANT SELECT (id,tenant_id,provider,provider_account_id,mode,status,health_status,
              generation,created_at) ON connections TO relaydesk_app;
GRANT EXECUTE ON FUNCTION session_current(bytea),session_revoke(bytea),
    auth_session_user(bytea),tenant_authorize(bytea,uuid,text),
    workspace_create(bytea,text,text),membership_change(bytea,uuid,uuid,text,text,text),
    my_workspaces(bytea,integer,timestamptz,uuid),tenant_members(bytea,uuid)
    TO relaydesk_app;

-- +goose StatementBegin
DO $$
DECLARE tab text; predicate text;
BEGIN
    FOREACH tab IN ARRAY ARRAY['tenants','connections','runs'] LOOP
        IF tab = 'tenants' THEN
            predicate := 'id';
        ELSE
            predicate := 'tenant_id';
        END IF;
        EXECUTE format($policy$
            CREATE POLICY browser_member ON %I TO relaydesk_app USING (
                %I = nullif(current_setting('app.tenant_id',true),'')::uuid
                AND public.tenant_authorize(
                    decode(coalesce(current_setting('app.session_hash',true),''),'hex'),
                    %I, 'read') IS NOT NULL)
        $policy$, tab, predicate, predicate);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'browser role migration is intentionally irreversible';
END $$;
-- +goose StatementEnd
