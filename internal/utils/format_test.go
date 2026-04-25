package utils

import (
	"testing"
	"time"
)

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0B"},
		{500, "500B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1572864, "1.5MB"},
		{1073741824, "1.0GB"},
		{1610612736, "1.5GB"},
	}

	for _, tt := range tests {
		result := FormatFileSize(tt.bytes)
		if result != tt.expected {
			t.Errorf("FormatFileSize(%d) = %q, want %q", tt.bytes, result, tt.expected)
		}
	}
}

func TestFormatDurationHuman(t *testing.T) {
	tests := []struct {
		d        time.Duration
		expected string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{1 * time.Second, "1s"},
		{45 * time.Second, "45s"},
		{60 * time.Second, "1m 0s"},
		{5*time.Minute + 12*time.Second, "5m 12s"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
		{24 * time.Hour, "24h 0m"},
	}

	for _, tt := range tests {
		result := FormatDurationHuman(tt.d)
		if result != tt.expected {
			t.Errorf("FormatDurationHuman(%v) = %q, want %q", tt.d, result, tt.expected)
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
