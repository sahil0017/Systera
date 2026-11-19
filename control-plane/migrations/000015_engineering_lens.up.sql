-- Stage 3 (Section 3): Engineering Lens hardening (§2.3, §7.2-§7.3, §20.3).
--   * Consent is a recorded fact: which consent text the user accepted and when (3.4).
--   * A page is captured at most once per session (3.9 bounded multi-page mode).
--   * Profiles record the observations they were derived from, so deleting an observation
--     deletes what was derived from it (3.8); one profile per session.
--   * PROMOTING claims a promotion so "Continue in workspace" is idempotent (3.10).

ALTER TABLE browser_sessions
    ADD COLUMN IF NOT EXISTS consent_version TEXT NOT NULL DEFAULT 'lens-consent-v1',
    ADD COLUMN IF NOT EXISTS consented_at    TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE browser_sessions DROP CONSTRAINT IF EXISTS browser_sessions_consent_required;
ALTER TABLE browser_sessions ADD CONSTRAINT browser_sessions_consent_required CHECK (consent_confirmed);
ALTER TABLE browser_sessions DROP CONSTRAINT IF EXISTS browser_sessions_allowed_pages_bounded;
ALTER TABLE browser_sessions ADD CONSTRAINT browser_sessions_allowed_pages_bounded CHECK (cardinality(allowed_pages) <= max_pages);
CREATE INDEX IF NOT EXISTS idx_browser_sessions_retention ON browser_sessions (retention_expires_at);
CREATE INDEX IF NOT EXISTS idx_browser_sessions_user ON browser_sessions (tenant_id, user_id, created_at DESC);

ALTER TABLE browser_observations DROP CONSTRAINT IF EXISTS browser_observations_session_url_key;
ALTER TABLE browser_observations ADD CONSTRAINT browser_observations_session_url_key UNIQUE (session_id, url);
ALTER TABLE browser_observations DROP CONSTRAINT IF EXISTS browser_observations_payload_bounded;
ALTER TABLE browser_observations ADD CONSTRAINT browser_observations_payload_bounded CHECK (payload_bytes <= 262144);

DELETE FROM website_profiles p USING website_profiles q
    WHERE p.session_id = q.session_id AND p.created_at < q.created_at;
ALTER TABLE website_profiles
    ADD COLUMN IF NOT EXISTS source_observation_ids UUID[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS promoted_at TIMESTAMPTZ;
ALTER TABLE website_profiles DROP CONSTRAINT IF EXISTS website_profiles_session_key;
ALTER TABLE website_profiles ADD CONSTRAINT website_profiles_session_key UNIQUE (session_id);
ALTER TABLE website_profiles DROP CONSTRAINT IF EXISTS website_profiles_status_check;
ALTER TABLE website_profiles ADD CONSTRAINT website_profiles_status_check CHECK (status IN ('DRAFT', 'ANALYZED', 'PROMOTING', 'PROMOTED'));
CREATE INDEX IF NOT EXISTS idx_website_profiles_sources ON website_profiles USING GIN (source_observation_ids);

GRANT INSERT ON audit_events TO systera_app, systera_system;
