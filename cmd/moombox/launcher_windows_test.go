//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

// captureLauncherWarnings redirects the launcher's stderr warning sink into a
// slice for the duration of one test. The launcher has no logger — it runs
// before, and outside, the child's logging stack — so this seam is the only way
// to assert that a failure was reported rather than swallowed.
func captureLauncherWarnings(t *testing.T) *[]string {
	t.Helper()
	var got []string
	real := launcherWarnf
	t.Cleanup(func() { launcherWarnf = real })
	launcherWarnf = func(format string, args ...any) {
		got = append(got, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}
	return &got
}

// TestHandleUpdateRestartFirstUpdateIsUnchanged pins case (a) of the plan's
// cross-version table: with no ~ in the way, the rename still happens, nothing
// is warned, and the artifact is the ~ file exactly as before.
//
// Mutant: making handleUpdateRestart skip the rename (or warn unconditionally)
// fails this.
func TestHandleUpdateRestartFirstUpdateIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if handleUpdateRestart(exePath) != "" {
		t.Fatal("a restart with no .old is a config restart — want no artifact")
	}
	if err := os.WriteFile(exePath+".old", []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	warnings := captureLauncherWarnings(t)

	if got, want := handleUpdateRestart(exePath), exePath+"~"; got != want {
		t.Fatalf("a restart with a .old present is a binary update — artifact = %q, want %q", got, want)
	}
	if _, err := os.Stat(exePath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old must be renamed away on the first update, stat err: %v", err)
	}
	data, err := os.ReadFile(exePath + "~")
	if err != nil {
		t.Fatalf("the ~ file must hold the previous binary: %v", err)
	}
	if string(data) != "previous binary" {
		t.Errorf("~ holds %q, want the previous binary", data)
	}
	if len(*warnings) != 0 {
		t.Errorf("the success path must stay silent, got %v", *warnings)
	}
}

// TestHandleUpdateRestartReportsARenameItCouldNotDo pins case (b): the ~ name
// is held by a file Windows will not let MoveFileEx replace — in the field the
// launcher's own mapped image, which denies delete-sharing and which the
// child's CleanupOldBinary could not delete for that same reason — so the
// rename fails. Merely HAVING a ~ file there does not; see the body comment
// below. The failure has to be reported, and the function must still report a
// binary update so the launcher's one-shot rollback window still arms.
//
// Mutant: dropping the launcherWarnf call (today's `os.Rename(...)` with the
// error discarded) leaves warnings empty and fails this.
func TestHandleUpdateRestartReportsARenameItCouldNotDo(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if err := os.WriteFile(exePath+".old", []byte("version N+1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+"~", []byte("version N"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Go's os.Rename is MoveFileEx with MOVEFILE_REPLACE_EXISTING, so merely
	// having a ~ file there is NOT enough — it would be overwritten. What makes
	// the field case fail is that the ~ file is a mapped image, which denies
	// delete-sharing. An ordinary *os.File handle denies exactly the same thing
	// (Go opens with FILE_SHARE_READ|FILE_SHARE_WRITE, never FILE_SHARE_DELETE),
	// so it produces the identical "Access is denied" without mapping anything,
	// starting a process or naming an image.
	held, err := os.Open(exePath + "~")
	if err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir's own cleanup, so it runs BEFORE it (LIFO) —
	// Windows cannot remove the directory while the handle is open.
	t.Cleanup(func() { held.Close() })
	warnings := captureLauncherWarnings(t)

	// The .old that stayed is one version back and therefore the artifact; ~
	// is two.
	//
	// Mutant: returning exePath+"~" from the failed-rename branch fails this.
	if got, want := handleUpdateRestart(exePath), exePath+".old"; got != want {
		t.Fatalf("a .old that could not be renamed is still a binary update, and its artifact — got %q, want %q", got, want)
	}
	if len(*warnings) != 1 {
		t.Fatalf("want exactly one warning about the failed rename, got %v", *warnings)
	}
	if !strings.Contains((*warnings)[0], ".old") {
		t.Errorf("the warning must name the file that stayed behind, got %q", (*warnings)[0])
	}
	// And the destination, so the operator can see WHICH name the shuffle
	// could not take — the ~ file is also what the rollback instructions name.
	// It reaches them twice over: the format names it, and os.Rename's
	// *os.LinkError carries both paths, so dropping only the %s still passes.
	//
	// Mutant: a terser warning that names the .old and nothing else
	// (`launcherWarnf("...could not rename %s...", oldPath)` — no destination,
	// no error) fails this, which is the shape that would actually leave the
	// operator without the name.
	if !strings.Contains((*warnings)[0], exePath+"~") {
		t.Errorf("the warning must name the ~ destination the rename could not take, got %q", (*warnings)[0])
	}
	data, err := os.ReadFile(exePath + ".old")
	if err != nil || string(data) != "version N+1" {
		t.Errorf(".old must be left intact, got %q (err %v)", data, err)
	}
	// And the ~ file — the version TWO back — is exactly what it was.
	older, err := os.ReadFile(exePath + "~")
	if err != nil || string(older) != "version N" {
		t.Errorf("~ must be left intact, got %q (err %v)", older, err)
	}
}

// TestASecondUpdatesSweptOldIsPastRollback walks the second update of one
// launcher lifetime through the launcher's own steps: handleUpdateRestart
// at each restart, its answer recorded and the exits judged by the loop's
// postUpdateBoot. The first update's rename makes ~ the launcher's image; the
// second cannot rename over it, so its artifact is .old; that boot's
// milestone sweeps .old and leaves ~, two versions back. Re-deriving the
// artifact from the names on disk at the exit found ~ and rolled back to it —
// N+2 to N, with N+1 gone.
//
// Mutants: handleUpdateRestart returning exePath+"~" from the failed-rename
// branch — the second update's record names ~, whose judgement would route
// the crash to a rollback; postUpdateBoot.judge handing judgeChildExit the
// at-exit lookup (TestASecondUpdateIsJudgedByTheArtifactItsRestartRecorded's
// first mutant) — the crash is routed to a rollback to ~.
func TestASecondUpdatesSweptOldIsPastRollback(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	captureLauncherWarnings(t)
	var boot postUpdateBoot

	// Update 1 (N → N+1): ApplyUpdate left N at .old.
	write(exePath, "N+1")
	write(exePath+".old", "N")
	boot.restarted(handleUpdateRestart(exePath), "", false, 0)
	if want := exePath + "~"; boot.artifact != want {
		t.Fatalf("first update's artifact = %q, want %q", boot.artifact, want)
	}
	// ~ is now the launcher's mapped image; an open handle denies the same
	// delete-sharing (see TestHandleUpdateRestartReportsARenameItCouldNotDo).
	held, err := os.Open(exePath + "~")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })

	// The N+1 boot proves itself, then exits 42 to apply update 2.
	action, first := boot.judge(false, exitCodeRestart, 10*time.Minute, false, 0)
	if action != childRestart || !first {
		t.Fatalf("the first boot's update restart = (%v, first %v), want a restart of the first boot", action, first)
	}

	// Update 2 (N+1 → N+2): ApplyUpdate left N+1 at .old.
	write(exePath+".old", "N+1")
	write(exePath, "N+2")
	boot.restarted(handleUpdateRestart(exePath), "", first, 10*time.Minute)
	if boot.artifact != exePath+".old" {
		t.Fatalf("second update's artifact = %q, want its .old", boot.artifact)
	}

	// The N+2 boot reaches its milestone: CleanupOldBinary sweeps .old (no
	// process maps N+1 any more) and cannot delete ~. Then it crashes.
	if err := os.Remove(boot.artifact); err != nil {
		t.Fatal(err)
	}
	action, first = boot.judge(false, 1, 30*time.Second, false, 0)
	if action != childCrash || first {
		t.Errorf("a quick crash after the swept .old = (%v, first %v), want a supervised crash, not a rollback to ~", action, first)
	}
}

// TestDeferDeleteCommandDeletesOnlyTheOldLauncher runs the real deferred
// cleanup against install directories whose names cmd would otherwise parse.
// With the path passed as a bare argument, `Tools&Apps` split the line and
// `del /f /q <dir>\Tools` emptied the sibling directory; `pct%OS%dir` was
// expanded to a path that does not exist, so the old launcher stayed.
//
// Mutant: go back to passing oldPath as del's argument (no CmdLine, no
// environment variable) — the decoy is deleted and the %OS% file survives.
func TestDeferDeleteCommandDeletesOnlyTheOldLauncher(t *testing.T) {
	for _, dir := range []string{"Tools&Apps", "pct%OS%dir"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			// The directory the '&' split names, and a file in it del must
			// never reach.
			decoyDir := filepath.Join(root, "Tools")
			if err := os.MkdirAll(decoyDir, 0o755); err != nil {
				t.Fatal(err)
			}
			decoy := filepath.Join(decoyDir, "keep.txt")
			if err := os.WriteFile(decoy, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			installDir := filepath.Join(root, dir)
			if err := os.MkdirAll(installDir, 0o755); err != nil {
				t.Fatal(err)
			}
			oldPath := filepath.Join(installDir, "moombox.exe~")
			if err := os.WriteFile(oldPath, []byte("old launcher"), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := deferDeleteCommand(oldPath)
			if cmd.SysProcAttr == nil || cmd.SysProcAttr.CmdLine != deferDeleteCmdLine {
				t.Fatalf("CmdLine must be the fixed line %q", deferDeleteCmdLine)
			}
			if strings.Contains(cmd.SysProcAttr.CmdLine, oldPath) {
				t.Fatalf("the path must not reach cmd's command line: %q", cmd.SysProcAttr.CmdLine)
			}
			if err := cmd.Run(); err != nil {
				t.Fatalf("cleanup command: %v", err)
			}
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Errorf("the old launcher must be deleted, stat err = %v", err)
			}
			if _, err := os.Stat(decoy); err != nil {
				t.Errorf("a file outside the install directory was deleted: %v", err)
			}
		})
	}
}

// TestCleanupOrphansResumesOnceASupersededMarkerIsCleared is the point of
// clearSupersededFailureMarker on this platform: while an earlier failure's
// .update-failed marker stands, the launcher's startup sweep keeps every
// later update's ~ image on disk; once a later update has landed and the boot
// cleared the marker, the sweep removes it again.
//
// Mutant: clearSupersededFailureMarker removing nothing — the ~ file survives
// the second sweep.
func TestCleanupOrphansResumesOnceASupersededMarkerIsCleared(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeAutoRollbackMarker(exePath, 1, true)
	failedAt := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(exePath+".update-failed", failedAt, failedAt); err != nil {
		t.Fatal(err)
	}
	pendingPath := exePath + updater.PendingVersionSuffix
	if err := os.WriteFile(pendingPath, []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+"~", []byte("the launcher image a later update replaced"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanupOrphans(exePath)
	if _, err := os.Stat(exePath + "~"); err != nil {
		t.Fatalf("the ~ file went while the marker stood (%v) — this test's premise moved", err)
	}

	if _, err := clearSupersededFailureMarker(exePath, pendingPath, "v9.9.9", "9.9.9"); err != nil {
		t.Fatal(err)
	}
	cleanupOrphans(exePath)
	if _, err := os.Stat(exePath + "~"); !os.IsNotExist(err) {
		t.Errorf("the ~ file survived the sweep after a later update cleared the stale marker (%v)", err)
	}
}
