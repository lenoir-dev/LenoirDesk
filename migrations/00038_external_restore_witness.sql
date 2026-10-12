-- +goose Up
-- This marker is deliberately included in pg_dump. Its independently stored
-- counterpart is not. An older database snapshot therefore cannot authorize
-- an external effect after the worker advanced the external high-water mark.
CREATE TABLE restore_witness_state (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 incarnation uuid NOT NULL DEFAULT gen_random_uuid(),
 effect_sequence bigint NOT NULL DEFAULT 0 CHECK (effect_sequence >= 0)
);
INSERT INTO restore_witness_state(singleton) VALUES(true);
GRANT SELECT,UPDATE(effect_sequence) ON restore_witness_state TO relaydesk_runtime;

CREATE TABLE restore_witness_audit (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 action text NOT NULL CHECK (action IN ('enroll','rebind')),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 12 AND 500),
 incarnation uuid NOT NULL,
 effect_sequence bigint NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON restore_witness_audit FROM PUBLIC;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'external restore witness migration is intentionally irreversible'; END $$;
-- +goose StatementEnd
