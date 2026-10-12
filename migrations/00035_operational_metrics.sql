-- +goose Up
-- Cross-tenant monitoring exposes only aggregate counts and ages. No tenant,
-- customer, external object, token or action identifier is returned.
-- +goose StatementBegin
CREATE FUNCTION operational_metrics()
RETURNS TABLE(oldest_dead_seconds bigint,oldest_pending_job_seconds bigint,
 coverage_gaps bigint,oldest_reconcile_lag_seconds bigint,
 unhealthy_connections bigint,storage_bytes bigint,
 succeeded_actions_24h bigint,failed_actions_24h bigint)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT
  (SELECT coalesce(extract(epoch from now()-min(updated_at))::bigint,0)
   FROM public.business_actions WHERE status='dead'),
  (SELECT coalesce(extract(epoch from now()-min(created_at))::bigint,0)
   FROM public.planning_jobs WHERE status='pending'),
  (SELECT count(*) FROM public.reconcile_checkpoints WHERE gap_code IS NOT NULL),
  (SELECT coalesce(extract(epoch from now()-min(coalesce(last_complete_at,updated_at)))::bigint,0)
   FROM public.reconcile_checkpoints WHERE kind='events'),
  (SELECT count(*) FROM public.connections WHERE status='active' AND health_status<>'verified'),
  pg_database_size(current_database()),
  (SELECT count(*) FROM public.business_actions WHERE status='succeeded' AND updated_at>now()-interval '24 hours'),
  (SELECT count(*) FROM public.business_actions WHERE status IN ('dead','unknown','needs_attention')
   AND updated_at>now()-interval '24 hours');
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION operational_metrics() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION operational_metrics() TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'operational metrics migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
