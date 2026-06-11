package routes

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"under limit unchanged", "hello", 10, "hello"},
		{"exact limit unchanged", "hello", 5, "hello"},
		{"ascii cut at limit", "hello world", 5, "hello"},
		{"multibyte not split", "日本語タイトル", 7, "日本"}, // 3 bytes per rune; byte 7 lands mid-rune
		{"cut lands on rune boundary", "日本語", 6, "日本"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is invalid UTF-8: %q", got)
			}
			if len(got) > tc.maxBytes {
				t.Errorf("result %d bytes exceeds max %d", len(got), tc.maxBytes)
			}
		})
	}
}

func TestDecodeImportHeader(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain title", "plain title"},
		{"%E6%97%A5%E6%9C%AC%E8%AA%9E", "日本語"}, // frontend encodeURIComponent output
		{"100%25 legit", "100% legit"},         // valid encoding decodes
		{"50% off", "50% off"},                 // malformed encoding falls back to raw
		{strings.Repeat("a", 3), "aaa"},        // no % → untouched fast path
	}
	for _, tc := range cases {
		if got := decodeImportHeader(tc.in); got != tc.want {
			t.Errorf("decodeImportHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
