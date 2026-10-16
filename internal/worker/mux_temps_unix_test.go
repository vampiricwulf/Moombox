//go:build !windows

package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// liveMuxHelperEnv makes TestTheBootSweepLeavesAnotherLiveInstancesMux its
// own second Moombox: with it set, the test runs a mux into the shared output
// directory it names and blocks in it.
const liveMuxHelperEnv = "MOOMBOX_WORKER_LIVE_MUX_HELPER"

// TestTheBootSweepLeavesAnotherLiveInstancesMux is the W30 review's
// shared-tree finding at the start-up sweep: two Moombox instances, each
// under its own data directory and so its own single-instance lock, write to
// one output tree (a compose stack per account, each binding the same
// media directory). Instance B is muxing; instance A starts, and its boot
// sweep met B's temporary file wearing a token that was not A's — and removed
// it. B's FFmpeg reopened its output by name for the faststart pass, the mux
// failed, and B's row went to Error. The sweep now leaves a file whose
// writer's lock is held, says so at Debug, and takes it once that writer is
// gone.
//
// Mutants: engine.removeStaleMuxTemp not trying the writer's lock (B's live
// file is removed); RemoveStaleMuxTemps not logging the live files (no Debug
// line).
func TestTheBootSweepLeavesAnotherLiveInstancesMux(t *testing.T) {
	if dir := os.Getenv(liveMuxHelperEnv); dir != "" {
		m := engine.NewMuxer(filepath.Join(dir, "ffmpeg"), discardLogger{})
		_ = m.MuxCopy(context.Background(), filepath.Join(dir, "video.mp4"), "", filepath.Join(dir, "out", "Chan", "Title [b].mp4"))
		os.Exit(0)
	}

	tools := t.TempDir()
	outputDir := filepath.Join(tools, "out")
	if err := os.MkdirAll(filepath.Join(outputDir, "Chan"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"for a; do out=\"$a\"; done\n" +
		"printf partial > \"$out\"\n" +
		"echo $$ > '" + tools + "/pid'\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(filepath.Join(tools, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "video.mp4"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestTheBootSweepLeavesAnotherLiveInstancesMux$", "-test.timeout=60s")
	helper.Env = append(os.Environ(), liveMuxHelperEnv+"="+tools)
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
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if pid == 0 {
		t.Fatal("the other instance's mux never started its FFmpeg")
	}

	// Instance A: its own database, a job of its own in the shared tree.
	_, db := testWorkerSetup(t)
	if _, err := db.AddJob(&database.Job{ID: "a", VideoID: "a", URL: "u", Status: database.StatusFinished,
		OutputDirectory: outputDir}); err != nil {
		t.Fatal(err)
	}
	inflight := muxTempsIn(t, filepath.Join(outputDir, "Chan"))
	if len(inflight) != 1 {
		t.Fatalf("the other instance should be writing one temporary file, found %v", inflight)
	}

	logs := &captureLogger{}
	if n := RemoveStaleMuxTemps(db, outputDir, logs); n != 0 {
		t.Errorf("the boot sweep removed %d files while the instance writing them was alive", n)
	}
	if _, err := os.Stat(inflight[0]); err != nil {
		t.Errorf("the boot sweep removed the other instance's live temporary file: %v", err)
	}
	if p, ok := logs.field("boot cleanup: left a temporary file another running Moombox process is writing", "path"); !ok || p != inflight[0] {
		t.Errorf("the sweep logged %v for the live file, want its path at Debug", logs.lines())
	}

	// Instance B dies mid-mux: its file is a leftover now.
	_ = helper.Process.Kill()
	_ = helper.Wait()
	if n := RemoveStaleMuxTemps(db, outputDir, discardLogger{}); n != 1 {
		t.Errorf("after the other instance died the sweep removed %d files, want its leftover", n)
	}
	if left := muxTempsIn(t, filepath.Join(outputDir, "Chan")); len(left) != 0 {
		t.Errorf("after the other instance died its temporary file is still there: %v", left)
	}
}

// muxTempsIn lists the temporary outputs in dir.
func muxTempsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if engine.IsMuxTempPath(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
