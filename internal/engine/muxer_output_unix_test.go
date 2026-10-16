//go:build !windows

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeFFmpeg writes a shell stand-in for FFmpeg into dir: it appends every
// output path it is handed (its last argument) to dir/outputs, writes
// "partial" there, and then — unless block is set — "whole" and exits 0. With
// block it records its pid in dir/pid and sleeps until it is killed.
func fakeFFmpeg(t *testing.T, dir string, block bool) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"for a; do out=\"$a\"; done\n" +
		"printf '%s\\n' \"$out\" >> '" + dir + "/outputs'\n" +
		"printf partial > \"$out\"\n"
	if block {
		script += "echo $$ > '" + dir + "/pid'\nexec sleep 30\n"
	} else {
		script += "printf whole > \"$out\"\n"
	}
	p := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEveryFFmpegWriteIsHandedATemporaryName walks every entry point that
// writes an output file — the copy mux, the two-pass encode, the part
// merge's concat and the cross-segment trim's — and checks what FFmpeg was
// actually told to write: never the final name, and the final name holds the
// finished file once the call returns. Before W28-A01 each of them handed
// FFmpeg the final name with -y, and a crash mid-write left a half-written
// archive there for the next start's re-mux to write over.
//
// Mutants: mux building its argv with outputPath instead of tmp (the copy
// mux hands FFmpeg the final name); the two-pass branch passing outputPath to
// runTwoPassEncode; ConcatCopy or TrimAndConcatWithProgress calling
// concatIntermediates with outputPath.
func TestEveryFFmpegWriteIsHandedATemporaryName(t *testing.T) {
	cases := []struct {
		name string
		run  func(m *Muxer, in []string, final string) error
	}{
		{"copy mux", func(m *Muxer, in []string, final string) error {
			return m.MuxCopy(context.Background(), in[0], in[1], final)
		}},
		{"two-pass encode", func(m *Muxer, in []string, final string) error {
			return m.Mux(context.Background(), in[0], in[1], final, &TrimOptions{
				TrimDuration: 5, VideoBitrate: 1000, UsePreciseTrim: true, TwoPass: true,
			})
		}},
		{"part merge concat", func(m *Muxer, in []string, final string) error {
			return m.ConcatCopy(context.Background(), in, final)
		}},
		{"cross-segment trim", func(m *Muxer, in []string, final string) error {
			return m.TrimAndConcatWithProgress(context.Background(), []TrimSegmentInput{
				{InputPath: in[0], Duration: 5}, {InputPath: in[1], Duration: 5},
			}, final, 64, 64, 30, 20, 128, func(float64) {})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, out := t.TempDir(), t.TempDir()
			in := []string{filepath.Join(tools, "video.mp4"), filepath.Join(tools, "audio.m4a")}
			for _, p := range in {
				if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			final := filepath.Join(out, "Title [abc].mp4")
			m := NewMuxer(fakeFFmpeg(t, tools, false), &testLogger{})
			if err := tc.run(m, in, final); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			raw, err := os.ReadFile(filepath.Join(tools, "outputs"))
			if err != nil {
				t.Fatal(err)
			}
			for _, handed := range strings.Fields(string(raw)) {
				if handed == final {
					t.Errorf("FFmpeg was told to write the final name %s", final)
				}
			}
			if b, err := os.ReadFile(final); err != nil || string(b) != "whole" {
				t.Errorf("the final name holds %q (err %v), want the finished file", b, err)
			}
			if names := dirNames(t, out); len(names) != 1 {
				t.Errorf("the output directory holds %v, want the final file alone", names)
			}
		})
	}
}

// muxCrashHelperEnv makes TestACrashMidMuxLeavesNoWriterAndNoFinalFile its own
// Moombox child: with it set, the test runs a mux and blocks in it.
const muxCrashHelperEnv = "MOOMBOX_ENGINE_MUX_CRASH_HELPER"

// TestACrashMidMuxLeavesNoWriterAndNoFinalFile is W28-A01 end to end with a
// stand-in for FFmpeg. A process blocked in MuxCopy is SIGKILLed — the
// abnormal death the launcher respawns from — and what it leaves is what the
// respawned child's boot re-mux meets:
//
//   - no FFmpeg still writing (Linux: the parent-death signal childproc sets);
//     before, the stand-in was reparented to init and ran on;
//   - nothing under the archive's name, only one temporary file wearing the
//     dead run's token;
//   - and that file goes in the boot sweep, which leaves the directory empty.
//
// Mutants: childproc's prepare not setting Pdeathsig (the stand-in is still
// running); mux handing FFmpeg the final name (a partial archive sits under
// it); RemoveStaleMuxTemps skipping a gone run's file (it is still there).
func TestACrashMidMuxLeavesNoWriterAndNoFinalFile(t *testing.T) {
	if os.Getenv(muxCrashHelperEnv) != "" {
		dir := os.Getenv(muxCrashHelperEnv)
		m := NewMuxer(filepath.Join(dir, "ffmpeg"), &testLogger{})
		_ = m.MuxCopy(context.Background(), filepath.Join(dir, "video.mp4"), "", filepath.Join(dir, "out", "Title.mp4"))
		os.Exit(0)
	}
	if runtime.GOOS != "linux" {
		t.Skip("the parent-death signal that ends the stand-in is Linux's")
	}

	tools := t.TempDir()
	out := filepath.Join(tools, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeFFmpeg(t, tools, true)
	if err := os.WriteFile(filepath.Join(tools, "video.mp4"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestACrashMidMuxLeavesNoWriterAndNoFinalFile$", "-test.timeout=60s")
	helper.Env = append(os.Environ(), muxCrashHelperEnv+"="+tools)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && pid == 0; {
		if raw, err := os.ReadFile(filepath.Join(tools, "pid")); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if pid == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if pid == 0 {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		t.Fatal("the mux never started its FFmpeg")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	_ = helper.Process.Kill()
	_ = helper.Wait()

	gone := false
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !gone; {
		gone = syscall.Kill(pid, 0) != nil || processIsZombie(pid)
		if !gone {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !gone {
		t.Errorf("FFmpeg (pid %d) is still writing 5s after the process that started it was killed", pid)
	}

	if _, err := os.Stat(filepath.Join(out, "Title.mp4")); err == nil {
		t.Error("the crash left a file under the archive's name; the boot re-mux would meet a half-written archive")
	}
	names := dirNames(t, out)
	if len(names) != 1 || !IsMuxTempPath(names[0]) || muxTempRunOf(names[0]) == muxRun {
		t.Fatalf("the crash left %v, want exactly one temporary file of the dead run's", names)
	}
	if _, err := RemoveStaleMuxTemps([]string{tools}, nil); err != nil {
		t.Fatal(err)
	}
	if names := dirNames(t, out); len(names) != 0 {
		t.Errorf("after the boot sweep the output directory holds %v, want nothing", names)
	}
}

// processIsZombie reports whether pid has exited and is only waiting to be
// reaped: a killed stand-in is reparented to whatever reaps orphans here, and
// one that does not would leave it a zombie kill(pid, 0) still finds.
func processIsZombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && (s[i+2] == 'Z' || s[i+2] == 'X')
}
