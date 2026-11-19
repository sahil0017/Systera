DROP POLICY IF EXISTS tenant_isolation_audit_events ON audit_events;
DROP POLICY IF EXISTS tenant_isolation_design_runs ON design_runs;
DROP POLICY IF EXISTS tenant_isolation_projects ON projects;

DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS design_runs;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS tenant_memberships;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;

