package auth

import (
	"crypto/rsa"
	"errors"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
)

// DefaultLensTokenTTL is the lifetime of an Engineering Lens token.
const DefaultLensTokenTTL = 15 * time.Minute

// TokenIssuer mints scoped RS256 tokens for an already-authenticated workspace user (the
// extension token exchange, §20.3). It signs with the same key the verifier trusts.
type TokenIssuer struct {
	priv     *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	now      func() time.Time
}

// NewTokenIssuer returns an issuer; issuer/audience must match the Verifier's.
func NewTokenIssuer(priv *rsa.PrivateKey, kid, issuer, audience string) (*TokenIssuer, error) {
	if priv == nil || issuer == "" || audience == "" {
		return nil, errors.New("token issuer needs a private key, issuer and audience")
	}
	return &TokenIssuer{priv: priv, kid: kid, issuer: issuer, audience: audience, now: time.Now}, nil
}

// IssueScoped mints a token for subject pinned to tenantID with the given scope. ttl is
// clamped to (0, MaxScopedTokenLifetime].
func (t *TokenIssuer) IssueScoped(subject, tenantID, scope string, ttl time.Duration) (string, time.Time, error) {
	if subject == "" || tenantID == "" || scope == "" {
		return "", time.Time{}, errors.New("subject, tenant and scope are required")
	}
	if ttl <= 0 || ttl > MaxScopedTokenLifetime {
		ttl = DefaultLensTokenTTL
	}
	now := t.now().UTC()
	exp := now.Add(ttl)
	tok, err := SignRS256(t.priv, t.kid, Claims{
		Issuer: t.issuer, Subject: subject, Audience: Audience{t.audience}, IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: exp.Unix(), ID: ids.New(), TenantID: tenantID, Scope: scope,
	})
	return tok, exp, err
}
