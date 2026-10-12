-- +goose Up
CREATE TABLE tenants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_subject text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE memberships (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    user_id uuid NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('owner','operator','viewer')),
    UNIQUE (tenant_id,id), UNIQUE (tenant_id,user_id)
);
CREATE TABLE connections (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    provider text NOT NULL CHECK (provider IN ('stripe','hubspot','slack')),
    provider_account_id text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('test','live')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disconnected')),
    webhook_ref_hash bytea UNIQUE,
    signing_secret_ciphertext bytea,
    signing_secret_nonce bytea,
    key_version integer,
    generation bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id,id),
    UNIQUE (provider,provider_account_id,mode),
    CHECK ((provider = 'stripe' AND webhook_ref_hash IS NOT NULL AND signing_secret_ciphertext IS NOT NULL AND signing_secret_nonce IS NOT NULL AND key_version IS NOT NULL)
        OR (provider <> 'stripe' AND webhook_ref_hash IS NULL AND signing_secret_ciphertext IS NULL AND signing_secret_nonce IS NULL AND key_version IS NULL))
);
CREATE TABLE automation_revisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    connection_id uuid NOT NULL,
    revision integer NOT NULL CHECK (revision > 0),
    status text NOT NULL CHECK (status IN ('active','inactive')),
    activated_at timestamptz NOT NULL DEFAULT now(),
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (tenant_id,id),
    UNIQUE (tenant_id,connection_id,revision),
    FOREIGN KEY (tenant_id,connection_id) REFERENCES connections(tenant_id,id)
);
CREATE UNIQUE INDEX one_active_revision ON automation_revisions (tenant_id,connection_id) WHERE status = 'active';
CREATE TABLE webhook_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    connection_id uuid NOT NULL,
    provider text NOT NULL,
    provider_account_id text NOT NULL,
    mode text NOT NULL,
    event_id text NOT NULL,
    event_type text NOT NULL,
    event_api_version text,
    raw_payload bytea NOT NULL,
    classification text NOT NULL CHECK (classification IN ('supported','ignored')),
    reason text,
    automation_revision_id uuid,
    correlation_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id,id),
    UNIQUE (tenant_id,provider,provider_account_id,mode,event_id),
    FOREIGN KEY (tenant_id,connection_id) REFERENCES connections(tenant_id,id),
    FOREIGN KEY (tenant_id,automation_revision_id) REFERENCES automation_revisions(tenant_id,id),
    CHECK ((classification = 'supported' AND automation_revision_id IS NOT NULL) OR classification = 'ignored')
);
CREATE TABLE planning_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    event_row_id uuid NOT NULL,
    automation_revision_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','planned')),
    dispatch_generation bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    planned_at timestamptz,
    UNIQUE (tenant_id,id), UNIQUE (tenant_id,event_row_id),
    FOREIGN KEY (tenant_id,event_row_id) REFERENCES webhook_events(tenant_id,id),
    FOREIGN KEY (tenant_id,automation_revision_id) REFERENCES automation_revisions(tenant_id,id)
);
CREATE TABLE runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    planning_job_id uuid NOT NULL,
    automation_revision_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id,id), UNIQUE (tenant_id,planning_job_id),
    FOREIGN KEY (tenant_id,planning_job_id) REFERENCES planning_jobs(tenant_id,id),
    FOREIGN KEY (tenant_id,automation_revision_id) REFERENCES automation_revisions(tenant_id,id)
);
CREATE TABLE outbox (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    job_id uuid NOT NULL,
    dispatch_generation bigint NOT NULL,
    correlation_id text NOT NULL,
    traceparent text NOT NULL DEFAULT '',
    lease_token uuid,
    lease_until timestamptz,
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id,id), UNIQUE (tenant_id,job_id,dispatch_generation),
    FOREIGN KEY (tenant_id,job_id) REFERENCES planning_jobs(tenant_id,id)
);
CREATE INDEX outbox_pending ON outbox (created_at,id) WHERE published_at IS NULL;
CREATE INDEX planning_pending ON planning_jobs (created_at,id) WHERE status = 'pending';

-- +goose StatementBegin
DO $$
DECLARE tab text;
BEGIN
    FOREACH tab IN ARRAY ARRAY['tenants','memberships','connections','automation_revisions','webhook_events','planning_jobs','runs','outbox'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', tab);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', tab);
        IF tab = 'tenants' THEN
            EXECUTE format('CREATE POLICY runtime_tenant ON %I TO relaydesk_runtime USING (id = nullif(current_setting(''app.tenant_id'', true), '''')::uuid) WITH CHECK (id = nullif(current_setting(''app.tenant_id'', true), '''')::uuid)', tab);
        ELSE
            EXECUTE format('CREATE POLICY runtime_tenant ON %I TO relaydesk_runtime USING (tenant_id = nullif(current_setting(''app.tenant_id'', true), '''')::uuid) WITH CHECK (tenant_id = nullif(current_setting(''app.tenant_id'', true), '''')::uuid)', tab);
        END IF;
        EXECUTE format('CREATE POLICY migration_admin ON %I TO relaydesk_migrate USING (true) WITH CHECK (true)', tab);
    END LOOP;
END $$;
-- +goose StatementEnd
GRANT USAGE ON SCHEMA public TO relaydesk_runtime;
GRANT SELECT ON tenants,memberships,automation_revisions,webhook_events,planning_jobs,runs,outbox TO relaydesk_runtime;
GRANT INSERT ON webhook_events,planning_jobs,runs,outbox TO relaydesk_runtime;
GRANT UPDATE ON planning_jobs,outbox TO relaydesk_runtime;
GRANT SELECT (id,tenant_id,provider,provider_account_id,mode,status,generation,created_at) ON connections TO relaydesk_runtime;

-- The runtime role can execute these narrowly scoped functions, never arbitrary global SELECT.
-- +goose StatementBegin
CREATE FUNCTION lookup_stripe_connection(p_ref_hash bytea)
RETURNS TABLE(tenant_id uuid, connection_id uuid, account_id text, mode text, ciphertext bytea, nonce bytea, key_version integer)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  SELECT c.tenant_id,c.id,c.provider_account_id,c.mode,c.signing_secret_ciphertext,c.signing_secret_nonce,c.key_version
  FROM public.connections c WHERE c.provider='stripe' AND c.status='active' AND c.webhook_ref_hash=p_ref_hash
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION claim_outbox(p_limit integer, p_lease_seconds integer)
RETURNS TABLE(id uuid,tenant_id uuid,job_id uuid,dispatch_generation bigint,correlation_id text,traceparent text,lease_token uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  WITH selected AS (
    SELECT o.id FROM public.outbox o
    WHERE o.published_at IS NULL AND (o.lease_until IS NULL OR o.lease_until < now())
    ORDER BY o.created_at,o.id FOR UPDATE SKIP LOCKED LIMIT LEAST(GREATEST(p_limit,0),100)
  ), claimed AS (
    UPDATE public.outbox o SET lease_token=gen_random_uuid(),lease_until=now()+make_interval(secs=>LEAST(GREATEST(p_lease_seconds,1),300))
    FROM selected WHERE o.id=selected.id
    RETURNING o.id,o.tenant_id,o.job_id,o.dispatch_generation,o.correlation_id,o.traceparent,o.lease_token
  ) SELECT * FROM claimed
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION claim_lost_dispatch(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE changed integer;
BEGIN
  WITH selected AS (
    SELECT j.id,j.tenant_id,j.dispatch_generation FROM public.planning_jobs j
    WHERE j.status='pending' AND NOT EXISTS (
      SELECT 1 FROM public.outbox o WHERE o.tenant_id=j.tenant_id AND o.job_id=j.id AND o.dispatch_generation=j.dispatch_generation AND (o.published_at IS NULL OR o.published_at > now()-interval '30 seconds')
    ) AND j.created_at < now()-interval '30 seconds'
    ORDER BY j.created_at FOR UPDATE SKIP LOCKED LIMIT LEAST(GREATEST(p_limit,0),100)
  ), bumped AS (
    UPDATE public.planning_jobs j SET dispatch_generation=j.dispatch_generation+1 FROM selected s WHERE j.id=s.id
    RETURNING j.id,j.tenant_id,j.dispatch_generation
  )
  INSERT INTO public.outbox (tenant_id,job_id,dispatch_generation,correlation_id)
  SELECT b.tenant_id,b.id,b.dispatch_generation,e.correlation_id FROM bumped b
  JOIN public.planning_jobs j ON j.id=b.id JOIN public.webhook_events e ON e.id=j.event_row_id;
  GET DIAGNOSTICS changed = ROW_COUNT;
  RETURN changed;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION lookup_stripe_connection(bytea),claim_outbox(integer,integer),claim_lost_dispatch(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lookup_stripe_connection(bytea),claim_outbox(integer,integer),claim_lost_dispatch(integer) TO relaydesk_runtime;

-- +goose Down
DROP FUNCTION IF EXISTS claim_lost_dispatch(integer),claim_outbox(integer,integer),lookup_stripe_connection(bytea);
DROP TABLE IF EXISTS outbox,runs,planning_jobs,webhook_events,automation_revisions,connections,memberships,users,tenants CASCADE;
