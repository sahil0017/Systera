// Package auth verifies caller identity server-side. Requests present a bearer JWT
// signed with RS256 and verified against either a static RSA public key or a remote
// OIDC/IdP JWKS endpoint. The token's subject is mapped to a user and the tenant
// membership (and therefore the role) is loaded from the database. Caller-supplied
// identity headers are never trusted.
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// ErrInvalidToken covers every reason a bearer token is rejected (bad format, alg,
	// unknown kid, signature, issuer, audience or validity window). Callers should not distinguish.
	ErrInvalidToken = errors.New("invalid bearer token")
	ErrKeyNotFound  = errors.New("signing key not found")
	ErrWeakKey      = errors.New("RSA key must be at least 2048 bits")
)

const (
	// MinRSAKeyBits is the smallest RSA modulus accepted for RS256 verification.
	MinRSAKeyBits = 2048
	// maxJWKSBytes bounds the JWKS response body read from the identity provider.
	maxJWKSBytes = 1 << 20
)

func checkKeySize(k *rsa.PublicKey) error {
	if k == nil || k.N.BitLen() < MinRSAKeyBits {
		return ErrWeakKey
	}
	return nil
}

var b64 = base64.RawURLEncoding

// Claims are the registered and Systera-specific JWT claims.
type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  Audience `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	ID        string   `json:"jti,omitempty"`
	// TenantID optionally pins the token to one tenant.
	TenantID string `json:"tid,omitempty"`
	// Scope (space-separated, RFC 8693 style) restricts what the token may do. Empty means
	// a full workspace token; "lens" is the Engineering Lens extension scope.
	Scope string `json:"scope,omitempty"`
}

// Scopes returns the token's scopes.
func (c Claims) Scopes() []string { return strings.Fields(c.Scope) }

// Audience accepts both the string and array JSON forms of "aud".
type Audience []string

func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a Audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

func (a Audience) contains(v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

// KeySet resolves RSA public keys by key ID (kid) for RS256 verification.
type KeySet interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// StaticKeySet holds a fixed set of RSA public keys (used for local dev, tests, or pinned keys).
type StaticKeySet struct {
	defaultKey *rsa.PublicKey
	keys       map[string]*rsa.PublicKey
}

// NewStaticKeySet constructs a KeySet from a single default public key and/or kid-indexed keys.
func NewStaticKeySet(defaultKey *rsa.PublicKey, keys map[string]*rsa.PublicKey) (*StaticKeySet, error) {
	if defaultKey == nil && len(keys) == 0 {
		return nil, errors.New("at least one RSA public key is required")
	}
	if defaultKey != nil {
		if err := checkKeySize(defaultKey); err != nil {
			return nil, err
		}
	}
	copied := make(map[string]*rsa.PublicKey, len(keys))
	for k, v := range keys {
		if v != nil {
			if err := checkKeySize(v); err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
			copied[k] = v
		}
	}
	return &StaticKeySet{defaultKey: defaultKey, keys: copied}, nil
}

func (s *StaticKeySet) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if kid != "" {
		if k, ok := s.keys[kid]; ok {
			return k, nil
		}
	}
	if s.defaultKey != nil {
		return s.defaultKey, nil
	}
	if kid == "" && len(s.keys) == 1 {
		for _, k := range s.keys {
			return k, nil
		}
	}
	return nil, ErrKeyNotFound
}

// JWKSKeySet fetches and caches RSA public keys from a remote JWKS endpoint.
type JWKSKeySet struct {
	url         string
	httpClient  *http.Client
	ttl         time.Duration
	minCooldown time.Duration

	// fetchMu serializes fetches so concurrent cache misses produce one request.
	fetchMu     sync.Mutex
	mu          sync.RWMutex
	keys        map[string]*rsa.PublicKey
	lastFetch   time.Time
	lastAttempt time.Time
}

// NewJWKSKeySet creates a caching JWKS KeySet for an external OIDC/IdP endpoint.
func NewJWKSKeySet(jwksURL string, ttl time.Duration) (*JWKSKeySet, error) {
	return NewJWKSKeySetWithClient(jwksURL, ttl, &http.Client{Timeout: 5 * time.Second})
}

// NewJWKSKeySetWithClient creates a caching JWKS KeySet using the supplied HTTP client.
func NewJWKSKeySetWithClient(jwksURL string, ttl time.Duration, client *http.Client) (*JWKSKeySet, error) {
	if jwksURL == "" {
		return nil, errors.New("JWKS URL is required")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &JWKSKeySet{
		url:         jwksURL,
		httpClient:  client,
		ttl:         ttl,
		minCooldown: 10 * time.Second,
		keys:        make(map[string]*rsa.PublicKey),
	}, nil
}

func (j *JWKSKeySet) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.RLock()
	k, ok := j.keys[kid]
	fresh := time.Since(j.lastFetch) < j.ttl
	j.mu.RUnlock()

	if ok && fresh {
		return k, nil
	}
	if err := j.refresh(ctx); err != nil {
		if ok {
			return k, nil
		}
		return nil, err
	}

	j.mu.RLock()
	defer j.mu.RUnlock()
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(j.keys) == 1 {
		for _, only := range j.keys {
			return only, nil
		}
	}
	return nil, ErrKeyNotFound
}

// refresh re-fetches the JWKS. Attempts are rate-limited by minCooldown whether or not
// keys are cached, so an unreachable or empty IdP cannot be hammered by unauthenticated
// requests carrying unknown kids. A failed or empty fetch keeps the previous keys.
func (j *JWKSKeySet) refresh(ctx context.Context) error {
	j.fetchMu.Lock()
	defer j.fetchMu.Unlock()

	j.mu.Lock()
	if !j.lastAttempt.IsZero() && time.Since(j.lastAttempt) < j.minCooldown {
		j.mu.Unlock()
		return nil
	}
	j.lastAttempt = time.Now()
	j.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := j.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch returned %d", resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}

	parsed := make(map[string]*rsa.PublicKey)
	for _, entry := range doc.Keys {
		if entry.Kty != "RSA" || (entry.Use != "" && entry.Use != "sig") {
			continue
		}
		if entry.Alg != "" && entry.Alg != "RS256" {
			continue
		}
		nBytes, err := b64.DecodeString(entry.N)
		if err != nil {
			continue
		}
		eBytes, err := b64.DecodeString(entry.E)
		if err != nil || len(eBytes) > 4 {
			continue
		}
		eInt := new(big.Int).SetBytes(eBytes).Int64()
		if eInt <= 1 {
			continue
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(eInt)}
		if checkKeySize(key) != nil {
			continue
		}
		parsed[entry.Kid] = key
	}
	if len(parsed) == 0 {
		return errors.New("jwks contained no usable RS256 keys")
	}

	j.mu.Lock()
	j.keys = parsed
	j.lastFetch = time.Now()
	j.mu.Unlock()
	return nil
}

// Verifier validates RS256 JWTs against a KeySet.
type Verifier struct {
	keys     KeySet
	issuer   string
	audience string
	leeway   time.Duration
	now      func() time.Time
}

func NewVerifier(keys KeySet, issuer, audience string) (*Verifier, error) {
	if keys == nil {
		return nil, errors.New("key set is required")
	}
	if issuer == "" || audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	return &Verifier{keys: keys, issuer: issuer, audience: audience, leeway: 30 * time.Second, now: time.Now}, nil
}

// Verify checks signature, algorithm (RS256 only), issuer, audience and validity window.
func (v *Verifier) Verify(token string) (Claims, error) {
	return v.VerifyContext(context.Background(), token)
}

// VerifyContext is Verify with a context bounding any key-set fetch (e.g. JWKS refresh).
func (v *Verifier) VerifyContext(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}

	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "RS256" {
		return Claims{}, ErrInvalidToken // rejects "none", HS256, and algorithm confusion
	}

	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}

	pubKey, err := v.keys.Key(ctx, header.Kid)
	if err != nil || pubKey == nil {
		return Claims{}, ErrInvalidToken
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, ErrInvalidToken
	}

	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrInvalidToken
	}

	now := v.now()
	switch {
	case c.Subject == "":
		return Claims{}, ErrInvalidToken
	case c.Issuer != v.issuer:
		return Claims{}, ErrInvalidToken
	case !c.Audience.contains(v.audience):
		return Claims{}, ErrInvalidToken
	case c.ExpiresAt == 0 || now.After(time.Unix(c.ExpiresAt, 0).Add(v.leeway)):
		return Claims{}, ErrInvalidToken
	case c.NotBefore != 0 && now.Add(v.leeway).Before(time.Unix(c.NotBefore, 0)):
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

// SignRS256 issues an RS256 JWT using an RSA private key (used by systera-admin and tests).
func SignRS256(priv *rsa.PrivateKey, kid string, c Claims) (string, error) {
	if priv == nil {
		return "", errors.New("RSA private key is required")
	}
	headerMap := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		headerMap["kid"] = kid
	}
	headerBytes, err := json.Marshal(headerMap)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signingInput := b64.EncodeToString(headerBytes) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(nil, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign RS256: %w", err)
	}
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// ParsePublicKeyPEM parses a PKIX or PKCS#1 RSA public key from PEM bytes.
func ParsePublicKeyPEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM block for RSA public key")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("PEM key is not an RSA public key")
		}
		return rsaPub, nil
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}

// ParsePrivateKeyPEM parses a PKCS#8 or PKCS#1 RSA private key from PEM bytes.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM block for RSA private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PEM key is not an RSA private key")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
