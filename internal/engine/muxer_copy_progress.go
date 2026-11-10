package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/childproc"
)

// CopyProgress is one report from a stream copy that was given a progress
// callback (MuxCopyArchiveWithProgress).
type CopyProgress struct {
	// OutputBytes is how much FFmpeg says it has written (`total_size` of
	// its -progress stream); InputBytes is the size of the media inputs. A
	// stream copy's output is about the size of its inputs, so the two give
	// a percent without probing a duration.
	OutputBytes, InputBytes int64
	// Finalizing is set when FFmpeg is still running but has stopped
	// reporting. FFmpeg reports from its copy loop, and writes the trailer
	// after it: for an MP4 written with `-movflags faststart` that is the
	// second pass, which rewrites the whole file to move the moov atom to
	// the front and reports nothing while it does. A copy that later
	// reports again (a read that hung for a while) clears it. FFmpeg's last
	// report, made once the trailer is written, sets it too: the copy is
	// then over but for FFmpeg's exit and the checks on the output.
	Finalizing bool
}

// Percent is the copy's progress as a whole percent, 0-99: never 100, since
// the copy is not done until FFmpeg has exited and the output has passed its
// checks. 0 when the inputs' size is unknown.
func (p CopyProgress) Percent() int {
	if p.InputBytes <= 0 || p.OutputBytes <= 0 {
		return 0
	}
	return int(min(p.OutputBytes*100/p.InputBytes, 99))
}

// copyProgressArgs are the global options that make FFmpeg write its
// machine-readable progress (key=value lines, one block per report closed by
// a `progress=` line) to standard output, and keep the human `frame=… time=…`
// line off standard error, whose tail is the error text of a failed run.
var copyProgressArgs = []string{"-nostats", "-progress", "pipe:1"}

// withCopyProgress puts copyProgressArgs ahead of an argv buildArgs made.
// Only a copy that was given a callback goes through it: every other run's
// argv is what it was.
func withCopyProgress(args []string) []string {
	out := make([]string, 0, len(args)+len(copyProgressArgs))
	out = append(out, copyProgressArgs...)
	return append(out, args...)
}

// copyStallAfter is how long FFmpeg may go without a progress report before
// the copy is reported as finalizing. FFmpeg reports every 0.5 s by default
// (-stats_period), so three missed reports. Measured on FFmpeg 8.0: a 6.8 GB
// copy reported every 0.5 s, went quiet for the whole faststart pass, and
// made its `progress=end` report only after it.
const copyStallAfter = 1500 * time.Millisecond

// inputBytes is the combined size of a copy's media inputs; a path that is
// empty or cannot be read counts for nothing.
func inputBytes(paths ...string) int64 {
	var total int64
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// runFFmpegCopyProgress is runFFmpeg for an argv that carries
// copyProgressArgs: it reads FFmpeg's progress from standard output and hands
// each report to fn, on this goroutine. Standard error is buffered exactly as
// runFFmpeg buffers it, so a failed copy's error reads the same — the worker
// reads a full disk from that text (isDiskFull).
func (m *Muxer) runFFmpegCopyProgress(ctx context.Context, args []string, total int64, fn func(CopyProgress)) error {
	cmd := childproc.CommandContext(ctx, m.ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrBuf := &cappedBuffer{maxSize: 1 << 20, keepSize: 512 << 10}
	cmd.Stderr = stderrBuf

	// failed is runFFmpeg's error for a run that did not exit 0.
	failed := func(err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
		}
		stderr := stderrTail(stderrBuf.String())
		m.logger.Error("ffmpeg failed", "stderr", stderr)
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", err, stderr)
	}

	// Start and Wait stay on this goroutine (childproc holds its OS thread
	// between them on Linux); only the pipe is read elsewhere.
	if err := cmd.Start(); err != nil {
		return failed(err)
	}
	// The pipe is read to its end before Wait, as os/exec requires.
	m.pumpCopyProgress(stdout, total, copyStallAfter, fn)
	if err := cmd.Wait(); err != nil {
		return failed(err)
	}
	return nil
}

// pumpCopyProgress reads FFmpeg's -progress stream from r until it ends and
// calls fn, on the calling goroutine, once per report and once per stallAfter
// that passes without one after the first. It returns when r is at its end:
// FFmpeg has exited, or been killed.
//
// The read itself runs on a helper goroutine, so that a quiet pipe can be
// timed. It always drains r to the end — a full pipe would block FFmpeg — and
// a line it cannot parse is skipped: the readout is a courtesy, and nothing
// about the copy's outcome is decided here.
func (m *Muxer) pumpCopyProgress(r io.Reader, total int64, stallAfter time.Duration, fn func(CopyProgress)) {
	type report struct {
		size int64
		end  bool
	}
	reports := make(chan report, 16)
	go func() {
		defer close(reports)
		defer func() {
			if rec := recover(); rec != nil {
				m.logger.Error("panic reading ffmpeg progress", "panic", rec)
				io.Copy(io.Discard, r)
			}
		}()
		size := int64(-1)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
			if !ok {
				continue
			}
			switch key {
			case "total_size":
				// "N/A" until the muxer has written something.
				if n, err := strconv.ParseInt(value, 10, 64); err == nil {
					size = n
				}
			case "progress":
				// Closes a block: "continue", or "end" for the last.
				if size >= 0 {
					reports <- report{size, value == "end"}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			// A line over the scanner's cap: keep the pipe moving.
			io.Copy(io.Discard, r)
		}
	}()

	last := CopyProgress{InputBytes: total}
	seen := false
	stall := time.NewTimer(stallAfter)
	defer stall.Stop()
	for {
		select {
		case rep, ok := <-reports:
			if !ok {
				return
			}
			seen = true
			last.OutputBytes, last.Finalizing = rep.size, rep.end
			fn(last)
			stall.Reset(stallAfter)
		case <-stall.C:
			if seen {
				last.Finalizing = true
				fn(last)
			}
			stall.Reset(stallAfter)
		}
	}
}
