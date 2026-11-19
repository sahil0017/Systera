ALTER TABLE IF EXISTS architecture_versions DROP CONSTRAINT IF EXISTS architecture_versions_patch_fk;
ALTER TABLE IF EXISTS architecture_models DROP CONSTRAINT IF EXISTS architecture_models_current_version_fk;

DROP TABLE IF EXISTS graph_snapshots;
DROP TABLE IF EXISTS graph_invariants;
DROP TABLE IF EXISTS graph_patches;
DROP TABLE IF EXISTS architecture_edges;
DROP TABLE IF EXISTS architecture_nodes;
DROP TABLE IF EXISTS architecture_versions;
DROP TABLE IF EXISTS architecture_models;

DROP FUNCTION IF EXISTS graph_snapshots_guard();
DROP FUNCTION IF EXISTS graph_patches_guard();
DROP FUNCTION IF EXISTS arch_nodes_close_check();
DROP FUNCTION IF EXISTS arch_edges_endpoints_check();
DROP FUNCTION IF EXISTS arch_node_live_at(UUID, UUID, INT);
DROP FUNCTION IF EXISTS arch_edges_identity();
DROP FUNCTION IF EXISTS arch_nodes_identity();
DROP FUNCTION IF EXISTS arch_rows_close_only();
DROP FUNCTION IF EXISTS arch_models_guard();
DROP FUNCTION IF EXISTS arch_versions_guard();
-- btree_gist is left installed: other schemas may depend on it.
