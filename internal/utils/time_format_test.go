package utils

import "testing"

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		ms       int64
		expected string
	}{
		{0, "0s"},
		{1000, "1s"},
		{45000, "45s"},
		{60000, "1m 0s"},
		{312000, "5m 12s"},
		{8100000, "2h 15m"},
		{-1, "0s"},
	}

	for _, tt := range tests {
		result := FormatDuration(tt.ms)
		if result != tt.expected {
			t.Errorf("FormatDuration(%d) = %q, want %q", tt.ms, result, tt.expected)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1536, "1.5 KB"},
		{2411724, "2.3 MB"},
		{1556925645, "1.45 GB"},
		{-1, "0 B"},
	}

	for _, tt := range tests {
		result := FormatBytes(tt.bytes)
		if result != tt.expected {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.bytes, result, tt.expected)
		}
	}
}

func TestFormatSpeed(t *testing.T) {
	result := FormatSpeed(1048576)
	if result != "1.0 MB/s" {
		t.Errorf("FormatSpeed(1048576) = %q, want %q", result, "1.0 MB/s")
	}

	result = FormatSpeed(0)
	if result != "0 B/s" {
		t.Errorf("FormatSpeed(0) = %q, want %q", result, "0 B/s")
	}
}

func TestFormatETA(t *testing.T) {
	result := FormatETA(312)
	if result != "5m 12s" {
		t.Errorf("FormatETA(312) = %q, want %q", result, "5m 12s")
	}

	result = FormatETA(0)
	if result != "" {
		t.Errorf("FormatETA(0) = %q, want empty", result)
	}
}
