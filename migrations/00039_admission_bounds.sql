-- +goose Up
-- Existing abandoned flows predate the cap and may otherwise keep admission
-- closed indefinitely. Migrations run with API traffic stopped.
DELETE FROM oidc_flows WHERE expires_at<=now();

CREATE INDEX webhook_events_ignored_tenant ON webhook_events(tenant_id)
WHERE classification='ignored';

-- Bound unauthenticated OIDC flow storage inside the privileged admission
-- function. The same transaction serializes capacity decisions across API pods.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION oidc_begin(p_state_hash bytea, p_ciphertext bytea, p_nonce bytea,
                           p_nonce_hash bytea, p_expires_at timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE v_count bigint;
BEGIN
    IF length(p_state_hash) <> 32 OR length(p_nonce_hash) <> 32 OR length(p_nonce) <> 12
       OR p_expires_at <= now() OR p_expires_at > now() + interval '10 minutes' THEN
        RAISE EXCEPTION 'invalid OIDC flow';
    END IF;
    PERFORM pg_advisory_xact_lock(41907);
    WITH due AS (
        SELECT state_hash FROM public.oidc_flows WHERE expires_at <= now()
        ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED
    ) DELETE FROM public.oidc_flows f USING due d WHERE f.state_hash=d.state_hash;
    SELECT count(*) INTO v_count FROM public.oidc_flows;
    IF v_count >= 10000 THEN
        RAISE EXCEPTION 'OIDC flow capacity exhausted';
    END IF;
    INSERT INTO public.oidc_flows(state_hash, verifier_ciphertext, verifier_nonce,
                                  nonce_hash, expires_at)
    VALUES (p_state_hash, p_ciphertext, p_nonce, p_nonce_hash, p_expires_at);
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'admission bound migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
