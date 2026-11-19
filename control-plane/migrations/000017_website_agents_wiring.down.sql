UPDATE agents SET allowed_tools = array_remove(array_remove(array_remove(allowed_tools, 'website.observations'), 'website.record_finding'), 'website.review_findings');
DROP TABLE IF EXISTS observation_ledger_evidence;
DROP INDEX IF EXISTS idx_architecture_evidence_observation;
DROP INDEX IF EXISTS idx_architecture_evidence_finding;
ALTER TABLE architecture_evidence DROP COLUMN IF EXISTS relation, DROP COLUMN IF EXISTS finding_id, DROP COLUMN IF EXISTS finding_type;
ALTER TABLE architecture_hypotheses DROP CONSTRAINT IF EXISTS architecture_hypotheses_inferred_cites;
ALTER TABLE architecture_hypotheses DROP COLUMN IF EXISTS claim_id, DROP COLUMN IF EXISTS observation_ids, DROP COLUMN IF EXISTS task_id,
    DROP COLUMN IF EXISTS run_id, DROP COLUMN IF EXISTS agent_type, DROP COLUMN IF EXISTS source, DROP COLUMN IF EXISTS rationale, DROP COLUMN IF EXISTS kind;
DROP INDEX IF EXISTS idx_website_capabilities_obs;
ALTER TABLE website_capabilities DROP CONSTRAINT IF EXISTS website_capabilities_evidence_rules;
ALTER TABLE website_capabilities DROP CONSTRAINT IF EXISTS website_capabilities_category_check;
ALTER TABLE website_capabilities DROP COLUMN IF EXISTS statement, DROP COLUMN IF EXISTS claim_id, DROP COLUMN IF EXISTS clue, DROP COLUMN IF EXISTS observation_ids,
    DROP COLUMN IF EXISTS task_id, DROP COLUMN IF EXISTS run_id, DROP COLUMN IF EXISTS agent_type, DROP COLUMN IF EXISTS source;
