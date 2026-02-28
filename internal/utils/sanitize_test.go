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
