package utils

import "testing"

func TestExtractVideoID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Bare ID
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Standard watch URLs
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://m.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// With extra params
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=30s", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLtest", "dQw4w9WgXcQ"},
		// Short URLs
		{"https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Live URLs
		{"https://www.youtube.com/live/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Shorts URLs
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Embed URLs
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// V URLs
		{"https://www.youtube.com/v/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// IDs with hyphens and underscores (must be exactly 11 chars)
		{"abc-_12-_AB", "abc-_12-_AB"},
		// Invalid inputs
		{"", ""},
		{"not-a-url", ""},
		{"https://example.com/watch?v=dQw4w9WgXcQ", ""},
		{"https://www.youtube.com/watch", ""},
		{"https://www.youtube.com/watch?v=tooshort", ""},
		{"https://www.youtube.com/watch?v=toolooooooong", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ExtractVideoID(tt.input)
			if result != tt.expected {
				t.Errorf("ExtractVideoID(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsVideoID(t *testing.T) {
	if !IsVideoID("dQw4w9WgXcQ") {
		t.Error("expected true for valid ID")
	}
	if IsVideoID("short") {
		t.Error("expected false for short string")
	}
	if IsVideoID("has spaces!!") {
		t.Error("expected false for invalid chars")
	}
}
