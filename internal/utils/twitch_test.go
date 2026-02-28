package utils

import "testing"

func TestExtractTwitchTarget(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected *TwitchTarget
	}{
		// Channel URLs
		{
			name:     "standard channel URL",
			input:    "https://twitch.tv/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "www channel URL",
			input:    "https://www.twitch.tv/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "mobile channel URL",
			input:    "https://m.twitch.tv/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "bare twitch.tv without protocol",
			input:    "twitch.tv/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "www.twitch.tv without protocol",
			input:    "www.twitch.tv/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "channel URL with trailing slash",
			input:    "https://twitch.tv/shroud/",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "channel URL mixed case lowered",
			input:    "https://twitch.tv/Shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "bare twitch.tv root returns nil",
			input:    "https://twitch.tv/",
			expected: nil,
		},
		{
			name:     "bare twitch.tv no path returns nil",
			input:    "https://twitch.tv",
			expected: nil,
		},

		// VOD URLs
		{
			name:     "VOD URL /videos/123",
			input:    "https://twitch.tv/videos/1234567890",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "1234567890"},
		},
		{
			name:     "channel video URL /channel/video/123",
			input:    "https://twitch.tv/shroud/video/1234567890",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "1234567890"},
		},

		// Clip URLs
		{
			name:     "clips.twitch.tv/slug",
			input:    "https://clips.twitch.tv/AwesomeClipSlug",
			expected: &TwitchTarget{Type: TwitchClip, Value: "AwesomeClipSlug"},
		},
		{
			name:     "clips.twitch.tv without protocol",
			input:    "clips.twitch.tv/SomeClipSlug",
			expected: &TwitchTarget{Type: TwitchClip, Value: "SomeClipSlug"},
		},
		{
			name:     "channel clip URL /channel/clip/slug",
			input:    "https://twitch.tv/shroud/clip/AwesomeClipSlug",
			expected: &TwitchTarget{Type: TwitchClip, Value: "AwesomeClipSlug"},
		},
		{
			name:     "clips.twitch.tv bare root returns nil",
			input:    "https://clips.twitch.tv/",
			expected: nil,
		},

		// Bare identifiers
		{
			name:     "v-prefixed VOD ID",
			input:    "v1234567890",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "1234567890"},
		},
		{
			name:     "bare numeric VOD ID 7 digits",
			input:    "1234567",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "1234567"},
		},
		{
			name:     "bare numeric VOD ID 12 digits",
			input:    "123456789012",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "123456789012"},
		},
		{
			name:     "bare login name",
			input:    "shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},
		{
			name:     "bare login with underscores",
			input:    "cool_streamer_99",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "cool_streamer_99"},
		},
		{
			name:     "bare login mixed case lowered",
			input:    "Shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
		},

		// Reserved paths return nil
		{
			name:     "reserved path directory",
			input:    "https://twitch.tv/directory",
			expected: nil,
		},
		{
			name:     "reserved path settings",
			input:    "https://twitch.tv/settings",
			expected: nil,
		},
		{
			name:     "reserved path videos",
			input:    "https://twitch.tv/videos",
			expected: nil,
		},
		{
			name:     "reserved path search",
			input:    "https://twitch.tv/search",
			expected: nil,
		},

		// Empty and invalid inputs
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: nil,
		},
		{
			name:     "numeric too short for VOD (6 digits)",
			input:    "123456",
			expected: nil,
		},
		{
			name:     "starts with digit not valid login",
			input:    "9invalid",
			expected: nil,
		},
		{
			name:     "login too long (26 chars)",
			input:    "abcdefghijklmnopqrstuvwxyz",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractTwitchTarget(tt.input)
			if tt.expected == nil {
				if result != nil {
					t.Errorf("ExtractTwitchTarget(%q) = {Type:%q, Value:%q}, want nil", tt.input, result.Type, result.Value)
				}
				return
			}
			if result == nil {
				t.Errorf("ExtractTwitchTarget(%q) = nil, want {Type:%q, Value:%q}", tt.input, tt.expected.Type, tt.expected.Value)
				return
			}
			if result.Type != tt.expected.Type {
				t.Errorf("ExtractTwitchTarget(%q).Type = %q, want %q", tt.input, result.Type, tt.expected.Type)
			}
			if result.Value != tt.expected.Value {
				t.Errorf("ExtractTwitchTarget(%q).Value = %q, want %q", tt.input, result.Value, tt.expected.Value)
			}
		})
	}
}
