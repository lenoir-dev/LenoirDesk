-- +goose Up
CREATE TABLE business_provider_cooldowns (
 tenant_id uuid NOT NULL REFERENCES tenants(id), connection_id uuid NOT NULL,
 cooldown_until timestamptz NOT NULL, error_class text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant_id,connection_id),
 FOREIGN KEY (tenant_id,connection_id) REFERENCES connections(tenant_id,id)
);
ALTER TABLE business_provider_cooldowns ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_provider_cooldowns FORCE ROW LEVEL SECURITY;
CREATE POLICY runtime_tenant ON business_provider_cooldowns TO relaydesk_runtime
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY migration_admin ON business_provider_cooldowns TO relaydesk_migrate USING (true) WITH CHECK (true);
GRANT SELECT,INSERT,UPDATE ON business_provider_cooldowns TO relaydesk_runtime;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION queue_business_actions(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a record; n integer:=0; v_generation bigint;
BEGIN
 FOR a IN SELECT b.id,b.tenant_id FROM public.business_actions b
  WHERE (b.status='pending' OR (b.status='retry_wait' AND b.next_attempt_at<=now()))
   AND NOT EXISTS (SELECT 1 FROM public.automation_gates g WHERE g.tenant_id=b.tenant_id
    AND g.stripe_connection_id=b.stripe_connection_id AND g.paused)
   AND NOT EXISTS (SELECT 1 FROM public.business_provider_cooldowns cd
     JOIN public.automation_revisions ar ON ar.tenant_id=b.tenant_id AND ar.id=b.automation_revision_id
     WHERE cd.tenant_id=b.tenant_id AND cd.cooldown_until>now() AND
      (cd.connection_id=b.stripe_connection_id OR cd.connection_id::text=CASE WHEN b.kind='crm_projection'
       THEN ar.config->>'hubspot_connection_id' ELSE ar.config->>'slack_connection_id' END))
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION claim_business_action_pointer(p_tenant uuid,p_action uuid,p_dispatch bigint)
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
 IF EXISTS (SELECT 1 FROM public.business_provider_cooldowns cd WHERE cd.tenant_id=a.tenant_id
  AND cd.connection_id IN (a.stripe_connection_id,v_target) AND cd.cooldown_until>now()) THEN RETURN; END IF;
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
DO $$ BEGIN RAISE EXCEPTION 'provider cooldown migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
