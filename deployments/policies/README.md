# Systera Security & Governance Policies

Declarative policy definitions enforcing V4.0 trust boundaries (§12, §13, §24.4, §27.1, §31, §33):

| File | Purpose |
| :--- | :--- |
| [tool-capabilities.rego](./tool-capabilities.rego) | OPA Rego policy for Tool Gateway capability grants & high-risk approval gates |
| [egress-allowlist.yaml](./egress-allowlist.yaml) | Default-deny outbound network egress & SSRF protection per worker pool |
| [sandbox-limits.yaml](./sandbox-limits.yaml) | CPU, memory, PID, disk, and wall-clock quotas for ephemeral compiler/simulation sandboxes |
| [sandbox-seccomp.json](./sandbox-seccomp.json) | Minimal Linux seccomp profile for isolated sandbox containers |
| [approval-matrix.yaml](./approval-matrix.yaml) | Human-in-the-loop approval boundaries (§33) |
