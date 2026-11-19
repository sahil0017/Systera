-- Stage 2 (Checklist 3.1-3.6): Activate remaining enforced graph invariants
-- and register no_orphaned_required_relations.

UPDATE graph_invariants
SET active = true
WHERE key IN (
    'edge_type_compatibility',
    'claim_evidence_requirements',
    'patch_optimistic_concurrency'
);

INSERT INTO graph_invariants (key, description, severity, enforced_by, active, spec_ref) VALUES
    ('no_orphaned_required_relations', 'Structural sub-elements (api, table, event, topic) must have at least one relation.', 'ERROR', 'GRAPH_SERVICE', true, 'Checklist 3.5')
ON CONFLICT (key) DO UPDATE SET active = EXCLUDED.active;
