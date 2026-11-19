-- Stage 3 (spec §2.7, §8.5): Engineering Learning Mode.
-- Lessons are derived on read from the canonical graph, ADR ledger and Engineering Pulse
-- knowledge; only user challenges and their revised explanations are stored. A challenge
-- never mutates the graph. node_id is a versioned graph node id (no FK: node rows are
-- temporal), scoped by model and tenant.

CREATE TABLE IF NOT EXISTS learning_challenges (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id       UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    node_id        UUID NOT NULL,
    graph_version  INT NOT NULL CHECK (graph_version >= 0),
    revision       INT NOT NULL CHECK (revision >= 1),
    challenge      TEXT NOT NULL CHECK (length(trim(challenge)) > 0 AND char_length(challenge) <= 2000),
    concern        TEXT NOT NULL CHECK (concern IN ('ALTERNATIVE', 'COMPLEXITY', 'COST', 'SCALE_PERFORMANCE',
                                                    'CONSISTENCY', 'RELIABILITY', 'SECURITY', 'GENERAL')),
    response_json  JSONB NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, node_id, revision)
);

CREATE INDEX IF NOT EXISTS idx_learning_challenges_tenant ON learning_challenges (tenant_id, created_at DESC);

GRANT SELECT, INSERT, DELETE ON learning_challenges TO systera_app, systera_system;

ALTER TABLE learning_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_challenges FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_learning_challenges ON learning_challenges;
CREATE POLICY tenant_isolation_learning_challenges ON learning_challenges TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_learning_challenges ON learning_challenges;
CREATE POLICY system_all_learning_challenges ON learning_challenges TO systera_system
    USING (true) WITH CHECK (true);
