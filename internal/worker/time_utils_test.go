package worker

import (
	"testing"
)

func TestParseTimeToSeconds(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
		err      bool
	}{
		{"120", 120, false},
		{"0", 0, false},
		{"5:30", 330, false},
		{"1:30:00", 5400, false},
		{"0:05", 5, false},
		{"2:00:30", 7230, false},
		{"", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		result, err := ParseTimeToSeconds(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("ParseTimeToSeconds(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeToSeconds(%q): %v", tt.input, err)
			continue
		}
		if result != tt.expected {
			t.Errorf("ParseTimeToSeconds(%q) = %f, want %f", tt.input, result, tt.expected)
		}
	}
}

func TestParseTimeToSeconds_EdgeCases(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
		err      bool
	}{
		{"1:2:3:4", 0, true},        // Too many colons
		{"  120  ", 120, false},     // Whitespace trimming
		{"-5", -5, false},           // Negative (accepted — caller validates)
		{"1.5", 1.5, false},         // Fractional seconds
		{"1:30.5", 90.5, false},     // Fractional MM:SS
		{"1:0:30.5", 3630.5, false}, // Fractional HH:MM:SS
	}

	for _, tt := range tests {
		result, err := ParseTimeToSeconds(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("ParseTimeToSeconds(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeToSeconds(%q): %v", tt.input, err)
			continue
		}
		if result != tt.expected {
			t.Errorf("ParseTimeToSeconds(%q) = %f, want %f", tt.input, result, tt.expected)
		}
	}
}

func TestFormatSecondsToTimestamp(t *testing.T) {
	tests := []struct {
		input    float64
		expected string
	}{
		{0, "0:00"},
		{5, "0:05"},
		{65, "1:05"},
		{3661, "1:01:01"},
		{7200, "2:00:00"},
		{-5, "0:00"},              // Negative clamped to 0
		{90.5, "1:30.500"},        // Fractional seconds
		{3661.123, "1:01:01.123"}, // Fractional with hours
	}

	for _, tt := range tests {
		result := FormatSecondsToTimestamp(tt.input)
		if result != tt.expected {
			t.Errorf("FormatSecondsToTimestamp(%f) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
