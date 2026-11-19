-- Stage 2 (Section 5): Decision Ledger (ADRs, §9.4).
--   * Enforced ADR state machine: PROPOSED, ACCEPTED, REJECTED, SUPERSEDED, NEEDS_REVIEW.
--   * Complete ADR fields: context, chosen_option, alternatives, rationale, trade_offs,
--     consequences, evidence_refs, confidence_band, owner, affected_nodes.
--   * Supersession chain (supersedes / superseded_by) and affected-node invalidation.

CREATE TABLE IF NOT EXISTS decisions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    model_version     INT NOT NULL DEFAULT 0 CHECK (model_version >= 0),
    adr_number        INT NOT NULL CHECK (adr_number >= 1),
    title             TEXT NOT NULL CHECK (length(trim(title)) > 0),
    context           TEXT NOT NULL CHECK (length(trim(context)) > 0),
    chosen_option     TEXT NOT NULL CHECK (length(trim(chosen_option)) > 0),
    alternatives_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    rationale         TEXT NOT NULL CHECK (length(trim(rationale)) > 0),
    trade_offs        TEXT NOT NULL DEFAULT '',
    consequences      TEXT NOT NULL DEFAULT '',
    evidence_refs     JSONB NOT NULL DEFAULT '[]'::jsonb,
    confidence_band   TEXT NOT NULL DEFAULT 'MEDIUM' CHECK (confidence_band IN ('HIGH', 'MEDIUM', 'LOW')),
    owner_type        TEXT NOT NULL CHECK (owner_type IN ('USER', 'AGENT', 'SYSTEM')),
    owner_id          TEXT NOT NULL CHECK (length(trim(owner_id)) > 0),
    affected_nodes    UUID[] NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'PROPOSED' CHECK (status IN (
                          'PROPOSED', 'ACCEPTED', 'REJECTED', 'SUPERSEDED', 'NEEDS_REVIEW'
                      )),
    supersedes        UUID REFERENCES decisions(id),
    superseded_by     UUID REFERENCES decisions(id),
    review_reason     TEXT NOT NULL DEFAULT '',
    run_id            UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, adr_number)
);

CREATE INDEX IF NOT EXISTS idx_decisions_model_status ON decisions (model_id, status);
CREATE INDEX IF NOT EXISTS idx_decisions_affected_nodes ON decisions USING GIN (affected_nodes);
CREATE INDEX IF NOT EXISTS idx_decisions_supersedes ON decisions (supersedes) WHERE supersedes IS NOT NULL;

GRANT SELECT, INSERT, UPDATE ON decisions TO systera_app, systera_system;
GRANT SELECT, UPDATE ON decisions TO systera_graph;

ALTER TABLE decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE decisions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_decisions ON decisions;
CREATE POLICY tenant_isolation_decisions ON decisions TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_decisions ON decisions;
CREATE POLICY system_all_decisions ON decisions TO systera_system
    USING (true) WITH CHECK (true);

DROP POLICY IF EXISTS graph_invalidation_decisions ON decisions;
CREATE POLICY graph_invalidation_decisions ON decisions TO systera_graph
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);
