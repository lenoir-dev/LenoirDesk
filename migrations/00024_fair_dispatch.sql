-- +goose Up
CREATE INDEX business_actions_tenant_dispatch ON business_actions(tenant_id,status,created_at,id)
 WHERE status IN ('pending','retry_wait','queued','running');

-- The first eligible action from each tenant is considered before the second.
-- Keep only four unacknowledged pointers per tenant so JetStream's global
-- MaxAckPending cannot be filled by one tenant's deep backlog.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION queue_business_actions(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; n integer:=0; v_generation bigint;
BEGIN
 FOR a IN
  SELECT locked.id,locked.tenant_id FROM (
   SELECT b.id,b.tenant_id,b.created_at,
    row_number() OVER(PARTITION BY b.tenant_id ORDER BY b.created_at,b.id) AS ordinal
   FROM public.business_actions b
   WHERE (b.status='pending' OR (b.status='retry_wait' AND b.next_attempt_at<=now()))
    AND NOT EXISTS (SELECT 1 FROM public.automation_gates g WHERE g.tenant_id=b.tenant_id
     AND g.stripe_connection_id=b.stripe_connection_id AND g.paused)
    AND (b.depends_on IS NULL OR EXISTS (SELECT 1 FROM public.business_actions d
     WHERE d.tenant_id=b.tenant_id AND d.id=b.depends_on AND d.status='succeeded'))
  ) ranked JOIN public.business_actions locked ON locked.id=ranked.id
  ORDER BY ranked.ordinal,ranked.created_at,ranked.id
  FOR UPDATE OF locked SKIP LOCKED LIMIT least(greatest(p_limit,0),16)
 LOOP
  PERFORM pg_advisory_xact_lock(hashtextextended(a.tenant_id::text,41905));
  IF (SELECT count(*) FROM public.business_actions q WHERE q.tenant_id=a.tenant_id AND q.status='queued')>=4 THEN
   CONTINUE;
  END IF;
  UPDATE public.business_actions SET status='queued',dispatch_generation=dispatch_generation+1,
   queued_at=now(),next_attempt_at=NULL,updated_at=now() WHERE id=a.id RETURNING dispatch_generation INTO v_generation;
  INSERT INTO public.business_action_outbox(tenant_id,action_id,dispatch_generation)
   VALUES(a.tenant_id,a.id,v_generation);
  PERFORM public.refresh_business_run_status(a.tenant_id,a.id);
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;

CREATE OR REPLACE FUNCTION claim_business_action_pointer(p_tenant uuid,p_action uuid,p_dispatch bigint)
RETURNS TABLE(tenant_id uuid,action_id uuid,fence_token uuid,fence_generation bigint,attempt_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; v_token uuid; v_target uuid; v_connection_generation bigint;
 v_aggregate text; v_lease uuid; v_attempt uuid;
BEGIN
 SELECT b.* INTO a FROM public.business_actions b WHERE b.tenant_id=p_tenant AND b.id=p_action FOR UPDATE;
 IF NOT FOUND OR a.status<>'queued' OR a.dispatch_generation<>p_dispatch THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(a.tenant_id::text,41905));
 IF (SELECT count(*) FROM public.business_actions active
  WHERE active.tenant_id=a.tenant_id AND active.status='running' AND active.lease_until>now())>=2 THEN RETURN; END IF;
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

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'fair dispatch migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
