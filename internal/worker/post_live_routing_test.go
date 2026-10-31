package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
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

func withLength(f *youtube.Format)     { f.ContentLength = "1048576" }
func asOTF(f *youtube.Format)          { f.IsOTF = true }
func asLiveAdaptive(f *youtube.Format) { f.TargetDurationSec = 2 }

const (
	h264    = `video/mp4; codecs="avc1.640028"`
	aac     = `audio/mp4; codecs="mp4a.40.2"`
	muxed18 = `video/mp4; codecs="avc1.42001E, mp4a.40.2"`
)

// TestDownloadVodLeavesSegmentedFormatsOut is G24's whole-file half. yt-dlp's
// https pass skips an OTF format and a live adaptive one (_video.py:
// 3492-3507), whose URL answers a whole-file GET with one fragment; the VOD
// selector looked at mimeType and URL alone and picked the OTF 1080p over the
// 720p file. The whole-file pool leaves both kinds out, the selection runs
// over what it can fetch whole — itag 18 included, as upstream would — and a
// stream the job wants that YouTube served segmented alone fails clearly
// rather than archiving the rest silently: an OTF video left an audio-only
// "archive" of a video job.
//
// Mutants (each run): the pool unfiltered (the 1080p and itag 18 rows pick
// the segmented 137, and the loss rows find nothing lost); the loss check
// dropped (every row: the loss rows download what is left, and no line names
// the formats left out); the loss check blind to the job's preference (the
// audio_only row fails); a progressive pick's audio counted as lost (the
// itag 18 row fails).
func TestDownloadVodLeavesSegmentedFormatsOut(t *testing.T) {
	audio := vodFormat(140, 0, 0, aac, withLength)
	for _, tc := range []struct {
		name      string
		pref      string
		formats   []youtube.Format
		wantVideo int    // the chosen video itag, 0 for none
		wantErr   string // a substring of the failure, "" for none
	}{
		{"an OTF 1080p gives way to the 720p file", "",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), vodFormat(136, 1280, 720, h264, withLength), audio}, 136, ""},
		{"a live adaptive 1080p gives way to the 720p file", "",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asLiveAdaptive), vodFormat(136, 1280, 720, h264, withLength), audio}, 136, ""},
		{"OTF adaptive video beside a whole muxed 18 takes 18", "",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), vodFormat(18, 640, 360, muxed18, withLength, func(f *youtube.Format) { f.AudioQuality = "AUDIO_QUALITY_LOW" }), vodFormat(140, 0, 0, aac, asOTF)}, 18, ""},
		{"video served segmented alone fails clearly", "",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), vodFormat(136, 1280, 720, h264, asOTF), audio}, 0, "this video's video only as segments"},
		{"video and audio served segmented alone fail clearly", "",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), vodFormat(140, 0, 0, aac, asOTF)}, 0, "this video's video and audio only as segments"},
		{"an audio_only job loses nothing it wants", "audio_only",
			[]youtube.Format{vodFormat(137, 1920, 1080, h264, asOTF), audio}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, logs := vodPotJob(t)
			job.Job.QualityPreference = tc.pref
			info := segmentedVodInfo(tc.formats...)
			before := slices.Clone(info.Formats)
			res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil)
			if !slices.EqualFunc(before, info.Formats, func(a, b youtube.Format) bool { return a.Itag == b.Itag && a.IsOTF == b.IsOTF }) {
				t.Error("the extraction's own format list was filtered in place")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("DownloadVod err = %v, want one saying %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DownloadVod: %v", err)
			}
			got := 0
			if res.VideoFormat != nil && res.VideoFormat.IsVideo() {
				got = res.VideoFormat.Itag
			}
			if got != tc.wantVideo {
				t.Errorf("video itag = %d, want %d", got, tc.wantVideo)
			}
			if logLine(logs, "VOD: left out formats YouTube serves only as segments") == nil {
				t.Errorf("no line names the formats left out: %v", logs.lines())
			}
		})
	}
}

// TestVodMissingPotSwapNeverLandsOnASegmentedCopy: a missing_pot degrade
// swaps a token-requiring format for its token-free shadow, a copy from
// another client's response — which can be one a whole-file GET cannot
// fetch. It leaves the pool as the extraction's own segmented copies do, so
// the selection takes the next file instead of the shadow's fragment.
//
// Mutant: the degraded pool not filtered again (the 1080p segmented shadow
// is chosen).
func TestVodMissingPotSwapNeverLandsOnASegmentedCopy(t *testing.T) {
	fakeVodMint(t, "", errors.New("sidecar down"))
	shadow := vodFormat(137, 1920, 1080, h264, asLiveAdaptive)
	winner := vodFormat(137, 1920, 1080, h264, withLength, func(f *youtube.Format) { f.Source = "watch_page"; f.TokenFreeAlternate = &shadow })
	info := segmentedVodInfo(winner, vodFormat(136, 1280, 720, h264, withLength), vodFormat(140, 0, 0, aac, withLength))
	job, _ := vodPotJob(t)
	res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadVod: %v", err)
	}
	if res.VideoFormat == nil || res.VideoFormat.Itag != 136 || res.VideoFormat.Segmented() {
		t.Errorf("video = %+v, want the 720p file — never the segmented shadow", res.VideoFormat)
	}
}

// TestPartitionManifestlessFormatsTakesOnlyAudioLessVideo mirrors the gate
// (youtube.HasSplitAdaptiveFormats): a muxed itag 18 listed without a
// contentLength is the progressive file, not a video half, and the &sq loop
// must never be handed it — under a 360p cap it out-bandwidths 134.
//
// Mutant: the partition's video test back to Width > 0 (18 is in the pool).
func TestPartitionManifestlessFormatsTakesOnlyAudioLessVideo(t *testing.T) {
	video, audio := partitionManifestlessFormats([]youtube.Format{
		vodFormat(18, 640, 360, muxed18),
		vodFormat(134, 640, 360, `video/mp4; codecs="avc1.4d401e"`),
		vodFormat(140, 0, 0, aac),
	})
	if len(video) != 1 || video[0].Itag != 134 {
		t.Errorf("video pool = %+v, want 134 alone", video)
	}
	if len(audio) != 1 || audio[0].Itag != 140 {
		t.Errorf("audio pool = %+v, want 140 alone", audio)
	}
}

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
