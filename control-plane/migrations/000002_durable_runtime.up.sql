-- Stage 0.1: durable runtime hardening.
--   * Idempotency conflict detection (request fingerprint) and nullable idempotency keys.
--   * Live event streaming via LISTEN/NOTIFY on the transactional outbox.
--   * Non-owner application role so Row-Level Security is actually enforced.

-- ---------------------------------------------------------------------------
-- 1. Idempotency
-- ---------------------------------------------------------------------------

-- Runs created without an Idempotency-Key store NULL, which never collides under the
-- UNIQUE (tenant_id, project_id, idempotency_key) constraint.
ALTER TABLE design_runs ALTER COLUMN idempotency_key DROP NOT NULL;
UPDATE design_runs SET idempotency_key = NULL WHERE idempotency_key = '';

-- SHA-256 of the normalized create-run request. A replay of the same key with a different
-- fingerprint is rejected instead of silently returning the original run.
ALTER TABLE design_runs ADD COLUMN IF NOT EXISTS request_fingerprint TEXT;

-- ---------------------------------------------------------------------------
-- 2. Outbox streaming
-- ---------------------------------------------------------------------------

CREATE INDEX IF NOT EXISTS idx_outbox_tenant_run_seq ON outbox_events(tenant_id, run_id, seq);

CREATE OR REPLACE FUNCTION systera_notify_outbox() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.run_id IS NOT NULL THEN
        -- Delivered only when the inserting transaction commits.
        PERFORM pg_notify('systera_outbox', NEW.run_id::text);
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_outbox_notify ON outbox_events;
CREATE TRIGGER trg_outbox_notify
    AFTER INSERT ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION systera_notify_outbox();

-- ---------------------------------------------------------------------------
-- 3. Enforced Row-Level Security
-- ---------------------------------------------------------------------------

-- Table owners and superusers bypass RLS. Tenant-scoped requests therefore run as
-- systera_app (SET LOCAL ROLE inside each transaction), which owns nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'systera_app') THEN
        CREATE ROLE systera_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
    END IF;
END
$$;

GRANT systera_app TO CURRENT_USER;

GRANT SELECT, INSERT, UPDATE ON projects, design_runs TO systera_app;
GRANT SELECT, INSERT ON outbox_events, audit_events TO systera_app;
GRANT USAGE ON SEQUENCE outbox_events_seq_seq TO systera_app;

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_outbox_events ON outbox_events;
CREATE POLICY tenant_isolation_outbox_events ON outbox_events
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE design_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
