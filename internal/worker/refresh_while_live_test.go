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

type scriptedProber struct {
	answers []*youtube.VideoInfo
	errs    []error
	calls   int
}

func (p *scriptedProber) GetVideoInfo(context.Context, string) (*youtube.VideoInfo, error) {
	i := p.calls
	p.calls++
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, p.errs[i]
	}
	if i < len(p.answers) {
		return p.answers[i], nil
	}
	return &youtube.VideoInfo{StreamStatus: youtube.StreamLive}, nil
}

var errManifest = errors.New("dash manifest fetch: 503")

// A live refresh that failed without resume evidence used to end the
// recording on the spot — a transient manifest or cipher fetch marked a
// stream YouTube had just called live Finished mid-broadcast. It is retried
// now, for as long as YouTube keeps saying live and within the still-live
// verify budget.
//
// Mutant: refreshWhileLiveWith returning the error at once — the "recovers"
// row gets no result.
func TestRefreshWhileLive(t *testing.T) {
	live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive}
	ended := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive}
	for _, tc := range []struct {
		name        string
		info        *youtube.VideoInfo
		prober      *scriptedProber
		startChecks int32
		failFirst   int // refreshes that fail before one succeeds
		wantOK      bool
		wantRefresh int
	}{
		{"recovers", live, &scriptedProber{}, 0, 1, true, 2},
		{"ends while retrying", live, &scriptedProber{answers: []*youtube.VideoInfo{ended}}, 0, 9, false, 0},
		{"budget spent", live, &scriptedProber{}, maxConsecutiveLiveChecks - 1, 9, false, 0},
		{"not live to begin with", ended, &scriptedProber{}, 0, 9, false, 0},
		{"a failed re-read is not a verdict", live, &scriptedProber{errs: []error{errors.New("429")}}, 0, 0, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks atomic.Int32
			checks.Store(tc.startChecks)
			refreshes := 0
			r, err := refreshWhileLiveWith(context.Background(), tc.prober, "v", tc.info, errManifest, &checks, func() {},
				time.Millisecond, func(*youtube.VideoInfo) (*DownloadResult, error) {
					refreshes++
					if refreshes <= tc.failFirst {
						return nil, errManifest
					}
					return &DownloadResult{}, nil
				}, nopWorkerLogger{}, "j")
			if ok := err == nil && r != nil; ok != tc.wantOK {
				t.Errorf("result %v, err %v — want success %v", r, err, tc.wantOK)
			}
			if refreshes != tc.wantRefresh {
				t.Errorf("%d refreshes, want %d", refreshes, tc.wantRefresh)
			}
			if !tc.wantOK && err == nil {
				t.Error("a failed retry must hand back an error")
			}
		})
	}
}

// runLiveStreamDownload cannot be driven from a test (the YouTube service has
// no injectable transport), so the two call sites are pinned by source: the
// quality-loss refresh without resume evidence and the split's refresh both
// go through refreshWhileLive before giving up.
//
// Mutant: either site calling refreshDownload alone again.
func TestLiveRefreshSitesRetryWhileLive(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "o.refreshWhileLive(ctx,"); n != 2 {
		t.Errorf("refreshWhileLive is called from %d live-loop sites, want 2 (quality loss, split)", n)
	}
}

// A capture that cannot write its staging stops the live loop with the error
// (Error status, staging and sidecar kept) instead of entering the stream-end
// verification, which re-verified a stream it could not write for up to an
// hour and then finished the job. runLiveStreamDownload cannot be driven, so
// the branch is pinned by source.
//
// Mutant: the ErrLocalWrite check removed.
func TestLiveLoopStopsOnALocalWriteFailure(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "errors.Is(downloadErr, engine.ErrLocalWrite)")
	if i < 0 {
		t.Fatal("the live loop no longer checks for engine.ErrLocalWrite")
	}
	if ret := strings.Index(body[i:], "return result, waitedForResume.value(), downloadErr"); ret < 0 || ret > 600 {
		t.Error("the ErrLocalWrite check no longer returns the download error")
	}
}

// After the engine's maximum_timeout finalize the quiet time already exceeds
// streamSegmentTimeout (both default to ten minutes), so the verify branch's
// old rule — give up on the first failed status look once quiet that long —
// ended the job on one bot-wall, 429 or 5xx. A failed look now spends the
// still-live budget first.
//
// Mutant: giving up on quiet time alone — the second row ends the capture.
func TestAnUnreadableStatusSpendsTheBudgetFirst(t *testing.T) {
	for _, tc := range []struct {
		checks int32
		quiet  time.Duration
		want   bool
	}{
		{1, time.Minute, false},
		{1, streamSegmentTimeout, false},
		{maxConsecutiveLiveChecks, time.Minute, false},
		{maxConsecutiveLiveChecks, streamSegmentTimeout, true},
	} {
		if got := unreadableStatusEndsCapture(tc.checks, tc.quiet); got != tc.want {
			t.Errorf("checks %d, quiet %v: gives up = %v, want %v", tc.checks, tc.quiet, got, tc.want)
		}
	}
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "unreadableStatusEndsCapture(checks, timeSinceLastSeg)") {
		t.Error("the verify branch no longer decides through unreadableStatusEndsCapture")
	}
}

// The still-live refresh starts its new downloaders from the position the
// stopped ones reached, like the quality-loss refresh. Without it they fell
// back to the job's start-of-run DB seq whenever the engine had cleared the
// sidecar — seq 0 on a fresh job (refused over the staged media) or a stale
// position on a restarted one (footage re-fetched and appended). Pinned by
// source; the loop cannot be driven.
//
// Mutant: the forced start removed from the still-live refresh.
func TestStillLiveRefreshStartsWhereTheCaptureStopped(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "// B4: Refresh manifests and create new downloaders")
	if i < 0 {
		t.Fatal("the still-live refresh (B4) is gone")
	}
	block := body[i:]
	call := strings.Index(block, "o.refreshDownload(ctx, curCtx, freshInfo, result.IsHls)")
	seed := strings.Index(block, "curCtx.VideoStartSeq = result.VideoDownloader.CurrentSeq()")
	clear := strings.Index(block, "curCtx.VideoStartSeq, curCtx.AudioStartSeq = 0, 0")
	if call < 0 || seed < 0 || seed > call {
		t.Error("the still-live refresh no longer seeds its start from the stopped downloaders' position")
	}
	if clear < call {
		t.Error("the still-live refresh's forced start is not cleared after the refresh")
	}
}
