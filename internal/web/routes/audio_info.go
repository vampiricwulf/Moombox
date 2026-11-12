package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/childproc"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// audioProbeTimeout bounds the one ffprobe -show_streams run behind GET
// /api/jobs/{id}/audio-info. The stream list sits in the container header (an
// MP4's moov, which the copy mux writes up front), so a healthy probe answers
// in milliseconds; the bound is for a file on a stalled share, which must
// cost the player its notice and never a hung request.
const audioProbeTimeout = 10 * time.Second

// AudioInfo is GET /api/jobs/{id}/audio-info's answer: the archive's first
// audio stream as ffprobe names it — codec_name ("aac", "opus", "eac3",
// "ac3") and its channel count — or the zero value when there is no audio
// stream, no archive, or the probe failed. The player decides from it
// whether the browser can play the track (owner decision PR-R7).
type AudioInfo struct {
	Codec    string `json:"codec,omitempty"`
	Channels int    `json:"channels,omitempty"`
}

// probeAudioStream is ffprobeAudioStream; a seam so a route test needs no
// FFmpeg. TestProbeAudioStreamIsFFprobe pins the production value.
var probeAudioStream = ffprobeAudioStream

// ffprobeAudioStream reads file's first audio stream.
func ffprobeAudioStream(ctx context.Context, ffprobePath, file string) (AudioInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, audioProbeTimeout)
	defer cancel()
	out, err := childproc.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json",
		"-select_streams", "a:0", "-show_entries", "stream=codec_name,channels", engine.FFmpegPathArg(file)).Output()
	if err != nil {
		return AudioInfo{}, err
	}
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Channels  int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return AudioInfo{}, err
	}
	if len(probe.Streams) == 0 {
		return AudioInfo{}, nil
	}
	return AudioInfo{Codec: probe.Streams[0].CodecName, Channels: probe.Streams[0].Channels}, nil
}

// audioInfoRoute registers GET /api/jobs/{id}/audio-info (owner decision
// PR-R7). Moombox keeps yt-dlp's audio ranking — a 5.1 E-AC-3 track ranks
// above stereo Opus and AAC by its channels (D15, FE-10) — and most Chrome
// and Firefox builds cannot decode E-AC-3 or AC-3, so the dashboard player
// played such an archive in silence with nothing saying why. The player asks
// here which codec the archive's audio is and checks it against the
// browser's canPlayType, and says when the browser cannot play it — the
// file itself is fine. The archive probed is the one the player plays: the
// first part of a split job (its parts share their rendition's codec), else
// the job's single file. Always 200 for a job that exists: a missing file or
// a failed probe answers {}, and the player shows nothing.
func audioInfoRoute(r chi.Router, db *database.Database, store *config.Store, lg interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) {
	// RefuseCrossSite: this GET spawns ffprobe on every call, so another
	// site's page must not be able to fire it.
	r.With(web.RefuseCrossSite).Get("/api/jobs/{id}/audio-info", func(rw http.ResponseWriter, req *http.Request) {
		job, ok := loadJob(rw, db, chi.URLParam(req, "id"))
		if !ok {
			return
		}
		path := ""
		if segs, err := db.GetSegments(job.ID); err == nil && len(segs) > 0 {
			// The containment guard GET /api/jobs/{id}/segments/{index}/video
			// and the chapters route apply to the same part: ffprobe reads
			// the file the player would play, and no other. A part outside
			// the output directory (or gone) is no archive, and answers {}.
			if abs, aerr := filepath.Abs(segs[0].FilePath); aerr == nil && segs[0].FilePath != "" {
				if p, ok := validatePathTraversal(abs, resolveOutputDir(job, store)); ok {
					path = p
				}
			}
		} else if p, _, code := jobVideoPath(job, store); code == 0 {
			path = p
		}
		var info AudioInfo
		if path != "" {
			var ffmpegPath string
			store.Read(func(c *config.MoomboxConfig) { ffmpegPath = c.Paths.FfmpegPath })
			got, err := probeAudioStream(req.Context(), engine.NewMuxer(ffmpegPath, lg).FFprobePath(), path)
			if err != nil {
				lg.Debug("audio-info: probe failed", "job", job.ID, "error", err)
			} else {
				info = got
			}
		}
		// Revalidate, as /video: a re-mux rewrites the file behind the URL.
		rw.Header().Set("Cache-Control", "private, no-cache")
		jsonResponse(rw, info)
	})
}
