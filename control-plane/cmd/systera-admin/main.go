// Command systera-admin bootstraps identities and issues access tokens.
//
//	systera-admin bootstrap -tenant-slug acme -tenant-name "Acme" -email a@acme.test -name "Ada" [-role OWNER] [-subject ...]
//	systera-admin token -subject a@acme.test [-tenant <uuid>] [-ttl 1h]
//
// bootstrap upserts a tenant, a user and their membership, and prints the IDs.
// token signs an RS256 JWT with the RSA private key in SYSTERA_AUTH_PRIVATE_KEY_FILE
// (see `make keys`). In production, tokens would
// come from your identity provider, verified via SYSTERA_AUTH_JWKS_URL.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/platform/config"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/postgres"
	"github.com/systera/control-plane/internal/tenancy"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: systera-admin bootstrap|token|pulse-sync|pulse-sources|pulse-approve|pulse-suspend|pulse-run [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bootstrap":
		err = bootstrap(os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	case "pulse-sync", "pulse-sources", "pulse-approve", "pulse-suspend", "pulse-run":
		err = pulseCommand(os.Args[1], os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "systera-admin:", err)
		os.Exit(1)
	}
}

func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	slug := fs.String("tenant-slug", "", "tenant slug (unique)")
	tenantName := fs.String("tenant-name", "", "tenant display name")
	email := fs.String("email", "", "user email (unique)")
	name := fs.String("name", "", "user display name")
	role := fs.String("role", string(tenancy.RoleOwner), "membership role")
	subject := fs.String("subject", "", "token subject (users.external_sub); defaults to email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" || *email == "" {
		return fmt.Errorf("-tenant-slug and -email are required")
	}
	if *tenantName == "" {
		*tenantName = *slug
	}
	if *name == "" {
		*name = *email
	}
	if *subject == "" {
		*subject = *email
	}
	if !tenancy.Role(*role).Valid() {
		return fmt.Errorf("invalid role %q", *role)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := postgres.Open(ctx, cfg.PostgresDSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	var tenantID, userID string
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `
		INSERT INTO tenants (slug, name) VALUES ($1, $2)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()
		RETURNING id::text`, *slug, *tenantName).Scan(&tenantID); err != nil {
		return fmt.Errorf("upsert tenant: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (email, display_name, external_sub) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET display_name = EXCLUDED.display_name,
		                                  external_sub = EXCLUDED.external_sub, updated_at = NOW()
		RETURNING id::text`, *email, *name, *subject).Scan(&userID); err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenant_memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		tenantID, userID, *role); err != nil {
		return fmt.Errorf("upsert membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{
		"tenant_id": tenantID, "user_id": userID, "subject": *subject, "role": *role,
	})
}

func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	subject := fs.String("subject", "", "token subject (users.external_sub)")
	tenant := fs.String("tenant", "", "optional tenant UUID to pin the token to")
	ttl := fs.Duration("ttl", time.Hour, "token lifetime")
	keyFile := fs.String("key-file", "", "path to RSA private key PEM (defaults to SYSTERA_AUTH_PRIVATE_KEY_FILE)")
	kid := fs.String("kid", "", "optional key ID (defaults to SYSTERA_AUTH_KEY_ID)")
	scope := fs.String("scope", "", "optional scope, e.g. lens (Engineering Lens extension: needs -tenant and -ttl <= 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" {
		return fmt.Errorf("-subject is required")
	}
	if *tenant != "" && !ids.Valid(*tenant) {
		return fmt.Errorf("-tenant must be a UUID")
	}
	if *scope != "" && (*tenant == "" || *ttl > auth.MaxScopedTokenLifetime) {
		return fmt.Errorf("scoped tokens need -tenant and -ttl of at most %s", auth.MaxScopedTokenLifetime)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	privPath := *keyFile
	if privPath == "" {
		privPath = cfg.AuthPrivateKeyFile
	}
	if privPath == "" {
		return fmt.Errorf("RSA private key file required via -key-file or SYSTERA_AUTH_PRIVATE_KEY_FILE")
	}
	pemBytes, err := os.ReadFile(privPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	privKey, err := auth.ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		return err
	}
	keyID := *kid
	if keyID == "" {
		keyID = cfg.AuthKeyID
	}
	now := time.Now()
	tok, err := auth.SignRS256(privKey, keyID, auth.Claims{
		Issuer:    cfg.AuthIssuer,
		Subject:   *subject,
		Audience:  auth.Audience{cfg.AuthAudience},
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(*ttl).Unix(),
		ID:        ids.New(),
		TenantID:  *tenant,
		Scope:     *scope,
	})
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
