-- +goose Up
CREATE INDEX webhook_events_redaction_due ON webhook_events(received_at,id) WHERE raw_payload<>''::bytea;
CREATE INDEX business_attempts_retention_due ON business_attempts(finished_at,id) WHERE finished_at IS NOT NULL;
CREATE INDEX business_provider_calls_retention_due ON business_provider_calls(finished_at,id) WHERE finished_at IS NOT NULL;
CREATE INDEX runs_retention_due ON runs(finished_at,id) WHERE finished_at IS NOT NULL;

-- All selections are bounded and SKIP LOCKED. The function does not remove
-- inbox event identities, business effect keys, unfinished actions or their
-- sole durable planning obligation. A failed transaction removes nothing.
-- +goose StatementBegin
CREATE FUNCTION retention_batch(p_limit integer,p_raw_days integer,p_history_days integer,p_audit_days integer)
RETURNS TABLE(raw_redacted integer,provider_calls_removed integer,attempts_removed integer,
 runs_removed integer,audit_removed integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_run record; v_count integer;
BEGIN
 IF p_limit<1 OR p_limit>500 OR p_raw_days<7 OR p_history_days<30 OR p_audit_days<90 THEN
  RAISE EXCEPTION 'unsafe retention settings';
 END IF;
 raw_redacted:=0; provider_calls_removed:=0; attempts_removed:=0; runs_removed:=0; audit_removed:=0;
 WITH due AS (
  SELECT e.tenant_id,e.id FROM public.webhook_events e
  WHERE e.received_at<now()-make_interval(days=>p_raw_days) AND e.raw_payload<>''::bytea
   AND NOT EXISTS (SELECT 1 FROM public.planning_jobs j WHERE j.tenant_id=e.tenant_id
    AND j.event_row_id=e.id AND j.status<>'planned')
  ORDER BY e.received_at,e.id LIMIT p_limit FOR UPDATE OF e SKIP LOCKED
 ) UPDATE public.webhook_events e SET raw_payload=''::bytea FROM due d
 WHERE e.tenant_id=d.tenant_id AND e.id=d.id;
 GET DIAGNOSTICS raw_redacted=ROW_COUNT;
 WITH due AS (
  SELECT c.tenant_id,c.id FROM public.business_provider_calls c
  JOIN public.business_attempts at ON at.tenant_id=c.tenant_id AND at.id=c.attempt_id
  JOIN public.business_actions a ON a.tenant_id=at.tenant_id AND a.id=at.action_id
  WHERE c.finished_at<now()-make_interval(days=>p_history_days)
   AND a.status IN ('succeeded','canceled','superseded')
  ORDER BY c.finished_at,c.id LIMIT p_limit FOR UPDATE OF c SKIP LOCKED
 ) DELETE FROM public.business_provider_calls c USING due d
 WHERE c.tenant_id=d.tenant_id AND c.id=d.id;
 GET DIAGNOSTICS provider_calls_removed=ROW_COUNT;
 WITH due AS (
  SELECT at.tenant_id,at.id FROM public.business_attempts at
  JOIN public.business_actions a ON a.tenant_id=at.tenant_id AND a.id=at.action_id
  WHERE at.finished_at<now()-make_interval(days=>p_history_days)
   AND a.status IN ('succeeded','canceled','superseded')
   AND NOT EXISTS (SELECT 1 FROM public.business_provider_calls c
    WHERE c.tenant_id=at.tenant_id AND c.attempt_id=at.id)
  ORDER BY at.finished_at,at.id LIMIT p_limit FOR UPDATE OF at SKIP LOCKED
 ) DELETE FROM public.business_attempts at USING due d
 WHERE at.tenant_id=d.tenant_id AND at.id=d.id;
 GET DIAGNOSTICS attempts_removed=ROW_COUNT;
 FOR v_run IN
  SELECT r.tenant_id,r.id,r.planning_job_id FROM public.runs r
  WHERE r.finished_at<now()-make_interval(days=>p_history_days)
   AND r.status IN ('succeeded','skipped','canceled','superseded')
   AND NOT EXISTS (SELECT 1 FROM public.business_run_actions ra
    JOIN public.business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id
    WHERE ra.tenant_id=r.tenant_id AND ra.run_id=r.id
     AND a.status NOT IN ('succeeded','canceled','superseded'))
   AND NOT EXISTS (SELECT 1 FROM public.outbox o WHERE o.tenant_id=r.tenant_id
    AND o.job_id=r.planning_job_id AND o.published_at IS NULL)
  ORDER BY r.finished_at,r.id LIMIT p_limit FOR UPDATE OF r SKIP LOCKED
 LOOP
  DELETE FROM public.business_run_actions ra WHERE ra.tenant_id=v_run.tenant_id AND ra.run_id=v_run.id;
  DELETE FROM public.runs r WHERE r.tenant_id=v_run.tenant_id AND r.id=v_run.id;
  GET DIAGNOSTICS v_count=ROW_COUNT;
  runs_removed:=runs_removed+v_count;
  DELETE FROM public.outbox o WHERE o.tenant_id=v_run.tenant_id AND o.job_id=v_run.planning_job_id;
  DELETE FROM public.planning_jobs j WHERE j.tenant_id=v_run.tenant_id AND j.id=v_run.planning_job_id;
 END LOOP;
 WITH due AS (
  SELECT l.tenant_id,l.id FROM public.audit_log l
  WHERE l.created_at<now()-make_interval(days=>p_audit_days)
  ORDER BY l.created_at,l.id LIMIT p_limit FOR UPDATE OF l SKIP LOCKED
 ) DELETE FROM public.audit_log l USING due d WHERE l.tenant_id=d.tenant_id AND l.id=d.id;
 GET DIAGNOSTICS audit_removed=ROW_COUNT;
 RETURN NEXT;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION retention_batch(integer,integer,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION retention_batch(integer,integer,integer,integer) TO relaydesk_runtime;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'retention migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
