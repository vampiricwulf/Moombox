package youtube

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// hlsBody is an OK live player response whose only streaming data is an HLS
// master at manifestURL.
func hlsBody(manifestURL string) string {
	return `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true, "isLiveContent": true},
	"streamingData": {"hlsManifestUrl": "` + manifestURL + `"}
}`
}

// TestCookielessHlsManifestTakesTheFirstMasterWithNoN pins the challenge-free
// live fallback's source (audit G10): the cookieless chain, visionos first,
// asked for an HLS master whose URL carries no n challenge. A master with an
// n is passed over — yt-dlp uses a manifest only `if solved_n or not
// n_challenge` (_video.py:3692-3713) — and so is a client that serves none;
// with no client left the error names the last reason.
//
// Mutants: the n check dropped (the second row takes visionos's n master);
// android_vr left out of the chain (the second row fails); the last reason
// dropped from the error (the third row).
func TestCookielessHlsManifestTakesTheFirstMasterWithNoN(t *testing.T) {
	const free = "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8"
	const withN = "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/abcdefghijkl/file/index.m3u8"
	type resp = struct {
		status int
		body   string
	}
	for _, tc := range []struct {
		name       string
		responses  map[string]resp
		wantURL    string
		wantSource string
		wantErr    string
		wantCalls  []string
	}{
		{"visionos serves one", map[string]resp{"101": {http.StatusOK, hlsBody(free)}}, free, "visionos", "", []string{"101"}},
		{"visionos's carries an n, android_vr's none", map[string]resp{"101": {http.StatusOK, hlsBody(withN)}, "28": {http.StatusOK, hlsBody(free)}}, free, "android_vr", "", []string{"101", "28"}},
		{"neither serves one", map[string]resp{"101": {http.StatusOK, hlsBody(withN)}, "28": {http.StatusOK, adequateOKBody}}, "", "", "android_vr: no HLS manifest", []string{"101", "28"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &clientKeyedTransport{responses: tc.responses}
			orig := apiClient
			apiClient = &http.Client{Transport: tr}
			t.Cleanup(func() { apiClient = orig })
			u, source, err := NewPlayerAPI(nil, noopLogger{}).CookielessHlsManifest(context.Background(), "test1234567", "vd")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("CookielessHlsManifest = %q, %q, %v; want an error naming %q", u, source, err, tc.wantErr)
				}
			} else if err != nil || u != tc.wantURL || source != tc.wantSource {
				t.Errorf("CookielessHlsManifest = %q, %q, %v; want %q from %s", u, source, err, tc.wantURL, tc.wantSource)
			}
			if strings.Join(tr.calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Errorf("clients asked %v, want %v", tr.calls, tc.wantCalls)
			}
		})
	}
}
