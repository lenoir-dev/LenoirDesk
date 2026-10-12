-- +goose Up
-- Expose only the gate's Boolean result after tenant authorization. The app
-- role never receives direct SELECT on the global restore control row.
-- +goose StatementBegin
CREATE FUNCTION read_outbound_gate(p_session bytea,p_tenant uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE v_enabled boolean;
BEGIN
 IF public.tenant_authorize(p_session,p_tenant,'read') IS NULL THEN
  RAISE EXCEPTION 'tenant read denied' USING ERRCODE='42501';
 END IF;
 SELECT outbound_enabled INTO v_enabled FROM public.restore_safety_gate WHERE singleton=true;
 RETURN v_enabled;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION read_outbound_gate(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION read_outbound_gate(bytea,uuid) TO relaydesk_app;
CREATE INDEX reconcile_requests_tenant_history
  ON reconcile_requests(tenant_id,created_at DESC,id DESC);

-- +goose Down
DROP INDEX reconcile_requests_tenant_history;
DROP FUNCTION read_outbound_gate(bytea,uuid);
