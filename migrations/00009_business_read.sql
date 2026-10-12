-- +goose Up
GRANT SELECT (id,tenant_id,event_row_id,automation_revision_id) ON planning_jobs TO relaydesk_app;
GRANT SELECT (id,tenant_id,event_id,event_type,provider_account_id,mode,connection_id,received_at) ON webhook_events TO relaydesk_app;
-- +goose StatementBegin
DO $$ DECLARE tab text; BEGIN
 FOREACH tab IN ARRAY ARRAY['planning_jobs','webhook_events'] LOOP
  EXECUTE format('CREATE POLICY browser_member ON %I TO relaydesk_app USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid AND public.tenant_authorize(decode(coalesce(current_setting(''app.session_hash'',true),''''),''hex''),tenant_id,''read'') IS NOT NULL)',tab);
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'business browser grants are intentionally irreversible'; END $$;
-- +goose StatementEnd
