package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/systera/control-plane/internal/tenancy"
)

type fakeMembers struct{}

func (fakeMembers) Resolve(_ context.Context, subject, tenantID string) (Membership, error) {
	if tenantID == "" {
		tenantID = "11111111-1111-1111-1111-111111111111"
	}
	return Membership{UserID: "u-" + subject, TenantID: tenantID, Role: tenancy.RoleEngineer}, nil
}

func TestScopedLensTokens(t *testing.T) {
	priv := generateTestRSAKey(t)
	ks, _ := NewStaticKeySet(&priv.PublicKey, nil)
	v, _ := NewVerifier(ks, "iss", "aud")
	a := NewAuthenticator(v, fakeMembers{})
	tenant := "22222222-2222-2222-2222-222222222222"
	now := time.Now()
	sign := func(c Claims) string {
		c.Issuer, c.Audience, c.Subject = "iss", Audience{"aud"}, "alice"
		tok, err := SignRS256(priv, "", c)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + tok
	}

	iss, err := NewTokenIssuer(priv, "", "iss", "aud")
	if err != nil {
		t.Fatal(err)
	}
	tok, exp, err := iss.IssueScoped("alice", tenant, ScopeLens, 0)
	if err != nil || time.Until(exp) > DefaultLensTokenTTL+time.Second {
		t.Fatalf("issue: %v %v", exp, err)
	}
	p, err := a.Authenticate(context.Background(), "Bearer "+tok, "")
	if err != nil || p.TenantID != tenant || len(p.Scopes) != 1 || p.Scopes[0] != ScopeLens || p.Subject != "alice" {
		t.Fatalf("lens principal: %+v %v", p, err)
	}
	if _, _, err := iss.IssueScoped("alice", tenant, ScopeLens, 3*time.Hour); err != nil {
		t.Fatal(err) // clamped, not refused
	}

	for name, c := range map[string]Claims{
		"too long-lived": {IssuedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Hour).Unix(), TenantID: tenant, Scope: ScopeLens},
		"no iat":         {ExpiresAt: now.Add(time.Minute).Unix(), TenantID: tenant, Scope: ScopeLens},
		"no tenant pin":  {IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Scope: ScopeLens},
		"unknown scope":  {IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), TenantID: tenant, Scope: "lens admin"},
	} {
		if _, err := a.Authenticate(context.Background(), sign(c), ""); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	// Unscoped workspace tokens are unaffected.
	if _, err := a.Authenticate(context.Background(), sign(Claims{ExpiresAt: now.Add(8 * time.Hour).Unix()}), ""); err != nil {
		t.Fatalf("workspace token: %v", err)
	}

	for path, want := range map[string]bool{
		"/v1/browser/sessions": true, "/v1/browser/observations": true, "/v1/websites": true, "/v1/websites/x/promote": true,
		"/v1/projects": false, "/v1/auth/lens-token": false, "/v1/websitesx": false, "/v1/knowledge/search": false,
	} {
		if got := ScopeAllows([]string{ScopeLens}, path); got != want {
			t.Errorf("lens scope on %s = %v", path, got)
		}
	}
	if !ScopeAllows(nil, "/v1/projects") {
		t.Fatal("unscoped token restricted")
	}
}
