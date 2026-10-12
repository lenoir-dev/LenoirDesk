-- +goose Up
ALTER TABLE business_actions DROP CONSTRAINT business_actions_status_check;
ALTER TABLE business_actions ADD CONSTRAINT business_actions_status_check CHECK (status IN
 ('pending','queued','running','retry_wait','blocked_auth','blocked_config','paused','unknown',
  'needs_attention','dead','succeeded','canceled','superseded'));
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN
 ('pending','running','succeeded','retry_wait','blocked_auth','blocked_config','needs_attention',
  'unknown','dead','paused','skipped','canceled','superseded'));
ALTER TABLE business_actions ADD COLUMN dispatch_generation bigint NOT NULL DEFAULT 0 CHECK (dispatch_generation>=0);
ALTER TABLE business_actions ADD COLUMN queued_at timestamptz;
ALTER TABLE business_actions ADD COLUMN budget_started_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE business_actions ADD COLUMN retry_failures integer NOT NULL DEFAULT 0 CHECK (retry_failures>=0);
ALTER TABLE business_actions ADD COLUMN first_failed_at timestamptz;
ALTER TABLE business_actions ADD COLUMN last_failed_at timestamptz;
ALTER TABLE business_actions ADD COLUMN confirmation_source text CHECK (confirmation_source IN ('provider','operator'));
ALTER TABLE business_actions ADD COLUMN resolution_reason text;
ALTER TABLE business_actions ADD CONSTRAINT business_action_lease_consistent CHECK
 ((status='running' AND lease_token IS NOT NULL AND lease_until IS NOT NULL) OR
  (status<>'running' AND lease_token IS NULL AND lease_until IS NULL));

CREATE TABLE business_action_decisions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES tenants(id),
 action_id uuid NOT NULL, actor_user_id uuid NOT NULL, decision text NOT NULL CHECK
 (decision IN ('manual_retry','confirm_unknown','leave_unknown','risk_retry')),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 1000),
 from_status text NOT NULL, from_generation bigint NOT NULL,
 to_status text NOT NULL, to_generation bigint NOT NULL,
 idempotency_key text, external_channel text, external_ts text,
 duplicate_risk_ack boolean NOT NULL DEFAULT false,
 correlation_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (tenant_id,id), FOREIGN KEY (tenant_id,action_id) REFERENCES business_actions(tenant_id,id)
);
CREATE UNIQUE INDEX business_action_decisions_idempotent ON business_action_decisions(tenant_id,action_id,idempotency_key)
 WHERE idempotency_key IS NOT NULL;
CREATE INDEX business_action_decisions_action ON business_action_decisions(tenant_id,action_id,created_at);

CREATE TABLE business_action_outbox (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES tenants(id),
 action_id uuid NOT NULL, dispatch_generation bigint NOT NULL CHECK (dispatch_generation>0),
 created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz,
 superseded_at timestamptz, lease_token uuid, lease_until timestamptz,
 UNIQUE (tenant_id,id), UNIQUE (tenant_id,action_id,dispatch_generation),
 FOREIGN KEY (tenant_id,action_id) REFERENCES business_actions(tenant_id,id)
);
CREATE INDEX business_action_outbox_pending ON business_action_outbox(created_at,id)
 WHERE published_at IS NULL AND superseded_at IS NULL;

-- +goose StatementBegin
DO $$ DECLARE tab text; BEGIN
 FOREACH tab IN ARRAY ARRAY['business_action_decisions','business_action_outbox'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',tab);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',tab);
  EXECUTE format('CREATE POLICY runtime_tenant ON %I TO relaydesk_runtime USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',tab);
  EXECUTE format('CREATE POLICY migration_admin ON %I TO relaydesk_migrate USING (true) WITH CHECK (true)',tab);
  EXECUTE format('CREATE POLICY browser_member ON %I TO relaydesk_app USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid AND public.tenant_authorize(decode(coalesce(current_setting(''app.session_hash'',true),''''),''hex''),tenant_id,''read'') IS NOT NULL)',tab);
 END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE ON business_action_outbox TO relaydesk_runtime;
GRANT SELECT ON business_action_outbox,business_action_decisions TO relaydesk_app;

-- Reused after executor, watchdog and operator commits. The run is complete only
-- when every linked action has succeeded (including operator-confirmed success).
-- +goose StatementBegin
CREATE FUNCTION refresh_business_run_status(p_tenant uuid,p_action uuid)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 UPDATE public.runs r SET status=s.status,
 finished_at=CASE WHEN s.status IN ('succeeded','canceled','superseded') THEN now() ELSE NULL END
 FROM (SELECT l.run_id,CASE
  WHEN bool_or(a.status='unknown') THEN 'unknown'
  WHEN bool_or(a.status='needs_attention') THEN 'needs_attention'
  WHEN bool_or(a.status='dead') THEN 'dead'
  WHEN bool_or(a.status='blocked_auth') THEN 'blocked_auth'
  WHEN bool_or(a.status='blocked_config') THEN 'blocked_config'
  WHEN bool_or(a.status='paused') THEN 'paused'
  WHEN bool_or(a.status='canceled') THEN 'canceled'
  WHEN bool_or(a.status='superseded') THEN 'superseded'
  WHEN bool_and(a.status='succeeded') THEN 'succeeded'
  WHEN bool_or(a.status='retry_wait') THEN 'retry_wait'
  ELSE 'running' END AS status
  FROM public.business_run_actions l JOIN public.business_actions a ON a.tenant_id=l.tenant_id AND a.id=l.action_id
  WHERE l.tenant_id=p_tenant AND l.run_id IN
   (SELECT run_id FROM public.business_run_actions WHERE tenant_id=p_tenant AND action_id=p_action)
  GROUP BY l.run_id) s WHERE r.tenant_id=p_tenant AND r.id=s.run_id;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION refresh_business_run_status(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION refresh_business_run_status(uuid,uuid) TO relaydesk_runtime;

-- All operator transitions are serialized on the action row. A repeated key
-- returns the original decision, while a different concurrent decision loses
-- the generation compare-and-swap.
-- +goose StatementBegin
CREATE FUNCTION decide_business_action(p_session bytea,p_tenant uuid,p_action uuid,p_decision text,
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
REVOKE ALL ON FUNCTION decide_business_action(bytea,uuid,uuid,text,text,bigint,text,text,text,boolean,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION decide_business_action(bytea,uuid,uuid,text,text,bigint,text,text,text,boolean,text) TO relaydesk_app;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business recovery migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
