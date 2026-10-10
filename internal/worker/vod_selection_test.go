package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

func vodVideo(itag int, mime string, w, h, fps, bitrate int, sig string) youtube.Format {
	return youtube.Format{Itag: itag, MimeType: mime, Width: &w, Height: &h, Fps: &fps, Bitrate: bitrate,
		URL: "https://example.invalid/videoplayback?itag=" + itoa(itag), EncryptedSig: sig}
}

func vodAudio(itag int, mime string, bitrate int, sig string) youtube.Format {
	return youtube.Format{Itag: itag, MimeType: mime, Bitrate: bitrate, AudioQuality: "AUDIO_QUALITY_MEDIUM",
		URL: "https://example.invalid/videoplayback?itag=" + itoa(itag), EncryptedSig: sig}
}

const vodTestPlayerURL = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func vodJob(t *testing.T, maxRes int, prefer60 bool, pref string) *JobContext {
	return &JobContext{
		Job:        &database.Job{ID: "v", VideoID: "v", QualityPreference: pref},
		Config:     &JobConfig{MaxVideoResolution: maxRes, Prefer60fps: prefer60},
		Logger:     discardLogger{},
		StagingDir: t.TempDir(),
	}
}

// The whole-file VOD path read quality_preference only for audio_only, so a
// "720p" channel's uploads and finished VODs downloaded at the global cap. A
// preferred size below the cap now caps the selection, and an explicit
// "…p60" asks for 60 fps whatever prefer_60fps says.
//
// Mutant: vodSelectionBounds ignoring the preference's height, or its fps.
func TestVodSelectionHonoursQualityPreference(t *testing.T) {
	pool := []youtube.Format{
		vodVideo(313, `video/webm; codecs="vp9"`, 3840, 2160, 30, 20_000_000, ""),
		vodVideo(299, `video/mp4; codecs="avc1.64002a"`, 1920, 1080, 60, 6_000_000, ""),
		vodVideo(137, `video/mp4; codecs="avc1.640028"`, 1920, 1080, 30, 4_000_000, ""),
		vodVideo(136, `video/mp4; codecs="avc1.4d401f"`, 1280, 720, 30, 2_000_000, ""),
		vodAudio(140, `audio/mp4; codecs="mp4a.40.2"`, 128_000, ""),
	}
	for _, tc := range []struct {
		pref     string
		prefer60 bool
		want     int
	}{
		{"", true, 313},
		{"720p", true, 136},
		{"1080p", false, 137},
		{"1080p60", false, 299},
		{"1440p", true, 299}, // nothing at 1440: the largest below it
	} {
		_, res := selectVodFormats(vodJob(t, 2160, tc.prefer60, tc.pref), pool, nil)
		if res.VideoFormat == nil || res.VideoFormat.Itag != tc.want {
			t.Errorf("pref=%q prefer60=%v: video %+v, want itag %d", tc.pref, tc.prefer60, res.VideoFormat, tc.want)
		}
	}
}

// When the chosen format's URL will not resolve, the alternate is the job's
// own selection re-run without it. It used to be the highest-bitrate format
// of the same kind — past the cap, past the preference — and it swapped only
// the video, so a progressive primary replaced by a video-only alternate made
// a silent file, and an audio-only job whose audio failed downloaded video.
//
// Mutant: restoring the bitrate pick, or adopting only alt.VideoFormat.
func TestVodReselectionKeepsTheJobsSelection(t *testing.T) {
	ctx := context.Background()

	t.Run("cap", func(t *testing.T) {
		pool := []youtube.Format{
			vodVideo(247, `video/webm; codecs="vp9"`, 1280, 720, 30, 2_500_000, "SIG"), // unresolvable
			vodVideo(136, `video/mp4; codecs="avc1.4d401f"`, 1280, 720, 30, 2_000_000, ""),
			vodVideo(313, `video/webm; codecs="vp9"`, 3840, 2160, 30, 20_000_000, ""),
			vodAudio(140, `audio/mp4; codecs="mp4a.40.2"`, 128_000, ""),
		}
		job := vodJob(t, 720, true, "")
		_, res := selectVodFormats(job, pool, nil)
		if res.VideoFormat == nil || res.VideoFormat.Itag != 247 {
			t.Fatalf("fixture: primary %+v, want 247", res.VideoFormat)
		}
		if _, _, err := resolveVodURLs(ctx, job, res, pool, failingSigSolver{}, nil, vodTestPlayerURL); err != nil {
			t.Fatalf("resolveVodURLs: %v", err)
		}
		if res.VideoFormat.Itag != 136 {
			t.Errorf("alternate %d, want 136 — the 720 cap's next choice, not the 2160 one", res.VideoFormat.Itag)
		}
	})

	t.Run("progressive to adaptive", func(t *testing.T) {
		prog := vodVideo(18, `video/mp4; codecs="avc1.42001E, mp4a.40.2"`, 640, 360, 30, 500_000, "SIG")
		prog.AudioQuality = "AUDIO_QUALITY_LOW"
		pool := []youtube.Format{
			prog,
			vodVideo(134, `video/mp4; codecs="avc1.4d401e"`, 640, 360, 30, 400_000, ""),
			vodAudio(140, `audio/mp4; codecs="mp4a.40.2"`, 128_000, ""),
		}
		job := vodJob(t, 360, true, "")
		res := &DownloadResult{HasVideo: true, VideoFormat: &pool[0], VideoPath: "video.mp4", HasAudio: true}
		_, audioURL, err := resolveVodURLs(ctx, job, res, pool, failingSigSolver{}, nil, vodTestPlayerURL)
		if err != nil {
			t.Fatalf("resolveVodURLs: %v", err)
		}
		if res.VideoFormat.Itag != 134 || res.AudioFormat == nil || res.AudioFormat.Itag != 140 || audioURL == "" {
			t.Errorf("a progressive primary replaced by a video-only alternate kept no separate audio: video %+v audio %+v url %q",
				res.VideoFormat, res.AudioFormat, audioURL)
		}
	})

	t.Run("audio only", func(t *testing.T) {
		pool := []youtube.Format{
			vodAudio(251, `audio/webm; codecs="opus"`, 160_000, "SIG"),
			vodAudio(140, `audio/mp4; codecs="mp4a.40.2"`, 128_000, ""),
			vodVideo(137, `video/mp4; codecs="avc1.640028"`, 1920, 1080, 30, 4_000_000, ""),
		}
		job := vodJob(t, 2160, true, "audio_only")
		_, res := selectVodFormats(job, pool, nil)
		if res.VideoFormat == nil || res.VideoFormat.Itag != 251 {
			t.Fatalf("fixture: primary %+v, want the opus audio", res.VideoFormat)
		}
		if _, _, err := resolveVodURLs(ctx, job, res, pool, failingSigSolver{}, nil, vodTestPlayerURL); err != nil {
			t.Fatalf("resolveVodURLs: %v", err)
		}
		if res.VideoFormat.Itag != 140 {
			t.Errorf("an audio-only job's alternate is itag %d, want the other audio (140)", res.VideoFormat.Itag)
		}
	})
}
