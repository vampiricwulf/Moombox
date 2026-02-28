package utils

import "fmt"

// FormatDuration formats a duration in milliseconds to a human-readable string.
func FormatDuration(ms int64) string {
	if ms < 0 {
		return "0s"
	}

	totalSeconds := ms / 1000
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

// FormatBytes formats a byte count to a human-readable string.
func FormatBytes(bytes int64) string {
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

// FormatSpeed formats a speed in bytes/sec to a human-readable string.
func FormatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "0 B/s"
	}

	return FormatBytes(int64(bytesPerSec)) + "/s"
}

// FormatETA formats an estimated time of arrival in seconds.
func FormatETA(seconds float64) string {
	if seconds <= 0 || seconds > 86400 {
		return ""
	}
	return FormatDuration(int64(seconds * 1000))
}
