package utils

import (
	"strings"
	"testing"
)

func TestSanitizeForFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal filename unchanged",
			input:    "my_video_2024",
			expected: "my_video_2024",
		},
		{
			name:     "invalid chars replaced with underscore",
			input:    `file<name>with:bad"/chars\and|more?stars*`,
			expected: "file_name_with_bad__chars_and_more_stars_",
		},
		{
			name:     "control chars stripped",
			input:    "hello\x00world\x1f\x7ftest",
			expected: "helloworldtest",
		},
		{
			name:     "whitespace collapsed and trimmed",
			input:    "  hello    world   ",
			expected: "hello world",
		},
		{
			name:     "tabs and newlines stripped as control chars",
			input:    "hello\t\tworld\n\ntest",
			expected: "helloworldtest",
		},
		{
			name:     "empty string becomes untitled",
			input:    "",
			expected: "untitled",
		},
		{
			name:     "only invalid chars replaced with underscores",
			input:    `<>:"/\|?*`,
			expected: "_________",
		},
		{
			name:     "unicode preserved",
			input:    "日本語のファイル名",
			expected: "日本語のファイル名",
		},
		{
			name:     "mixed unicode and invalid chars",
			input:    "カフェ: 素敵な<場所>",
			expected: "カフェ_ 素敵な_場所_",
		},
		{
			name:     "truncation at 200 characters",
			input:    strings.Repeat("a", 250),
			expected: strings.Repeat("a", 200),
		},
		{
			name:     "only whitespace becomes untitled",
			input:    "   \t\n   ",
			expected: "untitled",
		},
		{
			name:     "trailing dots stripped (Win32 strips them on create)",
			input:    "Stream ending...",
			expected: "Stream ending",
		},
		{
			name:     "trailing dot-space mixture stripped",
			input:    "name. .",
			expected: "name",
		},
		{
			name:     "only dots becomes untitled",
			input:    "...",
			expected: "untitled",
		},
		{
			name:     "reserved name exposed by trailing dot is prefixed",
			input:    "con.",
			expected: "_con",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeForFilename(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeForFilename(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSanitizeForFilename_WindowsReservedNames(t *testing.T) {
	reserved := []string{
		"CON", "PRN", "AUX", "NUL",
		"COM0", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT0", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"con", "prn", "aux", "nul", // lowercase variants
		"CONIN$", "CONOUT$",
	}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			got := SanitizeForFilename(name)
			if got == name || got == strings.ToLower(name) {
				t.Errorf("SanitizeForFilename(%q) = %q, should have been prefixed", name, got)
			}
			if !strings.HasPrefix(got, "_") {
				t.Errorf("SanitizeForFilename(%q) = %q, expected underscore prefix", name, got)
			}
		})
	}

	// Reserved name with extension should also be guarded
	got := SanitizeForFilename("con.txt")
	if !strings.HasPrefix(got, "_") {
		t.Errorf("SanitizeForFilename(%q) = %q, expected underscore prefix for reserved+ext", "con.txt", got)
	}

	// Non-reserved names should be untouched
	for _, safe := range []string{"config", "console", "printer", "null_value"} {
		got := SanitizeForFilename(safe)
		if strings.HasPrefix(got, "_") {
			t.Errorf("SanitizeForFilename(%q) = %q, should not be prefixed", safe, got)
		}
	}
}
