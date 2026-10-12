-- +goose Up
ALTER TABLE connections ADD COLUMN execution_environment text NOT NULL DEFAULT 'unverified'
  CHECK (execution_environment IN ('unverified','simulation','test','production'));
ALTER TABLE automation_revisions ADD COLUMN template_key text NOT NULL DEFAULT 'legacy_planning';
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK
  (status IN ('pending','running','succeeded','retry_wait','blocked_auth','blocked_config','needs_attention','unknown','dead','paused','skipped'));
ALTER TABLE runs ADD COLUMN finished_at timestamptz;
ALTER TABLE connection_credentials DROP CONSTRAINT connection_credentials_secret_kind_check;
ALTER TABLE connection_credentials ADD CONSTRAINT connection_credentials_secret_kind_check
  CHECK (secret_kind IN ('webhook_signing','api_token'));

CREATE TABLE automation_gates (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  stripe_connection_id uuid NOT NULL,
  paused boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id,stripe_connection_id),
  FOREIGN KEY (tenant_id,stripe_connection_id) REFERENCES connections(tenant_id,id)
);
CREATE TABLE business_actions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  automation_revision_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind IN ('crm_projection','slack_invoice_paid')),
  effect_key text NOT NULL,
  stripe_connection_id uuid NOT NULL,
  customer_id text NOT NULL,
  invoice_id text NOT NULL DEFAULT '',
  depends_on uuid,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN
    ('pending','queued','running','succeeded','retry_wait','blocked_auth','blocked_config','needs_attention','unknown','dead','paused')),
  generation bigint NOT NULL DEFAULT 1,
  attempts integer NOT NULL DEFAULT 0,
  lease_token uuid,
  lease_until timestamptz,
  next_attempt_at timestamptz,
  error_class text,
  provider_ref text,
  contact_id text,
  slack_channel text,
  slack_ts text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id,id),
  UNIQUE (tenant_id,effect_key),
  FOREIGN KEY (tenant_id,automation_revision_id) REFERENCES automation_revisions(tenant_id,id),
  FOREIGN KEY (tenant_id,stripe_connection_id) REFERENCES connections(tenant_id,id),
  FOREIGN KEY (tenant_id,depends_on) REFERENCES business_actions(tenant_id,id),
  CHECK ((kind='crm_projection' AND invoice_id='') OR (kind='slack_invoice_paid' AND invoice_id<>''))
);
CREATE INDEX business_actions_runnable ON business_actions(status,next_attempt_at,created_at)
  WHERE status IN ('pending','retry_wait','running');
CREATE TABLE business_run_actions (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  run_id uuid NOT NULL,
  action_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind IN ('crm_projection','slack_invoice_paid')),
  PRIMARY KEY (tenant_id,run_id,action_id),
  UNIQUE (tenant_id,run_id,kind),
  FOREIGN KEY (tenant_id,run_id) REFERENCES runs(tenant_id,id),
  FOREIGN KEY (tenant_id,action_id) REFERENCES business_actions(tenant_id,id)
);
CREATE TABLE business_attempts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  action_id uuid NOT NULL,
  attempt_number integer NOT NULL CHECK (attempt_number>0),
  effect_key text NOT NULL,
  connection_generation bigint NOT NULL CHECK (connection_generation>0),
  fence_generation bigint NOT NULL,
  lease_token uuid NOT NULL,
  request_started_at timestamptz,
  outcome text,
  provider_request_id text,
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  UNIQUE (tenant_id,id), UNIQUE (tenant_id,action_id,attempt_number),
  FOREIGN KEY (tenant_id,action_id) REFERENCES business_actions(tenant_id,id)
);
CREATE TABLE contact_links (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  customer_key text NOT NULL,
  hubspot_connection_id uuid NOT NULL,
  contact_id text NOT NULL,
  source text NOT NULL CHECK (source IN ('explicit','unique_key','upsert')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id,customer_key,hubspot_connection_id),
  UNIQUE (tenant_id,hubspot_connection_id,contact_id),
  FOREIGN KEY (tenant_id,hubspot_connection_id) REFERENCES connections(tenant_id,id)
);
CREATE TABLE crm_aggregate_leases (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  customer_key text NOT NULL,
  action_id uuid NOT NULL,
  lease_token uuid NOT NULL,
  lease_until timestamptz NOT NULL,
  PRIMARY KEY (tenant_id,customer_key),
  FOREIGN KEY (tenant_id,action_id) REFERENCES business_actions(tenant_id,id)
);

-- +goose StatementBegin
DO $$ DECLARE tab text; BEGIN
  FOREACH tab IN ARRAY ARRAY['automation_gates','business_actions','business_run_actions','business_attempts','contact_links','crm_aggregate_leases'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',tab);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',tab);
    EXECUTE format('CREATE POLICY runtime_tenant ON %I TO relaydesk_runtime USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',tab);
    EXECUTE format('CREATE POLICY migration_admin ON %I TO relaydesk_migrate USING (true) WITH CHECK (true)',tab);
  END LOOP;
END $$;
-- +goose StatementEnd
GRANT SELECT,INSERT,UPDATE ON business_actions,business_run_actions,business_attempts,contact_links,crm_aggregate_leases TO relaydesk_runtime;
GRANT DELETE ON crm_aggregate_leases TO relaydesk_runtime;
GRANT SELECT ON automation_gates TO relaydesk_runtime;
GRANT UPDATE (status,finished_at) ON runs TO relaydesk_runtime;
GRANT SELECT (execution_environment) ON connections TO relaydesk_runtime;
GRANT SELECT (template_key) ON automation_revisions TO relaydesk_runtime;
GRANT SELECT ON business_actions,business_run_actions,business_attempts,automation_revisions,automation_gates TO relaydesk_app;
GRANT SELECT (execution_environment) ON connections TO relaydesk_app;
-- +goose StatementBegin
DO $$ DECLARE tab text; BEGIN
  FOREACH tab IN ARRAY ARRAY['business_actions','business_run_actions','business_attempts','automation_revisions','automation_gates'] LOOP
    EXECUTE format('CREATE POLICY browser_member ON %I TO relaydesk_app USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid AND public.tenant_authorize(decode(coalesce(current_setting(''app.session_hash'',true),''''),''hex''),tenant_id,''read'') IS NOT NULL)',tab);
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION immutable_automation_config() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.connection_id IS DISTINCT FROM OLD.connection_id
 OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.activated_at IS DISTINCT FROM OLD.activated_at
 OR NEW.config IS DISTINCT FROM OLD.config OR NEW.template_key IS DISTINCT FROM OLD.template_key THEN
   RAISE EXCEPTION 'automation revision is immutable';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER automation_revision_immutable BEFORE UPDATE ON automation_revisions
 FOR EACH ROW EXECUTE FUNCTION immutable_automation_config();

-- Only a live owner/operator session can change the active revision or pause gate.
-- +goose StatementBegin
CREATE FUNCTION configure_billing_sync(p_session_hash bytea,p_tenant uuid,p_stripe uuid,p_config jsonb,p_activated timestamptz,p_correlation text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_revision uuid; v_number integer; v_hubspot uuid; v_slack uuid; v_environment text;
BEGIN
 IF public.tenant_authorize(p_session_hash,p_tenant,'operate') IS NULL THEN RAISE EXCEPTION 'configuration denied'; END IF;
 IF p_config->>'template'<>'subscription_billing_sync.v1' OR p_config->>'stripe_connection_id'<>p_stripe::text
    OR length(p_correlation)>128 OR p_activated IS NULL THEN RAISE EXCEPTION 'invalid configuration'; END IF;
 v_hubspot := (p_config->>'hubspot_connection_id')::uuid;
 v_slack := (p_config->>'slack_connection_id')::uuid;
 SELECT c.execution_environment INTO v_environment FROM public.connections c
  WHERE c.tenant_id=p_tenant AND c.id=p_stripe AND c.provider='stripe' AND c.mode='test' AND c.status='active' FOR UPDATE;
 IF v_environment IS NULL OR v_environment NOT IN ('simulation','test') THEN RAISE EXCEPTION 'Stripe connection not executable'; END IF;
 IF NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant AND c.id=v_hubspot
   AND c.provider='hubspot' AND c.status='active' AND c.execution_environment=v_environment) OR
   NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant AND c.id=v_slack
   AND c.provider='slack' AND c.status='active' AND c.execution_environment=v_environment) THEN
   RAISE EXCEPTION 'provider connections do not belong to this workspace and mode';
 END IF;
 UPDATE public.automation_revisions SET status='inactive' WHERE tenant_id=p_tenant AND connection_id=p_stripe AND status='active';
 SELECT coalesce(max(revision),0)+1 INTO v_number FROM public.automation_revisions WHERE tenant_id=p_tenant AND connection_id=p_stripe;
 INSERT INTO public.automation_revisions(tenant_id,connection_id,revision,status,activated_at,config,template_key)
 VALUES(p_tenant,p_stripe,v_number,'active',p_activated,p_config,'subscription_billing_sync.v1') RETURNING id INTO v_revision;
 INSERT INTO public.automation_gates(tenant_id,stripe_connection_id) VALUES(p_tenant,p_stripe) ON CONFLICT DO NOTHING;
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id)
 VALUES(p_tenant,public.auth_session_user(p_session_hash),'automation.configure','automation_revision',v_revision,'success',p_correlation);
 RETURN v_revision;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION set_billing_pause(p_session_hash bytea,p_tenant uuid,p_stripe uuid,p_paused boolean,p_correlation text)
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
REVOKE ALL ON FUNCTION configure_billing_sync(bytea,uuid,uuid,jsonb,timestamptz,text),set_billing_pause(bytea,uuid,uuid,boolean,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION configure_billing_sync(bytea,uuid,uuid,jsonb,timestamptz,text),set_billing_pause(bytea,uuid,uuid,boolean,text) TO relaydesk_app;

-- Credential bytes never receive a direct runtime table grant.
-- +goose StatementBegin
CREATE FUNCTION business_connection(p_tenant uuid,p_connection uuid,p_provider text)
RETURNS TABLE(account_id text,mode text,environment text,health text,generation bigint,ciphertext bytea,nonce bytea,key_version integer)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT c.provider_account_id,c.mode,c.execution_environment,c.health_status,c.generation,
   cc.ciphertext,cc.nonce,cc.key_version
 FROM public.connections c JOIN public.connection_credentials cc
 ON cc.tenant_id=c.tenant_id AND cc.connection_id=c.id AND cc.generation=c.generation
 AND cc.secret_kind='api_token' AND cc.revoked_at IS NULL
 WHERE c.tenant_id=p_tenant AND c.id=p_connection AND c.provider=p_provider AND c.status='active'
 AND c.execution_environment IN ('simulation','test')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION business_connection(uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION business_connection(uuid,uuid,text) TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business action migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
