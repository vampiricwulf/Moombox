package worker

import (
	"testing"
)

func TestParseFpsString(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"30/1", 30},
		{"30000/1001", 29},  // ~29.97 truncated
		{"60/1", 60},
		{"60000/1001", 59},  // ~59.94 truncated
		{"24/1", 24},
		{"0/1", 0},
		{"30", 30},
		{"", 0},
		{"abc", 0},
		{"30/0", 0},         // Division by zero
		{"1/0", 0},
	}

	for _, tt := range tests {
		result := parseFpsString(tt.input)
		if result != tt.expected {
			t.Errorf("parseFpsString(%q) = %d, want %d", tt.input, result, tt.expected)
		}
	}
}
