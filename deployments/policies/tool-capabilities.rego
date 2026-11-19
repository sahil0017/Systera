package systera.toolgateway

default allow := false

# Allow a tool call only when:
# 1. The task lease explicitly grants the tool's required capability.
# 2. The tenant_id on the lease matches the target resource tenant_id.
# 3. High-risk actions carry a verified human approval_id.
allow if {
    input.tenant_id != ""
    input.lease.tenant_id == input.tenant_id
    required_cap := input.tool.capability
    required_cap in input.lease.granted_capabilities
    not requires_approval_block
}

requires_approval_block if {
    input.risk_level == "HIGH"
    not input.approval.approved
}

# Deny reason helpers for audit logging
deny_reasons contains msg if {
    input.lease.tenant_id != input.tenant_id
    msg := "cross-tenant tool invocation denied"
}

deny_reasons contains msg if {
    required_cap := input.tool.capability
    not required_cap in input.lease.granted_capabilities
    msg := sprintf("missing capability grant: %s", [required_cap])
}

deny_reasons contains msg if {
    input.risk_level == "HIGH"
    not input.approval.approved
    msg := "high-risk action requires human approval"
}
