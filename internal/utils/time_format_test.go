package utils

import "testing"

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
