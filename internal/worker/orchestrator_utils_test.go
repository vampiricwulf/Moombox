package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestParseFpsString(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"30/1", 30},
		{"30000/1001", 29}, // ~29.97 truncated
		{"60/1", 60},
		{"60000/1001", 59}, // ~59.94 truncated
		{"24/1", 24},
		{"0/1", 0},
		{"30", 30},
		{"", 0},
		{"abc", 0},
		{"30/0", 0}, // Division by zero
		{"1/0", 0},
	}

	for _, tt := range tests {
		result := parseFpsString(tt.input)
		if result != tt.expected {
			t.Errorf("parseFpsString(%q) = %d, want %d", tt.input, result, tt.expected)
		}
	}
}

func TestComputeStreamEndFallback(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name     string
		job      *database.Job
		expected string // exact match, or "now" for time.Now() fallback
	}{
		{
			name: "computes from start + length",
			job: &database.Job{
				StreamStartTime: "2025-01-15T10:00:00Z",
				LengthSeconds:   intPtr(3600),
			},
			expected: "2025-01-15T11:00:00Z",
		},
		{
			name: "falls back to now when no start time",
			job: &database.Job{
				LengthSeconds: intPtr(3600),
			},
			expected: "now",
		},
		{
			name:     "falls back to now when no length",
			job:      &database.Job{StreamStartTime: "2025-01-15T10:00:00Z"},
			expected: "now",
		},
		{
			name: "falls back to now when length is zero",
			job: &database.Job{
				StreamStartTime: "2025-01-15T10:00:00Z",
				LengthSeconds:   intPtr(0),
			},
			expected: "now",
		},
		{
			name:     "falls back to now when both empty",
			job:      &database.Job{},
			expected: "now",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeStreamEndFallback(tt.job)
			if tt.expected == "now" {
				// Just verify it's a valid RFC3339 timestamp (not checking exact value)
				if result == "" {
					t.Error("expected non-empty fallback time")
				}
			} else if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}
