# Systera — Single Consolidated AWS CloudFormation Stack (Free Tier)

All AWS infrastructure resources are embedded into a **single CloudFormation stack**:
[systera-free-tier.yaml](./systera-free-tier.yaml)

## Embedded Free Tier Resources

| Resource | AWS Service | Free Tier Sizing |
| :--- | :--- | :--- |
| `VPC`, `PublicSubnetA`, `PrivateSubnetA/B`, `InternetGateway` | Amazon VPC | Zero NAT Gateway (avoids hourly NAT charges) |
| `ControlPlaneSecurityGroup`, `RDSSecurityGroup` | Amazon EC2 Security Groups | Least-privilege ingress (8080 HTTP; 5432 RDS from Control Plane only) |
| `ControlPlaneHost` | Amazon EC2 | `t3.micro` (or `t2.micro`), 20 GB encrypted `gp3` EBS |
| `PostgresDB` | Amazon RDS for PostgreSQL 16 | `db.t4g.micro`, 20 GB `gp2`, Single-AZ, encrypted |
| `ArtifactsBucket` | Amazon S3 | Standard tier, AES256 encryption, versioning enabled |
| `ControlPlaneECRRepository` | Amazon ECR | Private repository (500 MB/month free tier) |
| `ControlPlaneLogGroup` | Amazon CloudWatch Logs | 7-day retention (5 GB/month free tier) |
| `ControlPlaneEC2Role`, `ControlPlaneInstanceProfile` | AWS IAM | Scoped S3, ECR, and CloudWatch permissions |

## Deploy

```bash
aws cloudformation deploy \
  --stack-name systera-dev \
  --template-file deployments/aws/systera-free-tier.yaml \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides \
    Environment=dev \
    ImageTag="v0.1.0" \
    DBPassword="replace-with-strong-password"
```

## Populate Runtime Secrets in AWS SSM Parameter Store

Secrets are **not** written into EC2 `UserData`. After deploying the stack, store secrets as `SecureString` parameters in SSM Parameter Store; the EC2 instance fetches them via `/opt/systera/load-secrets.sh` using its IAM role:

```bash
# 1. PostgreSQL password (required)
aws ssm put-parameter \
  --name "/systera/dev/db_password" \
  --type SecureString \
  --value "replace-with-strong-password" \
  --overwrite

# 2. RSA public key PEM for RS256 verification (optional if using AuthJWKSURL)
aws ssm put-parameter \
  --name "/systera/dev/auth_public_key_pem" \
  --type SecureString \
  --value "$(cat public.pem)" \
  --overwrite
```

## Build, Push, and Start the API Service

The EC2 host installs a `systera-api.service` systemd unit that runs `/opt/systera/load-secrets.sh`, logs in to ECR, pulls `${ImageTag}`, and starts the container with CloudWatch logging. Once your image is pushed to ECR and SSM parameters are set:

```bash
sudo systemctl restart systera-api
sudo systemctl status systera-api
```
