package config

import (
	"fmt"
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

// UnmarshalTOML implements the TOML unmarshaler interface.
func (t *ChannelTerms) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case string:
		t.Simple = v
		t.IsMap = false
	case map[string]interface{}:
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
