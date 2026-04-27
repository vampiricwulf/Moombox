package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CleanupOldTrimTempDirs removes leftover `moombox-trim-*` and
// `moombox-2pass-*` directories from %TEMP% that are older than 24h.
// These are normally cleaned up by `defer os.RemoveAll(tempDir)` inside
// the trim path, but a hard process abort (panic in a sibling
// goroutine, OS kill, power loss) bypasses the defer and leaves the
// dirs orphaned. Windows eventually reclaims %TEMP% but on a
// long-running install they accumulate. Safe to call at startup; uses
// 24h age threshold so a concurrent trim's in-flight tempdir is never
// touched.
//
// Errors are logged-and-continue at the caller; this is housekeeping,
// not a critical path.
func CleanupOldTrimTempDirs() (removed int, err error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "moombox-trim-") && !strings.HasPrefix(name, "moombox-2pass-") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(os.TempDir(), name)
		if rmErr := os.RemoveAll(full); rmErr == nil {
			removed++
		}
	}
	return removed, nil
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

// TrimWithAudio creates a trimmed version with a specific audio bitrate.
// Caller probes the source audio bitrate first so the re-encode preserves
// fidelity instead of dropping to FFmpeg's default 128 kbps.
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

// TrimSegmentInput describes one segment for the TrimAndConcat pipeline.
type TrimSegmentInput struct {
	InputPath string
	StartTime float64 // local start time within this segment
	Duration  float64 // duration to extract from this segment
	NeedScale bool    // true if resolution/FPS differs from target
}

// TrimAndConcat trims multiple segment files, optionally scales to a common
// resolution/FPS, and concatenates them into a single output file.
// Used for cross-segment trims on multi-segment quality-split jobs.
func (m *Muxer) TrimAndConcat(ctx context.Context, segments []TrimSegmentInput, outputPath string, targetWidth, targetHeight, targetFPS, crf, audioBitrate int) error {
	if len(segments) == 0 {
		return fmt.Errorf("no segments to trim")
	}

	// Single segment: trim directly to output (no concat needed)
	if len(segments) == 1 {
		seg := segments[0]
		return m.TrimWithAudio(ctx, seg.InputPath, outputPath, seg.StartTime, seg.StartTime+seg.Duration, crf, audioBitrate)
	}

	// Multi-segment: trim each to intermediate, then concat
	tempDir, err := os.MkdirTemp("", "moombox-trim-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var intermediates []string

	for i, seg := range segments {
		intermediatePath := filepath.Join(tempDir, fmt.Sprintf("seg_%d.mp4", i))
		intermediates = append(intermediates, intermediatePath)

		args := m.buildTrimSegmentArgs(seg, intermediatePath, targetWidth, targetHeight, targetFPS, crf, audioBitrate)

		m.logger.Debug("ffmpeg trim segment", "index", i, "args", strings.Join(args, " "))
		if err := m.runFFmpeg(ctx, args); err != nil {
			return fmt.Errorf("trim segment %d: %w", i, err)
		}
	}

	return m.concatIntermediates(ctx, intermediates, outputPath, tempDir)
}

// buildTrimSegmentArgs builds FFmpeg arguments for trimming a single segment.
func (m *Muxer) buildTrimSegmentArgs(seg TrimSegmentInput, outputPath string, targetWidth, targetHeight, targetFPS, crf, audioBitrate int) []string {
	args := []string{"-y"}

	// Seek to start
	if seg.StartTime > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", seg.StartTime))
	}
	args = append(args, "-i", seg.InputPath)

	// Duration
	if seg.Duration > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", seg.Duration))
	}

	// Video: re-encode with optional scaling
	args = append(args, "-c:v", "libx264", "-crf", strconv.Itoa(crf), "-preset", "slow")
	if seg.NeedScale {
		vf := fmt.Sprintf("scale=%d:%d,fps=%d", targetWidth, targetHeight, targetFPS)
		args = append(args, "-vf", vf)
	}

	// Audio
	if audioBitrate > 0 {
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", audioBitrate))
	} else {
		args = append(args, "-c:a", "aac")
	}

	args = append(args, "-movflags", "faststart", outputPath)
	return args
}

// concatIntermediates concatenates intermediate files into a single output using FFmpeg concat demuxer.
func (m *Muxer) concatIntermediates(ctx context.Context, intermediates []string, outputPath, tempDir string) error {
	concatListPath := filepath.Join(tempDir, "concat.txt")
	listContent := buildConcatList(intermediates)
	if err := os.WriteFile(concatListPath, []byte(listContent), 0o644); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}

	// Ensure output directory exists
	if dir := filepath.Dir(outputPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}

	// Concat with codec copy (all intermediates are same codec/resolution/FPS)
	concatArgs := []string{
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", concatListPath,
		"-c", "copy",
		"-movflags", "faststart",
		outputPath,
	}

	m.logger.Debug("ffmpeg concat", "args", strings.Join(concatArgs, " "))
	if err := m.runFFmpeg(ctx, concatArgs); err != nil {
		return fmt.Errorf("concat segments: %w", err)
	}

	return nil
}

// buildConcatList generates the FFmpeg concat demuxer file list content.
// Converts backslashes to forward slashes and escapes single quotes for Windows compatibility.
func buildConcatList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, "\\", "/")
		escaped = strings.ReplaceAll(escaped, "'", "\\'")
		fmt.Fprintf(&b, "file '%s'\n", escaped)
	}
	return b.String()
}

// TrimAndConcatWithProgress is like TrimAndConcat but reports progress via progressFn.
// Progress is distributed across segments weighted by duration (95% for encoding, 5% for concat).
func (m *Muxer) TrimAndConcatWithProgress(ctx context.Context, segments []TrimSegmentInput, outputPath string, targetWidth, targetHeight, targetFPS, crf, audioBitrate int, progressFn func(float64)) error {
	if len(segments) == 0 {
		return fmt.Errorf("no segments to trim")
	}

	// Single segment: trim directly with progress
	if len(segments) == 1 {
		seg := segments[0]
		opts := &TrimOptions{
			TrimStartOffset: seg.StartTime,
			TrimDuration:    seg.Duration,
			CRF:             crf,
			AudioBitrate:    audioBitrate,
			UsePreciseTrim:  true,
			ProgressFn:      progressFn,
		}
		return m.Mux(ctx, seg.InputPath, "", outputPath, opts)
	}

	// Calculate total encoding duration for progress weighting
	var totalDuration float64
	for _, seg := range segments {
		totalDuration += seg.Duration
	}

	const encodePortion = 0.95 // 95% of progress for encoding, 5% for concat

	tempDir, err := os.MkdirTemp("", "moombox-trim-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var intermediates []string
	var cumulativeDuration float64

	for i, seg := range segments {
		intermediatePath := filepath.Join(tempDir, fmt.Sprintf("seg_%d.mp4", i))
		intermediates = append(intermediates, intermediatePath)

		baseProgress := (cumulativeDuration / totalDuration) * encodePortion * 100
		segWeight := (seg.Duration / totalDuration) * encodePortion * 100

		segProgress := func(pct float64) {
			overall := baseProgress + (pct/100)*segWeight
			progressFn(overall)
		}

		args := m.buildTrimSegmentArgs(seg, intermediatePath, targetWidth, targetHeight, targetFPS, crf, audioBitrate)

		m.logger.Debug("ffmpeg trim segment", "index", i, "args", strings.Join(args, " "))
		if err := m.runFFmpegWithProgress(ctx, args, seg.Duration, segProgress); err != nil {
			return fmt.Errorf("trim segment %d: %w", i, err)
		}

		cumulativeDuration += seg.Duration
	}

	// Concat phase (remaining 5%)
	progressFn(encodePortion * 100)

	if err := m.concatIntermediates(ctx, intermediates, outputPath, tempDir); err != nil {
		return err
	}

	progressFn(100)
	return nil
}
