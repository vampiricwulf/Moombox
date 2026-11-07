package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestSetupNamesWhyItFellToTheLiveEdge pins owner decision PR-R2's setup
// half: each arm that puts a live capture's first setup on the live HLS
// playlist marks its result with why (DownloadResult.LiveEdgeReason), and a
// setup that records from the start marks nothing — nor does a finished
// VOD's DRM fallback, whose HLS manifest is the whole video. A post-live
// stream's DRM fallback is marked: its HLS holds only the last ~2 h
// (yt-dlp _video.py, adjust_incomplete_format).
//
// Mutants: any one arm's withLiveEdge dropped (its row reads ""); the DRM
// arm marking every isVod setup (the VOD row reads liveEdgePostLiveDRM) or
// none (the post-live row reads "").
func TestSetupNamesWhyItFellToTheLiveEdge(t *testing.T) {
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	hlsInfo := func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
			HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/" + nChal + "/file/index.m3u8", HlsManifestSource: "tv_public"}
	}
	t.Run("from-start arms", func(t *testing.T) {
		systemic := fmt.Errorf("manifestless DASH: resolve video URL: itag 299: %w: %w", errChallengesUnsolvable, cipher.ErrNUnsolved)
		for _, tc := range []struct {
			name  string
			start string
			err   error
			want  string
		}{
			{name: "records from the start", start: ago(time.Hour)},
			{name: "a marathon stream", start: ago(130 * time.Hour), want: liveEdgeMarathon},
			{name: "missing_pot", err: errors.Join(errMissingPot, errors.New("sidecar down")), want: liveEdgeMissingPot},
			{name: "challenges unsolvable, no no-JS pair", err: systemic, want: liveEdgeNoJS},
		} {
			t.Run(tc.name, func(t *testing.T) {
				swapHls(t, &fakeStrategy{kind: "hls"})
				fakeCookieless(t, nil, nil)
				job, _ := reextractJob(t)
				job.Job.StreamStartTime = tc.start
				s := &fakeStrategy{kind: "manifestless_dash", answers: []error{tc.err}}
				res, err := setupWithFallback(context.Background(), job, s, false, hlsInfo(), &StrategyDeps{})
				if err != nil || res == nil {
					t.Fatalf("setup = %+v, %v; want a result", res, err)
				}
				if res.LiveEdgeReason != tc.want {
					t.Errorf("LiveEdgeReason = %q, want %q", res.LiveEdgeReason, tc.want)
				}
			})
		}
	})
	t.Run("DRM", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			isVod  bool
			status youtube.StreamStatus
			want   string
		}{
			{name: "live", status: youtube.StreamLive, want: liveEdgeDRM},
			{name: "post-live", isVod: true, status: youtube.StreamPostLive, want: liveEdgePostLiveDRM},
			{name: "VOD", isVod: true, status: youtube.StreamVOD},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(drmVideoMPD))
				}))
				t.Cleanup(srv.Close)
				swapHls(t, &fakeStrategy{kind: "hls"})
				job, _ := vodPotJob(t)
				info := &youtube.VideoInfo{StreamStatus: tc.status, DashManifestURL: srv.URL + "/api/manifest/dash/id/abc",
					DashManifestSource: "tv_public", HlsManifestURL: "https://m.example/x.m3u8", HlsManifestSource: "tv_public"}
				res, err := setupWithFallback(context.Background(), job, DashStrategy, tc.isVod, info, &StrategyDeps{})
				if err != nil || res == nil || !res.IsHls {
					t.Fatalf("setup = %+v, %v; want the HLS fallback", res, err)
				}
				if res.LiveEdgeReason != tc.want {
					t.Errorf("LiveEdgeReason = %q, want %q", res.LiveEdgeReason, tc.want)
				}
			})
		}
	})
}

// cancellingStrategy is a fakeStrategy that cancels the run's context when
// its setup succeeds (a failed one leaves it, so the fallback still runs), so ExecuteWithChat stops right after the setup — the
// recording itself is not what the test is about.
type cancellingStrategy struct {
	fakeStrategy
	cancel context.CancelFunc
}

func (c *cancellingStrategy) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	res, err := c.fakeStrategy.Download(ctx, job, info, deps)
	if err == nil {
		c.cancel()
	}
	return res, err
}

// TestLiveEdgeStartIsRecordedAndNotifiedOnEveryFall pins owner decision
// PR-R2's row half: ExecuteWithChat writes the setup's reason to the row's
// live_edge_reason and sends a "live_edge" notification; a capture recording
// from the start writes none; and a Reinitialize clears it. Owner decision
// PE-4: a second fall of the same recording (a Resume, a restart) rewrites
// the reason and notifies again — it sent nothing once the row held a
// reason, so the stretch a resumed capture lost to the live edge went
// unannounced.
//
// Mutants: ExecuteWithChat not calling recordLiveEdgeStart (no reason on the
// row, no notification); the prior-reason check back (the second fall sends
// nothing, one notification in all); the Reinitialize reset not clearing
// live_edge_reason.
func TestLiveEdgeStartIsRecordedAndNotifiedOnEveryFall(t *testing.T) {
	w, db := testWorkerSetup(t)
	rec := notificationtest.New()
	w.orchestrator.notifier = rec
	job := &database.Job{ID: "yt_edge", VideoID: "edge", URL: "https://www.youtube.com/watch?v=edge", Platform: "youtube",
		Title: "Marathon", ChannelName: "Chan", Status: database.StatusLive}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	run := func(mfErr error) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mf := &cancellingStrategy{fakeStrategy: fakeStrategy{kind: "manifestless_dash", answers: []error{mfErr}}, cancel: cancel}
		hls := &cancellingStrategy{fakeStrategy: fakeStrategy{kind: "hls"}, cancel: cancel}
		origMF, origHLS := ManifestlessDashStrategy, HlsStrategy
		ManifestlessDashStrategy, HlsStrategy = mf, hls
		defer func() { ManifestlessDashStrategy, HlsStrategy = origMF, origHLS }()
		row, _ := db.GetJob(job.ID)
		jobCtx := &JobContext{Job: row, DB: db, Config: &JobConfig{}, StagingDir: filepath.Join(t.TempDir(), "staging"), Logger: &captureLogger{}}
		info := manifestlessPotInfo("web_creator", "web_creator")
		info.HlsManifestURL, info.HlsManifestSource = "https://m.example/x.m3u8", "web_safari"
		_ = w.orchestrator.ExecuteWithChat(ctx, jobCtx, info, false, nil)
	}

	run(nil) // records from the start
	if row, _ := db.GetJob(job.ID); row.LiveEdgeReason != "" || len(rec.ByEvent("live_edge")) != 0 {
		t.Fatalf("a from-start capture: reason %q, %d notification(s); want none", row.LiveEdgeReason, len(rec.ByEvent("live_edge")))
	}

	missing := errors.Join(errMissingPot, errors.New("sidecar down"))
	run(missing)
	if row, _ := db.GetJob(job.ID); row.LiveEdgeReason != liveEdgeMissingPot {
		t.Errorf("live_edge_reason = %q, want %q", row.LiveEdgeReason, liveEdgeMissingPot)
	}
	if sent := rec.ByEvent("live_edge"); len(sent) != 1 {
		t.Fatalf("sent %d live_edge notification(s), want 1", len(sent))
	}

	run(missing) // the same recording falls back again, as after a Resume
	if sent := rec.ByEvent("live_edge"); len(sent) != 2 {
		t.Errorf("a second fall of the same recording: %d live_edge notification(s) in all, want 2", len(sent))
	}

	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusError})
	w.reinitializeNow(job.ID)
	if row, _ := db.GetJob(job.ID); row.LiveEdgeReason != "" {
		t.Errorf("after a Reinitialize live_edge_reason = %q, want it cleared", row.LiveEdgeReason)
	}
}

// TestLiveEdgeFallInALaterPartIsNotALateStart pins that a restart which
// resumes a live job into a later part (part 1 already recorded, so
// discoverResumeSegment answers seg_1) and falls to the live edge there
// records no live_edge_reason and sends no notification: the archive's
// first part holds the broadcast's start.
func TestLiveEdgeFallInALaterPartIsNotALateStart(t *testing.T) {
	w, db := testWorkerSetup(t)
	rec := notificationtest.New()
	w.orchestrator.notifier = rec
	job := &database.Job{ID: "yt_edge2", VideoID: "edge2", URL: "https://www.youtube.com/watch?v=edge2", Platform: "youtube",
		Title: "Split", ChannelName: "Chan", Status: database.StatusLive}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	part1 := filepath.Join(t.TempDir(), "Split - part1.mp4")
	if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: filepath.Base(part1), FilePath: part1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	missing := errors.Join(errMissingPot, errors.New("sidecar down"))
	mf := &cancellingStrategy{fakeStrategy: fakeStrategy{kind: "manifestless_dash", answers: []error{missing}}, cancel: cancel}
	hls := &cancellingStrategy{fakeStrategy: fakeStrategy{kind: "hls"}, cancel: cancel}
	origMF, origHLS := ManifestlessDashStrategy, HlsStrategy
	ManifestlessDashStrategy, HlsStrategy = mf, hls
	defer func() { ManifestlessDashStrategy, HlsStrategy = origMF, origHLS }()
	row, _ := db.GetJob(job.ID)
	jobCtx := &JobContext{Job: row, DB: db, Config: &JobConfig{}, StagingDir: filepath.Join(t.TempDir(), "staging"), Logger: &captureLogger{}}
	info := manifestlessPotInfo("web_creator", "web_creator")
	info.HlsManifestURL, info.HlsManifestSource = "https://m.example/x.m3u8", "web_safari"
	_ = w.orchestrator.ExecuteWithChat(ctx, jobCtx, info, false, nil)

	if len(hls.seen) == 0 {
		t.Fatal("the restart never fell to the HLS playlist; the test does not exercise the later-part fall")
	}
	if row, _ := db.GetJob(job.ID); row.LiveEdgeReason != "" {
		t.Errorf("a fall in part 2 recorded live_edge_reason %q, want none", row.LiveEdgeReason)
	}
	if sent := rec.ByEvent("live_edge"); len(sent) != 0 {
		t.Errorf("a fall in part 2 sent %d live_edge notification(s), want none", len(sent))
	}
}
