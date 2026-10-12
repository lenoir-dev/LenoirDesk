-- +goose Up
-- PostgreSQL owns action timing. JetStream carries only generation-fenced pointers.
-- +goose StatementBegin
CREATE FUNCTION queue_business_actions(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; n integer:=0; v_generation bigint;
BEGIN
 FOR a IN SELECT b.id,b.tenant_id FROM public.business_actions b
  WHERE (b.status='pending' OR (b.status='retry_wait' AND b.next_attempt_at<=now()))
   AND NOT EXISTS (SELECT 1 FROM public.automation_gates g WHERE g.tenant_id=b.tenant_id
    AND g.stripe_connection_id=b.stripe_connection_id AND g.paused)
   AND (b.depends_on IS NULL OR EXISTS (SELECT 1 FROM public.business_actions d
    WHERE d.tenant_id=b.tenant_id AND d.id=b.depends_on AND d.status='succeeded'))
  ORDER BY b.created_at,b.id FOR UPDATE OF b SKIP LOCKED LIMIT least(greatest(p_limit,0),100)
 LOOP
  UPDATE public.business_actions SET status='queued',dispatch_generation=dispatch_generation+1,
   queued_at=now(),next_attempt_at=NULL,updated_at=now() WHERE id=a.id RETURNING dispatch_generation INTO v_generation;
  INSERT INTO public.business_action_outbox(tenant_id,action_id,dispatch_generation)
   VALUES(a.tenant_id,a.id,v_generation);
  PERFORM public.refresh_business_run_status(a.tenant_id,a.id);
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION queue_business_actions(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_business_actions(integer) TO relaydesk_runtime;

-- +goose StatementBegin
CREATE FUNCTION claim_business_outbox(p_limit integer)
RETURNS TABLE(id uuid,tenant_id uuid,action_id uuid,dispatch_generation bigint,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 RETURN QUERY WITH candidate AS (
  SELECT o.id FROM public.business_action_outbox o
  WHERE o.published_at IS NULL AND o.superseded_at IS NULL
   AND (o.lease_until IS NULL OR o.lease_until<now())
  ORDER BY o.created_at,o.id FOR UPDATE SKIP LOCKED LIMIT least(greatest(p_limit,0),100)
 ) UPDATE public.business_action_outbox o SET lease_token=gen_random_uuid(),lease_until=now()+interval '15 seconds'
 FROM candidate c WHERE o.id=c.id
 RETURNING o.id,o.tenant_id,o.action_id,o.dispatch_generation,o.lease_token;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_business_outbox(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_business_outbox(integer) TO relaydesk_runtime;

-- Atomic pointer claim: dispatch generation, state and dependency are all
-- checked under the action lock. A new fencing generation is allocated only
-- after the CRM aggregate lease and target connection are available.
-- +goose StatementBegin
CREATE FUNCTION claim_business_action_pointer(p_tenant uuid,p_action uuid,p_dispatch bigint)
RETURNS TABLE(tenant_id uuid,action_id uuid,fence_token uuid,fence_generation bigint,attempt_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; v_token uuid; v_target uuid; v_connection_generation bigint;
 v_aggregate text; v_lease uuid; v_attempt uuid;
BEGIN
 SELECT b.* INTO a FROM public.business_actions b WHERE b.tenant_id=p_tenant AND b.id=p_action FOR UPDATE;
 IF NOT FOUND OR a.status<>'queued' OR a.dispatch_generation<>p_dispatch THEN RETURN; END IF;
 IF EXISTS (SELECT 1 FROM public.automation_gates g WHERE g.tenant_id=a.tenant_id
   AND g.stripe_connection_id=a.stripe_connection_id AND g.paused) THEN RETURN; END IF;
 IF a.depends_on IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.business_actions d
   WHERE d.tenant_id=a.tenant_id AND d.id=a.depends_on AND d.status='succeeded') THEN RETURN; END IF;
 SELECT CASE WHEN a.kind='crm_projection' THEN (r.config->>'hubspot_connection_id')::uuid
  ELSE (r.config->>'slack_connection_id')::uuid END INTO v_target FROM public.automation_revisions r
  WHERE r.tenant_id=a.tenant_id AND r.id=a.automation_revision_id;
 SELECT c.generation INTO v_connection_generation FROM public.connections c
  WHERE c.tenant_id=a.tenant_id AND c.id=v_target AND c.status='active';
 IF v_connection_generation IS NULL THEN
  UPDATE public.business_actions SET status='blocked_config',error_class='connection_missing',queued_at=NULL,
   updated_at=now() WHERE id=a.id;
  PERFORM public.refresh_business_run_status(a.tenant_id,a.id);
  RETURN;
 END IF;
 v_token:=gen_random_uuid(); v_lease:=NULL;
 IF a.kind='crm_projection' THEN
  SELECT c.provider_account_id||'|'||c.mode||'|'||a.customer_id INTO v_aggregate
   FROM public.connections c WHERE c.tenant_id=a.tenant_id AND c.id=a.stripe_connection_id;
  INSERT INTO public.crm_aggregate_leases(tenant_id,customer_key,action_id,lease_token,lease_until)
   VALUES(a.tenant_id,v_aggregate,a.id,v_token,now()+interval '30 seconds')
   ON CONFLICT ON CONSTRAINT crm_aggregate_leases_pkey DO UPDATE SET action_id=excluded.action_id,
    lease_token=excluded.lease_token,lease_until=excluded.lease_until
   WHERE public.crm_aggregate_leases.lease_until<now() RETURNING lease_token INTO v_lease;
  IF v_lease IS NULL THEN RETURN; END IF;
 END IF;
 UPDATE public.business_actions SET status='running',generation=generation+1,
  attempts=attempts+1,budget_started_at=CASE WHEN attempts=0 THEN now() ELSE budget_started_at END,
  lease_token=v_token,lease_until=now()+interval '30 seconds',queued_at=NULL,updated_at=now()
  WHERE id=a.id RETURNING generation INTO fence_generation;
 INSERT INTO public.business_attempts(tenant_id,action_id,attempt_number,effect_key,
  connection_generation,fence_generation,lease_token)
 SELECT b.tenant_id,b.id,b.attempts,b.effect_key,v_connection_generation,b.generation,v_token
 FROM public.business_actions b WHERE b.id=a.id RETURNING id INTO v_attempt;
 tenant_id:=a.tenant_id; action_id:=a.id; fence_token:=v_token; attempt_id:=v_attempt;
 RETURN NEXT;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_business_action_pointer(uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_business_action_pointer(uuid,uuid,bigint) TO relaydesk_runtime;

-- Repair never claims an external effect. It only restores a dispatch
-- obligation, or stops an ambiguous Slack effect at unknown.
-- +goose StatementBegin
CREATE FUNCTION repair_business_actions(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; n integer:=0; v_started timestamptz; v_status text;
BEGIN
 FOR a IN SELECT b.id,b.tenant_id,b.kind,b.status,b.lease_token,b.generation,b.dispatch_generation
  FROM public.business_actions b
  WHERE (b.status='running' AND b.lease_until<now())
    OR (b.status='queued' AND b.queued_at<now()-interval '45 seconds')
  ORDER BY b.updated_at,b.id FOR UPDATE OF b SKIP LOCKED LIMIT least(greatest(p_limit,0),100)
 LOOP
  IF a.status='running' THEN
   SELECT ba.request_started_at INTO v_started FROM public.business_attempts ba
    WHERE ba.tenant_id=a.tenant_id AND ba.action_id=a.id AND ba.fence_generation=a.generation
    ORDER BY ba.started_at DESC LIMIT 1;
   v_status:=CASE WHEN a.kind='slack_invoice_paid' AND v_started IS NOT NULL THEN 'unknown' ELSE 'pending' END;
   UPDATE public.business_actions SET status=v_status,lease_token=NULL,lease_until=NULL,
    error_class=CASE WHEN v_status='unknown' THEN 'slack_result_unknown' ELSE 'lease_expired_reconcile' END,
    updated_at=now() WHERE id=a.id;
   UPDATE public.business_attempts SET outcome=CASE WHEN v_status='unknown' THEN 'unknown' ELSE 'lease_expired' END,
    finished_at=now() WHERE tenant_id=a.tenant_id AND action_id=a.id AND fence_generation=a.generation
    AND finished_at IS NULL;
   DELETE FROM public.crm_aggregate_leases WHERE tenant_id=a.tenant_id AND action_id=a.id
    AND lease_token=a.lease_token;
   PERFORM public.refresh_business_run_status(a.tenant_id,a.id);
  ELSE
   UPDATE public.business_action_outbox SET superseded_at=now(),lease_token=NULL,lease_until=NULL
    WHERE tenant_id=a.tenant_id AND action_id=a.id AND dispatch_generation=a.dispatch_generation
      AND superseded_at IS NULL;
   UPDATE public.business_actions SET dispatch_generation=dispatch_generation+1,queued_at=now(),updated_at=now()
    WHERE id=a.id RETURNING dispatch_generation INTO a.dispatch_generation;
   INSERT INTO public.business_action_outbox(tenant_id,action_id,dispatch_generation)
    VALUES(a.tenant_id,a.id,a.dispatch_generation);
  END IF;
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION repair_business_actions(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION repair_business_actions(integer) TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business dispatch migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
