package utils

import (
	"math"
	"testing"
)

func TestParseFrameRate(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"30/1", 30.0},
		{"30000/1001", 29.97002997002997},
		{"60/1", 60.0},
		{"24000/1001", 23.976023976023978},
		{"0/1", 0.0},
		{"25", 25.0},
		{"", 0.0},
		{"invalid", 0.0},
		{"30/0", 0.0}, // zero denominator
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseFrameRate(tt.input)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("parseFrameRate(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
