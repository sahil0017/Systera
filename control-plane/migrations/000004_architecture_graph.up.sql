-- Stage 2 (Architecture Intelligence Core), part 1: the canonical graph data model.
--
-- Storage model (spec §29: PostgreSQL adjacency tables first, no graph database):
--   * architecture_models    one canonical model per project.
--   * architecture_versions  immutable, linear version chain (0, 1, 2, ...) per model.
--   * architecture_nodes /   append-only temporal rows. A logical node/edge keeps one
--     architecture_edges     stable id for life; each revision is a row valid for the
--                            version range [valid_from_version, valid_to_version). The
--                            graph "at version v" is every row whose range contains v.
--                            Edits close the old row and append a new one, so history
--                            is never rewritten (Appendix D).
--   * graph_patches          submitted change sets (GraphPatch v1) and their outcome.
--   * graph_invariants       registry of the invariants and where each is enforced.
--   * graph_snapshots        published, immutable versions (bound to one graph hash).
--
-- Integrity that does not depend on application code is enforced here: append-only
-- history, a gap-free version chain, no overlapping revisions, edges whose endpoints
-- exist in the same model at the same version, and immutable published snapshots.

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ---------------------------------------------------------------------------
-- 1.1 Models & 1.2 Versions
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS architecture_models (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id         UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    current_version_id UUID,
    schema_version     TEXT NOT NULL DEFAULT '1.0',
    status             TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ARCHIVED')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id)
);
CREATE INDEX IF NOT EXISTS idx_architecture_models_tenant ON architecture_models(tenant_id);

CREATE TABLE IF NOT EXISTS architecture_versions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    version_number    INT NOT NULL CHECK (version_number >= 0),
    parent_version_id UUID REFERENCES architecture_versions(id) ON DELETE CASCADE,
    graph_hash        TEXT NOT NULL CHECK (graph_hash ~ '^[0-9a-f]{64}$'),
    node_count        INT NOT NULL DEFAULT 0 CHECK (node_count >= 0),
    edge_count        INT NOT NULL DEFAULT 0 CHECK (edge_count >= 0),
    change_summary    TEXT NOT NULL DEFAULT '',
    author_type       TEXT NOT NULL CHECK (author_type IN ('USER', 'AGENT', 'SYSTEM')),
    author_id         TEXT NOT NULL,
    patch_id          UUID,
    run_id            UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, version_number),
    UNIQUE (id, model_id),
    CHECK ((version_number = 0) = (parent_version_id IS NULL))
);

ALTER TABLE architecture_models DROP CONSTRAINT IF EXISTS architecture_models_current_version_fk;
ALTER TABLE architecture_models ADD CONSTRAINT architecture_models_current_version_fk
    FOREIGN KEY (current_version_id, id) REFERENCES architecture_versions(id, model_id)
    DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- 1.3 Nodes & 1.4 Edges (append-only temporal rows)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS architecture_nodes (
    row_id             BIGSERIAL PRIMARY KEY,
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id           UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    node_id            UUID NOT NULL,
    node_key           TEXT NOT NULL CHECK (node_key ~ '^[a-z0-9][a-z0-9_.:-]{0,127}$'),
    node_type          TEXT NOT NULL CHECK (node_type IN (
                           'system', 'domain', 'service', 'component', 'api', 'event', 'topic',
                           'database', 'table', 'cache', 'queue', 'actor', 'deployment_unit',
                           'region', 'secret_boundary', 'external_dependency', 'slo', 'threat',
                           'workload', 'test')),
    name               TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    description        TEXT NOT NULL DEFAULT '',
    properties         JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(properties) = 'object'),
    owner_domain       TEXT,
    lifecycle_state    TEXT NOT NULL DEFAULT 'PROPOSED'
                           CHECK (lifecycle_state IN ('PROPOSED', 'ACTIVE', 'DEPRECATED', 'RETIRED')),
    evidence_state     TEXT NOT NULL DEFAULT 'PROPOSED'
                           CHECK (evidence_state IN ('OBSERVED', 'INFERRED', 'PROPOSED')),
    revision           INT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    valid_from_version INT NOT NULL,
    valid_to_version   INT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (valid_to_version IS NULL OR valid_to_version > valid_from_version),
    FOREIGN KEY (model_id, valid_from_version) REFERENCES architecture_versions(model_id, version_number) ON DELETE CASCADE,
    FOREIGN KEY (model_id, valid_to_version) REFERENCES architecture_versions(model_id, version_number) ON DELETE CASCADE,
    -- A logical node has at most one revision at any version, and a key names at most
    -- one node at any version.
    EXCLUDE USING gist (node_id WITH =, int4range(valid_from_version, valid_to_version) WITH &&),
    EXCLUDE USING gist (model_id WITH =, node_key WITH =, int4range(valid_from_version, valid_to_version) WITH &&)
);
CREATE INDEX IF NOT EXISTS idx_architecture_nodes_current ON architecture_nodes(model_id, node_id) WHERE valid_to_version IS NULL;
CREATE INDEX IF NOT EXISTS idx_architecture_nodes_history ON architecture_nodes(model_id, node_id, valid_from_version);
CREATE INDEX IF NOT EXISTS idx_architecture_nodes_type ON architecture_nodes(model_id, node_type) WHERE valid_to_version IS NULL;

CREATE TABLE IF NOT EXISTS architecture_edges (
    row_id             BIGSERIAL PRIMARY KEY,
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id           UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    edge_id            UUID NOT NULL,
    source_node_id     UUID NOT NULL,
    target_node_id     UUID NOT NULL,
    relation_type      TEXT NOT NULL CHECK (relation_type IN (
                           'owns', 'calls', 'publishes', 'subscribes', 'reads', 'writes',
                           'deploys_to', 'depends_on', 'authenticates', 'replicates',
                           'fails_over_to', 'derived_from')),
    direction          TEXT NOT NULL DEFAULT 'DIRECTED' CHECK (direction IN ('DIRECTED', 'BIDIRECTIONAL')),
    protocol           TEXT,
    sync_mode          TEXT CHECK (sync_mode IN ('SYNC', 'ASYNC', 'BATCH', 'STREAM')),
    constraints        JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(constraints) = 'object'),
    properties         JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(properties) = 'object'),
    revision           INT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    valid_from_version INT NOT NULL,
    valid_to_version   INT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (source_node_id <> target_node_id),
    CHECK (valid_to_version IS NULL OR valid_to_version > valid_from_version),
    FOREIGN KEY (model_id, valid_from_version) REFERENCES architecture_versions(model_id, version_number) ON DELETE CASCADE,
    FOREIGN KEY (model_id, valid_to_version) REFERENCES architecture_versions(model_id, version_number) ON DELETE CASCADE,
    EXCLUDE USING gist (edge_id WITH =, int4range(valid_from_version, valid_to_version) WITH &&)
);
CREATE INDEX IF NOT EXISTS idx_architecture_edges_out ON architecture_edges(model_id, source_node_id, valid_from_version);
CREATE INDEX IF NOT EXISTS idx_architecture_edges_in ON architecture_edges(model_id, target_node_id, valid_from_version);
CREATE INDEX IF NOT EXISTS idx_architecture_edges_current ON architecture_edges(model_id, edge_id) WHERE valid_to_version IS NULL;

-- ---------------------------------------------------------------------------
-- 1.5 Graph patches (= architecture change sets)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS graph_patches (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id             UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    run_id               UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    agent_task_id        UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
    base_version_number  INT NOT NULL CHECK (base_version_number >= 0),
    idempotency_key      TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_fingerprint  TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    operations           JSONB NOT NULL CHECK (jsonb_typeof(operations) = 'array' AND jsonb_array_length(operations) > 0),
    expected_invariants  TEXT[] NOT NULL DEFAULT '{}',
    author_type          TEXT NOT NULL CHECK (author_type IN ('USER', 'AGENT', 'SYSTEM')),
    author_id            TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'PENDING'
                             CHECK (status IN ('PENDING', 'ACCEPTED', 'REJECTED', 'CONFLICT')),
    risk_summary         JSONB NOT NULL DEFAULT '{}'::jsonb,
    approval_state       TEXT NOT NULL DEFAULT 'NOT_REQUIRED'
                             CHECK (approval_state IN ('NOT_REQUIRED', 'PENDING', 'APPROVED', 'REJECTED')),
    approved_by          TEXT,
    approved_at          TIMESTAMPTZ,
    result               JSONB,
    resulting_version_id UUID,
    -- The version this change set can be rolled back to (its base when accepted).
    rollback_version_id  UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at           TIMESTAMPTZ,
    UNIQUE (model_id, idempotency_key),
    FOREIGN KEY (resulting_version_id, model_id) REFERENCES architecture_versions(id, model_id),
    FOREIGN KEY (rollback_version_id, model_id) REFERENCES architecture_versions(id, model_id),
    CHECK (status <> 'ACCEPTED' OR (resulting_version_id IS NOT NULL AND approval_state IN ('NOT_REQUIRED', 'APPROVED'))),
    CHECK ((status = 'PENDING') = (decided_at IS NULL)),
    CHECK ((approval_state IN ('APPROVED', 'REJECTED')) = (approved_at IS NOT NULL AND approved_by IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_graph_patches_model ON graph_patches(model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_graph_patches_pending ON graph_patches(model_id) WHERE status = 'PENDING';

ALTER TABLE architecture_versions DROP CONSTRAINT IF EXISTS architecture_versions_patch_fk;
ALTER TABLE architecture_versions ADD CONSTRAINT architecture_versions_patch_fk
    FOREIGN KEY (patch_id) REFERENCES graph_patches(id) DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- 1.6 Invariant registry & published snapshots
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS graph_invariants (
    key          TEXT PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_]{2,63}$'),
    description  TEXT NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('ERROR', 'WARNING')),
    enforced_by  TEXT NOT NULL CHECK (enforced_by IN ('DATABASE', 'GRAPH_SERVICE')),
    active       BOOLEAN NOT NULL,
    spec_ref     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO graph_invariants (key, description, severity, enforced_by, active, spec_ref) VALUES
    ('edge_endpoints_exist', 'Every edge references nodes that exist in the same tenant and model for the edge''s whole lifetime.', 'ERROR', 'DATABASE', true, 'Appendix D'),
    ('history_append_only', 'Node and edge ids are immutable; edits append new revisions and never rewrite history.', 'ERROR', 'DATABASE', true, 'Appendix D'),
    ('version_chain_linear', 'Versions form a gap-free chain 0..n; each version''s parent is the previous version of the same model.', 'ERROR', 'DATABASE', true, '§23.1'),
    ('single_revision_per_version', 'A node, edge or node key has at most one live revision at any version.', 'ERROR', 'DATABASE', true, '§23'),
    ('snapshot_immutable', 'Published snapshots are immutable and reference exactly one version and its graph hash.', 'ERROR', 'DATABASE', true, 'Appendix D'),
    ('edge_type_compatibility', 'Edge relation types connect compatible node types (e.g. writes targets a data store).', 'ERROR', 'GRAPH_SERVICE', false, 'Checklist 3.5'),
    ('claim_evidence_requirements', 'OBSERVED claims require direct evidence; INFERRED claims require evidence and reasoning.', 'ERROR', 'GRAPH_SERVICE', false, 'Appendix D'),
    ('patch_optimistic_concurrency', 'A patch applies only to the model''s current version (base_graph_version).', 'ERROR', 'GRAPH_SERVICE', false, '§23.1')
ON CONFLICT (key) DO NOTHING;

CREATE TABLE IF NOT EXISTS graph_snapshots (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id       UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    version_id     UUID NOT NULL,
    version_number INT NOT NULL,
    graph_hash     TEXT NOT NULL CHECK (graph_hash ~ '^[0-9a-f]{64}$'),
    label          TEXT NOT NULL DEFAULT '',
    object_ref     TEXT,           -- serialized graph in object storage (large snapshots)
    published_by   TEXT NOT NULL,
    published_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (version_id),
    FOREIGN KEY (version_id, model_id) REFERENCES architecture_versions(id, model_id) ON DELETE CASCADE,
    FOREIGN KEY (model_id, version_number) REFERENCES architecture_versions(model_id, version_number) ON DELETE CASCADE
);

-- ---------------------------------------------------------------------------
-- Integrity triggers
-- ---------------------------------------------------------------------------

-- Versions: immutable; parent must be the previous version of the same model and tenant.
CREATE OR REPLACE FUNCTION arch_versions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent RECORD;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'architecture_versions are immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.version_number > 0 THEN
        SELECT model_id, version_number, tenant_id INTO parent FROM architecture_versions WHERE id = NEW.parent_version_id;
        IF NOT FOUND OR parent.model_id <> NEW.model_id OR parent.tenant_id <> NEW.tenant_id
           OR parent.version_number <> NEW.version_number - 1 THEN
            RAISE EXCEPTION 'version % of model % must have version % of the same model as parent',
                NEW.version_number, NEW.model_id, NEW.version_number - 1 USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM architecture_models WHERE id = NEW.model_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'version tenant does not match its model' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_versions_guard ON architecture_versions;
CREATE TRIGGER trg_arch_versions_guard BEFORE INSERT OR UPDATE ON architecture_versions
    FOR EACH ROW EXECUTE FUNCTION arch_versions_guard();

-- Models: identity is immutable; the current version may only move forward.
CREATE OR REPLACE FUNCTION arch_models_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_n INT; new_n INT;
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id OR NEW.project_id <> OLD.project_id THEN
        RAISE EXCEPTION 'architecture model identity is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.current_version_id IS DISTINCT FROM OLD.current_version_id THEN
        IF NEW.current_version_id IS NULL THEN
            RAISE EXCEPTION 'current_version_id cannot be cleared' USING ERRCODE = 'check_violation';
        END IF;
        SELECT version_number INTO new_n FROM architecture_versions WHERE id = NEW.current_version_id AND model_id = NEW.id;
        SELECT version_number INTO old_n FROM architecture_versions WHERE id = OLD.current_version_id;
        IF new_n IS NULL OR (old_n IS NOT NULL AND new_n <= old_n) THEN
            RAISE EXCEPTION 'current version must be a later version of the same model' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_models_guard ON architecture_models;
CREATE TRIGGER trg_arch_models_guard BEFORE UPDATE ON architecture_models
    FOR EACH ROW EXECUTE FUNCTION arch_models_guard();

-- Nodes/edges: the only permitted update is closing an open revision.
CREATE OR REPLACE FUNCTION arch_rows_close_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.valid_to_version IS NOT NULL OR NEW.valid_to_version IS NULL
       OR (to_jsonb(NEW) - 'valid_to_version') IS DISTINCT FROM (to_jsonb(OLD) - 'valid_to_version') THEN
        RAISE EXCEPTION '% rows are append-only: only an open revision may be closed', TG_TABLE_NAME
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_nodes_close_only ON architecture_nodes;
CREATE TRIGGER trg_arch_nodes_close_only BEFORE UPDATE ON architecture_nodes
    FOR EACH ROW EXECUTE FUNCTION arch_rows_close_only();
DROP TRIGGER IF EXISTS trg_arch_edges_close_only ON architecture_edges;
CREATE TRIGGER trg_arch_edges_close_only BEFORE UPDATE ON architecture_edges
    FOR EACH ROW EXECUTE FUNCTION arch_rows_close_only();

-- Node revisions keep their logical identity: same model, tenant and type; contiguous
-- revision numbers.
CREATE OR REPLACE FUNCTION arch_nodes_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE prev RECORD;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM architecture_models WHERE id = NEW.model_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'node tenant does not match its model' USING ERRCODE = 'check_violation';
    END IF;
    SELECT model_id, tenant_id, node_type, revision INTO prev FROM architecture_nodes
        WHERE node_id = NEW.node_id ORDER BY revision DESC LIMIT 1;
    IF FOUND THEN
        IF prev.model_id <> NEW.model_id OR prev.tenant_id <> NEW.tenant_id OR prev.node_type <> NEW.node_type THEN
            RAISE EXCEPTION 'node % cannot change model, tenant or type', NEW.node_id USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.revision <> prev.revision + 1 THEN
            RAISE EXCEPTION 'node % revision must be %', NEW.node_id, prev.revision + 1 USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.revision <> 1 THEN
        RAISE EXCEPTION 'first revision of node % must be 1', NEW.node_id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_nodes_identity ON architecture_nodes;
CREATE TRIGGER trg_arch_nodes_identity BEFORE INSERT ON architecture_nodes
    FOR EACH ROW EXECUTE FUNCTION arch_nodes_identity();

CREATE OR REPLACE FUNCTION arch_edges_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE prev RECORD;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM architecture_models WHERE id = NEW.model_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'edge tenant does not match its model' USING ERRCODE = 'check_violation';
    END IF;
    SELECT model_id, source_node_id, target_node_id, relation_type, revision INTO prev FROM architecture_edges
        WHERE edge_id = NEW.edge_id ORDER BY revision DESC LIMIT 1;
    IF FOUND THEN
        IF prev.model_id <> NEW.model_id OR prev.source_node_id <> NEW.source_node_id
           OR prev.target_node_id <> NEW.target_node_id OR prev.relation_type <> NEW.relation_type THEN
            RAISE EXCEPTION 'edge % cannot change model, endpoints or relation type', NEW.edge_id USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.revision <> prev.revision + 1 THEN
            RAISE EXCEPTION 'edge % revision must be %', NEW.edge_id, prev.revision + 1 USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.revision <> 1 THEN
        RAISE EXCEPTION 'first revision of edge % must be 1', NEW.edge_id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_edges_identity ON architecture_edges;
CREATE TRIGGER trg_arch_edges_identity BEFORE INSERT ON architecture_edges
    FOR EACH ROW EXECUTE FUNCTION arch_edges_identity();

-- edge_endpoints_exist, checked at commit (deferred) so a patch can add nodes and the
-- edges between them in any order. A node must be live for the whole lifetime of every
-- edge that references it; since a node's revisions are contiguous when edited, it is
-- enough to require that no edge is live at a version where its endpoint is not.
CREATE OR REPLACE FUNCTION arch_node_live_at(m UUID, n UUID, v INT) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM architecture_nodes
                   WHERE model_id = m AND node_id = n AND valid_from_version <= v
                     AND (valid_to_version IS NULL OR valid_to_version > v));
$$;

CREATE OR REPLACE FUNCTION arch_edges_endpoints_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT arch_node_live_at(NEW.model_id, NEW.source_node_id, NEW.valid_from_version)
       OR NOT arch_node_live_at(NEW.model_id, NEW.target_node_id, NEW.valid_from_version) THEN
        RAISE EXCEPTION 'edge % references a node that does not exist in model % at version %',
            NEW.edge_id, NEW.model_id, NEW.valid_from_version USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_edges_endpoints ON architecture_edges;
CREATE CONSTRAINT TRIGGER trg_arch_edges_endpoints AFTER INSERT ON architecture_edges
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION arch_edges_endpoints_check();

CREATE OR REPLACE FUNCTION arch_nodes_close_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- A closed node with a successor revision starting at the same version is an edit,
    -- not a removal. A removal must not leave any edge live at the removal version.
    -- (Together with the edge insert check this guarantees an edge's endpoints are live
    -- for its whole lifetime: any gap in a node's lifetime begins with a removal.)
    IF NOT arch_node_live_at(NEW.model_id, NEW.node_id, NEW.valid_to_version) AND EXISTS (
        SELECT 1 FROM architecture_edges e
        WHERE e.model_id = NEW.model_id
          AND (e.source_node_id = NEW.node_id OR e.target_node_id = NEW.node_id)
          AND e.valid_from_version <= NEW.valid_to_version
          AND (e.valid_to_version IS NULL OR e.valid_to_version > NEW.valid_to_version)) THEN
        RAISE EXCEPTION 'node % was removed at version % while edges still reference it',
            NEW.node_id, NEW.valid_to_version USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS trg_arch_nodes_close_check ON architecture_nodes;
CREATE CONSTRAINT TRIGGER trg_arch_nodes_close_check AFTER UPDATE OF valid_to_version ON architecture_nodes
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION arch_nodes_close_check();

-- Patches: identity and content are immutable; a decided patch is frozen.
CREATE OR REPLACE FUNCTION graph_patches_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'graph patch % is already %', OLD.id, OLD.status USING ERRCODE = 'restrict_violation';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'risk_summary', 'approval_state', 'approved_by', 'approved_at',
                              'result', 'resulting_version_id', 'rollback_version_id', 'decided_at'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['status', 'risk_summary', 'approval_state', 'approved_by', 'approved_at',
                              'result', 'resulting_version_id', 'rollback_version_id', 'decided_at']) THEN
        RAISE EXCEPTION 'graph patch content is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.approval_state IN ('APPROVED', 'REJECTED') AND NEW.approval_state <> OLD.approval_state THEN
        RAISE EXCEPTION 'graph patch approval is already %', OLD.approval_state USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_graph_patches_guard ON graph_patches;
CREATE TRIGGER trg_graph_patches_guard BEFORE UPDATE ON graph_patches
    FOR EACH ROW EXECUTE FUNCTION graph_patches_guard();

-- Snapshots: immutable and bound to their version's graph hash.
CREATE OR REPLACE FUNCTION graph_snapshots_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'graph snapshots are immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM architecture_versions
                   WHERE id = NEW.version_id AND model_id = NEW.model_id AND tenant_id = NEW.tenant_id
                     AND version_number = NEW.version_number AND graph_hash = NEW.graph_hash) THEN
        RAISE EXCEPTION 'snapshot must match its version''s tenant, number and graph hash' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_graph_snapshots_guard ON graph_snapshots;
CREATE TRIGGER trg_graph_snapshots_guard BEFORE INSERT OR UPDATE ON graph_snapshots
    FOR EACH ROW EXECUTE FUNCTION graph_snapshots_guard();

-- ---------------------------------------------------------------------------
-- 1.7 Privileges & RLS (no DELETE for either role: history is append-only)
-- ---------------------------------------------------------------------------

GRANT SELECT ON graph_invariants TO systera_app, systera_system;
GRANT SELECT, INSERT, UPDATE ON architecture_models, architecture_nodes, architecture_edges, graph_patches
    TO systera_app, systera_system;
GRANT SELECT, INSERT ON architecture_versions, graph_snapshots TO systera_app, systera_system;
GRANT USAGE ON SEQUENCE architecture_nodes_row_id_seq, architecture_edges_row_id_seq TO systera_app, systera_system;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['architecture_models', 'architecture_versions', 'architecture_nodes',
                             'architecture_edges', 'graph_patches', 'graph_snapshots'] LOOP
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
