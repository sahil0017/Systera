-- Stage 2 (Checklist 4.1-4.4): Evidence ledger, claims, assumptions and revalidation.
--
--   evidence_records  immutable captured sources (hash, trust, access policy, expiry).
--   assumptions       explicit, owned assumptions: never implied facts.
--   claims            statements about the architecture with a class (OBSERVED /
--                     INFERRED / PROPOSED), a calibrated confidence band, review state,
--                     a lifecycle (ACTIVE / STALE / RETRACTED / SUPERSEDED) and expiry.
--   claim_evidence    claim <-> evidence links (SUPPORTS / CONTRADICTS / CONTEXT).
--   claim_assumptions claim -> assumptions it depends on.
--
-- Spec Appendix D is enforced here, at commit, for every ACTIVE claim:
--   OBSERVED  needs >= 1 SUPPORTS link to active, unexpired DIRECT evidence and may not
--             rest on assumptions (an observation is not an assumption);
--   INFERRED  needs reasoning and >= 1 SUPPORTS link to active, unexpired evidence;
--   PROPOSED  is a recommendation and may stand on reasoning or assumptions alone.
-- Every ACTIVE claim (any class) must also be unexpired and rest only on ACTIVE/VALIDATED,
-- unexpired assumptions. claim_requirements_unmet() is the single definition of "valid"
-- used at commit, by revalidation and by the staleness sweep (4.4), which marks evidence
-- EXPIRED, assumptions NEEDS_REVALIDATION and claims STALE when it stops holding.

-- ---------------------------------------------------------------------------
-- Evidence
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS evidence_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id        UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    source_ref      TEXT NOT NULL CHECK (length(source_ref) BETWEEN 1 AND 2000),
    source_type     TEXT NOT NULL CHECK (source_type IN (
                        'SOURCE_DOCUMENT', 'WEB_PAGE', 'OBSERVATION', 'TEST_RESULT', 'MEASUREMENT',
                        'USER_STATEMENT', 'SIMULATION', 'RETRIEVED_KNOWLEDGE', 'AGENT_ANALYSIS')),
    -- DIRECT: the source material itself; INDIRECT: derived from other material.
    directness      TEXT NOT NULL GENERATED ALWAYS AS (
                        CASE WHEN source_type IN ('SIMULATION', 'RETRIEVED_KNOWLEDGE', 'AGENT_ANALYSIS')
                             THEN 'INDIRECT' ELSE 'DIRECT' END) STORED,
    content_hash    TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    excerpt         TEXT NOT NULL DEFAULT '' CHECK (length(excerpt) <= 4000),
    captured_at     TIMESTAMPTZ NOT NULL,
    trust_level     TEXT NOT NULL CHECK (trust_level IN ('VERIFIED', 'HIGH', 'MEDIUM', 'LOW', 'UNVERIFIED')),
    access_policy   TEXT NOT NULL DEFAULT 'TENANT' CHECK (access_policy IN ('PUBLIC', 'TENANT', 'RESTRICTED')),
    expires_at      TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'EXPIRED', 'REVOKED')),
    status_reason   TEXT,
    status_changed_at TIMESTAMPTZ,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('USER', 'AGENT', 'SYSTEM')),
    created_by      TEXT NOT NULL,
    run_id          UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    agent_task_id   UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, source_ref, content_hash),
    UNIQUE (id, model_id),
    CHECK (expires_at IS NULL OR expires_at > captured_at),
    CHECK ((status = 'ACTIVE') = (status_changed_at IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_evidence_model ON evidence_records(model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evidence_expiry ON evidence_records(expires_at) WHERE status = 'ACTIVE' AND expires_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Assumptions
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS assumptions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id          UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    statement         TEXT NOT NULL CHECK (length(statement) BETWEEN 1 AND 2000),
    rationale         TEXT NOT NULL CHECK (length(rationale) BETWEEN 1 AND 4000),
    owner             TEXT NOT NULL,
    risk_if_wrong     TEXT NOT NULL CHECK (risk_if_wrong IN ('LOW', 'MEDIUM', 'HIGH')),
    validation_method TEXT NOT NULL DEFAULT '',
    subject_node_ids  UUID[] NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'ACTIVE'
                          CHECK (status IN ('ACTIVE', 'VALIDATED', 'INVALIDATED', 'NEEDS_REVALIDATION', 'RETIRED')),
    status_note       TEXT,
    status_changed_by TEXT,
    status_changed_at TIMESTAMPTZ,
    expires_at        TIMESTAMPTZ,
    created_by_type   TEXT NOT NULL CHECK (created_by_type IN ('USER', 'AGENT', 'SYSTEM')),
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (id, model_id),
    CHECK (expires_at IS NULL OR expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS idx_assumptions_model ON assumptions(model_id, status);

-- ---------------------------------------------------------------------------
-- Claims
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS claims (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id           UUID NOT NULL REFERENCES architecture_models(id) ON DELETE CASCADE,
    statement          TEXT NOT NULL CHECK (length(statement) BETWEEN 1 AND 2000),
    claim_class        TEXT NOT NULL CHECK (claim_class IN ('OBSERVED', 'INFERRED', 'PROPOSED')),
    reasoning          TEXT NOT NULL DEFAULT '' CHECK (length(reasoning) <= 8000),
    confidence_band    TEXT NOT NULL CHECK (confidence_band IN ('HIGH', 'MEDIUM', 'LOW')),
    -- Optional calibrated score; must fall inside its band.
    confidence_score   NUMERIC(4, 3) CHECK (confidence_score BETWEEN 0 AND 1),
    subject_node_ids   UUID[] NOT NULL DEFAULT '{}',
    graph_version      INT NOT NULL CHECK (graph_version >= 0),
    review_status      TEXT NOT NULL DEFAULT 'UNREVIEWED'
                           CHECK (review_status IN ('UNREVIEWED', 'ACCEPTED', 'DISPUTED', 'REJECTED')),
    reviewer           TEXT,
    review_note        TEXT,
    reviewed_at        TIMESTAMPTZ,
    lifecycle          TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (lifecycle IN ('ACTIVE', 'STALE', 'RETRACTED', 'SUPERSEDED')),
    stale_reason       TEXT,
    stale_since        TIMESTAMPTZ,
    expires_at         TIMESTAMPTZ,
    revalidate_after   INTERVAL,
    last_validated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_by      UUID REFERENCES claims(id),
    retracted_reason   TEXT,
    created_by_type    TEXT NOT NULL CHECK (created_by_type IN ('USER', 'AGENT', 'SYSTEM')),
    created_by         TEXT NOT NULL,
    run_id             UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    agent_task_id      UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (id, model_id),
    FOREIGN KEY (model_id, graph_version) REFERENCES architecture_versions(model_id, version_number),
    CHECK (confidence_score IS NULL
           OR (confidence_band = 'HIGH' AND confidence_score >= 0.8)
           OR (confidence_band = 'MEDIUM' AND confidence_score >= 0.5 AND confidence_score < 0.8)
           OR (confidence_band = 'LOW' AND confidence_score < 0.5)),
    CHECK (claim_class <> 'INFERRED' OR length(btrim(reasoning)) > 0),
    CHECK ((lifecycle = 'STALE') = (stale_since IS NOT NULL AND stale_reason IS NOT NULL)),
    CHECK ((lifecycle = 'SUPERSEDED') = (superseded_by IS NOT NULL)),
    CHECK ((lifecycle = 'RETRACTED') = (retracted_reason IS NOT NULL)),
    CHECK ((review_status = 'UNREVIEWED') = (reviewer IS NULL AND reviewed_at IS NULL)),
    CHECK (reviewer IS NULL OR reviewer <> created_by),
    CHECK (revalidate_after IS NULL OR revalidate_after > INTERVAL '0')
);
CREATE INDEX IF NOT EXISTS idx_claims_model ON claims(model_id, lifecycle, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_claims_subjects ON claims USING gin (subject_node_ids);
CREATE INDEX IF NOT EXISTS idx_claims_expiry ON claims(expires_at) WHERE lifecycle = 'ACTIVE' AND expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS claim_evidence (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id    UUID NOT NULL,
    claim_id    UUID NOT NULL,
    evidence_id UUID NOT NULL,
    relation    TEXT NOT NULL CHECK (relation IN ('SUPPORTS', 'CONTRADICTS', 'CONTEXT')),
    note        TEXT NOT NULL DEFAULT '',
    linked_by   TEXT NOT NULL,
    linked_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (claim_id, evidence_id),
    -- Same-model links only.
    FOREIGN KEY (claim_id, model_id) REFERENCES claims(id, model_id) ON DELETE CASCADE,
    FOREIGN KEY (evidence_id, model_id) REFERENCES evidence_records(id, model_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_claim_evidence_evidence ON claim_evidence(evidence_id);

CREATE TABLE IF NOT EXISTS claim_assumptions (
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_id      UUID NOT NULL,
    claim_id      UUID NOT NULL,
    assumption_id UUID NOT NULL,
    linked_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (claim_id, assumption_id),
    FOREIGN KEY (claim_id, model_id) REFERENCES claims(id, model_id) ON DELETE CASCADE,
    FOREIGN KEY (assumption_id, model_id) REFERENCES assumptions(id, model_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_claim_assumptions_assumption ON claim_assumptions(assumption_id);

-- ---------------------------------------------------------------------------
-- Integrity triggers
-- ---------------------------------------------------------------------------

-- Evidence content is immutable; status moves ACTIVE -> EXPIRED | REVOKED, once.
CREATE OR REPLACE FUNCTION evidence_records_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'ACTIVE' THEN
        RAISE EXCEPTION 'evidence % is already %', OLD.id, OLD.status USING ERRCODE = 'restrict_violation';
    END IF;
    -- directness is a STORED generated column: it is NULL in NEW inside a BEFORE trigger
    -- (computed afterwards) and cannot be written directly, so it is excluded here.
    IF (to_jsonb(NEW) - ARRAY['status', 'status_reason', 'status_changed_at', 'directness'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'status_reason', 'status_changed_at', 'directness']) THEN
        RAISE EXCEPTION 'evidence records are immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_evidence_records_guard ON evidence_records;
CREATE TRIGGER trg_evidence_records_guard BEFORE UPDATE ON evidence_records
    FOR EACH ROW EXECUTE FUNCTION evidence_records_guard();

-- Assumption statements are immutable; only status (and its note/actor/time) changes.
-- INVALIDATED and RETIRED are final.
CREATE OR REPLACE FUNCTION assumptions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('INVALIDATED', 'RETIRED') THEN
        RAISE EXCEPTION 'assumption % is already %', OLD.id, OLD.status USING ERRCODE = 'restrict_violation';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'status_note', 'status_changed_by', 'status_changed_at', 'expires_at'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'status_note', 'status_changed_by', 'status_changed_at', 'expires_at']) THEN
        RAISE EXCEPTION 'assumption content is immutable; record a new assumption instead' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_assumptions_guard ON assumptions;
CREATE TRIGGER trg_assumptions_guard BEFORE UPDATE ON assumptions
    FOR EACH ROW EXECUTE FUNCTION assumptions_guard();

-- Claim content is immutable (edits supersede); RETRACTED and SUPERSEDED are final.
CREATE OR REPLACE FUNCTION claims_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mutable TEXT[] := ARRAY['review_status', 'reviewer', 'review_note', 'reviewed_at', 'lifecycle',
                                'stale_reason', 'stale_since', 'expires_at', 'last_validated_at',
                                'superseded_by', 'retracted_reason'];
BEGIN
    IF OLD.lifecycle IN ('RETRACTED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'claim % is already %', OLD.id, OLD.lifecycle USING ERRCODE = 'restrict_violation';
    END IF;
    IF (to_jsonb(NEW) - mutable) IS DISTINCT FROM (to_jsonb(OLD) - mutable) THEN
        RAISE EXCEPTION 'claim content is immutable; supersede it with a new claim' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_claims_guard ON claims;
CREATE TRIGGER trg_claims_guard BEFORE UPDATE ON claims
    FOR EACH ROW EXECUTE FUNCTION claims_guard();

-- claim_evidence_requirements (Appendix D), checked at commit for ACTIVE claims.
CREATE OR REPLACE FUNCTION claim_requirements_unmet(c claims) RETURNS TEXT
LANGUAGE plpgsql STABLE AS $$
DECLARE supports INT;
BEGIN
    IF c.lifecycle <> 'ACTIVE' THEN
        RETURN NULL;
    END IF;
    IF c.expires_at IS NOT NULL AND c.expires_at <= NOW() THEN
        RETURN 'claim has expired and must be revalidated';
    END IF;
    IF EXISTS (SELECT 1 FROM claim_assumptions ca JOIN assumptions a ON a.id = ca.assumption_id
               WHERE ca.claim_id = c.id
                 AND (a.status NOT IN ('ACTIVE', 'VALIDATED') OR (a.expires_at IS NOT NULL AND a.expires_at <= NOW()))) THEN
        RETURN 'claim rests on an assumption that is invalidated, retired, expired or awaiting revalidation';
    END IF;
    IF c.claim_class = 'PROPOSED' THEN
        RETURN NULL;  -- recommendations need no evidence
    END IF;
    SELECT count(*) INTO supports
    FROM claim_evidence ce JOIN evidence_records ev ON ev.id = ce.evidence_id
    WHERE ce.claim_id = c.id AND ce.relation = 'SUPPORTS' AND ev.status = 'ACTIVE'
      AND (ev.expires_at IS NULL OR ev.expires_at > NOW())
      AND (c.claim_class = 'INFERRED' OR ev.directness = 'DIRECT');
    IF c.claim_class = 'OBSERVED' THEN
        IF supports = 0 THEN
            RETURN 'OBSERVED claims require supporting, active, unexpired direct evidence';
        END IF;
        IF EXISTS (SELECT 1 FROM claim_assumptions WHERE claim_id = c.id) THEN
            RETURN 'OBSERVED claims cannot rest on assumptions';
        END IF;
    ELSIF supports = 0 THEN
        RETURN 'INFERRED claims require reasoning and supporting, active, unexpired evidence';
    END IF;
    -- confidence_calibration: HIGH needs high-trust support and no high-trust contradiction.
    IF c.confidence_band = 'HIGH' THEN
        IF NOT EXISTS (
            SELECT 1 FROM claim_evidence ce JOIN evidence_records ev ON ev.id = ce.evidence_id
            WHERE ce.claim_id = c.id AND ce.relation = 'SUPPORTS' AND ev.status = 'ACTIVE'
              AND (ev.expires_at IS NULL OR ev.expires_at > NOW()) AND ev.trust_level IN ('VERIFIED', 'HIGH')) THEN
            RETURN 'HIGH confidence requires supporting evidence with VERIFIED or HIGH trust';
        END IF;
        IF EXISTS (
            SELECT 1 FROM claim_evidence ce JOIN evidence_records ev ON ev.id = ce.evidence_id
            WHERE ce.claim_id = c.id AND ce.relation = 'CONTRADICTS' AND ev.status = 'ACTIVE'
              AND (ev.expires_at IS NULL OR ev.expires_at > NOW()) AND ev.trust_level IN ('VERIFIED', 'HIGH')) THEN
            RETURN 'HIGH confidence is not allowed while high-trust evidence contradicts the claim';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION claims_requirements_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c claims; problem TEXT;
BEGIN
    SELECT * INTO c FROM claims WHERE id = NEW.id;
    IF FOUND THEN
        problem := claim_requirements_unmet(c);
        IF problem IS NOT NULL THEN
            RAISE EXCEPTION 'claim %: %', c.id, problem USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS trg_claims_requirements ON claims;
CREATE CONSTRAINT TRIGGER trg_claims_requirements AFTER INSERT OR UPDATE OF lifecycle ON claims
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION claims_requirements_check();

-- Links are append-only.
CREATE OR REPLACE FUNCTION ledger_links_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END;
$$;
DROP TRIGGER IF EXISTS trg_claim_evidence_append_only ON claim_evidence;
CREATE TRIGGER trg_claim_evidence_append_only BEFORE UPDATE ON claim_evidence
    FOR EACH ROW EXECUTE FUNCTION ledger_links_append_only();
DROP TRIGGER IF EXISTS trg_claim_assumptions_append_only ON claim_assumptions;
CREATE TRIGGER trg_claim_assumptions_append_only BEFORE UPDATE ON claim_assumptions
    FOR EACH ROW EXECUTE FUNCTION ledger_links_append_only();

-- ---------------------------------------------------------------------------
-- Privileges & RLS (no DELETE: the ledger is append-only)
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON evidence_records, assumptions, claims TO systera_app, systera_system;
GRANT SELECT, INSERT ON claim_evidence, claim_assumptions TO systera_app, systera_system;
-- The graph service validates evidence:<uuid> refs in patches.
GRANT SELECT ON evidence_records TO systera_graph;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['evidence_records', 'assumptions', 'claims', 'claim_evidence', 'claim_assumptions'] LOOP
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

UPDATE graph_invariants SET enforced_by = 'DATABASE', active = true,
    description = 'ACTIVE claims: OBSERVED need supporting active direct evidence and no assumptions; INFERRED need reasoning and supporting active evidence; PROPOSED are recommendations.'
WHERE key = 'claim_evidence_requirements';
INSERT INTO graph_invariants (key, description, severity, enforced_by, active, spec_ref) VALUES
    ('ledger_append_only', 'Evidence, assumption and claim content is immutable; only status/review/lifecycle fields change, and final states are final.', 'ERROR', 'DATABASE', true, '§23.11'),
    ('reviewer_independence', 'A claim cannot be reviewed by its author.', 'ERROR', 'DATABASE', true, '§23.11'),
    ('confidence_calibration', 'A confidence score lies inside its band; HIGH confidence needs high-trust support and no unresolved high-trust contradiction.', 'ERROR', 'DATABASE', true, '§23.11')
ON CONFLICT (key) DO NOTHING;
