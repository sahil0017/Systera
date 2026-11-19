-- Stage 0 Foundation Migration: Identity, Tenancy, Projects, Design Runs, Outbox & Audit
-- Applied by `go run ./cmd/migrate up`, which wraps each file and its schema_migrations row in one transaction.
-- Enforces PostgreSQL Row-Level Security (RLS) on tenant-scoped tables.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- 1. Identity & Tenancy
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DELETED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    external_sub TEXT UNIQUE,
    status      TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tenant_memberships (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('OWNER', 'ADMIN', 'ARCHITECT', 'ENGINEER', 'VIEWER')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

-- ---------------------------------------------------------------------------
-- 2. Projects & Design Runs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by  UUID NOT NULL REFERENCES users(id),
    status      TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ARCHIVED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_projects_tenant_id ON projects(tenant_id);

CREATE TABLE IF NOT EXISTS design_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id          UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    idempotency_key     TEXT NOT NULL,
    objective           TEXT NOT NULL,
    constraints_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    acceptance_criteria JSONB NOT NULL DEFAULT '[]'::jsonb,
    state               TEXT NOT NULL DEFAULT 'CREATED' CHECK (state IN (
                            'CREATED', 'PLANNING', 'PLAN_VALIDATING', 'READY', 'RUNNING',
                            'WAITING_EXTERNAL', 'EVALUATING', 'REVISING', 'PUBLISH_READY',
                            'COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED'
                        )),
    max_budget_usd      NUMERIC(10, 4) NOT NULL DEFAULT 5.0000,
    spent_budget_usd    NUMERIC(10, 4) NOT NULL DEFAULT 0.0000,
    correlation_id      UUID NOT NULL DEFAULT gen_random_uuid(),
    created_by          UUID NOT NULL REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, project_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_design_runs_tenant_project ON design_runs(tenant_id, project_id);
CREATE INDEX IF NOT EXISTS idx_design_runs_state ON design_runs(state);

-- ---------------------------------------------------------------------------
-- 3. Transactional Outbox (NATS JetStream)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS outbox_events (
    seq             BIGSERIAL PRIMARY KEY,
    event_id        UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    event_type      TEXT NOT NULL,
    schema_version  TEXT NOT NULL DEFAULT '1.0',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    tenant_id       UUID NOT NULL,
    project_id      UUID,
    run_id          UUID,
    correlation_id  UUID NOT NULL,
    causation_id    UUID,
    payload         JSONB NOT NULL,
    published_at    TIMESTAMPTZ,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error      TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_unpublished ON outbox_events(seq) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- 4. Immutable Audit Events
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id        UUID NOT NULL,
    actor_type      TEXT NOT NULL CHECK (actor_type IN ('USER', 'AGENT', 'SYSTEM')),
    action          TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     UUID,
    correlation_id  UUID NOT NULL,
    metadata_json   JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_time ON audit_events(tenant_id, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- 5. Row-Level Security (Tenant Isolation)
-- ---------------------------------------------------------------------------

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE design_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_projects ON projects;
CREATE POLICY tenant_isolation_projects ON projects
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_design_runs ON design_runs;
CREATE POLICY tenant_isolation_design_runs ON design_runs
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_audit_events ON audit_events;
CREATE POLICY tenant_isolation_audit_events ON audit_events
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

