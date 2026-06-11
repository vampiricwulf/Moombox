package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

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
// Simple terms are serialized as a quoted string, named terms as an INLINE
// table. The inline form is required: this marshaler's output lands at the
// value position (`terms = <bytes>`), so the multi-line `k = "v"` document a
// plain map encode produces would corrupt the saved config into
// `terms = k = "v"` — unparseable TOML that prevents the next startup.
func (ct ChannelTerms) MarshalTOML() ([]byte, error) {
	if ct.IsMap {
		// Sorted keys for deterministic config files across saves.
		keys := slices.Sorted(maps.Keys(ct.Named))
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(", ")
			}
			keyBytes, err := encodeTOMLString(k)
			if err != nil {
				return nil, err
			}
			valBytes, err := encodeTOMLString(ct.Named[k])
			if err != nil {
				return nil, err
			}
			buf.Write(keyBytes)
			buf.WriteString(" = ")
			buf.Write(valBytes)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	}
	return encodeTOMLString(ct.Simple)
}

// encodeTOMLString renders s as a quoted TOML string with proper escaping
// (backslashes, quotes, control chars). Trims only the trailing newline the
// encoder appends — bytes.TrimSpace would also strip meaningful
// leading/trailing space inside a quoted string the user configured on
// purpose.
func encodeTOMLString(s string) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
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
