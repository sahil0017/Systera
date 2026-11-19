DROP TABLE IF EXISTS task_checkpoints;
DROP TABLE IF EXISTS task_attempts;
DROP TABLE IF EXISTS task_dependencies;
DROP TABLE IF EXISTS agent_tasks;
DROP TABLE IF EXISTS agents;

ALTER TABLE design_runs DROP CONSTRAINT IF EXISTS design_runs_budget_nonnegative;
ALTER TABLE design_runs DROP COLUMN IF EXISTS failure_reason;
ALTER TABLE design_runs DROP COLUMN IF EXISTS reserved_budget_usd;

DROP POLICY IF EXISTS system_all_audit_events ON audit_events;
DROP POLICY IF EXISTS system_all_outbox_events ON outbox_events;
DROP POLICY IF EXISTS system_all_design_runs ON design_runs;

REVOKE ALL ON design_runs, outbox_events, audit_events FROM systera_system;
REVOKE ALL ON SEQUENCE outbox_events_seq_seq FROM systera_system;
