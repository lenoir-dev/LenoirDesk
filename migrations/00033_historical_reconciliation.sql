-- +goose Up
CREATE TABLE reconcile_requests (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 stripe_connection_id uuid NOT NULL,
 actor_user_id uuid NOT NULL REFERENCES users(id),
 from_at timestamptz NOT NULL,
 to_at timestamptz NOT NULL,
 max_events integer NOT NULL CHECK (max_events BETWEEN 1 AND 500),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 12 AND 500),
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed','blocked','limit_reached')),
 cursor text NOT NULL DEFAULT '',
 scanned_count integer NOT NULL DEFAULT 0,
 discovered_count integer NOT NULL DEFAULT 0,
 gap_code text,
 lease_token uuid,
 lease_until timestamptz,
 next_run_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE (tenant_id,id),
 FOREIGN KEY (tenant_id,stripe_connection_id) REFERENCES connections(tenant_id,id),
 CHECK (from_at<to_at),
 CHECK ((lease_token IS NULL)=(lease_until IS NULL))
);
CREATE INDEX reconcile_requests_due ON reconcile_requests(next_run_at,created_at)
 WHERE status='pending' AND lease_token IS NULL;
ALTER TABLE reconcile_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconcile_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY reconcile_request_runtime ON reconcile_requests TO relaydesk_runtime
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY reconcile_request_app ON reconcile_requests TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);
CREATE POLICY reconcile_request_migrate ON reconcile_requests TO relaydesk_migrate USING (true) WITH CHECK (true);
GRANT SELECT,UPDATE ON reconcile_requests TO relaydesk_runtime;
GRANT SELECT ON reconcile_requests TO relaydesk_app;

-- +goose StatementBegin
CREATE FUNCTION request_historical_reconcile(p_session bytea,p_tenant uuid,p_connection uuid,
 p_from timestamptz,p_to timestamptz,p_max integer,p_reason text,p_correlation text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_actor uuid; v_id uuid;
BEGIN
 v_actor:=public.tenant_authorize(p_session,p_tenant,'operate');
 IF v_actor IS NULL THEN RAISE EXCEPTION 'reconciliation denied'; END IF;
 IF p_from IS NULL OR p_to IS NULL OR p_from>=p_to OR p_from<now()-interval '29 days'
    OR p_to>now()-interval '2 minutes' OR p_max<1 OR p_max>500
    OR length(btrim(p_reason)) NOT BETWEEN 12 AND 500 OR length(p_correlation)>128 THEN
  RAISE EXCEPTION 'invalid historical reconciliation bounds';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM public.connections c WHERE c.tenant_id=p_tenant AND c.id=p_connection
  AND c.provider='stripe' AND c.mode='test' AND c.status='active'
  AND c.execution_environment IN ('simulation','test') AND c.health_status='verified') THEN
  RAISE EXCEPTION 'Stripe connection unavailable';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM public.automation_revisions r WHERE r.tenant_id=p_tenant
  AND r.connection_id=p_connection AND r.status='active' AND r.template_key='subscription_billing_sync.v1') THEN
  RAISE EXCEPTION 'automation unavailable';
 END IF;
 INSERT INTO public.reconcile_requests(tenant_id,stripe_connection_id,actor_user_id,from_at,to_at,max_events,reason)
 VALUES(p_tenant,p_connection,v_actor,p_from,p_to,p_max,btrim(p_reason)) RETURNING id INTO v_id;
 INSERT INTO public.audit_log(tenant_id,actor_user_id,operation,target_type,target_id,result,correlation_id,context)
 VALUES(p_tenant,v_actor,'reconciliation.request','reconciliation',v_id,'success',p_correlation,
  jsonb_build_object('from',p_from,'to',p_to,'max_events',p_max,'reason',btrim(p_reason)));
 RETURN v_id;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION request_historical_reconcile(bytea,uuid,uuid,timestamptz,timestamptz,integer,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION request_historical_reconcile(bytea,uuid,uuid,timestamptz,timestamptz,integer,text,text) TO relaydesk_app;

-- +goose StatementBegin
CREATE FUNCTION claim_historical_reconcile()
RETURNS TABLE(id uuid,tenant_id uuid,stripe_connection_id uuid,from_at timestamptz,to_at timestamptz,
 cursor text,max_events integer,scanned_count integer,lease_token uuid)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 WITH picked AS (
  SELECT q.id FROM public.reconcile_requests q
  WHERE q.status='pending' AND q.next_run_at<=now() AND (q.lease_until IS NULL OR q.lease_until<now())
  ORDER BY q.next_run_at,q.created_at LIMIT 1 FOR UPDATE SKIP LOCKED
 ), leased AS (
  UPDATE public.reconcile_requests q SET lease_token=gen_random_uuid(),lease_until=now()+interval '5 minutes'
  FROM picked p WHERE q.id=p.id
  RETURNING q.id,q.tenant_id,q.stripe_connection_id,q.from_at,q.to_at,q.cursor,q.max_events,q.scanned_count,q.lease_token
 ) SELECT l.* FROM leased l;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_historical_reconcile() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_historical_reconcile() TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'historical reconciliation migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
