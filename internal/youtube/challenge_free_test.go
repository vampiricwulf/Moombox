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

// refusedHlsBody is a player response that refuses the video — a bot wall —
// while its streaming data still names an HLS master at manifestURL.
func refusedHlsBody(manifestURL string) string {
	return `{
	"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you're not a bot"},
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
// A client that refuses the video is passed over too, whatever its answer
// carries.
//
// Mutants: the n check dropped (the second row takes visionos's n master);
// android_vr left out of the chain (the second row fails); the last reason
// dropped from the error (the third row); the playability check dropped (the
// refused row takes visionos's master).
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
		{"visionos refuses the video", map[string]resp{"101": {http.StatusOK, refusedHlsBody(free + "?refused")}, "28": {http.StatusOK, hlsBody(free)}}, free, "android_vr", "", []string{"101", "28"}},
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

// tvSignedBody is TV's answer as it comes for a public VOD: an adequate
// split pair, each a signatureCipher entry whose URL carries an n — formats
// only the player JS makes fetchable — and, when hls is set, a live HLS
// master whose path carries an n.
func tvSignedBody(hls bool) string {
	master := ""
	if hls {
		master = `, "hlsManifestUrl": "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/abcdefghijkl/file/index.m3u8"`
	}
	return `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 299, "signatureCipher": "s=ENCSIG&sp=sig&url=https%3A%2F%2Ftv.example%2Fv%3Fn%3Dabcdefghijkl", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 140, "signatureCipher": "s=ENCSIG&sp=sig&url=https%3A%2F%2Ftv.example%2Fa%3Fn%3Dabcdefghijkl", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	]` + master + `}
}`
}

// visionosDirectBody is visionos's answer: the same streams with direct
// URLs that carry no n, and a live HLS master with none.
const visionosDirectBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 299, "url": "https://visionos.example/v", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 140, "url": "https://visionos.example/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	], "hlsManifestUrl": "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/source/visionos/file/index.m3u8"}
}`

// TestJSLessExtractionRanksVisionOSFirst is owner decision 4 (audit G10):
// while the player's JS challenges cannot be solved — the cipher sidecar
// disabled or down — an extraction ranks for a download without player JS,
// as yt-dlp asks _DEFAULT_JSLESS_CLIENTS = ('visionos',) when no JS runtime
// is available (_video.py:144, 2972-2991): visionos is asked even though
// TV's answer was adequate (the cascade asks the cookieless chain only when
// it is not), its copies win their streams over TV's signatureCipher ones,
// its n-free HLS master replaces TV's, and the info says so (JSLess) for the
// worker's "formats may be missing" warning. While they can be solved, and
// with no hook at all (the default), nothing changes: visionos is not asked
// and TV's copies win. When the cookieless chain ran anyway (TV
// inadequate), visionos is asked once, not twice, and still wins. The
// signed-in cascade ranks the same way, which goes beyond upstream (its
// authenticated tuple has no JS-less variant).
//
// Two extractions get no extra visionos call though they rank JS-lessly: an
// upcoming stream's (the waiting room has no formats to rank, and a poll
// every 30 s would pay a call each time) and a refused one's (a members-only
// verdict, which a cookieless client cannot answer) — both still say JSLess.
// And a master is never added where the extraction found none: an HLS
// master decides which strategy a stream is routed to, and visionos's is
// only a replacement for one whose n cannot be answered.
//
// Mutants: the hook ignored (the JS-less row asks no visionos and keeps tv's
// copies); visionos pooled at its usual level while JS-less (the chain row's
// tv 140 wins); rankJSLess's visionos call dropped (the JS-less row); its
// asked check dropped (the chain row asks visionos twice); its upcoming
// check, or its playability check, dropped (the upcoming row, or the
// members-only row, asks visionos); the HLS swap dropped (the JS-less row
// keeps tv's master), or made to fill an empty master (the no-master row);
// JSLess left unset (the JS-less rows).
func TestJSLessExtractionRanksVisionOSFirst(t *testing.T) {
	type resp = struct {
		status int
		body   string
	}
	audioOnlyTV := `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 140, "signatureCipher": "s=ENCSIG&sp=sig&url=https%3A%2F%2Ftv.example%2Fa%3Fn%3Dabcdefghijkl", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	]}
}`
	for _, tc := range []struct {
		name          string
		authed        bool
		hook          func() bool
		tv            string
		wantSource    string
		wantVisionOS  int
		wantJSLess    bool
		wantHlsSource string
	}{
		{"JS-less: visionos asked and ranked first", false, func() bool { return false }, tvSignedBody(true), "visionos", 1, true, "visionos"},
		{"solvable: unchanged", false, func() bool { return true }, tvSignedBody(true), "tv_public", 0, false, "tv_public"},
		{"no hook: unchanged", false, nil, tvSignedBody(true), "tv_public", 0, false, "tv_public"},
		{"JS-less, the chain ran: visionos asked once", false, func() bool { return false }, audioOnlyTV, "visionos", 1, true, "visionos"},
		{"signed in, JS-less: visionos ranked first too", true, func() bool { return false }, tvSignedBody(true), "visionos", 1, true, "visionos"},
		{"signed in, solvable: unchanged", true, func() bool { return true }, tvSignedBody(true), "tv_auth", 0, false, "tv_auth"},
		{"JS-less, upcoming: visionos not asked", false, func() bool { return false }, upcomingTVBody, "", 0, true, ""},
		{"signed in, JS-less, members-only: visionos not asked", true, func() bool { return false }, membersOnlyBody, "", 0, true, ""},
		{"JS-less, no master: none added", false, func() bool { return false }, tvSignedBody(false), "visionos", 1, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubWatchPage(t)
			tr := &clientKeyedTransport{responses: map[string]resp{"7": {http.StatusOK, tc.tv}, "101": {http.StatusOK, visionosDirectBody}}}
			if tc.tv == membersOnlyBody {
				// WEB_CREATOR agrees, so the cascade skips the cookieless
				// chain: no anonymous client can answer a members-only video.
				tr.responses["62"] = resp{http.StatusOK, membersOnlyBody}
			}
			orig := apiClient
			apiClient = &http.Client{Transport: tr}
			t.Cleanup(func() { apiClient = orig })
			p := newRetryTestAPI()
			if tc.hook != nil {
				p.SetJSChallengeSolvable(tc.hook)
			}
			extract := p.GetVideoInfoPublic
			if tc.authed {
				extract = p.GetVideoInfoAuthenticated
			}
			info, err := extract(context.Background(), "test1234567")
			if info == nil || (err != nil && tc.tv != membersOnlyBody) {
				t.Fatalf("GetVideoInfo = %+v, %v", info, err)
			}
			asked := 0
			for _, c := range tr.calls {
				if c == "101" {
					asked++
				}
			}
			if asked != tc.wantVisionOS {
				t.Errorf("visionos asked %d time(s), want %d (calls %v)", asked, tc.wantVisionOS, tr.calls)
			}
			for _, itag := range []int{299, 140} {
				for _, f := range info.Formats {
					if f.Itag == itag && f.Source != tc.wantSource {
						t.Errorf("itag %d from %s, want %s", itag, f.Source, tc.wantSource)
					}
				}
			}
			if info.JSLess != tc.wantJSLess {
				t.Errorf("JSLess = %v, want %v", info.JSLess, tc.wantJSLess)
			}
			if info.HlsManifestSource != tc.wantHlsSource {
				t.Errorf("HLS master from %q (%s), want %q", info.HlsManifestSource, info.HlsManifestURL, tc.wantHlsSource)
			}
		})
	}
}

// membersOnlyBody is a members-only refusal, as TV and WEB_CREATOR give it to
// an account that is not a member.
const membersOnlyBody = `{
	"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Join this channel to get access to members-only content like this video, and other exclusive perks."},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"}
}`

// TestCookielessFormatsRankJSLessly: the cookieless re-extract the worker
// merges into its pool (the missing_pot degrade, the challenge-free fallback)
// labels visionos's copies the way the cascade does — at
// AuthLevelVisionOSJSLess while no player JS challenge can be solved, so the
// merge's re-dedup ranks them over the tv copies that cannot be fetched then,
// and at AuthLevelVisionOS otherwise.
//
// Mutant: CookielessFormats not marking its extraction (the JS-less row).
func TestCookielessFormatsRankJSLessly(t *testing.T) {
	for _, tc := range []struct {
		solvable bool
		want     int
	}{{false, AuthLevelVisionOSJSLess}, {true, AuthLevelVisionOS}} {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{"101": {http.StatusOK, adequateOKBody}}}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		p := NewPlayerAPI(nil, noopLogger{})
		p.SetJSChallengeSolvable(func() bool { return tc.solvable })
		got, err := p.CookielessFormats(context.Background(), "test1234567", "vd")
		apiClient = orig
		if err != nil || len(got) == 0 {
			t.Fatalf("solvable=%v: CookielessFormats = %v, %v", tc.solvable, got, err)
		}
		for _, f := range got {
			if f.AuthLevel == nil || *f.AuthLevel != tc.want {
				t.Errorf("solvable=%v: itag %d at AuthLevel %v, want %d", tc.solvable, f.Itag, f.AuthLevel, tc.want)
			}
		}
	}
}
