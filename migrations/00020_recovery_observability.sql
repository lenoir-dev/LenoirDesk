-- +goose Up
-- Aggregate-only operational counters. The runtime role cannot scan cross-tenant
-- rows directly; this function never returns tenant or provider identifiers.
-- +goose StatementBegin
CREATE FUNCTION business_recovery_health()
RETURNS TABLE(due_actions bigint,oldest_due_seconds bigint,unpublished_outbox bigint,
 oldest_unpublished_seconds bigint,active_leases bigint,expired_leases bigint,
 stalled_actions bigint,dead_actions bigint,unknown_actions bigint,oldest_unknown_seconds bigint,
 retries_24h bigint)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT
  (SELECT count(*) FROM public.business_actions WHERE status='retry_wait' AND next_attempt_at<=now()),
  (SELECT coalesce(extract(epoch from now()-min(next_attempt_at))::bigint,0)
   FROM public.business_actions WHERE status='retry_wait' AND next_attempt_at<=now()),
  (SELECT count(*) FROM public.business_action_outbox WHERE published_at IS NULL AND superseded_at IS NULL),
  (SELECT coalesce(extract(epoch from now()-min(created_at))::bigint,0)
   FROM public.business_action_outbox WHERE published_at IS NULL AND superseded_at IS NULL),
  (SELECT count(*) FROM public.business_actions WHERE status='running' AND lease_until>now()),
  (SELECT count(*) FROM public.business_actions WHERE status='running' AND lease_until<=now()),
  (SELECT count(*) FROM public.business_actions WHERE status='queued' AND queued_at<now()-interval '45 seconds'),
  (SELECT count(*) FROM public.business_actions WHERE status='dead'),
  (SELECT count(*) FROM public.business_actions WHERE status='unknown'),
  (SELECT coalesce(extract(epoch from now()-min(updated_at))::bigint,0)
   FROM public.business_actions WHERE status='unknown'),
  (SELECT count(*) FROM public.business_attempts WHERE outcome='retry_wait' AND finished_at>now()-interval '24 hours');
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION business_recovery_health() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION business_recovery_health() TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recovery observability migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
