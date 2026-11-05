package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestMarathonLiveCaptureRecordsTheLiveEdge pins audit G11's setup half and
// owner decision 10: a live capture of a broadcast live longer than ~118 h
// (marathonLiveEdge) recorded nothing — it always began at sq 0 or the DB
// seq, which YouTube's ~120 h window has dropped, and spent half an hour of
// still-live checks before ending with zero bytes. It now records the live
// HLS playlist, as yt-dlp records a live stream by default, and the job log
// says why from-start was dropped. Younger streams, VODs and post-live
// runs, and a part that already holds a DASH capture to append to, keep the
// from-start setup; an HLS setup that fails leaves it to run.
//
// Mutants: the marathon arm dropped from setupWithFallback (the 130 h rows
// run the from-start strategy); the age compared against 120 h of the wrong
// unit or not at all (the 100 h row records HLS); the DASH-manifest kind
// left out (its row); the isVod guard dropped (the post-live row records
// HLS); the staged-capture exemption dropped (the resumed row records HLS);
// the from-start retry after a failed HLS setup dropped (the HLS-fails row
// returns the HLS error); the Warn dropped (every HLS row).
func TestMarathonLiveCaptureRecordsTheLiveEdge(t *testing.T) {
	const warnMsg = "[Live] the stream has been live longer than YouTube keeps its segments (~120 h) — recording from the start cannot run; recording the live HLS playlist instead, from the live edge"
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		kind     string
		isVod    bool
		start    string
		staged   string // a DASH file the part already holds
		hlsErr   error
		wantHls  bool // HLS's result is the setup's
		wantFrom bool // the from-start strategy ran
	}{
		{name: "manifest-free, live 130 h", kind: "manifestless_dash", start: ago(130 * time.Hour), wantHls: true},
		{name: "DASH manifest, live 130 h", kind: "dash", start: ago(130 * time.Hour), wantHls: true},
		{name: "live 100 h", kind: "manifestless_dash", start: ago(100 * time.Hour), wantFrom: true},
		{name: "no start time", kind: "manifestless_dash", wantFrom: true},
		{name: "a start that does not parse", kind: "manifestless_dash", start: "2026-01-01", wantFrom: true},
		{name: "post-live, 130 h", kind: "manifestless_dash", isVod: true, start: ago(130 * time.Hour), wantFrom: true},
		{name: "resumed into a part holding its DASH capture", kind: "manifestless_dash", start: ago(130 * time.Hour), staged: "video_stream", wantFrom: true},
		{name: "the live HLS setup fails", kind: "manifestless_dash", start: ago(130 * time.Hour), hlsErr: errors.New("no HLS manifest URL available"), wantFrom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls", answers: []error{tc.hlsErr}}
			swapHls(t, hls)
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			job.Job.StreamStartTime = tc.start
			if tc.staged != "" {
				if err := os.WriteFile(filepath.Join(job.StagingDir, tc.staged), []byte("moof"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := &fakeStrategy{kind: tc.kind}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8"}
			res, err := setupWithFallback(context.Background(), job, s, tc.isVod, info, &StrategyDeps{})
			if err != nil || res == nil {
				t.Fatalf("setup = %+v, %v; want a result", res, err)
			}
			if res.IsHls != tc.wantHls {
				t.Errorf("result records HLS = %v, want %v", res.IsHls, tc.wantHls)
			}
			if ran := len(s.seen) > 0; ran != tc.wantFrom {
				t.Errorf("from-start strategy ran = %v, want %v", ran, tc.wantFrom)
			}
			triedHls := tc.wantHls || tc.hlsErr != nil
			if (len(hls.seen) == 1) != triedHls || len(hls.seen) > 1 {
				t.Errorf("HLS tried %d time(s), want %v", len(hls.seen), triedHls)
			}
			if rec.warned(warnMsg) != triedHls {
				t.Errorf("the from-start-dropped Warn logged = %v, want %v: %v", rec.warned(warnMsg), triedHls, logs.lines())
			}
			if triedHls {
				if line := logLine(logs, warnMsg); line == nil || line["liveFor"] == "" || line["streamStartTime"] != tc.start {
					t.Errorf("Warn = %v, want the age and the start it was read from", line)
				}
			}
			if tc.hlsErr != nil && !rec.warned("[Live] the live HLS setup failed — trying the from-start capture anyway") {
				t.Errorf("no Warn saying the HLS setup failed: %v", logs.lines())
			}
		})
	}
}

// TestMarathonLiveEdgeAge pins the threshold owner decision 10 set: two hours
// short of yt-dlp's MAX_DURATION, 432000 s.
func TestMarathonLiveEdgeAge(t *testing.T) {
	if marathonLiveEdgeAge != 118*time.Hour {
		t.Errorf("marathonLiveEdgeAge = %v, want 118h", marathonLiveEdgeAge)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	job, _ := reextractJob(t)
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{118 * time.Hour, false}, {118*time.Hour + time.Second, true}, {-time.Hour, false}} {
		job.Job.StreamStartTime = now.Add(-tc.age).Format(time.RFC3339)
		if _, got := marathonLiveEdge(job, &fakeStrategy{kind: "manifestless_dash"}, false, now); got != tc.want {
			t.Errorf("live for %v: marathonLiveEdge = %v, want %v", tc.age, got, tc.want)
		}
	}
}
