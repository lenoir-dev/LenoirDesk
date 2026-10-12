-- +goose Up
CREATE POLICY provider_token_app_read ON provider_token_state TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);
GRANT SELECT (tenant_id,connection_id,expires_at,rotation_mode,auth_state) ON provider_token_state TO relaydesk_app;
GRANT SELECT (verified_at,last_checked_at) ON connections TO relaydesk_app;
CREATE INDEX runs_tenant_history ON runs(tenant_id,created_at DESC,id DESC);
CREATE INDEX runs_tenant_status_history ON runs(tenant_id,status,created_at DESC,id DESC);
GRANT SELECT (tenant_id,customer_key,hubspot_connection_id,contact_id,source,created_at) ON contact_links TO relaydesk_app;
CREATE POLICY contact_links_app_read ON contact_links TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);
GRANT SELECT (tenant_id,operation,target_type,target_id,result,created_at) ON audit_log TO relaydesk_app;
CREATE POLICY audit_app_read ON audit_log TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);

-- Browser callers can commit only a previously verified, encrypted Test Mode
-- restricted key and webhook secret. They cannot read credential material.
-- +goose StatementBegin
CREATE FUNCTION stripe_test_connection_commit(
 p_session bytea,p_tenant uuid,p_account text,p_existing uuid,p_new uuid,p_ref_hash bytea,
 p_key_cipher bytea,p_key_nonce bytea,p_sign_cipher bytea,p_sign_nonce bytea,p_correlation text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_id uuid; v_generation bigint; v_user uuid;
BEGIN
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL
  OR p_account !~ '^acct_[A-Za-z0-9]+$' OR length(p_account)>128
  OR length(p_ref_hash)<>32 OR length(p_key_cipher)=0 OR length(p_key_nonce)<>12
  OR length(p_sign_cipher)=0 OR length(p_sign_nonce)<>12 OR length(p_correlation)>128
 THEN RAISE EXCEPTION 'Stripe connection denied'; END IF;
 IF p_existing IS NULL THEN
  INSERT INTO public.connections(id,tenant_id,provider,provider_account_id,mode,status,
   health_status,execution_environment,webhook_ref_hash,verified_at,last_checked_at)
  VALUES(p_new,p_tenant,'stripe',p_account,'test','active','verified','test',p_ref_hash,now(),now())
  RETURNING id,generation INTO v_id,v_generation;
 ELSE
  SELECT id,generation INTO v_id,v_generation FROM public.connections
   WHERE tenant_id=p_tenant AND id=p_existing AND provider='stripe'
    AND provider_account_id=p_account AND mode='test' FOR UPDATE;
  IF v_id IS NULL THEN RAISE EXCEPTION 'Stripe reconnect identity mismatch'; END IF;
  UPDATE public.connection_credentials SET revoked_at=now(),ciphertext='\x'::bytea,
   nonce='\x000000000000000000000000'::bytea
   WHERE tenant_id=p_tenant AND connection_id=v_id AND revoked_at IS NULL;
  v_generation:=v_generation+1;
  UPDATE public.connections SET generation=v_generation,status='active',health_status='verified',
   execution_environment='test',webhook_ref_hash=p_ref_hash,verified_at=now(),last_checked_at=now()
   WHERE tenant_id=p_tenant AND id=v_id;
 END IF;
 INSERT INTO public.connection_credentials(tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version)
 VALUES(p_tenant,v_id,v_generation,'api_token',p_key_cipher,p_key_nonce,1);
 INSERT INTO public.connection_credentials(tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version)
 VALUES(p_tenant,v_id,v_generation,'webhook_signing',p_sign_cipher,p_sign_nonce,1);
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,v_user,CASE WHEN p_existing IS NULL THEN 'connection.connect' ELSE 'connection.reconnect' END,
  'connection',v_id,'success',p_correlation);
 RETURN v_id;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION stripe_test_connection_commit(bytea,uuid,text,uuid,uuid,bytea,bytea,bytea,bytea,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stripe_test_connection_commit(bytea,uuid,text,uuid,uuid,bytea,bytea,bytea,bytea,bytea,text) TO relaydesk_app;

-- +goose Down
DROP FUNCTION stripe_test_connection_commit(bytea,uuid,text,uuid,uuid,bytea,bytea,bytea,bytea,bytea,text);
DROP INDEX runs_tenant_status_history;
DROP INDEX runs_tenant_history;
DROP POLICY audit_app_read ON audit_log;
REVOKE SELECT (tenant_id,operation,target_type,target_id,result,created_at) ON audit_log FROM relaydesk_app;
DROP POLICY contact_links_app_read ON contact_links;
REVOKE SELECT (tenant_id,customer_key,hubspot_connection_id,contact_id,source,created_at) ON contact_links FROM relaydesk_app;
REVOKE SELECT (verified_at,last_checked_at) ON connections FROM relaydesk_app;
REVOKE SELECT (tenant_id,connection_id,expires_at,rotation_mode,auth_state) ON provider_token_state FROM relaydesk_app;
DROP POLICY provider_token_app_read ON provider_token_state;
