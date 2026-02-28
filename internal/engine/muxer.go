package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const muxTimeout = 10 * time.Minute

// TrimOptions configures trimming and encoding during muxing.
type TrimOptions struct {
	TrimStartOffset float64 // Seconds into first segment
	TrimDuration    float64 // Total duration from trim start
	VideoBitrate    int     // kbps for ABR mode (0 = no target)
	AudioBitrate    int     // kbps
	CRF             int     // 0-51, 18 = perceptually lossless (0 = copy)
	UsePreciseTrim  bool    // Default true
	TwoPass         bool    // Only for ABR with VideoBitrate
}

// Muxer handles FFmpeg operations.
type Muxer struct {
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewMuxer creates a new FFmpeg muxer.
func NewMuxer(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Muxer {
	return &Muxer{logger: logger}
}

// MuxCopy muxes video and audio streams using codec copy (fast, no re-encoding).
func (m *Muxer) MuxCopy(ctx context.Context, videoPath, audioPath, outputPath string) error {
	return m.Mux(ctx, videoPath, audioPath, outputPath, nil)
}

// MuxEncode muxes with CRF re-encoding for precise trimming.
func (m *Muxer) MuxEncode(ctx context.Context, videoPath, audioPath, outputPath string, crf int) error {
	return m.Mux(ctx, videoPath, audioPath, outputPath, &TrimOptions{CRF: crf, UsePreciseTrim: true})
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
		return m.runTwoPassEncode(ctx, videoPath, audioPath, outputPath, opts)
	}

	args := m.buildArgs(videoPath, audioPath, outputPath, opts, needsEncode)

	m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
	return m.runFFmpeg(ctx, args)
}

// Trim creates a trimmed version of a file.
func (m *Muxer) Trim(ctx context.Context, inputPath, outputPath string, startTime, endTime float64, crf int) error {
	duration := endTime - startTime
	opts := &TrimOptions{
		TrimStartOffset: startTime,
		TrimDuration:    duration,
		CRF:             crf,
		UsePreciseTrim:  true,
	}
	return m.Mux(ctx, inputPath, "", outputPath, opts)
}

// TrimWithAudio creates a trimmed version with a specific audio bitrate (matches TS probeAudioBitrate flow).
func (m *Muxer) TrimWithAudio(ctx context.Context, inputPath, outputPath string, startTime, endTime float64, crf int, audioBitrate int) error {
	duration := endTime - startTime
	opts := &TrimOptions{
		TrimStartOffset: startTime,
		TrimDuration:    duration,
		CRF:             crf,
		AudioBitrate:    audioBitrate,
		UsePreciseTrim:  true,
	}
	return m.Mux(ctx, inputPath, "", outputPath, opts)
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
		args = append(args, "-c:v", "libx264", "-crf", fmt.Sprintf("%d", opts.CRF), "-preset", "slow")
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

func (m *Muxer) runTwoPassEncode(ctx context.Context, videoPath, audioPath, outputPath string, opts *TrimOptions) error {
	passLogFile := filepath.Join(os.TempDir(), fmt.Sprintf("moombox-2pass-%d", time.Now().UnixMilli()))
	defer func() {
		// Clean up passlog files
		os.Remove(passLogFile + "-0.log")
		os.Remove(passLogFile + "-0.log.mbtree")
	}()

	// Pass 1: Analysis
	args1 := []string{"-y"}
	if opts.TrimStartOffset > 0 {
		args1 = append(args1, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	args1 = append(args1, "-i", videoPath)
	if opts.TrimDuration > 0 {
		args1 = append(args1, "-t", fmt.Sprintf("%.3f", opts.TrimDuration))
	}
	args1 = append(args1,
		"-c:v", "libx264",
		"-b:v", fmt.Sprintf("%dk", opts.VideoBitrate),
		"-preset", "fast",
		"-pass", "1",
		"-passlogfile", passLogFile,
		"-an",
		"-f", "null",
	)
	args1 = append(args1, os.DevNull)

	m.logger.Debug("ffmpeg pass 1", "args", strings.Join(args1, " "))
	if err := m.runFFmpeg(ctx, args1); err != nil {
		return fmt.Errorf("pass 1: %w", err)
	}

	// Pass 2: Encode
	args2 := []string{"-y"}
	if opts.TrimStartOffset > 0 {
		args2 = append(args2, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	args2 = append(args2, "-i", videoPath)
	if audioPath != "" {
		if opts.TrimStartOffset > 0 {
			args2 = append(args2, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
		}
		args2 = append(args2, "-i", audioPath)
	}
	if opts.TrimDuration > 0 {
		args2 = append(args2, "-t", fmt.Sprintf("%.3f", opts.TrimDuration))
	}
	args2 = append(args2,
		"-c:v", "libx264",
		"-b:v", fmt.Sprintf("%dk", opts.VideoBitrate),
		"-preset", "fast",
		"-pass", "2",
		"-passlogfile", passLogFile,
	)
	if audioPath != "" {
		if opts.AudioBitrate > 0 {
			args2 = append(args2, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", opts.AudioBitrate))
		} else {
			args2 = append(args2, "-c:a", "copy")
		}
	}
	args2 = append(args2, "-movflags", "faststart", outputPath)

	m.logger.Debug("ffmpeg pass 2", "args", strings.Join(args2, " "))
	return m.runFFmpeg(ctx, args2)
}

func (m *Muxer) runFFmpeg(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = nil

	// Capture stderr for error reporting
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg timed out: %w", ctx.Err())
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
