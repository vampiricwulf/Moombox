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
	// Pass 2: re-encode audio to maintain sync with re-encoded video
	if audioPath != "" {
		if opts.AudioBitrate > 0 {
			args2 = append(args2, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", opts.AudioBitrate))
		} else {
			args2 = append(args2, "-c:a", "aac")
		}
	}
	args2 = append(args2, "-movflags", "faststart", outputPath)

	m.logger.Debug("ffmpeg pass 2", "args", strings.Join(args2, " "))
	return m.runFFmpeg(ctx, args2)
}
