-- +goose Up
CREATE TABLE provider_oauth_flows (
 state_hash bytea PRIMARY KEY CHECK (length(state_hash)=32),
 session_hash bytea NOT NULL CHECK (length(session_hash)=32),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 provider text NOT NULL CHECK (provider IN ('hubspot','slack')),
 expected_account_id text NOT NULL CHECK (length(expected_account_id) BETWEEN 1 AND 128),
 reconnect_id uuid,
 redirect_uri text NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 1024),
 verifier_ciphertext bytea,
 verifier_nonce bytea,
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (tenant_id,reconnect_id) REFERENCES connections(tenant_id,id)
);
CREATE INDEX provider_oauth_flows_expiry ON provider_oauth_flows(expires_at);
ALTER TABLE provider_oauth_flows ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_oauth_flows FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_migrate ON provider_oauth_flows TO relaydesk_migrate USING (true) WITH CHECK (true);

ALTER TABLE connection_credentials DROP CONSTRAINT connection_credentials_tenant_id_connection_id_generation_s_key;
ALTER TABLE connection_credentials ADD COLUMN credential_version bigint NOT NULL DEFAULT 1 CHECK (credential_version > 0);
ALTER TABLE connection_credentials ADD CONSTRAINT connection_credentials_version_key
 UNIQUE (tenant_id,connection_id,generation,secret_kind,credential_version);
CREATE UNIQUE INDEX connection_credentials_one_current ON connection_credentials(tenant_id,connection_id,generation,secret_kind)
 WHERE revoked_at IS NULL;

CREATE TABLE provider_token_state (
 tenant_id uuid NOT NULL,
 connection_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation > 0),
 credential_version bigint NOT NULL CHECK (credential_version > 0),
 expires_at timestamptz,
 rotation_mode text NOT NULL CHECK (rotation_mode IN ('rotating','static')),
 refresh_lease uuid,
 refresh_lease_until timestamptz,
 auth_state text NOT NULL CHECK (auth_state IN ('ready','refreshing','blocked_auth')),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,connection_id),
 FOREIGN KEY (tenant_id,connection_id) REFERENCES connections(tenant_id,id)
);
ALTER TABLE provider_token_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_token_state FORCE ROW LEVEL SECURITY;
CREATE POLICY token_migrate ON provider_token_state TO relaydesk_migrate USING (true) WITH CHECK (true);

-- +goose StatementBegin
CREATE FUNCTION provider_oauth_begin(p_hash bytea,p_session bytea,p_tenant uuid,p_provider text,
 p_expected text,p_reconnect uuid,p_redirect text,p_expires timestamptz,p_cipher bytea,p_nonce bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 IF length(p_hash)<>32 OR length(p_session)<>32 OR p_provider NOT IN ('hubspot','slack')
  OR length(p_expected) NOT BETWEEN 1 AND 128 OR length(p_redirect) NOT BETWEEN 1 AND 1024
  OR p_expires<=now() OR p_expires>now()+interval '10 minutes'
  OR (p_provider='hubspot' AND (length(p_cipher)=0 OR length(p_nonce)<>12))
  OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL THEN RAISE EXCEPTION 'oauth begin denied'; END IF;
 IF p_reconnect IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant
  AND c.id=p_reconnect AND c.provider=p_provider AND c.provider_account_id=p_expected AND c.mode='test') THEN
  RAISE EXCEPTION 'reconnect target mismatch'; END IF;
 INSERT INTO public.provider_oauth_flows(state_hash,session_hash,tenant_id,provider,expected_account_id,reconnect_id,redirect_uri,expires_at,verifier_ciphertext,verifier_nonce)
 VALUES(p_hash,p_session,p_tenant,p_provider,p_expected,p_reconnect,p_redirect,p_expires,p_cipher,p_nonce);
END $$;

CREATE FUNCTION provider_oauth_consume(p_hash bytea,p_session bytea,p_provider text,p_redirect text)
RETURNS TABLE(tenant_id uuid,expected_account_id text,reconnect_id uuid,verifier_ciphertext bytea,verifier_nonce bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 RETURN QUERY WITH consumed AS (
  DELETE FROM public.provider_oauth_flows f WHERE f.state_hash=p_hash AND f.session_hash=p_session
   AND f.provider=p_provider AND f.redirect_uri=p_redirect AND f.expires_at>now()
  RETURNING f.tenant_id,f.expected_account_id,f.reconnect_id,f.verifier_ciphertext,f.verifier_nonce
 ) SELECT c.tenant_id,c.expected_account_id,c.reconnect_id,c.verifier_ciphertext,c.verifier_nonce FROM consumed c
  WHERE public.tenant_authorize(p_session,c.tenant_id,'members_write') IS NOT NULL;
END $$;

CREATE FUNCTION provider_connection_commit(p_session bytea,p_tenant uuid,p_provider text,p_account text,
 p_expected text,p_reconnect uuid,p_new_id uuid,p_generation bigint,p_cipher bytea,p_nonce bytea,p_expires timestamptz,p_rotation text,
 p_correlation text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_id uuid; v_generation bigint; v_user uuid;
BEGIN
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL
  OR p_provider NOT IN ('hubspot','slack') OR p_account<>p_expected OR length(p_account) NOT BETWEEN 1 AND 128
  OR length(p_cipher)=0 OR length(p_nonce)<>12 OR p_rotation NOT IN ('rotating','static')
  OR length(p_correlation)>128 THEN RAISE EXCEPTION 'connection commit denied'; END IF;
 IF p_rotation='rotating' AND (p_expires IS NULL OR p_expires<=now()) THEN RAISE EXCEPTION 'token expiry required'; END IF;
 IF p_reconnect IS NULL THEN
  IF p_generation<>1 THEN RAISE EXCEPTION 'initial generation mismatch'; END IF;
  INSERT INTO public.connections(id,tenant_id,provider,provider_account_id,mode,status,health_status,execution_environment,verified_at,last_checked_at)
  VALUES(p_new_id,p_tenant,p_provider,p_account,'test','active','verified','test',now(),now()) RETURNING id,generation INTO v_id,v_generation;
 ELSE
  SELECT id,generation INTO v_id,v_generation FROM public.connections c WHERE c.id=p_reconnect AND c.tenant_id=p_tenant
   AND c.provider=p_provider AND c.provider_account_id=p_account AND c.mode='test' FOR UPDATE;
  IF v_id IS NULL THEN RAISE EXCEPTION 'reconnect identity mismatch'; END IF;
  IF p_generation<>v_generation+1 THEN RAISE EXCEPTION 'reconnect generation mismatch'; END IF;
  UPDATE public.connection_credentials SET revoked_at=now() WHERE tenant_id=p_tenant AND connection_id=v_id AND revoked_at IS NULL;
  v_generation:=v_generation+1;
  UPDATE public.connections SET generation=v_generation,status='active',health_status='verified',
   execution_environment='test',verified_at=now(),last_checked_at=now() WHERE id=v_id;
 END IF;
 INSERT INTO public.connection_credentials(tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version,credential_version)
 VALUES(p_tenant,v_id,v_generation,'api_token',p_cipher,p_nonce,1,1);
 INSERT INTO public.provider_token_state(tenant_id,connection_id,generation,credential_version,expires_at,rotation_mode,auth_state)
 VALUES(p_tenant,v_id,v_generation,1,p_expires,p_rotation,'ready')
 ON CONFLICT(tenant_id,connection_id) DO UPDATE SET generation=excluded.generation,credential_version=1,
  expires_at=excluded.expires_at,rotation_mode=excluded.rotation_mode,refresh_lease=NULL,refresh_lease_until=NULL,
  auth_state='ready',updated_at=now();
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,v_user,CASE WHEN p_reconnect IS NULL THEN 'connection.connect' ELSE 'connection.reconnect' END,
  'connection',v_id,'success',p_correlation);
 RETURN v_id;
END $$;

CREATE FUNCTION provider_connection_disconnect(p_session bytea,p_tenant uuid,p_connection uuid,p_correlation text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_user uuid; v_changed boolean;
BEGIN
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL
  OR length(p_correlation)>128 THEN RAISE EXCEPTION 'disconnect denied'; END IF;
 UPDATE public.connections SET status='disconnected',health_status='invalid',generation=generation+1
 WHERE id=p_connection AND tenant_id=p_tenant AND provider IN ('hubspot','slack') AND status<>'disconnected';
 v_changed:=FOUND;
 IF v_changed THEN
  DELETE FROM public.connection_credentials WHERE tenant_id=p_tenant AND connection_id=p_connection;
  UPDATE public.provider_token_state SET auth_state='blocked_auth',refresh_lease=NULL,refresh_lease_until=NULL,
   updated_at=now() WHERE tenant_id=p_tenant AND connection_id=p_connection;
  INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
  VALUES(p_tenant,v_user,'connection.disconnect','connection',p_connection,'success',p_correlation);
 END IF;
 RETURN v_changed;
END $$;

CREATE FUNCTION provider_refresh_claim(p_tenant uuid,p_connection uuid,p_lease uuid)
RETURNS TABLE(provider text,account_id text,generation bigint,credential_version bigint,ciphertext bytea,nonce bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 UPDATE public.provider_token_state s SET refresh_lease=p_lease,refresh_lease_until=now()+interval '30 seconds',
  auth_state='refreshing',updated_at=now()
 FROM public.connections c WHERE s.tenant_id=p_tenant AND s.connection_id=p_connection
  AND c.tenant_id=s.tenant_id AND c.id=s.connection_id AND c.status='active' AND c.generation=s.generation
  AND s.rotation_mode='rotating' AND s.auth_state<>'blocked_auth' AND s.expires_at<=now()+interval '90 seconds'
  AND (s.refresh_lease IS NULL OR s.refresh_lease_until<now());
 IF NOT FOUND THEN RETURN; END IF;
 RETURN QUERY SELECT c.provider,c.provider_account_id,c.generation,s.credential_version,k.ciphertext,k.nonce
 FROM public.connections c JOIN public.provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 JOIN public.connection_credentials k ON k.tenant_id=c.tenant_id AND k.connection_id=c.id
  AND k.generation=s.generation AND k.credential_version=s.credential_version AND k.secret_kind='api_token' AND k.revoked_at IS NULL
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND s.refresh_lease=p_lease;
END $$;

CREATE FUNCTION provider_refresh_commit(p_tenant uuid,p_connection uuid,p_lease uuid,p_generation bigint,
 p_version bigint,p_cipher bytea,p_nonce bytea,p_expires timestamptz)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_state public.provider_token_state%ROWTYPE;
BEGIN
 SELECT * INTO v_state FROM public.provider_token_state WHERE tenant_id=p_tenant AND connection_id=p_connection FOR UPDATE;
 IF v_state.refresh_lease IS DISTINCT FROM p_lease OR v_state.generation<>p_generation
  OR v_state.credential_version<>p_version OR v_state.refresh_lease_until<now()
  OR p_expires<=now() OR length(p_cipher)=0 OR length(p_nonce)<>12
  OR NOT EXISTS (SELECT 1 FROM public.connections WHERE tenant_id=p_tenant AND id=p_connection
    AND status='active' AND generation=p_generation) THEN RETURN false; END IF;
 UPDATE public.connection_credentials SET revoked_at=now(),ciphertext='\x'::bytea,nonce='\x000000000000000000000000'::bytea
  WHERE tenant_id=p_tenant AND connection_id=p_connection AND generation=p_generation
   AND secret_kind='api_token' AND credential_version=p_version AND revoked_at IS NULL;
 INSERT INTO public.connection_credentials(tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version,credential_version)
 VALUES(p_tenant,p_connection,p_generation,'api_token',p_cipher,p_nonce,1,p_version+1);
 UPDATE public.provider_token_state SET credential_version=p_version+1,expires_at=p_expires,auth_state='ready',
  refresh_lease=NULL,refresh_lease_until=NULL,updated_at=now() WHERE tenant_id=p_tenant AND connection_id=p_connection;
 RETURN true;
END $$;

CREATE FUNCTION provider_refresh_block(p_tenant uuid,p_connection uuid,p_lease uuid,p_generation bigint,p_version bigint)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 UPDATE public.provider_token_state SET auth_state='blocked_auth',refresh_lease=NULL,refresh_lease_until=NULL,updated_at=now()
 WHERE tenant_id=p_tenant AND connection_id=p_connection AND generation=p_generation
  AND credential_version=p_version AND refresh_lease=p_lease;
 IF NOT FOUND THEN RETURN false; END IF;
 UPDATE public.connections SET health_status='invalid' WHERE tenant_id=p_tenant AND id=p_connection AND generation=p_generation;
 RETURN true;
END $$;

-- +goose StatementEnd

REVOKE ALL ON FUNCTION provider_oauth_begin(bytea,bytea,uuid,text,text,uuid,text,timestamptz,bytea,bytea),
 provider_oauth_consume(bytea,bytea,text,text),
 provider_connection_commit(bytea,uuid,text,text,text,uuid,uuid,bigint,bytea,bytea,timestamptz,text,text),
 provider_connection_disconnect(bytea,uuid,uuid,text),
 provider_refresh_claim(uuid,uuid,uuid),provider_refresh_commit(uuid,uuid,uuid,bigint,bigint,bytea,bytea,timestamptz),
 provider_refresh_block(uuid,uuid,uuid,bigint,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION provider_oauth_begin(bytea,bytea,uuid,text,text,uuid,text,timestamptz,bytea,bytea),
 provider_oauth_consume(bytea,bytea,text,text),
 provider_connection_commit(bytea,uuid,text,text,text,uuid,uuid,bigint,bytea,bytea,timestamptz,text,text),
 provider_connection_disconnect(bytea,uuid,uuid,text) TO relaydesk_auth;
GRANT EXECUTE ON FUNCTION provider_refresh_claim(uuid,uuid,uuid),
 provider_refresh_commit(uuid,uuid,uuid,bigint,bigint,bytea,bytea,timestamptz),
 provider_refresh_block(uuid,uuid,uuid,bigint,bigint) TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'provider OAuth migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
