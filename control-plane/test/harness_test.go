package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/systera/control-plane/internal/auth"
	"github.com/systera/control-plane/internal/platform/ids"
)

const (
	testIssuer   = "systera-test"
	testAudience = "systera-control-plane-test"
)

var (
	testPrivateKey, _  = rsa.GenerateKey(rand.Reader, 2048)
	otherPrivateKey, _ = rsa.GenerateKey(rand.Reader, 2048)
)

func newVerifier(t *testing.T) *auth.Verifier {
	t.Helper()
	ks, err := auth.NewStaticKeySet(&testPrivateKey.PublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(ks, testIssuer, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mintToken(t *testing.T, subject, tenantID string, ttl time.Duration) string {
	t.Helper()
	return mintTokenWith(t, testPrivateKey, auth.Claims{Subject: subject, TenantID: tenantID}, ttl)
}

func mintTokenWith(t *testing.T, priv *rsa.PrivateKey, c auth.Claims, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	if c.Issuer == "" {
		c.Issuer = testIssuer
	}
	if c.Audience == nil {
		c.Audience = auth.Audience{testAudience}
	}
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(ttl).Unix()
	c.ID = ids.New()
	tok, err := auth.SignRS256(priv, "", c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type client struct {
	t       *testing.T
	handler http.Handler
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (c client) do(method, path, token, body string, headers map[string]string) response {
	c.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return response{code: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func expect(t *testing.T, r response, code int, what string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("%s: expected %d, got %d: %s", what, code, r.code, r.body)
	}
}

// sseEvent is one parsed Server-Sent Event.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// openSSE connects to a real HTTP server and returns a channel of parsed events.
func openSSE(t *testing.T, ctx context.Context, url, token string, headers map[string]string) <-chan sseEvent {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("sse: expected 200, got %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("sse: content-type %q", ct)
	}
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					out <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, ch <-chan sseEvent, within time.Duration) sseEvent {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("sse stream closed unexpectedly")
		}
		return e
	case <-time.After(within):
		t.Fatalf("no sse event within %s", within)
	}
	return sseEvent{}
}

func expectClosed(t *testing.T, ch <-chan sseEvent, within time.Duration) {
	t.Helper()
	select {
	case e, ok := <-ch:
		if ok {
			t.Fatalf("expected stream to close, got event %+v", e)
		}
	case <-time.After(within):
		t.Fatalf("stream did not close within %s", within)
	}
}
