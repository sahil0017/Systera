-- Stage 2 (Checklist 6.1-6.5): Dependency engine and impact analysis (spec §9.3, §23.1).
--
--   artifacts              a named projection of the model (HLD, ERD, OpenAPI, tests...),
--                          with status CURRENT / STALE / RETIRED and its current version.
--   artifact_versions      immutable versions: checksum, content ref, owner, generation run,
--                          evidence refs, the graph version (and hash) it was generated from.
--   artifact_dependencies  typed dependency edges of a version (NODE / EDGE / ARTIFACT /
--                          DECISION / CLAIM) with the dependency's content hash at binding.
--   revalidation_requests  stale artifacts awaiting a revalidation run (picked up by the
--                          scheduler, which creates runtime tasks for them).
--
-- Staleness is decided by the graph service in the same transaction as the patch that
-- causes it (impact traversal), and by a drift sweep for decision / claim / artifact
-- dependencies that change outside patches. Dependency hashes make verification
-- deterministic: an artifact is fresh iff every recorded hash still matches.

CREATE TABLE IF NOT EXISTS artifacts (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id             UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_.:/-]{0,127}$'),
    projection_type      TEXT NOT NULL CHECK (projection_type IN (
                             'REQUIREMENTS', 'HLD', 'LLD', 'COMPONENT_CATALOGUE', 'ERD', 'SQL_DDL', 'OPENAPI',
                             'EVENT_SCHEMA', 'SEQUENCE_DIAGRAM', 'DEPLOYMENT_DIAGRAM', 'THREAT_MODEL',
                             'CAPACITY_PLAN', 'ADR_EXPORT', 'TEST_SUITE', 'SIMULATION_REPORT', 'OTHER')),
    owner_type           TEXT NOT NULL CHECK (owner_type IN ('USER', 'AGENT', 'SYSTEM')),
    owner_id             TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'CURRENT' CHECK (status IN ('CURRENT', 'STALE', 'RETIRED')),
    current_version_id   UUID,
    stale_reason         TEXT,
    stale_since          TIMESTAMPTZ,
    stale_graph_version  INT,
    revalidation_run_id  UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, name),
    UNIQUE (id, model_id),
    CHECK ((status = 'STALE') = (stale_reason IS NOT NULL AND stale_since IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_artifacts_model_status ON artifacts(model_id, status);

CREATE TABLE IF NOT EXISTS artifact_versions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID NOT NULL,
    artifact_id       UUID NOT NULL,
    version_number    INT NOT NULL CHECK (version_number >= 1),
    checksum          TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    content_ref       TEXT NOT NULL DEFAULT '' CHECK (length(content_ref) <= 2000),
    media_type        TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    graph_version     INT NOT NULL,
    graph_hash        TEXT NOT NULL CHECK (graph_hash ~ '^[0-9a-f]{64}$'),
    generated_from    JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(generated_from) = 'object'),
    generation_run_id UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    agent_task_id     UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
    evidence_refs     TEXT[] NOT NULL DEFAULT '{}',
    created_by_type   TEXT NOT NULL CHECK (created_by_type IN ('USER', 'AGENT', 'SYSTEM')),
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (artifact_id, version_number),
    UNIQUE (id, model_id),
    FOREIGN KEY (artifact_id, model_id) REFERENCES artifacts(id, model_id) ON DELETE CASCADE,
    FOREIGN KEY (model_id, graph_version) REFERENCES architecture_versions(model_id, version_number)
);

ALTER TABLE artifacts DROP CONSTRAINT IF EXISTS artifacts_current_version_fk;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_current_version_fk
    FOREIGN KEY (current_version_id, model_id) REFERENCES artifact_versions(id, model_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE IF NOT EXISTS artifact_dependencies (
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id            UUID NOT NULL,
    artifact_version_id UUID NOT NULL,
    dep_type            TEXT NOT NULL CHECK (dep_type IN ('NODE', 'EDGE', 'ARTIFACT', 'DECISION', 'CLAIM')),
    target_id           UUID NOT NULL,
    target_hash         TEXT NOT NULL CHECK (target_hash ~ '^[0-9a-f]{64}$'),
    -- "direct" or "closure:<root node id>" (reached by the bounded dependency closure).
    via                 TEXT NOT NULL DEFAULT 'direct',
    PRIMARY KEY (artifact_version_id, dep_type, target_id),
    FOREIGN KEY (artifact_version_id, model_id) REFERENCES artifact_versions(id, model_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_artifact_deps_target ON artifact_dependencies(model_id, dep_type, target_id);

CREATE TABLE IF NOT EXISTS revalidation_requests (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id       UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    artifact_ids   UUID[] NOT NULL CHECK (cardinality(artifact_ids) > 0),
    graph_version  INT NOT NULL,
    cause          TEXT NOT NULL,             -- e.g. patch:<id> or drift-sweep
    reason         TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'SCHEDULED', 'FAILED')),
    run_id         UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    attempts       INT NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scheduled_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_revalidation_pending ON revalidation_requests(created_at) WHERE status = 'PENDING';

-- ---------------------------------------------------------------------------
-- Integrity
-- ---------------------------------------------------------------------------

-- Artifact identity is immutable; only status, stale fields, current version and the
-- revalidation run change. RETIRED is final.
CREATE OR REPLACE FUNCTION artifacts_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mutable TEXT[] := ARRAY['status', 'current_version_id', 'stale_reason', 'stale_since',
                                'stale_graph_version', 'revalidation_run_id', 'updated_at'];
BEGIN
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'artifact % is retired', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    IF (to_jsonb(NEW) - mutable) IS DISTINCT FROM (to_jsonb(OLD) - mutable) THEN
        RAISE EXCEPTION 'artifact identity (name, type, owner, model) is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_artifacts_guard ON artifacts;
CREATE TRIGGER trg_artifacts_guard BEFORE UPDATE ON artifacts FOR EACH ROW EXECUTE FUNCTION artifacts_guard();

-- Versions and dependencies are immutable (append-only, no UPDATE privilege either).
CREATE OR REPLACE FUNCTION artifact_rows_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END;
$$;
DROP TRIGGER IF EXISTS trg_artifact_versions_immutable ON artifact_versions;
CREATE TRIGGER trg_artifact_versions_immutable BEFORE UPDATE ON artifact_versions FOR EACH ROW EXECUTE FUNCTION artifact_rows_immutable();
DROP TRIGGER IF EXISTS trg_artifact_dependencies_immutable ON artifact_dependencies;
CREATE TRIGGER trg_artifact_dependencies_immutable BEFORE UPDATE ON artifact_dependencies FOR EACH ROW EXECUTE FUNCTION artifact_rows_immutable();

-- A version's graph_hash must be the hash of the graph version it claims to be bound to.
CREATE OR REPLACE FUNCTION artifact_versions_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM architecture_versions
                   WHERE model_id = NEW.model_id AND version_number = NEW.graph_version AND graph_hash = NEW.graph_hash) THEN
        RAISE EXCEPTION 'artifact version must reference an existing graph version and its graph hash'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_artifact_versions_binding ON artifact_versions;
CREATE TRIGGER trg_artifact_versions_binding BEFORE INSERT ON artifact_versions FOR EACH ROW EXECUTE FUNCTION artifact_versions_binding();

-- ---------------------------------------------------------------------------
-- Revalidation agent (runs as an ordinary runtime task)
-- ---------------------------------------------------------------------------

INSERT INTO agents (agent_type, display_name, description, system_prompt, allowed_tools, max_tokens)
VALUES (
    'artifact_revalidator',
    'Artifact Revalidator',
    'Re-checks an artifact that the dependency engine marked stale against the current architecture graph.',
    'You are the Artifact Revalidator. An artifact was marked stale because the architecture it was generated '
    'from changed. Using the task input (artifact, stale reason, changed nodes) and the run context, determine '
    'which parts of the artifact are now inaccurate, what must be regenerated, and whether the artifact can be '
    're-affirmed unchanged. Be specific about sections and elements; do not invent architecture facts.',
    ARRAY['run.context', 'task.dependency_results'],
    4096
)
ON CONFLICT (agent_type) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Privileges & RLS (no DELETE)
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON artifacts TO systera_app, systera_system, systera_graph;
GRANT SELECT, INSERT ON artifact_versions, artifact_dependencies TO systera_app, systera_system;
GRANT SELECT ON artifact_versions, artifact_dependencies TO systera_graph;
GRANT SELECT, INSERT ON revalidation_requests TO systera_app, systera_graph;
GRANT SELECT, INSERT, UPDATE ON revalidation_requests TO systera_system;
-- The graph service stales claims about changed nodes in the patch transaction; the claim
-- commit trigger it fires reads the ledger links as that role.
GRANT SELECT, UPDATE ON claims TO systera_graph;
GRANT SELECT ON claim_evidence, claim_assumptions, assumptions TO systera_graph;
-- The revalidation scheduler (system role) creates runs owned by the project's creator.
GRANT SELECT ON projects TO systera_system;
DROP POLICY IF EXISTS system_read_projects ON projects;
CREATE POLICY system_read_projects ON projects FOR SELECT TO systera_system USING (true);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['artifacts', 'artifact_versions', 'artifact_dependencies', 'revalidation_requests'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_%s ON %I', t, t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_%s ON %I TO systera_app, systera_graph
            USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)$p$, t, t);
        EXECUTE format('DROP POLICY IF EXISTS system_all_%s ON %I', t, t);
        EXECUTE format('CREATE POLICY system_all_%s ON %I TO systera_system USING (true) WITH CHECK (true)', t, t);
    END LOOP;
END
$$;

INSERT INTO graph_invariants (key, description, severity, enforced_by, active, spec_ref) VALUES
    ('artifact_binding', 'An artifact version references exactly one existing graph version and that version''s graph hash.', 'ERROR', 'DATABASE', true, 'Appendix D'),
    ('artifact_history_immutable', 'Artifact versions and their dependency hashes are immutable.', 'ERROR', 'DATABASE', true, '§9.3'),
    ('impact_invalidation', 'A change marks every artifact, decision and claim that depends on the changed nodes (transitively) stale in the same transaction; a truncated traversal invalidates conservatively.', 'ERROR', 'GRAPH_SERVICE', true, '§23.1')
ON CONFLICT (key) DO NOTHING;
