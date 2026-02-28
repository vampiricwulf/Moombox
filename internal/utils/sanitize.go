package utils

import (
	"regexp"
	"strings"
)

var (
	// controlChars matches control characters
	controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	// invalidFSChars matches characters invalid on most filesystems
	invalidFSChars = regexp.MustCompile(`[<>:"/\\|?*]`)
	// multiSpaces matches multiple consecutive spaces
	multiSpaces = regexp.MustCompile(`\s+`)
)

const maxFilenameLength = 200

// SanitizeForFilename removes characters that are invalid in filenames.
func SanitizeForFilename(name string) string {
	result := controlChars.ReplaceAllString(name, "")
	result = invalidFSChars.ReplaceAllString(result, "_")
	result = multiSpaces.ReplaceAllString(result, " ")
	result = strings.TrimSpace(result)

	if len(result) > maxFilenameLength {
		result = result[:maxFilenameLength]
	}

	if result == "" {
		result = "untitled"
	}

	return result
}
