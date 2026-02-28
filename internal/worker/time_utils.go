package worker

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseTimeToSeconds parses a time string to seconds.
// Accepts: "120" (seconds), "1:30:00" (HH:MM:SS), "5:30" (MM:SS).
func ParseTimeToSeconds(input string) (float64, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, fmt.Errorf("empty input")
	}

	// Try plain number (seconds)
	if !strings.Contains(input, ":") {
		val, err := strconv.ParseFloat(input, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid time: %q", input)
		}
		return val, nil
	}

	// Split by ":"
	parts := strings.Split(input, ":")
	switch len(parts) {
	case 2: // MM:SS
		minutes, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid minutes: %q", parts[0])
		}
		seconds, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid seconds: %q", parts[1])
		}
		return minutes*60 + seconds, nil

	case 3: // HH:MM:SS
		hours, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid hours: %q", parts[0])
		}
		minutes, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid minutes: %q", parts[1])
		}
		seconds, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid seconds: %q", parts[2])
		}
		return hours*3600 + minutes*60 + seconds, nil

	default:
		return 0, fmt.Errorf("invalid time format: %q", input)
	}
}

// FormatSecondsToTimestamp formats seconds to "H:MM:SS" or "MM:SS".
func FormatSecondsToTimestamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	totalSecs := int(seconds)
	h := totalSecs / 3600
	m := (totalSecs % 3600) / 60
	s := totalSecs % 60

	frac := seconds - float64(totalSecs)

	if h > 0 {
		if frac > 0.001 {
			return fmt.Sprintf("%d:%02d:%02d.%03d", h, m, s, int(frac*1000))
		}
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	if frac > 0.001 {
		return fmt.Sprintf("%d:%02d.%03d", m, s, int(frac*1000))
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
