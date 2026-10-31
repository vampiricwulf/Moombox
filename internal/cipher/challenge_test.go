package cipher

import "testing"

// TestHasNChallengeReadsBothForms pins the n detection the worker's
// challenge-free fallback (audit G10) keys on: an n in the query, or in the
// path at RoutedDecryptNInURL's 10-character floor — the forms that function
// solves — and nothing for a URL that carries neither, such as visionos's
// format URLs and its live HLS master.
//
// Mutants: the query arm dropped (the query row); the path arm dropped (the
// path row); the path floor dropped (the short-path row).
func TestHasNChallengeReadsBothForms(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://r1.googlevideo.com/videoplayback?itag=302&n=abcdefghijkl", true},
		{"https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/abcdefghijkl/file/index.m3u8", true},
		{"https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/short/file/index.m3u8", false},
		{"https://r1.googlevideo.com/videoplayback?itag=302&sig=x", false},
		{"https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", false},
		{"", false},
	} {
		if got := HasNChallenge(tc.url); got != tc.want {
			t.Errorf("HasNChallenge(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
