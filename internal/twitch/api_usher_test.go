package twitch

import (
	"net/url"
	"strings"
	"testing"
)

// usherParams parses a built usher URL back into its query values.
func usherParams(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse usher URL: %v", err)
	}
	return u.Query()
}

// TestUsherURLsOptInToEnhancedBroadcasts is the owner's "Twitch usher"
// decision. yt-dlp sends platform=web and supported_codecs=av1,h265,h264
// (references/yt-dlp/yt_dlp/extractor/twitch.py, _extract_twitch_m3u8_formats);
// without them Twitch never offers the HEVC/AV1 source and the capture takes
// the H.264 transcode.
//
// Every pre-existing parameter is asserted too: usher is unforgiving, and
// dropping fast_bread or type while adding these two would be a silent
// regression in latency or rerun coverage that no other test would catch.
//
// Mutants: adding platform but not supported_codecs (or the reverse); sending
// the spelling yt-dlp does not ("h265" vs "hevc", "av1" vs "av01" — the usher
// REQUEST uses the short names, unlike the playlist's RFC 6381 ids).
func TestUsherURLsOptInToEnhancedBroadcasts(t *testing.T) {
	token := &TwitchAccessToken{Value: "token-value", Signature: "sig-value"}

	for _, tc := range []struct {
		name string
		url  string
		host string
	}{
		{"live", BuildUsherLiveURL("TestChan", token), "/api/channel/hls/testchan.m3u8"},
		{"vod", BuildUsherVodURL("123456789", token), "/vod/123456789.m3u8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.url, tc.host) {
				t.Fatalf("URL path is not %s", tc.host)
			}
			q := usherParams(t, tc.url)
			if got := q.Get("platform"); got != "web" {
				t.Errorf("platform = %q, want %q", got, "web")
			}
			if got := q.Get("supported_codecs"); got != "av1,h265,h264" {
				t.Errorf("supported_codecs = %q, want %q", got, "av1,h265,h264")
			}
			for k, want := range map[string]string{
				"allow_source":               "true",
				"allow_audio_only":           "true",
				"allow_spectre":              "true",
				"player":                     "twitchweb",
				"playlist_include_framerate": "true",
				"type":                       "any",
				"sig":                        "sig-value",
				"token":                      "token-value",
			} {
				if got := q.Get(k); got != want {
					t.Errorf("%s = %q, want %q (a pre-existing parameter must survive)", k, got, want)
				}
			}
			if q.Get("p") == "" {
				t.Error("p is empty — the cache-buster must survive")
			}
		})
	}

	if q := usherParams(t, BuildUsherLiveURL("TestChan", token)); q.Get("fast_bread") != "true" {
		t.Error("fast_bread = \"\" on the LIVE URL, want \"true\" — low-latency mode is live-only " +
			"and must survive")
	}
	if q := usherParams(t, BuildUsherVodURL("123456789", token)); q.Get("fast_bread") != "" {
		t.Error("fast_bread is set on the VOD URL — it never was, and a VOD has no low-latency edge")
	}
}
