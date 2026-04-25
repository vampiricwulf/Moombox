package utils

import (
	"fmt"
	"time"
)

// FormatFileSize formats bytes into a human-readable string (e.g. "1.5GB", "250.0MB").
func FormatFileSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1fGB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1fMB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1fKB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// FormatSpeed formats a speed in bytes/sec to a human-readable string.
// Uses two-decimal precision for TB/GB to keep network-rate displays
// stable, falling back to one-decimal for MB/KB and integer-only for
// sub-KB. The space separator and "/s" suffix are intentional.
func FormatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "0 B/s"
	}
	return formatBytesForSpeed(int64(bytesPerSec)) + "/s"
}

// formatBytesForSpeed is the internal byte formatter used by FormatSpeed.
// Kept private — if you need a human-readable byte formatter elsewhere,
// use FormatFileSize above.
func formatBytesForSpeed(bytes int64) string {
	if bytes < 0 {
		return "0 B"
	}
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// FormatDurationHuman formats a time.Duration into a human-readable string (e.g. "1h 23m", "5m 30s").
func FormatDurationHuman(d time.Duration) string {
	d = max(d, 0)
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
