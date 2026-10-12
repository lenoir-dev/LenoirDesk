-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION link_billing_contact(p_session_hash bytea,p_tenant uuid,p_hubspot uuid,p_customer_key text,p_contact text,p_correlation text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 IF public.tenant_authorize(p_session_hash,p_tenant,'operate') IS NULL THEN RAISE EXCEPTION 'link denied'; END IF;
 IF p_customer_key !~ '^stripe:acct_[A-Za-z0-9_-]+:(test|live):cus_[A-Za-z0-9_-]+$'
    OR p_contact !~ '^[0-9]{1,32}$' OR length(p_correlation)>128 THEN RAISE EXCEPTION 'invalid Contact link'; END IF;
 IF NOT EXISTS(SELECT 1 FROM public.connections WHERE tenant_id=p_tenant AND id=p_hubspot AND provider='hubspot' AND status='active')
 THEN RAISE EXCEPTION 'HubSpot connection unavailable'; END IF;
 INSERT INTO public.contact_links(tenant_id,customer_key,hubspot_connection_id,contact_id,source)
 VALUES(p_tenant,p_customer_key,p_hubspot,p_contact,'explicit')
 ON CONFLICT(tenant_id,customer_key,hubspot_connection_id) DO UPDATE
 SET contact_id=excluded.contact_id,source='explicit';
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,public.auth_session_user(p_session_hash),'contact.link','connection',p_hubspot,'success',p_correlation);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION link_billing_contact(bytea,uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION link_billing_contact(bytea,uuid,uuid,text,text,text) TO relaydesk_app;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'contact link API migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
