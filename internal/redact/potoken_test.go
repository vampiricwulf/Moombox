package redact

import (
	"testing"
)

// TestPoTokenURL pins the URL rule for both places a googlevideo URL carries
// the GVS PO token, and that nothing else in the URL changes.
//
// Mutants (run):
//   - PoTokenURL without its redactPotPath call: every path-form row keeps
//     its token (the W24-08 defect — the live DASH/HLS manifest URL).
//   - redactPotPath without the authority skip: "host named pot" rewrites
//     the first path segment.
//   - the query loop matching on strings.HasPrefix(p, "pot") instead of the
//     key: "potato" is rewritten.
//   - PoTokenText (the unparseable fallback) without potPathRe: the
//     "unparseable, path form" row keeps its token.
func TestPoTokenURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no token", "https://h/videoplayback?itag=1&sq=5", "https://h/videoplayback?itag=1&sq=5"},
		{"query, last", "https://h/v?itag=1&pot=SECRET", "https://h/v?itag=1&pot=<redacted>"},
		{"query, first", "https://h/v?pot=SECRET&itag=1", "https://h/v?pot=<redacted>&itag=1"},
		{"query with fragment", "https://h/v?pot=SECRET#t=1", "https://h/v?pot=<redacted>#t=1"},
		{"query, other key kept", "https://h/v?potato=1&pot=SECRET", "https://h/v?potato=1&pot=<redacted>"},
		{
			"path, live DASH manifest",
			"https://manifest.googlevideo.com/api/manifest/dash/id/abc/source/yt_live_broadcast/pot/SECRET",
			"https://manifest.googlevideo.com/api/manifest/dash/id/abc/source/yt_live_broadcast/pot/<redacted>",
		},
		{
			"path, HLS variant playlist",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/id/x/itag/95/playlist/index.m3u8/pot/SECRET",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/id/x/itag/95/playlist/index.m3u8/pot/<redacted>",
		},
		{"path, mid-URL", "https://h/a/pot/SECRET/b/c", "https://h/a/pot/<redacted>/b/c"},
		{"path and query", "https://h/a/pot/SECRET?pot=SECRET&x=1", "https://h/a/pot/<redacted>?pot=<redacted>&x=1"},
		{"path, trailing slash", "https://h/a/pot/", "https://h/a/pot/"},
		{"host named pot", "https://pot/SECRETLESS/x", "https://pot/SECRETLESS/x"},
		{"no scheme", "/api/pot/SECRET", "/api/pot/<redacted>"},
		{"already redacted", "https://h/a/pot/<redacted>?pot=<redacted>", "https://h/a/pot/<redacted>?pot=<redacted>"},
		{"unparseable, query form", "http://[::1%zz/v?pot=SECRET&a=b", "http://[::1%zz/v?pot=<redacted>&a=b"},
		{"unparseable, path form", "http://[::1%zz/v/pot/SECRET?a=b", "http://[::1%zz/v/pot/<redacted>?a=b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PoTokenURL(tc.in); got != tc.want {
				t.Fatalf("PoTokenURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPoTokenText pins the free-text rule: both forms go wherever they sit in
// an already flattened message, and text without a token is left alone.
//
// Mutants (run): dropping either regexp's ReplaceAllString fails the row of
// its form; a fast path that returns s when it lacks "pot=" fails the path
// row.
func TestPoTokenText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/id/abc/pot/SECRET": dial tcp: connection refused`,
			`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/id/abc/pot/<redacted>": dial tcp: connection refused`},
		{`download: Get "https://h/v?itag=1&pot=SECRET": EOF`, `download: Get "https://h/v?itag=1&pot=<redacted>": EOF`},
		{"no token at all", "no token at all"},
		{"spotless", "spotless"},
	}
	for _, tc := range cases {
		if got := PoTokenText(tc.in); got != tc.want {
			t.Errorf("PoTokenText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
