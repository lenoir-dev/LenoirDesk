-- +goose Up
-- Every browser/API Contact repair carries an auditable operator reason.
-- The existing six-argument function is no longer callable by the app role.
REVOKE EXECUTE ON FUNCTION link_billing_contact(bytea,uuid,uuid,text,text,text) FROM relaydesk_app;
-- +goose StatementBegin
CREATE FUNCTION link_billing_contact_reviewed(p_session bytea,p_tenant uuid,p_hubspot uuid,
 p_key text,p_contact text,p_reason text,p_correlation text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_user uuid; v_previous text;
BEGIN
 v_user:=public.auth_session_user(p_session);
 IF v_user IS NULL OR public.tenant_authorize(p_session,p_tenant,'operate') IS NULL
  OR p_key !~ '^stripe:acct_[A-Za-z0-9_-]+:test:cus_[A-Za-z0-9_-]+$'
  OR p_contact !~ '^[0-9]{1,32}$' OR length(btrim(p_reason))<3 OR length(p_reason)>1000
  OR length(p_correlation)>128 THEN RAISE EXCEPTION 'Contact link denied'; END IF;
 IF NOT EXISTS(SELECT 1 FROM public.connections WHERE tenant_id=p_tenant AND id=p_hubspot
  AND provider='hubspot' AND mode='test' AND status='active' AND health_status='verified')
 THEN RAISE EXCEPTION 'HubSpot connection unavailable'; END IF;
 SELECT contact_id INTO v_previous FROM public.contact_links
  WHERE tenant_id=p_tenant AND customer_key=p_key AND hubspot_connection_id=p_hubspot FOR UPDATE;
 IF EXISTS(SELECT 1 FROM public.contact_links WHERE tenant_id=p_tenant AND hubspot_connection_id=p_hubspot
  AND contact_id=p_contact AND customer_key<>p_key)
 THEN RAISE EXCEPTION 'Contact is bound to another customer'; END IF;
 INSERT INTO public.contact_links(tenant_id,customer_key,hubspot_connection_id,contact_id,source)
 VALUES(p_tenant,p_key,p_hubspot,p_contact,'explicit')
 ON CONFLICT(tenant_id,customer_key,hubspot_connection_id) DO UPDATE
 SET contact_id=excluded.contact_id,source='explicit';
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id,context)
 VALUES(p_tenant,v_user,'contact.link','connection',p_hubspot,'success',p_correlation,
  jsonb_build_object('reason',p_reason,'previous_contact_id',v_previous,'contact_id',p_contact,'customer_key',p_key));
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION link_billing_contact_reviewed(bytea,uuid,uuid,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION link_billing_contact_reviewed(bytea,uuid,uuid,text,text,text,text) TO relaydesk_app;
-- +goose Down
DROP FUNCTION link_billing_contact_reviewed(bytea,uuid,uuid,text,text,text,text);
GRANT EXECUTE ON FUNCTION link_billing_contact(bytea,uuid,uuid,text,text,text) TO relaydesk_app;
