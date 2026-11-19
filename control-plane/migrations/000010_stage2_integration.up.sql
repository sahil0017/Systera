-- Stage 2 (Section 8): Integration with Stage 1.
--   * 8.4: Seed specialist agents beyond requirements_analyst (HLD, data modeller, API, security, reviewer).
--   * 8.5: Human approval gate (approval_requests) for high-risk graph change sets.

-- ---------------------------------------------------------------------------
-- 1. Specialist agents (Checklist 8.4)
-- ---------------------------------------------------------------------------

INSERT INTO agents (agent_type, display_name, description, system_prompt, allowed_tools, max_tokens)
VALUES
(
    'hld_architect',
    'High-Level Design Architect',
    'Defines system boundaries, services, components, communication protocols, and proposes canonical graph patches and ADRs.',
    'You are the High-Level Design Architect. Read the requirements and current architecture graph, '
    'then propose services, components, and edges via graph.propose_patch. Document key architectural '
    'trade-offs via decision.propose and cite evidence via evidence.make_claim.',
    ARRAY['run.context', 'task.dependency_results', 'graph.read', 'graph.query', 'graph.propose_patch', 'evidence.capture', 'evidence.make_claim', 'evidence.record_assumption', 'decision.propose', 'decision.list'],
    8192
),
(
    'data_modeller',
    'Database & Data Modeller',
    'Designs logical data entities, tables, caches, queues, partitioning, and consistency boundaries in the canonical graph.',
    'You are the Data Modeller. Inspect the architecture graph and requirements, then model databases, '
    'tables, caches, and read/write edges using graph.propose_patch. Record consistency assumptions '
    'via evidence.record_assumption and storage ADRs via decision.propose.',
    ARRAY['run.context', 'task.dependency_results', 'graph.read', 'graph.query', 'graph.propose_patch', 'evidence.capture', 'evidence.make_claim', 'evidence.record_assumption', 'decision.propose', 'decision.list'],
    8192
),
(
    'api_architect',
    'API & Contract Architect',
    'Defines API endpoints, event topics, synchronous/asynchronous contracts, and idempotency semantics.',
    'You are the API Architect. Add API and event/topic nodes and calls/publishes/subscribes edges '
    'via graph.propose_patch, ensuring every API node is owned by a service.',
    ARRAY['run.context', 'task.dependency_results', 'graph.read', 'graph.query', 'graph.propose_patch', 'evidence.make_claim', 'decision.propose', 'decision.list'],
    4096
),
(
    'security_architect',
    'Security & Trust Boundary Architect',
    'Models trust boundaries, authentication edges, secret boundaries, and threat nodes in the canonical graph.',
    'You are the Security Architect. Model secret_boundary, threat, and authenticates edges via '
    'graph.propose_patch and record security ADRs via decision.propose.',
    ARRAY['run.context', 'task.dependency_results', 'graph.read', 'graph.query', 'graph.propose_patch', 'evidence.make_claim', 'decision.propose', 'decision.list'],
    4096
),
(
    'independent_reviewer',
    'Independent Architecture Reviewer',
    'Reviews claims, ADRs, and the canonical graph for gaps, contradictions, and unsupported assumptions.',
    'You are the Independent Reviewer. Inspect the architecture graph and ADRs using graph.read, '
    'graph.query, and decision.list. Challenge unsupported claims and identify missing failure modes.',
    ARRAY['run.context', 'task.dependency_results', 'graph.read', 'graph.query', 'decision.list'],
    4096
)
ON CONFLICT (agent_type) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Human approval gate for high-risk change sets (Checklist 8.5)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS approval_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE CASCADE,
    model_id        UUID REFERENCES architecture_models(id) ON DELETE CASCADE,
    run_id          UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    action_type     TEXT NOT NULL CHECK (action_type IN ('HIGH_RISK_GRAPH_PATCH', 'EXTERNAL_PUBLISH', 'PRODUCTION_CHANGE')),
    resource_id     TEXT NOT NULL,
    risk_level      TEXT NOT NULL CHECK (risk_level IN ('LOW', 'MEDIUM', 'HIGH')),
    risk_reasons    JSONB NOT NULL DEFAULT '[]'::jsonb,
    payload_hash    TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'EXPIRED')),
    requested_by    TEXT NOT NULL,
    decided_by      TEXT,
    decision_reason TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_approval_requests_tenant_status ON approval_requests (tenant_id, status, created_at DESC);

GRANT SELECT, INSERT, UPDATE ON approval_requests TO systera_app, systera_system;

ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_requests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_approval_requests ON approval_requests;
CREATE POLICY tenant_isolation_approval_requests ON approval_requests TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS system_all_approval_requests ON approval_requests;
CREATE POLICY system_all_approval_requests ON approval_requests TO systera_system
    USING (true) WITH CHECK (true);
