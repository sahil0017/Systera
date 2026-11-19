-- Stage 1: Durable Agent Core.
--   * Agent registry, task DAG (tasks + dependencies), attempts (leases) and checkpoints.
--   * Per-run budget reservation so concurrent leases cannot overspend a run.
--   * systera_system: the role background processes (outbox relay, scheduler, workers)
--     assume to operate across tenants. RLS stays FORCEd; this role gets explicit
--     permissive policies rather than BYPASSRLS (which RDS does not allow granting).

-- ---------------------------------------------------------------------------
-- 1. System role
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'systera_system') THEN
        CREATE ROLE systera_system NOLOGIN NOSUPERUSER NOBYPASSRLS;
    END IF;
END
$$;
GRANT systera_system TO CURRENT_USER;

GRANT SELECT, UPDATE ON design_runs TO systera_system;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO systera_system;
GRANT INSERT ON audit_events TO systera_system;
GRANT USAGE ON SEQUENCE outbox_events_seq_seq TO systera_system;

DROP POLICY IF EXISTS system_all_design_runs ON design_runs;
CREATE POLICY system_all_design_runs ON design_runs TO systera_system USING (true) WITH CHECK (true);
DROP POLICY IF EXISTS system_all_outbox_events ON outbox_events;
CREATE POLICY system_all_outbox_events ON outbox_events TO systera_system USING (true) WITH CHECK (true);
DROP POLICY IF EXISTS system_all_audit_events ON audit_events;
CREATE POLICY system_all_audit_events ON audit_events TO systera_system USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------------------
-- 2. Run budget reservation & failure reason
-- ---------------------------------------------------------------------------

ALTER TABLE design_runs ADD COLUMN IF NOT EXISTS reserved_budget_usd NUMERIC(10, 4) NOT NULL DEFAULT 0;
ALTER TABLE design_runs ADD COLUMN IF NOT EXISTS failure_reason TEXT;
ALTER TABLE design_runs DROP CONSTRAINT IF EXISTS design_runs_budget_nonnegative;
ALTER TABLE design_runs ADD CONSTRAINT design_runs_budget_nonnegative
    CHECK (reserved_budget_usd >= 0 AND spent_budget_usd >= 0);

-- ---------------------------------------------------------------------------
-- 3. Agent registry (global catalog of specialist agent definitions)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS agents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_type      TEXT NOT NULL UNIQUE CHECK (agent_type ~ '^[a-z][a-z0-9_]{1,62}$'),
    display_name    TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    -- Empty model means "use the worker's configured default model".
    model           TEXT NOT NULL DEFAULT '',
    system_prompt   TEXT NOT NULL,
    allowed_tools   TEXT[] NOT NULL DEFAULT '{}',
    max_tokens      INT NOT NULL DEFAULT 4096 CHECK (max_tokens > 0),
    status          TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO agents (agent_type, display_name, description, system_prompt, allowed_tools, max_tokens)
VALUES (
    'requirements_analyst',
    'Requirements & Constraints Analyst',
    'Turns a design objective into explicit functional requirements, non-functional requirements, assumptions and open questions.',
    'You are the Requirements & Constraints Analyst in an autonomous system-design team. '
    'Given a design objective, constraints, acceptance criteria and the outputs of upstream tasks, '
    'produce precise, testable functional and non-functional requirements (with quantified targets '
    'where the inputs allow), explicit assumptions, and open questions. Never invent facts presented '
    'as given; label anything inferred as an assumption.',
    ARRAY['run.context', 'task.dependency_results'],
    4096
)
ON CONFLICT (agent_type) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 4. Task DAG
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS agent_tasks (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    run_id               UUID NOT NULL REFERENCES design_runs(id) ON DELETE CASCADE,
    task_key             TEXT NOT NULL,
    agent_type           TEXT NOT NULL REFERENCES agents(agent_type),
    objective            TEXT NOT NULL,
    input_json           JSONB NOT NULL DEFAULT '{}'::jsonb,
    granted_capabilities TEXT[] NOT NULL DEFAULT '{}',
    state                TEXT NOT NULL CHECK (state IN ('PENDING', 'READY', 'LEASED', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    priority             INT NOT NULL DEFAULT 0,
    budget_usd           NUMERIC(10, 4) NOT NULL CHECK (budget_usd >= 0),
    max_tokens           INT NOT NULL CHECK (max_tokens > 0),
    max_tool_calls       INT NOT NULL DEFAULT 0 CHECK (max_tool_calls >= 0),
    max_attempts         INT NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    timeout_seconds      INT NOT NULL DEFAULT 600 CHECK (timeout_seconds > 0),
    attempt_count        INT NOT NULL DEFAULT 0,
    available_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner          TEXT,
    lease_token          UUID,
    lease_expires_at     TIMESTAMPTZ,
    spent_usd            NUMERIC(12, 6) NOT NULL DEFAULT 0,
    result_json          JSONB,
    last_error           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at         TIMESTAMPTZ,
    UNIQUE (run_id, task_key),
    CHECK ((state = 'LEASED') = (lease_token IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_agent_tasks_ready ON agent_tasks (agent_type, priority DESC, created_at)
    WHERE state = 'READY';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_lease_expiry ON agent_tasks (lease_expires_at)
    WHERE state = 'LEASED';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_run ON agent_tasks (run_id, state);

CREATE TABLE IF NOT EXISTS task_dependencies (
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    run_id             UUID NOT NULL REFERENCES design_runs(id) ON DELETE CASCADE,
    task_id            UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    depends_on_task_id UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on_task_id),
    CHECK (task_id <> depends_on_task_id)
);

CREATE INDEX IF NOT EXISTS idx_task_dependencies_upstream ON task_dependencies (depends_on_task_id);

CREATE TABLE IF NOT EXISTS task_attempts (
    id                 UUID PRIMARY KEY,               -- equals the lease token (fencing token)
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    task_id            UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    attempt_number     INT NOT NULL,
    worker_id          TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'EXPIRED', 'CANCELLED')),
    started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_heartbeat_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at        TIMESTAMPTZ,
    error              TEXT,
    model_calls        INT NOT NULL DEFAULT 0,
    tool_calls         INT NOT NULL DEFAULT 0,
    prompt_tokens      INT NOT NULL DEFAULT 0,
    completion_tokens  INT NOT NULL DEFAULT 0,
    cost_usd           NUMERIC(12, 6) NOT NULL DEFAULT 0,
    UNIQUE (task_id, attempt_number)
);

CREATE TABLE IF NOT EXISTS task_checkpoints (
    seq         BIGSERIAL PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    task_id     UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    attempt_id  UUID NOT NULL REFERENCES task_attempts(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('PROGRESS', 'RESULT')),
    state_json  JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_task_checkpoints_task ON task_checkpoints (task_id, seq DESC);

-- ---------------------------------------------------------------------------
-- 5. Privileges & RLS
-- ---------------------------------------------------------------------------

GRANT SELECT ON agents TO systera_app, systera_system;
GRANT SELECT, INSERT, UPDATE ON agent_tasks TO systera_app, systera_system;
GRANT SELECT, INSERT ON task_dependencies TO systera_app, systera_system;
GRANT SELECT, INSERT, UPDATE ON task_attempts TO systera_app, systera_system;
GRANT SELECT, INSERT ON task_checkpoints TO systera_app, systera_system;
GRANT USAGE ON SEQUENCE task_checkpoints_seq_seq TO systera_app, systera_system;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['agent_tasks', 'task_dependencies', 'task_attempts', 'task_checkpoints'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_%s ON %I', t, t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_%s ON %I TO systera_app
            USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)$p$, t, t);
        EXECUTE format('DROP POLICY IF EXISTS system_all_%s ON %I', t, t);
        EXECUTE format('CREATE POLICY system_all_%s ON %I TO systera_system USING (true) WITH CHECK (true)', t, t);
    END LOOP;
END
$$;
