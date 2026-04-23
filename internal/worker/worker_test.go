package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func _removed_TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"normal_filename", "normal_filename"},
		{"file/with\\slashes", "file_with_slashes"},
		{"file:with*special?chars", "file_with_special_chars"},
		{"file\"with<angle>brackets|pipe", "file_with_angle_brackets_pipe"},
		{"", ""},
		// Truncation at 200 runes
		{string(make([]byte, 250)), string(make([]byte, 200))},
	}

	for _, tt := range tests {
		result := sanitizeFilename(tt.input)
		if result != tt.expected {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestSanitizeFilename_UTF8Truncation(t *testing.T) {
	// Build a string of 201 multi-byte runes to verify rune-safe truncation
	runes := make([]rune, 201)
	for i := range runes {
		runes[i] = '\u4e16' // CJK character (3 bytes in UTF-8)
	}
	input := string(runes)
	result := sanitizeFilename(input)
	resultRunes := []rune(result)
	if len(resultRunes) != 200 {
		t.Errorf("expected 200 runes, got %d", len(resultRunes))
	}
}

func TestIsTerminalStatus(t *testing.T) {
	tests := []struct {
		status   database.JobStatus
		expected bool
	}{
		{database.StatusFinished, true},
		{database.StatusError, true},
		{database.StatusCancelled, true},
		{database.StatusUpcoming, false},
		{database.StatusLive, false},
		{database.StatusDownloading, false},
		{database.StatusMuxing, false},
		{database.StatusCookies, false},
	}

	for _, tt := range tests {
		result := isTerminalStatus(tt.status)
		if result != tt.expected {
			t.Errorf("isTerminalStatus(%q) = %v, want %v", tt.status, result, tt.expected)
		}
	}
}
