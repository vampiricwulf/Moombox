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

// The still-live retry stops on a credential wall read twice in a row — no
// retry gets past dead credentials, and the budget it spent was what turned
// the park into a Finished job. One walled read is not believed: a transient
// wall on a healthy session parked a live capture for good (a membership park
// waits for an account change nothing will make), so it is read once more.
//
// Mutants: the first wall believed — parking on the read the failed refresh
// was given ("one wall, then healthy" never refreshes) or on the first walled
// re-read ("a wall between healthy reads" parks); the wall check dropped (the
// doomed refresh runs and the manifest error comes back instead).
func TestRefreshWhileLiveStopsAtACredentialWall(t *testing.T) {
	walled := &youtube.VideoInfo{
		StreamStatus:     youtube.StreamLive,
		PlayabilityError: youtube.PlayabilityMembersOnly,
		SessionAuth:      youtube.SessionAuthLoggedOut,
	}
	healthy := &youtube.VideoInfo{StreamStatus: youtube.StreamLive}
	for _, tc := range []struct {
		name          string
		info          *youtube.VideoInfo
		prober        *scriptedProber
		wantPark      bool
		wantRefreshes int
	}{
		{"walled twice", walled, &scriptedProber{answers: []*youtube.VideoInfo{walled}}, true, 0},
		{"walls up while retrying", healthy, &scriptedProber{answers: []*youtube.VideoInfo{walled, walled}}, true, 0},
		{"one wall, then healthy", walled, &scriptedProber{answers: []*youtube.VideoInfo{healthy}}, false, 1},
		{"a wall between healthy reads", healthy, &scriptedProber{answers: []*youtube.VideoInfo{walled, healthy}}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks atomic.Int32
			refreshes := 0
			r, err := refreshWhileLiveWith(context.Background(), tc.prober, "v", tc.info, errManifest, &checks, func() {},
				time.Millisecond, func(*youtube.VideoInfo) (*DownloadResult, error) {
					refreshes++
					return &DownloadResult{}, nil
				}, nopWorkerLogger{}, "j")
			if parked := errors.Is(err, ErrCookiesRequired); parked != tc.wantPark {
				t.Errorf("err %v, result %v — want park %v", err, r, tc.wantPark)
			}
			if refreshes != tc.wantRefreshes {
				t.Errorf("%d refreshes, want %d", refreshes, tc.wantRefreshes)
			}
		})
	}
}

// runLiveStreamDownload cannot be driven, so the three refresh failures that
// can see a credential wall — the quality-loss refresh, the split's refresh
// and the stream-end verify's refresh — are pinned by source to return it
// rather than finish the capture. The first two get the two-read rule from
// refreshWhileLiveWith; the verify branch keeps its own (verifyWalled), set on
// a walled failure, cleared on a successful refresh, and parking only on the
// second wall in a row.
//
// Mutants: any one of the three returning nil again; the verify branch
// parking on its first wall; verifyWalled never cleared.
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
		{"if credErr != nil && verifyWalled {", "return result, waitedForResume.value(), credErr"},
		{"credErr := liveCredentialFailure(freshInfo)", "verifyWalled = credErr != nil"},
		{"waitedForResume.resolved()\n\t\t\twaitEpisode.reset()", "verifyWalled = false"},
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
