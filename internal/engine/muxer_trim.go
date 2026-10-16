package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// trimTempMaxAge is how old a leftover temp directory must be before a sweep
// removes it: longer than any encode that can own one, so an in-flight one is
// never touched.
const trimTempMaxAge = 24 * time.Hour

// CleanupOldTrimTempDirs removes leftover `moombox-2pass-*` and
// `moombox-concat-*` directories (ConcatCopy's, which the worker's part merge
// uses) from %TEMP% that are older than 24h, and the `moombox-trim-*` ones
// that versions before multi-segment trims wrote their intermediates beside
// the output (trimTempDirPattern) left there — RemoveStaleTrimTempDirs sweeps
// where they go now. These are normally cleaned up by `defer
// os.RemoveAll(tempDir)`, but a hard process abort (panic in a sibling
// goroutine, OS kill, power loss) bypasses the defer and leaves the dirs
// orphaned. Windows eventually reclaims %TEMP% but on a long-running install
// they accumulate. Safe to call at startup; uses 24h age threshold so a
// concurrent mux's in-flight tempdir is never touched.
//
// Errors are logged-and-continue at the caller; this is housekeeping,
// not a critical path.
func CleanupOldTrimTempDirs() (removed int, err error) {
	return utils.RemoveStaleTempEntries(trimTempMaxAge, "moombox-trim-", "moombox-2pass-", "moombox-concat-")
}

// trimTempDirMarker sits between the output's stem and os.MkdirTemp's random
// part in the name of a multi-segment trim's intermediates directory.
const trimTempDirMarker = ".moombox-trim-"

// trimTempDirRe is the whole name of such a directory: a stem, the marker,
// and MkdirTemp's random part, which is decimal. A clip the trim service
// places is "<id> [<start>s-<end>s].mp4" and never takes this shape, so the
// sweep below can never match one.
var trimTempDirRe = regexp.MustCompile(`^.+` + regexp.QuoteMeta(trimTempDirMarker) + `[0-9]+$`)

// trimTempDirPattern is the os.MkdirTemp pattern for the directory a
// multi-segment trim re-encodes its parts into: BESIDE outputPath and named
// after its stem — "<stem>.moombox-trim-<n>".
//
// Beside the output, because the intermediates together are about the size
// of the clip, and the clip's volume is the one the disk alerts and the
// backlog's disk gate watch. Under os.TempDir() they went to the container's
// /tmp in Docker — the writable layer on the host's /var/lib/docker, which no
// Moombox check sees — and to RAM on a host whose /tmp is a tmpfs, where a
// long trim failed with ENOSPC however much room the output disk had.
//
// After its stem, because the trim service claims the clip's stem for the
// length of the encode (claimOutputStem, internal/worker): the orphan sweep
// never offers an intermediate the encode is still writing, and a directory a
// crash left says whose it was.
func trimTempDirPattern(outputPath string) string {
	base := filepath.Base(outputPath)
	return strings.TrimSuffix(base, filepath.Ext(base)) + trimTempDirMarker + "*"
}

// RemoveStaleTrimTempDirs removes the multi-segment trim intermediates
// directories (trimTempDirPattern) in dir that are older than 24h: what a
// hard abort mid-trim left in a trim directory before the deferred removal
// ran. A missing dir has nothing to sweep. Only a directory of exactly that
// name is touched, so a file in dir — a clip — never is.
func RemoveStaleTrimTempDirs(dir string) (removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-trimTempMaxAge)
	for _, ent := range entries {
		if !ent.IsDir() || !trimTempDirRe.MatchString(ent.Name()) {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(dir, ent.Name())) == nil {
			removed++
		}
	}
	return removed, nil
}

// TrimSegmentInput describes one segment for the TrimAndConcatWithProgress pipeline.
type TrimSegmentInput struct {
	InputPath string
	StartTime float64 // local start time within this segment
	Duration  float64 // duration to extract from this segment
	NeedScale bool    // true if resolution/FPS differs from target
}

// buildTrimSegmentArgs builds FFmpeg arguments for trimming a single segment.
func (m *Muxer) buildTrimSegmentArgs(seg TrimSegmentInput, outputPath string, targetWidth, targetHeight, targetFPS, crf, audioBitrate int) []string {
	args := []string{"-y"}

	// Seek to start
	if seg.StartTime > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", seg.StartTime))
	}
	// Both path arguments go through ffmpegPathArg (sweep-2 ENGINE-12): the
	// input is a staged recording and the output is the user-composed archive
	// path, the same shapes buildArgs guards.
	args = append(args, "-i", ffmpegPathArg(seg.InputPath))

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

	args = append(args, "-movflags", "faststart", ffmpegPathArg(outputPath))
	return args
}

// concatIntermediates concatenates intermediate files into a single output using FFmpeg concat demuxer.
//
// Every entry is written to the list as an absolute path. The concat demuxer
// resolves a relative entry against the LIST FILE's directory — tempDir, a
// moombox-concat-* directory under the system temp or a trim's intermediates
// directory beside its output (trimTempDirPattern) — and not
// against the working directory the rest of FFmpeg's argv is resolved
// against. A part row under the default paths.output_directory ("./output")
// is relative to the working directory, so the Tier 4 part merge handed
// ConcatCopy names FFmpeg looked for under the temp directory, and every
// merge on a native install with a relative output directory failed.
func (m *Muxer) concatIntermediates(ctx context.Context, intermediates []string, outputPath, tempDir string) error {
	entries := make([]string, len(intermediates))
	for i, p := range intermediates {
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve concat input %q: %w", p, err)
		}
		entries[i] = abs
	}
	concatListPath := filepath.Join(tempDir, "concat.txt")
	listContent := buildConcatList(entries)
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
	concatArgs := m.buildConcatArgs(concatListPath, outputPath)

	m.logger.Debug("ffmpeg concat", "args", strings.Join(concatArgs, " "))
	if err := m.runFFmpeg(ctx, concatArgs); err != nil {
		return fmt.Errorf("concat segments: %w", err)
	}

	return nil
}

// buildConcatArgs builds the concat-demuxer argv. Split out of
// concatIntermediates so the argv is assertable without running FFmpeg, like
// buildArgs and buildTrimSegmentArgs.
//
// outputPath goes through ffmpegPathArg (sweep-2 ENGINE-12) — it is the
// user-composed archive path — and so does concatListPath: the part merge's
// is <os.MkdirTemp dir>/concat.txt under the system temp, short, but a
// trim's sits in its intermediates directory beside the clip
// (trimTempDirPattern), inside the user's output tree. The list's own
// ENTRIES must stay unprefixed regardless, because buildConcatList rewrites
// backslashes to forward slashes for the demuxer and a prefixed entry would
// come out as //?/C:/… and fail to parse.
func (m *Muxer) buildConcatArgs(concatListPath, outputPath string) []string {
	return []string{
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", ffmpegPathArg(concatListPath),
		"-c", "copy",
		"-movflags", "faststart",
		ffmpegPathArg(outputPath),
	}
}

// ConcatCopy concatenates inputs into outputPath using FFmpeg's concat
// demuxer with codec copy (lossless, no re-encode). It is the exported
// entry point for the Tier 4 same-format part merge: callers are expected
// to have already confirmed the inputs share compatible stream params (see
// probeStreamParams / streamParams.equal in internal/worker) — concat
// demuxer copy mode silently produces a broken file if inputs disagree on
// codec, resolution, framerate, or audio params. Like every output, it is
// written under a temporary name and renamed onto outputPath once FFmpeg has
// exited 0 (writeOutput).
func (m *Muxer) ConcatCopy(ctx context.Context, inputs []string, outputPath string) error {
	if len(inputs) == 0 {
		return fmt.Errorf("no inputs to concat")
	}

	tempDir, err := os.MkdirTemp("", "moombox-concat-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	return m.writeOutput(ctx, outputPath, nil, func(tmp string) error {
		return m.concatIntermediates(ctx, inputs, tmp, tempDir)
	})
}

// buildConcatList generates the FFmpeg concat demuxer file list content.
// Converts backslashes to forward slashes and escapes single quotes for
// Windows compatibility.
//
// ffconcat quoting is shell-style, not C-style: a single quote inside a
// single-quoted string cannot be escaped with a leading backslash (`\'` is
// not a recognized escape and breaks parsing) — the correct sequence is
// close the quote, place a backslash-escaped literal quote OUTSIDE any
// quoting, then reopen the quote: close-backslash-quote-quote. A path like
// "It's a stream.mp4" must become close-backslash-quote-quote-escaped
// ("'It'\”s a stream.mp4'"), not the invalid "'It\'s a stream.mp4'" the
// naive replacement used to produce.
func buildConcatList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, "\\", "/")
		escaped = strings.ReplaceAll(escaped, "'", `'\''`)
		fmt.Fprintf(&b, "file '%s'\n", escaped)
	}
	return b.String()
}

// TrimAndConcatWithProgress trims multiple segment files, optionally scales
// them to a common resolution/FPS, and concatenates them into a single output
// file, reporting progress via progressFn. Used for cross-segment trims on
// multi-segment quality-split jobs; every trim reports its progress (the
// trim service hands it on), so there is no variant without it. Progress is
// distributed across segments weighted by duration (95% for encoding, 5% for
// concat).
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

	// The intermediates go beside the output, on its volume
	// (trimTempDirPattern), whose directory need not exist yet.
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(outputPath), trimTempDirPattern(outputPath))
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

	if err := m.writeOutput(ctx, outputPath, nil, func(tmp string) error {
		return m.concatIntermediates(ctx, intermediates, tmp, tempDir)
	}); err != nil {
		return err
	}

	progressFn(100)
	return nil
}
