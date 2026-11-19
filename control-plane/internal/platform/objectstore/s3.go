// Package objectstore provides an S3-compatible object storage adapter (MinIO / AWS S3)
// using AWS Signature V4 over standard net/http, plus an in-memory store for unit tests.
package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("object not found")

// Store persists and retrieves immutable artifact blobs.
type Store interface {
	PutObject(ctx context.Context, key, contentType string, data []byte) (ref string, err error)
	GetObject(ctx context.Context, key string) ([]byte, error)
}

// MemoryStore is a thread-safe in-memory object store for unit tests.
type MemoryStore struct {
	mu     sync.RWMutex
	bucket string
	items  map[string][]byte
}

func NewMemoryStore(bucket string) *MemoryStore {
	if bucket == "" {
		bucket = "systera-artifacts"
	}
	return &MemoryStore{bucket: bucket, items: make(map[string][]byte)}
}

func (m *MemoryStore) PutObject(_ context.Context, key, _ string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	m.items[key] = copied
	return fmt.Sprintf("s3://%s/%s", m.bucket, strings.TrimPrefix(key, "/")), nil
}

func (m *MemoryStore) GetObject(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.items[key]
	if !ok {
		return nil, ErrNotFound
	}
	copied := make([]byte, len(b))
	copy(copied, b)
	return copied, nil
}

// S3Config configures the S3-compatible object store client.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3Store writes and reads objects from MinIO or Amazon S3 using SigV4 path-style requests.
type S3Store struct {
	cfg    S3Config
	client *http.Client
	now    func() time.Time
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3 endpoint and bucket are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3Store{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
		now:    time.Now,
	}, nil
}

func (s *S3Store) objectURL(key string) (string, string, error) {
	base := strings.TrimRight(s.cfg.Endpoint, "/")
	cleanKey := strings.TrimLeft(key, "/")
	full := fmt.Sprintf("%s/%s/%s", base, s.cfg.Bucket, cleanKey)
	u, err := url.Parse(full)
	if err != nil {
		return "", "", err
	}
	return full, u.EscapedPath(), nil
}

func (s *S3Store) PutObject(ctx context.Context, key, contentType string, data []byte) (string, error) {
	fullURL, canonicalURI, err := s.objectURL(key)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fullURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	s.signV4(req, canonicalURI, data)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("s3 put %s returned %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Sprintf("s3://%s/%s", s.cfg.Bucket, strings.TrimLeft(key, "/")), nil
}

func (s *S3Store) GetObject(ctx context.Context, key string) ([]byte, error) {
	fullURL, canonicalURI, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	s.signV4(req, canonicalURI, nil)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 get %s returned %d", key, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (s *S3Store) signV4(req *http.Request, canonicalURI string, payload []byte) {
	if s.cfg.AccessKey == "" || s.cfg.SecretKey == "" {
		return
	}
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(payload)

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.URL.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, sig,
	))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
