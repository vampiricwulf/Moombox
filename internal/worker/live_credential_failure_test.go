package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A live capture whose cookies die mid-broadcast used to have its refresh
// retried and then finish like an ended stream: no COOKIES? park, no
// "Authentication Required" alert, and the credential-recovery sweep — which
// resumes COOKIES? rows only — never looked at the job again. A player
// response that is credential-walled and has nothing to download is now the
// COOKIES?-routing error.
//
// Mutant: liveCredentialFailure ignoring the format count — the "formats
// still served" row parks a capture that can carry on.
func TestLiveCredentialFailure(t *testing.T) {
	formats := []youtube.Format{{Itag: 299}}
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		want error
	}{
		{"no response", nil, nil},
		{"healthy", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityOK}, nil},
		{"cookies dead on a members stream", &youtube.VideoInfo{
			PlayabilityError: youtube.PlayabilityMembersOnly, SessionAuth: youtube.SessionAuthLoggedOut}, ErrCookiesRequired},
		{"signed in but not a member", &youtube.VideoInfo{
			PlayabilityError: youtube.PlayabilityMembersOnly, SessionAuth: youtube.SessionAuthLoggedIn}, ErrNotAMember},
		{"login wall", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityLoginRequired}, ErrCookiesRequired},
		{"formats still served", &youtube.VideoInfo{
			PlayabilityError: youtube.PlayabilityLoginRequired, Formats: formats}, nil},
		{"age gate is not a credential park", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityAgeRestricted}, nil},
		{"unavailable is not a credential park", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityUnavailable}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := liveCredentialFailure(tc.info)
			if tc.want == nil {
				if err != nil {
					t.Errorf("got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want it to wrap %v", err, tc.want)
			}
			if !cookiesStatusError(err) {
				t.Errorf("%v would not park the job at COOKIES?", err)
			}
		})
	}
}

// The still-live retry stops at once on a credential wall: no retry gets
// past dead cookies, and the budget it spent was what turned the park into a
// Finished job.
//
// Mutant: the credential check in refreshWhileLiveWith removed — the retry
// runs and hands back the manifest error instead.
func TestRefreshWhileLiveStopsAtACredentialWall(t *testing.T) {
	walled := &youtube.VideoInfo{
		StreamStatus:     youtube.StreamLive,
		PlayabilityError: youtube.PlayabilityMembersOnly,
		SessionAuth:      youtube.SessionAuthLoggedOut,
	}
	for _, tc := range []struct {
		name   string
		info   *youtube.VideoInfo
		prober *scriptedProber
	}{
		{"walled already", walled, &scriptedProber{}},
		{"walls up while retrying", &youtube.VideoInfo{StreamStatus: youtube.StreamLive},
			&scriptedProber{answers: []*youtube.VideoInfo{walled}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks atomic.Int32
			refreshes := 0
			_, err := refreshWhileLiveWith(context.Background(), tc.prober, "v", tc.info, errManifest, &checks, func() {},
				time.Millisecond, func(*youtube.VideoInfo) (*DownloadResult, error) {
					refreshes++
					return nil, errManifest
				}, nopWorkerLogger{}, "j")
			if !errors.Is(err, ErrCookiesRequired) {
				t.Fatalf("got %v, want the COOKIES? error", err)
			}
			if refreshes > 1 {
				t.Errorf("%d refreshes against a credential wall", refreshes)
			}
		})
	}
}

// runLiveStreamDownload cannot be driven, so the three refresh failures that
// can see a credential wall — the quality-loss refresh, the split's refresh
// and the stream-end verify's refresh — are pinned by source to return it
// rather than finish the capture.
//
// Mutant: any one of the three returning nil again.
func TestLiveLoopReturnsACredentialWall(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, site := range []struct {
		check, ret string
	}{
		{"if cookiesStatusError(refreshErr) {", "return true, refreshErr"},
		{"if cookiesStatusError(refreshErr) {", "return result, waitedForResume.value(), refreshErr"},
		{"if credErr := liveCredentialFailure(freshInfo); credErr != nil {", "return result, waitedForResume.value(), credErr"},
	} {
		found := false
		for off := 0; ; {
			i := strings.Index(body[off:], site.check)
			if i < 0 {
				break
			}
			i += off
			if j := strings.Index(body[i:], site.ret); j >= 0 && j < 300 {
				found = true
				break
			}
			off = i + len(site.check)
		}
		if !found {
			t.Errorf("no %q followed by %q", site.check, site.ret)
		}
	}
}
