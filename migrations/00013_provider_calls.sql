-- +goose Up
CREATE TABLE business_provider_calls (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 attempt_id uuid NOT NULL,
 provider text NOT NULL CHECK (provider IN ('stripe','hubspot','slack')),
 method text NOT NULL CHECK (method IN ('GET','POST','PATCH')),
 request_path text NOT NULL,
 request_started_at timestamptz NOT NULL DEFAULT now(),
 response_status integer,
 outcome text,
 provider_request_id text,
 finished_at timestamptz,
 UNIQUE (tenant_id,id),
 FOREIGN KEY (tenant_id,attempt_id) REFERENCES business_attempts(tenant_id,id)
);
CREATE INDEX business_provider_calls_attempt ON business_provider_calls(tenant_id,attempt_id,request_started_at);
ALTER TABLE business_provider_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_provider_calls FORCE ROW LEVEL SECURITY;
CREATE POLICY runtime_tenant ON business_provider_calls TO relaydesk_runtime
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY migration_admin ON business_provider_calls TO relaydesk_migrate USING (true) WITH CHECK (true);
GRANT SELECT,INSERT,UPDATE ON business_provider_calls TO relaydesk_runtime;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'provider-call history is intentionally irreversible'; END $$;
-- +goose StatementEnd
