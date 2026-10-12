-- +goose Up
-- A refresh lease is intentionally not stealable after it expires: the old
-- refresh token may already have been consumed remotely. Reconnect is required.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION provider_refresh_claim(p_tenant uuid,p_connection uuid,p_lease uuid)
RETURNS TABLE(provider text,account_id text,generation bigint,credential_version bigint,ciphertext bytea,nonce bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 UPDATE public.provider_token_state SET auth_state='blocked_auth',refresh_lease=NULL,
  refresh_lease_until=NULL,updated_at=now() WHERE tenant_id=p_tenant AND connection_id=p_connection
  AND auth_state='refreshing' AND refresh_lease_until<now();
 IF FOUND THEN
  UPDATE public.connections SET health_status='invalid' WHERE tenant_id=p_tenant AND id=p_connection;
  RETURN;
 END IF;
 UPDATE public.provider_token_state s SET refresh_lease=p_lease,refresh_lease_until=now()+interval '30 seconds',
  auth_state='refreshing',updated_at=now()
 FROM public.connections c WHERE s.tenant_id=p_tenant AND s.connection_id=p_connection
  AND c.tenant_id=s.tenant_id AND c.id=s.connection_id AND c.status='active' AND c.generation=s.generation
  AND s.rotation_mode='rotating' AND s.auth_state='ready' AND s.expires_at<=now()+interval '90 seconds'
  AND s.refresh_lease IS NULL;
 IF NOT FOUND THEN RETURN; END IF;
 RETURN QUERY SELECT c.provider,c.provider_account_id,c.generation,s.credential_version,k.ciphertext,k.nonce
 FROM public.connections c JOIN public.provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 JOIN public.connection_credentials k ON k.tenant_id=c.tenant_id AND k.connection_id=c.id
  AND k.generation=s.generation AND k.credential_version=s.credential_version AND k.secret_kind='api_token' AND k.revoked_at IS NULL
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND s.refresh_lease=p_lease;
END $$;

CREATE FUNCTION provider_refresh_status(p_tenant uuid,p_connection uuid)
RETURNS TABLE(auth_state text,expires_at timestamptz,rotation_mode text)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT s.auth_state,s.expires_at,s.rotation_mode FROM public.provider_token_state s
 JOIN public.connections c ON c.tenant_id=s.tenant_id AND c.id=s.connection_id
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND c.status='active' AND c.generation=s.generation
$$;

CREATE FUNCTION provider_revoke_material(p_session bytea,p_tenant uuid,p_connection uuid)
RETURNS TABLE(provider text,ciphertext bytea,nonce bytea)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT c.provider,k.ciphertext,k.nonce FROM public.connections c
 JOIN public.connection_credentials k ON k.tenant_id=c.tenant_id AND k.connection_id=c.id
  AND k.generation=c.generation AND k.secret_kind='api_token' AND k.revoked_at IS NULL
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND c.status='active'
  AND public.tenant_authorize(p_session,p_tenant,'members_write') IS NOT NULL
$$;

CREATE OR REPLACE FUNCTION business_connection(p_tenant uuid,p_connection uuid,p_provider text)
RETURNS TABLE(account_id text,mode text,environment text,health text,generation bigint,ciphertext bytea,nonce bytea,key_version integer)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT c.provider_account_id,c.mode,c.execution_environment,c.health_status,c.generation,
  cc.ciphertext,cc.nonce,cc.key_version
 FROM public.connections c JOIN public.connection_credentials cc
 ON cc.tenant_id=c.tenant_id AND cc.connection_id=c.id AND cc.generation=c.generation
 AND cc.secret_kind='api_token' AND cc.revoked_at IS NULL
 LEFT JOIN public.provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND c.provider=p_provider AND c.status='active'
 AND c.execution_environment IN ('simulation','test')
 AND (s.connection_id IS NULL OR (s.generation=c.generation AND s.auth_state='ready'
  AND (s.rotation_mode='static' OR s.expires_at>now()+interval '30 seconds')))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION provider_refresh_status(uuid,uuid),provider_revoke_material(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION provider_refresh_status(uuid,uuid) TO relaydesk_runtime;
GRANT EXECUTE ON FUNCTION provider_revoke_material(bytea,uuid,uuid) TO relaydesk_auth;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'provider refresh guard is intentionally irreversible'; END $$;
-- +goose StatementEnd
