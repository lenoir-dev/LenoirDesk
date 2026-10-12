-- +goose Up
-- Browser-side Contact inspection needs a credential only inside the server.
-- The app role receives no direct credential table access.
-- +goose StatementBegin
CREATE FUNCTION contact_preview_material(p_session bytea,p_tenant uuid,p_hubspot uuid)
RETURNS TABLE(mode text,environment text,health text,ciphertext bytea,nonce bytea,key_version integer)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT c.mode,c.execution_environment,c.health_status,k.ciphertext,k.nonce,k.key_version
 FROM public.connections c JOIN public.connection_credentials k
  ON k.tenant_id=c.tenant_id AND k.connection_id=c.id AND k.generation=c.generation
  AND k.secret_kind='api_token' AND k.revoked_at IS NULL
 LEFT JOIN public.provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 WHERE c.tenant_id=p_tenant AND c.id=p_hubspot AND c.provider='hubspot'
  AND c.status='active' AND c.mode='test' AND c.health_status='verified'
  AND c.execution_environment IN ('simulation','test')
  AND (s.connection_id IS NULL OR (s.generation=c.generation AND s.auth_state='ready'
   AND (s.rotation_mode='static' OR s.expires_at>now()+interval '30 seconds')))
  AND public.tenant_authorize(p_session,p_tenant,'operate') IS NOT NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION contact_preview_material(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION contact_preview_material(bytea,uuid,uuid) TO relaydesk_app;
-- +goose Down
DROP FUNCTION contact_preview_material(bytea,uuid,uuid);
