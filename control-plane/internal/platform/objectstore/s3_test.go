package objectstore

import (
	"bytes"
	"context"
	"testing"
)

func TestMemoryObjectStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore("systera-artifacts")
	ref, err := st.PutObject(ctx, "t1/m1/v1/openapi.yaml", "application/yaml", []byte("openapi: 3.1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ref != "s3://systera-artifacts/t1/m1/v1/openapi.yaml" {
		t.Fatalf("unexpected ref %q", ref)
	}
	got, err := st.GetObject(ctx, "t1/m1/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("openapi: 3.1.0\n")) {
		t.Fatalf("got %q", got)
	}
}
