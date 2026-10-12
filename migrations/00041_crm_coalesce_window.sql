-- +goose Up
-- Stripe can deliver many distinct events for one Customer in a short burst.
-- Give a newly created CRM projection one fixed second to gather accepted
-- events before its first provider snapshot. Its created_at is durable and
-- never extended by later events, so continuous traffic cannot starve it.
-- Slack actions and operator retries are not delayed by this window.
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
   WHERE ((b.status='pending' AND
            (b.kind<>'crm_projection' OR b.created_at<=now()-interval '1 second'))
       OR (b.status='retry_wait' AND b.next_attempt_at<=now()))
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
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'CRM coalescing-window migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
