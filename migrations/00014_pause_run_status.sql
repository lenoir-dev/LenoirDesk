-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_billing_pause(p_session_hash bytea,p_tenant uuid,p_stripe uuid,p_paused boolean,p_correlation text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 IF public.tenant_authorize(p_session_hash,p_tenant,'operate') IS NULL OR length(p_correlation)>128 THEN
   RAISE EXCEPTION 'pause denied'; END IF;
 IF NOT EXISTS (SELECT 1 FROM public.automation_revisions WHERE tenant_id=p_tenant AND connection_id=p_stripe
   AND template_key='subscription_billing_sync.v1') THEN RAISE EXCEPTION 'automation not found'; END IF;
 INSERT INTO public.automation_gates(tenant_id,stripe_connection_id,paused,updated_at)
 VALUES(p_tenant,p_stripe,p_paused,now()) ON CONFLICT (tenant_id,stripe_connection_id)
 DO UPDATE SET paused=excluded.paused,updated_at=now();
 UPDATE public.business_actions SET status=CASE WHEN p_paused THEN 'paused' ELSE 'pending' END,
   updated_at=now() WHERE tenant_id=p_tenant AND stripe_connection_id=p_stripe
   AND status=CASE WHEN p_paused THEN 'pending' ELSE 'paused' END;
 UPDATE public.runs r SET status=CASE WHEN p_paused THEN 'paused' ELSE 'running' END
 WHERE r.tenant_id=p_tenant AND r.status=CASE WHEN p_paused THEN 'running' ELSE 'paused' END
   AND EXISTS (SELECT 1 FROM public.business_run_actions l JOIN public.business_actions a
     ON a.tenant_id=l.tenant_id AND a.id=l.action_id
     WHERE l.tenant_id=p_tenant AND l.run_id=r.id AND a.stripe_connection_id=p_stripe
       AND a.status=CASE WHEN p_paused THEN 'paused' ELSE 'pending' END);
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,public.auth_session_user(p_session_hash),CASE WHEN p_paused THEN 'automation.pause' ELSE 'automation.resume' END,
 'connection',p_stripe,'success',p_correlation);
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'pause status migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
