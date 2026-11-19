-- Phase 3, Section 7 (Checklist 7.1-7.5): Architecture Counterfactual & Decision Matrix.
-- Stores >= 2 viable architectural alternatives per model or website, each linked to its
-- own graph version (enabling canonical version diffing), scored against 8 explicit
-- constraints, with "when appropriate" explanations and ADR linkage.

CREATE TABLE IF NOT EXISTS architecture_alternatives (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID REFERENCES architecture_models(id) ON DELETE CASCADE,
    website_id        UUID REFERENCES website_profiles(id) ON DELETE CASCADE,
    set_id            UUID NOT NULL,
    style             TEXT NOT NULL CHECK (length(trim(style)) > 0),
    name              TEXT NOT NULL CHECK (length(trim(name)) > 0),
    summary           TEXT NOT NULL DEFAULT '',
    when_appropriate  TEXT NOT NULL CHECK (length(trim(when_appropriate)) > 0),
    pros              TEXT NOT NULL DEFAULT '',
    cons              TEXT NOT NULL DEFAULT '',
    graph_version     INT NOT NULL DEFAULT 0 CHECK (graph_version >= 0),
    is_recommended    BOOLEAN NOT NULL DEFAULT FALSE,
    scores_json       JSONB NOT NULL DEFAULT '{}'::jsonb,
    weighted_score    NUMERIC(6, 3) NOT NULL DEFAULT 0,
    constraints_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    decision_id       UUID REFERENCES decisions(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (model_id IS NOT NULL OR website_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_arch_alternatives_model ON architecture_alternatives (model_id, set_id, weighted_score DESC);
CREATE INDEX IF NOT EXISTS idx_arch_alternatives_website ON architecture_alternatives (website_id, set_id, weighted_score DESC);
CREATE INDEX IF NOT EXISTS idx_arch_alternatives_tenant ON architecture_alternatives (tenant_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON architecture_alternatives TO systera_app, systera_system;

ALTER TABLE architecture_alternatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE architecture_alternatives FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_architecture_alternatives ON architecture_alternatives;
CREATE POLICY tenant_isolation_architecture_alternatives ON architecture_alternatives TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_architecture_alternatives ON architecture_alternatives;
CREATE POLICY system_all_architecture_alternatives ON architecture_alternatives TO systera_system
    USING (true) WITH CHECK (true);
