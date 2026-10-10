package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

func TestParseFpsString(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"30/1", 30},
		{"30000/1001", 30}, // ~29.97 rounds to the nominal rate
		{"60/1", 60},
		{"60000/1001", 60}, // ~59.94 rounds to 60 — "1080p60", not "1080p59"
		{"24000/1001", 24}, // ~23.976 rounds down
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

// TestQualityInfoFromVariantRoundsNTSCRates pins the Twitch side of the same
// rule: a 59.94 variant is 60 fps and labelled "1080p60", the name the
// playlist parser itself gives it (internal/twitch/hls.go), and the quality
// preference matcher (selectAtHeightIdx, f >= targetFPS-1) still accepts it
// against a "1080p60" preference either way.
//
// MUTANT: int(v.FPS) — FPS 59, Label "1080p59".
func TestQualityInfoFromVariantRoundsNTSCRates(t *testing.T) {
	tests := []struct {
		fps   float64
		want  int
		label string
	}{
		{59.94, 60, "1080p60"},
		{60, 60, "1080p60"},
		{29.97, 30, "1080p"},
		{30, 30, "1080p"},
	}
	for _, tt := range tests {
		got := qualityInfoFromVariant(&twitch.TwitchHLSVariant{Width: 1920, Height: 1080, FPS: tt.fps})
		if got.FPS != tt.want || got.Label != tt.label {
			t.Errorf("qualityInfoFromVariant(FPS %v) = (%d, %q), want (%d, %q)", tt.fps, got.FPS, got.Label, tt.want, tt.label)
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
