-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION tenant_members_page(p_session_hash bytea, p_tenant uuid,
                                    p_after_user uuid, p_limit integer)
RETURNS TABLE(user_id uuid, role text, status text)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT m.user_id, m.role, m.status FROM public.memberships m
    WHERE m.tenant_id = p_tenant AND m.status = 'active'
      AND (p_after_user IS NULL OR m.user_id > p_after_user)
      AND public.tenant_authorize(p_session_hash, p_tenant, 'members_read') = 'owner'
    ORDER BY m.user_id
    LIMIT LEAST(GREATEST(p_limit, 1), 101)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tenant_members_page(bytea,uuid,uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_members_page(bytea,uuid,uuid,integer) TO relaydesk_app;
REVOKE EXECUTE ON FUNCTION tenant_members(bytea,uuid) FROM relaydesk_app;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'member pagination migration is intentionally irreversible';
END $$;
-- +goose StatementEnd
