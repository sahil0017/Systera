-- Stage 3 (Section 2): Engineering Pulse (§2.2, §6.2, §6.5, §20.2).
--
-- Pipeline: trend_sources (approved registry) -> trend_source_fetches (retrieval log)
--   -> trend_items (dedup to a canonical item) -> trend_item_topics (taxonomy)
--   -> trend_observations (daily per-topic signal) -> trend_scores (0.25P+0.25G+0.20E+0.20R+0.10N)
--   -> research_reports + research_report_evidence -> validation gate
--   -> knowledge_updates (PUBLIC knowledge_documents) -> agent_knowledge_notifications.
--
-- Pulse data is platform-wide public-source intelligence: it carries no tenant_id, only
-- systera_system (the research worker / admin CLI) writes it, and systera_app reads it.
-- run_knowledge_references is tenant-scoped: it records which pattern a design run used and why.

-- ---------------------------------------------------------------------------
-- 0. Knowledge versioning fix: versions are numbered per (tenant, canonical_key).
--    000012 made (canonical_key, version_number) globally unique while each tenant only
--    sees its own rows when computing the next version, so two tenants ingesting the same
--    key collided. PUBLIC (tenant_id NULL) knowledge forms its own version chain.
-- ---------------------------------------------------------------------------
ALTER TABLE knowledge_documents DROP CONSTRAINT IF EXISTS knowledge_documents_canonical_key_version_number_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_docs_tenant_key_version
    ON knowledge_documents (tenant_id, canonical_key, version_number) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS idx_knowledge_docs_key ON knowledge_documents (canonical_key text_pattern_ops);

-- ---------------------------------------------------------------------------
-- 1. Source registry (2.1)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS trend_sources (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_key            TEXT NOT NULL UNIQUE CHECK (source_key ~ '^[a-z0-9][a-z0-9._-]{1,79}$'),
    name                  TEXT NOT NULL CHECK (length(trim(name)) > 0),
    connector             TEXT NOT NULL CHECK (connector IN ('GITHUB_RELEASES', 'RSS', 'ARXIV')),
    feed_url              TEXT NOT NULL CHECK (feed_url ~ '^https?://'),
    homepage_url          TEXT NOT NULL DEFAULT '',
    -- Knowledge source_type the source's items count as (RELEASE_NOTES, ENGINEERING_BLOG, PAPER, PRIMARY_DOC).
    source_type           TEXT NOT NULL CHECK (source_type IN ('PRIMARY_DOC', 'RFC', 'RELEASE_NOTES', 'ENGINEERING_BLOG', 'PAPER', 'COMMUNITY')),
    is_primary_source     BOOLEAN NOT NULL DEFAULT false,
    reliability           NUMERIC(4, 3) NOT NULL CHECK (reliability >= 0 AND reliability <= 1),
    -- Terms of use: where they are, what they say, what we may keep.
    terms_url             TEXT NOT NULL DEFAULT '',
    terms_summary         TEXT NOT NULL DEFAULT '',
    licence               TEXT NOT NULL DEFAULT 'UNKNOWN',
    allowed_content       TEXT NOT NULL DEFAULT 'METADATA_ONLY' CHECK (allowed_content IN ('METADATA_ONLY', 'SUMMARY', 'FULL_TEXT')),
    max_summary_chars     INT NOT NULL DEFAULT 1000 CHECK (max_summary_chars BETWEEN 0 AND 20000),
    attribution_required  BOOLEAN NOT NULL DEFAULT true,
    -- Rate limits: hard cap per rolling hour plus a minimum interval between fetches.
    rate_limit_per_hour   INT NOT NULL DEFAULT 4 CHECK (rate_limit_per_hour BETWEEN 1 AND 3600),
    min_interval_seconds  INT NOT NULL DEFAULT 900 CHECK (min_interval_seconds >= 1),
    config                JSONB NOT NULL DEFAULT '{}'::jsonb,
    status                TEXT NOT NULL DEFAULT 'PENDING_REVIEW' CHECK (status IN ('PENDING_REVIEW', 'APPROVED', 'SUSPENDED', 'RETIRED')),
    approved_by           TEXT,
    approved_at           TIMESTAMPTZ,
    -- Fetch state.
    etag                  TEXT NOT NULL DEFAULT '',
    last_modified         TEXT NOT NULL DEFAULT '',
    last_fetched_at       TIMESTAMPTZ,
    next_fetch_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    consecutive_failures  INT NOT NULL DEFAULT 0,
    last_error            TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A source may only be fetched once a named person approved it against documented terms.
    CONSTRAINT trend_sources_approval_needs_terms CHECK (
        status <> 'APPROVED' OR (approved_by IS NOT NULL AND approved_at IS NOT NULL
                                 AND length(terms_url) > 0 AND length(terms_summary) > 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_trend_sources_due ON trend_sources (next_fetch_at) WHERE status = 'APPROVED';

-- Every fetch attempt, successful or not: the retrieval metadata and the rate-limit ledger.
CREATE TABLE IF NOT EXISTS trend_source_fetches (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id      UUID NOT NULL REFERENCES trend_sources(id) ON DELETE CASCADE,
    requested_url  TEXT NOT NULL,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at    TIMESTAMPTZ,
    http_status    INT,
    not_modified   BOOLEAN NOT NULL DEFAULT false,
    content_type   TEXT NOT NULL DEFAULT '',
    bytes          BIGINT NOT NULL DEFAULT 0,
    items_seen     INT NOT NULL DEFAULT 0,
    items_new      INT NOT NULL DEFAULT 0,
    error          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_trend_fetches_source_time ON trend_source_fetches (source_id, started_at DESC);

-- ---------------------------------------------------------------------------
-- 2. Items, deduplication (2.2, 2.3)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS trend_items (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id          UUID NOT NULL REFERENCES trend_sources(id),
    fetch_id           UUID NOT NULL REFERENCES trend_source_fetches(id),
    external_id        TEXT NOT NULL,
    url                TEXT NOT NULL,
    canonical_url      TEXT NOT NULL,
    title              TEXT NOT NULL CHECK (length(trim(title)) > 0),
    norm_title         TEXT NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    content            TEXT NOT NULL DEFAULT '',
    authors            TEXT[] NOT NULL DEFAULT '{}',
    published_at       TIMESTAMPTZ,
    retrieved_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retrieval_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- series_key groups versions of one document (arXiv id without vN, an edited post).
    series_key         TEXT NOT NULL DEFAULT '',
    version_label      TEXT NOT NULL DEFAULT '',
    engagement         INT NOT NULL DEFAULT 0 CHECK (engagement >= 0),
    content_hash       TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    simhash            BIGINT NOT NULL,
    sim_tokens         INT NOT NULL DEFAULT 0,
    canonical_item_id  UUID REFERENCES trend_items(id),
    duplicate_kind     TEXT CHECK (duplicate_kind IN ('EXACT', 'MIRROR', 'REPOST', 'VERSION', 'NEAR_DUPLICATE')),
    duplicate_score    NUMERIC(4, 3),
    maturity           TEXT NOT NULL DEFAULT 'EMERGING' CHECK (maturity IN ('EXPERIMENTAL', 'EMERGING', 'MAINSTREAM', 'LEGACY', 'DEPRECATED')),
    significance       NUMERIC(4, 3) NOT NULL DEFAULT 0 CHECK (significance >= 0 AND significance <= 1),
    classified_at      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source_id, external_id),
    CONSTRAINT trend_items_duplicate_shape CHECK (
        (canonical_item_id IS NULL AND duplicate_kind IS NULL)
        OR (canonical_item_id IS NOT NULL AND duplicate_kind IS NOT NULL AND canonical_item_id <> id)
    )
);
CREATE INDEX IF NOT EXISTS idx_trend_items_canonical ON trend_items (canonical_item_id);
CREATE INDEX IF NOT EXISTS idx_trend_items_curl ON trend_items (canonical_url);
CREATE INDEX IF NOT EXISTS idx_trend_items_hash ON trend_items (content_hash);
CREATE INDEX IF NOT EXISTS idx_trend_items_title ON trend_items (norm_title);
CREATE INDEX IF NOT EXISTS idx_trend_items_series ON trend_items (series_key) WHERE series_key <> '';
CREATE INDEX IF NOT EXISTS idx_trend_items_published ON trend_items (published_at DESC);
CREATE INDEX IF NOT EXISTS idx_trend_items_unclassified ON trend_items (created_at) WHERE classified_at IS NULL;

-- ---------------------------------------------------------------------------
-- 3. Topic taxonomy and classification (2.4)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS trend_topics (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic_key   TEXT NOT NULL UNIQUE CHECK (topic_key ~ '^[a-z][a-z0-9-]{1,62}$'),
    name        TEXT NOT NULL,
    facet       TEXT NOT NULL CHECK (facet IN ('DOMAIN', 'TECHNOLOGY', 'PATTERN')),
    parent_id   UUID REFERENCES trend_topics(id),
    keywords    TEXT[] NOT NULL CHECK (cardinality(keywords) > 0),
    description TEXT NOT NULL DEFAULT '',
    maturity    TEXT NOT NULL DEFAULT 'EMERGING' CHECK (maturity IN ('EXPERIMENTAL', 'EMERGING', 'MAINSTREAM', 'LEGACY', 'DEPRECATED')),
    first_seen_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS trend_item_topics (
    item_id       UUID NOT NULL REFERENCES trend_items(id) ON DELETE CASCADE,
    topic_id      UUID NOT NULL REFERENCES trend_topics(id) ON DELETE CASCADE,
    confidence    NUMERIC(4, 3) NOT NULL CHECK (confidence > 0 AND confidence <= 1),
    matched_terms TEXT[] NOT NULL DEFAULT '{}',
    method        TEXT NOT NULL DEFAULT 'RULES' CHECK (method IN ('RULES', 'MODEL', 'MANUAL')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (item_id, topic_id)
);
CREATE INDEX IF NOT EXISTS idx_trend_item_topics_topic ON trend_item_topics (topic_id);

-- ---------------------------------------------------------------------------
-- 4. Observations and scores (2.5)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS trend_observations (
    topic_id      UUID NOT NULL REFERENCES trend_topics(id) ON DELETE CASCADE,
    day           DATE NOT NULL,
    item_count    INT NOT NULL DEFAULT 0,  -- canonical items
    mention_count INT NOT NULL DEFAULT 0,  -- canonical items + their mirrors/reposts
    source_count  INT NOT NULL DEFAULT 0,
    primary_count INT NOT NULL DEFAULT 0,
    engagement    BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (topic_id, day)
);

CREATE TABLE IF NOT EXISTS trend_scores (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic_id       UUID NOT NULL REFERENCES trend_topics(id) ON DELETE CASCADE,
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    window_days    INT NOT NULL CHECK (window_days > 0),
    popularity     NUMERIC(5, 2) NOT NULL CHECK (popularity BETWEEN 0 AND 100),
    growth         NUMERIC(5, 2) NOT NULL CHECK (growth BETWEEN 0 AND 100),
    significance   NUMERIC(5, 2) NOT NULL CHECK (significance BETWEEN 0 AND 100),
    reliability    NUMERIC(5, 2) NOT NULL CHECK (reliability BETWEEN 0 AND 100),
    novelty        NUMERIC(5, 2) NOT NULL CHECK (novelty BETWEEN 0 AND 100),
    -- The formula is fixed in the schema so no writer can weight it differently (§2.2).
    score          NUMERIC(5, 2) GENERATED ALWAYS AS (
                       round(0.25 * popularity + 0.25 * growth + 0.20 * significance + 0.20 * reliability + 0.10 * novelty, 2)
                   ) STORED,
    inputs         JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_trend_scores_topic_time ON trend_scores (topic_id, computed_at DESC);

-- ---------------------------------------------------------------------------
-- 5. Research reports and validation (2.6, 2.7)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS research_reports (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic_id         UUID NOT NULL REFERENCES trend_topics(id),
    trend_score_id   UUID REFERENCES trend_scores(id),
    title            TEXT NOT NULL,
    summary          TEXT NOT NULL,
    findings         JSONB NOT NULL DEFAULT '[]'::jsonb,   -- [{claim, evidence:[item_id], kind: TECHNICAL|ADOPTION}]
    limitations      TEXT[] NOT NULL DEFAULT '{}',
    caveats          TEXT[] NOT NULL DEFAULT '{}',
    confidence       NUMERIC(4, 3) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    confidence_band  TEXT NOT NULL CHECK (confidence_band IN ('HIGH', 'MEDIUM', 'LOW')),
    maturity         TEXT NOT NULL CHECK (maturity IN ('EXPERIMENTAL', 'EMERGING', 'MAINSTREAM', 'LEGACY', 'DEPRECATED')),
    freshness_at     TIMESTAMPTZ,          -- newest evidence publication time
    stale_after      TIMESTAMPTZ NOT NULL,
    evidence_hash    TEXT NOT NULL CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    status           TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'VALIDATED', 'REJECTED', 'PUBLISHED', 'SUPERSEDED', 'STALE')),
    validation       JSONB NOT NULL DEFAULT '{}'::jsonb,
    validated_at     TIMESTAMPTZ,
    author           TEXT NOT NULL DEFAULT 'engineering_pulse',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_research_reports_topic ON research_reports (topic_id, created_at DESC);

CREATE TABLE IF NOT EXISTS research_report_evidence (
    report_id   UUID NOT NULL REFERENCES research_reports(id) ON DELETE CASCADE,
    item_id     UUID NOT NULL REFERENCES trend_items(id),
    role        TEXT NOT NULL CHECK (role IN ('PRIMARY', 'CORROBORATING')),
    PRIMARY KEY (report_id, item_id)
);

-- ---------------------------------------------------------------------------
-- 6. Knowledge distribution (2.7, 2.8)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS knowledge_updates (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    report_id     UUID NOT NULL UNIQUE REFERENCES research_reports(id),
    topic_id      UUID NOT NULL REFERENCES trend_topics(id),
    document_id   UUID NOT NULL REFERENCES knowledge_documents(id),
    status        TEXT NOT NULL DEFAULT 'PUBLISHED' CHECK (status IN ('PUBLISHED', 'SUPERSEDED', 'STALE')),
    published_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_knowledge_updates_topic ON knowledge_updates (topic_id, published_at DESC);
CREATE INDEX IF NOT EXISTS idx_knowledge_updates_document ON knowledge_updates (document_id);

-- The validation gate is enforced by the database, not only by the worker: nothing becomes
-- knowledge unless its report passed validation.
CREATE OR REPLACE FUNCTION knowledge_update_requires_validated_report() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st TEXT; passed BOOLEAN;
BEGIN
    SELECT status, COALESCE((validation->>'passed')::boolean, false) INTO st, passed
    FROM research_reports WHERE id = NEW.report_id;
    IF st IS NULL OR st NOT IN ('VALIDATED', 'PUBLISHED') OR NOT passed THEN
        RAISE EXCEPTION 'knowledge_update for report % rejected: report has not passed the validation gate (status %)', NEW.report_id, st
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS trg_knowledge_update_gate ON knowledge_updates;
CREATE TRIGGER trg_knowledge_update_gate BEFORE INSERT ON knowledge_updates
    FOR EACH ROW EXECUTE FUNCTION knowledge_update_requires_validated_report();

CREATE TABLE IF NOT EXISTS agent_knowledge_subscriptions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_type     TEXT NOT NULL REFERENCES agents(agent_type),
    topic_id       UUID NOT NULL REFERENCES trend_topics(id),   -- matches the topic and all descendants
    min_confidence NUMERIC(4, 3) NOT NULL DEFAULT 0.5 CHECK (min_confidence >= 0 AND min_confidence <= 1),
    active         BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (agent_type, topic_id)
);

-- Durable per-agent-domain inbox: one row per (subscription, update).
CREATE TABLE IF NOT EXISTS agent_knowledge_notifications (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id     UUID NOT NULL REFERENCES agent_knowledge_subscriptions(id) ON DELETE CASCADE,
    agent_type          TEXT NOT NULL,
    knowledge_update_id UUID NOT NULL REFERENCES knowledge_updates(id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (subscription_id, knowledge_update_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_knowledge_notifications_agent ON agent_knowledge_notifications (agent_type, created_at DESC);

-- ---------------------------------------------------------------------------
-- 7. Design-run retrieval record (2.10): which pattern a run used, why, and from which sources.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS run_knowledge_references (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    run_id              UUID NOT NULL REFERENCES design_runs(id) ON DELETE CASCADE,
    task_id             UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
    agent_type          TEXT NOT NULL DEFAULT '',
    knowledge_update_id UUID NOT NULL REFERENCES knowledge_updates(id),
    document_id         UUID NOT NULL REFERENCES knowledge_documents(id),
    topic_key           TEXT NOT NULL,
    relevance_score     NUMERIC(6, 4) NOT NULL,
    explanation         JSONB NOT NULL,   -- {why:[...], matched_terms, supporting_sources:[...], limitations, adoption_signal}
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_run_knowledge_refs ON run_knowledge_references (run_id, COALESCE(task_id, '00000000-0000-0000-0000-000000000000'::uuid), knowledge_update_id);
CREATE INDEX IF NOT EXISTS idx_run_knowledge_refs_run ON run_knowledge_references (tenant_id, run_id);

-- ---------------------------------------------------------------------------
-- 8. Privileges & RLS
-- ---------------------------------------------------------------------------
GRANT SELECT ON trend_sources, trend_source_fetches, trend_items, trend_topics, trend_item_topics,
    trend_observations, trend_scores, research_reports, research_report_evidence, knowledge_updates,
    agent_knowledge_subscriptions, agent_knowledge_notifications
TO systera_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON trend_sources, trend_source_fetches, trend_items, trend_topics, trend_item_topics,
    trend_observations, trend_scores, research_reports, research_report_evidence, knowledge_updates,
    agent_knowledge_subscriptions, agent_knowledge_notifications
TO systera_system;
GRANT SELECT, INSERT ON run_knowledge_references TO systera_app, systera_system;
GRANT SELECT ON agents TO systera_system;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['trend_sources', 'trend_source_fetches', 'trend_items', 'trend_topics', 'trend_item_topics',
        'trend_observations', 'trend_scores', 'research_reports', 'research_report_evidence', 'knowledge_updates',
        'agent_knowledge_subscriptions', 'agent_knowledge_notifications'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS app_read_%s ON %I', t, t);
        EXECUTE format('CREATE POLICY app_read_%s ON %I FOR SELECT TO systera_app USING (true)', t, t);
        EXECUTE format('DROP POLICY IF EXISTS system_all_%s ON %I', t, t);
        EXECUTE format('CREATE POLICY system_all_%s ON %I TO systera_system USING (true) WITH CHECK (true)', t, t);
    END LOOP;
END $$;

ALTER TABLE run_knowledge_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_knowledge_references FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_run_knowledge_references ON run_knowledge_references;
CREATE POLICY tenant_isolation_run_knowledge_references ON run_knowledge_references TO systera_app
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);
DROP POLICY IF EXISTS system_all_run_knowledge_references ON run_knowledge_references;
CREATE POLICY system_all_run_knowledge_references ON run_knowledge_references TO systera_system
    USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------------------
-- 9. Seed taxonomy (DOMAIN > TECHNOLOGY / PATTERN). Keywords are matched on word boundaries.
-- ---------------------------------------------------------------------------
INSERT INTO trend_topics (topic_key, name, facet, keywords, description, maturity) VALUES
    ('distributed-systems', 'Distributed systems', 'DOMAIN', ARRAY['distributed system','consensus','replication','partition tolerance','microservice','microservices'], 'Coordination, replication and failure handling across machines.', 'MAINSTREAM'),
    ('data-storage',        'Data storage',        'DOMAIN', ARRAY['database','storage engine','query planner','indexing','transaction','sql'], 'Databases, storage engines and query processing.', 'MAINSTREAM'),
    ('messaging-streaming', 'Messaging & streaming','DOMAIN', ARRAY['message queue','event streaming','stream processing','pub/sub','pubsub','message broker'], 'Queues, logs and stream processing.', 'MAINSTREAM'),
    ('platform-infra',      'Platform & infrastructure','DOMAIN', ARRAY['container','orchestration','infrastructure as code','serverless','service mesh','platform engineering'], 'Runtime platforms and infrastructure automation.', 'MAINSTREAM'),
    ('observability',       'Observability',       'DOMAIN', ARRAY['observability','tracing','distributed tracing','metrics','telemetry','logging'], 'Telemetry, tracing and monitoring.', 'MAINSTREAM'),
    ('security',            'Security',            'DOMAIN', ARRAY['security','vulnerability','cve','authentication','authorization','zero trust','supply chain'], 'Application, platform and supply-chain security.', 'MAINSTREAM'),
    ('ai-systems',          'AI systems',          'DOMAIN', ARRAY['llm','large language model','embedding','embeddings','inference','retrieval-augmented','vector database'], 'Engineering of ML/LLM-backed systems.', 'EMERGING')
ON CONFLICT (topic_key) DO NOTHING;

INSERT INTO trend_topics (topic_key, name, facet, parent_id, keywords, description, maturity)
SELECT v.topic_key, v.name, v.facet, p.id, v.keywords, v.description, v.maturity
FROM (VALUES
    ('postgresql',   'PostgreSQL',   'TECHNOLOGY', 'data-storage',        ARRAY['postgresql','postgres'], 'PostgreSQL relational database.', 'MAINSTREAM'),
    ('pgvector',     'pgvector',     'TECHNOLOGY', 'ai-systems',          ARRAY['pgvector'], 'Vector similarity search inside PostgreSQL.', 'EMERGING'),
    ('redis',        'Redis / Valkey','TECHNOLOGY','data-storage',        ARRAY['redis','valkey'], 'In-memory data store.', 'MAINSTREAM'),
    ('kafka',        'Apache Kafka', 'TECHNOLOGY', 'messaging-streaming', ARRAY['kafka'], 'Distributed log and streaming platform.', 'MAINSTREAM'),
    ('nats',         'NATS',         'TECHNOLOGY', 'messaging-streaming', ARRAY['nats','jetstream'], 'NATS messaging and JetStream persistence.', 'MAINSTREAM'),
    ('kubernetes',   'Kubernetes',   'TECHNOLOGY', 'platform-infra',      ARRAY['kubernetes','k8s'], 'Container orchestration.', 'MAINSTREAM'),
    ('opentelemetry','OpenTelemetry','TECHNOLOGY', 'observability',       ARRAY['opentelemetry','otel','otlp'], 'Vendor-neutral telemetry standard.', 'MAINSTREAM'),
    ('grpc',         'gRPC',         'TECHNOLOGY', 'distributed-systems', ARRAY['grpc','protocol buffers','protobuf'], 'RPC framework over HTTP/2.', 'MAINSTREAM'),
    ('webassembly',  'WebAssembly',  'TECHNOLOGY', 'platform-infra',      ARRAY['webassembly','wasm','wasi'], 'Portable sandboxed bytecode.', 'EMERGING'),
    ('event-sourcing','Event sourcing','PATTERN',  'distributed-systems', ARRAY['event sourcing','event-sourced','event store'], 'State as an append-only sequence of events.', 'MAINSTREAM'),
    ('cqrs',         'CQRS',         'PATTERN',    'distributed-systems', ARRAY['cqrs','command query responsibility segregation'], 'Separate write and read models.', 'MAINSTREAM'),
    ('transactional-outbox','Transactional outbox','PATTERN','messaging-streaming', ARRAY['transactional outbox','outbox pattern','outbox'], 'Atomically record state and the events to publish.', 'MAINSTREAM'),
    ('saga',         'Saga',         'PATTERN',    'distributed-systems', ARRAY['saga pattern','sagas','compensating transaction'], 'Long-running transactions via compensations.', 'MAINSTREAM'),
    ('circuit-breaker','Circuit breaker','PATTERN','distributed-systems', ARRAY['circuit breaker','circuit-breaker'], 'Stop calling a failing dependency.', 'MAINSTREAM'),
    ('rate-limiting','Rate limiting','PATTERN',    'distributed-systems', ARRAY['rate limiting','rate limiter','token bucket','leaky bucket'], 'Bound request rates per client or resource.', 'MAINSTREAM'),
    ('caching',      'Caching',      'PATTERN',    'data-storage',        ARRAY['cache','caching','cache invalidation','cache-aside','write-through'], 'Serve hot reads from a faster tier.', 'MAINSTREAM'),
    ('sharding',     'Sharding & partitioning','PATTERN','data-storage',  ARRAY['sharding','shard','partitioning','consistent hashing'], 'Split data across nodes by key.', 'MAINSTREAM'),
    ('idempotency',  'Idempotency',  'PATTERN',    'distributed-systems', ARRAY['idempotency','idempotent','exactly-once','deduplication'], 'Make retries safe.', 'MAINSTREAM'),
    ('consensus-raft','Consensus (Raft/Paxos)','PATTERN','distributed-systems', ARRAY['raft','paxos','leader election'], 'Agreement on a replicated log.', 'MAINSTREAM'),
    ('crdt',         'CRDTs',        'PATTERN',    'distributed-systems', ARRAY['crdt','crdts','conflict-free replicated'], 'Merge concurrent updates without coordination.', 'EMERGING'),
    ('rag',          'Retrieval-augmented generation','PATTERN','ai-systems', ARRAY['retrieval-augmented generation','rag','hybrid search','reranking'], 'Ground model output in retrieved documents.', 'EMERGING'),
    ('zero-trust',   'Zero trust',   'PATTERN',    'security',            ARRAY['zero trust','zero-trust','mtls','mutual tls'], 'Authenticate and authorize every request.', 'MAINSTREAM')
) AS v(topic_key, name, facet, parent_key, keywords, description, maturity)
JOIN trend_topics p ON p.topic_key = v.parent_key
ON CONFLICT (topic_key) DO NOTHING;

-- Agent domain subscriptions (2.8).
INSERT INTO agent_knowledge_subscriptions (agent_type, topic_id, min_confidence)
SELECT s.agent_type, t.id, s.min_conf
FROM (VALUES
    ('hld_architect',        'distributed-systems', 0.5),
    ('hld_architect',        'messaging-streaming', 0.5),
    ('hld_architect',        'platform-infra',      0.5),
    ('hld_architect',        'observability',       0.5),
    ('data_modeller',        'data-storage',        0.5),
    ('data_modeller',        'ai-systems',          0.5),
    ('api_architect',        'grpc',                0.5),
    ('api_architect',        'idempotency',         0.5),
    ('api_architect',        'rate-limiting',       0.5),
    ('security_architect',   'security',            0.4),
    ('independent_reviewer', 'distributed-systems', 0.7),
    ('independent_reviewer', 'security',            0.7)
) AS s(agent_type, topic_key, min_conf)
JOIN trend_topics t ON t.topic_key = s.topic_key
JOIN agents a ON a.agent_type = s.agent_type
ON CONFLICT (agent_type, topic_id) DO NOTHING;

-- Let design agents query Pulse knowledge explicitly (the runner also injects it, §20.2).
UPDATE agents SET allowed_tools = array_append(allowed_tools, 'pulse.patterns'), updated_at = NOW()
WHERE agent_type IN ('hld_architect', 'data_modeller', 'api_architect', 'security_architect', 'independent_reviewer')
  AND NOT ('pulse.patterns' = ANY(allowed_tools));
UPDATE agents SET allowed_tools = array_append(allowed_tools, 'pulse.compare_options'), updated_at = NOW()
WHERE agent_type IN ('hld_architect', 'data_modeller', 'api_architect', 'security_architect', 'independent_reviewer')
  AND NOT ('pulse.compare_options' = ANY(allowed_tools));
