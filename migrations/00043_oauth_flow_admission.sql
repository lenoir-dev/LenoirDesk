-- +goose Up
-- Policy rows are writable only by the migration role. Tests use smaller values
-- in an isolated database; callers cannot supply or increase their own limits.
CREATE TABLE oauth_admission_policy (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 oidc_source_per_minute integer NOT NULL CHECK (oidc_source_per_minute BETWEEN 1 AND 500),
 oidc_global_per_minute integer NOT NULL CHECK (oidc_global_per_minute BETWEEN 1 AND 900),
 provider_owner_pending integer NOT NULL CHECK (provider_owner_pending BETWEEN 1 AND 100),
 provider_tenant_pending integer NOT NULL CHECK (provider_tenant_pending BETWEEN 1 AND 500),
 provider_global_pending integer NOT NULL CHECK (provider_global_pending BETWEEN 1 AND 10000)
);
INSERT INTO oauth_admission_policy VALUES (true,120,600,20,40,10000);
REVOKE ALL ON oauth_admission_policy FROM PUBLIC,relaydesk_auth,relaydesk_runtime;

CREATE TABLE oidc_admission_buckets (
 bucket_start timestamptz NOT NULL,
 source_hash bytea NOT NULL CHECK (length(source_hash)=32),
 attempts integer NOT NULL CHECK (attempts>0),
 PRIMARY KEY(bucket_start,source_hash)
);
CREATE INDEX oidc_admission_buckets_expiry ON oidc_admission_buckets(bucket_start);
REVOKE ALL ON oidc_admission_buckets FROM PUBLIC,relaydesk_auth,relaydesk_runtime;

-- A zero hash is reserved for the global bucket; source fingerprints are HMACs.
-- Existing oidc_begin remains migration-role-only for old administrative tests.
REVOKE ALL ON FUNCTION oidc_begin(bytea,bytea,bytea,bytea,timestamptz) FROM PUBLIC,relaydesk_auth,relaydesk_runtime;
-- +goose StatementBegin
CREATE FUNCTION oidc_admit_begin(p_source bytea,p_state bytea,p_cipher bytea,p_nonce bytea,
 p_nonce_hash bytea,p_expires timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_bucket timestamptz; v_source integer; v_global integer;
 v_source_limit integer; v_global_limit integer; v_global_hash bytea := decode(repeat('00',32),'hex');
BEGIN
 IF p_source IS NULL OR length(p_source)<>32 OR p_source=v_global_hash THEN
  RAISE EXCEPTION 'invalid admission source'; END IF;
 PERFORM pg_advisory_xact_lock(41907);
 SELECT oidc_source_per_minute,oidc_global_per_minute INTO v_source_limit,v_global_limit
 FROM public.oauth_admission_policy WHERE id=true;
 IF v_source_limit IS NULL THEN RAISE EXCEPTION 'admission policy unavailable'; END IF;
 v_bucket:=date_trunc('minute',now());
 SELECT attempts INTO v_source FROM public.oidc_admission_buckets WHERE bucket_start=v_bucket AND source_hash=p_source;
 SELECT attempts INTO v_global FROM public.oidc_admission_buckets WHERE bucket_start=v_bucket AND source_hash=v_global_hash;
 IF coalesce(v_source,0)>=v_source_limit OR coalesce(v_global,0)>=v_global_limit THEN
  RAISE EXCEPTION USING ERRCODE='P5301',MESSAGE='OIDC admission exhausted';
 END IF;
 INSERT INTO public.oidc_admission_buckets(bucket_start,source_hash,attempts) VALUES(v_bucket,p_source,1)
 ON CONFLICT(bucket_start,source_hash) DO UPDATE SET attempts=public.oidc_admission_buckets.attempts+1;
 INSERT INTO public.oidc_admission_buckets(bucket_start,source_hash,attempts) VALUES(v_bucket,v_global_hash,1)
 ON CONFLICT(bucket_start,source_hash) DO UPDATE SET attempts=public.oidc_admission_buckets.attempts+1;
 PERFORM public.oidc_begin(p_state,p_cipher,p_nonce,p_nonce_hash,p_expires);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION oidc_admit_begin(bytea,bytea,bytea,bytea,bytea,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION oidc_admit_begin(bytea,bytea,bytea,bytea,bytea,timestamptz) TO relaydesk_auth;

ALTER TABLE provider_oauth_flows ADD COLUMN actor_user_id uuid REFERENCES users(id);
UPDATE provider_oauth_flows f SET actor_user_id=s.user_id
 FROM user_sessions s WHERE f.session_hash=s.token_hash;
CREATE INDEX provider_oauth_flows_actor_active ON provider_oauth_flows(actor_user_id,expires_at);
CREATE INDEX provider_oauth_flows_tenant_active ON provider_oauth_flows(tenant_id,expires_at);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION provider_oauth_begin(p_hash bytea,p_session bytea,p_tenant uuid,p_provider text,
 p_expected text,p_reconnect uuid,p_redirect text,p_expires timestamptz,p_cipher bytea,p_nonce bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_user uuid; v_owner_limit integer; v_tenant_limit integer; v_global_limit integer;
BEGIN
 IF p_hash IS NULL OR p_session IS NULL OR p_provider IS NULL OR p_expected IS NULL OR p_redirect IS NULL
  OR p_expires IS NULL OR length(p_hash)<>32 OR length(p_session)<>32 OR p_provider NOT IN ('hubspot','slack')
  OR length(p_expected) NOT BETWEEN 1 AND 128 OR length(p_redirect) NOT BETWEEN 1 AND 1024
  OR p_expires<=now() OR p_expires>now()+interval '10 minutes'
  OR (p_provider='hubspot' AND (p_cipher IS NULL OR length(p_cipher)=0 OR p_nonce IS NULL OR length(p_nonce)<>12)) THEN
  RAISE EXCEPTION 'oauth begin denied'; END IF;
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL THEN
  RAISE EXCEPTION 'oauth begin denied'; END IF;
 IF p_reconnect IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant
  AND c.id=p_reconnect AND c.provider=p_provider AND c.provider_account_id=p_expected AND c.mode='test') THEN
  RAISE EXCEPTION 'reconnect target mismatch'; END IF;
 PERFORM pg_advisory_xact_lock(41908);
 SELECT provider_owner_pending,provider_tenant_pending,provider_global_pending
 INTO v_owner_limit,v_tenant_limit,v_global_limit FROM public.oauth_admission_policy WHERE id=true;
 IF v_owner_limit IS NULL THEN RAISE EXCEPTION 'admission policy unavailable'; END IF;
 IF (SELECT count(*) FROM public.provider_oauth_flows WHERE actor_user_id=v_user AND expires_at>now())>=v_owner_limit
  OR (SELECT count(*) FROM public.provider_oauth_flows WHERE tenant_id=p_tenant AND expires_at>now())>=v_tenant_limit
  OR (SELECT count(*) FROM public.provider_oauth_flows WHERE expires_at>now())>=v_global_limit THEN
  RAISE EXCEPTION USING ERRCODE='P5302',MESSAGE='provider OAuth admission exhausted';
 END IF;
 INSERT INTO public.provider_oauth_flows(state_hash,session_hash,tenant_id,provider,expected_account_id,
  reconnect_id,redirect_uri,expires_at,verifier_ciphertext,verifier_nonce,actor_user_id)
 VALUES(p_hash,p_session,p_tenant,p_provider,p_expected,p_reconnect,p_redirect,p_expires,p_cipher,p_nonce,v_user);
END $$;
-- +goose StatementEnd

-- Background sweeps also run when no new login or provider flow is admitted.
-- SKIP LOCKED makes concurrent API instances safe; valid flows are untouched.
-- +goose StatementBegin
CREATE FUNCTION oauth_flow_cleanup(p_batch integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_count integer:=0; v_changed integer;
BEGIN
 IF p_batch<1 OR p_batch>500 THEN RAISE EXCEPTION 'invalid cleanup batch'; END IF;
 WITH due AS (SELECT state_hash FROM public.provider_oauth_flows WHERE expires_at<=now()
  ORDER BY expires_at LIMIT p_batch FOR UPDATE SKIP LOCKED)
 DELETE FROM public.provider_oauth_flows f USING due d WHERE f.state_hash=d.state_hash;
 GET DIAGNOSTICS v_changed=ROW_COUNT; v_count:=v_count+v_changed;
 WITH due AS (SELECT state_hash FROM public.oidc_flows WHERE expires_at<=now()
  ORDER BY expires_at LIMIT p_batch FOR UPDATE SKIP LOCKED)
 DELETE FROM public.oidc_flows f USING due d WHERE f.state_hash=d.state_hash;
 GET DIAGNOSTICS v_changed=ROW_COUNT; v_count:=v_count+v_changed;
 WITH due AS (SELECT bucket_start,source_hash FROM public.oidc_admission_buckets
  WHERE bucket_start<date_trunc('minute',now())-interval '1 minute'
  ORDER BY bucket_start LIMIT p_batch FOR UPDATE SKIP LOCKED)
 DELETE FROM public.oidc_admission_buckets f USING due d
 WHERE f.bucket_start=d.bucket_start AND f.source_hash=d.source_hash;
 GET DIAGNOSTICS v_changed=ROW_COUNT; v_count:=v_count+v_changed;
 RETURN v_count;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION oauth_flow_cleanup(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION oauth_flow_cleanup(integer) TO relaydesk_auth;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'OAuth admission migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
