package utils

import "testing"

func TestLooksLikeURL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"https://www.youtube.com/watch?v=abc", true},
		{"youtube.com/@SomeChannel", true},
		{"https://youtu.be/abc123", true},
		{"https://twitch.tv/shroud", true},
		{"twitch.tv/shroud", true},
		{"shroud", false},
		{"dQw4w9WgXcQ", false},
		{"", false},
		{"   ", false},
		{"https://example.com/page", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := LooksLikeURL(tt.input)
			if got != tt.want {
				t.Errorf("LooksLikeURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
