-- +goose Up
-- Correct the existing live migration and preserve the test-only legacy claimant's first-attempt clock.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION decide_business_action(p_session bytea,p_tenant uuid,p_action uuid,p_decision text,
 p_reason text,p_generation bigint,p_idempotency text,p_channel text,p_ts text,p_ack boolean,p_correlation text)
RETURNS TABLE(decision_id uuid, action_status text, action_generation bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a public.business_actions%ROWTYPE; v_existing public.business_action_decisions%ROWTYPE;
 v_actor uuid; v_destination text; v_from text; v_oldgen bigint;
BEGIN
 IF public.tenant_authorize(p_session,p_tenant,'operate') IS NULL THEN
  RAISE EXCEPTION 'operation denied' USING ERRCODE='42501'; END IF;
 v_actor:=public.auth_session_user(p_session);
 IF p_reason IS NULL OR length(btrim(p_reason)) NOT BETWEEN 1 AND 1000 OR
    length(p_correlation)>128 OR (p_idempotency IS NOT NULL AND length(p_idempotency) NOT BETWEEN 1 AND 128) THEN
  RAISE EXCEPTION 'invalid recovery request' USING ERRCODE='22023'; END IF;
 IF p_idempotency IS NOT NULL THEN
  SELECT * INTO v_existing FROM public.business_action_decisions d WHERE d.tenant_id=p_tenant
   AND d.action_id=p_action AND d.idempotency_key=p_idempotency;
  IF FOUND THEN
   IF v_existing.decision<>p_decision OR v_existing.reason<>p_reason OR
      v_existing.external_channel IS DISTINCT FROM p_channel OR v_existing.external_ts IS DISTINCT FROM p_ts OR
      v_existing.duplicate_risk_ack<>p_ack THEN
    RAISE EXCEPTION 'idempotency key conflict' USING ERRCODE='23505'; END IF;
   decision_id:=v_existing.id; action_status:=v_existing.to_status; action_generation:=v_existing.to_generation;
   RETURN NEXT; RETURN;
  END IF;
 END IF;
 SELECT * INTO a FROM public.business_actions WHERE tenant_id=p_tenant AND id=p_action FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'action not found' USING ERRCODE='P0002'; END IF;
 IF p_idempotency IS NOT NULL THEN
  SELECT * INTO v_existing FROM public.business_action_decisions d WHERE d.tenant_id=p_tenant
   AND d.action_id=p_action AND d.idempotency_key=p_idempotency;
  IF FOUND THEN
   IF v_existing.decision<>p_decision OR v_existing.reason<>p_reason OR
      v_existing.external_channel IS DISTINCT FROM p_channel OR v_existing.external_ts IS DISTINCT FROM p_ts OR
      v_existing.duplicate_risk_ack<>p_ack THEN
    RAISE EXCEPTION 'idempotency key conflict' USING ERRCODE='23505'; END IF;
   decision_id:=v_existing.id; action_status:=v_existing.to_status; action_generation:=v_existing.to_generation;
   RETURN NEXT; RETURN;
  END IF;
 END IF;
 IF a.generation<>p_generation THEN RAISE EXCEPTION 'stale action generation' USING ERRCODE='40001'; END IF;
 v_from:=a.status; v_oldgen:=a.generation;
 IF p_decision='manual_retry' THEN
  IF a.status NOT IN ('dead','needs_attention','blocked_auth','blocked_config') THEN
   RAISE EXCEPTION 'ordinary retry forbidden in current state' USING ERRCODE='22023'; END IF;
  UPDATE public.business_actions SET status='pending',generation=generation+1,budget_started_at=now(),
   retry_failures=0,next_attempt_at=NULL,error_class=NULL,queued_at=NULL,updated_at=now() WHERE id=a.id;
 ELSIF p_decision='confirm_unknown' THEN
  IF a.status<>'unknown' OR a.kind<>'slack_invoice_paid' THEN
   RAISE EXCEPTION 'confirmation requires unknown Slack action' USING ERRCODE='22023'; END IF;
  SELECT r.config->>'slack_channel_id' INTO v_destination FROM public.automation_revisions r
    WHERE r.tenant_id=p_tenant AND r.id=a.automation_revision_id;
  IF p_channel IS NULL OR p_channel<>v_destination OR p_ts IS NULL OR
     p_ts !~ '^[0-9]{10,16}\.[0-9]{6}$' THEN
   RAISE EXCEPTION 'invalid Slack confirmation reference' USING ERRCODE='22023'; END IF;
  UPDATE public.business_actions SET status='succeeded',generation=generation+1,
   slack_channel=p_channel,slack_ts=p_ts,confirmation_source='operator',resolution_reason=p_reason,
   error_class=NULL,updated_at=now() WHERE id=a.id;
 ELSIF p_decision='leave_unknown' THEN
  IF a.status<>'unknown' THEN RAISE EXCEPTION 'action is not unknown' USING ERRCODE='22023'; END IF;
  UPDATE public.business_actions SET generation=generation+1,resolution_reason=p_reason,updated_at=now() WHERE id=a.id;
 ELSIF p_decision='risk_retry' THEN
  IF a.status<>'unknown' OR a.kind<>'slack_invoice_paid' OR p_ack IS DISTINCT FROM true THEN
   RAISE EXCEPTION 'unknown retry requires explicit duplicate-risk acknowledgment' USING ERRCODE='22023'; END IF;
  UPDATE public.business_actions SET status='pending',generation=generation+1,budget_started_at=now(),
   retry_failures=0,next_attempt_at=NULL,error_class=NULL,queued_at=NULL,resolution_reason=p_reason,
   updated_at=now() WHERE id=a.id;
 ELSE RAISE EXCEPTION 'unsupported recovery decision' USING ERRCODE='22023'; END IF;
 SELECT status,generation INTO action_status,action_generation FROM public.business_actions WHERE id=a.id;
 INSERT INTO public.business_action_decisions(tenant_id,action_id,actor_user_id,decision,reason,
  from_status,from_generation,to_status,to_generation,idempotency_key,external_channel,external_ts,
  duplicate_risk_ack,correlation_id)
 VALUES(p_tenant,p_action,v_actor,p_decision,btrim(p_reason),v_from,v_oldgen,action_status,
  action_generation,p_idempotency,p_channel,p_ts,p_ack,p_correlation) RETURNING id INTO decision_id;
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,v_actor,'business.'||p_decision,'business_action',p_action,'success',p_correlation);
 PERFORM public.refresh_business_run_status(p_tenant,p_action);
 RETURN NEXT;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION claim_business_action()
RETURNS TABLE(tenant_id uuid, action_id uuid, fence_token uuid, fence_generation bigint, attempt_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; v_token uuid; v_target uuid; v_generation bigint; v_attempt uuid;
  v_aggregate text; v_lease uuid; v_started timestamptz;
BEGIN
 FOR a IN
   SELECT b.id,b.tenant_id,b.kind,b.status,b.customer_id,b.stripe_connection_id,b.automation_revision_id,b.attempts,b.generation
   FROM public.business_actions b
   WHERE (b.status='pending' OR (b.status='retry_wait' AND b.next_attempt_at<=now())
          OR (b.status='running' AND b.lease_until<now()))
     AND NOT EXISTS (SELECT 1 FROM public.automation_gates g WHERE g.tenant_id=b.tenant_id
       AND g.stripe_connection_id=b.stripe_connection_id AND g.paused)
     AND (b.depends_on IS NULL OR EXISTS (SELECT 1 FROM public.business_actions d
       WHERE d.tenant_id=b.tenant_id AND d.id=b.depends_on AND d.status='succeeded'))
   ORDER BY b.created_at,b.id FOR UPDATE OF b SKIP LOCKED LIMIT 20
 LOOP
   v_lease:=NULL;
   v_started:=NULL;
   IF a.kind='slack_invoice_paid' AND a.status='running' THEN
     SELECT ba.request_started_at INTO v_started FROM public.business_attempts ba
       WHERE ba.tenant_id=a.tenant_id AND ba.action_id=a.id ORDER BY ba.attempt_number DESC LIMIT 1;
     IF v_started IS NOT NULL THEN
       UPDATE public.business_actions SET status='unknown',lease_token=NULL,lease_until=NULL,
         error_class='slack_result_unknown',updated_at=now() WHERE id=a.id;
       UPDATE public.runs r SET status='unknown' FROM public.business_run_actions l
         WHERE l.tenant_id=a.tenant_id AND l.action_id=a.id AND r.tenant_id=l.tenant_id AND r.id=l.run_id;
       CONTINUE;
     END IF;
   END IF;
   SELECT CASE WHEN a.kind='crm_projection' THEN (r.config->>'hubspot_connection_id')::uuid
      ELSE (r.config->>'slack_connection_id')::uuid END INTO v_target
      FROM public.automation_revisions r WHERE r.tenant_id=a.tenant_id AND r.id=a.automation_revision_id;
   SELECT c.generation INTO v_generation FROM public.connections c
      WHERE c.tenant_id=a.tenant_id AND c.id=v_target AND c.status='active';
   IF v_generation IS NULL THEN
     UPDATE public.business_actions SET status='blocked_config',error_class='connection_missing',updated_at=now() WHERE id=a.id;
     CONTINUE;
   END IF;
   v_token:=gen_random_uuid();
   IF a.kind='crm_projection' THEN
     SELECT c.provider_account_id||'|'||c.mode||'|'||a.customer_id INTO v_aggregate
       FROM public.connections c WHERE c.tenant_id=a.tenant_id AND c.id=a.stripe_connection_id;
     INSERT INTO public.crm_aggregate_leases(tenant_id,customer_key,action_id,lease_token,lease_until)
       VALUES(a.tenant_id,v_aggregate,a.id,v_token,now()+interval '30 seconds')
       ON CONFLICT ON CONSTRAINT crm_aggregate_leases_pkey DO UPDATE SET action_id=excluded.action_id,
         lease_token=excluded.lease_token,lease_until=excluded.lease_until
       WHERE public.crm_aggregate_leases.lease_until<now()
       RETURNING lease_token INTO v_lease;
     IF v_lease IS NULL THEN CONTINUE; END IF;
   END IF;
   UPDATE public.business_actions SET status='running',generation=generation+1,
     attempts=attempts+1,budget_started_at=CASE WHEN attempts=0 THEN now() ELSE budget_started_at END,lease_token=v_token,lease_until=now()+interval '30 seconds',
     next_attempt_at=NULL,updated_at=now() WHERE id=a.id RETURNING generation INTO fence_generation;
   INSERT INTO public.business_attempts(tenant_id,action_id,attempt_number,effect_key,
     connection_generation,fence_generation,lease_token)
   SELECT b.tenant_id,b.id,b.attempts,b.effect_key,v_generation,b.generation,v_token
   FROM public.business_actions b WHERE b.id=a.id RETURNING id INTO v_attempt;
   tenant_id:=a.tenant_id; action_id:=a.id; fence_token:=v_token; attempt_id:=v_attempt;
   RETURN NEXT;
   RETURN;
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recovery correction is intentionally irreversible'; END $$;
-- +goose StatementEnd
