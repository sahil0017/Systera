ALTER TABLE outbox_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE design_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE projects NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_outbox_events ON outbox_events;
ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;

REVOKE ALL ON projects, design_runs, outbox_events, audit_events FROM systera_app;
REVOKE ALL ON SEQUENCE outbox_events_seq_seq FROM systera_app;

DROP TRIGGER IF EXISTS trg_outbox_notify ON outbox_events;
DROP FUNCTION IF EXISTS systera_notify_outbox();
DROP INDEX IF EXISTS idx_outbox_tenant_run_seq;

ALTER TABLE design_runs DROP COLUMN IF EXISTS request_fingerprint;
UPDATE design_runs SET idempotency_key = 'legacy-' || id::text WHERE idempotency_key IS NULL;
ALTER TABLE design_runs ALTER COLUMN idempotency_key SET NOT NULL;
