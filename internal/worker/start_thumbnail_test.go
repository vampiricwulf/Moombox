package worker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// serveYouTubeThumbnails points ytThumbnailBase at a stub for the test and
// returns the count of requests it has answered. A nil body answers 404 for
// every size.
func serveYouTubeThumbnails(t *testing.T, body []byte) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	orig := ytThumbnailBase
	ytThumbnailBase = srv.URL
	t.Cleanup(func() { ytThumbnailBase = orig })
	return &asked
}

// TestCopyAssetsPrefersTheFreshYouTubeThumbnail is owner ruling R2-X2's
// finalize half: with a thumbnail staged at capture start, the one fetched at
// the finalize still wins, and the staged one is the archive's only when
// every fetch failed — ahead of the channel avatar, which is not the video's
// thumbnail at all.
//
// Mutants: the staged copy read first, as Twitch's is (the "both" row saves
// the staged bytes); the staged fallback dropped (the "video is gone" rows
// save nothing, or the avatar).
func TestCopyAssetsPrefersTheFreshYouTubeThumbnail(t *testing.T) {
	fresh := bytes.Repeat([]byte("f"), 2000)
	staged := bytes.Repeat([]byte("s"), 2000)
	avatar := bytes.Repeat([]byte("a"), 2000)
	avatars := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(avatar) }))
	defer avatars.Close()

	for _, tc := range []struct {
		name      string
		served    []byte // what the thumbnail host answers; nil is 404
		staged    bool
		thumbURL  bool // the row names a thumbnail URL (on the same, failing, host)
		avatarURL bool
		want      []byte // nil: no thumbnail saved
	}{
		{"both: the fresh one", fresh, true, false, false, fresh},
		{"the video is gone: the staged one", nil, true, false, false, staged},
		{"the video is gone and the row's own URL fails too: the staged one", nil, true, true, true, staged},
		{"the staged one ahead of the channel avatar", nil, true, false, true, staged},
		{"nothing staged and no thumbnail URL: the avatar, as before", nil, false, false, true, avatar},
		{"nothing staged, nothing served: none", nil, false, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveYouTubeThumbnails(t, tc.served)
			o := &DownloadOrchestrator{logger: discardLogger{}}
			job := &database.Job{ID: "yt_thumb", VideoID: "vid", Platform: "youtube"}
			if tc.thumbURL {
				job.ThumbnailURL = ytThumbnailBase + "/vid/maxresdefault.jpg"
			}
			if tc.avatarURL {
				job.ChannelAvatarURL = avatars.URL + "/avatar.jpg"
			}
			jobCtx := &JobContext{Job: job, StagingDir: t.TempDir()}
			if tc.staged {
				if err := os.WriteFile(filepath.Join(jobCtx.StagingDir, startThumbnailFile), staged, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			outDir := t.TempDir()
			updates := map[string]any{}
			o.copyAssets(context.Background(), jobCtx, outDir, "A Job", "A Job", updates, false)

			wantPath := filepath.Join(outDir, "A Job.jpg")
			if tc.want == nil {
				if v, ok := updates["thumbnail_file"]; ok {
					t.Errorf("thumbnail_file = %v, want none", v)
				}
				return
			}
			if updates["thumbnail_file"] != wantPath {
				t.Fatalf("thumbnail_file = %v, want %s", updates["thumbnail_file"], wantPath)
			}
			body, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatalf("read the saved thumbnail: %v", err)
			}
			if !bytes.Equal(body, tc.want) {
				t.Errorf("the saved thumbnail starts %q, want %q", body[:1], tc.want[:1])
			}
		})
	}
}

// TestStageStartThumbnail: the early fetch stages the thumbnail under its own
// name, leaves a copy an earlier start staged alone without asking again,
// stages nothing when the host has no image, and its stop ends a fetch that
// is still waiting on the host rather than waiting it out.
//
// Mutants: the existing-file check dropped (the host is asked again and the
// first start's copy is replaced); stop not cancelling (the hung fetch holds
// stop for the whole timeout).
func TestStageStartThumbnail(t *testing.T) {
	image := bytes.Repeat([]byte("i"), 2000)
	o := &DownloadOrchestrator{logger: discardLogger{}}
	newCtx := func(t *testing.T) *JobContext {
		return &JobContext{Job: &database.Job{ID: "yt_start", VideoID: "vid", Platform: "youtube"},
			StagingDir: filepath.Join(t.TempDir(), "yt_start")}
	}
	waitFor := func(t *testing.T, path string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !fileExists(path) {
			if time.Now().After(deadline) {
				t.Fatalf("%s was never staged", filepath.Base(path))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	t.Run("stages the thumbnail", func(t *testing.T) {
		serveYouTubeThumbnails(t, image)
		jobCtx := newCtx(t)
		stop := o.stageStartThumbnail(context.Background(), jobCtx)
		staged := filepath.Join(jobCtx.StagingDir, startThumbnailFile)
		waitFor(t, staged)
		stop()
		if body, err := os.ReadFile(staged); err != nil || !bytes.Equal(body, image) {
			t.Errorf("staged thumbnail: err %v, %d bytes; want the served image", err, len(body))
		}
		// The staged thumbnail alone is nothing to resume or delete.
		if HasStagingFiles(filepath.Dir(jobCtx.StagingDir), "yt_start") {
			t.Error("a staging dir holding only the start thumbnail reads as preserved staging")
		}
	})

	t.Run("keeps an earlier start's copy", func(t *testing.T) {
		asked := serveYouTubeThumbnails(t, image)
		jobCtx := newCtx(t)
		staged := filepath.Join(jobCtx.StagingDir, startThumbnailFile)
		if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(staged, []byte("first start"), 0o644); err != nil {
			t.Fatal(err)
		}
		o.stageStartThumbnail(context.Background(), jobCtx)()
		if body, _ := os.ReadFile(staged); string(body) != "first start" || asked.Load() != 0 {
			t.Errorf("staged = %q after %d request(s); want the first start's copy and no request", body, asked.Load())
		}
	})

	t.Run("no image: nothing staged", func(t *testing.T) {
		asked := serveYouTubeThumbnails(t, nil)
		jobCtx := newCtx(t)
		stop := o.stageStartThumbnail(context.Background(), jobCtx)
		deadline := time.Now().Add(10 * time.Second)
		for int(asked.Load()) < len(ytThumbnailQualities) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		stop()
		entries, _ := os.ReadDir(jobCtx.StagingDir)
		if len(entries) != 0 {
			t.Errorf("staging holds %d file(s) after a fetch that found no image", len(entries))
		}
	})

	t.Run("stop ends a fetch the host never answers", func(t *testing.T) {
		reached, release := make(chan struct{}, 8), make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			reached <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		orig := ytThumbnailBase
		ytThumbnailBase = srv.URL
		defer func() { ytThumbnailBase = orig }()

		jobCtx := newCtx(t)
		stop := o.stageStartThumbnail(context.Background(), jobCtx)
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			t.Fatal("the fetch never reached the host")
		}
		start := time.Now()
		stop()
		if took := time.Since(start); took > startThumbnailTimeout/2 {
			t.Errorf("stop took %v: it waited the fetch out", took)
		}
		if entries, _ := os.ReadDir(jobCtx.StagingDir); len(entries) != 0 {
			t.Errorf("staging holds %d file(s) after a cancelled fetch", len(entries))
		}
	})
}

// thumbnailWaitingStrategy is a download whose setup waits for the start
// thumbnail to be staged (or for wait to run out), records whether it was,
// and then fails, so ExecuteWithChat returns.
type thumbnailWaitingStrategy struct {
	wait   time.Duration
	staged atomic.Bool
}

var errThumbnailTestStop = errors.New("stop after the thumbnail check")

func (s *thumbnailWaitingStrategy) Kind() string { return "manifestless_dash" }

func (s *thumbnailWaitingStrategy) Download(_ context.Context, job *JobContext, _ *youtube.VideoInfo, _ *StrategyDeps) (*DownloadResult, error) {
	path := filepath.Join(job.StagingDir, startThumbnailFile)
	for deadline := time.Now().Add(s.wait); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if fileExists(path) {
			s.staged.Store(true)
			break
		}
	}
	return nil, errThumbnailTestStop
}

// TestExecuteWithChatStagesTheThumbnailOfALiveCapture is owner ruling R2-X2's
// capture half: a live capture's thumbnail is in staging while the capture
// runs, fetched beside the download and not ahead of it, and a from-the-start
// download of a video asks for none.
//
// Mutants: the stageStartThumbnail call removed (nothing is staged); made for
// a VOD as well (the thumbnail host is asked).
func TestExecuteWithChatStagesTheThumbnailOfALiveCapture(t *testing.T) {
	image := bytes.Repeat([]byte("i"), 2000)

	t.Run("live", func(t *testing.T) {
		serveYouTubeThumbnails(t, image)
		w, db := testWorkerSetup(t)
		job := &database.Job{ID: "yt_live_thumb", VideoID: "livethumb", URL: "https://www.youtube.com/watch?v=livethumb",
			Platform: "youtube", Title: "T", ChannelName: "C", Status: database.StatusLive}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		strat := &thumbnailWaitingStrategy{wait: 10 * time.Second}
		orig := ManifestlessDashStrategy
		ManifestlessDashStrategy = strat
		defer func() { ManifestlessDashStrategy = orig }()
		row, _ := db.GetJob(job.ID)
		jobCtx := &JobContext{Job: row, DB: db, Config: &JobConfig{}, StagingDir: filepath.Join(t.TempDir(), job.ID), Logger: &captureLogger{}}
		err := w.orchestrator.ExecuteWithChat(context.Background(), jobCtx, manifestlessPotInfo("web_creator", "web_creator"), false, nil)
		if !errors.Is(err, errThumbnailTestStop) {
			t.Fatalf("ExecuteWithChat = %v, want the strategy's stop", err)
		}
		if !strat.staged.Load() {
			t.Fatal("no thumbnail was staged while the capture ran")
		}
		if body, err := os.ReadFile(filepath.Join(jobCtx.StagingDir, startThumbnailFile)); err != nil || !bytes.Equal(body, image) {
			t.Errorf("staged thumbnail: err %v, %d bytes; want the served image", err, len(body))
		}
	})

	t.Run("a from-the-start download asks for none", func(t *testing.T) {
		asked := serveYouTubeThumbnails(t, image)
		w, db := testWorkerSetup(t)
		job := &database.Job{ID: "yt_vod_thumb", VideoID: "vodthumb", URL: "u", Platform: "youtube",
			Title: "T", ChannelName: "C", Status: database.StatusUpcoming, OutputDirectory: t.TempDir()}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		stored, _ := db.GetJob(job.ID)
		jobCtx := w.buildJobContext(stored)
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled})
			http.Error(rw, "cut", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		o := NewDownloadOrchestrator(db, nil, "", discardLogger{}, nil, stubCipherSolver{}, nil, notificationtest.New(), nil)
		_ = o.ExecuteWithChat(context.Background(), jobCtx, wholeFileVodInfo(srv, 1024), true, nil)
		if n := asked.Load(); n != 0 {
			t.Errorf("the thumbnail host was asked %d time(s) for a from-the-start download", n)
		}
		if fileExists(filepath.Join(jobCtx.StagingDir, startThumbnailFile)) {
			t.Error("a start thumbnail was staged for a from-the-start download")
		}
	})
}
