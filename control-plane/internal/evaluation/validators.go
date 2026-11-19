// Package evaluation provides deterministic validators for generated architecture
// projections (SQL DDL, OpenAPI, Mermaid, and Service Catalogue) per spec §15.2.
package evaluation

import (
	"fmt"
	"strings"
)

// ValidationResult reports whether a generated projection passed deterministic checks.
type ValidationResult struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ValidateProjection dispatches deterministic validation by projection type.
func ValidateProjection(projectionType, content string) ValidationResult {
	switch strings.ToUpper(strings.TrimSpace(projectionType)) {
	case "SQL":
		return ValidateSQL(content)
	case "OPENAPI":
		return ValidateOpenAPI(content)
	case "MERMAID":
		return ValidateMermaid(content)
	case "CATALOGUE":
		return ValidateCatalogue(content)
	default:
		return ValidationResult{Valid: false, Errors: []string{fmt.Sprintf("unsupported projection type %q", projectionType)}}
	}
}

// ValidateSQL performs deterministic structural and syntax checks on generated SQL DDL.
func ValidateSQL(ddl string) ValidationResult {
	var errs []string
	trimmed := strings.TrimSpace(ddl)
	if trimmed == "" {
		return ValidationResult{Valid: false, Errors: []string{"SQL DDL is empty"}}
	}

	parenBalance := 0
	inSingleQuote := false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == '\'' {
			inSingleQuote = !inSingleQuote
			continue
		}
		if inSingleQuote {
			continue
		}
		switch c {
		case '(':
			parenBalance++
		case ')':
			parenBalance--
			if parenBalance < 0 {
				errs = append(errs, "unmatched closing parenthesis ')' in SQL DDL")
				return ValidationResult{Valid: false, Errors: errs}
			}
		}
	}
	if inSingleQuote {
		errs = append(errs, "unterminated single-quoted string in SQL DDL")
	}
	if parenBalance != 0 {
		errs = append(errs, fmt.Sprintf("unbalanced parentheses in SQL DDL (delta=%d)", parenBalance))
	}

	upper := strings.ToUpper(trimmed)
	if !strings.Contains(upper, "CREATE TABLE") && !strings.Contains(upper, "ALTER TABLE") && !strings.Contains(upper, "CREATE INDEX") {
		if !strings.HasPrefix(trimmed, "--") {
			errs = append(errs, "SQL DDL must contain at least one CREATE/ALTER statement or header comment")
		}
	} else if !strings.Contains(trimmed, ";") {
		errs = append(errs, "SQL DDL statements must be semicolon-terminated")
	}

	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}

// ValidateOpenAPI checks that an OpenAPI 3.x YAML skeleton contains required top-level keys.
func ValidateOpenAPI(specYAML string) ValidationResult {
	var errs []string
	trimmed := strings.TrimSpace(specYAML)
	if trimmed == "" {
		return ValidationResult{Valid: false, Errors: []string{"OpenAPI specification is empty"}}
	}
	if !strings.Contains(trimmed, "openapi: 3.") {
		errs = append(errs, "OpenAPI spec must declare 'openapi: 3.x'")
	}
	for _, key := range []string{"info:", "title:", "version:", "paths:"} {
		if !strings.Contains(trimmed, key) {
			errs = append(errs, fmt.Sprintf("OpenAPI spec missing required key %q", key))
		}
	}
	if strings.Contains(trimmed, "\t") {
		errs = append(errs, "OpenAPI YAML must not contain tab characters")
	}
	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}

// ValidateMermaid checks diagram header, balanced brackets/quotes, and syntax rules.
func ValidateMermaid(diagram string) ValidationResult {
	var errs []string
	lines := strings.Split(strings.TrimSpace(diagram), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return ValidationResult{Valid: false, Errors: []string{"Mermaid diagram is empty"}}
	}

	header := strings.TrimSpace(lines[0])
	validHeaders := []string{"flowchart TD", "flowchart LR", "graph TD", "graph LR", "sequenceDiagram", "classDiagram", "erDiagram", "stateDiagram-v2"}
	headerOK := false
	for _, h := range validHeaders {
		if strings.HasPrefix(header, h) {
			headerOK = true
			break
		}
	}
	if !headerOK {
		errs = append(errs, fmt.Sprintf("unsupported Mermaid header %q", header))
	}

	for idx, line := range lines[1:] {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "%%") {
			continue
		}
		if strings.Count(l, "[") != strings.Count(l, "]") {
			errs = append(errs, fmt.Sprintf("line %d: unbalanced square brackets", idx+2))
		}
		if strings.Count(l, "\"")%2 != 0 {
			errs = append(errs, fmt.Sprintf("line %d: unterminated double quote", idx+2))
		}
	}

	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}

// ValidateCatalogue checks that a service catalogue markdown projection is non-empty and structured.
func ValidateCatalogue(md string) ValidationResult {
	var errs []string
	trimmed := strings.TrimSpace(md)
	if trimmed == "" {
		return ValidationResult{Valid: false, Errors: []string{"service catalogue is empty"}}
	}
	if !strings.HasPrefix(trimmed, "#") {
		errs = append(errs, "service catalogue must start with a Markdown heading")
	}
	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}
