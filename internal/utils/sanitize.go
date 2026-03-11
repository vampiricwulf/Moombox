package utils

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// controlChars matches control characters
	controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	// invalidFSChars matches characters invalid on most filesystems
	invalidFSChars = regexp.MustCompile(`[<>:"/\\|?*]`)

	// windowsReservedNames are device names that cannot be used as filenames on Windows.
	windowsReservedNames = map[string]bool{
		"con": true, "prn": true, "aux": true, "nul": true,
		"com1": true, "com2": true, "com3": true, "com4": true,
		"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
		"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
		"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	}
)

const maxFilenameLength = 200

// SanitizeForFilename removes characters that are invalid in filenames.
func SanitizeForFilename(name string) string {
	result := controlChars.ReplaceAllString(name, "")
	result = invalidFSChars.ReplaceAllString(result, "_")
	result = strings.Join(strings.Fields(result), " ")

	if utf8.RuneCountInString(result) > maxFilenameLength {
		// Truncate at rune boundary (200 characters, not bytes)
		runes := []rune(result)
		result = string(runes[:maxFilenameLength])
	}

	result = strings.TrimSpace(result)

	if result == "" {
		result = "untitled"
	}

	// Guard against Windows reserved device names (CON, PRN, AUX, NUL, COM1-9, LPT1-9)
	base := strings.ToLower(strings.TrimSuffix(result, filepath.Ext(result)))
	if windowsReservedNames[base] {
		result = "_" + result
	}

	return result
}
