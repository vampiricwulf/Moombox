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
		{1099511627776, "1.0TB"},
		{1649267441664, "1.5TB"},
		{2199023255552, "2.0TB"},
	}

	for _, tt := range tests {
		result := FormatFileSize(tt.bytes)
		if result != tt.expected {
			t.Errorf("FormatFileSize(%d) = %q, want %q", tt.bytes, result, tt.expected)
		}
	}
}

// TestFormatFileSizeRoundsTiesLikeTheWeb: a size of exactly N.25 of a unit is
// a tie at one decimal, and the Web's formatBytes (toFixed(1)) rounds it up
// where %.1f rounded it to even — the TUI read "1.2GB" beside the dashboard's
// "1.3GB". The wants are what formatBytes in web/public/modules/utils.js
// prints for the same inputs; N.75 ties and non-ties already agreed.
//
// Mutant: toFixed1 formatting v with %.1f (round half to even) — every .25
// tie reads one tenth low.
func TestFormatFileSizeRoundsTiesLikeTheWeb(t *testing.T) {
	for in, want := range map[int64]string{
		1280:                "1.3KB", // 1.25 KiB
		5*1024 + 256:        "5.3KB",
		2*1024*1024 + 1<<18: "2.3MB",
		1342177280:          "1.3GB", // 1.25 GiB
		5 << 40 / 4:         "1.3TB",
		1792:                "1.8KB", // 1.75: both rules round up
		1288:                "1.3KB", // 1.2578…, not a tie
		1331:                "1.3KB", // 1.2998…
		1023 * 1024:         "1023.0KB",
		1024*1024 - 1:       "1024.0KB", // rounds up within its tier, as the Web does
	} {
		if got := FormatFileSize(in); got != want {
			t.Errorf("FormatFileSize(%d) = %q, the Web's formatBytes prints %q", in, got, want)
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
