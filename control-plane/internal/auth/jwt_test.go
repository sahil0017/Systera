package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func generateTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

func TestVerifyRoundTripAndRejections(t *testing.T) {
	priv := generateTestRSAKey(t)
	ks, err := NewStaticKeySet(&priv.PublicKey, map[string]*rsa.PublicKey{"k1": &priv.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(ks, "iss", "aud")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	good := Claims{Issuer: "iss", Subject: "u", Audience: Audience{"aud"}, ExpiresAt: now.Add(time.Minute).Unix()}
	tok, err := SignRS256(priv, "k1", good)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := v.Verify(tok); err != nil || c.Subject != "u" {
		t.Fatalf("verify good token: %v %+v", err, c)
	}

	parts := strings.Split(tok, ".")
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	hs256 := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + parts[1] + "." + parts[2]
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"iss","sub":"admin","aud":"aud","exp":9999999999}`))
	tampered := parts[0] + "." + forged + "." + parts[2]

	otherPriv := generateTestRSAKey(t)
	wrongKeyTok, _ := SignRS256(otherPriv, "k1", good)

	noExp := good
	noExp.ExpiresAt = 0
	noExpTok, _ := SignRS256(priv, "k1", noExp)
	future := good
	future.NotBefore = now.Add(time.Hour).Unix()
	futureTok, _ := SignRS256(priv, "k1", future)

	for name, bad := range map[string]string{
		"alg none":      none,
		"alg hs256":     hs256,
		"tampered":      tampered,
		"wrong rsa key": wrongKeyTok,
		"no exp":        noExpTok,
		"not yet valid": futureTok,
		"two parts":     parts[0] + "." + parts[1],
		"empty":         "",
	} {
		if _, err := v.Verify(bad); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestAudienceArrayForm(t *testing.T) {
	priv := generateTestRSAKey(t)
	ks, _ := NewStaticKeySet(&priv.PublicKey, nil)
	v, _ := NewVerifier(ks, "iss", "aud")
	tok, _ := SignRS256(priv, "", Claims{Issuer: "iss", Subject: "u", Audience: Audience{"other", "aud"}, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("array audience: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestJWKSKeySetRoundTrip(t *testing.T) {
	priv := generateTestRSAKey(t)
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())

	jwksPayload, _ := json.Marshal(map[string]any{
		"keys": []map[string]string{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "jwks-key-1", "n": n, "e": e},
		},
	})

	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(jwksPayload)),
			}, nil
		}),
	}

	ks, err := NewJWKSKeySetWithClient("https://idp.example.com/.well-known/jwks.json", time.Minute, client)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(ks, "iss", "aud")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := SignRS256(priv, "jwks-key-1", Claims{
		Issuer:    "iss",
		Subject:   "ext-user",
		Audience:  Audience{"aud"},
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(tok)
	if err != nil || c.Subject != "ext-user" {
		t.Fatalf("jwks verify: %v %+v", err, c)
	}
}

func jwksDoc(t *testing.T, kid string, pub *rsa.PublicKey) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An unreachable IdP or a flood of unknown kids must not turn every request into an
// outbound JWKS fetch, even when nothing is cached yet.
func TestJWKSRefreshIsRateLimited(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	ks, _ := NewJWKSKeySetWithClient("https://idp.example.com/jwks", time.Minute, client)
	v, _ := NewVerifier(ks, "iss", "aud")
	tok, _ := SignRS256(generateTestRSAKey(t), "unknown", Claims{Issuer: "iss", Subject: "u", Audience: Audience{"aud"}, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	for i := 0; i < 50; i++ {
		if _, err := v.Verify(tok); err == nil {
			t.Fatal("token must be rejected while JWKS is unavailable")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected 1 JWKS fetch within the cooldown, got %d", n)
	}
}

// A malformed or empty JWKS response must not wipe the cached keys.
func TestJWKSBadResponseKeepsCachedKeys(t *testing.T) {
	priv := generateTestRSAKey(t)
	good := jwksDoc(t, "k1", &priv.PublicKey)
	var serveEmpty atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := good
		if serveEmpty.Load() {
			body = []byte(`{"keys":[]}`)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	ks, _ := NewJWKSKeySetWithClient("https://idp.example.com/jwks", time.Nanosecond, client)
	ks.minCooldown = 0
	v, _ := NewVerifier(ks, "iss", "aud")
	tok, _ := SignRS256(priv, "k1", Claims{Issuer: "iss", Subject: "u", Audience: Audience{"aud"}, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("initial verify: %v", err)
	}
	serveEmpty.Store(true)
	time.Sleep(time.Millisecond) // cache is now stale; next lookup refreshes
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("empty JWKS response dropped cached key: %v", err)
	}
}

func TestWeakRSAKeysRejected(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStaticKeySet(&small.PublicKey, nil); err == nil {
		t.Fatal("1024-bit static key must be rejected")
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(jwksDoc(t, "weak", &small.PublicKey)))}, nil
	})}
	ks, _ := NewJWKSKeySetWithClient("https://idp.example.com/jwks", time.Minute, client)
	if _, err := ks.Key(t.Context(), "weak"); err == nil {
		t.Fatal("1024-bit JWKS key must be ignored")
	}
}
