DELETE FROM graph_invariants WHERE key IN ('ledger_append_only', 'reviewer_independence', 'confidence_calibration');
UPDATE graph_invariants SET enforced_by = 'GRAPH_SERVICE' WHERE key = 'claim_evidence_requirements';

DROP FUNCTION IF EXISTS claim_requirements_unmet(claims);
DROP TABLE IF EXISTS claim_assumptions;
DROP TABLE IF EXISTS claim_evidence;
DROP TABLE IF EXISTS claims;
DROP TABLE IF EXISTS assumptions;
DROP TABLE IF EXISTS evidence_records;

DROP FUNCTION IF EXISTS ledger_links_append_only();
DROP FUNCTION IF EXISTS claims_requirements_check();
DROP FUNCTION IF EXISTS claims_guard();
DROP FUNCTION IF EXISTS assumptions_guard();
DROP FUNCTION IF EXISTS evidence_records_guard();
