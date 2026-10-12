-- +goose Up
ALTER TABLE business_actions ADD COLUMN paused_from_status text CHECK
 (paused_from_status IN ('pending','queued','retry_wait'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_billing_pause(p_session_hash bytea,p_tenant uuid,p_stripe uuid,p_paused boolean,p_correlation text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record;
BEGIN
 IF public.tenant_authorize(p_session_hash,p_tenant,'operate') IS NULL OR length(p_correlation)>128 THEN
  RAISE EXCEPTION 'pause denied'; END IF;
 IF NOT EXISTS (SELECT 1 FROM public.automation_revisions WHERE tenant_id=p_tenant AND connection_id=p_stripe
  AND template_key='subscription_billing_sync.v1') THEN RAISE EXCEPTION 'automation not found'; END IF;
 INSERT INTO public.automation_gates(tenant_id,stripe_connection_id,paused,updated_at)
 VALUES(p_tenant,p_stripe,p_paused,now()) ON CONFLICT (tenant_id,stripe_connection_id)
 DO UPDATE SET paused=excluded.paused,updated_at=now();
 IF p_paused THEN
  UPDATE public.business_action_outbox o SET superseded_at=now(),lease_token=NULL,lease_until=NULL
   FROM public.business_actions b WHERE o.tenant_id=b.tenant_id AND o.action_id=b.id
   AND o.dispatch_generation=b.dispatch_generation AND o.superseded_at IS NULL
   AND b.tenant_id=p_tenant AND b.stripe_connection_id=p_stripe AND b.status='queued';
  FOR a IN UPDATE public.business_actions SET paused_from_status=status,status='paused',queued_at=NULL,
    updated_at=now() WHERE tenant_id=p_tenant AND stripe_connection_id=p_stripe
    AND status IN ('pending','queued','retry_wait') RETURNING id LOOP
   PERFORM public.refresh_business_run_status(p_tenant,a.id);
  END LOOP;
 ELSE
  FOR a IN UPDATE public.business_actions SET
    status=CASE WHEN paused_from_status='retry_wait' AND next_attempt_at>now() THEN 'retry_wait' ELSE 'pending' END,
    paused_from_status=NULL,updated_at=now()
    WHERE tenant_id=p_tenant AND stripe_connection_id=p_stripe AND status='paused' RETURNING id LOOP
   PERFORM public.refresh_business_run_status(p_tenant,a.id);
  END LOOP;
 END IF;
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,public.auth_session_user(p_session_hash),CASE WHEN p_paused THEN 'automation.pause' ELSE 'automation.resume' END,
 'connection',p_stripe,'success',p_correlation);
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'pause recovery migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
