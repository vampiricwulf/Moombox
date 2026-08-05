package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// warnCaptureLogger is a discardLogger variant that records Warn messages,
// so eviction-diagnosis tests can assert the diagnosis actually logged
// without depending on log-line wording.
type warnCaptureLogger struct {
	warns []string
}

func (l *warnCaptureLogger) Debug(string, ...any) {}
func (l *warnCaptureLogger) Info(string, ...any)  {}
func (l *warnCaptureLogger) Warn(msg string, _ ...any) {
	l.warns = append(l.warns, msg)
}
func (l *warnCaptureLogger) Error(string, ...any) {}

func TestSelectedVideoTargetDuration(t *testing.T) {
	itag137 := 137
	itag140 := 140

	tests := []struct {
		name        string
		jobCtx      *JobContext
		videoInfo   *youtube.VideoInfo
		wantSec     int
		wantUnknown bool
	}{
		{
			name:      "nil videoInfo falls back to unknown",
			jobCtx:    &JobContext{Job: &database.Job{}},
			videoInfo: nil,
			wantSec:   1, wantUnknown: true,
		},
		{
			name:   "exact itag match wins",
			jobCtx: &JobContext{Job: &database.Job{SelectedVideoItag: &itag137}},
			videoInfo: &youtube.VideoInfo{Formats: []youtube.Format{
				{Itag: itag140, Width: ptrInt(1280), TargetDurationSec: 5},
				{Itag: itag137, Width: ptrInt(1920), TargetDurationSec: 2},
			}},
			wantSec: 2, wantUnknown: false,
		},
		{
			name:   "no selected itag falls back to any video format carrying it",
			jobCtx: &JobContext{Job: &database.Job{}},
			videoInfo: &youtube.VideoInfo{Formats: []youtube.Format{
				{Itag: 140, TargetDurationSec: 5}, // audio (no Width/Height) — skipped
				{Itag: 137, Width: ptrInt(1920), TargetDurationSec: 5},
			}},
			wantSec: 5, wantUnknown: false,
		},
		{
			name:   "no format carries it at all",
			jobCtx: &JobContext{Job: &database.Job{SelectedVideoItag: &itag137}},
			videoInfo: &youtube.VideoInfo{Formats: []youtube.Format{
				{Itag: itag137, Width: ptrInt(1920)},
			}},
			wantSec: 1, wantUnknown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sec, unknown := selectedVideoTargetDuration(tt.jobCtx, tt.videoInfo)
			if sec != tt.wantSec || unknown != tt.wantUnknown {
				t.Errorf("selectedVideoTargetDuration() = (%d, %v), want (%d, %v)", sec, unknown, tt.wantSec, tt.wantUnknown)
			}
		})
	}
}

func TestEvictionError(t *testing.T) {
	err := evictionError(437123, 121.0)
	want := "stream exceeds YouTube's ~120h retention window: segments 0..437122 are evicted (~121h of the broadcast). " +
		"From-start archiving of marathon streams requires init recovery — see docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D"
	if got := err.Error(); got != want {
		t.Errorf("evictionError() = %q, want %q", got, want)
	}
}

func TestTotalBytesWritten(t *testing.T) {
	if got := totalBytesWritten(&DownloadResult{}); got != 0 {
		t.Errorf("both downloaders nil: got %d, want 0", got)
	}
	unstarted := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: "http://example.invalid/x"})
	if got := totalBytesWritten(&DownloadResult{VideoDownloader: unstarted}); got != 0 {
		t.Errorf("unstarted downloader: got %d, want 0", got)
	}
}

// TestDiagnoseEvictedStart_GuardSkipsNonCandidates exercises the fast-path
// guards that must return nil WITHOUT running a bisection: wrong platform,
// no video downloader, and a HeadSeq that never got deep enough to be
// plausible eviction (the common case — an ordinary failed start).
func TestDiagnoseEvictedStart_GuardSkipsNonCandidates(t *testing.T) {
	o := &DownloadOrchestrator{logger: &discardLogger{}}
	ctx := context.Background()

	t.Run("non-youtube platform", func(t *testing.T) {
		jobCtx := &JobContext{Job: &database.Job{ID: "j1", Platform: "twitch"}}
		result := &DownloadResult{VideoDownloader: engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: "http://example.invalid/x"})}
		if err := o.diagnoseEvictedStart(ctx, jobCtx, nil, result); err != nil {
			t.Errorf("non-youtube platform must skip diagnosis, got err: %v", err)
		}
	})

	t.Run("nil video downloader", func(t *testing.T) {
		jobCtx := &JobContext{Job: &database.Job{ID: "j1", Platform: "youtube"}}
		if err := o.diagnoseEvictedStart(ctx, jobCtx, nil, &DownloadResult{}); err != nil {
			t.Errorf("nil VideoDownloader must skip diagnosis, got err: %v", err)
		}
	})

	t.Run("head never learned (fresh downloader)", func(t *testing.T) {
		jobCtx := &JobContext{Job: &database.Job{ID: "j1", Platform: "youtube"}}
		result := &DownloadResult{VideoDownloader: engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: "http://example.invalid/x"})}
		if err := o.diagnoseEvictedStart(ctx, jobCtx, nil, result); err != nil {
			t.Errorf("HeadSeq below minEvictionHead must skip diagnosis, got err: %v", err)
		}
	})
}

// TestDiagnoseEvictedStart_ConfirmedEviction is the end-to-end happy path:
// a real *engine.SegmentDownloader primed with a HeadSeq deep past
// minEvictionHead (exactly like a real failed download would learn it from
// X-Head-Seqnum on early fetch attempts) against a fake GVS that 403s
// everything below `front` and serves real bytes at/above it. The
// diagnosis must bisect to front, inspect the boundary segment, and return
// the brief's exact eviction message.
func TestDiagnoseEvictedStart_ConfirmedEviction(t *testing.T) {
	const head = 150000
	const front = 120000
	const targetDuration = 2
	const itag = 137

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		seq, _ := strconv.Atoi(r.URL.Query().Get("sq"))
		if seq < front {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// Minimal ftyp box so InspectSegment has real box data to walk.
		w.Write([]byte{0, 0, 0, 8, 'f', 't', 'y', 'p'})
	}))
	defer srv.Close()

	videoDl := engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL: srv.URL + "/videoplayback?id=evict-test&itag=137",
	})
	// Prime HeadSeq exactly the way a real failed download would: one
	// fetch attempt that harvests X-Head-Seqnum, without writing any bytes
	// to the output (ProbeSegmentAvailable never touches bytesWritten).
	if _, _, err := videoDl.ProbeSegmentAvailable(context.Background(), 0); err != nil {
		t.Fatalf("priming probe failed: %v", err)
	}
	if got := videoDl.HeadSeq(); got != head {
		t.Fatalf("HeadSeq priming: got %d, want %d", got, head)
	}
	if got := videoDl.BytesWritten(); got != 0 {
		t.Fatalf("priming probe must not write bytes, got %d", got)
	}

	logger := &warnCaptureLogger{}
	o := &DownloadOrchestrator{logger: logger}

	jobCtx := &JobContext{Job: &database.Job{ID: "j1", VideoID: "vid1", Platform: "youtube", SelectedVideoItag: ptrInt(itag)}}
	videoInfo := &youtube.VideoInfo{Formats: []youtube.Format{
		{Itag: itag, Width: ptrInt(1920), Height: ptrInt(1080), TargetDurationSec: targetDuration},
	}}
	result := &DownloadResult{VideoDownloader: videoDl}

	err := o.diagnoseEvictedStart(context.Background(), jobCtx, videoInfo, result)
	if err == nil {
		t.Fatal("expected a confirmed-eviction error, got nil")
	}

	wantHours := float64(front) * float64(targetDuration) / 3600
	wantMsg := fmt.Sprintf(
		"stream exceeds YouTube's ~120h retention window: segments 0..%d are evicted (~%.0fh of the broadcast). "+
			"From-start archiving of marathon streams requires init recovery — see docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D",
		front-1, wantHours)
	if got := err.Error(); got != wantMsg {
		t.Errorf("error = %q, want %q", got, wantMsg)
	}
	if len(logger.warns) == 0 {
		t.Error("expected diagnoseEvictedStart to log a Warn diagnosis")
	}
}

// TestDiagnoseEvictedStart_DeadURLSkipsEvictionError verifies the brief's
// explicit non-eviction case: when the head segment itself is unavailable
// (dead/expired URL), diagnoseEvictedStart must NOT set an eviction error —
// the caller's ordinary empty-download failure path is expected to run
// instead.
func TestDiagnoseEvictedStart_DeadURLSkipsEvictionError(t *testing.T) {
	const head = 150000

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		w.WriteHeader(http.StatusForbidden) // everything 403s, including head
	}))
	defer srv.Close()

	videoDl := engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL: srv.URL + "/videoplayback?id=dead-url-test&itag=137",
	})
	if _, _, err := videoDl.ProbeSegmentAvailable(context.Background(), 0); err != nil {
		t.Fatalf("priming probe failed: %v", err)
	}

	logger := &warnCaptureLogger{}
	o := &DownloadOrchestrator{logger: logger}
	jobCtx := &JobContext{Job: &database.Job{ID: "j1", Platform: "youtube"}}
	result := &DownloadResult{VideoDownloader: videoDl}

	if err := o.diagnoseEvictedStart(context.Background(), jobCtx, nil, result); err != nil {
		t.Errorf("dead URL (head unavailable) must not set an eviction error, got: %v", err)
	}
	if len(logger.warns) == 0 {
		t.Error("expected a Warn log explaining the aborted bisection")
	}
}
