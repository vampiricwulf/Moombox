package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// runDownloaders runs video and audio downloaders using errgroup (B7: fixes goroutine leak).
//
// Each downloader recovers its own panic into an error. The callers' recovers
// cannot: a panic in one of these goroutines is not theirs to catch, and it
// took the whole process down — every other job's capture with it — where
// the live loop's "runDownloaders panic" error was meant to end this one.
func (o *DownloadOrchestrator) runDownloaders(ctx context.Context, result *DownloadResult) error {
	g, gctx := errgroup.WithContext(ctx)

	start := func(name string, d *engine.SegmentDownloader) {
		g.Go(func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%s downloader panic: %v", name, r)
				}
			}()
			return d.Start(gctx)
		})
	}
	if result.VideoDownloader != nil {
		start("video", result.VideoDownloader)
	}
	if result.AudioDownloader != nil {
		start("audio", result.AudioDownloader)
	}

	return g.Wait()
}

// ffprobeData holds metadata extracted by ffprobe.
type ffprobeData struct {
	Width       int
	Height      int
	Fps         int
	DurationSec float64
	// StartSec is format.start_time: where on its own timeline the file's
	// first packet sits. Zero for a recording captured from the start of a
	// broadcast and for every output FFmpeg writes; the position in the
	// broadcast for a part that began after a split or a restart.
	StartSec float64
	// FormatName is the demuxer ffprobe opened the file with — its
	// comma-separated name list ("mov,mp4,m4a,3gp,3g2,mj2", "mpegts").
	FormatName string
}

// spanSec is the length of media a `-c copy` of this file carries across —
// what the copy's own duration will read, since FFmpeg rebases the copy's
// timeline to zero. It is NOT always DurationSec: the mov demuxer reports a
// fragmented MP4's duration as the END timestamp of its last fragment, start
// included, so a DASH part whose first fragment sits at 208 s of the broadcast
// probes as start_time=208, duration=7876 while holding 7668 s of media. The
// shortfall check (verifyMuxedDuration) read that as a copy 208 s short and
// rejected every later part of a split, and every recording that began past
// sequence zero, leaving its staging unmuxed with advice to re-run a mux that
// failed the same way.
//
// Only a mov container is corrected, because the quirk is the mov demuxer's:
// the MPEG-TS demuxer (Twitch, YouTube HLS) estimates duration from the
// packets as last minus first, so its duration already IS the span and its
// start_time — hours into a broadcast for a Twitch capture — must not be
// taken off it. A negative start (AAC priming, an edit list) is left alone.
func (p *ffprobeData) spanSec() float64 {
	if p == nil {
		return 0
	}
	if p.StartSec > 0 && isMovContainer(p.FormatName) {
		return p.DurationSec - p.StartSec
	}
	return p.DurationSec
}

// isMovContainer reports whether an ffprobe format_name names the mov
// demuxer — the one whose fragmented-file duration includes the start time.
// Matched entry by entry rather than on the whole list, so an FFmpeg build
// that orders or extends the aliases differently still matches.
func isMovContainer(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		switch strings.TrimSpace(name) {
		case "mov", "mp4", "m4a", "3gp", "3g2", "mj2":
			return true
		}
	}
	return false
}

// runFFprobe extracts video metadata using ffprobe (B5).
func (o *DownloadOrchestrator) runFFprobe(ctx context.Context, filePath string) *ffprobeData {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, o.mux().FFprobePath(),
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		filePath,
	)

	out, err := cmd.Output()
	if err != nil {
		// Promote from Debug → Warn: ffprobe failure usually means a bad
		// FFmpeg install or a truly malformed file — falling back to format
		// metadata silently used to hide both (audit worker.md Finding 13).
		o.logger.Warn("ffprobe failed; falling back to format metadata", "err", err)
		return nil
	}

	var probe struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
			StartTime  string `json:"start_time"`
			Duration   string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out, &probe); err != nil {
		o.logger.Debug("ffprobe parse failed", "err", err)
		return nil
	}

	data := &ffprobeData{FormatName: probe.Format.FormatName}

	// Find video stream
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			data.Width = s.Width
			data.Height = s.Height
			if s.RFrameRate != "" {
				data.Fps = parseFpsString(s.RFrameRate)
			}
			break
		}
	}

	// Duration
	if probe.Format.Duration != "" {
		data.DurationSec, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	}
	if probe.Format.StartTime != "" {
		data.StartSec, _ = strconv.ParseFloat(probe.Format.StartTime, 64)
	}

	return data
}

// parseFpsString parses ffprobe's r_frame_rate format (e.g. "30/1" or "30000/1001").
// NTSC rates round to the nominal figure — "60000/1001" is 60, not 59 — so
// the label FormatQualityLabel builds from it reads "1080p60" for the same
// content Twitch's own variant naming (internal/twitch/hls.go) and the DASH
// manifest parser (internal/engine/manifest.go, math.Round) already call 60.
func parseFpsString(fps string) int {
	numStr, denStr, ok := strings.Cut(fps, "/")
	if !ok {
		v, _ := strconv.Atoi(fps)
		return v
	}
	num, _ := strconv.ParseFloat(numStr, 64)
	den, _ := strconv.ParseFloat(denStr, 64)
	if den == 0 {
		return 0
	}
	return int(math.Round(num / den))
}

// copyFile copies a file from src to dst using streaming I/O, through a
// uniquely named temp file beside dst and utils.ReplaceFile — the shape
// utils.WriteFileAtomic gives every other output write in this package.
//
// It used to os.Create(dst) and stream into it, which left a TRUNCATED dst
// behind on any failure; copyKeptChatSidecar skips the copy when dst already
// exists, so one failed copy left a corrupt chat file beside the archive for
// good. Now dst either keeps what it had or holds the whole of src, and a
// failure leaves no temp behind. The POSIX mode is exactly 0644 (the chmod
// WriteFileAtomic applies) rather than 0666 masked by the process umask —
// the same one deliberate difference writeDescriptionAtomic documents.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := utils.ReplaceFile(tmpPath, dst); err != nil {
		return err
	}
	done = true
	return nil
}

// extractQualityFromResult derives QualityInfo from a DownloadResult.
// Prefers VideoFormat (set by VOD strategy) but falls back to the direct
// VideoWidth/VideoHeight fields (set by DASH and HLS strategies).
func (o *DownloadOrchestrator) extractQualityFromResult(result *DownloadResult) QualityInfo {
	qi := QualityInfo{Label: "unknown"}
	if result.VideoFormat != nil {
		if result.VideoFormat.Width != nil {
			qi.Width = *result.VideoFormat.Width
		}
		if result.VideoFormat.Height != nil {
			qi.Height = *result.VideoFormat.Height
		}
		if result.VideoFormat.Fps != nil {
			qi.FPS = *result.VideoFormat.Fps
		}
	}
	// Fallback to direct fields (DASH/HLS strategies set these, not VideoFormat)
	if qi.Width == 0 && result.VideoWidth > 0 {
		qi.Width = result.VideoWidth
	}
	if qi.Height == 0 && result.VideoHeight > 0 {
		qi.Height = result.VideoHeight
	}
	if qi.FPS == 0 && result.VideoFps > 0 {
		qi.FPS = result.VideoFps
	}
	if qi.Height > 0 {
		qi.Label = FormatQualityLabel(qi.Height, qi.FPS)
	}
	return qi
}

// computeStreamEndFallback computes a stream end time when YouTube/Twitch didn't
// provide one. Prefers start_time + length_seconds (accurate for VODs/premieres),
// falling back to time.Now() for live streams where neither is available.
func computeStreamEndFallback(job *database.Job) string {
	if job.StreamStartTime != "" && job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		if start, err := time.Parse(time.RFC3339, job.StreamStartTime); err == nil {
			return start.Add(time.Duration(*job.LengthSeconds) * time.Second).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// formatDurationHuman formats a time.Duration into a human-readable string (e.g. "1h 23m", "5m 30s").
var formatDurationHuman = utils.FormatDurationHuman
