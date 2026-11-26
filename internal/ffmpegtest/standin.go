// Package ffmpegtest provides a stand-in FFmpeg for tests that need a trim
// they can hold open, stop or fail: the test binary itself, run again as the
// encoder. A shell script did this before and ran nowhere but Unix, so the
// tests that stop a running trim never ran on Windows.
//
// A package using it calls RunIfStandIn first thing in its TestMain. It is
// an ordinary package rather than a _test.go helper because internal/worker
// and internal/web/routes both use it. Nothing in production imports it.
package ffmpegtest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dirEnv names the stand-in's directory. Gated sets it for the length of one
// test; the trim service starts FFmpeg with the environment it inherited, so
// the child sees it and RunIfStandIn takes over.
const dirEnv = "MOOMBOX_TEST_FFMPEG_STANDIN_DIR"

// giveUpAfter bounds the wait for the gate, so a run nothing stops (a broken
// Stop) cannot outlive the test binary by much.
const giveUpAfter = 30 * time.Second

// Gated returns the path of a stand-in FFmpeg and its directory. Run as
// FFmpeg, the stand-in:
//
//   - appends the output it was given (its last argument) to dir/outputs;
//   - writes a few bytes there, the way a real encode opens its output at
//     once;
//   - reports 24 s encoded on stderr, the way FFmpeg's stats line does;
//   - waits for dir/gate (create it to let the encode end), giving up with
//     exit status 1 after 30 s;
//   - then, with dir/fail present, fails as FFmpeg does on a full disk;
//     otherwise finishes the file.
//
// Its sibling ffprobe does not exist, so a probe beside it fails without
// running anything.
//
// The directory travels in the process environment, so one test must not
// have stand-ins of two directories running at once, and a test that calls
// Gated cannot be parallel (t.Setenv refuses).
func Gated(t *testing.T) (ffmpeg, dir string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("ffmpegtest: the test binary's path: %v", err)
	}
	dir = t.TempDir()
	t.Setenv(dirEnv, dir)
	return exe, dir
}

// RunIfStandIn returns at once in the test process. In a process started as
// the stand-in it acts FFmpeg out and exits, never returning. It must be the
// first thing TestMain does: the stand-in has FFmpeg's argv, not go test's,
// and must not run the package's tests or its TestMain's setup.
func RunIfStandIn() {
	dir := os.Getenv(dirEnv)
	if dir == "" {
		return
	}
	os.Exit(run(dir, os.Args))
}

func run(dir string, args []string) int {
	out := args[len(args)-1]
	f, err := os.OpenFile(filepath.Join(dir, "outputs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 2
	}
	_, werr := fmt.Fprintln(f, out)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 2
	}
	if err := os.WriteFile(out, []byte("partial"), 0o644); err != nil {
		return 2
	}
	fmt.Fprint(os.Stderr, "frame=120 fps=60 q=28.0 size=256kB time=00:00:24.00 bitrate=87.4kbits/s speed=12x\r")

	gate := filepath.Join(dir, "gate")
	for deadline := time.Now().Add(giveUpAfter); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 1
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fail")); err == nil {
		fmt.Fprintln(os.Stderr, "Error writing trailer: No space left on device")
		fmt.Fprintln(os.Stderr, "Conversion failed!")
		return 1
	}
	if err := os.WriteFile(out, []byte("whole"), 0o644); err != nil {
		return 2
	}
	return 0
}
