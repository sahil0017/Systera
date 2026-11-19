-- Stage 3 (Section 1): Knowledge Base, pgvector Embeddings, Provenance & Memory (§6.1-§6.5).
--   * knowledge_documents, knowledge_chunks (vector(384) + tsvector), knowledge_provenance.
--   * memory_records (WORKING, EPISODIC, SEMANTIC, PROJECT) and memory_links.
--   * Tenant isolation via FORCE ROW LEVEL SECURITY (PUBLIC knowledge readable across tenants).

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- 1. Knowledge Documents, Chunks & Provenance (§6.1, §6.2, §6.5)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS knowledge_documents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID REFERENCES tenants(id) ON DELETE CASCADE,
    canonical_key     TEXT NOT NULL,
    version_number    INT NOT NULL DEFAULT 1 CHECK (version_number >= 1),
    title             TEXT NOT NULL CHECK (length(trim(title)) > 0),
    source_url        TEXT NOT NULL DEFAULT '',
    source_type       TEXT NOT NULL CHECK (source_type IN (
                          'PRIMARY_DOC', 'RFC', 'RELEASE_NOTES', 'ENGINEERING_BLOG',
                          'PAPER', 'COMMUNITY', 'INTERNAL'
                      )),
    is_primary_source BOOLEAN NOT NULL DEFAULT false,
    licence           TEXT NOT NULL DEFAULT 'UNKNOWN',
    access_policy     TEXT NOT NULL DEFAULT 'TENANT' CHECK (access_policy IN ('PUBLIC', 'TENANT', 'RESTRICTED')),
    published_at      TIMESTAMPTZ,
    maturity          TEXT NOT NULL DEFAULT 'MAINSTREAM' CHECK (maturity IN (
                          'EXPERIMENTAL', 'EMERGING', 'MAINSTREAM', 'LEGACY', 'DEPRECATED'
                      )),
    confidence_band   TEXT NOT NULL DEFAULT 'MEDIUM' CHECK (confidence_band IN ('HIGH', 'MEDIUM', 'LOW')),
    status            TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'STALE', 'SUPERSEDED', 'CONFLICTED')),
    superseded_by     UUID REFERENCES knowledge_documents(id),
    content_hash      TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    metadata_json     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (canonical_key, version_number)
);

CREATE INDEX IF NOT EXISTS idx_knowledge_docs_tenant_status ON knowledge_documents (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_knowledge_docs_hash ON knowledge_documents (content_hash);
CREATE INDEX IF NOT EXISTS idx_knowledge_docs_published ON knowledge_documents (published_at DESC);

CREATE TABLE IF NOT EXISTS knowledge_chunks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID REFERENCES tenants(id) ON DELETE CASCADE,
    document_id   UUID NOT NULL REFERENCES knowledge_documents(id) ON DELETE CASCADE,
    chunk_index   INT NOT NULL CHECK (chunk_index >= 0),
    content       TEXT NOT NULL CHECK (length(content) > 0),
    code_snippets JSONB NOT NULL DEFAULT '[]'::jsonb,
    token_count   INT NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    content_hash  TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    embedding     vector(384),
    search_tsv    tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (document_id, chunk_index)
);

CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_doc ON knowledge_chunks (document_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_tsv ON knowledge_chunks USING GIN (search_tsv);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_embedding ON knowledge_chunks USING hnsw (embedding vector_cosine_ops);

CREATE TABLE IF NOT EXISTS knowledge_provenance (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID REFERENCES tenants(id) ON DELETE CASCADE,
    document_id           UUID NOT NULL REFERENCES knowledge_documents(id) ON DELETE CASCADE,
    source_uri            TEXT NOT NULL,
    retrieved_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    collector             TEXT NOT NULL DEFAULT 'manual',
    corroborating_doc_ids UUID[] NOT NULL DEFAULT '{}',
    conflicts_with_doc_ids UUID[] NOT NULL DEFAULT '{}',
    conflict_note         TEXT NOT NULL DEFAULT '',
    trust_score           NUMERIC(4, 3) NOT NULL DEFAULT 0.500 CHECK (trust_score >= 0 AND trust_score <= 1),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_knowledge_provenance_doc ON knowledge_provenance (document_id);

-- ---------------------------------------------------------------------------
-- 2. Memory Records & Memory Links (§6.4)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS memory_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE CASCADE,
    run_id          UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    memory_type     TEXT NOT NULL CHECK (memory_type IN ('WORKING', 'EPISODIC', 'SEMANTIC', 'PROJECT')),
    summary         TEXT NOT NULL CHECK (length(trim(summary)) > 0 AND length(summary) <= 4000),
    content_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_refs     TEXT[] NOT NULL DEFAULT '{}',
    embedding       vector(384),
    relevance_score NUMERIC(4, 3) NOT NULL DEFAULT 0.500 CHECK (relevance_score >= 0 AND relevance_score <= 1),
    access_policy   TEXT NOT NULL DEFAULT 'PROJECT' CHECK (access_policy IN ('TENANT', 'PROJECT', 'RESTRICTED')),
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_memory_records_tenant_project ON memory_records (tenant_id, project_id, memory_type);
CREATE INDEX IF NOT EXISTS idx_memory_records_embedding ON memory_records USING hnsw (embedding vector_cosine_ops);

CREATE TABLE IF NOT EXISTS memory_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    memory_id   UUID NOT NULL REFERENCES memory_records(id) ON DELETE CASCADE,
    target_type TEXT NOT NULL CHECK (target_type IN ('NODE', 'DECISION', 'CLAIM', 'ARTIFACT', 'KNOWLEDGE_DOC', 'MEMORY')),
    target_id   UUID NOT NULL,
    relation    TEXT NOT NULL CHECK (relation IN ('RELATES_TO', 'DERIVED_FROM', 'CONTRADICTS', 'UPDATES')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_memory_links_memory ON memory_links (memory_id);
CREATE INDEX IF NOT EXISTS idx_memory_links_target ON memory_links (target_type, target_id);

-- ---------------------------------------------------------------------------
-- 3. Privileges & Row-Level Security
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON
    knowledge_documents,
    knowledge_chunks,
    knowledge_provenance,
    memory_records,
    memory_links
TO systera_app, systera_system;

ALTER TABLE knowledge_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_knowledge_documents ON knowledge_documents;
CREATE POLICY tenant_isolation_knowledge_documents ON knowledge_documents TO systera_app
    USING (
        access_policy = 'PUBLIC'
        OR tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    );
DROP POLICY IF EXISTS system_all_knowledge_documents ON knowledge_documents;
CREATE POLICY system_all_knowledge_documents ON knowledge_documents TO systera_system
    USING (true) WITH CHECK (true);

ALTER TABLE knowledge_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_knowledge_chunks ON knowledge_chunks;
CREATE POLICY tenant_isolation_knowledge_chunks ON knowledge_chunks TO systera_app
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    );
DROP POLICY IF EXISTS system_all_knowledge_chunks ON knowledge_chunks;
CREATE POLICY system_all_knowledge_chunks ON knowledge_chunks TO systera_system
    USING (true) WITH CHECK (true);

ALTER TABLE knowledge_provenance ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_provenance FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_knowledge_provenance ON knowledge_provenance;
CREATE POLICY tenant_isolation_knowledge_provenance ON knowledge_provenance TO systera_app
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid
    );
DROP POLICY IF EXISTS system_all_knowledge_provenance ON knowledge_provenance;
CREATE POLICY system_all_knowledge_provenance ON knowledge_provenance TO systera_system
    USING (true) WITH CHECK (true);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['memory_records', 'memory_links'] LOOP
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
