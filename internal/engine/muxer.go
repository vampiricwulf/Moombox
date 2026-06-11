package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// TrimOptions configures trimming and encoding during muxing.
type TrimOptions struct {
	TrimStartOffset float64               // Seconds into first segment
	TrimDuration    float64               // Total duration from trim start
	VideoBitrate    int                   // kbps for ABR mode (0 = no target)
	AudioBitrate    int                   // kbps
	CRF             int                   // 0-51, 18 = perceptually lossless (0 = copy)
	UsePreciseTrim  bool                  // Default true
	TwoPass         bool                  // Only for ABR with VideoBitrate
	ProgressFn      func(percent float64) // Optional: called with 0-100 as FFmpeg encodes
}

// Muxer handles FFmpeg operations.
type Muxer struct {
	ffmpegPath  string
	ffprobePath string
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewMuxer creates a new FFmpeg muxer. If ffmpegPath is empty, "ffmpeg" is
// used (resolved via PATH). The ffprobe path is derived from the ffmpeg path.
func NewMuxer(ffmpegPath string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Muxer {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	return &Muxer{
		ffmpegPath:  ffmpegPath,
		ffprobePath: deriveFFprobePath(ffmpegPath),
		logger:      logger,
	}
}

// FFprobePath returns the resolved ffprobe binary path.
func (m *Muxer) FFprobePath() string {
	return m.ffprobePath
}

// deriveFFprobePath returns the ffprobe path corresponding to the given ffmpeg path.
func deriveFFprobePath(ffmpegPath string) string {
	if ffmpegPath == "" || ffmpegPath == "ffmpeg" {
		return "ffprobe"
	}
	dir := filepath.Dir(ffmpegPath)
	base := filepath.Base(ffmpegPath)
	// Case-insensitive match: Windows paths commonly carry arbitrary casing
	// ("FFmpeg.exe"), and a failed replacement would silently point the
	// ffprobe path at the ffmpeg binary itself.
	if idx := strings.Index(strings.ToLower(base), "ffmpeg"); idx >= 0 {
		return filepath.Join(dir, base[:idx]+"ffprobe"+base[idx+len("ffmpeg"):])
	}
	// Base name doesn't contain "ffmpeg" at all (renamed binary): fall back
	// to an ffprobe sibling in the same directory, preserving the extension.
	return filepath.Join(dir, "ffprobe"+filepath.Ext(base))
}

// MuxCopy muxes video and audio streams using codec copy (fast, no re-encoding).
func (m *Muxer) MuxCopy(ctx context.Context, videoPath, audioPath, outputPath string) error {
	return m.Mux(ctx, videoPath, audioPath, outputPath, nil)
}

// Mux performs FFmpeg muxing with optional trimming/encoding.
func (m *Muxer) Mux(ctx context.Context, videoPath, audioPath, outputPath string, opts *TrimOptions) error {
	// Validate inputs
	if videoPath != "" {
		if _, err := os.Stat(videoPath); err != nil {
			return fmt.Errorf("video file not found: %w", err)
		}
	}
	if audioPath != "" {
		if _, err := os.Stat(audioPath); err != nil {
			return fmt.Errorf("audio file not found: %w", err)
		}
	}

	// Ensure output directory exists
	if dir := filepath.Dir(outputPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}

	// Determine strategy
	needsEncode := opts != nil && opts.UsePreciseTrim && hasTrim(opts) && (opts.CRF > 0 || opts.VideoBitrate > 0)
	needsTwoPass := needsEncode && opts.TwoPass && opts.VideoBitrate > 0

	if needsTwoPass {
		err := m.runTwoPassEncode(ctx, videoPath, audioPath, outputPath, opts)
		cleanupFailedMux(ctx, err, outputPath)
		return err
	}

	args := m.buildArgs(videoPath, audioPath, outputPath, opts, needsEncode)

	m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
	var err error
	if opts != nil && opts.ProgressFn != nil && hasTrim(opts) && opts.TrimDuration > 0 {
		err = m.runFFmpegWithProgress(ctx, args, opts.TrimDuration, opts.ProgressFn)
	} else {
		err = m.runFFmpeg(ctx, args)
	}
	cleanupFailedMux(ctx, err, outputPath)
	return err
}

// cleanupFailedMux removes the output file on mux failure, but preserves
// partial output when the failure is due to caller-initiated cancellation.
// Context cancel during mux is almost always a user shutdown — keeping the
// partial file lets the user decide whether to restart the mux or salvage
// what's there. True ffmpeg errors (invalid args, corrupt input, etc.)
// still get cleaned up so we don't leave broken MP4s on disk.
func cleanupFailedMux(ctx context.Context, err error, outputPath string) {
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		// Parent context cancelled — preserve partial output.
		return
	}
	os.Remove(outputPath)
}

func (m *Muxer) buildArgs(videoPath, audioPath, outputPath string, opts *TrimOptions, needsEncode bool) []string {
	args := []string{"-y"} // Overwrite

	// Input args with optional trim
	if opts != nil && opts.TrimStartOffset > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	if videoPath != "" {
		args = append(args, "-i", videoPath)
	}

	if audioPath != "" {
		if opts != nil && opts.TrimStartOffset > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
		}
		args = append(args, "-i", audioPath)
	}

	// Duration limit
	if opts != nil && opts.TrimDuration > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", opts.TrimDuration))
	}

	if needsEncode {
		args = m.appendEncodeArgs(args, audioPath, opts)
	} else {
		// Codec copy
		args = append(args, "-c", "copy")
	}

	// Output options
	args = append(args, "-movflags", "faststart")
	args = append(args, outputPath)

	return args
}

func (m *Muxer) appendEncodeArgs(args []string, audioPath string, opts *TrimOptions) []string {
	if opts.CRF > 0 {
		// CRF mode: re-encode video with constant quality
		args = append(args, "-c:v", "libx264", "-crf", strconv.Itoa(opts.CRF), "-preset", "slow")
		// Re-encode audio to keep sync with re-encoded video
		if opts.AudioBitrate > 0 {
			args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", opts.AudioBitrate))
		} else {
			args = append(args, "-c:a", "aac")
		}
	} else {
		// ABR mode: target bitrate encoding
		if opts.VideoBitrate > 0 {
			args = append(args, "-c:v", "libx264", "-b:v", fmt.Sprintf("%dk", opts.VideoBitrate), "-preset", "fast")
		} else {
			args = append(args, "-c:v", "copy")
		}
		if audioPath != "" && opts.AudioBitrate > 0 {
			args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", opts.AudioBitrate))
		} else if audioPath != "" {
			args = append(args, "-c:a", "copy")
		}
	}

	return args
}

func (m *Muxer) runFFmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)
	cmd.Stdout = nil

	// Capture stderr for error reporting with a 1MB cap.
	// If output exceeds the cap, only the last 512KB is kept.
	stderrBuf := &cappedBuffer{maxSize: 1 << 20, keepSize: 512 << 10}
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
		}
		stderr := stderrBuf.String()
		// Log last 500 chars of stderr for debugging
		if len(stderr) > 500 {
			stderr = stderr[len(stderr)-500:]
		}
		m.logger.Error("ffmpeg failed", "stderr", stderr)
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr)
	}

	return nil
}

func hasTrim(opts *TrimOptions) bool {
	return opts != nil && (opts.TrimStartOffset > 0 || opts.TrimDuration > 0)
}
