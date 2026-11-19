DELETE FROM graph_invariants WHERE key = 'no_orphaned_required_relations';

UPDATE graph_invariants
SET active = false
WHERE key IN (
    'edge_type_compatibility',
    'claim_evidence_requirements',
    'patch_optimistic_concurrency'
);
