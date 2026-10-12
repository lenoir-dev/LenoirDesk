-- +goose Up
-- tenant_authorize returns a role name, not a user UUID. Preserve its access
-- check, then obtain the audited actor identity from the same session hash.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION request_historical_reconcile(p_session bytea,p_tenant uuid,p_connection uuid,
 p_from timestamptz,p_to timestamptz,p_max integer,p_reason text,p_correlation text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_actor uuid; v_id uuid;
BEGIN
 IF public.tenant_authorize(p_session,p_tenant,'operate') IS NULL THEN
  RAISE EXCEPTION 'reconciliation denied' USING ERRCODE='42501';
 END IF;
 v_actor:=public.auth_session_user(p_session);
 IF v_actor IS NULL THEN RAISE EXCEPTION 'session unavailable' USING ERRCODE='42501'; END IF;
 IF p_from IS NULL OR p_to IS NULL OR p_from>=p_to OR p_from<now()-interval '29 days'
    OR p_to>now()-interval '2 minutes' OR p_max<1 OR p_max>500
    OR length(btrim(p_reason)) NOT BETWEEN 12 AND 500 OR length(p_correlation)>128 THEN
  RAISE EXCEPTION 'invalid historical reconciliation bounds' USING ERRCODE='22023';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant AND c.id=p_connection
  AND c.provider='stripe' AND c.mode='test' AND c.status='active'
  AND c.execution_environment IN ('simulation','test') AND c.health_status='verified') THEN
  RAISE EXCEPTION 'Stripe connection unavailable' USING ERRCODE='22023';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM public.automation_revisions r WHERE r.tenant_id=p_tenant
  AND r.connection_id=p_connection AND r.status='active' AND r.template_key='subscription_billing_sync.v1') THEN
  RAISE EXCEPTION 'automation unavailable' USING ERRCODE='22023';
 END IF;
 INSERT INTO public.reconcile_requests(tenant_id,stripe_connection_id,actor_user_id,from_at,to_at,max_events,reason)
 VALUES(p_tenant,p_connection,v_actor,p_from,p_to,p_max,btrim(p_reason)) RETURNING id INTO v_id;
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id,context)
 VALUES(p_tenant,v_actor,'reconciliation.request','reconciliation',v_id,'success',p_correlation,
  jsonb_build_object('from',p_from,'to',p_to,'max_events',p_max,'reason',btrim(p_reason)));
 RETURN v_id;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'historical actor fix is intentionally irreversible'; END $$;
-- +goose StatementEnd
