-- Stage 4 (spec §23.4, §26): design-time Architecture Digital Twin.
-- A twin build is an immutable record of one graph version plus explicit workload,
-- environment and failure assumptions, the normalized twin and its six-dimension
-- validation report. Builds are deterministic, so the same graph version, assumptions and
-- builder version identify exactly one build (reproducibility key).
-- Every build describes declared design intent (basis = DESIGN_INTENT), never telemetry.

CREATE TABLE IF NOT EXISTS digital_twins (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    graph_version     INT NOT NULL CHECK (graph_version >= 0),
    graph_hash        TEXT NOT NULL CHECK (length(graph_hash) > 0),
    builder_version   TEXT NOT NULL CHECK (length(builder_version) > 0),
    assumptions_hash  TEXT NOT NULL CHECK (assumptions_hash ~ '^[0-9a-f]{64}$'),
    basis             TEXT NOT NULL DEFAULT 'DESIGN_INTENT' CHECK (basis = 'DESIGN_INTENT'),
    status            TEXT NOT NULL CHECK (status IN ('VALID', 'VALID_WITH_WARNINGS', 'INVALID')),
    error_count       INT NOT NULL CHECK (error_count >= 0),
    warning_count     INT NOT NULL CHECK (warning_count >= 0),
    assumptions_json  JSONB NOT NULL,
    twin_json         JSONB NOT NULL,
    report_json       JSONB NOT NULL,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((status = 'INVALID') = (error_count > 0)),
    UNIQUE (model_id, graph_version, assumptions_hash, builder_version)
);

CREATE INDEX IF NOT EXISTS idx_digital_twins_model ON digital_twins (model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_digital_twins_tenant ON digital_twins (tenant_id, created_at DESC);

-- Immutable: no UPDATE for anyone; DELETE only through model/tenant cascade.
GRANT SELECT, INSERT ON digital_twins TO systera_app, systera_system;

ALTER TABLE digital_twins ENABLE ROW LEVEL SECURITY;
ALTER TABLE digital_twins FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_digital_twins ON digital_twins;
CREATE POLICY tenant_isolation_digital_twins ON digital_twins TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_digital_twins ON digital_twins;
CREATE POLICY system_all_digital_twins ON digital_twins TO systera_system
    USING (true) WITH CHECK (true);
