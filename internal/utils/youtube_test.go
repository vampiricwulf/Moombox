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

func TestParseYouTubeChannelURL(t *testing.T) {
	tests := []struct {
		input     string
		wantNil   bool
		wantID    string // expected ChannelID (direct extraction)
		wantPath  string // expected Path (needs resolution)
	}{
		// /channel/UCxxx → direct extraction
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://m.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		// /channel/UCxxx with trailing path
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx/videos", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},

		// /@Handle → needs resolution
		{"https://www.youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"https://www.youtube.com/@SomeHandle/videos", false, "", "/@SomeHandle"},

		// /c/CustomName → needs resolution
		{"https://www.youtube.com/c/CustomChannel", false, "", "/c/CustomChannel"},
		{"https://www.youtube.com/c/CustomChannel/live", false, "", "/c/CustomChannel"},

		// /user/Username → needs resolution
		{"https://www.youtube.com/user/SomeUser", false, "", "/user/SomeUser"},
		{"https://www.youtube.com/user/SomeUser/videos", false, "", "/user/SomeUser"},

		// Video URLs → nil (not channel URLs)
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", true, "", ""},
		{"https://www.youtube.com/live/dQw4w9WgXcQ", true, "", ""},
		{"https://youtu.be/dQw4w9WgXcQ", true, "", ""},

		// Non-YouTube URLs → nil
		{"https://example.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"https://twitch.tv/streamer", true, "", ""},

		// Bare strings → nil
		{"UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"@SomeHandle", true, "", ""},
		{"", true, "", ""},

		// Invalid channel ID format
		{"https://www.youtube.com/channel/NotAChannelID", true, "", ""},

		// Empty handle
		{"https://www.youtube.com/@", true, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ParseYouTubeChannelURL(tt.input)
			if tt.wantNil {
				if result != nil {
					t.Errorf("ParseYouTubeChannelURL(%q) = %+v, want nil", tt.input, result)
				}
				return
			}
			if result == nil {
				t.Fatalf("ParseYouTubeChannelURL(%q) = nil, want non-nil", tt.input)
			}
			if result.ChannelID != tt.wantID {
				t.Errorf("ParseYouTubeChannelURL(%q).ChannelID = %q, want %q", tt.input, result.ChannelID, tt.wantID)
			}
			if result.Path != tt.wantPath {
				t.Errorf("ParseYouTubeChannelURL(%q).Path = %q, want %q", tt.input, result.Path, tt.wantPath)
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
