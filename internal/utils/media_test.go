package utils

import "testing"

func TestExtractMediaID(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		expectedNil      bool
		expectedPlatform string
		expectedVideoID  string        // only checked for YouTube
		expectedTwitch   *TwitchTarget // only checked for Twitch
	}{
		// YouTube URLs
		{
			name:             "YouTube watch URL",
			input:            "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			expectedPlatform: "youtube",
			expectedVideoID:  "dQw4w9WgXcQ",
		},
		{
			name:             "YouTube short URL",
			input:            "https://youtu.be/dQw4w9WgXcQ",
			expectedPlatform: "youtube",
			expectedVideoID:  "dQw4w9WgXcQ",
		},
		{
			name:             "YouTube live URL",
			input:            "https://www.youtube.com/live/dQw4w9WgXcQ",
			expectedPlatform: "youtube",
			expectedVideoID:  "dQw4w9WgXcQ",
		},
		{
			name:             "bare YouTube video ID",
			input:            "dQw4w9WgXcQ",
			expectedPlatform: "youtube",
			expectedVideoID:  "dQw4w9WgXcQ",
		},

		// Explicit Twitch URLs (checked before YouTube)
		{
			name:             "Twitch channel URL",
			input:            "https://twitch.tv/shroud",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:             "Twitch VOD URL",
			input:            "https://twitch.tv/videos/1234567890",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchVOD, Value: "1234567890"},
		},
		{
			name:             "clips.twitch.tv URL",
			input:            "https://clips.twitch.tv/AwesomeClipSlug",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchClip, Value: "AwesomeClipSlug"},
		},

		// Bare login fallback to Twitch (not a valid 11-char YouTube ID)
		{
			name:             "bare login falls back to Twitch",
			input:            "shroud",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},

		// 11-char v-prefixed string matches YouTube ID regex first
		{
			name:             "v-prefixed 11-char matches YouTube ID",
			input:            "v1234567890",
			expectedPlatform: "youtube",
			expectedVideoID:  "v1234567890",
		},
		// Shorter v-prefix VOD falls to Twitch (not 11 chars)
		{
			name:             "v-prefixed short VOD falls to Twitch",
			input:            "v123456789",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchVOD, Value: "123456789"},
		},

		// Bare numeric VOD falls to Twitch
		{
			name:             "bare numeric VOD falls to Twitch",
			input:            "1234567890",
			expectedPlatform: "twitch",
			expectedTwitch:   &TwitchTarget{Type: TwitchVOD, Value: "1234567890"},
		},

		// Empty and invalid
		{
			name:        "empty string",
			input:       "",
			expectedNil: true,
		},
		{
			name:        "whitespace only",
			input:       "   ",
			expectedNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractMediaID(tt.input)
			if tt.expectedNil {
				if result != nil {
					t.Errorf("ExtractMediaID(%q) = {Platform:%q}, want nil", tt.input, result.Platform)
				}
				return
			}
			if result == nil {
				t.Errorf("ExtractMediaID(%q) = nil, want non-nil", tt.input)
				return
			}
			if result.Platform != tt.expectedPlatform {
				t.Errorf("ExtractMediaID(%q).Platform = %q, want %q", tt.input, result.Platform, tt.expectedPlatform)
			}
			if tt.expectedPlatform == "youtube" {
				if result.VideoID != tt.expectedVideoID {
					t.Errorf("ExtractMediaID(%q).VideoID = %q, want %q", tt.input, result.VideoID, tt.expectedVideoID)
				}
			}
			if tt.expectedPlatform == "twitch" {
				if result.Twitch == nil {
					t.Errorf("ExtractMediaID(%q).Twitch = nil, want non-nil", tt.input)
					return
				}
				if result.Twitch.Type != tt.expectedTwitch.Type {
					t.Errorf("ExtractMediaID(%q).Twitch.Type = %q, want %q", tt.input, result.Twitch.Type, tt.expectedTwitch.Type)
				}
				if result.Twitch.Value != tt.expectedTwitch.Value {
					t.Errorf("ExtractMediaID(%q).Twitch.Value = %q, want %q", tt.input, result.Twitch.Value, tt.expectedTwitch.Value)
				}
			}
		})
	}
}
