package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestRefreshGvsCredentialsKeepsURLWhenTheVisitorSessionMoved pins audit
// G51: the 403 refresh mints under the setup binding first, then re-extracts,
// and a re-extraction issued another visitor session — invalidate403Caches
// dropped the cached visitor data — returns URLs of that session. yt-dlp
// mints each extraction's tokens under its own visitor_data (_video.py:
// 3084-3096, 1953-1960); here the token-carrying stream keeps its current URL
// and takes the fresh token alone. The same visitor binding, a stream that
// carries no token, and a binding that is no visitor-data one install the
// fresh URL as before.
//
// Mutant: dropping the binding guard fails the moved row (the fresh URL is
// installed under a token of another session).
func TestRefreshGvsCredentialsKeepsURLWhenTheVisitorSessionMoved(t *testing.T) {
	const warnMsg = "[POT] credential refresh: the re-extraction's visitor session is not the one the token is bound to — keeping the current URL"
	for _, tc := range []struct {
		name        string
		source      string
		freshKind   string
		freshValue  string
		wantInstall bool
		wantToken   string
	}{
		{name: "the visitor session moved", source: "web_creator", freshKind: youtube.BindingVisitorData, freshValue: "vd-new", wantToken: "fresh-token"},
		{name: "the same visitor session", source: "web_creator", freshKind: youtube.BindingVisitorData, freshValue: "vd-123", wantInstall: true, wantToken: "fresh-token"},
		{name: "a stream that carries no token", source: "tv_auth", freshKind: youtube.BindingVisitorData, freshValue: "vd-new", wantInstall: true},
		{name: "the video-ID binding", source: "web_creator", freshKind: youtube.BindingVideoID, freshValue: "vid1", wantInstall: true, wantToken: "fresh-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := refreshVideoInfo
			refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
				return &youtube.VideoInfo{
					PlayerURL:      "https://www.youtube.com/s/player/deadbeef/player.js",
					GvsBinding:     tc.freshValue,
					GvsBindingKind: tc.freshKind,
					Formats:        []youtube.Format{{Itag: 140, URL: "https://fresh.invalid/videoplayback?id=2&n=abc123", Source: tc.source}},
				}, nil
			}
			t.Cleanup(func() { refreshVideoInfo = orig })
			fake := &fakePotProvider{generate: func(context.Context, string, bool) (string, error) { return "fresh-token", nil }}
			solver := &orderedSolver{onN: func(context.Context, string, string) (string, error) { return "decrypted-n", nil }}
			logs := &captureLogger{}
			job := &JobContext{Job: &database.Job{ID: "test-job", VideoID: "vid1"}, YT: youtube.NewService(nil, &discardLogger{}), Logger: logs}
			videoInfo := &youtube.VideoInfo{
				PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
				Formats:   []youtube.Format{{Itag: 140, URL: "https://setup.invalid/videoplayback?id=1&n=abc123", Source: tc.source}},
			}

			baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, solver, nil, fake, "vd-123", "test")

			if token != tc.wantToken {
				t.Errorf("token = %q, want %q", token, tc.wantToken)
			}
			warns := potLines(logs, warnMsg)
			if tc.wantInstall {
				if !strings.Contains(baseURL, "fresh.invalid") || len(warns) != 0 {
					t.Errorf("baseURL = %q, binding warns %v; want the fresh URL installed", baseURL, warns)
				}
				return
			}
			if baseURL != "" || len(warns) != 1 {
				t.Errorf("baseURL = %q, binding warns %v; want the current URL kept, said once", baseURL, warns)
			}
		})
	}
}
