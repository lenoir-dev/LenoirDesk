-- +goose Up
-- This gate is an additional barrier. The worker also requires a separate,
-- explicit deployment setting; the setting is deliberately not backed up.
CREATE TABLE restore_safety_gate (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 outbound_enabled boolean NOT NULL DEFAULT true,
 recovery_started_at timestamptz,
 recovery_cleared_at timestamptz,
 quarantined_notifications bigint NOT NULL DEFAULT 0,
 note text NOT NULL DEFAULT ''
);
INSERT INTO restore_safety_gate(singleton) VALUES(true);
GRANT SELECT ON restore_safety_gate TO relaydesk_runtime;

-- The restore operator calls this only after all workers have stopped. It is
-- deliberately not granted to the runtime or browser role.
-- +goose StatementBegin
CREATE FUNCTION enter_restore_recovery(p_note text)
RETURNS bigint LANGUAGE plpgsql SECURITY INVOKER AS $$
DECLARE v_count bigint;
BEGIN
 IF length(btrim(coalesce(p_note,''))) NOT BETWEEN 12 AND 500 THEN
  RAISE EXCEPTION 'restore note required';
 END IF;
 UPDATE restore_safety_gate SET outbound_enabled=false,recovery_started_at=now(),
  recovery_cleared_at=NULL,note=btrim(p_note) WHERE singleton;
 WITH affected AS (
  UPDATE business_actions SET status='unknown',error_class='restore_uncertain',
   lease_token=NULL,lease_until=NULL,queued_at=NULL,next_attempt_at=NULL,
   generation=generation+1,dispatch_generation=dispatch_generation+1,updated_at=now()
  WHERE kind='slack_invoice_paid' AND status IN
   ('pending','queued','running','retry_wait','blocked_auth','blocked_config','paused','needs_attention')
  RETURNING tenant_id,id
 ), obsolete AS (
  UPDATE business_action_outbox o SET superseded_at=now(),lease_token=NULL,lease_until=NULL
  FROM affected a WHERE o.tenant_id=a.tenant_id AND o.action_id=a.id AND o.superseded_at IS NULL
  RETURNING o.id
 ) SELECT count(*) INTO v_count FROM affected;
 UPDATE restore_safety_gate SET quarantined_notifications=quarantined_notifications+v_count WHERE singleton;
 UPDATE runs r SET status='unknown',finished_at=NULL
 WHERE EXISTS (SELECT 1 FROM business_run_actions l JOIN business_actions a
  ON a.tenant_id=l.tenant_id AND a.id=l.action_id
  WHERE l.tenant_id=r.tenant_id AND l.run_id=r.id AND a.kind='slack_invoice_paid'
   AND a.status='unknown' AND a.error_class='restore_uncertain');
 RETURN v_count;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION enter_restore_recovery(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enter_restore_recovery(text) TO relaydesk_migrate;

-- Clearance refuses to hide an unreviewed notification. The operator must use
-- the existing audited action-decision flow first.
-- +goose StatementBegin
CREATE FUNCTION clear_restore_recovery(p_note text)
RETURNS void LANGUAGE plpgsql SECURITY INVOKER AS $$
BEGIN
 IF length(btrim(coalesce(p_note,''))) NOT BETWEEN 12 AND 500 THEN
  RAISE EXCEPTION 'clearance note required';
 END IF;
 IF EXISTS (SELECT 1 FROM business_actions WHERE kind='slack_invoice_paid'
  AND status='unknown' AND error_class='restore_uncertain') THEN
  RAISE EXCEPTION 'unreviewed restore notifications remain';
 END IF;
 UPDATE restore_safety_gate SET outbound_enabled=true,recovery_cleared_at=now(),note=btrim(p_note)
 WHERE singleton AND outbound_enabled=false;
 IF NOT FOUND THEN RAISE EXCEPTION 'restore gate is not active'; END IF;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION clear_restore_recovery(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION clear_restore_recovery(text) TO relaydesk_migrate;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'restore safety migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
