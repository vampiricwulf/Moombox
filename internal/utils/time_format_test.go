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
	tests := []struct {
		bytesPerSec float64
		expected    string
	}{
		{0, "0 B/s"},
		{-1, "0 B/s"},
		{-999, "0 B/s"},
		{500, "500 B/s"},
		{1024, "1.0 KB/s"},
		{1048576, "1.0 MB/s"},
		{1572864, "1.5 MB/s"},
		{1073741824, "1.00 GB/s"},
		{0.5, "0 B/s"}, // float64 < 1 truncates to 0 bytes via int64 cast
	}

	for _, tt := range tests {
		result := FormatSpeed(tt.bytesPerSec)
		if result != tt.expected {
			t.Errorf("FormatSpeed(%v) = %q, want %q", tt.bytesPerSec, result, tt.expected)
		}
	}
}

func TestFormatETA(t *testing.T) {
	tests := []struct {
		seconds  float64
		expected string
	}{
		{0, ""},
		{-1, ""},
		{-100, ""},
		{1, "1s"},
		{59, "59s"},
		{60, "1m 0s"},
		{312, "5m 12s"},
		{3600, "1h 0m"},
		{3661, "1h 1m"},
		{86400, "24h 0m"}, // exactly 24h — boundary (> 86400, not >=)
		{86401, ""},       // over 24h
		{100000, ""},    // way over 24h
		{86399, "23h 59m"}, // just under 24h
		{0.5, "0s"},    // fractional second truncates to 0ms via int64
	}

	for _, tt := range tests {
		result := FormatETA(tt.seconds)
		if result != tt.expected {
			t.Errorf("FormatETA(%v) = %q, want %q", tt.seconds, result, tt.expected)
		}
	}
}
