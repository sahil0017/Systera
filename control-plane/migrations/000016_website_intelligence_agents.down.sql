DROP TABLE IF EXISTS architecture_evidence;
DROP TABLE IF EXISTS architecture_hypotheses;
DROP TABLE IF EXISTS website_capabilities;

DELETE FROM agents WHERE agent_type IN (
    'product_discovery',
    'user_journey',
    'technology_signal',
    'domain_architect',
    'architecture_hypothesis',
    'database_architect',
    'infrastructure_agent'
);
