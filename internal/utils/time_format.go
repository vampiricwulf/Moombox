package utils

import "fmt"

// FormatSpeed formats a speed in bytes/sec to a human-readable string.
func FormatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "0 B/s"
	}

	return formatBytesForSpeed(int64(bytesPerSec)) + "/s"
}

// formatBytesForSpeed is the internal byte formatter used by FormatSpeed.
// Kept private — if you need a human-readable byte formatter elsewhere,
// use FormatFileSize in format.go instead.
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
