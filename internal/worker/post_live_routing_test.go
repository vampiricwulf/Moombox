package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// segmentedVodInfo is a VOD whose formats are given, each a plain http URL
// the stub solver passes through (vodPotInfo's convention).
func segmentedVodInfo(formats ...youtube.Format) *youtube.VideoInfo {
	return &youtube.VideoInfo{
		StreamStatus: youtube.StreamVOD,
		PlayerURL:    "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		Formats:      formats,
	}
}

// vodFormat builds one format of a VOD pool: video when w > 0, audio
// otherwise; mod marks it (a length, OTF, live adaptive, a source).
func vodFormat(itag, w, h int, mime string, mod ...func(*youtube.Format)) youtube.Format {
	f := youtube.Format{Itag: itag, URL: "http://127.0.0.1:1/videoplayback?itag=" + strconv.Itoa(itag), MimeType: mime, Bitrate: 100_000 + w*1000, Source: "tv_public"}
	if w > 0 {
		f.Width, f.Height = &w, &h
	}
	for _, m := range mod {
		m(&f)
	}
	return f
}

func asLiveAdaptive(f *youtube.Format) { f.TargetDurationSec = 2 }

const (
	h264 = `video/mp4; codecs="avc1.640028"`
	aac  = `audio/mp4; codecs="mp4a.40.2"`
)

// TestResumeInThePostLiveWindowContinuesTheLiveCapture is G6 on the Resume
// path. Resume of an incomplete_tail job re-runs Process (worker.ResumeJob),
// and in the post-live window its fetch is TV's answer: segmented formats,
// no manifest. Read as a VOD, it went to VodStrategy, which set the live
// capture — the one Resume exists to finish — aside before its whole-file
// GET fetched a fragment and failed. Process now hands on the extraction's
// post-live status (or, unpromoted, formats carrying targetDurationSec), the
// routing takes the manifest-free path, and its downloaders write the very
// files the capture is in, the engine's resume sidecar deciding where they
// continue; nothing is set aside.
//
// Mutant: the routing's live adaptive arm dropped (the unpromoted row goes
// to vod). The promotion itself is the extraction's (finishExtraction,
// TestTVPostLiveAnswerIsPostLive).
func TestResumeInThePostLiveWindowContinuesTheLiveCapture(t *testing.T) {
	for _, status := range []youtube.StreamStatus{youtube.StreamPostLive, youtube.StreamVOD} {
		t.Run(string(status), func(t *testing.T) {
			db := openTestDB(t)
			row := &database.Job{ID: "yt_resume_pl", VideoID: "resumepl", URL: "https://www.youtube.com/watch?v=resumepl", Platform: "youtube", Status: database.StatusDownloading}
			if _, err := db.AddJob(row); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			// What ResumeJob leaves an incomplete_tail row as: Downloading,
			// the tail flag still set until the finalize clears it.
			db.UpdateJobFields(row.ID, map[string]any{"incomplete_tail": true})
			job, err := db.GetJob(row.ID)
			if err != nil || job == nil || !job.IncompleteTail {
				t.Fatalf("GetJob: %v, %+v", err, job)
			}

			staging := t.TempDir()
			capture := []string{"audio_stream", "video_stream"}
			for _, name := range capture {
				if err := os.WriteFile(filepath.Join(staging, name), []byte("live capture"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			fetched := segmentedVodInfo(
				vodFormat(137, 1920, 1080, h264, asLiveAdaptive),
				vodFormat(140, 0, 0, aac, asLiveAdaptive),
			)
			fetched.StreamStatus, fetched.IsPostLiveDVR = status, status == youtube.StreamPostLive
			fetched.PlayabilityError = youtube.PlayabilityOK
			sp := &StreamProcessor{db: db, logger: discardLogger{},
				getVideoInfoFn: func(context.Context, string) (*youtube.VideoInfo, error) { return fetched, nil }}
			res, err := sp.Process(context.Background(), job)
			if err != nil || res == nil || !res.ShouldDownload || !res.IsVod {
				t.Fatalf("Process = %+v, %v; want a VOD download", res, err)
			}

			strategy, err := selectDownloadStrategy(res.IsVod, res.VideoInfo)
			if err != nil {
				t.Fatalf("selectDownloadStrategy: %v", err)
			}
			if strategy.Kind() != "manifestless_dash" {
				t.Fatalf("strategy = %s, want manifestless_dash — the whole-file path sets the capture aside and fetches a fragment", strategy.Kind())
			}
			jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging, Logger: discardLogger{}}
			dl, err := strategy.Download(context.Background(), jobCtx, res.VideoInfo, &StrategyDeps{RoutedCipherSolver: stubCipherSolver{}})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if dl.VideoPath != filepath.Join(staging, "video_stream") || dl.AudioPath != filepath.Join(staging, "audio_stream") {
				t.Errorf("downloaders write %s / %s, want the capture's own files", dl.VideoPath, dl.AudioPath)
			}
			entries, err := os.ReadDir(staging)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if !slices.Equal(names, capture) {
				t.Errorf("staging = %v, want the capture untouched: %v", names, capture)
			}
		})
	}
}
