-- Stage 3 (Section 5): Architecture Fingerprint (§2.4, Checklist 5.1-5.5).
--   * architecture_fingerprints: structured, reusable representation of observable
--     capabilities + proposed architecture across 8 graph layers, quality attributes,
--     and evidence/confidence/provenance links.
--   * Explicitly not a factual map of hidden infrastructure.
--   * Cascades on website_profiles / browser_sessions deletion; RLS enforced.

CREATE TABLE IF NOT EXISTS architecture_fingerprints (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    website_id              UUID NOT NULL REFERENCES website_profiles(id) ON DELETE CASCADE,
    session_id              UUID NOT NULL REFERENCES browser_sessions(id) ON DELETE CASCADE,
    model_id                UUID REFERENCES architecture_models(id) ON DELETE SET NULL,
    graph_version           INT NOT NULL DEFAULT 0 CHECK (graph_version >= 0),
    domain                  TEXT NOT NULL CHECK (length(trim(domain)) > 0),
    disclaimer              TEXT NOT NULL DEFAULT 'Structured hypothesis of observable capabilities and proposed architecture; not a factual map of hidden infrastructure.',
    layers_json             JSONB NOT NULL DEFAULT '{}'::jsonb,
    quality_attributes_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    provenance_json         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (website_id)
);

CREATE INDEX IF NOT EXISTS idx_architecture_fingerprints_tenant ON architecture_fingerprints (tenant_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_architecture_fingerprints_model ON architecture_fingerprints (model_id) WHERE model_id IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON architecture_fingerprints TO systera_app, systera_system;

ALTER TABLE architecture_fingerprints ENABLE ROW LEVEL SECURITY;
ALTER TABLE architecture_fingerprints FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_architecture_fingerprints ON architecture_fingerprints;
CREATE POLICY tenant_isolation_architecture_fingerprints ON architecture_fingerprints TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_architecture_fingerprints ON architecture_fingerprints;
CREATE POLICY system_all_architecture_fingerprints ON architecture_fingerprints TO systera_system
    USING (true) WITH CHECK (true);
