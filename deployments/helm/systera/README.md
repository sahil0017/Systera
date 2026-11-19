# Systera Helm Chart

Deploys the Systera Control Plane (`api`, `scheduler`, `worker-agent`, and pre-upgrade `migrate` hook) on Kubernetes.

## Quickstart

1. Create a Kubernetes Secret (recommended over passing secrets in `values.yaml`):
   ```bash
   kubectl create secret generic systera-secrets \
     --from-literal=SYSTERA_POSTGRES_PASSWORD="strong-password" \
     --from-literal=SYSTERA_MODEL_API_KEY="gsk_your_key" \
     --from-literal=SYSTERA_S3_ACCESS_KEY="your-access-key" \
     --from-literal=SYSTERA_S3_SECRET_KEY="your-secret-key"
   ```

2. Install or upgrade the chart:
   ```bash
   helm upgrade --install systera ./deployments/helm/systera \
     --set secrets.existingSecret=systera-secrets \
     --set config.auth.jwksUrl="https://idp.example.com/.well-known/jwks.json"
   ```
