package models

import (
	"context"
	"strings"
	"testing"
)

func TestDeterministicHashEmbedderAndPGVector(t *testing.T) {
	emb := NewDeterministicHashEmbedder(DefaultEmbeddingDimensions)
	vecs, err := emb.Embed(context.Background(), "local", []string{
		"transactional outbox pattern with postgresql and nats jetstream",
		"transactional outbox pattern postgresql nats",
		"completely unrelated gardening recipe with tomatoes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 3 || len(vecs[0]) != DefaultEmbeddingDimensions {
		t.Fatalf("unexpected embedding dimensions: %d x %d", len(vecs), len(vecs[0]))
	}
	simClose := CosineSimilarity(vecs[0], vecs[1])
	simFar := CosineSimilarity(vecs[0], vecs[2])
	if simClose <= simFar {
		t.Fatalf("expected closer text to have higher similarity (%f <= %f)", simClose, simFar)
	}

	lit := FormatPGVector([]float32{0.5, -0.25})
	if !strings.HasPrefix(lit, "[0.500000,-0.250000]") {
		t.Fatalf("unexpected pgvector literal: %q", lit)
	}
}
