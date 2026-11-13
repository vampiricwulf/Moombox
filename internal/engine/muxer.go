package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/vampiricwulf/Moombox/internal/childproc"
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

// ffmpegPathOS is runtime.GOOS, overridable in tests so the Windows branch of
// ffmpegPathArg is exercised on every platform. Production never reassigns it.
var ffmpegPathOS = runtime.GOOS

// ffmpegPathArg prepares a file path for FFmpeg's or ffprobe's argv (ffprobe's
// through FFmpegPathArg, from outside this package). A composed output path —
// output dir + channel subdir + a 200-rune title + " - partN.chat.json" — can
// exceed Windows' 260-character MAX_PATH. Go opens
// such a path through the \\?\ extended-length prefix automatically; FFmpeg
// receives the raw string and fails on a host without the LongPathsEnabled
// policy (sweep-2 ENGINE-12). Only paths that actually need the prefix get it,
// so the ordinary case is byte-identical to before and carries no risk from
// FFmpeg's own path parsing.
//
// Both halves of "actually need" have to be measured the way Windows measures
// them, or that containment claim is false:
//
//   - MAX_PATH counts UTF-16 CODE UNITS. len(p) counts UTF-8 bytes — three per
//     CJK rune, four per emoji — so a 140-unit Japanese title (380 bytes) or a
//     220-unit emoji one (420 bytes) used to be rewritten for nothing. For a
//     VTuber-stream archiver those are the characteristic filenames, not
//     exotic inputs.
//   - the string FFmpeg has to open is the RESOLVED path, so the length is
//     measured after filepath.Abs. A relative argument still comes back
//     byte-identical when it resolves to something short.
func ffmpegPathArg(p string) string {
	if ffmpegPathOS != "windows" || p == "" || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if len(utf16.Encode([]rune(abs))) < 260 {
		return p
	}
	if rest, ok := strings.CutPrefix(abs, `\\`); ok {
		// A UNC path takes the \\?\UNC\ form, not \\?\\\server.
		return `\\?\UNC\` + rest
	}
	return `\\?\` + abs
}

// FFmpegPathArg is ffmpegPathArg for an argv built outside this package: the
// ffprobe calls in internal/worker (runFFprobe, probeStreamParams,
// probeAudioBitrate) and the import's duration probe pass their file through
// it. ffprobe opens its input through the same libavformat file protocol
// FFmpeg does, so a build that needs the prefix to open a path needs it for
// both: an archive path the mux wrote through the prefix was probed raw, the
// probe failed, and the row finished with no resolution or duration and
// without the short-output check (ENGINE-9) that runs on the probe's answer.
func FFmpegPathArg(p string) string { return ffmpegPathArg(p) }

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
// FFmpeg writes a temporary file beside outputPath, and checks run on it
// before it is renamed into place (writeOutput): an archive mux passes its
// duration check here, so a short copy never takes the archive's name.
func (m *Muxer) MuxCopy(ctx context.Context, videoPath, audioPath, outputPath string, checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, InputOffsets{}, "", "", nil, checks)
}

// InputOffsets delays the inputs of a MuxCopyAt, in seconds, each passed to
// FFmpeg as that input's -itsoffset. FFmpeg starts every input file at its
// own first timestamp, so two inputs that begin at different points of one
// timeline come out level unless told how far apart they begin. 0 (or less)
// is no delay.
type InputOffsets struct{ Video, Audio float64 }

// MuxCopyAt is MuxCopy with each input delayed by its offset: the input's
// first sample lands that far into the output.
func (m *Muxer) MuxCopyAt(ctx context.Context, videoPath, audioPath, outputPath string, off InputOffsets, checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, off, "", "", nil, checks)
}

// MuxCopyAspect is MuxCopyAt that also sets the video's display aspect ratio
// to aspect — FFmpeg's `-aspect`, which a stream copy carries as container
// metadata without re-encoding a frame. It is yt-dlp's FFmpegFixupStretchedPP
// (postprocessor/ffmpeg.py:860-867: stream copy plus `-aspect` with the
// ratio as `%f`) folded into the archive's own copy, where upstream runs it
// as a second remux: the yt:stretch fix for an old anamorphic upload. An
// empty aspect is MuxCopyAt.
func (m *Muxer) MuxCopyAspect(ctx context.Context, videoPath, audioPath, outputPath string, off InputOffsets, aspect string, checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, off, aspect, "", nil, checks)
}

// MuxCopyAtChapters is MuxCopyAt that also embeds the chapters of an
// FFMETADATA1 file (utils.FFMetadataChapters) as the output's chapter list.
// An empty chaptersPath is MuxCopyAt exactly.
//
// The chapters ride in as one more input, `-f ffmetadata -i <file>`, mapped
// with `-map_chapters <its index>` — the same input yt-dlp's FFmpegMetadataPP
// adds for --embed-chapters (postprocessor/ffmpeg.py, b77c9304e), which maps
// it with -map_metadata instead and so also replaces the global metadata; only
// the chapters are taken here. The file has no streams, so FFmpeg's default
// stream selection — no -map is given — still picks the same video and audio.
// Owner decision D23: the chapter plumbing is new to Moombox.
func (m *Muxer) MuxCopyAtChapters(ctx context.Context, videoPath, audioPath, outputPath string, off InputOffsets, chaptersPath string, checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, off, "", chaptersPath, nil, checks)
}

// MuxCopyArchive is MuxCopyAt with both of the archive's own extras: the
// yt:stretch display aspect (MuxCopyAspect) and the chapter list
// (MuxCopyAtChapters). Either may be empty; both empty is MuxCopyAt.
func (m *Muxer) MuxCopyArchive(ctx context.Context, videoPath, audioPath, outputPath string, off InputOffsets, aspect, chaptersPath string, checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, off, aspect, chaptersPath, nil, checks)
}

// MuxCopyArchiveWithProgress is MuxCopyArchive that reports the copy's
// progress to progress while FFmpeg runs (CopyProgress), on the calling
// goroutine. FFmpeg is then run with `-nostats -progress pipe:1` ahead of the
// same arguments; a nil progress is MuxCopyArchive exactly, argv included.
func (m *Muxer) MuxCopyArchiveWithProgress(ctx context.Context, videoPath, audioPath, outputPath string, off InputOffsets, aspect, chaptersPath string, progress func(CopyProgress), checks ...OutputCheck) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, nil, off, aspect, chaptersPath, progress, checks)
}

// RemuxChapters rewrites a finished archive with a new chapter list (owner
// decision CI-1, the chapter import): a stream copy of archive onto output
// that keeps every stream, the global metadata and each stream's own —
// `-map 0 -map_metadata 0 -c copy`, so a yt:stretch display aspect, carried
// as the video stream's sample aspect ratio, survives the copy as it is —
// less the data streams (`-map -0:d`): an MP4's chapter list rides in a text
// track of its own, which ffprobe lists as a data stream, and `-map 0` copies
// it as one beside the new list's track, so every import would add one. And it
// takes its chapters from the FFMETADATA1 file at chaptersPath (the same
// `-f ffmetadata -i … -map_chapters N` input MuxCopyAtChapters adds, built by
// the same buildArgs), or, when chaptersPath is empty, none at all
// (`-map_chapters -1`: a restore to an archive that had none).
//
// It goes through writeOutput like every archive write: FFmpeg writes a
// temporary file beside output, checks run on it, and only a file that
// passed them replaces output — so a failed or refused copy leaves output
// exactly as it was. output may be archive itself.
func (m *Muxer) RemuxChapters(ctx context.Context, archive, output, chaptersPath string, checks ...OutputCheck) error {
	if _, err := os.Stat(archive); err != nil {
		return fmt.Errorf("archive not found: %w", err)
	}
	return m.writeOutput(ctx, output, checks, func(tmp string) error {
		args := withAllStreamsKept(m.buildArgs(archive, "", tmp, nil, false, InputOffsets{}, chaptersPath), chaptersPath == "")
		m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
		return m.runFFmpeg(ctx, args)
	})
}

// withAllStreamsKept adds the output options that make a buildArgs argv over
// one input a whole-file copy — `-map 0` (every stream, not FFmpeg's default
// one-video-one-audio pick) less `-map -0:d` (the old chapter track; see
// RemuxChapters), `-map_metadata 0` (the input's global metadata), and
// `-map_chapters -1` when no chapter input replaces the input's own — ahead
// of the output path (its last argument).
func withAllStreamsKept(args []string, dropChapters bool) []string {
	extra := []string{"-map", "0", "-map", "-0:d", "-map_metadata", "0"}
	if dropChapters {
		extra = append(extra, "-map_chapters", "-1")
	}
	out := make([]string, 0, len(args)+len(extra))
	out = append(out, args[:len(args)-1]...)
	out = append(out, extra...)
	return append(out, args[len(args)-1])
}

// Mux performs FFmpeg muxing with optional trimming/encoding, into a
// temporary file renamed onto outputPath once FFmpeg has exited 0
// (writeOutput).
func (m *Muxer) Mux(ctx context.Context, videoPath, audioPath, outputPath string, opts *TrimOptions) error {
	return m.mux(ctx, videoPath, audioPath, outputPath, opts, InputOffsets{}, "", "", nil, nil)
}

// mux is the one FFmpeg run behind Mux and the MuxCopy family. copyProgress
// is the stream copy's optional progress callback
// (MuxCopyArchiveWithProgress); it is read only when opts is nil.
func (m *Muxer) mux(ctx context.Context, videoPath, audioPath, outputPath string, opts *TrimOptions, off InputOffsets, aspect, chaptersPath string, copyProgress func(CopyProgress), checks []OutputCheck) error {
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

	return m.writeOutput(ctx, outputPath, checks, func(tmp string) error {
		if needsTwoPass {
			return m.runTwoPassEncode(ctx, videoPath, audioPath, tmp, opts)
		}
		args := m.buildArgs(videoPath, audioPath, tmp, opts, needsEncode, off, chaptersPath)
		if videoPath != "" {
			args = withDisplayAspect(args, aspect)
		}
		if opts == nil && copyProgress != nil {
			args = withCopyProgress(args)
			m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
			return m.runFFmpegCopyProgress(ctx, args, inputBytes(videoPath, audioPath), copyProgress)
		}
		m.logger.Debug("ffmpeg", "args", strings.Join(args, " "))
		if opts != nil && opts.ProgressFn != nil && hasTrim(opts) && opts.TrimDuration > 0 {
			return m.runFFmpegWithProgress(ctx, args, opts.TrimDuration, opts.ProgressFn)
		}
		return m.runFFmpeg(ctx, args)
	})
}

func (m *Muxer) buildArgs(videoPath, audioPath, outputPath string, opts *TrimOptions, needsEncode bool, off InputOffsets, chaptersPath string) []string {
	args := []string{"-y"} // Overwrite

	// Input args with optional trim
	if opts != nil && opts.TrimStartOffset > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
	}
	// Every path argument goes through ffmpegPathArg: FFmpeg opens the inputs
	// before it ever touches the output, so a long input path fails first
	// (sweep-2 ENGINE-12). Short paths come back byte-identical.
	if videoPath != "" {
		args = appendInputOffset(args, off.Video)
		args = append(args, "-i", ffmpegPathArg(videoPath))
	}

	if audioPath != "" {
		if opts != nil && opts.TrimStartOffset > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", opts.TrimStartOffset))
		}
		args = appendInputOffset(args, off.Audio)
		args = append(args, "-i", ffmpegPathArg(audioPath))
	}

	// Chapters (MuxCopyAtChapters): the FFMETADATA file is the last input, and
	// its index is the number of media inputs ahead of it.
	if chaptersPath != "" {
		idx := 0
		if videoPath != "" {
			idx++
		}
		if audioPath != "" {
			idx++
		}
		args = append(args, "-f", "ffmetadata", "-i", ffmpegPathArg(chaptersPath), "-map_chapters", strconv.Itoa(idx))
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
	args = append(args, ffmpegPathArg(outputPath))

	return args
}

// withDisplayAspect adds `-aspect aspect` to an argv buildArgs made, as an
// output option ahead of the output path (its last argument); an empty
// aspect leaves the argv as it was.
func withDisplayAspect(args []string, aspect string) []string {
	if aspect == "" || len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:len(args)-1]...)
	return append(out, "-aspect", aspect, args[len(args)-1])
}

// appendInputOffset adds the -itsoffset that delays the next input by sec
// seconds, when it is a delay at all. It is an input option: it goes
// between that input's -ss and its -i.
func appendInputOffset(args []string, sec float64) []string {
	if sec <= 0 {
		return args
	}
	return append(args, "-itsoffset", strconv.FormatFloat(sec, 'f', 6, 64))
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
	cmd := childproc.CommandContext(ctx, m.ffmpegPath, args...)
	cmd.Stdout = nil

	// Capture stderr for error reporting with a 1MB cap.
	// If output exceeds the cap, only the last 512KB is kept.
	stderrBuf := &cappedBuffer{maxSize: 1 << 20, keepSize: 512 << 10}
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
		}
		stderr := stderrTail(stderrBuf.String())
		m.logger.Error("ffmpeg failed", "stderr", stderr)
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr)
	}

	return nil
}

// ffmpegStderrTail is how much of FFmpeg's stderr a failed run's error
// carries, in bytes: the last lines, where FFmpeg says why it failed. The
// worker reads a full disk from an archive mux's text (isDiskFull,
// internal/worker), so it must reach back past the closing "Conversion
// failed!" to the line that names the error.
const ffmpegStderrTail = 500

// stderrTail is the last ffmpegStderrTail bytes of FFmpeg's stderr.
func stderrTail(stderr string) string {
	if len(stderr) > ffmpegStderrTail {
		return stderr[len(stderr)-ffmpegStderrTail:]
	}
	return stderr
}

func hasTrim(opts *TrimOptions) bool {
	return opts != nil && (opts.TrimStartOffset > 0 || opts.TrimDuration > 0)
}
