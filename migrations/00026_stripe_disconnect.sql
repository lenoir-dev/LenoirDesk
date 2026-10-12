-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION stripe_test_connection_disconnect(p_session bytea,p_tenant uuid,p_connection uuid,p_correlation text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_user uuid; v_changed boolean;
BEGIN
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'members_write') IS NULL
  OR length(p_correlation)>128 THEN RAISE EXCEPTION 'Stripe disconnect denied'; END IF;
 UPDATE public.connections SET status='disconnected',health_status='invalid',generation=generation+1
 WHERE tenant_id=p_tenant AND id=p_connection AND provider='stripe' AND mode='test' AND status<>'disconnected';
 v_changed:=FOUND;
 IF v_changed THEN
  UPDATE public.connection_credentials SET revoked_at=now(),ciphertext='\x'::bytea,
   nonce='\x000000000000000000000000'::bytea
   WHERE tenant_id=p_tenant AND connection_id=p_connection AND revoked_at IS NULL;
  INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
  VALUES(p_tenant,v_user,'connection.disconnect','connection',p_connection,'success',p_correlation);
 END IF;
 RETURN v_changed;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION stripe_test_connection_disconnect(bytea,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stripe_test_connection_disconnect(bytea,uuid,uuid,text) TO relaydesk_app;
-- +goose Down
DROP FUNCTION stripe_test_connection_disconnect(bytea,uuid,uuid,text);
