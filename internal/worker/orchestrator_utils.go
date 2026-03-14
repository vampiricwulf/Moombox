package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// runDownloaders runs video and audio downloaders using errgroup (B7: fixes goroutine leak).
func (o *DownloadOrchestrator) runDownloaders(ctx context.Context, result *DownloadResult) error {
	g, gctx := errgroup.WithContext(ctx)

	if result.VideoDownloader != nil {
		g.Go(func() error {
			return result.VideoDownloader.Start(gctx)
		})
	}

	if result.AudioDownloader != nil {
		g.Go(func() error {
			return result.AudioDownloader.Start(gctx)
		})
	}

	return g.Wait()
}

// ffprobeData holds metadata extracted by ffprobe.
type ffprobeData struct {
	Width       int
	Height      int
	Fps         int
	DurationSec float64
}

// runFFprobe extracts video metadata using ffprobe (B5).
func (o *DownloadOrchestrator) runFFprobe(ctx context.Context, filePath string) *ffprobeData {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, o.muxer.FFprobePath(),
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		filePath,
	)

	out, err := cmd.Output()
	if err != nil {
		o.logger.Debug("ffprobe failed", "err", err)
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
			Duration string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out, &probe); err != nil {
		o.logger.Debug("ffprobe parse failed", "err", err)
		return nil
	}

	data := &ffprobeData{}

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

	return data
}

// parseFpsString parses ffprobe's r_frame_rate format (e.g. "30/1" or "30000/1001").
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
	return int(num / den)
}

// copyFile copies a file from src to dst using streaming I/O.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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

// formatFileSize formats bytes into human-readable string.
var formatFileSize = utils.FormatFileSize

// formatDurationHuman formats a time.Duration into a human-readable string (e.g. "1h 23m", "5m 30s").
var formatDurationHuman = utils.FormatDurationHuman
