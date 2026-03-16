package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/BurntSushi/toml"
)

// ChannelTerms represents channel filter terms, which can be either a simple
// string (regex pattern) or a map of named patterns for backwards compatibility.
type ChannelTerms struct {
	// Simple is the string value when terms is a simple regex string.
	Simple string
	// Named is the map value when terms is an object of named patterns.
	Named map[string]string
	// IsMap indicates whether terms was parsed as a map.
	IsMap bool
}

// IsEmpty returns true if no terms are configured.
func (t ChannelTerms) IsEmpty() bool {
	if t.IsMap {
		return len(t.Named) == 0
	}
	return t.Simple == ""
}

// Patterns returns all term patterns as a slice for matching.
func (t ChannelTerms) Patterns() []string {
	if t.IsMap {
		patterns := make([]string, 0, len(t.Named))
		for _, v := range t.Named {
			patterns = append(patterns, v)
		}
		return patterns
	}
	if t.Simple == "" {
		return nil
	}
	return []string{t.Simple}
}

// MarshalTOML serializes ChannelTerms back to its original TOML format.
// Simple terms are serialized as a quoted string, named terms as a TOML table.
func (ct ChannelTerms) MarshalTOML() ([]byte, error) {
	if ct.IsMap {
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(ct.Named); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	// Use the TOML encoder to properly escape backslashes, quotes, etc.
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(ct.Simple); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// MarshalJSON serializes ChannelTerms as a JSON string (simple) or object (named).
// This ensures the Web UI receives terms in a natural format rather than
// the internal struct layout.
func (ct ChannelTerms) MarshalJSON() ([]byte, error) {
	if ct.IsMap {
		return json.Marshal(ct.Named)
	}
	return json.Marshal(ct.Simple)
}

// UnmarshalJSON deserializes ChannelTerms from a JSON string or object.
// Accepts both "regex" (simple) and {"key": "regex"} (named) formats.
func (t *ChannelTerms) UnmarshalJSON(data []byte) error {
	// Try string first (simple terms)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		t.Simple = s
		t.IsMap = false
		return nil
	}
	// Try object (named terms)
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		t.Named = m
		t.IsMap = true
		return nil
	}
	return fmt.Errorf("ChannelTerms must be a string or object, got: %s", string(data))
}

// UnmarshalTOML implements the TOML unmarshaler interface.
func (t *ChannelTerms) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		t.Simple = v
		t.IsMap = false
	case map[string]any:
		t.Named = make(map[string]string, len(v))
		t.IsMap = true
		for k, val := range v {
			t.Named[k] = fmt.Sprintf("%v", val)
		}
	default:
		return fmt.Errorf("unsupported type for ChannelTerms: %T", data)
	}
	return nil
}
