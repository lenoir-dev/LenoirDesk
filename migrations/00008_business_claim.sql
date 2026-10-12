-- +goose Up
-- Global scheduler entry point. The runtime login has no cross-tenant table read;
-- this definer function returns only a fenced action pointer and attempt ID.
-- +goose StatementBegin
CREATE FUNCTION claim_business_action()
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
     attempts=attempts+1,lease_token=v_token,lease_until=now()+interval '30 seconds',
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
REVOKE ALL ON FUNCTION claim_business_action() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_business_action() TO relaydesk_runtime;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business claim migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
