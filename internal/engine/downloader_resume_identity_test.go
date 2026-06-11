package engine

import "testing"

// Regression tests for the resume-identity rules. The both-URLs-opaque case
// is the load-bearing one: Twitch weaver URLs rotate their session token on
// every master-playlist fetch, and treating that as a mismatch used to
// discard the resume state and O_TRUNC hours of recording on every daemon
// restart.
func TestResumeIdentityMismatch(t *testing.T) {
	const (
		ytDashA  = "https://r4---sn.googlevideo.com/videoplayback/id/abc123DEF45.1/itag/299/source/yt_live_broadcast/sq/100"
		ytDashA2 = "https://r2---sn.googlevideo.com/videoplayback/id/abc123DEF45.2/itag/299/source/yt_live_broadcast/expire/999/sq/200"
		ytDashB  = "https://r4---sn.googlevideo.com/videoplayback/id/zzz999XYZ11.1/itag/299/source/yt_live_broadcast/sq/5"
		weaverA  = "https://video-weaver.fra02.hls.ttvnw.net/v1/playlist/token-AAAA.m3u8"
		weaverB  = "https://video-weaver.sea01.hls.ttvnw.net/v1/playlist/token-BBBB.m3u8"
	)

	cases := []struct {
		name         string
		state        ResumeState
		optStreamID  string
		currentURL   string
		wantMismatch bool
	}{
		{
			name:         "twitch: opaque URLs differ but stream IDs match — RESUME (the truncation fix)",
			state:        ResumeState{BaseURL: weaverA, StreamID: "55511122"},
			optStreamID:  "55511122",
			currentURL:   weaverB,
			wantMismatch: false,
		},
		{
			name:         "twitch: explicit stream IDs differ — new broadcast, fresh start",
			state:        ResumeState{BaseURL: weaverA, StreamID: "55511122"},
			optStreamID:  "66600099",
			currentURL:   weaverB,
			wantMismatch: true,
		},
		{
			name:         "twitch legacy state without StreamID: opaque URLs differ — still resume",
			state:        ResumeState{BaseURL: weaverA},
			optStreamID:  "55511122",
			currentURL:   weaverB,
			wantMismatch: false,
		},
		{
			name:         "youtube: same video+itag across session-param rotation — resume",
			state:        ResumeState{BaseURL: ytDashA},
			currentURL:   ytDashA2,
			wantMismatch: false,
		},
		{
			name:         "youtube: different video — fresh start",
			state:        ResumeState{BaseURL: ytDashA},
			currentURL:   ytDashB,
			wantMismatch: true,
		},
		{
			name:         "mixed shapes (one identity extracts, one doesn't) — conservative fresh start",
			state:        ResumeState{BaseURL: ytDashA},
			currentURL:   weaverA,
			wantMismatch: true,
		},
		{
			name:         "empty saved BaseURL — nothing to compare, resume",
			state:        ResumeState{},
			currentURL:   weaverA,
			wantMismatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := resumeIdentityMismatch(&tc.state, tc.optStreamID, tc.currentURL)
			if got != tc.wantMismatch {
				t.Errorf("mismatch = %v (reason %q), want %v", got, reason, tc.wantMismatch)
			}
		})
	}
}
