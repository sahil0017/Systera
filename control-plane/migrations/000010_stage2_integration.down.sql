DROP POLICY IF EXISTS system_all_approval_requests ON approval_requests;
DROP POLICY IF EXISTS tenant_isolation_approval_requests ON approval_requests;
DROP TABLE IF EXISTS approval_requests;

DELETE FROM agents WHERE agent_type IN (
    'hld_architect',
    'data_modeller',
    'api_architect',
    'security_architect',
    'independent_reviewer'
);
