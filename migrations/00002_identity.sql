-- +goose Up
-- Existing users have no verified issuer. They cannot be adopted by matching email or subject.
ALTER TABLE users ADD COLUMN oidc_issuer text;
UPDATE users SET oidc_issuer = 'urn:relaydesk:legacy-unverified';
ALTER TABLE users ALTER COLUMN oidc_issuer SET NOT NULL;
ALTER TABLE users ADD COLUMN email text;
ALTER TABLE users DROP CONSTRAINT users_oidc_subject_key;
ALTER TABLE users ADD CONSTRAINT users_oidc_identity_key UNIQUE (oidc_issuer, oidc_subject);

ALTER TABLE memberships ADD COLUMN status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'removed'));

CREATE TABLE oidc_flows (
    state_hash bytea PRIMARY KEY CHECK (length(state_hash) = 32),
    verifier_ciphertext bytea NOT NULL,
    verifier_nonce bytea NOT NULL CHECK (length(verifier_nonce) = 12),
    nonce_hash bytea NOT NULL CHECK (length(nonce_hash) = 32),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oidc_flows_expiry ON oidc_flows (expires_at);

CREATE TABLE user_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    token_hash bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX user_sessions_user ON user_sessions (user_id, created_at DESC);
CREATE INDEX user_sessions_expiry ON user_sessions (expires_at);

CREATE TABLE audit_log (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    actor_user_id uuid NOT NULL REFERENCES users(id),
    operation text NOT NULL,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    result text NOT NULL CHECK (result IN ('success', 'denied')),
    correlation_id text NOT NULL,
    context jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);
CREATE INDEX audit_log_history ON audit_log (tenant_id, created_at DESC, id DESC);
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY runtime_tenant ON audit_log TO relaydesk_runtime
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY migration_admin ON audit_log TO relaydesk_migrate USING (true) WITH CHECK (true);
GRANT SELECT ON audit_log TO relaydesk_runtime;

-- Credentials are separate from browser-visible connection metadata.
CREATE TABLE connection_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    connection_id uuid NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    secret_kind text NOT NULL CHECK (secret_kind IN ('webhook_signing')),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (length(nonce) = 12),
    key_version integer NOT NULL CHECK (key_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, connection_id, generation, secret_kind),
    FOREIGN KEY (tenant_id, connection_id) REFERENCES connections(tenant_id, id)
);
ALTER TABLE connection_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE connection_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY runtime_tenant ON connection_credentials TO relaydesk_runtime
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY migration_admin ON connection_credentials TO relaydesk_migrate USING (true) WITH CHECK (true);
-- Deliberately no direct runtime grant on connection_credentials.

INSERT INTO connection_credentials
    (tenant_id, connection_id, generation, secret_kind, ciphertext, nonce, key_version)
SELECT tenant_id, id, generation, 'webhook_signing',
       signing_secret_ciphertext, signing_secret_nonce, key_version
FROM connections WHERE provider = 'stripe';
DROP FUNCTION lookup_stripe_connection(bytea);
ALTER TABLE connections DROP CONSTRAINT connections_check;
ALTER TABLE connections DROP COLUMN signing_secret_ciphertext;
ALTER TABLE connections DROP COLUMN signing_secret_nonce;
ALTER TABLE connections DROP COLUMN key_version;
ALTER TABLE connections ADD CONSTRAINT connections_stripe_reference_check
    CHECK ((provider = 'stripe' AND webhook_ref_hash IS NOT NULL)
        OR (provider <> 'stripe' AND webhook_ref_hash IS NULL));
ALTER TABLE connections ADD COLUMN health_status text NOT NULL DEFAULT 'unverified'
    CHECK (health_status IN ('unverified', 'verified', 'degraded', 'invalid'));
ALTER TABLE connections ADD COLUMN last_checked_at timestamptz;
ALTER TABLE connections ADD COLUMN verified_at timestamptz;
GRANT SELECT (health_status, last_checked_at, verified_at) ON connections TO relaydesk_runtime;
ALTER TABLE connections DROP CONSTRAINT connections_status_check;
ALTER TABLE connections ADD CONSTRAINT connections_status_check
    CHECK (status IN ('pending_validation', 'active', 'disconnected'));
ALTER TABLE connections ALTER COLUMN status SET DEFAULT 'pending_validation';
ALTER TABLE webhook_events ADD COLUMN connection_generation bigint NOT NULL DEFAULT 1
    CHECK (connection_generation > 0);

-- +goose StatementBegin
CREATE FUNCTION lookup_stripe_connection(p_ref_hash bytea)
RETURNS TABLE(tenant_id uuid, connection_id uuid, account_id text, mode text,
              ciphertext bytea, nonce bytea, key_version integer, generation bigint)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT c.tenant_id, c.id, c.provider_account_id, c.mode,
           cc.ciphertext, cc.nonce, cc.key_version, c.generation
    FROM public.connections c
    JOIN public.connection_credentials cc
      ON cc.tenant_id = c.tenant_id AND cc.connection_id = c.id
     AND cc.generation = c.generation AND cc.secret_kind = 'webhook_signing'
     AND cc.revoked_at IS NULL
    WHERE c.provider = 'stripe' AND c.status = 'active'
      AND c.webhook_ref_hash = p_ref_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION lookup_stripe_connection(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lookup_stripe_connection(bytea) TO relaydesk_runtime;

-- No authenticated browser operation needs CREATE on public.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION oidc_begin(p_state_hash bytea, p_ciphertext bytea, p_nonce bytea,
                           p_nonce_hash bytea, p_expires_at timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    IF length(p_state_hash) <> 32 OR length(p_nonce_hash) <> 32 OR length(p_nonce) <> 12
       OR p_expires_at <= now() OR p_expires_at > now() + interval '10 minutes' THEN
        RAISE EXCEPTION 'invalid OIDC flow';
    END IF;
    INSERT INTO public.oidc_flows(state_hash, verifier_ciphertext, verifier_nonce,
                                  nonce_hash, expires_at)
    VALUES (p_state_hash, p_ciphertext, p_nonce, p_nonce_hash, p_expires_at);
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION oidc_consume(p_state_hash bytea)
RETURNS TABLE(verifier_ciphertext bytea, verifier_nonce bytea, nonce_hash bytea)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    DELETE FROM public.oidc_flows WHERE state_hash = p_state_hash AND expires_at > now()
    RETURNING oidc_flows.verifier_ciphertext, oidc_flows.verifier_nonce, oidc_flows.nonce_hash
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION oidc_user(p_issuer text, p_subject text, p_email text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_id uuid;
BEGIN
    IF length(p_issuer) < 8 OR length(p_issuer) > 512 OR length(p_subject) < 1
       OR length(p_subject) > 512 OR length(p_email) > 320 THEN
        RAISE EXCEPTION 'invalid OIDC identity';
    END IF;
    INSERT INTO public.users (oidc_issuer, oidc_subject, email)
    VALUES (p_issuer, p_subject, nullif(p_email, ''))
    ON CONFLICT (oidc_issuer, oidc_subject) DO UPDATE SET email = EXCLUDED.email
    RETURNING id INTO v_id;
    RETURN v_id;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION session_rotate(p_old_hash bytea, p_new_hash bytea, p_user_id uuid,
                               p_expires_at timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    IF length(p_new_hash) <> 32 OR p_expires_at <= now()
       OR p_expires_at > now() + interval '12 hours' THEN
        RAISE EXCEPTION 'invalid session';
    END IF;
    IF p_old_hash IS NOT NULL THEN
        UPDATE public.user_sessions SET revoked_at = now()
        WHERE token_hash = p_old_hash AND revoked_at IS NULL;
    END IF;
    INSERT INTO public.user_sessions(user_id, token_hash, expires_at)
    VALUES (p_user_id, p_new_hash, p_expires_at);
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION session_current(p_hash bytea)
RETURNS TABLE(user_id uuid, email text, oidc_issuer text, expires_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    WITH live AS (
        UPDATE public.user_sessions s SET last_seen_at = now()
        WHERE s.token_hash = p_hash AND s.revoked_at IS NULL
          AND s.expires_at > now() AND s.last_seen_at > now() - interval '30 minutes'
        RETURNING s.user_id, s.expires_at
    )
    SELECT u.id, u.email, u.oidc_issuer, live.expires_at
    FROM live JOIN public.users u ON u.id = live.user_id
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION session_revoke(p_hash bytea)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    UPDATE public.user_sessions SET revoked_at = now()
    WHERE token_hash = p_hash AND revoked_at IS NULL
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION auth_session_user(p_hash bytea)
RETURNS uuid LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT s.user_id FROM public.user_sessions s
    WHERE s.token_hash = p_hash AND s.revoked_at IS NULL
      AND s.expires_at > now() AND s.last_seen_at > now() - interval '30 minutes'
    FOR SHARE OF s
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION tenant_authorize(p_session_hash bytea, p_tenant uuid, p_permission text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_user uuid; v_role text;
BEGIN
    v_user := public.auth_session_user(p_session_hash);
    IF v_user IS NULL THEN RETURN NULL; END IF;
    SELECT m.role INTO v_role FROM public.memberships m
    WHERE m.tenant_id = p_tenant AND m.user_id = v_user AND m.status = 'active'
    FOR SHARE OF m;
    IF v_role IS NULL THEN RETURN NULL; END IF;
    IF p_permission = 'read' OR
       (p_permission = 'operate' AND v_role IN ('owner','operator')) OR
       (p_permission IN ('members_read','members_write') AND v_role = 'owner') THEN
        RETURN v_role;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION workspace_create(p_session_hash bytea, p_name text, p_correlation_id text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_user uuid; v_tenant uuid;
BEGIN
    v_user := public.auth_session_user(p_session_hash);
    IF v_user IS NULL OR length(btrim(p_name)) < 1 OR length(p_name) > 120
       OR length(p_correlation_id) > 128 THEN
        RAISE EXCEPTION 'workspace creation denied';
    END IF;
    INSERT INTO public.tenants(name) VALUES (btrim(p_name)) RETURNING id INTO v_tenant;
    INSERT INTO public.memberships(tenant_id, user_id, role, status)
    VALUES (v_tenant, v_user, 'owner', 'active');
    INSERT INTO public.audit_log(tenant_id, actor_user_id, operation, target_type,
                                 target_id, result, correlation_id)
    VALUES (v_tenant, v_user, 'workspace.create', 'workspace', v_tenant, 'success', p_correlation_id);
    RETURN v_tenant;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION membership_change(p_session_hash bytea, p_tenant uuid, p_target_user uuid,
                                  p_action text, p_role text, p_correlation_id text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_actor uuid; v_old_role text; v_old_status text; v_count integer;
BEGIN
    v_actor := public.auth_session_user(p_session_hash);
    IF v_actor IS NULL OR length(p_correlation_id) > 128 THEN
        RAISE EXCEPTION 'membership change denied';
    END IF;
    PERFORM 1 FROM public.tenants WHERE id = p_tenant FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'workspace not found'; END IF;
    PERFORM 1 FROM public.memberships
    WHERE tenant_id = p_tenant AND user_id = v_actor AND status = 'active' AND role = 'owner';
    IF NOT FOUND THEN RAISE EXCEPTION 'membership change denied'; END IF;
    IF p_action NOT IN ('grant','role','remove') OR
       (p_action <> 'remove' AND p_role NOT IN ('owner','operator','viewer')) THEN
        RAISE EXCEPTION 'invalid membership action';
    END IF;
    SELECT role, status INTO v_old_role, v_old_status FROM public.memberships
    WHERE tenant_id = p_tenant AND user_id = p_target_user;
    IF p_action = 'grant' THEN
        INSERT INTO public.memberships(tenant_id, user_id, role, status)
        VALUES (p_tenant, p_target_user, p_role, 'active')
        ON CONFLICT (tenant_id, user_id) DO UPDATE
          SET role = EXCLUDED.role, status = 'active';
    ELSE
        IF v_old_role IS NULL OR v_old_status <> 'active' THEN
            RAISE EXCEPTION 'membership not found';
        END IF;
        IF v_old_role = 'owner' AND (p_action = 'remove' OR p_role <> 'owner') THEN
            SELECT count(*) INTO v_count FROM public.memberships
            WHERE tenant_id = p_tenant AND role = 'owner' AND status = 'active';
            IF v_count <= 1 THEN RAISE EXCEPTION 'last owner cannot be removed'; END IF;
        END IF;
        IF p_action = 'remove' THEN
            UPDATE public.memberships SET status = 'removed'
            WHERE tenant_id = p_tenant AND user_id = p_target_user;
        ELSE
            UPDATE public.memberships SET role = p_role
            WHERE tenant_id = p_tenant AND user_id = p_target_user;
        END IF;
    END IF;
    INSERT INTO public.audit_log(tenant_id, actor_user_id, operation, target_type,
                                 target_id, result, correlation_id,
                                 context)
    VALUES (p_tenant, v_actor, 'membership.' || p_action, 'user', p_target_user,
            'success', p_correlation_id, jsonb_build_object('role', CASE WHEN p_action='remove' THEN v_old_role ELSE p_role END));
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION my_workspaces(p_session_hash bytea, p_limit integer, p_before_at timestamptz, p_before_id uuid)
RETURNS TABLE(id uuid, name text, role text, created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT t.id, t.name, m.role, t.created_at
    FROM public.tenants t JOIN public.memberships m ON m.tenant_id = t.id
    WHERE m.user_id = public.auth_session_user(p_session_hash) AND m.status = 'active'
      AND (p_before_at IS NULL OR (t.created_at, t.id) < (p_before_at, p_before_id))
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT LEAST(GREATEST(p_limit, 1), 101)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION tenant_members(p_session_hash bytea, p_tenant uuid)
RETURNS TABLE(user_id uuid, role text, status text)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT m.user_id, m.role, m.status FROM public.memberships m
    WHERE m.tenant_id = p_tenant AND m.status = 'active'
      AND public.tenant_authorize(p_session_hash, p_tenant, 'members_read') = 'owner'
    ORDER BY m.user_id
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION oidc_begin(bytea,bytea,bytea,bytea,timestamptz),
    oidc_consume(bytea), oidc_user(text,text,text),
    session_rotate(bytea,bytea,uuid,timestamptz), session_current(bytea),
    session_revoke(bytea), auth_session_user(bytea), tenant_authorize(bytea,uuid,text),
    workspace_create(bytea,text,text),
    membership_change(bytea,uuid,uuid,text,text,text),
    my_workspaces(bytea,integer,timestamptz,uuid), tenant_members(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION oidc_begin(bytea,bytea,bytea,bytea,timestamptz),
    oidc_consume(bytea), oidc_user(text,text,text),
    session_rotate(bytea,bytea,uuid,timestamptz), session_current(bytea),
    session_revoke(bytea), auth_session_user(bytea), tenant_authorize(bytea,uuid,text),
    workspace_create(bytea,text,text),
    membership_change(bytea,uuid,uuid,text,text,text),
    my_workspaces(bytea,integer,timestamptz,uuid), tenant_members(bytea,uuid) TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'identity migration is intentionally irreversible: rollback would discard sessions, audit, or credential generations';
END $$;
-- +goose StatementEnd
