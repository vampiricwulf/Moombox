package utils

import "testing"

func TestNormalizeText(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello World", "hello world"},
		{"café", "cafe"},
		{"  multiple   spaces  ", "multiple spaces"},
		{"über cool", "uber cool"},
		{"Ñoño", "nono"},
		{"", ""},
	}

	for _, tt := range tests {
		result := NormalizeText(tt.input)
		if result != tt.expected {
			t.Errorf("NormalizeText(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		haystack string
		needle   string
		expected bool
	}{
		{"Hello World", "hello", true},
		{"café latte", "cafe", true},
		{"Test Stream", "stream", true},
		{"Test Stream", "missing", false},
		{"über cool stream", "uber", true},
	}

	for _, tt := range tests {
		result := FuzzyMatch(tt.haystack, tt.needle)
		if result != tt.expected {
			t.Errorf("FuzzyMatch(%q, %q) = %v, want %v", tt.haystack, tt.needle, result, tt.expected)
		}
	}
}
