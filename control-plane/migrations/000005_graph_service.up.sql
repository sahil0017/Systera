-- Stage 2.2: the graph service is the only writer of the architecture graph.
--
-- systera_graph is assumed (SET LOCAL ROLE) exclusively by internal/graph's write paths
-- (model creation, patch application, snapshot publishing). The general application and
-- system roles become read-only on graph tables, so agents, workers and any other code
-- path cannot write graph rows directly: every change must go through a GraphPatch.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'systera_graph') THEN
        CREATE ROLE systera_graph NOLOGIN NOSUPERUSER NOBYPASSRLS;
    END IF;
END
$$;
GRANT systera_graph TO CURRENT_USER;

REVOKE INSERT, UPDATE, DELETE ON architecture_models, architecture_versions, architecture_nodes,
    architecture_edges, graph_patches, graph_snapshots FROM systera_app, systera_system;
GRANT SELECT ON architecture_models, architecture_versions, architecture_nodes, architecture_edges,
    graph_patches, graph_snapshots, graph_invariants TO systera_app, systera_system;

GRANT SELECT, INSERT, UPDATE ON architecture_models, architecture_nodes, architecture_edges, graph_patches TO systera_graph;
GRANT SELECT, INSERT ON architecture_versions, graph_snapshots TO systera_graph;
GRANT SELECT ON graph_invariants, projects, design_runs, agent_tasks TO systera_graph;
GRANT SELECT, INSERT ON outbox_events TO systera_graph;
GRANT USAGE ON SEQUENCE architecture_nodes_row_id_seq, architecture_edges_row_id_seq, outbox_events_seq_seq TO systera_graph;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['architecture_models', 'architecture_versions', 'architecture_nodes',
                             'architecture_edges', 'graph_patches', 'graph_snapshots'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS graph_writer_%s ON %I', t, t);
        EXECUTE format($p$CREATE POLICY graph_writer_%s ON %I TO systera_graph
            USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid)$p$, t, t);
    END LOOP;
END
$$;

-- design_runs / agent_tasks are read by the graph service to validate patch provenance.
DROP POLICY IF EXISTS graph_reader_design_runs ON design_runs;
CREATE POLICY graph_reader_design_runs ON design_runs FOR SELECT TO systera_graph
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);
DROP POLICY IF EXISTS graph_reader_agent_tasks ON agent_tasks;
CREATE POLICY graph_reader_agent_tasks ON agent_tasks FOR SELECT TO systera_graph
    USING (tenant_id = NULLIF(current_setting('app.current_tenant_id', true), '')::uuid);

-- Invariants the graph service now enforces.
UPDATE graph_invariants SET active = true
WHERE key IN ('edge_type_compatibility', 'patch_optimistic_concurrency');
INSERT INTO graph_invariants (key, description, severity, enforced_by, active, spec_ref) VALUES
    ('graph_writes_via_patches', 'Only the graph service (role systera_graph) can write graph tables; all changes are GraphPatches.', 'ERROR', 'DATABASE', true, '§23.1'),
    ('unique_node_key', 'A node key names at most one live node in a model.', 'ERROR', 'GRAPH_SERVICE', true, '§23'),
    ('patch_has_effect', 'An accepted patch changes the graph content (graph_hash differs from its base).', 'ERROR', 'GRAPH_SERVICE', true, 'Checklist 2.7')
ON CONFLICT (key) DO NOTHING;
