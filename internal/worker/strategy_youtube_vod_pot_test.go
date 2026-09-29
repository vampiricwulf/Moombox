package worker

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeVodMint swaps mintGvsPoToken for a fake that returns token and counts
// its calls, restoring the production seam when the test ends.
func fakeVodMint(t *testing.T, token string, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := mintGvsPoToken
	mintGvsPoToken = func(ctx context.Context, p *bgutils.PotProvider, binding string) (string, error) {
		calls.Add(1)
		return token, err
	}
	t.Cleanup(func() { mintGvsPoToken = orig })
	return &calls
}

// vodPotJob builds a JobContext DownloadVod can run against: a real database
// (it stores the chosen video's dimensions), a temp staging dir and a
// capturing logger.
func vodPotJob(t *testing.T) (*JobContext, *captureLogger) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	job := &database.Job{ID: "yt_vodpot", VideoID: "vodpot", URL: "https://www.youtube.com/watch?v=vodpot", Platform: "youtube", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	logs := &captureLogger{}
	return &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: logs}, logs
}

// vodPotInfo is a VideoInfo with one video-only and one audio-only format
// from the given clients. The URLs are plain http:// with no sig cipher and
// no n-param, so resolveFormatURL passes them through the routed solver
// untouched (stubCipherSolver is never asked to decrypt anything).
func vodPotInfo(videoSource, audioSource string) *youtube.VideoInfo {
	w, h, fps := 1280, 720, 30
	return &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		Formats: []youtube.Format{
			{Itag: 302, URL: "http://127.0.0.1:1/videoplayback?itag=302", MimeType: `video/webm; codecs="vp9"`, Bitrate: 2_000_000, Width: &w, Height: &h, Fps: &fps, Source: videoSource},
			{Itag: 251, URL: "http://127.0.0.1:1/videoplayback?itag=251", MimeType: `audio/webm; codecs="opus"`, Bitrate: 160_000, Source: audioSource},
		},
	}
}

// potMintLine returns the args of the "[POT] GVS mint" Info line, or nil.
func potMintLine(logs *captureLogger) map[string]any {
	for _, m := range logs.msgs {
		if m[0] != "[POT] GVS mint" {
			continue
		}
		kv := map[string]any{}
		for i := 1; i+1 < len(m); i += 2 {
			if k, ok := m[i].(string); ok {
				kv[k] = m[i+1]
			}
		}
		return kv
	}
	return nil
}

func runVodPot(t *testing.T, job *JobContext, info *youtube.VideoInfo, pot *bgutils.PotProvider) *DownloadResult {
	t.Helper()
	res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, pot)
	if err != nil {
		t.Fatalf("DownloadVod = %v, want nil", err)
	}
	if res.VideoDownloader == nil || res.AudioDownloader == nil {
		t.Fatalf("want both downloaders, got video=%v audio=%v", res.VideoDownloader, res.AudioDownloader)
	}
	return res
}

// TestDownloadVodAttachesGvsTokenToWebFamily pins the VOD 403 fix
// (2026-09-29): a web_creator format URL 403s its first chunk without the GVS
// PO token, so DownloadVod mints one and hands it to the downloader of every
// stream whose own client requires it — and to no other.
//
// Mutants (each run): dropping PoToken from the video downloader's options
// fails case 1 and case 3 on the video; passing it to the audio downloader
// unconditionally fails case 3 on the tv_auth audio; minting without the
// GvsTokenRequired gate fails case 2's mint count; minting once per stream
// fails case 1's mint count; dropping the potProvider != nil guard fails
// case 4; dropping the sources from the mint line fails cases 1 and 3.
func TestDownloadVodAttachesGvsTokenToWebFamily(t *testing.T) {
	t.Run("web_creator video and audio", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, vodPotInfo("web_creator", "web_creator"), &bgutils.PotProvider{})
		if got := res.VideoDownloader.PoToken(); got != "tok123" {
			t.Errorf("video downloader token = %q, want tok123", got)
		}
		if got := res.AudioDownloader.PoToken(); got != "tok123" {
			t.Errorf("audio downloader token = %q, want tok123", got)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("mint ran %d times, want exactly 1", n)
		}
		line := potMintLine(logs)
		if line == nil {
			t.Fatalf("no [POT] GVS mint line logged: %v", logs.msgs)
		}
		if line["videoSource"] != "web_creator" || line["audioSource"] != "web_creator" {
			t.Errorf("[POT] GVS mint line = %v, want videoSource=web_creator audioSource=web_creator", line)
		}
		if line["tokenLength"] != len("tok123") {
			t.Errorf("[POT] GVS mint tokenLength = %v, want %d", line["tokenLength"], len("tok123"))
		}
	})

	t.Run("visionos video and audio", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, vodPotInfo("visionos", "visionos"), &bgutils.PotProvider{})
		if got := res.VideoDownloader.PoToken(); got != "" {
			t.Errorf("video downloader token = %q, want none", got)
		}
		if got := res.AudioDownloader.PoToken(); got != "" {
			t.Errorf("audio downloader token = %q, want none", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("mint ran %d times, want 0 — no chosen format requires a token", n)
		}
		if line := potMintLine(logs); line != nil {
			t.Errorf("unexpected [POT] GVS mint line: %v", line)
		}
	})

	t.Run("web_creator video beside tv_auth audio", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, vodPotInfo("web_creator", "tv_auth"), &bgutils.PotProvider{})
		if got := res.VideoDownloader.PoToken(); got != "tok123" {
			t.Errorf("video downloader token = %q, want tok123", got)
		}
		if got := res.AudioDownloader.PoToken(); got != "" {
			t.Errorf("audio (tv_auth) downloader token = %q, want none", got)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("mint ran %d times, want exactly 1", n)
		}
		line := potMintLine(logs)
		if line == nil || line["videoSource"] != "web_creator" || line["audioSource"] != "tv_auth" {
			t.Errorf("[POT] GVS mint line = %v, want videoSource=web_creator audioSource=tv_auth", line)
		}
	})

	t.Run("no PotProvider", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, _ := vodPotJob(t)
		res := runVodPot(t, job, vodPotInfo("web_creator", "web_creator"), nil)
		if got := res.VideoDownloader.PoToken(); got != "" {
			t.Errorf("video downloader token = %q, want none", got)
		}
		if got := res.AudioDownloader.PoToken(); got != "" {
			t.Errorf("audio downloader token = %q, want none", got)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("mint ran %d times with a nil PotProvider, want 0", n)
		}
	})
}
