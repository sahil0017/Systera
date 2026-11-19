package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Schema is a validated subset of JSON Schema (draft 2020-12) sufficient for tool
// arguments: type, properties, required, additionalProperties (bool), items, enum,
// minLength/maxLength, minimum/maximum, minItems/maxItems.
type Schema struct {
	Type                 string             `json:"type"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Description          string             `json:"description,omitempty"`
}

var knownTypes = map[string]bool{"object": true, "array": true, "string": true, "integer": true, "number": true, "boolean": true, "null": true}

// ParseSchema decodes a schema and rejects keywords outside the supported subset, so a
// schema can never silently validate less than its author intended.
func ParseSchema(raw []byte) (*Schema, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Schema
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	if err := s.check("$"); err != nil {
		return nil, err
	}
	return &s, nil
}

// MustSchema is ParseSchema for compile-time constant schemas.
func MustSchema(raw string) *Schema {
	s, err := ParseSchema([]byte(raw))
	if err != nil {
		panic(err)
	}
	return s
}

func (s *Schema) check(path string) error {
	if !knownTypes[s.Type] {
		return fmt.Errorf("schema %s: unsupported or missing type %q", path, s.Type)
	}
	if s.Type == "object" && s.AdditionalProperties == nil {
		f := false
		s.AdditionalProperties = &f // closed by default: unexpected arguments are rejected
	}
	for name, p := range s.Properties {
		if p == nil {
			return fmt.Errorf("schema %s.%s: empty", path, name)
		}
		if err := p.check(path + "." + name); err != nil {
			return err
		}
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			return fmt.Errorf("schema %s: required property %q is not declared", path, r)
		}
	}
	if s.Items != nil {
		if err := s.Items.check(path + "[]"); err != nil {
			return err
		}
	}
	return nil
}

// ValidationError lists every violation found.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Validate checks a decoded JSON value (decoded with UseNumber) against the schema.
func (s *Schema) Validate(v any) error {
	var problems []string
	s.validate("$", v, &problems)
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func (s *Schema) validate(path string, v any, out *[]string) {
	bad := func(format string, args ...any) { *out = append(*out, path+": "+fmt.Sprintf(format, args...)) }

	if len(s.Enum) > 0 {
		match := false
		for _, e := range s.Enum {
			if jsonEqual(e, v) {
				match = true
				break
			}
		}
		if !match {
			bad("must be one of %v", s.Enum)
			return
		}
	}

	switch s.Type {
	case "null":
		if v != nil {
			bad("must be null")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			bad("must be a boolean")
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			bad("must be a string")
			return
		}
		n := utf8.RuneCountInString(str)
		if s.MinLength != nil && n < *s.MinLength {
			bad("must be at least %d characters", *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			bad("must be at most %d characters", *s.MaxLength)
		}
	case "integer", "number":
		num, ok := v.(json.Number)
		if !ok {
			bad("must be a %s", s.Type)
			return
		}
		f, err := num.Float64()
		if err != nil {
			bad("invalid number")
			return
		}
		if s.Type == "integer" {
			if _, err := num.Int64(); err != nil {
				bad("must be an integer")
				return
			}
		}
		if s.Minimum != nil && f < *s.Minimum {
			bad("must be >= %v", *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			bad("must be <= %v", *s.Maximum)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			bad("must be an array")
			return
		}
		if s.MinItems != nil && len(arr) < *s.MinItems {
			bad("must have at least %d items", *s.MinItems)
		}
		if s.MaxItems != nil && len(arr) > *s.MaxItems {
			bad("must have at most %d items", *s.MaxItems)
		}
		if s.Items != nil {
			for i, item := range arr {
				s.Items.validate(fmt.Sprintf("%s[%d]", path, i), item, out)
			}
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			bad("must be an object")
			return
		}
		for _, r := range s.Required {
			if _, ok := obj[r]; !ok {
				bad("missing required property %q", r)
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := s.Properties[k]; ok {
				p.validate(path+"."+k, obj[k], out)
			} else if s.AdditionalProperties != nil && !*s.AdditionalProperties {
				bad("unexpected property %q", k)
			}
		}
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}
