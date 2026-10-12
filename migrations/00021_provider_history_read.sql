-- +goose Up
GRANT SELECT ON business_provider_calls TO relaydesk_app;
CREATE POLICY browser_member ON business_provider_calls TO relaydesk_app
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND public.tenant_authorize(decode(coalesce(current_setting('app.session_hash',true),''),'hex'),tenant_id,'read') IS NOT NULL);
-- +goose Down
REVOKE SELECT ON business_provider_calls FROM relaydesk_app;
DROP POLICY browser_member ON business_provider_calls;
