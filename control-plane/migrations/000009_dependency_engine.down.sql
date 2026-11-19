DELETE FROM graph_invariants WHERE key IN ('artifact_binding', 'artifact_history_immutable', 'impact_invalidation');
REVOKE UPDATE ON claims FROM systera_graph;
REVOKE SELECT ON claim_evidence, claim_assumptions, assumptions FROM systera_graph;
DROP POLICY IF EXISTS system_read_projects ON projects;
REVOKE SELECT ON projects FROM systera_system;

ALTER TABLE IF EXISTS artifacts DROP CONSTRAINT IF EXISTS artifacts_current_version_fk;
DROP TABLE IF EXISTS revalidation_requests;
DROP TABLE IF EXISTS artifact_dependencies;
DROP TABLE IF EXISTS artifact_versions;
DROP TABLE IF EXISTS artifacts;

DROP FUNCTION IF EXISTS artifact_versions_binding();
DROP FUNCTION IF EXISTS artifact_rows_immutable();
DROP FUNCTION IF EXISTS artifacts_guard();

-- The seeded agent is left in place if tasks reference it.
DELETE FROM agents WHERE agent_type = 'artifact_revalidator'
    AND NOT EXISTS (SELECT 1 FROM agent_tasks WHERE agent_type = 'artifact_revalidator');
