package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateMemoryRecordRejectsFullTranscripts(t *testing.T) {
	good := RecordInput{
		MemoryType: TypeProject,
		Summary:    "URL shortener requires < 20ms p95 redirect latency and 100:1 read/write ratio.",
		SourceRefs: []string{"run:r1", "knowledge:k1"},
	}
	if err := ValidateRecord(&good); err != nil {
		t.Fatalf("expected valid memory record, got %v", err)
	}

	tooBig := good
	tooBig.Summary = strings.Repeat("transcript line ", 400)
	if err := ValidateRecord(&tooBig); !errors.Is(err, ErrTranscriptTooLarge) {
		t.Fatalf("expected ErrTranscriptTooLarge, got %v", err)
	}

	badType := good
	badType.MemoryType = "RAW_CHAT"
	if err := ValidateRecord(&badType); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid memory_type, got %v", err)
	}
}
