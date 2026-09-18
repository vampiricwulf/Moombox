package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *Muxer) runTwoPassEncode(ctx context.Context, videoPath, audioPath, outputPath string, opts *TrimOptions) error {
	passLogDir, err := os.MkdirTemp("", "moombox-2pass-*")
	if err != nil {
		return fmt.Errorf("create passlog temp dir: %w", err)
	}
	passLogFile := filepath.Join(passLogDir, "passlog")
	defer os.RemoveAll(passLogDir)

	args1, args2 := m.buildTwoPassArgs(videoPath, audioPath, outputPath, passLogFile, opts)

	m.logger.Debug("ffmpeg pass 1", "args", strings.Join(args1, " "))
	if err := m.runFFmpeg(ctx, args1); err != nil {
		return fmt.Errorf("pass 1: %w", err)
	}

	m.logger.Debug("ffmpeg pass 2", "args", strings.Join(args2, " "))
	return m.runFFmpeg(ctx, args2)
}

// buildTwoPassArgs builds both ffmpeg invocations of the ABR two-pass encode.
// Split out of runTwoPassEncode so the argv is assertable without running
// FFmpeg — the same shape buildArgs and buildTrimSegmentArgs already have, and
// what lets one table pin the long-path seam across every builder.
//
// Every path argument goes through ffmpegPathArg (sweep-2 ENGINE-12): this
// encode carries exactly the user-composed output path the archive mux does.
// The pass-log file and os.DevNull are ours, short, and not user-controlled.
func (m *Muxer) buildTwoPassArgs(videoPath, audioPath, outputPath, passLogFile string, opts *TrimOptions) (pass1, pass2 []string) {
	// Pass 1: Analysis
	args1 := []string{"-y"}
	if opts.TrimStartOffset > 0 {
		args1 = append(args1, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	args1 = append(args1, "-i", ffmpegPathArg(videoPath))
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

	// Pass 2: Encode
	args2 := []string{"-y"}
	if opts.TrimStartOffset > 0 {
		args2 = append(args2, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	args2 = append(args2, "-i", ffmpegPathArg(videoPath))
	if audioPath != "" {
		if opts.TrimStartOffset > 0 {
			args2 = append(args2, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
		}
		args2 = append(args2, "-i", ffmpegPathArg(audioPath))
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
	// Pass 2: re-encode audio to maintain sync with re-encoded video
	if audioPath != "" {
		if opts.AudioBitrate > 0 {
			args2 = append(args2, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", opts.AudioBitrate))
		} else {
			args2 = append(args2, "-c:a", "aac")
		}
	}
	args2 = append(args2, "-movflags", "faststart", ffmpegPathArg(outputPath))

	return args1, args2
}
