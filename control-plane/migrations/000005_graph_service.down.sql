DELETE FROM graph_invariants WHERE key IN ('graph_writes_via_patches', 'unique_node_key', 'patch_has_effect');
UPDATE graph_invariants SET active = false WHERE key IN ('edge_type_compatibility', 'patch_optimistic_concurrency');

DROP POLICY IF EXISTS graph_reader_agent_tasks ON agent_tasks;
DROP POLICY IF EXISTS graph_reader_design_runs ON design_runs;
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['architecture_models', 'architecture_versions', 'architecture_nodes',
                             'architecture_edges', 'graph_patches', 'graph_snapshots'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS graph_writer_%s ON %I', t, t);
    END LOOP;
END
$$;

REVOKE ALL ON architecture_models, architecture_versions, architecture_nodes, architecture_edges,
    graph_patches, graph_snapshots, graph_invariants, projects, design_runs, agent_tasks, outbox_events FROM systera_graph;
REVOKE ALL ON SEQUENCE architecture_nodes_row_id_seq, architecture_edges_row_id_seq, outbox_events_seq_seq FROM systera_graph;

-- Restore 000004 privileges.
GRANT SELECT, INSERT, UPDATE ON architecture_models, architecture_nodes, architecture_edges, graph_patches
    TO systera_app, systera_system;
GRANT SELECT, INSERT ON architecture_versions, graph_snapshots TO systera_app, systera_system;
