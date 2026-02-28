package twitch

import (
	"reflect"
	"testing"
)

func TestParseIRCTags(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect map[string]string
	}{
		{
			name:  "key value pairs",
			input: "color=#FF0000;display-name=TestUser;emotes=;subscriber=1",
			expect: map[string]string{
				"color":        "#FF0000",
				"display-name": "TestUser",
				"emotes":       "",
				"subscriber":   "1",
			},
		},
		{
			name:   "empty string",
			input:  "",
			expect: map[string]string{},
		},
		{
			name:  "single pair",
			input: "id=abc-123-def",
			expect: map[string]string{
				"id": "abc-123-def",
			},
		},
		{
			name:  "key with no value (no equals sign)",
			input: "somekey",
			expect: map[string]string{
				"somekey": "",
			},
		},
		{
			name:  "key with empty value",
			input: "emotes=",
			expect: map[string]string{
				"emotes": "",
			},
		},
		{
			name:  "escaped characters in value",
			input: `system-msg=User\ssubscribed\sat\sTier\s1`,
			expect: map[string]string{
				"system-msg": `User\ssubscribed\sat\sTier\s1`,
			},
		},
		{
			name:  "mixed keys with and without values",
			input: "turbo;color=#1E90FF;user-id=12345",
			expect: map[string]string{
				"turbo":   "",
				"color":   "#1E90FF",
				"user-id": "12345",
			},
		},
		{
			name:  "tmi-sent-ts timestamp",
			input: "tmi-sent-ts=1678900000000;id=msg-123",
			expect: map[string]string{
				"tmi-sent-ts": "1678900000000",
				"id":          "msg-123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIRCTags(tt.input)
			if len(got) != len(tt.expect) {
				t.Errorf("parseIRCTags(%q) returned %d entries, expected %d", tt.input, len(got), len(tt.expect))
				t.Errorf("  got:    %v", got)
				t.Errorf("  expect: %v", tt.expect)
				return
			}
			for k, v := range tt.expect {
				if got[k] != v {
					t.Errorf("parseIRCTags(%q)[%q] = %q, expected %q", tt.input, k, got[k], v)
				}
			}
		})
	}
}

func TestParseBadges(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{
			name:   "multiple badges",
			input:  "subscriber/12,moderator/1",
			expect: []string{"subscriber/12", "moderator/1"},
		},
		{
			name:   "empty string returns nil",
			input:  "",
			expect: nil,
		},
		{
			name:   "single badge",
			input:  "broadcaster/1",
			expect: []string{"broadcaster/1"},
		},
		{
			name:   "three badges",
			input:  "subscriber/24,moderator/1,turbo/1",
			expect: []string{"subscriber/24", "moderator/1", "turbo/1"},
		},
		{
			name:   "vip badge",
			input:  "vip/1",
			expect: []string{"vip/1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBadges(tt.input)
			if tt.expect == nil {
				if got != nil {
					t.Errorf("parseBadges(%q) = %v, expected nil", tt.input, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.expect) {
				t.Errorf("parseBadges(%q) = %v, expected %v", tt.input, got, tt.expect)
			}
		})
	}
}

func TestParseEmoteTags(t *testing.T) {
	tests := []struct {
		name      string
		emotesStr string
		message   string
		expect    []TwitchEmoteRef
	}{
		{
			name:      "empty emotes string returns nil",
			emotesStr: "",
			message:   "hello world",
			expect:    nil,
		},
		{
			name:      "single emote",
			emotesStr: "25:0-4",
			message:   "Kappa hello",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
			},
		},
		{
			name:      "single emote multiple positions",
			emotesStr: "25:0-4,12-16",
			message:   "Kappa hello Kappa",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
				{ID: "25", Name: "Kappa", Start: 12, End: 16},
			},
		},
		{
			name:      "multiple different emotes",
			emotesStr: "25:0-4/1902:6-10",
			message:   "Kappa Keepo",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
				{ID: "1902", Name: "Keepo", Start: 6, End: 10},
			},
		},
		{
			name:      "invalid position format skipped",
			emotesStr: "25:invalid",
			message:   "Kappa",
			expect:    nil,
		},
		{
			name:      "no colon separator skipped",
			emotesStr: "25",
			message:   "Kappa",
			expect:    nil,
		},
		{
			name:      "emote at end of message",
			emotesStr: "25:6-10",
			message:   "hello Kappa",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 6, End: 10},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEmoteTags(tt.emotesStr, tt.message)
			if tt.expect == nil {
				if got != nil {
					t.Errorf("parseEmoteTags(%q, %q) = %v, expected nil", tt.emotesStr, tt.message, got)
				}
				return
			}
			if len(got) != len(tt.expect) {
				t.Errorf("parseEmoteTags(%q, %q) returned %d refs, expected %d", tt.emotesStr, tt.message, len(got), len(tt.expect))
				return
			}
			for i, ref := range got {
				exp := tt.expect[i]
				if ref.ID != exp.ID || ref.Name != exp.Name || ref.Start != exp.Start || ref.End != exp.End {
					t.Errorf("parseEmoteTags(%q, %q)[%d] = {ID:%q Name:%q Start:%d End:%d}, expected {ID:%q Name:%q Start:%d End:%d}",
						tt.emotesStr, tt.message, i,
						ref.ID, ref.Name, ref.Start, ref.End,
						exp.ID, exp.Name, exp.Start, exp.End)
				}
			}
		})
	}
}
