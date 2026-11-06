package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		{
			name:     "clips.twitch.tv extra path segments take first segment only",
			input:    "https://clips.twitch.tv/SomeClipSlug/edit",
			expected: &TwitchTarget{Type: TwitchClip, Value: "SomeClipSlug"},
		},
		{
			name:     "VOD URL with non-numeric id returns nil",
			input:    "https://twitch.tv/videos/abc",
			expected: nil,
		},
		{
			name:     "VOD URL with short legacy id accepted",
			input:    "https://twitch.tv/videos/123456",
			expected: &TwitchTarget{Type: TwitchVOD, Value: "123456"},
		},
		{
			name:     "channel video URL with non-numeric id returns nil",
			input:    "https://twitch.tv/shroud/video/notanid!",
			expected: nil,
		},
		{
			name:     "channel clip URL with empty slug returns nil",
			input:    "https://twitch.tv/shroud/clip/",
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
		{
			name:     "reserved path store",
			input:    "https://twitch.tv/store",
			expected: nil,
		},
		{
			name:     "reserved path tags",
			input:    "https://twitch.tv/tags",
			expected: nil,
		},
		{
			name:     "reserved path drops",
			input:    "https://twitch.tv/drops",
			expected: nil,
		},
		{
			name:     "reserved path subscriptions",
			input:    "https://twitch.tv/subscriptions",
			expected: nil,
		},

		// URL with port (hostname must strip port)
		{
			name:     "URL with port still matches channel",
			input:    "https://twitch.tv:443/shroud",
			expected: &TwitchTarget{Type: TwitchChannel, Value: "shroud"},
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

// twitchURLRow is one row of testdata/twitch_urls.json, the table the Go
// parser, the TUI and the dashboard are all tested against.
type twitchURLRow struct {
	Input string `json:"input"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

func loadTwitchURLTable(t *testing.T) []twitchURLRow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "twitch_urls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Rows []twitchURLRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) == 0 {
		t.Fatal("the shared Twitch URL table is empty")
	}
	return table.Rows
}

// TestExtractTwitchTargetSharedURLTable runs ExtractTwitchTarget over the
// shared table (G35): yt-dlp's URL forms on www, go and m hosts, /v/ and
// /video/, schedule?vodID=, player.twitch.tv's video= and channel=, clips
// with and without a channel, and the listings that must never be taken for a
// channel (a silent live-wait job instead of the refusal).
//
// Mutants: dropping the go. host, the /v/ case, the schedule?vodID= case, the
// channel-less /clip/ case, the last-segment clip slug, or the listing cases
// each fails its rows.
func TestExtractTwitchTargetSharedURLTable(t *testing.T) {
	for _, row := range loadTwitchURLTable(t) {
		t.Run(row.Input, func(t *testing.T) {
			got := ExtractTwitchTarget(row.Input)
			if row.Type == "" {
				if got != nil {
					t.Errorf("= {%s %q}, want nil", got.Type, got.Value)
				}
				return
			}
			if got == nil || string(got.Type) != row.Type || got.Value != row.Value {
				t.Errorf("= %+v, want {%s %q}", got, row.Type, row.Value)
			}
		})
	}
}

// TestTwitchTargetUnsupported pins which kinds the add paths refuse until
// their jobs exist: clip lists only. A channel, a VOD, a clip (owner decision
// D26), a channel's video list and a collection (owner decision D18, W14) are
// accepted.
//
// Mutants: the listings left refused (videos and collection fail); the clip
// refusal restored (TwitchClip fails).
func TestTwitchTargetUnsupported(t *testing.T) {
	for typ, wantRefused := range map[TwitchTargetType]bool{
		TwitchChannel: false, TwitchVOD: false, TwitchClip: false,
		TwitchVideosListing: false, TwitchClipsListing: true, TwitchCollection: false,
	} {
		if got := (&TwitchTarget{Type: typ}).Unsupported() != ""; got != wantRefused {
			t.Errorf("%s: refused = %v, want %v", typ, got, wantRefused)
		}
	}
	if (*TwitchTarget)(nil).Unsupported() != "" {
		t.Error("a nil target has no refusal")
	}
}

// TestTwitchVideosQuery reads a pasted /<ch>/videos URL's ?filter= and ?sort=,
// with or without its scheme, as yt-dlp's TwitchVideosIE parse_qs does.
//
// Mutant: the sort read from "filter" (the views row fails).
func TestTwitchVideosQuery(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://www.twitch.tv/spamfish/videos?filter=archives&sort=views": {"archives", "views"},
		"twitch.tv/spamfish/videos?filter=highlights":                      {"highlights", ""},
		"https://www.twitch.tv/spamfish/videos":                            {"", ""},
		"spamfish":                                                         {"", ""},
	} {
		if f, s := TwitchVideosQuery(in); f != want[0] || s != want[1] {
			t.Errorf("TwitchVideosQuery(%q) = (%q, %q), want %v", in, f, s, want)
		}
	}
}

// TestValidTwitchClipSlug pins the shape the add route holds a raw clip slug
// to: the URL table's (letters, digits, "_" and "-"), at most 100 bytes — the
// slug becomes the job ID and a staging directory's name.
//
// Mutant: the length cap dropped; "/" or ".." accepted.
func TestValidTwitchClipSlug(t *testing.T) {
	for slug, want := range map[string]bool{
		"FaintLightGullWholeWheat":                      true,
		"CulturedAmazingKuduDatSheffy-TiZ_-ixAGYR3y2Uy": true,
		"":                       false,
		"../etc":                 false,
		"a/b":                    false,
		strings.Repeat("a", 101): false,
	} {
		if got := ValidTwitchClipSlug(slug); got != want {
			t.Errorf("ValidTwitchClipSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}
