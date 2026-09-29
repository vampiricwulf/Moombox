package worker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// vodMixedInfo is a VideoInfo where the best video and the best audio come
// from web_creator (token required) and a lesser pair from tv_public (no
// requirement): web_creator 303 1080p + tv_public 247 720p video, web_creator
// 251 160k + tv_public 250 70k opus audio.
func vodMixedInfo() *youtube.VideoInfo {
	w1080, h1080, w720, h720, fps := 1920, 1080, 1280, 720, 30
	return &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		Formats: []youtube.Format{
			{Itag: 303, URL: "http://127.0.0.1:1/videoplayback?itag=303", MimeType: `video/webm; codecs="vp9"`, Bitrate: 4_000_000, Width: &w1080, Height: &h1080, Fps: &fps, Source: "web_creator"},
			{Itag: 247, URL: "http://127.0.0.1:1/videoplayback?itag=247", MimeType: `video/webm; codecs="vp9"`, Bitrate: 1_500_000, Width: &w720, Height: &h720, Fps: &fps, Source: "tv_public"},
			{Itag: 251, URL: "http://127.0.0.1:1/videoplayback?itag=251", MimeType: `audio/webm; codecs="opus"`, Bitrate: 160_000, Source: "web_creator"},
			{Itag: 250, URL: "http://127.0.0.1:1/videoplayback?itag=250", MimeType: `audio/webm; codecs="opus"`, Bitrate: 70_000, Source: "tv_public"},
		},
	}
}

// failingSigSolver fails every decryption, so a format carrying EncryptedSig
// fails its resolve while a plain URL (nothing to solve) still passes.
type failingSigSolver struct{}

func (failingSigSolver) Sig(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("sig solve failed")
}
func (failingSigSolver) N(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("n solve failed")
}
func (failingSigSolver) Batch(_ context.Context, _ string, _, _ []string) (map[string]string, map[string]string, error) {
	return nil, nil, errors.New("batch solve failed")
}

// logLine returns the args of the first log line whose message starts with
// prefix, as a key/value map (with the message under ""), or nil.
func logLine(logs *captureLogger, prefix string) map[string]any {
	for _, m := range logs.msgs {
		msg, _ := m[0].(string)
		if !strings.HasPrefix(msg, prefix) {
			continue
		}
		kv := map[string]any{"": msg}
		for i := 1; i+1 < len(m); i += 2 {
			if k, ok := m[i].(string); ok {
				kv[k] = m[i+1]
			}
		}
		return kv
	}
	return nil
}

// TestDownloadVodMissingPotDegrades pins yt-dlp's missing_pot behaviour on
// the VOD path: when a chosen format needs a GVS PO token and none can be
// minted, the WEB-family formats leave the pool and the selection re-runs, so
// the job downloads the token-free TV/visionos URLs instead of 403ing.
//
// Mutants (each run): skipping the re-selection after a failed mint fails
// case 5 (the web_creator pair is still chosen); filtering the caller's
// videoInfo.Formats in place (or reassigning it) fails case 5's copy
// assertions; dropping the manual-itag fallback warning fails case 7;
// handing the post-degrade resolve the caller's list instead of the filtered
// pool fails the cipher re-selection case (the alternate becomes the dropped
// web_creator 303).
func TestDownloadVodMissingPotDegrades(t *testing.T) {
	mintErr := errors.New("sidecar down")

	for _, tc := range []struct {
		name  string
		token string
		err   error
	}{
		{"mint error", "", mintErr},
		{"empty token", "", nil},
	} {
		t.Run("degrades to tv_public on "+tc.name, func(t *testing.T) {
			calls := fakeVodMint(t, tc.token, tc.err)
			job, logs := vodPotJob(t)
			info := vodMixedInfo()
			before := append([]youtube.Format(nil), info.Formats...)
			res := runVodPot(t, job, info, &bgutils.PotProvider{})
			if res.VideoFormat == nil || res.VideoFormat.Itag != 247 || res.VideoFormat.Source != "tv_public" {
				t.Errorf("video format = %+v, want tv_public itag 247", res.VideoFormat)
			}
			if res.AudioFormat == nil || res.AudioFormat.Itag != 250 || res.AudioFormat.Source != "tv_public" {
				t.Errorf("audio format = %+v, want tv_public itag 250", res.AudioFormat)
			}
			if got := res.VideoDownloader.PoToken(); got != "" {
				t.Errorf("video downloader token = %q, want none after the degrade", got)
			}
			if got := res.AudioDownloader.PoToken(); got != "" {
				t.Errorf("audio downloader token = %q, want none after the degrade", got)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("mint ran %d times, want exactly 1", n)
			}
			if n := len(info.Formats); n != 4 {
				t.Errorf("caller's videoInfo.Formats has %d entries after the degrade, want 4 — the filter must work on a copy", n)
			}
			for i := range before {
				if i < len(info.Formats) && (info.Formats[i].Itag != before[i].Itag || info.Formats[i].Source != before[i].Source) {
					t.Errorf("caller's videoInfo.Formats[%d] = itag %d %s after the degrade, want itag %d %s — the filter must work on a copy",
						i, info.Formats[i].Itag, info.Formats[i].Source, before[i].Itag, before[i].Source)
				}
			}
			line := logLine(logs, "[POT] missing_pot")
			if line == nil {
				t.Fatalf("no [POT] missing_pot line logged: %v", logs.msgs)
			}
			if line["dropped"] != 2 {
				t.Errorf("[POT] missing_pot dropped = %v, want 2 (line %v)", line["dropped"], line)
			}
			if _, ok := line["err"]; !ok {
				t.Errorf("[POT] missing_pot line carries no err: %v", line)
			}
			if l := potMintLine(logs); l != nil {
				t.Errorf("unexpected [POT] GVS mint line: %v", l)
			}
		})
	}

	t.Run("a cipher re-selection after the degrade stays in the filtered pool", func(t *testing.T) {
		// The degraded pick (tv_public 247) fails its sig resolve; the alternate
		// must come from the filtered pool (tv_public 244), not the caller's
		// list, whose highest-bitrate video is the dropped web_creator 303.
		fakeVodMint(t, "", mintErr)
		job, _ := vodPotJob(t)
		info := vodMixedInfo()
		info.Formats[1].EncryptedSig = "broken"
		w, h, fps := 854, 480, 30
		info.Formats = append(info.Formats, youtube.Format{Itag: 244, URL: "http://127.0.0.1:1/videoplayback?itag=244", MimeType: `video/webm; codecs="vp9"`, Bitrate: 800_000, Width: &w, Height: &h, Fps: &fps, Source: "tv_public"})
		res, err := DownloadVod(context.Background(), job, info, failingSigSolver{}, nil, &bgutils.PotProvider{})
		if err != nil {
			t.Fatalf("DownloadVod = %v, want nil", err)
		}
		if res.VideoFormat == nil || res.VideoFormat.Itag != 244 {
			t.Errorf("video format = %+v, want the filtered-pool alternate tv_public 244", res.VideoFormat)
		}
		if got := res.VideoDownloader.PoToken(); got != "" {
			t.Errorf("video downloader token = %q, want none", got)
		}
	})

	t.Run("only web-family formats is an error naming the GVS token", func(t *testing.T) {
		fakeVodMint(t, "", mintErr)
		job, _ := vodPotJob(t)
		res, err := DownloadVod(context.Background(), job, vodPotInfo("web_creator", "web_creator"), stubCipherSolver{}, nil, &bgutils.PotProvider{})
		if err == nil {
			t.Fatalf("DownloadVod = %+v, nil — want an error", res)
		}
		if !errors.Is(err, mintErr) {
			t.Errorf("error %q does not wrap the mint error", err)
		}
		if !strings.Contains(err.Error(), "GVS PO token") {
			t.Errorf("error %q does not name the GVS PO token", err)
		}
	})

	t.Run("manual itag on a dropped format falls back to auto", func(t *testing.T) {
		fakeVodMint(t, "", mintErr)
		job, logs := vodPotJob(t)
		itag := 303
		job.Job.SelectedVideoItag = &itag
		res := runVodPot(t, job, vodMixedInfo(), &bgutils.PotProvider{})
		if res.VideoFormat == nil || res.VideoFormat.Itag != 247 {
			t.Errorf("video format = %+v, want the auto tv_public itag 247", res.VideoFormat)
		}
		line := logLine(logs, "[FormatSelector] Manual video itag 303 requires a GVS token")
		if line == nil {
			t.Errorf("no manual-itag GVS fallback warning: %v", logs.msgs)
		}
		if l := logLine(logs, "[FormatSelector] Manual video itag 303 not found"); l != nil {
			t.Errorf("the generic not-found warning fired instead of (or beside) the GVS one: %v", l)
		}
	})

	t.Run("a minted token keeps the web-family selection", func(t *testing.T) {
		calls := fakeVodMint(t, "tok123", nil)
		job, logs := vodPotJob(t)
		res := runVodPot(t, job, vodMixedInfo(), &bgutils.PotProvider{})
		if res.VideoFormat == nil || res.VideoFormat.Itag != 303 || res.AudioFormat == nil || res.AudioFormat.Itag != 251 {
			t.Errorf("formats = %+v / %+v, want the web_creator pair 303 + 251", res.VideoFormat, res.AudioFormat)
		}
		if res.VideoDownloader.PoToken() != "tok123" || res.AudioDownloader.PoToken() != "tok123" {
			t.Errorf("tokens = %q / %q, want tok123 on both", res.VideoDownloader.PoToken(), res.AudioDownloader.PoToken())
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("mint ran %d times, want exactly 1", n)
		}
		if l := logLine(logs, "[POT] missing_pot"); l != nil {
			t.Errorf("unexpected missing_pot line on a successful mint: %v", l)
		}
	})
}
