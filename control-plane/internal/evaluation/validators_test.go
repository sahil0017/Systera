package evaluation

import "testing"

func TestDeterministicValidators(t *testing.T) {
	if res := ValidateSQL("CREATE TABLE IF NOT EXISTS urls (id UUID PRIMARY KEY);"); !res.Valid {
		t.Fatalf("expected valid SQL, got %+v", res.Errors)
	}
	if res := ValidateSQL("CREATE TABLE broken (id UUID;"); res.Valid {
		t.Fatal("expected unbalanced parens SQL to fail")
	}

	validOAI := "openapi: 3.1.0\ninfo:\n  title: Test\n  version: \"1.0\"\npaths:\n  /healthz:\n    get:\n      responses:\n        \"200\":\n          description: OK\n"
	if res := ValidateOpenAPI(validOAI); !res.Valid {
		t.Fatalf("expected valid OpenAPI, got %+v", res.Errors)
	}
	if res := ValidateOpenAPI("info:\n  title: Missing\n"); res.Valid {
		t.Fatal("expected incomplete OpenAPI to fail")
	}

	if res := ValidateMermaid("flowchart TD\n    a[\"A\"] --> b[\"B\"]\n"); !res.Valid {
		t.Fatalf("expected valid Mermaid, got %+v", res.Errors)
	}
	if res := ValidateMermaid("timeline\n    a --> b\n"); res.Valid {
		t.Fatal("expected unsupported Mermaid diagram type to fail")
	}
	if res := ValidateMermaid("flowchart TD\n    a[\"Unclosed] --> b\n"); res.Valid {
		t.Fatal("expected unterminated quote in Mermaid to fail")
	}

	if res := ValidateCatalogue("# Catalogue\n- **svc**"); !res.Valid {
		t.Fatalf("expected valid Catalogue, got %+v", res.Errors)
	}
}
