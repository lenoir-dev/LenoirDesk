-- +goose Up
-- Discovered events use the same inbox/planner but never authorize historical
-- invoice notifications. A later webhook replay keeps the first source.
ALTER TABLE webhook_events ADD COLUMN source text NOT NULL DEFAULT 'webhook'
 CHECK (source IN ('webhook','event_discovery','known_state','historical'));
GRANT SELECT (source) ON webhook_events TO relaydesk_app;

CREATE TABLE reconcile_checkpoints (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 stripe_connection_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('events','known')),
 window_start timestamptz NOT NULL DEFAULT now()-interval '15 minutes',
 window_end timestamptz NOT NULL DEFAULT now()-interval '2 minutes',
 cursor text NOT NULL DEFAULT '',
 last_complete_at timestamptz,
 last_attempt_at timestamptz,
 next_run_at timestamptz NOT NULL DEFAULT now(),
 lease_token uuid,
 lease_until timestamptz,
 gap_code text,
 gap_at timestamptz,
 scanned_count bigint NOT NULL DEFAULT 0,
 discovered_count bigint NOT NULL DEFAULT 0,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,stripe_connection_id,kind),
 FOREIGN KEY (tenant_id,stripe_connection_id) REFERENCES connections(tenant_id,id),
 CHECK (window_start < window_end),
 CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);
CREATE INDEX reconcile_due ON reconcile_checkpoints(next_run_at,updated_at)
 WHERE lease_token IS NULL;
ALTER TABLE reconcile_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconcile_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY reconcile_runtime ON reconcile_checkpoints TO relaydesk_runtime
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY reconcile_app ON reconcile_checkpoints TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);
CREATE POLICY reconcile_migrate ON reconcile_checkpoints TO relaydesk_migrate USING (true) WITH CHECK (true);
GRANT SELECT,UPDATE ON reconcile_checkpoints TO relaydesk_runtime;
GRANT SELECT ON reconcile_checkpoints TO relaydesk_app;

-- The runtime role has no global SELECT on connections. This bounded definer
-- allocates one fenced page across active test-mode automations, fairly.
-- +goose StatementBegin
CREATE FUNCTION claim_reconcile_page()
RETURNS TABLE(tenant_id uuid,stripe_connection_id uuid,kind text,window_start timestamptz,
 window_end timestamptz,cursor text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 INSERT INTO public.reconcile_checkpoints(tenant_id,stripe_connection_id,kind)
 SELECT c.tenant_id,c.id,k.kind FROM public.connections c
 JOIN public.automation_revisions r ON r.tenant_id=c.tenant_id AND r.connection_id=c.id AND r.status='active'
 CROSS JOIN (VALUES ('events'),('known')) AS k(kind)
 WHERE c.provider='stripe' AND c.mode='test' AND c.status='active'
 AND c.execution_environment IN ('simulation','test')
 ON CONFLICT DO NOTHING;
 RETURN QUERY
 WITH picked AS (
  SELECT cp.tenant_id,cp.stripe_connection_id,cp.kind
  FROM public.reconcile_checkpoints cp
  JOIN public.connections c ON c.tenant_id=cp.tenant_id AND c.id=cp.stripe_connection_id
  WHERE cp.next_run_at<=now() AND (cp.lease_until IS NULL OR cp.lease_until<now())
   AND c.status='active' AND c.mode='test'
   AND EXISTS (SELECT 1 FROM public.automation_revisions r
    WHERE r.tenant_id=c.tenant_id AND r.connection_id=c.id AND r.status='active')
  ORDER BY cp.next_run_at,cp.updated_at LIMIT 1 FOR UPDATE OF cp SKIP LOCKED
 ), leased AS (
  UPDATE public.reconcile_checkpoints cp SET lease_token=gen_random_uuid(),
   lease_until=now()+interval '2 minutes',last_attempt_at=now(),updated_at=now()
  FROM picked p WHERE cp.tenant_id=p.tenant_id AND cp.stripe_connection_id=p.stripe_connection_id AND cp.kind=p.kind
  RETURNING cp.tenant_id,cp.stripe_connection_id,cp.kind,cp.window_start,cp.window_end,cp.cursor,cp.lease_token
 ) SELECT l.tenant_id,l.stripe_connection_id,l.kind,l.window_start,l.window_end,l.cursor,l.lease_token FROM leased l;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_reconcile_page() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_reconcile_page() TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business reconciliation migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
