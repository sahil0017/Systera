-- Stage 3 (Section 3): Engineering Lens — Browser Intelligence (§2.3, §7, §20.3).
--   * browser_sessions, browser_observations, website_profiles.
--   * ON DELETE CASCADE from browser_sessions ensures user-initiated deletion cascades
--     through observations and derived findings (Checklist 3.8).
--   * Tenant isolation via FORCE ROW LEVEL SECURITY for systera_app and systera_system.

CREATE TABLE IF NOT EXISTS browser_sessions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id              UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    domain               TEXT NOT NULL CHECK (length(trim(domain)) > 0),
    analysis_mode        TEXT NOT NULL DEFAULT 'quick_scan' CHECK (analysis_mode IN (
                             'quick_scan', 'product_map', 'full_system_design', 'compare_websites'
                         )),
    allowed_pages        TEXT[] NOT NULL DEFAULT '{}',
    max_pages            INT NOT NULL DEFAULT 10 CHECK (max_pages >= 1 AND max_pages <= 25),
    consent_confirmed    BOOLEAN NOT NULL DEFAULT true,
    retention_expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    status               TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ANALYZED', 'PROMOTED', 'DELETED')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_browser_sessions_tenant ON browser_sessions (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS browser_observations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id          UUID NOT NULL REFERENCES browser_sessions(id) ON DELETE CASCADE,
    url                 TEXT NOT NULL CHECK (length(trim(url)) > 0),
    title               TEXT NOT NULL DEFAULT '',
    collector_version   TEXT NOT NULL DEFAULT '1.0',
    schema_version      TEXT NOT NULL DEFAULT '1.0',
    observations_json   JSONB NOT NULL,
    redactions_applied  TEXT[] NOT NULL DEFAULT '{}',
    injection_suspected BOOLEAN NOT NULL DEFAULT false,
    payload_bytes       INT NOT NULL DEFAULT 0 CHECK (payload_bytes >= 0),
    captured_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_browser_observations_session ON browser_observations (session_id, created_at ASC);

CREATE TABLE IF NOT EXISTS website_profiles (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id        UUID NOT NULL REFERENCES browser_sessions(id) ON DELETE CASCADE,
    domain            TEXT NOT NULL,
    project_id        UUID REFERENCES projects(id) ON DELETE SET NULL,
    model_id          UUID REFERENCES architecture_models(id) ON DELETE SET NULL,
    run_id            UUID REFERENCES design_runs(id) ON DELETE SET NULL,
    capabilities_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    journeys_json     JSONB NOT NULL DEFAULT '[]'::jsonb,
    tech_signals_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    fingerprint_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    alternatives_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    status            TEXT NOT NULL DEFAULT 'ANALYZED' CHECK (status IN ('DRAFT', 'ANALYZED', 'PROMOTED')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_website_profiles_tenant ON website_profiles (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_website_profiles_session ON website_profiles (session_id);

-- ---------------------------------------------------------------------------
-- Privileges & Row-Level Security (DELETE granted so user deletion cascades)
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE, DELETE ON
    browser_sessions,
    browser_observations,
    website_profiles
TO systera_app, systera_system;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['browser_sessions', 'browser_observations', 'website_profiles'] LOOP
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
