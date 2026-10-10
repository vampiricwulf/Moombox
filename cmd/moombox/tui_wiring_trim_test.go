package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// errTrimFFmpegFailure is the error TrimService.CreateTrim returned for a
// progress-reporting trim whose FFmpeg ran out of space at the trailer,
// captured with the real FFmpeg writing to /dev/full: the stderr tail the
// progress runner keeps, line breaks and all.
var errTrimFFmpegFailure = fmt.Errorf("ffmpeg trim: %w", errors.New("ffmpeg: exit status 228 (stderr: 0:0 -> #0:0 (copy)\n"+
	"Press [q] to stop, [?] for help\n"+
	"size=       0kB time=-00:00:00.50 bitrate=  -0.0kbits/s speed=N/A    \n"+
	"[out#0/mpegts @ 0x556fb868a680] Error writing trailer: No space left on device\n"+
	"[out#0/mpegts @ 0x556fb868a680] Error closing file: No space left on device\n"+
	"[out#0/mpegts @ 0x556fb868a680] video:12kB audio:0kB subtitle:0kB other streams:0kB global headers:0kB muxing overhead: 65.226764%\n"+
	"size=      19kB time=00:00:01.90 bitrate=  83.9kbits/s speed=1.53e+03x    \n"+
	"Conversion failed!)"))

// TestTUITrimFailureTextFitsOneLine: the TUI shows a failed Trim Video on one
// line — the trim dialog's error line, or the feedback row once the dialog is
// dismissed. Handed err.Error(), an FFmpeg failure brought its stderr tail
// with it, seven line breaks in 534 bytes, and the dialog grew to 27 rows on
// a 20- or 24-row terminal, pushing the frame's top rows off the screen. A
// refusal is still shown as written, as the Web route answers it: it is what
// the operator acts on, and it is one short line.
//
// Mutants: trimFailureText returning err.Error() — the FFmpeg failure is
// eight lines; drop its refusal branch — a refusal reads as the fixed line;
// OnCreateTrim returning err.Error() again — the structural check fails.
func TestTUITrimFailureTextFitsOneLine(t *testing.T) {
	for _, refused := range []*worker.TrimRefusedError{
		{Reason: "end time (90s) exceeds video duration (60s)"},
		{Reason: "another trim operation is already in progress for this job", Conflict: true},
	} {
		if got := trimFailureText(fmt.Errorf("wrapped: %w", refused)); got != refused.Reason {
			t.Errorf("trimFailureText(refusal) = %q, want its reason %q", got, refused.Reason)
		}
	}

	got := trimFailureText(errTrimFFmpegFailure)
	// 58 columns is the trim dialog's content width at 80 columns and wider
	// (a 60-column box less its border).
	if strings.ContainsAny(got, "\r\n") || utf8.RuneCountInString(got) > 58 {
		t.Errorf("trimFailureText(FFmpeg failure) = %q (%d runes): want one line of at most 58", got, utf8.RuneCountInString(got))
	}

	// OnCreateTrim is installed on the live tui.App runTUI builds, with no
	// seam to call it through, so its use of the helper is pinned as text,
	// as TestRecoverAsidesWiringForwardsTheWorker pins its neighbours.
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	if !strings.Contains(string(src), `return "", trimFailureText(err)`) {
		t.Error(`OnCreateTrim does not return trimFailureText(err) — the TUI would show the trim's raw error, FFmpeg's stderr tail included`)
	}
}
