# Linux Build Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Compile, ship, and run Moombox on Linux x64 + arm64 alongside the existing Windows x64 target, with pragmatic feature parity. Bundle two opportunistic improvements: browser-selection dropdown for auto-cookies setup, and a fix for the broken `~` cleanup that switched from ping to schtasks.

**Architecture:** Cross-platform via Go build tags (`_windows.go` / `_unix.go`), per-platform Node.js sidecar binaries embedded behind build tags, platform-aware updater asset matching, server-side markdown rendering for release notes. Three release assets per platform: `Moombox.exe`/`moombox-linux-amd64`/`moombox-linux-arm64` plus matching `.sig` signatures.

**Tech Stack:** Go 1.25 (modules), modernc/sqlite (no CGo), Charmbracelet Bubble Tea suite (TUI), Shoelace v2.16 (web UI), goldmark + bluemonday (server-side markdown render), glamour (TUI markdown render), syscall.Flock (Linux single-instance), syscall.Statfs (Linux disk space).

**Setup before starting:** Recommended to create a dedicated git worktree:
```bash
git worktree add ../moombox-linux feat/linux-build-support
cd ../moombox-linux
```

**Sequencing:** Phases 1-7 are the critical path (gets Linux compiling and CI working). Phases 8-11 are independent improvements that can be done in any order or in parallel by different agents. Phase 12 (docs) depends on all earlier phases.

---

## Spec Reference

This plan implements `docs/superpowers/specs/2026-05-02-linux-build-support-design.md`. When in doubt about intent, consult that doc.

---

## Phase 1: Per-package Linux fallbacks

These get the codebase compiling on Linux. Without these, `GOOS=linux go build ./...` fails immediately.

### Task 1: Disk space query for Linux

**Files:**
- Create: `internal/disk/disk_unix.go`
- Modify: `internal/disk/disk_windows.go` (filename rename only)
- Modify: `internal/disk/disk_windows_test.go` (filename rename only)
- Test: `internal/disk/disk_unix_test.go`

**Goal:** Add a `syscall.Statfs`-based implementation of `GetDiskSpace` for non-Windows platforms, mirroring the existing Windows API.

- [x] **Step 1: Verify file naming conventions**

The existing file is `disk_windows.go`. Go's filename build constraints already restrict it to Windows builds — no change needed. Verify with:
```bash
grep -l "^//go:build" internal/disk/disk_windows.go || echo "Uses filename constraint only"
```
Expected output: `Uses filename constraint only` (the file's name `_windows.go` is sufficient).

- [x] **Step 2: Write the failing test**

Create `internal/disk/disk_unix_test.go`:
```go
//go:build !windows

package disk

import (
	"os"
	"testing"
)

func TestGetDiskSpaceUnix(t *testing.T) {
	tmp, err := os.MkdirTemp("", "disk-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmp)

	ds, err := GetDiskSpace(tmp)
	if err != nil {
		t.Fatalf("GetDiskSpace returned error: %v", err)
	}
	if ds == nil {
		t.Fatal("GetDiskSpace returned nil")
	}
	if ds.Total == 0 {
		t.Error("Total bytes is 0; expected > 0 on a real filesystem")
	}
	if ds.UsedPct < 0 || ds.UsedPct > 100 {
		t.Errorf("UsedPct out of range [0,100]: %v", ds.UsedPct)
	}
	if ds.Free > ds.Total {
		t.Errorf("Free (%d) > Total (%d), invariant violated", ds.Free, ds.Total)
	}
}

func TestGetDiskSpaceUnixNonexistentPath(t *testing.T) {
	_, err := GetDiskSpace("/this/path/does/not/exist/anywhere")
	if err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}
```

- [x] **Step 3: Run test on Linux to verify it fails**

```bash
GOOS=linux go test -v ./internal/disk/...
```
Expected: build error (`undefined: GetDiskSpace`) because no implementation exists for non-Windows.

- [x] **Step 4: Create the Linux implementation**

Create `internal/disk/disk_unix.go`:
```go
//go:build !windows

package disk

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// GetDiskSpace returns disk space information for the volume containing path.
// On Unix, uses statfs(2). Bavail (blocks available to non-superuser) is
// reported as Free so per-user quotas are reflected, matching the Windows
// behaviour of using freeBytesAvailable.
func GetDiskSpace(path string) (*DiskSpace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve path: %w", err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(abs, &stat); err != nil {
		return nil, fmt.Errorf("disk: statfs %q: %w", abs, err)
	}

	// Bsize is the optimal transfer block size; Bavail is blocks available
	// to a non-privileged user. uint64 cast handles 32-bit Bsize on some
	// platforms.
	bsize := uint64(stat.Bsize)
	free := stat.Bavail * bsize
	total := stat.Blocks * bsize

	var usedPct float64
	if total > 0 {
		usedPct = float64(total-free) / float64(total) * 100
	}

	return &DiskSpace{
		Free:    free,
		Total:   total,
		UsedPct: usedPct,
	}, nil
}
```

- [x] **Step 5: Run test to verify it passes**

```bash
GOOS=linux go test -v ./internal/disk/...
```
Expected: PASS for both `TestGetDiskSpaceUnix` and `TestGetDiskSpaceUnixNonexistentPath`. Also verify Windows still works:
```bash
GOOS=windows go build ./internal/disk/...
```
Expected: build succeeds (existing Windows test only runs natively, so we're verifying compile-only).

- [x] **Step 6: Commit**

```bash
git add internal/disk/disk_unix.go internal/disk/disk_unix_test.go
git commit -m "feat(disk): add Linux/Unix disk space query via statfs

Mirrors the existing Windows GetDiskFreeSpaceExW behaviour using
syscall.Statfs. Bavail used so per-user quotas are reflected, matching
the Windows freeBytesAvailable semantics."
```

---

### Task 2: FFmpeg elevation stubs for non-Windows

**Files:**
- Create: `internal/web/routes/ffmpeg_elevation_other.go`
- Test: `internal/web/routes/ffmpeg_elevation_other_test.go`

**Goal:** Provide non-Windows stubs for `isElevated`, `runElevated`, and `waitForProcess` so the package compiles on Linux. The existing `runtime.GOOS != "windows"` guards in `ffmpeg.go` already prevent install endpoints from firing on non-Windows; these stubs just need to compile.

- [x] **Step 1: Read existing Windows file to understand the API**

```bash
head -30 internal/web/routes/ffmpeg_elevation_windows.go
```
Note the three functions to stub: `isElevated() bool`, `runElevated(script string) (syscall.Handle, error)`, `waitForProcess(handle syscall.Handle, timeout time.Duration) error`.

- [x] **Step 2: Write the failing test**

Create `internal/web/routes/ffmpeg_elevation_other_test.go`:
```go
//go:build !windows

package routes

import (
	"errors"
	"testing"
)

func TestRunElevatedNotSupported(t *testing.T) {
	_, err := runElevated("echo hi")
	if err == nil {
		t.Fatal("expected error for runElevated on non-Windows, got nil")
	}
	if !errors.Is(err, errElevationNotSupported) {
		t.Errorf("expected errElevationNotSupported, got: %v", err)
	}
}

func TestIsElevatedReportsRoot(t *testing.T) {
	// Just verify the function returns without panicking. The actual
	// boolean depends on test runner UID; we don't assert a specific
	// value because CI may run as either root or unprivileged.
	_ = isElevated()
}
```

- [x] **Step 3: Run test to verify it fails**

```bash
GOOS=linux go test -v ./internal/web/routes/ -run "TestRunElevatedNotSupported|TestIsElevatedReportsRoot"
```
Expected: build error (`undefined: runElevated, isElevated`).

- [x] **Step 4: Create the stub file**

Create `internal/web/routes/ffmpeg_elevation_other.go`:
```go
//go:build !windows

package routes

import (
	"errors"
	"os"
	"time"
)

// On non-Windows, these stubs exist purely so the package compiles.
// The runtime.GOOS != "windows" guards in ffmpeg.go prevent any install
// endpoints from invoking these functions. If those guards are ever
// removed, callers will see a clear "not supported" error instead of a
// panic.

// errElevationNotSupported is returned by runElevated on non-Windows.
// Exported as a package-level var so tests can assert with errors.Is.
var errElevationNotSupported = errors.New("UAC elevation is not supported on this platform")

// isElevated returns true when running as root on Unix-like systems.
// Used by ffmpeg.go's PrepareInstall, which already short-circuits on
// non-Windows; this implementation is a sensible default if that ever
// changes.
func isElevated() bool {
	return os.Geteuid() == 0
}

// runElevated is a stub on non-Windows; UAC has no equivalent here.
// Linux installs use sudo/pkexec which require user-driven invocation,
// not programmatic elevation from a long-running process.
func runElevated(script string) (uintptr, error) {
	return 0, errElevationNotSupported
}

// waitForProcess is a stub on non-Windows. The Windows implementation
// uses WaitForSingleObject; the Linux equivalent (waitpid) isn't needed
// because runElevated above never returns a real handle.
func waitForProcess(handle uintptr, timeout time.Duration) error {
	return errElevationNotSupported
}
```

- [x] **Step 5: Update Windows file's runElevated/waitForProcess signatures to match**

The Windows file currently uses `syscall.Handle` types. Since `syscall.Handle` only exists on Windows (it's `uintptr` underneath), and our stub uses `uintptr`, we need both files to agree. Open `internal/web/routes/ffmpeg_elevation_windows.go` and change the signatures:

```bash
grep -n "func runElevated\|func waitForProcess" internal/web/routes/ffmpeg_elevation_windows.go
```

Edit `ffmpeg_elevation_windows.go`:
- Change `func runElevated(script string) (syscall.Handle, error)` to `func runElevated(script string) (uintptr, error)`. Internally, build the `syscall.Handle` from `sei.hProcess` and return as `uintptr(syscall.Handle(sei.hProcess))` — actually, since `sei.hProcess` is already `uintptr`, just return `sei.hProcess` directly as `uintptr` and let the caller cast if needed.
- Actually the simpler path: change the returned `syscall.Handle(sei.hProcess)` to `uintptr(sei.hProcess)`, return type to `uintptr`.
- Change `func waitForProcess(handle syscall.Handle, timeout time.Duration) error` to `func waitForProcess(handle uintptr, timeout time.Duration) error`. Inside, cast back: `defer syscall.CloseHandle(syscall.Handle(handle))` and `waitForSingleObj.Call(uintptr(handle), ...)` (the second already takes uintptr).

Verify the call sites in `ffmpeg.go`:
```bash
grep -n "runElevated\|waitForProcess" internal/web/routes/ffmpeg.go
```
Both calls already pass/receive what looks like a handle. Change `var handle syscall.Handle` declarations in `ffmpeg.go` to `var handle uintptr` if any exist; the call patterns should otherwise work unchanged.

- [x] **Step 6: Run tests on both platforms**

```bash
GOOS=linux go build ./internal/web/routes/...
GOOS=linux go test -v ./internal/web/routes/ -run "TestRunElevatedNotSupported|TestIsElevatedReportsRoot"
GOOS=windows go build ./internal/web/routes/...
```
Expected: All pass. Linux test reports PASS for both new tests.

- [x] **Step 7: Commit**

```bash
git add internal/web/routes/ffmpeg_elevation_other.go internal/web/routes/ffmpeg_elevation_other_test.go internal/web/routes/ffmpeg_elevation_windows.go internal/web/routes/ffmpeg.go
git commit -m "feat(web): add Linux stubs for FFmpeg UAC elevation

Adds ffmpeg_elevation_other.go with isElevated (geteuid check),
runElevated (returns ErrNotSupported), and waitForProcess (stub).
The runtime.GOOS != \"windows\" guards in ffmpeg.go already prevent
the install endpoints from firing on non-Windows; these stubs only
exist for compile-time correctness.

Changed Windows runElevated/waitForProcess to use uintptr instead
of syscall.Handle so signatures match across platforms."
```

---

## Phase 2: Launcher restructure

The launcher is the most Windows-coupled file in the codebase (uses `syscall.SysProcAttr.CreationFlags`, `schtasks`, etc.). Restructure into platform-tagged files and fix the broken schtasks deferred cleanup along the way.

### Task 3: Extract launcher into platform-tagged files

**Files:**
- Modify: `cmd/moombox/launcher.go` (extract platform-specific bits)
- Create: `cmd/moombox/launcher_windows.go`
- Create: `cmd/moombox/launcher_unix.go`
- Modify: `cmd/moombox/main.go` (move `createNoWindow` constant)

**Goal:** Split the launcher so the build succeeds on both Windows and Linux. Define platform-specific helpers (`cleanupOrphans`, `handleUpdateRestart`, `setSysProcAttr`) and call them from the shared core in `launcher.go`.

- [x] **Step 1: Read current launcher to identify split points**

```bash
cat cmd/moombox/launcher.go
```
Identify Windows-coupled code: the `os.Remove(exePath + "~")` at line 50 (cleanup orphans), the `os.Rename(.old → ~)` block in the loop (handle update restart), the `deferDeleteOldLauncher` function (deferred cleanup), and the `cmd.SysProcAttr = ...` calls inside that function.

- [x] **Step 2: Extract Windows-specific code into `launcher_windows.go`**

Create `cmd/moombox/launcher_windows.go`:
```go
//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// createNoWindow prevents a console window from appearing when spawning
// detached processes on Windows (passed to SysProcAttr.CreationFlags).
const createNoWindow = 0x08000000

// cleanupOrphans removes any stale `~` files left over from a prior
// session. Runs once at launcher startup, before the supervised child
// is spawned. The .exe~ may exist if a prior launcher exited before
// the deferred timeout/del fired (system shutdown during the 11s
// window, antivirus blocked the cmd, etc.). Now-unlocked, removable.
func cleanupOrphans(exePath string) {
	os.Remove(exePath + "~")
}

// handleUpdateRestart runs after the child exits with exitCodeRestart.
// On Windows the launcher cannot delete its own running binary, so we
// rename the just-superseded .old to ~ to free the .old name for the
// next update. The ~ file is then deferred-cleaned on launcher exit.
func handleUpdateRestart(exePath string) {
	oldPath := exePath + ".old"
	if _, statErr := os.Stat(oldPath); statErr == nil {
		os.Rename(oldPath, exePath+"~")
	}
}

// setSysProcAttr applies Windows-only CreationFlags so the spawned
// process doesn't open a visible console window. Used for any
// fire-and-forget background spawn.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// deferDeleteOldLauncher schedules deletion of the .exe~ file via a
// detached cmd /c invocation that uses timeout.exe (Windows Vista+)
// to wait 11 seconds, then runs del. The 11s window covers normal
// launcher exit-and-handle-release time. The launcher's startup
// cleanupOrphans is the safety net if this somehow doesn't fire.
//
// History: previous schtasks-based approach was fragile (task name
// containing ~, no exit-code check, /st time wraparound, /tr quoting,
// permission context mismatches) and rarely actually deleted the
// orphan. Reverted to a single timeout/del invocation which is what
// worked historically.
func deferDeleteOldLauncher(exePath string) {
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file to clean up
	}
	delayedDel := fmt.Sprintf(
		`timeout /t 11 /nobreak >nul & del /f /q "%s" >nul 2>nul`, oldPath)
	cleanup := exec.Command("cmd", "/C", delayedDel)
	setSysProcAttr(cleanup)
	cleanup.Start() // fire-and-forget; we exit shortly anyway
}
```

- [x] **Step 3: Extract Unix-specific code into `launcher_unix.go`**

Create `cmd/moombox/launcher_unix.go`:
```go
//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// cleanupOrphans is a no-op on Linux. Linux can delete a running
// binary directly (file deletion removes the directory entry while
// keeping the inode alive for any process holding it open), so the
// child's CleanupOldBinary at startup handles everything.
func cleanupOrphans(exePath string) {}

// handleUpdateRestart is a no-op on Linux. After the child exits with
// exitCodeRestart, we just spawn a new child from the new binary path.
// The old .old is removed by the next child's CleanupOldBinary on
// startup (works on Linux because os.Remove of a locked file succeeds).
func handleUpdateRestart(exePath string) {}

// setSysProcAttr is a no-op on Linux. There's no equivalent of
// CreationFlags=createNoWindow because Linux processes don't open
// console windows the same way Windows ones do.
func setSysProcAttr(cmd *exec.Cmd) {}

// deferDeleteOldLauncher is a no-op on Linux. No deferred cleanup
// needed because Linux has no orphan files to clean.
func deferDeleteOldLauncher(exePath string) {}
```

- [x] **Step 4: Refactor `launcher.go` to use the platform helpers**

Modify `cmd/moombox/launcher.go`:
```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// launchAndSupervise is the launcher/supervisor loop. It spawns moombox
// as a child process in the same console and waits. If the child exits
// with exitCodeRestart (config change or update applied), it respawns —
// picking up any new binary on disk. For any other exit code it
// propagates and exits.
//
// This keeps one stable parent holding the console connection so the
// child's BubbleTea properly restores terminal state, and avoids
// process chain buildup since the launcher always swaps to a fresh
// child rather than nesting.
//
// Platform-specific cleanup logic (handling the .exe~ orphan on
// Windows, no-op on Linux) lives in launcher_windows.go and
// launcher_unix.go.
func launchAndSupervise() {
	if err := acquireSingleInstanceLock(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		fmt.Fprintln(os.Stderr, "If you believe this is in error (the previous instance crashed), wait a few seconds and try again.")
		os.Exit(1)
	}
	defer releaseSingleInstanceLock()

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to determine executable path: %v\n", err)
		os.Exit(1)
	}

	// Clean up old launcher binary from a previous session (now unlocked).
	// Windows-only; no-op on Linux.
	cleanupOrphans(exePath)

	// Ignore interrupts in the launcher — the child handles Ctrl+C.
	signal.Ignore(os.Interrupt)

	for {
		cmd := exec.Command(exePath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "_MOOMBOX_CHILD=1")

		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if exitErr.ExitCode() == exitCodeRestart {
					// Update applied: rename .old → ~ on Windows so the
					// .old name is free for the next update. No-op on Linux.
					handleUpdateRestart(exePath)
					continue
				}
				deferDeleteOldLauncher(exePath)
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "Failed to run moombox: %v\n", err)
			deferDeleteOldLauncher(exePath)
			os.Exit(1)
		}
		// Normal exit (code 0)
		deferDeleteOldLauncher(exePath)
		os.Exit(0)
	}
}
```

The original `deferDeleteOldLauncher` function and its `schtasks`/`ping` fallback code is now in `launcher_windows.go`. Remove the entire `deferDeleteOldLauncher` body and the `import "syscall"` and `import "path/filepath"` and `import "time"` from `launcher.go` — those are no longer needed in the shared file.

- [x] **Step 5: Move `createNoWindow` out of `main.go`**

In `cmd/moombox/main.go`, find and remove:
```go
// createNoWindow prevents a console window from appearing when spawning
// detached processes on Windows (passed to SysProcAttr.CreationFlags).
const createNoWindow = 0x08000000
```
The constant is now defined in `launcher_windows.go` (Windows-only, unused on Linux). Verify nothing else in `main.go` references it:
```bash
grep -n "createNoWindow" cmd/moombox/main.go
```
Expected: no matches.

- [x] **Step 6: Verify build on both platforms**

```bash
GOOS=windows go build ./cmd/moombox/...
GOOS=linux go build ./cmd/moombox/...
```
Both must succeed.

Run the existing helpers test:
```bash
go test -v ./cmd/moombox/...
```
Expected: all existing tests still pass.

- [x] **Step 7: Commit**

```bash
git add cmd/moombox/launcher.go cmd/moombox/launcher_windows.go cmd/moombox/launcher_unix.go cmd/moombox/main.go
git commit -m "refactor(launcher): split into platform-tagged files

Extracts Windows-coupled launcher logic (createNoWindow, .exe~ rename,
deferDeleteOldLauncher) into launcher_windows.go and provides Linux
no-op equivalents in launcher_unix.go. The shared launchAndSupervise
core in launcher.go calls cleanupOrphans, handleUpdateRestart,
setSysProcAttr, and deferDeleteOldLauncher — each backed by the
platform-appropriate implementation.

createNoWindow constant moves from main.go to launcher_windows.go
since only the launcher uses it.

Linux launcher does no cleanup because Linux can delete a running
binary directly; the child's CleanupOldBinary at startup handles
.old removal on both platforms uniformly."
```

---

### Task 4: Replace broken schtasks deferred cleanup with timeout/del

This was already done in Task 3 above (the new `launcher_windows.go` uses `timeout /t 11 /nobreak & del`). This task is just a verification placeholder — confirming the intent.

- [x] **Step 1: Verify the new deferDeleteOldLauncher uses timeout**

```bash
grep -n "timeout /t\|schtasks" cmd/moombox/launcher_windows.go
```
Expected: one match for `timeout /t`, zero matches for `schtasks`.

- [x] **Step 2: Acknowledge — no separate commit**

The schtasks→timeout swap was bundled into Task 3's commit. No additional work.

---

## Phase 3: Single-instance lock on Linux

### Task 5: Replace `single_instance_other.go` no-op with flock implementation

**Files:**
- Delete: `cmd/moombox/single_instance_other.go`
- Create: `cmd/moombox/single_instance_unix.go`
- Test: `cmd/moombox/single_instance_unix_test.go`

**Goal:** Real single-instance enforcement on Linux via `syscall.Flock`. Same lifetime guarantees as the Windows mutex (kernel releases on process death, no stale locks possible).

- [x] **Step 1: Write the failing test**

Create `cmd/moombox/single_instance_unix_test.go`:
```go
//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleInstanceLockAcquireAndRelease(t *testing.T) {
	// Use a temp HOME so the test doesn't touch the user's real lock dir
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	releaseSingleInstanceLock()

	// After release, should be acquirable again
	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("second acquire after release failed: %v", err)
	}
	releaseSingleInstanceLock()
}

func TestSingleInstanceLockSecondAcquireFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer releaseSingleInstanceLock()

	// Second acquire in the same process should be a no-op success
	// (already held), not an error
	if err := acquireSingleInstanceLock(); err != nil {
		t.Errorf("second acquire (same process) should succeed, got: %v", err)
	}
}

func TestSingleInstanceLockWritesPidFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := acquireSingleInstanceLock(); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer releaseSingleInstanceLock()

	lockPath := filepath.Join(tmp, ".local", "share", "moombox", "moombox.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Error("lock file is empty; expected PID")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

```bash
GOOS=linux go test -v ./cmd/moombox/ -run TestSingleInstance
```
Expected: build error or test failure (current `single_instance_other.go` is a no-op stub that always returns nil and writes no file).

- [x] **Step 3: Delete the old no-op stub**

```bash
rm cmd/moombox/single_instance_other.go
```

- [x] **Step 4: Create the flock-based implementation**

Create `cmd/moombox/single_instance_unix.go`:
```go
//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockFile holds the open file handle whose flock we own. Kept at
// package scope so releaseSingleInstanceLock can close it. nil when no
// lock is currently held.
var lockFile *os.File

// lockDirFor returns the directory in which the lock file should live.
// Prefers $HOME/.local/share/moombox; falls back to /tmp if HOME is
// unset (rare; happens in some service contexts).
func lockDirFor() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "share", "moombox")
	}
	return "/tmp"
}

// acquireSingleInstanceLock obtains an exclusive flock on a lock file
// in the user's data dir. Returns nil if this process is the first
// moombox in the user's session. If another process already holds the
// lock (kernel-managed, released automatically on process death — no
// stale locks possible), returns a clear error.
//
// Idempotent: a second call from the same process is a no-op success.
func acquireSingleInstanceLock() error {
	if lockFile != nil {
		return nil // already held
	}

	lockDir := lockDirFor()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("create lock dir %q: %w", lockDir, err)
	}

	lockPath := filepath.Join(lockDir, "moombox.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %q: %w", lockPath, err)
	}

	// LOCK_EX (exclusive) | LOCK_NB (non-blocking — fail fast if held)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("another moombox instance is already running (lock held on %s)", lockPath)
	}

	// Truncate and write our PID for human debugging. The lock is on the
	// file descriptor, not the contents, so this doesn't affect locking.
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())

	lockFile = f
	return nil
}

// releaseSingleInstanceLock releases the flock and closes the file.
// Safe to call on a process that never acquired (no-op).
func releaseSingleInstanceLock() {
	if lockFile == nil {
		return
	}
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	lockFile.Close()
	lockFile = nil
}
```

- [x] **Step 5: Run test to verify it passes**

```bash
GOOS=linux go test -v ./cmd/moombox/ -run TestSingleInstance
```
Expected: all three tests PASS.

Also verify Windows still builds (the existing `single_instance_windows.go` is unchanged):
```bash
GOOS=windows go build ./cmd/moombox/...
```
Expected: build succeeds.

- [x] **Step 6: Commit**

```bash
git add cmd/moombox/single_instance_unix.go cmd/moombox/single_instance_unix_test.go
git rm cmd/moombox/single_instance_other.go
git commit -m "feat(launcher): add Linux single-instance lock via flock

Replaces the single_instance_other.go no-op stub with a real flock-based
implementation under \$HOME/.local/share/moombox/moombox.lock (falls back
to /tmp if HOME is unset).

flock semantics match the Windows named-mutex: kernel-managed lifetime,
released automatically on process death, no stale locks possible. Lock
file contains the PID for human debugging."
```

---

## Phase 4: Multi-platform Node sidecar embed

### Task 6: Extend `tools/fetch-node` for all three platforms

**Files:**
- Modify: `tools/fetch-node/main.go`
- Modify: `internal/bgutils/embed/.gitignore` (verify gz files excluded)
- Test: `tools/fetch-node/main_test.go`

**Goal:** Fetch Windows x64, Linux x64, and Linux arm64 Node binaries in a single run. Each platform's binary is gzipped to its own embed file. Linux releases use `.tar.xz` archives so we need an xz reader.

- [x] **Step 1: Add the xz dependency**

```bash
go get github.com/ulikunitz/xz
go mod tidy
```
Verify it appears:
```bash
grep ulikunitz go.mod
```

- [x] **Step 2: Write the failing test**

Create `tools/fetch-node/main_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestVersionStampIncludesAllPlatforms(t *testing.T) {
	stamp := versionStamp()
	for _, platform := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !strings.Contains(stamp, platform) {
			t.Errorf("versionStamp missing platform %q: %s", platform, stamp)
		}
	}
}

func TestNodeTargetsCoverAllPlatforms(t *testing.T) {
	targets := nodeTargets()
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	gotPlatforms := map[string]bool{}
	for _, tgt := range targets {
		key := tgt.goos + "-" + tgt.goarch
		gotPlatforms[key] = true
		if tgt.expectedSHA == "" {
			t.Errorf("target %s has empty expectedSHA", key)
		}
		if tgt.embedName == "" {
			t.Errorf("target %s has empty embedName", key)
		}
	}
	for _, want := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !gotPlatforms[want] {
			t.Errorf("missing target for %s", want)
		}
	}
}
```

- [x] **Step 3: Run test to verify it fails**

```bash
go test -v ./tools/fetch-node/...
```
Expected: build error (`undefined: nodeTargets`) or test failure.

- [x] **Step 4: Rewrite `tools/fetch-node/main.go` with multi-platform support**

Replace the body of `tools/fetch-node/main.go`:
```go
// fetch-node downloads pinned Node.js binaries for Windows x64, Linux x64,
// and Linux arm64. Each platform's `node` binary (or `node.exe` on Windows)
// is extracted, gzipped, and written to internal/bgutils/embed/ behind a
// platform-specific filename. The Moombox build embeds the matching blob
// via go:embed under build tags.
//
// Usage (from repo root):
//
//	go run ./tools/fetch-node
//
// Idempotent: if internal/bgutils/embed/version.txt already matches the
// pinned manifest, this tool exits 0 without re-downloading.
//
// Bumping the pinned version:
//  1. Pick a new Node v22 LTS patch from https://nodejs.org/dist/index.json
//  2. Fetch SHASUMS256.txt for that release; copy the per-platform SHAs.
//  3. Update nodeVersion + per-target expectedSHA constants below.
//  4. `go run ./tools/fetch-node` to refresh all three embeds.
//  5. `MOOMBOX_LIVE_BG_TEST=1 go test ./internal/bgutils/...` to confirm.
//  6. Commit internal/bgutils/embed/version.txt only -- the .gz blobs are
//     gitignored and CI rebuilds them.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// Pinned Node.js v22 LTS release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-04-26 — v22.22.2 was the latest v22 LTS (Jod) at the
// time the sidecar landed.
const nodeVersion = "v22.22.2"

// nodeTarget describes one platform's Node release artifact.
type nodeTarget struct {
	goos, goarch string
	archiveType  string // "zip" (windows) or "tar.xz" (linux)
	binaryName   string // "node.exe" or "node"
	embedName    string // file in internal/bgutils/embed/
	urlInfix     string // "win-x64" / "linux-x64" / "linux-arm64"
	expectedSHA  string // SHA-256 of the downloaded archive (from SHASUMS256.txt)
}

// nodeTargets returns the per-platform Node binary download manifest.
// SHA-256 values come from https://nodejs.org/dist/<nodeVersion>/SHASUMS256.txt.
// Each entry's expectedSHA is the line ending in the matching archive name.
func nodeTargets() []nodeTarget {
	return []nodeTarget{
		{
			goos: "windows", goarch: "amd64",
			archiveType: "zip", binaryName: "node.exe",
			embedName: "node-windows-amd64.gz", urlInfix: "win-x64",
			expectedSHA: "7c93e9d92bf68c07182b471aa187e35ee6cd08ef0f24ab060dfff605fcc1c57c",
		},
		{
			goos: "linux", goarch: "amd64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-amd64.gz", urlInfix: "linux-x64",
			// TODO(plan-executor): replace with the SHA from SHASUMS256.txt
			// for node-v22.22.2-linux-x64.tar.xz BEFORE running fetch-node.
			// Look it up at https://nodejs.org/dist/v22.22.2/SHASUMS256.txt
			expectedSHA: "0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			goos: "linux", goarch: "arm64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-arm64.gz", urlInfix: "linux-arm64",
			// TODO(plan-executor): replace with the SHA from SHASUMS256.txt
			// for node-v22.22.2-linux-arm64.tar.xz BEFORE running fetch-node.
			expectedSHA: "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-node:", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	embedDir := filepath.Join(repoRoot, "internal", "bgutils", "embed")
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		return fmt.Errorf("mkdir embed: %w", err)
	}

	versionPath := filepath.Join(embedDir, "version.txt")
	wantStamp := versionStamp()

	// Idempotency: skip if every target already exists and version.txt
	// matches the pinned manifest.
	if existing, _ := os.ReadFile(versionPath); strings.TrimSpace(string(existing)) == wantStamp {
		allPresent := true
		for _, tgt := range nodeTargets() {
			if _, err := os.Stat(filepath.Join(embedDir, tgt.embedName)); err != nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			fmt.Printf("fetch-node: already up to date (%s)\n", wantStamp)
			return nil
		}
	}

	for _, tgt := range nodeTargets() {
		if err := fetchOne(embedDir, tgt); err != nil {
			return fmt.Errorf("%s/%s: %w", tgt.goos, tgt.goarch, err)
		}
	}

	if err := os.WriteFile(versionPath, []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write version.txt: %w", err)
	}
	fmt.Printf("fetch-node: %s\n", wantStamp)
	return nil
}

func fetchOne(embedDir string, tgt nodeTarget) error {
	url := fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-%s.%s",
		nodeVersion, nodeVersion, tgt.urlInfix, tgt.archiveType)
	fmt.Printf("fetch-node: downloading %s\n", url)
	archiveBytes, err := download(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	gotSHA := hex.EncodeToString(sha256Sum(archiveBytes))
	if gotSHA != tgt.expectedSHA {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s",
			tgt.embedName, gotSHA, tgt.expectedSHA)
	}
	fmt.Printf("fetch-node: SHA-256 verified (%s)\n", gotSHA)

	var binBytes []byte
	switch tgt.archiveType {
	case "zip":
		binBytes, err = extractFromZip(archiveBytes, tgt.binaryName)
	case "tar.xz":
		binBytes, err = extractFromTarXz(archiveBytes, tgt.binaryName)
	default:
		return fmt.Errorf("unknown archiveType %q", tgt.archiveType)
	}
	if err != nil {
		return fmt.Errorf("extract %s: %w", tgt.binaryName, err)
	}
	fmt.Printf("fetch-node: extracted %s (%.1f MB)\n",
		tgt.binaryName, float64(len(binBytes))/1024.0/1024.0)

	gzPath := filepath.Join(embedDir, tgt.embedName)
	if err := writeGzipped(gzPath, binBytes); err != nil {
		return fmt.Errorf("gzip write: %w", err)
	}
	if info, err := os.Stat(gzPath); err == nil {
		fmt.Printf("fetch-node: wrote %s (%.1f MB gzipped)\n",
			gzPath, float64(info.Size())/1024.0/1024.0)
	}
	return nil
}

func versionStamp() string {
	parts := []string{fmt.Sprintf("node@%s", nodeVersion)}
	for _, tgt := range nodeTargets() {
		parts = append(parts, fmt.Sprintf("%s-%s@%s", tgt.goos, tgt.goarch, tgt.expectedSHA))
	}
	return strings.Join(parts, " ")
}

func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found above %s; run from inside the Moombox repo", cwd)
}

func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func extractFromZip(zipBytes []byte, binaryName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != binaryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip entry %q: %w", f.Name, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s not found in zip archive", binaryName)
}

func extractFromTarXz(xzBytes []byte, binaryName string) ([]byte, error) {
	xr, err := xz.NewReader(bytes.NewReader(xzBytes))
	if err != nil {
		return nil, fmt.Errorf("xz reader: %w", err)
	}
	tr := tar.NewReader(xr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar header: %w", err)
		}
		// Linux Node tarballs put node at bin/node under a versioned dir,
		// e.g. node-v22.22.2-linux-x64/bin/node
		if filepath.Base(hdr.Name) == binaryName && strings.Contains(hdr.Name, "/bin/") {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("%s not found in tar.xz archive", binaryName)
}

func writeGzipped(outPath string, raw []byte) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Sync()
}
```

- [x] **Step 5: Look up real SHAs and replace placeholders**

```bash
curl https://nodejs.org/dist/v22.22.2/SHASUMS256.txt | grep -E "linux-x64\.tar\.xz|linux-arm64\.tar\.xz"
```
Take the two SHA hex strings shown and replace the `0000...` placeholders in `nodeTargets()`. Re-run the test:
```bash
go test -v ./tools/fetch-node/...
```
Expected: `TestNodeTargetsCoverAllPlatforms` and `TestVersionStampIncludesAllPlatforms` both PASS.

- [x] **Step 6: Run fetch-node end-to-end to populate embeds**

```bash
go run ./tools/fetch-node
```
Expected: downloads three archives (~30-50 MB each), prints SHA verification for each, writes three `.gz` files to `internal/bgutils/embed/`, writes new `version.txt`. Total time ~30-90 seconds depending on network.

Verify output:
```bash
ls -la internal/bgutils/embed/*.gz
cat internal/bgutils/embed/version.txt
```

- [x] **Step 7: Verify .gitignore excludes the new gz files**

```bash
cat internal/bgutils/embed/.gitignore
```
If only `node.exe.gz` is listed, update to:
```
# Per-platform Node binaries fetched by tools/fetch-node — not committed.
node-windows-amd64.gz
node-linux-amd64.gz
node-linux-arm64.gz
node.exe.gz
sidecar.tar.gz
```
The old `node.exe.gz` line is kept for backward compat with any leftover from before this change.

- [x] **Step 8: Commit**

```bash
git add tools/fetch-node/main.go tools/fetch-node/main_test.go go.mod go.sum
git add internal/bgutils/embed/.gitignore internal/bgutils/embed/version.txt
git commit -m "feat(fetch-node): fetch all three platform Node binaries

Extends tools/fetch-node to fetch and gzip Windows x64, Linux x64, and
Linux arm64 Node v22.22.2 binaries. Linux uses tar.xz archives via
github.com/ulikunitz/xz (pure Go).

Each platform writes to a distinct embed file (node-windows-amd64.gz /
node-linux-amd64.gz / node-linux-arm64.gz). version.txt manifest covers
all three SHAs so cache invalidation triggers on any platform's bump."
```

---

### Task 7: Build-tagged embed files in `internal/bgutils/embed`

**Files:**
- Modify: `internal/bgutils/embed/embed.go` (split or rewrite)
- Create: `internal/bgutils/embed/embed_windows_amd64.go`
- Create: `internal/bgutils/embed/embed_linux_amd64.go`
- Create: `internal/bgutils/embed/embed_linux_arm64.go`

**Goal:** Replace the single hardcoded `//go:embed node.exe.gz` with per-platform embeds behind build tags. The exported `EmbeddedNode` variable stays the same shape so downstream consumers don't need to change.

- [x] **Step 1: Read current embed.go**

```bash
cat internal/bgutils/embed/embed.go
```
Note what variables and embeds exist. Likely something like:
```go
//go:embed node.exe.gz
var EmbeddedNode []byte

//go:embed sidecar.tar.gz
var EmbeddedSidecar []byte
```

- [x] **Step 2: Refactor `embed.go` to keep only platform-agnostic embeds**

Edit `internal/bgutils/embed/embed.go` so it only declares what's shared (`EmbeddedSidecar` for the JS sidecar tarball — that's platform-agnostic). Move `EmbeddedNode` declaration out — it'll be defined per-platform. Keep package documentation:

```go
// Package embed bundles the Node.js binary and the bgutil-sidecar JS
// tarball for the BotGuard sidecar. The Node binary is platform-specific
// and lives in embed_<goos>_<goarch>.go files behind build tags. The
// sidecar tarball is platform-agnostic JavaScript and embedded once here.
package embed

import _ "embed"

//go:embed sidecar.tar.gz
var EmbeddedSidecar []byte
```

- [x] **Step 3: Create per-platform embed files**

Create `internal/bgutils/embed/embed_windows_amd64.go`:
```go
//go:build windows && amd64

package embed

import _ "embed"

//go:embed node-windows-amd64.gz
var EmbeddedNode []byte
```

Create `internal/bgutils/embed/embed_linux_amd64.go`:
```go
//go:build linux && amd64

package embed

import _ "embed"

//go:embed node-linux-amd64.gz
var EmbeddedNode []byte
```

Create `internal/bgutils/embed/embed_linux_arm64.go`:
```go
//go:build linux && arm64

package embed

import _ "embed"

//go:embed node-linux-arm64.gz
var EmbeddedNode []byte
```

- [x] **Step 4: Verify build on all three platforms**

```bash
GOOS=windows GOARCH=amd64 go build ./internal/bgutils/embed/
GOOS=linux GOARCH=amd64 go build ./internal/bgutils/embed/
GOOS=linux GOARCH=arm64 go build ./internal/bgutils/embed/
```
All three must succeed. Each platform's build only references its own embed file because of the build tags.

Run any existing bgutils embed tests:
```bash
go test ./internal/bgutils/embed/...
```
Expected: PASS.

- [x] **Step 5: Verify downstream consumers still compile**

The runtime extraction code in the bgutils package should consume `embed.EmbeddedNode` as a `[]byte`. Verify it builds on all three:
```bash
GOOS=windows GOARCH=amd64 go build ./internal/bgutils/...
GOOS=linux GOARCH=amd64 go build ./internal/bgutils/...
GOOS=linux GOARCH=arm64 go build ./internal/bgutils/...
```
Expected: all succeed.

- [x] **Step 6: Commit**

```bash
git add internal/bgutils/embed/embed.go internal/bgutils/embed/embed_windows_amd64.go internal/bgutils/embed/embed_linux_amd64.go internal/bgutils/embed/embed_linux_arm64.go
git commit -m "refactor(bgutils-embed): per-platform Node binary embeds

Splits EmbeddedNode declaration into three build-tagged files
(embed_windows_amd64.go, embed_linux_amd64.go, embed_linux_arm64.go),
each embedding the matching gz file from tools/fetch-node. The
EmbeddedSidecar (platform-agnostic JS tarball) stays in embed.go.

Downstream consumers see the same []byte variable name; no API change."
```

---

## Phase 5: Updater changes

### Task 8: Add `~` to CleanupOldBinary sweep on Windows

**Files:**
- Modify: `internal/updater/updater.go` (CleanupOldBinary function around line 331)
- Test: `internal/updater/updater_test.go` (extend existing TestCleanupOldBinaryRemovesStaleArtifacts)

**Goal:** Catch any orphaned `~` files that escaped the launcher's startup sweep AND the deferred timeout/del. Belt-and-suspenders. Windows-only because Linux has no `~` orphans (and `moombox~` could be a legitimate editor backup file there).

- [x] **Step 1: Read existing CleanupOldBinary and its test**

```bash
grep -n "CleanupOldBinary" internal/updater/updater.go
grep -n "TestCleanupOldBinaryRemovesStaleArtifacts" internal/updater/updater_test.go
```

- [x] **Step 2: Extend the existing test to cover `~` on Windows**

Find `TestCleanupOldBinaryRemovesStaleArtifacts` in `internal/updater/updater_test.go`. Modify the `stale` slice to conditionally include `~`:

```go
func TestCleanupOldBinaryRemovesStaleArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	stale := []string{exePath + ".old", exePath + ".new", exePath + ".new.sig", exePath + ".sig"}
	if runtime.GOOS == "windows" {
		stale = append(stale, exePath+"~")
	}
	preserved := exePath + ".update-broken"
	for _, p := range append(stale, preserved) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	u.CleanupOldBinary()

	for _, p := range stale {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale file not removed: %s", p)
		}
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Errorf("preserved marker was deleted: %v", err)
	}
}
```

Add `"runtime"` to the test file's imports if not already present.

- [x] **Step 3: Run test to verify it fails on Windows**

```bash
GOOS=windows go test -v ./internal/updater/ -run TestCleanupOldBinaryRemovesStaleArtifacts
```
Expected: FAIL on Windows with "stale file not removed: ...~". On Linux: still passes (no `~` added to stale list).

- [x] **Step 4: Update CleanupOldBinary to sweep `~` on Windows**

Edit `internal/updater/updater.go` around line 331:
```go
// CleanupOldBinary removes stale files left over from previous updates:
// .old (previous binary), .new (interrupted download), .new.sig (interrupted
// verification), and .sig (VerifyCurrentSignature intermediate that may be
// left behind if ApplyUpdate was interrupted between its write and rename).
//
// On Windows, also sweeps `~` (orphaned by the launcher's deferred cleanup
// or by a prior installation that lacked the launcher startup sweep). The
// runtime.GOOS guard prevents accidentally targeting an editor backup file
// on Linux/macOS where `<name>~` is a legitimate file pattern.
//
// .update-broken markers from a failed double-rename rollback are
// intentionally NOT cleaned here — they are evidence for the user that
// manual recovery may be needed and should be deleted explicitly.
func (u *Updater) CleanupOldBinary() {
	suffixes := []string{".old", ".new", ".new.sig", ".sig"}
	if runtime.GOOS == "windows" {
		suffixes = append(suffixes, "~")
	}
	for _, suffix := range suffixes {
		path := u.exePath + suffix
		if _, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				u.logger.Warn("[Updater] Failed to remove stale file",
					"path", path,
					"error", err.Error(),
				)
			} else {
				u.logger.Info("[Updater] Cleaned up stale file", "path", path)
			}
		}
	}
}
```

Add `"runtime"` to the import block if not already imported.

- [x] **Step 5: Run test to verify it passes**

```bash
GOOS=windows go test -v ./internal/updater/ -run TestCleanupOldBinaryRemovesStaleArtifacts
GOOS=linux go test -v ./internal/updater/ -run TestCleanupOldBinaryRemovesStaleArtifacts
```
Both: PASS.

- [x] **Step 6: Commit**

```bash
git add internal/updater/updater.go internal/updater/updater_test.go
git commit -m "feat(updater): sweep ~ orphans on Windows in CleanupOldBinary

Belt-and-suspenders defense for the rare case where both the launcher's
startup os.Remove(\"~\") and the deferred timeout/del cleanup miss an
orphaned ~ file. Windows-only because <name>~ is a legitimate editor
backup pattern on Linux/macOS."
```

---

### Task 9: Platform-aware updater asset matching

**Files:**
- Modify: `internal/updater/updater.go` (asset name lookup around lines 132-142, 290-296)
- Test: `internal/updater/updater_test.go`

**Goal:** Replace the hardcoded `Moombox.exe` / `Moombox.exe.sig` asset matching with a platform-aware lookup table so Linux clients can find their own assets. Windows entry stays mapped to `Moombox.exe` so existing 2.6.2 clients continue to work.

- [x] **Step 1: Read existing asset matching code**

```bash
grep -n "Moombox.exe" internal/updater/updater.go
```
Note the two locations: in the release-fetching code (sets `downloadURL` and `signatureURL`), and in `VerifyCurrentSignature` (sets `signatureURL` only).

- [x] **Step 2: Write the failing test**

Add to `internal/updater/updater_test.go`:
```go
func TestCurrentPlatformAssetsKnownPlatforms(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantBinary   string
		wantSig      string
	}{
		{"windows", "amd64", "Moombox.exe", "Moombox.exe.sig"},
		{"linux", "amd64", "moombox-linux-amd64", "moombox-linux-amd64.sig"},
		{"linux", "arm64", "moombox-linux-arm64", "moombox-linux-arm64.sig"},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			a, ok := assetsForPlatform(tc.goos, tc.goarch)
			if !ok {
				t.Fatalf("expected assets for %s/%s, got !ok", tc.goos, tc.goarch)
			}
			if a.binary != tc.wantBinary {
				t.Errorf("binary: got %q, want %q", a.binary, tc.wantBinary)
			}
			if a.sig != tc.wantSig {
				t.Errorf("sig: got %q, want %q", a.sig, tc.wantSig)
			}
		})
	}
}

func TestCurrentPlatformAssetsUnknown(t *testing.T) {
	if _, ok := assetsForPlatform("plan9", "amd64"); ok {
		t.Error("expected !ok for unknown platform")
	}
	if _, ok := assetsForPlatform("linux", "mips"); ok {
		t.Error("expected !ok for unsupported arch")
	}
}
```

- [x] **Step 3: Run test to verify it fails**

```bash
go test -v ./internal/updater/ -run TestCurrentPlatformAssets
```
Expected: build error (`undefined: assetsForPlatform, currentPlatformAssets`).

- [x] **Step 4: Add the platform lookup table**

In `internal/updater/updater.go`, add near the top (below the `ReleaseInfo` type):
```go
// assetNames bundles the GitHub release asset names for one platform.
// binary is the runnable executable; sig is its Ed25519 signature.
type assetNames struct {
	binary, sig string
}

// releaseAssetMap maps GOOS/GOARCH to the asset names CI publishes.
// Adding a new platform: extend this map AND ensure the release workflow
// uploads matching artifacts. The Windows entry keeps the historical
// Moombox.exe name so existing 2.6.2 clients continue to find it.
var releaseAssetMap = map[string]assetNames{
	"windows/amd64": {binary: "Moombox.exe", sig: "Moombox.exe.sig"},
	"linux/amd64":   {binary: "moombox-linux-amd64", sig: "moombox-linux-amd64.sig"},
	"linux/arm64":   {binary: "moombox-linux-arm64", sig: "moombox-linux-arm64.sig"},
}

// assetsForPlatform looks up the asset names for an explicit goos/goarch
// (used by tests). Production code calls currentPlatformAssets() below.
func assetsForPlatform(goos, goarch string) (assetNames, bool) {
	a, ok := releaseAssetMap[goos+"/"+goarch]
	return a, ok
}

// currentPlatformAssets returns the asset names for the running build's
// GOOS/GOARCH, sourced from runtime.GOOS and runtime.GOARCH.
func currentPlatformAssets() (assetNames, bool) {
	return assetsForPlatform(runtime.GOOS, runtime.GOARCH)
}
```

- [x] **Step 5: Replace the hardcoded asset matching in `getRelease`**

Find the loop that picks `downloadURL` and `signatureURL` (around lines 134-142):
```go
// Find the Moombox.exe and Moombox.exe.sig assets
var downloadURL, signatureURL string
for _, asset := range release.Assets {
    switch {
    case strings.EqualFold(asset.Name, "Moombox.exe"):
        downloadURL = asset.BrowserDownloadURL
    case strings.EqualFold(asset.Name, "Moombox.exe.sig"):
        signatureURL = asset.BrowserDownloadURL
    }
}
```

Replace with:
```go
// Find the platform-appropriate binary and sig assets.
assets, ok := currentPlatformAssets()
if !ok {
    return nil, fmt.Errorf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
var downloadURL, signatureURL string
for _, asset := range release.Assets {
    switch {
    case strings.EqualFold(asset.Name, assets.binary):
        downloadURL = asset.BrowserDownloadURL
    case strings.EqualFold(asset.Name, assets.sig):
        signatureURL = asset.BrowserDownloadURL
    }
}
```

- [x] **Step 6: Replace the hardcoded asset matching in `VerifyCurrentSignature`**

Find the similar block around line 290-296:
```go
// Find the .sig asset
var signatureURL string
for _, asset := range release.Assets {
    if strings.EqualFold(asset.Name, "Moombox.exe.sig") {
        signatureURL = asset.BrowserDownloadURL
        break
    }
}
```

Replace with:
```go
// Find the platform-appropriate sig asset.
assets, ok := currentPlatformAssets()
if !ok {
    return fmt.Errorf("signature verification unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
var signatureURL string
for _, asset := range release.Assets {
    if strings.EqualFold(asset.Name, assets.sig) {
        signatureURL = asset.BrowserDownloadURL
        break
    }
}
```

- [x] **Step 7: Run all updater tests on both platforms**

```bash
GOOS=windows go test -v ./internal/updater/...
GOOS=linux go test -v ./internal/updater/...
```
Expected: all PASS, including the new `TestCurrentPlatformAssetsKnownPlatforms` and `TestCurrentPlatformAssetsUnknown`.

The existing tests that mock GitHub releases with `Moombox.exe` assets continue to work on Windows because the lookup table maps `windows/amd64 → Moombox.exe`. On Linux those tests would fail because the mocks return Windows asset names — verify the existing tests use `runtime.GOOS == "windows"` skip guards or update them to test against the platform-appropriate asset name. Inspect any test failures and either adapt the mock data to match the test platform or add a build constraint (`//go:build windows`) to tests that are inherently Windows-flow specific.

- [x] **Step 8: Commit**

```bash
git add internal/updater/updater.go internal/updater/updater_test.go
git commit -m "feat(updater): platform-aware release asset matching

Replaces hardcoded Moombox.exe / Moombox.exe.sig asset name lookup with
a platform map (windows/amd64, linux/amd64, linux/arm64). The Windows
entry keeps the historical Moombox.exe name so existing 2.6.2 clients
continue to find their asset; new Linux entries enable Linux clients
to find their own platform's binary."
```

---

## Phase 6: CI workflow

### Task 10: Split `release.yml` into Windows + Linux jobs

**Files:**
- Modify: `.github/workflows/release.yml`

**Goal:** Add a Linux job that builds amd64 and arm64 binaries, signs them, and uploads to the same GitHub release. Windows job continues to build the body and create the release; Linux job appends its assets.

- [x] **Step 1: Read current release.yml end-to-end**

```bash
cat .github/workflows/release.yml
```
Note the existing job structure: single `release` job on `windows-latest`, with steps for sidecar build, fetch-node, go-winres, build, sign, build release body, create release.

- [x] **Step 2: Restructure into two parallel jobs**

Replace the entire `jobs:` section in `.github/workflows/release.yml`:
```yaml
jobs:
  windows:
    runs-on: windows-latest
    outputs:
      release_id: ${{ steps.create_release.outputs.id }}
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - uses: actions/setup-node@v4
        with:
          node-version: '22'

      - name: Build BotGuard sidecar payload
        shell: bash
        run: |
          cd bgutil-sidecar
          npm ci --omit=dev --no-audit --no-fund --ignore-scripts
          node build.mjs
          cd ..

      - name: Fetch embedded Node binaries (all platforms)
        run: go run ./tools/fetch-node

      - name: Generate Windows resources
        run: |
          $version = "${{ github.ref_name }}".TrimStart("v")
          $commit = (git rev-parse --short HEAD)
          $versionString = "$version ($commit)"
          $numericBase = ($version -split '-')[0]
          $numericVersion = "$numericBase.0"
          go install github.com/tc-hib/go-winres@latest
          $json = Get-Content cmd/moombox/winres/winres.json -Raw | ConvertFrom-Json
          $json.RT_MANIFEST.'#1'.'0409'.identity.version = $numericVersion
          $fixed = $json.RT_VERSION.'#1'.'0000'.fixed
          $fixed.file_version = $numericVersion
          $fixed.product_version = $numericVersion
          $info = $json.RT_VERSION.'#1'.'0000'.info.'0409'
          $info.FileVersion = $versionString
          $info.ProductVersion = $versionString
          $json | ConvertTo-Json -Depth 10 | Set-Content cmd/moombox/winres/winres.json
          Push-Location cmd/moombox
          go-winres make --arch amd64
          Pop-Location

      - name: Build Windows executable
        env:
          CGO_ENABLED: 0
          GOOS: windows
          GOARCH: amd64
        run: |
          $version = "${{ github.ref_name }}".TrimStart("v")
          $commit = git rev-parse --short HEAD
          go build -ldflags "-s -w -X main.version=$version -X main.commit=$commit" -o Moombox.exe ./cmd/moombox

      - name: Sign binary
        env:
          SIGNING_KEY: ${{ secrets.SIGNING_KEY }}
        run: go run ./cmd/sign Moombox.exe

      - name: Build release body
        shell: bash
        env:
          TAG_NAME: ${{ github.ref_name }}
          REPO: ${{ github.repository }}
        run: |
          WIN_LINK="[**\`Download Moombox.exe for Windows (x64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/Moombox.exe)"
          LIN_AMD_LINK="[**\`Download moombox-linux-amd64 for Linux (x64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/moombox-linux-amd64)"
          LIN_ARM_LINK="[**\`Download moombox-linux-arm64 for Linux (arm64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/moombox-linux-arm64)"

          if [ -f RELEASE_NOTES.md ] && [ -s RELEASE_NOTES.md ]; then
            {
              echo "$WIN_LINK"
              echo "$LIN_AMD_LINK"
              echo "$LIN_ARM_LINK"
              echo ""
              echo "---"
              echo ""
              cat RELEASE_NOTES.md
            } > release_body.md
            echo "use_github_notes=false" >> "$GITHUB_ENV"
          else
            {
              echo "$WIN_LINK"
              echo "$LIN_AMD_LINK"
              echo "$LIN_ARM_LINK"
            } > release_body.md
            echo "use_github_notes=true" >> "$GITHUB_ENV"
          fi

      - name: Create GitHub Release
        id: create_release
        uses: softprops/action-gh-release@v2
        with:
          body_path: release_body.md
          generate_release_notes: ${{ env.use_github_notes == 'true' }}
          prerelease: ${{ contains(github.ref_name, '-') }}
          make_latest: ${{ contains(github.ref_name, '-') && 'false' || 'true' }}
          files: |
            Moombox.exe
            Moombox.exe.sig

  linux:
    runs-on: ubuntu-latest
    needs: windows  # release must exist before linux uploads to it
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - uses: actions/setup-node@v4
        with:
          node-version: '22'

      - name: Build BotGuard sidecar payload
        run: |
          cd bgutil-sidecar
          npm ci --omit=dev --no-audit --no-fund --ignore-scripts
          node build.mjs
          cd ..

      - name: Fetch embedded Node binaries (all platforms)
        run: go run ./tools/fetch-node

      - name: Build Linux amd64 executable
        env:
          CGO_ENABLED: 0
          GOOS: linux
          GOARCH: amd64
        run: |
          VERSION="${GITHUB_REF_NAME#v}"
          COMMIT=$(git rev-parse --short HEAD)
          go build -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o moombox-linux-amd64 ./cmd/moombox

      - name: Build Linux arm64 executable
        env:
          CGO_ENABLED: 0
          GOOS: linux
          GOARCH: arm64
        run: |
          VERSION="${GITHUB_REF_NAME#v}"
          COMMIT=$(git rev-parse --short HEAD)
          go build -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o moombox-linux-arm64 ./cmd/moombox

      - name: Sign Linux binaries
        env:
          SIGNING_KEY: ${{ secrets.SIGNING_KEY }}
        run: |
          go run ./cmd/sign moombox-linux-amd64
          go run ./cmd/sign moombox-linux-arm64

      - name: Upload Linux assets to release
        uses: softprops/action-gh-release@v2
        with:
          # Reuse the existing release created by the windows job; don't
          # overwrite the body or release_notes flags here. action-gh-release
          # appends new files to the matching tag.
          files: |
            moombox-linux-amd64
            moombox-linux-amd64.sig
            moombox-linux-arm64
            moombox-linux-arm64.sig
```

- [x] **Step 3: Verify the workflow YAML parses**

```bash
# yamllint or just visual inspection
grep -c "^  windows:\|^  linux:" .github/workflows/release.yml
```
Expected output: `2` (one for each job).

- [x] **Step 4: Commit (CI verification happens at next tag push)**

```bash
git add .github/workflows/release.yml
git commit -m "ci(release): split into parallel windows + linux jobs

Windows job builds Moombox.exe, signs, builds the release body with all
three platform download links, and creates the GitHub release. Linux job
builds amd64 + arm64 binaries, signs, and uploads them to the existing
release via softprops/action-gh-release v2's append behaviour.

Release body now includes Linux x64 and arm64 download links alongside
the existing Windows link."
```

---

### Task 11: Add `linux-test` workflow

**Files:**
- Create: `.github/workflows/linux-test.yml`

**Goal:** Run `go build ./...` and `go test ./...` for both Linux arches on every PR/push so cross-platform regressions are caught before tagging a release.

- [x] **Step 1: Create the workflow**

Create `.github/workflows/linux-test.yml`:
```yaml
name: Linux Build & Test

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  build-test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        goarch: [amd64, arm64]
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - uses: actions/setup-node@v4
        with:
          node-version: '22'

      - name: Build BotGuard sidecar payload
        run: |
          cd bgutil-sidecar
          npm ci --omit=dev --no-audit --no-fund --ignore-scripts
          node build.mjs
          cd ..

      - name: Fetch embedded Node binaries
        run: go run ./tools/fetch-node

      - name: Build for linux/${{ matrix.goarch }}
        env:
          CGO_ENABLED: 0
          GOOS: linux
          GOARCH: ${{ matrix.goarch }}
        run: go build ./...

      - name: Vet
        env:
          GOOS: linux
          GOARCH: ${{ matrix.goarch }}
        run: go vet ./...

      # Tests only run for amd64 because arm64 needs QEMU on x86 runners.
      # The build step above proves arm64 compiles; behaviour is verified
      # via the amd64 test run.
      - name: Test (amd64 only)
        if: matrix.goarch == 'amd64'
        env:
          GOOS: linux
          GOARCH: amd64
        run: go test ./...
```

- [x] **Step 2: Verify YAML parses**

```bash
grep -c "matrix:" .github/workflows/linux-test.yml
```
Expected: `1`.

- [x] **Step 3: Commit**

```bash
git add .github/workflows/linux-test.yml
git commit -m "ci: add linux-test workflow for build/test on PR

Runs go build and go vet for both linux/amd64 and linux/arm64 on every
PR and push to main. Catches Linux compilation regressions before they
reach a release tag.

Tests run for amd64 only because arm64 would need QEMU on x86 runners;
the cross-compile build step proves arm64 compiles, and behaviour
verification via amd64 tests is sufficient."
```

---

## Phase 7: Browser-selection dropdown

This phase is independent of phases 1-6 and can be implemented in parallel by a different agent.

### Task 12: Add `DetectBrowsers()` (plural) and config fields

**Files:**
- Modify: `internal/cookies/autocookies_detect.go`
- Modify: `internal/config/config.go` (add cookies fields)
- Test: `internal/cookies/autocookies_detect_test.go`

**Goal:** Add a function that returns ALL detected browsers (not just the best match), and add config fields for user-overridden browser path/type.

- [x] **Step 1: Read existing DetectBrowser and detectBrowserUncached**

```bash
grep -n "DetectBrowser\|detectBrowserUncached" internal/cookies/autocookies_detect.go
```
Note the existing single-result function structure.

- [x] **Step 2: Write the failing test**

Add to `internal/cookies/autocookies_detect_test.go`:
```go
func TestDetectBrowsersReturnsSliceNeverNil(t *testing.T) {
	browsers := DetectBrowsers()
	if browsers == nil {
		t.Fatal("DetectBrowsers returned nil; expected empty slice")
	}
	// On a CI runner without browsers installed this may be empty.
	// We only assert non-nil to make ranges safe in callers.
	for _, b := range browsers {
		if b.Type == "" || b.Name == "" || b.Path == "" {
			t.Errorf("incomplete browser entry: %+v", b)
		}
	}
}
```

- [x] **Step 3: Run test to verify it fails**

```bash
go test -v ./internal/cookies/ -run TestDetectBrowsersReturnsSliceNeverNil
```
Expected: build error (`undefined: DetectBrowsers`).

- [x] **Step 4: Add `DetectBrowsers()` (plural)**

In `internal/cookies/autocookies_detect.go`, below the existing `DetectBrowser()` function, add:
```go
// DetectBrowsers enumerates every browser the package can find, in the
// same priority order as DetectBrowser (system default first if known,
// then knownBrowsers list with Firefox-family before Chromium-family).
// Returns an empty (non-nil) slice when none are detected so callers
// can range without nil-checks.
//
// Unlike DetectBrowser, the result is NOT cached — callers asking for
// the full list typically render a UI on top, where freshness is more
// important than the ~ms cost of re-scanning.
func DetectBrowsers() []DetectedBrowser {
	out := make([]DetectedBrowser, 0, 4)

	// Build search order: default browser first, then remaining.
	order := knownBrowsers
	if defType := detectDefaultBrowserType(); defType != "" && defType != "edge" {
		reordered := make([]browserInfo, 0, len(knownBrowsers))
		for _, b := range knownBrowsers {
			if b.typ == defType {
				reordered = append([]browserInfo{b}, reordered...)
			} else {
				reordered = append(reordered, b)
			}
		}
		order = reordered
	}

	// Build Windows install path roots once.
	var windowsRoots []string
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("PROGRAMFILES"); pf != "" {
			windowsRoots = append(windowsRoots, pf)
		}
		if pf86 := os.Getenv("PROGRAMFILES(X86)"); pf86 != "" {
			windowsRoots = append(windowsRoots, pf86)
		}
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			windowsRoots = append(windowsRoots, localApp)
		}
	}

	seen := map[string]struct{}{} // dedupe by absolute path

	addIfNew := func(b DetectedBrowser) {
		if _, dup := seen[b.Path]; dup {
			return
		}
		seen[b.Path] = struct{}{}
		out = append(out, b)
	}

	for _, b := range order {
		for _, name := range b.pathsFn() {
			if path, err := exec.LookPath(name); err == nil {
				addIfNew(DetectedBrowser{Type: b.typ, Path: path, Name: b.name})
			}
		}
		for _, relPath := range b.windowsPaths {
			for _, root := range windowsRoots {
				fullPath := filepath.Join(root, relPath)
				if _, err := os.Stat(fullPath); err == nil {
					addIfNew(DetectedBrowser{Type: b.typ, Path: fullPath, Name: b.name})
				}
			}
		}
	}

	return out
}
```

- [x] **Step 5: Add config fields**

Find the `[cookies]` config struct in `internal/config/config.go`. Add fields:
```go
// Inside the cookies section struct:
BrowserPath string `toml:"browser_path,omitempty"` // override auto-detected browser; empty = auto
BrowserType string `toml:"browser_type,omitempty"` // required when BrowserPath is set: firefox-family or chromium-family identifier
```

If there's a config validation function for the cookies section, add:
- If `BrowserPath != ""` then `BrowserType` must also be set
- `BrowserType` must be in the known list: firefox, waterfox, librewolf, zen, chrome, brave, edge, vivaldi, thorium, opera

- [x] **Step 6: Run test to verify it passes**

```bash
go test -v ./internal/cookies/ -run TestDetectBrowsersReturnsSliceNeverNil
go test ./internal/cookies/...
go test ./internal/config/...
```
Expected: PASS.

- [x] **Step 7: Commit**

```bash
git add internal/cookies/autocookies_detect.go internal/cookies/autocookies_detect_test.go internal/config/config.go
git commit -m "feat(cookies): add DetectBrowsers and browser_path config fields

DetectBrowsers returns all detected browsers (vs DetectBrowser which
returns the single best match). Used by the auto-cookies setup UI to
populate a dropdown with every available browser, plus a custom-path
option.

Config gains [cookies] browser_path and browser_type fields. Empty
defaults to existing auto-detect behaviour. When set, the user's
choice overrides DetectBrowser. Validated at config-load time."
```

---

### Task 13: Add browser-path validation function and API endpoint

**Files:**
- Create: `internal/cookies/browser_validate.go`
- Test: `internal/cookies/browser_validate_test.go`
- Modify: `internal/web/routes/cookies.go` (add validation endpoint)

**Goal:** Server-side validation that a user-specified browser path exists, is executable, and responds to `--version`. Used by the frontend before saving config.

- [x] **Step 1: Write the failing test**

Create `internal/cookies/browser_validate_test.go`:
```go
package cookies

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateBrowserPathRejectsEmpty(t *testing.T) {
	if err := ValidateBrowserPath("", "firefox"); err == nil {
		t.Error("expected error for empty path, got nil")
	}
}

func TestValidateBrowserPathRejectsNonexistent(t *testing.T) {
	if err := ValidateBrowserPath("/this/does/not/exist/anywhere", "firefox"); err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}

func TestValidateBrowserPathRejectsUnknownType(t *testing.T) {
	// Use any file that exists; we expect rejection on type, not path.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if err := ValidateBrowserPath(exe, "not-a-real-browser"); err == nil {
		t.Error("expected error for unknown browser type, got nil")
	}
}

func TestValidateBrowserPathRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows treats all files as potentially executable")
	}
	tmp := t.TempDir()
	plain := filepath.Join(tmp, "plain.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBrowserPath(plain, "firefox"); err == nil {
		t.Error("expected error for non-executable file, got nil")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

```bash
go test -v ./internal/cookies/ -run TestValidateBrowserPath
```
Expected: build error (`undefined: ValidateBrowserPath`).

- [x] **Step 3: Implement ValidateBrowserPath**

Create `internal/cookies/browser_validate.go`:
```go
package cookies

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// knownBrowserTypes must match the browser-type identifiers used by
// DetectBrowser / autocookies_chromium / autocookies_firefox so a custom
// path with a known type plugs into the existing extraction machinery.
var knownBrowserTypes = map[string]struct{}{
	"firefox": {}, "waterfox": {}, "librewolf": {}, "zen": {},
	"chrome": {}, "brave": {}, "edge": {}, "vivaldi": {}, "thorium": {}, "opera": {},
}

// ValidateBrowserPath checks that a user-specified browser path is usable:
// non-empty, file exists, has the executable bit set (Unix), the supplied
// browser type is known, and the binary responds to --version within 10s.
//
// Used by the /api/auto-cookies/validate-browser-path endpoint before
// saving the user's config selection. Returns nil when the path is good.
func ValidateBrowserPath(path, browserType string) error {
	if path == "" {
		return fmt.Errorf("browser path is empty")
	}
	if _, ok := knownBrowserTypes[browserType]; !ok {
		return fmt.Errorf("unknown browser type %q (must be one of: firefox, waterfox, librewolf, zen, chrome, brave, edge, vivaldi, thorium, opera)", browserType)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	// Executable-bit check skipped on Windows where mode bits don't work
	// the same way (Windows uses the .exe extension and ACLs).
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable (chmod +x)", path)
	}

	// Run --version with a short timeout. Any non-error exit means the
	// binary at least starts up; we don't parse the output because each
	// browser formats it differently.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %q --version failed: %w", path, err)
	}
	return nil
}
```

- [x] **Step 4: Add the validation endpoint**

In `internal/web/routes/cookies.go` (or wherever auto-cookies endpoints live — locate by grep if unsure):
```bash
grep -rn "auto-cookies/status\|/auto-cookies/" internal/web/routes/
```

Add a new route handler:
```go
// POST /api/auto-cookies/validate-browser-path
// Body: { "path": "...", "type": "firefox" }
// Response 200: { "valid": true }
// Response 400: { "valid": false, "error": "..." }
rl.Post("/api/auto-cookies/validate-browser-path", func(rw http.ResponseWriter, req *http.Request) {
    var body struct {
        Path string `json:"path"`
        Type string `json:"type"`
    }
    if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
        jsonError(rw, "invalid request body", http.StatusBadRequest)
        return
    }
    if err := cookies.ValidateBrowserPath(body.Path, body.Type); err != nil {
        jsonResponse(rw, map[string]any{"valid": false, "error": err.Error()})
        return
    }
    jsonResponse(rw, map[string]any{"valid": true})
})
```

Register on the rate-limited routes group (`rl`) since this triggers a subprocess. If unsure which group to use, check how other auto-cookies endpoints are wired.

- [x] **Step 5: Extend AutoCookieStatus to include AvailableBrowsers**

In `internal/cookies/autocookies.go`, modify `AutoCookieStatus`:
```go
type AutoCookieStatus struct {
    Configured            bool                      `json:"configured"`
    SetupInProgress       bool                      `json:"setupInProgress"`
    Browser               *DetectedBrowser          `json:"browser"`           // best-match, kept for backward compat
    AvailableBrowsers     []DetectedBrowser         `json:"availableBrowsers"` // new: full list for dropdown
    ConfiguredBrowserPath string                    `json:"configuredBrowserPath,omitempty"`
    ConfiguredBrowserType string                    `json:"configuredBrowserType,omitempty"`
    LastRefresh           *string                   `json:"lastRefresh"`
    LastError             *string                   `json:"lastError"`
    NeedsManualRelogin    AutoCookieReloginRequired `json:"needsManualRelogin"`
}
```

In `GetStatus()`, populate the new fields:
```go
return AutoCookieStatus{
    // ... existing fields
    Browser:               browser,
    AvailableBrowsers:     DetectBrowsers(), // new: call outside lock
    ConfiguredBrowserPath: s.configuredBrowserPath, // assuming you wire this from config
    ConfiguredBrowserType: s.configuredBrowserType,
    // ... rest
}
```

The `s.configuredBrowserPath` and `s.configuredBrowserType` need to be passed in via the constructor or read from the config store at status time. The simpler approach: read from configStore inside GetStatus:
```go
var cfgPath, cfgType string
configStore.Read(func(c *config.MoomboxConfig) {
    cfgPath = c.Cookies.BrowserPath
    cfgType = c.Cookies.BrowserType
})
```
Add the configStore as a dependency on AutoCookieService if it isn't already.

- [x] **Step 6: Run tests**

```bash
go test ./internal/cookies/...
go test ./internal/web/routes/...
go build ./...
```
Expected: PASS / clean build.

- [x] **Step 7: Commit**

```bash
git add internal/cookies/browser_validate.go internal/cookies/browser_validate_test.go internal/cookies/autocookies.go internal/web/routes/cookies.go
git commit -m "feat(cookies): browser-path validation + dropdown API surface

Adds ValidateBrowserPath (existence, executable bit, type whitelist,
--version smoke test) and POST /api/auto-cookies/validate-browser-path
for the frontend to call before save.

AutoCookieStatus gains AvailableBrowsers (full DetectBrowsers list),
ConfiguredBrowserPath, and ConfiguredBrowserType so the UI can render
a dropdown with the user's current selection highlighted."
```

---

### Task 14: Web UI browser dropdown

**Files:**
- Modify: `web/public/modules/setup.js` (auto-cookies setup step)
- Modify: `web/public/modules/settings.js` (auto-cookies settings panel)
- Modify: `web/public/index.html` (if needed for new components)

**Goal:** Replace the current "detected browser" display with a Shoelace `<sl-select>` dropdown listing all detected browsers + a "Custom path…" option. On custom-path selection, reveal text inputs for path and type.

- [x] **Step 1: Find the existing auto-cookies setup display**

```bash
grep -n "browser\|auto-cookie\|autoCookie" web/public/modules/setup.js
grep -n "browser\|auto-cookie\|autoCookie" web/public/modules/settings.js
```
Identify the function(s) that render the current single-browser display.

- [x] **Step 2: Add the dropdown component to setup.js**

In the auto-cookies setup step's render function, replace the existing browser display with:
```javascript
function renderBrowserSelector(status) {
    const wrap = document.createElement('div');
    wrap.className = 'browser-selector';

    const select = document.createElement('sl-select');
    select.label = 'Browser for cookie extraction';
    select.name = 'browser_path';
    select.value = status.configuredBrowserPath || '';

    // Auto-detect option first
    const autoOpt = document.createElement('sl-option');
    autoOpt.value = '';
    autoOpt.textContent = 'Auto-detect (recommended)';
    select.appendChild(autoOpt);

    // Detected browsers
    for (const b of status.availableBrowsers || []) {
        const opt = document.createElement('sl-option');
        opt.value = b.path;
        opt.dataset.type = b.type;
        opt.textContent = b.name;
        // Show path as a small secondary text via title attribute for tooltip
        opt.title = b.path;
        select.appendChild(opt);
    }

    // Custom path entry
    const customOpt = document.createElement('sl-option');
    customOpt.value = '__custom__';
    customOpt.textContent = 'Custom path…';
    select.appendChild(customOpt);

    wrap.appendChild(select);

    // Hidden custom-path inputs, revealed when __custom__ selected
    const customWrap = document.createElement('div');
    customWrap.className = 'custom-browser-path';
    customWrap.style.display = 'none';
    customWrap.innerHTML = `
        <sl-input id="custom-browser-path-input" placeholder="/path/to/browser/binary" label="Browser executable path"></sl-input>
        <sl-select id="custom-browser-type-input" label="Browser type">
            <sl-option value="firefox">Firefox-family (firefox, waterfox, librewolf, zen)</sl-option>
            <sl-option value="chrome">Chromium-family (chrome, brave, edge, vivaldi, thorium, opera)</sl-option>
        </sl-select>
        <div id="custom-browser-validation-msg" class="validation-msg"></div>
    `;
    wrap.appendChild(customWrap);

    select.addEventListener('sl-change', () => {
        customWrap.style.display = select.value === '__custom__' ? 'block' : 'none';
    });

    // Pre-fill custom path if config currently has one and it's not in the
    // detected list
    const detectedPaths = new Set((status.availableBrowsers || []).map(b => b.path));
    if (status.configuredBrowserPath && !detectedPaths.has(status.configuredBrowserPath)) {
        select.value = '__custom__';
        customWrap.style.display = 'block';
        wrap.querySelector('#custom-browser-path-input').value = status.configuredBrowserPath;
        // Type may be firefox-family or chromium-family identifier; normalise
        const ft = (status.configuredBrowserType || '').toLowerCase();
        const familyValue = ['firefox', 'waterfox', 'librewolf', 'zen'].includes(ft) ? 'firefox' : 'chrome';
        wrap.querySelector('#custom-browser-type-input').value = familyValue;
    }

    return wrap;
}
```

- [x] **Step 3: Add save logic with validation**

When the user clicks "Save" (or moves to next setup step), before PUT `/api/config`, validate the custom path:
```javascript
async function collectBrowserSelection(rootEl) {
    const select = rootEl.querySelector('sl-select[name="browser_path"]');
    if (select.value === '__custom__') {
        const path = rootEl.querySelector('#custom-browser-path-input').value;
        const type = rootEl.querySelector('#custom-browser-type-input').value;
        const msgEl = rootEl.querySelector('#custom-browser-validation-msg');
        msgEl.textContent = 'Validating…';
        const resp = await fetch('/api/auto-cookies/validate-browser-path', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ path, type }),
        });
        const result = await resp.json();
        if (!result.valid) {
            msgEl.textContent = `Invalid browser: ${result.error}`;
            msgEl.classList.add('error');
            throw new Error(result.error);
        }
        msgEl.textContent = '';
        msgEl.classList.remove('error');
        return { browser_path: path, browser_type: type };
    } else if (select.value === '') {
        // Auto-detect — clear any previously configured override
        return { browser_path: '', browser_type: '' };
    } else {
        // Detected browser chosen
        const opt = select.querySelector(`sl-option[value="${select.value}"]`);
        return { browser_path: select.value, browser_type: opt?.dataset.type || '' };
    }
}
```

- [x] **Step 4: Add settings.js parity**

Mirror the same dropdown component in `web/public/modules/settings.js` for the live settings panel. The render+collect logic is the same; just wire to the settings save flow.

- [x] **Step 5: Visual verification**

Build and run Moombox locally:
```bash
go build -o moombox.exe ./cmd/moombox
./moombox.exe
```
Open http://localhost:774 in a browser. Trigger setup wizard or open Settings. Verify:
- Dropdown lists all installed browsers
- Selecting a detected browser persists across reload
- Selecting "Custom path…" reveals the path + type inputs
- Submitting an invalid path shows the error message inline
- Submitting a valid path saves and the dropdown reflects it on next reload

If you can't run a browser test, at minimum confirm `go vet ./...` passes and the code blocks load without console errors.

- [x] **Step 6: Commit**

```bash
git add web/public/modules/setup.js web/public/modules/settings.js web/public/index.html
git commit -m "feat(web): browser-selection dropdown with custom path support

Replaces the single auto-detected browser display with a Shoelace
<sl-select> listing all detected browsers + a 'Custom path…' option.
Custom path entry reveals path + type inputs and validates against
POST /api/auto-cookies/validate-browser-path before save.

Default selection mirrors the configured browser_path or, if empty,
shows 'Auto-detect (recommended)' as the explicit zero-state."
```

---

### Task 15: TUI browser dropdown parity

**Files:**
- Modify: `internal/tui/setup_*.go` or wherever the auto-cookies TUI screen lives

**Goal:** Mirror the web UI's dropdown in the TUI setup/settings screens using `huh.NewSelect[string]()`.

- [x] **Step 1: Locate the TUI auto-cookies setup screen**

```bash
grep -rn "auto-cookie\|AutoCookie\|browser" internal/tui/ | grep -i "select\|setup\|browser"
```

- [x] **Step 2: Add a browser-selection field to the TUI**

Use `huh.NewSelect[string]()` with options built from `cookies.DetectBrowsers()`:
```go
import "github.com/charmbracelet/huh"

browsers := cookies.DetectBrowsers()
options := []huh.Option[string]{
    huh.NewOption("Auto-detect (recommended)", ""),
}
for _, b := range browsers {
    label := b.Name + "  " + b.Path
    options = append(options, huh.NewOption(label, b.Path))
}
options = append(options, huh.NewOption("Custom path…", "__custom__"))

var selected string
sel := huh.NewSelect[string]().
    Title("Browser for cookie extraction").
    Options(options...).
    Value(&selected)
```

If the user picks `"__custom__"`, follow with a `huh.NewInput()` for path and a `huh.NewSelect[string]()` for type (firefox-family vs chromium-family). Validate via the same `cookies.ValidateBrowserPath()` function used by the API.

The exact integration point depends on how the existing TUI setup screen is structured — read the relevant file first, then adapt the pattern. If the existing screen uses huh forms, add the new fields to the same form. If it's a custom bubbletea model, add a new view state.

- [x] **Step 3: Verify build and tests**

```bash
go build ./internal/tui/...
go test ./internal/tui/...
```
Expected: PASS.

- [x] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): browser-selection dropdown matching web UI

TUI setup/settings screens get a huh.NewSelect dropdown over the same
DetectBrowsers list used by the web UI, plus a custom-path option that
chains into a path input and type select. Same validation helper
(cookies.ValidateBrowserPath) used by the API."
```

---

## Phase 8: FFmpeg distro suggestion on Linux

### Task 16: Distro detection + suggestion endpoint

**Files:**
- Create: `internal/web/routes/ffmpeg_distro_unix.go`
- Create: `internal/web/routes/ffmpeg_distro_other.go` (Windows stub)
- Modify: `internal/web/routes/ffmpeg.go` (add new endpoint)
- Test: `internal/web/routes/ffmpeg_distro_unix_test.go`

**Goal:** Add a new endpoint that returns a distro-appropriate FFmpeg install command for Linux users. Existing Chocolatey/winget logic on Windows stays unchanged.

- [x] **Step 1: Write the failing test**

Create `internal/web/routes/ffmpeg_distro_unix_test.go`:
```go
//go:build !windows

package routes

import (
	"strings"
	"testing"
)

func TestSuggestFFmpegInstallByOSRelease(t *testing.T) {
	cases := []struct {
		name        string
		osRelease   string
		wantContains string
	}{
		{"ubuntu", `ID=ubuntu` + "\n" + `ID_LIKE=debian`, "apt install ffmpeg"},
		{"debian", `ID=debian`, "apt install ffmpeg"},
		{"fedora", `ID=fedora`, "dnf install ffmpeg"},
		{"arch", `ID=arch`, "pacman -S ffmpeg"},
		{"alpine", `ID=alpine`, "apk add ffmpeg"},
		{"unknown", `ID=void`, "https://ffmpeg.org/download.html"},
		{"empty", ``, "https://ffmpeg.org/download.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestFFmpegInstallFromOSRelease(tc.osRelease)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("for %s, got %q, want substring %q", tc.name, got, tc.wantContains)
			}
		})
	}
}
```

- [x] **Step 2: Run test to verify it fails**

```bash
GOOS=linux go test -v ./internal/web/routes/ -run TestSuggestFFmpegInstallByOSRelease
```
Expected: build error.

- [x] **Step 3: Implement the Unix file**

Create `internal/web/routes/ffmpeg_distro_unix.go`:
```go
//go:build !windows

package routes

import (
	"os"
	"strings"
)

// suggestFFmpegInstall returns a copy-pasteable shell command (or a
// fallback URL) for installing FFmpeg on the user's Linux distro.
// Reads /etc/os-release at process start; the result is cached in
// suggestFFmpegInstallCache.
func suggestFFmpegInstall() string {
	if cached := suggestFFmpegInstallCache; cached != "" {
		return cached
	}
	data, _ := os.ReadFile("/etc/os-release")
	suggestFFmpegInstallCache = suggestFFmpegInstallFromOSRelease(string(data))
	return suggestFFmpegInstallCache
}

var suggestFFmpegInstallCache string

// suggestFFmpegInstallFromOSRelease is the testable seam — pure function
// over the contents of /etc/os-release.
func suggestFFmpegInstallFromOSRelease(osRelease string) string {
	id, idLike := parseOSReleaseIDs(osRelease)
	candidates := append([]string{id}, strings.Fields(idLike)...)
	for _, c := range candidates {
		switch strings.TrimSpace(c) {
		case "debian", "ubuntu":
			return "sudo apt install ffmpeg"
		case "fedora", "rhel", "centos":
			return "sudo dnf install ffmpeg"
		case "arch", "manjaro":
			return "sudo pacman -S ffmpeg"
		case "alpine":
			return "sudo apk add ffmpeg"
		case "opensuse", "opensuse-tumbleweed", "opensuse-leap", "suse":
			return "sudo zypper install ffmpeg"
		}
	}
	return "Visit https://ffmpeg.org/download.html for installation instructions"
}

// parseOSReleaseIDs extracts ID= and ID_LIKE= from /etc/os-release contents.
func parseOSReleaseIDs(content string) (id, idLike string) {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ID="):
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		case strings.HasPrefix(line, "ID_LIKE="):
			idLike = strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`)
		}
	}
	return
}
```

- [x] **Step 4: Implement the Windows stub**

Create `internal/web/routes/ffmpeg_distro_other.go`:
```go
//go:build windows

package routes

// suggestFFmpegInstall is unused on Windows where the Chocolatey/winget
// install endpoints handle FFmpeg setup. Kept as a stub so the
// /api/ffmpeg/install-suggestion endpoint can be wired uniformly.
func suggestFFmpegInstall() string {
	return ""
}
```

- [x] **Step 5: Add the endpoint**

In `internal/web/routes/ffmpeg.go`, add to the route registration:
```go
// GET /api/ffmpeg/install-suggestion — distro-appropriate install command for Linux
r.Get("/api/ffmpeg/install-suggestion", func(rw http.ResponseWriter, req *http.Request) {
    jsonResponse(rw, map[string]any{
        "platform":   runtime.GOOS,
        "suggestion": suggestFFmpegInstall(),
    })
})
```

- [x] **Step 6: Run tests**

```bash
GOOS=linux go test -v ./internal/web/routes/ -run TestSuggestFFmpegInstallByOSRelease
GOOS=windows go build ./internal/web/routes/...
```
Expected: PASS / clean build on both.

- [x] **Step 7: Commit**

```bash
git add internal/web/routes/ffmpeg_distro_unix.go internal/web/routes/ffmpeg_distro_other.go internal/web/routes/ffmpeg.go internal/web/routes/ffmpeg_distro_unix_test.go
git commit -m "feat(web): distro-aware FFmpeg install suggestion endpoint

Adds GET /api/ffmpeg/install-suggestion which on Linux reads
/etc/os-release and returns the matching apt/dnf/pacman/apk/zypper
install command. Unknown distros and Windows return empty / fallback
to https://ffmpeg.org/download.html."
```

---

### Task 17: Web UI / TUI consume the suggestion

**Files:**
- Modify: `web/public/modules/setup.js` (FFmpeg setup step on Linux)
- Modify: `internal/tui/setup_*.go` (TUI FFmpeg setup screen)

**Goal:** When the user is on Linux, replace the Chocolatey/winget install buttons with a copy-pasteable command displayed via `<sl-input readonly>` plus a "Copy" and "I've installed FFmpeg, recheck" button.

- [x] **Step 1: Locate the FFmpeg setup step in setup.js**

```bash
grep -n "ffmpeg\|chocolatey\|winget" web/public/modules/setup.js
```

- [x] **Step 2: Branch on platform in the FFmpeg step**

In `setup.js`, where the FFmpeg install options are rendered, fetch the platform via `/api/ffmpeg/install-options` (existing) AND `/api/ffmpeg/install-suggestion` (new). Render Linux differently:

```javascript
async function renderFFmpegInstallStep(container) {
    const opts = await fetch('/api/ffmpeg/install-options').then(r => r.json());
    if (opts.platform === 'windows') {
        // Existing logic — render Choco/winget buttons
        renderWindowsFFmpegOptions(container, opts);
        return;
    }
    // Linux/other — show the install command
    const sug = await fetch('/api/ffmpeg/install-suggestion').then(r => r.json());
    container.innerHTML = `
        <p>FFmpeg is not installed. Run this command in your terminal to install it:</p>
        <sl-input id="ffmpeg-install-cmd" readonly value="${sug.suggestion}"></sl-input>
        <sl-button id="copy-cmd-btn" variant="default">Copy command</sl-button>
        <sl-button id="recheck-ffmpeg-btn" variant="primary">I've installed FFmpeg, recheck</sl-button>
    `;
    container.querySelector('#copy-cmd-btn').addEventListener('click', () => {
        navigator.clipboard.writeText(sug.suggestion);
    });
    container.querySelector('#recheck-ffmpeg-btn').addEventListener('click', async () => {
        const check = await fetch('/api/ffmpeg/check').then(r => r.json());
        if (check.valid) {
            // Advance the wizard to the next step
            // ... existing pattern
        } else {
            // Show error / stay on this step
        }
    });
}
```

- [x] **Step 3: TUI parity**

In the TUI FFmpeg setup screen, render an analogous text block + recheck action when not running on Windows:
```go
import "runtime"

if runtime.GOOS != "windows" {
    suggestion := getFFmpegInstallSuggestion() // calls the API or directly the function
    // Render in TUI: a text panel with the suggestion + a key-press to recheck
}
```

- [x] **Step 4: Visual verification**

Build and run on Linux (or use a Linux VM/WSL). Open setup wizard. Verify the FFmpeg step shows the apt/dnf/pacman command instead of the Choco/winget buttons. Click "Copy command" — check clipboard.

- [x] **Step 5: Commit**

```bash
git add web/public/modules/setup.js internal/tui/
git commit -m "feat(web,tui): show distro install command for FFmpeg on Linux

When platform is non-Windows, the FFmpeg setup step replaces the
Chocolatey/winget buttons with the distro-appropriate install command
returned by /api/ffmpeg/install-suggestion. User copies, runs in
terminal, then clicks 'recheck' to verify."
```

---

## Phase 9: Release notes & in-app rendering

### Task 18: Server-side strip + render markdown

**Files:**
- Modify: `internal/updater/updater.go` (strip + render in getRelease)
- Test: `internal/updater/updater_test.go`
- Modify: `go.mod` (add goldmark + bluemonday)

**Goal:** Strip the download-link section from the GitHub release body before exposing it, and add a server-side rendered HTML version for the web UI.

- [x] **Step 1: Add dependencies**

```bash
go get github.com/yuin/goldmark
go get github.com/microcosm-cc/bluemonday
go mod tidy
```

- [x] **Step 2: Write the failing test**

Add to `internal/updater/updater_test.go`:
```go
func TestStripDownloadLinks(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{
			name:  "with separator",
			input: "[**`Download Foo`**](url)\n\n---\n\n## Features\n- thing",
			want:  "## Features\n- thing",
		},
		{
			name:  "with multiple links",
			input: "[A](u)\n[B](u2)\n\n---\n\n# Notes",
			want:  "# Notes",
		},
		{
			name:  "no separator returns unchanged",
			input: "## Just notes\n- thing",
			want:  "## Just notes\n- thing",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripDownloadLinks(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderReleaseNotesHtmlSanitizesScripts(t *testing.T) {
	html := renderReleaseNotesHtml("## OK\n\n<script>alert(1)</script>")
	if strings.Contains(html, "<script>") {
		t.Errorf("script tag survived sanitization: %s", html)
	}
	if !strings.Contains(html, "OK") {
		t.Errorf("legitimate content stripped: %s", html)
	}
}

func TestRenderReleaseNotesHtmlRendersHeading(t *testing.T) {
	html := renderReleaseNotesHtml("## Heading\n- bullet")
	if !strings.Contains(html, "<h2>") {
		t.Errorf("expected <h2> in rendered output: %s", html)
	}
	if !strings.Contains(html, "<li>") {
		t.Errorf("expected <li> in rendered output: %s", html)
	}
}
```

- [x] **Step 3: Run tests to verify they fail**

```bash
go test -v ./internal/updater/ -run "TestStripDownloadLinks|TestRenderReleaseNotesHtml"
```
Expected: build error (`undefined: stripDownloadLinks, renderReleaseNotesHtml`).

- [x] **Step 4: Add the strip + render functions**

In `internal/updater/updater.go`, add near the top (after imports):
```go
import (
    // ... existing imports
    "bytes"
    "github.com/microcosm-cc/bluemonday"
    "github.com/yuin/goldmark"
)

// markdownPolicy is the bluemonday HTML sanitizer policy applied to
// rendered release notes. UGCPolicy permits common formatting (headings,
// lists, links, code, emphasis) but strips scripts, event handlers, and
// dangerous protocols. Source markdown comes from our own RELEASE_NOTES.md
// but we sanitize anyway as defense-in-depth.
var markdownPolicy = bluemonday.UGCPolicy()

// stripDownloadLinks removes the leading download-link section from a
// GitHub release body. Our release workflow puts download links above a
// `\n---\n` separator and the actual changelog below; this returns just
// the changelog. Bodies without a separator are returned unchanged.
func stripDownloadLinks(body string) string {
    if i := strings.Index(body, "\n---\n"); i >= 0 {
        return strings.TrimSpace(body[i+len("\n---\n"):])
    }
    return body
}

// renderReleaseNotesHtml converts markdown release notes to sanitized HTML
// suitable for direct innerHTML assignment in the web UI.
func renderReleaseNotesHtml(markdown string) string {
    if markdown == "" {
        return ""
    }
    var buf bytes.Buffer
    if err := goldmark.Convert([]byte(markdown), &buf); err != nil {
        // Fall back to escaped plain text on render failure
        return "<pre>" + html.EscapeString(markdown) + "</pre>"
    }
    return markdownPolicy.Sanitize(buf.String())
}
```

Add `"html"` to the import list.

- [x] **Step 5: Wire stripping + rendering into getRelease**

In `getRelease()` (around line 159 where `ReleaseInfo` is constructed):
```go
strippedBody := stripDownloadLinks(release.Body)
return &ReleaseInfo{
    Version:          version,
    TagName:          release.TagName,
    DownloadURL:      downloadURL,
    SignatureURL:     signatureURL,
    ReleaseNotes:     strippedBody,
    ReleaseNotesHtml: renderReleaseNotesHtml(strippedBody),
    PublishedAt:      release.PublishedAt,
}, nil
```

Add the `ReleaseNotesHtml` field to the struct:
```go
type ReleaseInfo struct {
    Version          string `json:"version"`
    TagName          string `json:"tagName"`
    DownloadURL      string `json:"downloadUrl"`
    SignatureURL     string `json:"signatureUrl,omitempty"`
    ReleaseNotes     string `json:"releaseNotes"`
    ReleaseNotesHtml string `json:"releaseNotesHtml"`
    PublishedAt      string `json:"publishedAt"`
}
```

- [x] **Step 6: Run tests**

```bash
go test -v ./internal/updater/...
```
Expected: all PASS, including the new ones.

- [x] **Step 7: Update routes that pass through ReleaseNotes**

Find the routes that serialize `ReleaseInfo` to JSON for the web UI:
```bash
grep -rn "releaseNotes" internal/web/routes/
```
In each handler, add the new field to the response map:
```go
resp["releaseNotesHtml"] = ui.ReleaseNotesHtml
```

- [x] **Step 8: Commit**

```bash
git add internal/updater/updater.go internal/updater/updater_test.go internal/web/routes/update.go internal/web/routes/jobs.go go.mod go.sum
git commit -m "feat(updater): strip download links and render markdown

Strips the download-link section from GitHub release bodies (split on
\\n---\\n separator) before exposing as ReleaseNotes. Adds a new
ReleaseNotesHtml field rendered server-side via goldmark + sanitized
with bluemonday's UGCPolicy.

Web UI consumers can now use innerHTML with the rendered HTML for proper
heading/list/link formatting; TUI continues to receive the raw stripped
markdown for glamour rendering."
```

---

### Task 19: Web UI renders the HTML

**Files:**
- Modify: `web/public/app.js` (around line 819)
- Modify: `web/public/moombox.css` (style for rendered notes)

**Goal:** Web UI uses `innerHTML` with the server-rendered HTML so headings/lists/code render properly.

- [x] **Step 1: Read the current display logic**

```bash
sed -n '810,830p' web/public/app.js
```

- [x] **Step 2: Swap textContent for innerHTML**

In `web/public/app.js`, find:
```javascript
notes.textContent = this._updateAvailable.releaseNotes || "No release notes available.";
```

Replace with:
```javascript
const html = this._updateAvailable.releaseNotesHtml || "";
if (html) {
    notes.innerHTML = html;
} else {
    notes.textContent = this._updateAvailable.releaseNotes || "No release notes available.";
}
```

The fallback to `textContent` is a safety net in case the server didn't render (e.g., older API response without the field).

- [x] **Step 3: Add styles for rendered markdown**

In `web/public/moombox.css`, find the existing `#update-release-notes` rule (around line 2575). Replace and extend:
```css
#update-release-notes {
    max-height: 400px;
    overflow-y: auto;
    word-break: break-word;
    font-size: 0.95em;
    line-height: 1.5;
}
#update-release-notes h1,
#update-release-notes h2,
#update-release-notes h3 {
    margin: 1em 0 0.4em;
    font-weight: 600;
}
#update-release-notes h1 { font-size: 1.3em; }
#update-release-notes h2 { font-size: 1.15em; }
#update-release-notes h3 { font-size: 1em; color: var(--sl-color-primary-600); }
#update-release-notes ul,
#update-release-notes ol {
    margin: 0.4em 0 0.4em 1.5em;
    padding: 0;
}
#update-release-notes li {
    margin: 0.2em 0;
}
#update-release-notes code {
    background: var(--sl-color-neutral-100);
    padding: 0.1em 0.3em;
    border-radius: 3px;
    font-family: var(--sl-font-mono);
    font-size: 0.9em;
}
#update-release-notes a {
    color: var(--sl-color-primary-600);
    text-decoration: underline;
}
#update-release-notes p {
    margin: 0.5em 0;
}
```

Note: removed `white-space: pre-wrap` because rendered HTML handles whitespace naturally.

- [x] **Step 4: Visual verification**

Run Moombox locally with a release that has formatted notes. Trigger the update dialog (or temporarily call `showUpdateDialog` from the dev console with a fake `_updateAvailable`). Verify headings render as headings, bullets as bullets, no markdown syntax characters visible.

- [x] **Step 5: Commit**

```bash
git add web/public/app.js web/public/moombox.css
git commit -m "feat(web): render release notes markdown as HTML

Update dialog now uses the server-rendered releaseNotesHtml field with
innerHTML, falling back to textContent of releaseNotes for backward
compat. CSS adds heading/list/code/link styles using existing Shoelace
tokens."
```

---

### Task 20: TUI release-notes overlay with glamour rendering

**Files:**
- Create: `internal/tui/release_notes_overlay.go`
- Create: `internal/tui/release_notes_overlay_test.go`
- Modify: `internal/tui/app.go` (add overlay state field)
- Modify: `internal/tui/app_actions.go` (add `R N` chord, handle overlay key routing)
- Modify: `internal/tui/app_layout.go` (render the overlay when active)
- Modify: `internal/tui/app_keys.go` (route keys to overlay when open)
- Modify: `go.mod` (add glamour)

**Goal:** Add a release-notes overlay (no equivalent exists today — TUI users currently update blind). New chord `R N` opens a scrollable bordered modal showing the release notes rendered via `charmbracelet/glamour`. From within the overlay, `U` applies the update directly; `Esc` / `Q` closes without applying. Modeled after the existing help overlay (`internal/tui/help.go`) which uses `bubbles/viewport` for scroll.

**Why this task got bigger:** the original plan assumed a TUI display surface for release notes existed. Investigation showed only a "⬆ Update!" version-bumped indicator and a feedback toast — the actual notes were stored but never displayed. This task adds the missing surface.

- [x] **Step 1: Add the glamour dependency**

```bash
go get github.com/charmbracelet/glamour
go mod tidy
```
Verify:
```bash
grep glamour go.mod
```

- [x] **Step 2: Write the failing test for the overlay component**

Create `internal/tui/release_notes_overlay_test.go`:
```go
package tui

import (
	"strings"
	"testing"
)

func TestReleaseNotesOverlayInitiallyClosed(t *testing.T) {
	o := newReleaseNotesOverlay()
	if o.isOpen() {
		t.Error("new overlay should not be open")
	}
}

func TestReleaseNotesOverlayOpenStores(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "## Features\n- thing", 80, 24)
	if !o.isOpen() {
		t.Error("overlay should be open after open()")
	}
	if o.tag != "v2.7.0" {
		t.Errorf("tag: got %q, want v2.7.0", o.tag)
	}
}

func TestReleaseNotesOverlayCloseClears(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "x", 80, 24)
	o.close()
	if o.isOpen() {
		t.Error("overlay should be closed after close()")
	}
}

func TestReleaseNotesOverlayRenderRendersHeading(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "## Features\n- thing\n\n## Bug Fixes\n- another", 80, 24)
	view := o.View()
	if view == "" {
		t.Fatal("View returned empty string")
	}
	// Glamour replaces "## Features" with stylized text — we don't assert
	// on exact ANSI, just that the original raw markdown isn't passed
	// through unchanged.
	if strings.Contains(view, "## Features") {
		t.Errorf("View still contains raw markdown ##: %q", view)
	}
	// The tag should appear in the title bar.
	if !strings.Contains(view, "v2.7.0") {
		t.Errorf("View missing tag in title: %q", view)
	}
}

func TestReleaseNotesOverlayRenderEmptyNotes(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.7.0", "", 80, 24)
	view := o.View()
	if !strings.Contains(view, "No release notes") {
		t.Errorf("expected 'No release notes' fallback, got: %q", view)
	}
}
```

- [x] **Step 3: Run test to verify it fails**

```bash
go test -v ./internal/tui/ -run TestReleaseNotesOverlay
```
Expected: build error (`undefined: newReleaseNotesOverlay`).

- [x] **Step 4: Create the overlay component**

Create `internal/tui/release_notes_overlay.go`:
```go
package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
)

// releaseNotesOverlay is a modal that shows release notes for a pending
// update. Scrollable via arrow keys / pgup-pgdn (handled by the
// embedded viewport; letter-key bindings disabled to avoid conflict
// with app chords). The overlay is opened from the R N chord and from
// within the overlay the user can press U to apply the update or
// Esc/Q to close.
type releaseNotesOverlay struct {
	open_    bool
	tag      string // version tag, e.g. "v2.7.0"
	rawNotes string // stripped raw markdown
	width    int
	height   int
	vp       viewport.Model
}

// newReleaseNotesOverlay returns a closed overlay.
func newReleaseNotesOverlay() *releaseNotesOverlay {
	vp := viewport.New(80, 20)
	vp.KeyMap = helpViewportKeyMap() // reuse help.go's safe keymap
	return &releaseNotesOverlay{vp: vp}
}

// isOpen reports whether the overlay is currently visible.
func (o *releaseNotesOverlay) isOpen() bool { return o.open_ }

// open prepares and shows the overlay. width/height are the terminal
// dimensions; the overlay sizes itself to ~80% of those.
func (o *releaseNotesOverlay) open(tag, rawNotes string, width, height int) {
	o.open_ = true
	o.tag = tag
	o.rawNotes = rawNotes
	o.width = width
	o.height = height

	// Size the viewport to leave room for borders + title + footer.
	vpWidth := max(40, width*8/10-4)
	vpHeight := max(8, height*8/10-6)
	o.vp.Width = vpWidth
	o.vp.Height = vpHeight

	o.vp.SetContent(o.renderBody(vpWidth))
	o.vp.GotoTop()
}

// close hides the overlay and clears its state.
func (o *releaseNotesOverlay) close() {
	o.open_ = false
	o.tag = ""
	o.rawNotes = ""
}

// Update routes a tea.Msg to the embedded viewport for scroll handling.
// Returns the tea.Cmd from the viewport (typically nil).
func (o *releaseNotesOverlay) Update(msg tea.Msg) tea.Cmd {
	if !o.open_ {
		return nil
	}
	var cmd tea.Cmd
	o.vp, cmd = o.vp.Update(msg)
	return cmd
}

// View returns the rendered overlay frame. Empty string when closed.
func (o *releaseNotesOverlay) View() string {
	if !o.open_ {
		return ""
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorGreen).
		Padding(0, 1)
	footerStyle := lipgloss.NewStyle().Faint(true).Padding(0, 1)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGreen)

	title := titleStyle.Render("Release Notes — " + o.tag)
	footer := footerStyle.Render("U: Apply update  ↑/↓: Scroll  Esc/Q: Close")

	body := o.vp.View()
	inner := lipgloss.JoinVertical(lipgloss.Left, title, body, footer)
	return borderStyle.Render(inner)
}

// renderBody runs glamour over the raw markdown to produce ANSI text
// sized to the given width. Returns a fallback message for empty notes.
func (o *releaseNotesOverlay) renderBody(width int) string {
	if strings.TrimSpace(o.rawNotes) == "" {
		return "No release notes available for this update."
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return o.rawNotes // fall back to raw markdown
	}
	rendered, err := r.Render(o.rawNotes)
	if err != nil {
		return o.rawNotes
	}
	return rendered
}
```

- [x] **Step 5: Run test to verify it passes**

```bash
go test -v ./internal/tui/ -run TestReleaseNotesOverlay
```
Expected: all 5 tests PASS.

- [x] **Step 6: Wire overlay state into the app**

Open `internal/tui/app.go`. Find the app struct (around line 240-260 where `updateAvailable` is declared). Add the overlay field next to it:
```go
// Update status
updateAvailable    *UpdateStatusMsg
version            string
releaseNotesPopup  *releaseNotesOverlay  // shown when R N is pressed
```

In the app constructor (where the app struct is initialized), add:
```go
releaseNotesPopup: newReleaseNotesOverlay(),
```

- [x] **Step 7: Add `R N` chord to the action menu**

Open `internal/tui/app_actions.go`. Find the section that builds Request-prefixed menu items (around line 446-449 where `R V` and `R U` are added). Add `R N` between them, conditional on an update being available:
```go
if a.updateAvailable != nil {
    items = append(items, ActionMenuItem{
        Chord: "R N", Label: "View Release Notes " + a.updateAvailable.TagName,
        HintLabel: "Notes", Category: "Request",
    })
}
if a.updateAvailable != nil && a.OnApplyUpdate != nil {
    items = append(items, ActionMenuItem{
        Chord: "R U", Label: "Apply Update " + a.updateAvailable.TagName,
        HintLabel: "Update", Category: "Request",
    })
}
```

In the same file, find the chord dispatcher (the `case "R U":` block around line 175). Add a `case "R N":` block above it:
```go
case "R N":
    if a.updateAvailable != nil {
        a.releaseNotesPopup.open(
            a.updateAvailable.TagName,
            a.updateAvailable.ReleaseNotes,
            a.width, a.height,
        )
        return a, nil
    }
    a.setFeedback("No update available — release notes unavailable")
    return a, nil
```

(`a.width` and `a.height` are existing fields on the app model from the bubbletea WindowSizeMsg handler. If they're named differently, grep the existing code for the actual field names and use those.)

- [x] **Step 8: Route keys to overlay when open + add U/Esc/Q handlers**

Open `internal/tui/app_keys.go`. Find the main key handler. Add a check at the very top — if the overlay is open, route specific keys there and consume others:
```go
// When the release-notes overlay is open, intercept keys before the
// main chord/dispatch loop so the overlay's scroll bindings work and
// the user can confirm/cancel the update directly.
if a.releaseNotesPopup.isOpen() {
    switch msg := msg.(type) {
    case tea.KeyMsg:
        switch msg.String() {
        case "esc", "q", "Q":
            a.releaseNotesPopup.close()
            return a, nil
        case "u", "U":
            // Apply the update from inside the overlay
            if a.updateAvailable != nil && a.OnApplyUpdate != nil {
                a.setFeedback(fmt.Sprintf("Updating to %s...", a.updateAvailable.TagName))
                ver := a.updateAvailable.Version
                applyFn := a.OnApplyUpdate
                a.releaseNotesPopup.close()
                return a, safeCmd(func() tea.Msg {
                    if err := applyFn(); err != nil {
                        return updateAppliedMsg{Err: err.Error()}
                    }
                    return updateAppliedMsg{Version: ver}
                })
            }
            return a, nil
        }
    }
    // Forward scroll-related messages to the viewport
    cmd := a.releaseNotesPopup.Update(msg)
    return a, cmd
}
```

The exact placement depends on the existing key handler structure; insert this as the FIRST check inside the handler so overlay keys take precedence over chord matching.

- [x] **Step 9: Render overlay in app View**

Open `internal/tui/app_layout.go`. Find the main `View()` function. Near the end, before returning the final composed string, add overlay rendering:
```go
// Composite the overlay on top of the base view if it's open.
if a.releaseNotesPopup.isOpen() {
    base := <existing base view assembly>
    overlay := a.releaseNotesPopup.View()
    return lipgloss.Place(
        a.width, a.height,
        lipgloss.Center, lipgloss.Center,
        overlay,
        lipgloss.WithWhitespaceChars(" "),
    )
}
return <existing base view>
```

(If the existing layout already uses a `lipgloss.Place` pattern for help/menu overlays, follow that exact pattern instead. Read `app_layout.go` for the conventions before editing.)

- [x] **Step 10: Update CLAUDE.md chord doc**

In `CLAUDE.md`, find the chord prefixes documentation and add `R N` to the Request examples list. Specifically, the line around "Prefixes: A (Action), R (Request)..." doesn't list every chord, but if there's a more detailed table elsewhere, add the entry there.

If there's no detailed list (just the prefix description), this step is a no-op.

- [x] **Step 11: Update README.md keyboard controls table**

In `README.md`, find the "**Request (R)**" table:
```markdown
**Request (R)**

| Chord | Action |
|-------|--------|
| R C | Recheck cookie authentication |
| R F | Force browser cookie refresh |
| R V | Check for updates |
| R U | Apply pending update |
| R P P | Restart program (confirm) |
```

Add `R N` between `R V` and `R U`:
```markdown
| R V | Check for updates |
| R N | View release notes for pending update |
| R U | Apply pending update |
```

- [x] **Step 12: Build and visually verify**

```bash
go build ./cmd/moombox
./moombox.exe   # Windows; or ./moombox-linux-amd64 on Linux
```

In the running app:
- Trigger an update check (`R V`) — wait for one to be detected, or temporarily inject a fake `updateAvailable` for testing.
- Press `R N` — the overlay should appear, showing the release notes rendered with colored headings.
- Use ↑/↓/PgUp/PgDn to scroll if notes are long.
- Press `Esc` — overlay closes, base UI restored.
- Re-open with `R N`, press `U` — should trigger the update apply (same as `R U` from outside the overlay).

If you can't run an interactive TUI, at minimum verify `go test ./internal/tui/...` passes and `go vet ./internal/tui/...` is clean.

- [x] **Step 13: Commit**

```bash
git add internal/tui/release_notes_overlay.go internal/tui/release_notes_overlay_test.go internal/tui/app.go internal/tui/app_actions.go internal/tui/app_keys.go internal/tui/app_layout.go README.md go.mod go.sum
git commit -m "feat(tui): release-notes overlay with glamour rendering

Adds a release_notes_overlay component (modeled after the help overlay's
viewport pattern) that renders update notes via charmbracelet/glamour.
New chord R N opens the overlay; from within, U applies the update
directly and Esc/Q closes.

Previously the TUI displayed nothing for release notes — only a small
'⬆ Update!' indicator and a feedback toast — so users updated blind.
This adds the missing surface and gives Linux/Windows TUI users feature
parity with the web UI's update dialog."
```

---

## Phase 10: Documentation

### Task 21: Create BUILDING.md

**Files:**
- Create: `BUILDING.md`

**Goal:** A focused build doc covering all the prerequisites and per-platform commands needed to build Moombox from source.

- [x] **Step 1: Create the file**

Create `BUILDING.md`:
```markdown
# Building Moombox from Source

This document covers prerequisites and build commands for Moombox on Windows and Linux.

## Prerequisites

- **Go 1.25+** — https://go.dev/dl/
- **Node.js 22 LTS** — https://nodejs.org/ (only required for building the BotGuard sidecar tarball; not a runtime dep)
- **FFmpeg** — only required at runtime, not at build time

## One-Time Setup

After a fresh checkout, run these once to populate the `internal/bgutils/embed/` directory with the BotGuard sidecar tarball and the Node.js binaries embedded in the final executable:

```bash
# Build the BotGuard sidecar payload (~3.5 MB tarball)
cd bgutil-sidecar
npm ci --omit=dev
node build.mjs
cd ..

# Fetch the pinned Node.js binaries for all three platforms
go run ./tools/fetch-node
```

The `tools/fetch-node` step downloads ~150 MB total (Node binaries for Windows x64, Linux x64, and Linux arm64) and gzips them into the embed dir. Idempotent: re-runs are no-ops if `version.txt` matches.

CI runs both steps automatically (see `.github/workflows/release.yml`); for local builds you only need to re-run them when the pinned versions change.

## Build Commands

### Windows x64 (native)

```bash
go build -o Moombox.exe ./cmd/moombox
```

With version info (release builds):
```bash
go build -ldflags "-s -w -X main.version=2.7.0 -X main.commit=$(git rev-parse --short HEAD)" -o Moombox.exe ./cmd/moombox
```

### Linux x64 (cross-compile or native)

```bash
GOOS=linux GOARCH=amd64 go build -o moombox-linux-amd64 ./cmd/moombox
```

### Linux arm64 (cross-compile)

```bash
GOOS=linux GOARCH=arm64 go build -o moombox-linux-arm64 ./cmd/moombox
```

Cross-compiling Linux binaries from a Windows dev box uses the same env vars — Go handles the toolchain transparently because Moombox uses `CGO_ENABLED=0`.

## Windows Resource Embedding (optional)

Moombox.exe ships with an embedded icon, manifest, and version info on Windows. CI generates these at build time via `go-winres`. For local Windows builds with the icon:

```bash
go install github.com/tc-hib/go-winres@latest
cd cmd/moombox
go-winres make --arch amd64
cd ../..
go build -o Moombox.exe ./cmd/moombox
```

The generated `.syso` files are gitignored. Skipping this step produces a working `Moombox.exe` without the embedded icon/version info.

## Signing Releases

Release artifacts are signed with Ed25519 via `cmd/sign`. CI runs this with the private key stored as a GitHub Actions secret (`SIGNING_KEY`). The corresponding public key is hex-embedded in `internal/updater/signing.go` and verifies updates at runtime.

To sign a binary locally (for testing):
```bash
SIGNING_KEY=<hex-encoded-private-key> go run ./cmd/sign Moombox.exe
# Produces Moombox.exe.sig (64-byte Ed25519 signature)
```

To generate a new key pair (for setting up a fresh signing chain):
```bash
go run ./cmd/sign -genkey -out keys.txt
# Reads keys.txt, prints the public key, reminds you to delete the file
```

## Tests

```bash
go test ./...                                       # all tests
go test -v ./internal/engine/...                    # one package, verbose
go test -v -run TestParseDash ./internal/engine/... # one test
go vet ./...                                        # static analysis
```

## CI Workflows

- **`.github/workflows/release.yml`** — runs on tag push (`v*`). Builds and signs all three platforms, creates a GitHub release with the matching assets and a body listing all download links.
- **`.github/workflows/linux-test.yml`** — runs on every PR/push to `main`. Builds for both Linux arches and runs tests for amd64. Catches Linux compilation regressions before they reach a release tag.

## Profiling

For memory or CPU investigations, set `MOOMBOX_PPROF=1` before launching. Moombox then exposes the standard Go `net/http/pprof` endpoints on `localhost:6060` (loopback-only). Off by default with zero overhead when unset.

```bash
MOOMBOX_PPROF=1 ./moombox-linux-amd64
# In another terminal:
go tool pprof http://localhost:6060/debug/pprof/heap     # live heap
go tool pprof http://localhost:6060/debug/pprof/profile  # 30s CPU profile
```
```

- [x] **Step 2: Commit**

```bash
git add BUILDING.md
git commit -m "docs: add BUILDING.md with cross-platform build instructions

Covers prerequisites, one-time setup (sidecar build + fetch-node),
build commands for Windows x64 / Linux x64 / Linux arm64, optional
Windows resource embedding via go-winres, signing for releases, tests,
CI workflow overview, and profiling.

Replaces detailed build steps that were previously embedded in
README.md."
```

---

### Task 22: Update README.md

**Files:**
- Modify: `README.md`

**Goal:** Add Linux to the requirements/quick-start sections; trim the build-from-source section to a pointer to BUILDING.md.

- [x] **Step 1: Update Requirements section**

Find the "Requirements" section in `README.md`. Replace:
```markdown
**Running the pre-built executable:**
- Windows (x64)
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH (for muxing video + audio)
```

With:
```markdown
**Running the pre-built executable:**
- Windows x64, **or** Linux x64, **or** Linux arm64
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH (for muxing video + audio)
```

- [x] **Step 2: Update Quick Start with Linux instructions**

Find the "Quick Start" section. Replace the Windows-only instructions with a tabbed/collapsible pair:

```markdown
## Quick Start

### Windows

1. Download `Moombox.exe` from the [latest release](https://github.com/vampiricwulf/Moombox/releases/latest)
2. Place it in a directory of your choice
3. Run `Moombox.exe`

### Linux (x64)

```bash
wget https://github.com/vampiricwulf/Moombox/releases/latest/download/moombox-linux-amd64
chmod +x moombox-linux-amd64
./moombox-linux-amd64
```

### Linux (arm64)

```bash
wget https://github.com/vampiricwulf/Moombox/releases/latest/download/moombox-linux-arm64
chmod +x moombox-linux-arm64
./moombox-linux-arm64
```

A built-in setup wizard walks you through first-time configuration on launch. The TUI opens by default — press **W** to open the web dashboard in your browser.
```

- [x] **Step 3: Replace the "Building from Source" section with a pointer**

Find the "Building from Source" section. Replace its entire body with:
```markdown
## Building from Source

See [BUILDING.md](BUILDING.md) for prerequisites and build commands for Windows, Linux x64, and Linux arm64.
```

- [x] **Step 4: Verify rendering**

Open `README.md` in a markdown previewer (or just visual inspect). Make sure section headings flow correctly and no orphaned references remain.

- [x] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs(readme): add Linux quickstart, point to BUILDING.md

Requirements lists Windows x64, Linux x64, and Linux arm64. Quick Start
section adds wget/chmod/run instructions for both Linux arches. Build
from source steps moved to the new BUILDING.md to keep the README
focused on user-facing info."
```

---

## Final verification

### Task 23: End-to-end smoke test

- [x] **Step 1: Cross-platform build sanity**

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/moombox-test.exe ./cmd/moombox
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/moombox-test-linux-amd64 ./cmd/moombox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/moombox-test-linux-arm64 ./cmd/moombox
```
All three must succeed.

- [x] **Step 2: Full test suite**

```bash
go test ./...
GOOS=linux go test ./...
```
Expected: all PASS on both.

- [x] **Step 3: Vet**

```bash
go vet ./...
GOOS=linux go vet ./...
```
Expected: no warnings.

- [x] **Step 4: Run native binary, exercise key paths**

On Windows:
```bash
./Moombox.exe --version
./Moombox.exe
# Open localhost:774, verify settings page loads, Auto Cookies dropdown shows
# Verify Update dialog (if any update is available — or fake one in dev console)
```

On Linux (if available):
```bash
./moombox-linux-amd64 --version
./moombox-linux-amd64
# Same checks
```

- [x] **Step 5: Cleanup test artifacts**

```bash
rm -f /tmp/moombox-test*
```

- [x] **Step 6: No commit needed for verification**

---

## Plan completion checklist

After running through all tasks:
- [x] All builds pass on Windows + Linux amd64 + Linux arm64
- [x] All tests pass on Windows + Linux amd64
- [x] `go vet ./...` clean on both
- [x] CI workflows committed and ready to run on the next tag push
- [x] Documentation reflects the new platform support

When ready to release, follow the existing release process documented in CLAUDE.md (bump version in `cmd/moombox/main.go`, write `RELEASE_NOTES.md`, commit, tag, push). CI will produce all three signed binaries and create the GitHub release with the multi-platform download links.
