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
	// With a .old beside it the ~ is not the update's artifact (the second
	// update of one launcher lifetime), so RV-10's rename does not apply and
	// the marker alone is what keeps the ~.
	if err := os.WriteFile(exePath+updater.OldSuffix, []byte("the binary this update replaced"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanupOrphans(exePath, "9.9.9")
	if _, err := os.Stat(exePath + "~"); err != nil {
		t.Fatalf("the ~ file went while the marker stood (%v) — this test's premise moved", err)
	}

	if _, err := clearSupersededFailureMarker(exePath, pendingPath, "v9.9.9", "9.9.9"); err != nil {
		t.Fatal(err)
	}
	// The boot that clears the marker resolves the breadcrumb in the same
	// step (finishUpdateBreadcrumb).
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	cleanupOrphans(exePath, "9.9.9")
	if _, err := os.Stat(exePath + "~"); !os.IsNotExist(err) {
		t.Errorf("the ~ file survived the sweep after a later update cleared the stale marker (%v)", err)
	}
}

// firstUpdateRestarted lays out a first update of a launcher lifetime at the
// moment its restart has run: the installed release at the plain name, the
// one it replaced renamed to ~ by handleUpdateRestart, the breadcrumb naming
// the installed release, and the launcher's record armed with the ~.
func firstUpdateRestarted(t *testing.T) (exePath string, boot *postUpdateBoot) {
	t.Helper()
	exePath = filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{
		exePath:                                "N+1",
		exePath + updater.OldSuffix:            "N",
		exePath + updater.PendingVersionSuffix: "v2.0.1",
	})
	boot = &postUpdateBoot{}
	boot.restarted(handleUpdateRestart(exePath), pendingUpdateTag(exePath), false, 0)
	if !boot.armed || boot.artifact != exePath+updater.LauncherImageSuffix {
		t.Fatalf("after the restart: armed=%v artifact=%q, want the boot armed with the ~", boot.armed, boot.artifact)
	}
	return exePath, boot
}

// recordLauncherImageDeletes swaps the deferred ~ delete for a counter: the
// real one starts a detached cmd.
func recordLauncherImageDeletes(t *testing.T) *int {
	t.Helper()
	n := 0
	real := scheduleOldLauncherDelete
	t.Cleanup(func() { scheduleOldLauncherDelete = real })
	scheduleOldLauncherDelete = func(string) { n++ }
	return &n
}

// Owner ruling RV-10. The first boot of a Windows update ends before its
// started milestone: it exits 3 (a port in use, a config it cannot load), or
// it is stopped or quit in its first seconds. The previous binary is the ~
// the restart renamed .old to. The launcher used to schedule its deletion on
// a stop or a quit, and the next start swept it after an exit 3 (whose notice
// had just told the operator it was still there and to start again), so that
// start ran the unproven release with nothing to roll back to. Now the
// launcher leaves the ~ on its way out, and the next start renames it back to
// .old and arms its first child with it, as for an update applied and never
// booted.
//
// Mutants: keepsLauncherImage answering false (the stop and the quit schedule
// the delete); cleanupOrphans removing the ~ as before (.old is missing and
// the start is not armed).
func TestAnUpdatesFirstBootEndingBeforeItsMilestoneStaysArmed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		terminating bool
	}{
		{"startup error", exitCodeStartupError, false},
		{"stopped", 1, true},
		{"quit", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath, boot := firstUpdateRestarted(t)
			deletes := recordLauncherImageDeletes(t)

			// What the loop does with the exit.
			action, _ := boot.judge(false, tc.code, 5*time.Second, tc.terminating, 0)
			switch action {
			case childPropagate:
				boot.sweepLauncherImage(exePath)
			case childPostUpdateFailure:
				var restored bool
				captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, tc.code) })
				if restored {
					t.Fatal("this exit must not be rolled back")
				}
			default:
				t.Fatalf("action = %v, want the launcher to exit", action)
			}
			if *deletes != 0 {
				t.Fatal("the launcher scheduled the deletion of the ~, the only copy of the previous binary")
			}

			// The next start, in launchAndSupervise's order.
			cleanupOrphans(exePath, "2.0.1")
			var next postUpdateBoot
			armed := next.armInterruptedUpdate(exePath, "2.0.1")
			if !bytesEqualFile(t, exePath+updater.OldSuffix, "N") {
				t.Error("the previous binary must be back at .old")
			}
			if _, err := os.Stat(exePath + updater.LauncherImageSuffix); !os.IsNotExist(err) {
				t.Errorf("the ~ must have been renamed, not copied or left (stat: %v)", err)
			}
			if !armed || !next.atFreshStart || next.artifact != exePath+updater.OldSuffix {
				t.Errorf("next start: armed=%v atFreshStart=%v artifact=%q, want it armed at its own start with .old",
					armed, next.atFreshStart, next.artifact)
			}
		})
	}
}

// RV-10 has no exception for a failed-update marker left by an EARLIER update.
// That marker stands until a later update reaches its milestone, so it is on
// disk during the first boot of the first update after any rolled-back one,
// and the start's sweep used to return at it before the rename: the update
// most in need of the rollback was the one left unarmed. A marker older than
// the breadcrumb no longer stops the rename. One that is not older is this
// update's own failed launch, whose instructions name the ~, and still does.
//
// Mutants: cleanupOrphans returning at any marker (the older-marker case has
// no .old and is not armed); cleanupOrphans ignoring markers for the rename
// (the newer-marker case moves the ~ the marker points at).
func TestAnEarlierUpdatesFailureMarkerDoesNotDisarmTheNextUpdate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		markerAge time.Duration // relative to the breadcrumb
		wantArmed bool
	}{
		{"marker older than the breadcrumb", -48 * time.Hour, true},
		{"marker newer than the breadcrumb", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath, boot := firstUpdateRestarted(t)
			captureStderr(t, func() { boot.recoverFailedBoot(exePath, exitCodeStartupError) })
			marker := exePath + updater.UpdateFailedMarkerSuffix
			writeReleases(t, map[string]string{marker: "an update failed"})
			applied := time.Now().Add(-time.Hour)
			if err := os.Chtimes(exePath+updater.PendingVersionSuffix, applied, applied); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(marker, applied.Add(tc.markerAge), applied.Add(tc.markerAge)); err != nil {
				t.Fatal(err)
			}

			cleanupOrphans(exePath, "2.0.1")
			var next postUpdateBoot
			armed := next.armInterruptedUpdate(exePath, "2.0.1")
			if armed != tc.wantArmed {
				t.Errorf("next start armed = %v, want %v", armed, tc.wantArmed)
			}
			at, gone := exePath+updater.LauncherImageSuffix, exePath+updater.OldSuffix
			if tc.wantArmed {
				at, gone = gone, at
			}
			if !bytesEqualFile(t, at, "N") {
				t.Errorf("the previous binary must be at %s", at)
			}
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("%s is on disk (stat: %v), want the previous binary under one name only", gone, err)
			}
		})
	}
}

// The exit-3 notice promises the keep and the automatic rollback only when the
// next start delivers them. A release that reached its milestone (breadcrumb
// resolved) and then exits 3 on a settings restart inside the post-update
// window still has the ~ as its recorded artifact, and the notice used to say
// the next start would move it to .old and stay armed, when that start removes
// it and arms nothing.
//
// Mutants: startupErrorKeepNote always promising (the "started" case prints
// the promise); never promising (the first-start case loses it).
func TestTheStartupErrorNoticePromisesOnlyWhatTheNextStartDoes(t *testing.T) {
	const promise, moves = "goes back to it by itself if", "the next start moves it to"
	for _, tc := range []struct {
		name    string
		started bool
	}{
		{"before the milestone", false},
		{"after the milestone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath, boot := firstUpdateRestarted(t)
			if tc.started {
				resolveLandedUpdateBreadcrumb(exePath, "2.0.1", &nopLogger{})
				// A settings restart 30 s in: the launcher carries the boot forward.
				_, wasFirst := boot.judge(false, exitCodeRestart, 30*time.Second, false, 0)
				boot.restarted(boot.restartArtifact(exePath, wasFirst), pendingUpdateTag(exePath), wasFirst, 30*time.Second)
				if !boot.armed {
					t.Fatal("the restart inside the window did not carry the boot forward")
				}
			}
			stderr := captureStderr(t, func() { boot.recoverFailedBoot(exePath, exitCodeStartupError) })
			for _, s := range []string{promise, moves} {
				if got := strings.Contains(stderr, s); got == tc.started {
					t.Errorf("notice contains %q = %v, want %v:\n%s", s, got, !tc.started, stderr)
				}
			}
			if got := strings.Contains(stderr, "already started properly once"); got != tc.started {
				t.Errorf("notice says the release already started = %v, want %v:\n%s", got, tc.started, stderr)
			}

			// What the notice describes is what the next start does.
			cleanupOrphans(exePath, "2.0.1")
			var next postUpdateBoot
			if armed := next.armInterruptedUpdate(exePath, "2.0.1"); armed == tc.started {
				t.Errorf("next start armed = %v, but the notice said otherwise", armed)
			}
		})
	}
}

// The other side of RV-10: once the release has started properly, the ~ is
// an old launcher image again. The milestone resolves the breadcrumb, a stop
// after it schedules the delete as before, and a start that still finds the ~
// removes it and arms nothing. The same goes for a ~ beside a breadcrumb that
// names another release (the binary a rollback restored).
//
// Mutants: keepsLauncherImage not reading the breadcrumb (no delete is
// scheduled after the milestone); cleanupOrphans renaming without the version
// test (the "another release" case leaves a .old).
func TestALauncherImageIsSweptOnceItsUpdateHasStarted(t *testing.T) {
	exePath, boot := firstUpdateRestarted(t)
	deletes := recordLauncherImageDeletes(t)
	resolveLandedUpdateBreadcrumb(exePath, "2.0.1", &nopLogger{})

	if action, _ := boot.judge(false, 1, 10*time.Minute, true, 0); action != childPropagate {
		t.Fatalf("a stop: action = %v, want it propagated", action)
	}
	boot.sweepLauncherImage(exePath)
	if *deletes != 1 {
		t.Errorf("deletes scheduled after the milestone = %d, want 1", *deletes)
	}

	for _, tc := range []struct{ name, breadcrumb string }{
		{"no breadcrumb", ""},
		{"a breadcrumb naming another release", "v2.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			files := map[string]string{exePath: "N+1", exePath + updater.LauncherImageSuffix: "N"}
			if tc.breadcrumb != "" {
				files[exePath+updater.PendingVersionSuffix] = tc.breadcrumb
			}
			writeReleases(t, files)
			cleanupOrphans(exePath, "2.0.1")
			for _, suffix := range []string{updater.LauncherImageSuffix, updater.OldSuffix} {
				if _, err := os.Stat(exePath + suffix); !os.IsNotExist(err) {
					t.Errorf("%s is on disk after the start's sweep (stat: %v), want the ~ removed and no .old made", suffix, err)
				}
			}
		})
	}
}

// The second update of one launcher lifetime: the rename to ~ failed, so the
// artifact is .old and the ~ beside it is the launcher's own image, one
// version older. RV-10 must not keep or promote that file: a boot that ends
// before its milestone schedules the ~ for deletion as before, and the next
// start arms with the .old that is there, never with the ~ renamed over it.
//
// Mutants: keepsLauncherImage decided from the names on disk (a breadcrumb
// and a ~: no delete is scheduled); cleanupOrphans renaming the ~ over an
// existing .old (the start arms a rollback two versions back).
func TestASecondUpdatesOlderLauncherImageIsNeverTheArtifact(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{
		exePath:                                "N+2",
		exePath + updater.OldSuffix:            "N+1",
		exePath + updater.LauncherImageSuffix:  "N",
		exePath + updater.PendingVersionSuffix: "v2.0.2",
	})
	deletes := recordLauncherImageDeletes(t)
	var boot postUpdateBoot
	// handleUpdateRestart's answer when its rename fails.
	boot.restarted(exePath+updater.OldSuffix, pendingUpdateTag(exePath), false, 0)

	if action, _ := boot.judge(false, 1, 5*time.Second, true, 0); action != childPropagate {
		t.Fatalf("a stop: action = %v, want it propagated", action)
	}
	boot.sweepLauncherImage(exePath)
	if *deletes != 1 {
		t.Errorf("deletes scheduled = %d, want 1: the ~ is two versions back and stands for nothing", *deletes)
	}

	cleanupOrphans(exePath, "2.0.2")
	if !bytesEqualFile(t, exePath+updater.OldSuffix, "N+1") {
		t.Error(".old must still hold the release this update replaced")
	}
	if _, err := os.Stat(exePath + updater.LauncherImageSuffix); !os.IsNotExist(err) {
		t.Errorf("the older ~ survived the start's sweep (stat: %v)", err)
	}
}

// RV-10 when the start's rename of the ~ back to .old fails: something holds
// the ~ without delete sharing (an antivirus scan of the freshly unmapped
// image). The start warned that the previous binary stays there, yet nothing
// recorded the ~ as the artifact, so a stop, a quit or an exit 3 before the
// milestone scheduled its deletion and the only copy of the previous binary
// went. The launcher now keeps it on its way out while the breadcrumb names
// that release, and the next start renames it back and arms with it; once the
// release has reached its milestone the ~ is swept as before.
//
// Mutants: cleanupOrphans reporting false on the failed rename (the stop
// schedules the delete); keepsLauncherImage not reading keptImageTag (the
// same); keptImageTag kept past the breadcrumb (no delete after the
// milestone).
func TestALauncherImageThatCannotBeRenamedBackIsKept(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{
		exePath:                                "N+1",
		exePath + updater.LauncherImageSuffix:  "N",
		exePath + updater.PendingVersionSuffix: "v2.0.1",
	})
	warnings := captureLauncherWarnings(t)
	deletes := recordLauncherImageDeletes(t)

	// os.Open shares read and write but not delete, so the rename fails.
	holder, err := os.Open(exePath + updater.LauncherImageSuffix)
	if err != nil {
		t.Fatal(err)
	}
	kept := cleanupOrphans(exePath, "2.0.1")
	holder.Close()
	if !kept {
		t.Fatalf("cleanupOrphans reported the ~ not kept (warnings %q)", *warnings)
	}
	var boot postUpdateBoot
	boot.keptImageTag = "v2.0.1" // what launchAndSupervise records from it
	if boot.armInterruptedUpdate(exePath, "2.0.1") {
		t.Fatal("the start armed without a .old")
	}
	if action, _ := boot.judge(false, 1, 5*time.Second, true, 0); action != childPropagate {
		t.Fatalf("a stop: action = %v, want it propagated", action)
	}
	boot.sweepLauncherImage(exePath)
	if *deletes != 0 {
		t.Fatal("the launcher scheduled the deletion of the ~, the only copy of the previous binary")
	}

	// The next start, with nothing holding the ~ any more.
	if cleanupOrphans(exePath, "2.0.1") {
		t.Error("the next start reported the ~ kept, want it renamed back")
	}
	var next postUpdateBoot
	if !next.armInterruptedUpdate(exePath, "2.0.1") || !bytesEqualFile(t, exePath+updater.OldSuffix, "N") {
		t.Error("the next start did not arm with the previous binary at .old")
	}

	// Past the milestone the record keeps nothing.
	resolveLandedUpdateBreadcrumb(exePath, "2.0.1", &nopLogger{})
	boot.sweepLauncherImage(exePath)
	if *deletes != 1 {
		t.Errorf("deletes scheduled after the milestone = %d, want 1", *deletes)
	}
}
