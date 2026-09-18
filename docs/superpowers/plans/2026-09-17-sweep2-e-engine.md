# Sweep 2 — Arc E (engine + worker) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the engine's three `O_TRUNC`-over-staged-media doors, stop a single Twitch GQL flap from finalizing a live recording, replace the 30 s total fetch deadline with a read-progress deadline, bound the HLS VOD reorder buffer, and land the remaining engine/worker rows of `reports/sweep-2026-09-15b.md`.

**Architecture:** Three mechanisms carry most of the arc. (1) A shared no-truncate guard in `engine.SegmentDownloader.Start` plus three call-site decisions (restart routing, truncate retry, direct-download Range preservation) replaces every implicit `O_TRUNC` with either a resume or a named error. (2) A two-sample Twitch liveness helper in the worker's `CheckStreamFn`/`RecheckStreamFn` closures covers all six engine consult sites at once, paired with a propagate-only change inside `twitch.API.GetStreamInfo`. (3) A read-progress ("idle") deadline — an `io.ReadCloser` wrapper that pushes a `time.AfterFunc` out on every byte-delivering `Read` — replaces `context.WithTimeout(parent, SegmentTimeout)` on the two body-reading fetches, which also lets the engine HTTP client's 5-minute ceiling go. Everything else is local: the HLS VOD consumer reuses the existing `reorderBuffer`, the VOD chat wait gets its own bound and releases the download slot first, the mux contexts descend from one cancellable root the worker's `Stop` can cut, and the lifecycle slot moves from `Dequeue` to the `ShouldDownload` decision.

**Tech Stack:** Go 1.27 (no CGo), `internal/engine`, `internal/worker`, `internal/twitch`, `internal/database`, `docs/spec/architecture.md`. Tests are standard `testing` + `net/http/httptest` + `modernc.org/sqlite` through `database.Open`.

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` (§0 decisions O-A, O-B, O-C, O-E, O-F, O-G; §2 constraints; §3 "Arc E"; §4 order; §5 rulings). Report rows: `reports/sweep-2026-09-15b.md` #1, #2, #3, #8, #10, #21–#25, #47 (engine half), #48–#53, #102 (ENGINE-16). Area report `reports/sweep-2026-09-15b/engine.md`; verifier `reports/sweep-2026-09-15b/_verify-engine-twitch.md` — its merges **M1–M6 override the area report**.

**Branch / worktree:** `git worktree add -b sweep2-e-engine .worktrees/sweep2-e-engine main`, then copy `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and `internal/cipher/testdata/*.js` into the worktree (both are gitignored and the engine/worker packages pull `internal/bgutils` transitively). No `npm ci` is needed — Arc E touches no JS.

---

## Global Constraints

Copied verbatim from the spec's §2. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...` is part of every merge candidate).
- LF line endings in every file the chain touches. Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it (the reviewer verifies at least one by execution).
- Every JS-touching task gates `go test ./internal/web/routes/` AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`/`CLAUDE.md`, gates `go test ./internal/docs/` (the citation test requires the DECLARING file; after O-Z it also scans `SPEC.md` and `CLAUDE.md`).
- Protected behaviour stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper, never rarer); the DB layer untouched for perf; `monitors.probe_cooldown` default 0; connectivity-monitor probe design; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays unbounded (the timeout); RotateCookies rejected; DPAPI two-pass never; never kill processes by image name; never `rm -rf` under %TEMP%; no Shoelace bundling; extraction mirrors yt-dlp (android_vr retained; VISIONOS split-adaptive; homepage minting); update-path compatibility (old launcher + new child analysed before any launcher/updater/exit-code/swap-artifact change; success paths byte-identical); the setup wizard stays loopback-gated.
- Implementers commit with the pathspec ON the commit (`git add <files> && git commit -m … -- <same files>`); no stash/checkout/rebase/reset/amend; reviewers never edit (they reproduce in `git archive` scratch exports, copying the four gitignored embed blobs when a package needs them — youtube, chat, twitch, worker, engine, monitor, web all pull `internal/bgutils` transitively); scratch test files never named `*_linux_test.go`/`*_windows_test.go`; `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; ONE controller-run `go test -count=1 ./...` at a time.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

### Arc E specifics

- **Package gates** (run per task, whichever apply to the files touched): `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/`, `./internal/worker/`, `./internal/twitch/`, `./internal/database/`, `./internal/docs/`.
- **Merge-candidate gates** (controller): merge `main` into the branch first; `gofmt -l ./cmd ./internal ./tools ./web`; `go vet ./...` on Windows **and** `GOOS=linux GOARCH=amd64 go vet ./...`; `staticcheck ./...`; three builds (native, linux/amd64, linux/arm64); `node --test web/tests/*.test.mjs`; ONE full `go test -count=1 ./...`.
- **Arc E has NO live gate.** No `MOOMBOX_LIVE_*` variable is set at any point.
- Arc T merges after Arc E and shares `internal/twitch/api.go` — Arc E touches **only** the body of `GetStreamInfo` there (Task 5). Nothing else in `internal/twitch`.

### File-set deviations (declared up front)

The spec's Arc E file list could not anticipate three helpers that the rows require. Each lands in a file **no other arc claims**, so no wave-1 or wave-2 arc conflicts:

| File | Why | Task |
|---|---|---|
| `internal/worker/orchestrator.go` beyond the chat-wait hunk | O-E needs two struct fields on `DownloadOrchestrator` and two constructor lines; a struct cannot be split across files. No other arc lists this file. | 8 |
| `internal/database/database_extras.go` | ENGINE-17's "one query" needs a `GetAllTrims`. Arc M claims `database.go`/`database_jobs.go` only; `database_extras.go` is claimed by nobody. | 11 |
| `internal/engine/delays.go` | O-Q's exported fast-delays seam. Inside `internal/engine/**`, which Arc E owns wholesale. | 11 |

Residual handed to Arc X (do NOT fix here — `internal/utils/**` is Arc X's): TOOL-2's second half, the false doc comment on `utils.ReplaceFile` (`internal/utils/replacefile.go:8-15`) claiming it is "the last step of every atomic write in Moombox". Arc E swaps the archive-facing call sites; Arc X owns the comment (it also owns TOOL-19's `utils.WriteFileAtomic`, the same paragraph).

---

## File Structure

| File | Responsibility after this arc | Tasks |
|---|---|---|
| `internal/engine/downloader.go` | `ErrStagedMediaPresent`, `DownloaderOptions.DiscardStaged`, the shared no-truncate guard and the truncate-for-resume retry ladder | 1 |
| `internal/engine/downloader_direct.go` | `discardStagedMedia` (the only explicit discard), Range-preserving streaming fallback, probe retry | 1, 4 |
| `internal/engine/downloader_fetch.go` | `idleBody`/`withReadProgressDeadline`/`errFetchIdle`; `probeFileSizeWithRetry`; bounded 206 drain; no client-level `Timeout` | 3, 4 |
| `internal/engine/downloader_hls.go` | VOD consumer backed by `reorderBuffer` under `hlsVodBufferBytes` | 6 |
| `internal/engine/downloader_resume.go` | media fsync before each sidecar save; zero-byte save skip; `utils.ReplaceFile` | 8, 10 |
| `internal/engine/connectivity_wait.go` | single-flight `callIsOnline` | 10 |
| `internal/engine/muxer.go` | `ffmpegPathArg` long-path prefix on Windows | 10 |
| `internal/engine/delays.go` | `SetFastDelaysForTests` seam | 11 |
| `internal/worker/worker.go` | restart-from-Muxing routing; two-sample Twitch closures; lifecycle-slot acquire; chat-incomplete staging keep; `Stop` cancels muxes | 2, 5, 7, 8, 9 |
| `internal/worker/queue.go` | lifecycle slot decoupled from `Dequeue`; drop logged once per job | 9 |
| `internal/worker/orchestrator.go` | mux root context + `CancelMuxes`; VOD chat wait at the YouTube site | 7, 8 |
| `internal/worker/orchestrator_chat.go` | `vodChatWaitTimeout` | 7 |
| `internal/worker/orchestrator_twitch.go` | unknown-verdict exit re-verify → Error; VOD chat wait at the Twitch site; mux root context | 5, 7, 8 |
| `internal/worker/orchestrator_mux.go` | muxed-duration check; `utils.ReplaceFile` renames | 8 |
| `internal/worker/mux_finalize.go` | `utils.ReplaceFile` renames | 8 |
| `internal/worker/quality_split_common.go` | background mux under the mux root context | 8 |
| `internal/worker/stream_processor_twitch.go` | two-sample fetch at `processTwitchLive`; hint-cache subscribe in `waitForTwitchLive` | 5, 11 |
| `internal/worker/strategy_youtube_manifestless_dash.go` | sets `DiscardStaged` for the deliberate non-live sq=0 restart | 1 |
| `internal/worker/orphans.go` | chat-incomplete staging keep; one-query trim scan | 7, 11 |
| `internal/twitch/api.go` | `GetStreamInfo` propagates `errStreamSlotUnavailable` | 5 |
| `internal/database/database_extras.go` | `GetAllTrims` | 11 |
| `docs/spec/architecture.md` | ENGINE-16's three false sentences | 12 |

---

## Task 1: The shared no-truncate guard

Report row #1 (ENGINE-1 + ENGINE-5 + ENGINE-6), verifier merge **M2**: "three doors into the same room". This task builds the guard and closes door (b) — the failed resume truncate. Door (a) is Task 2, door (c) is Task 4.

**Files:**
- Modify: `internal/engine/downloader.go` (sentinel near `ErrGapDetected:41`; `DownloaderOptions` field; `Start`'s guard at `:764-772` and truncate block at `:785-810`)
- Modify: `internal/engine/downloader_direct.go:159-181` (rename `resetForStreamingFallback` → `discardStagedMedia`)
- Modify: `internal/worker/strategy_youtube_manifestless_dash.go:282,341` (set `DiscardStaged`)
- Test: `internal/engine/downloader_notruncate_test.go` (new)
- Test: `internal/worker/strategy_youtube_manifestless_dash_test.go` (extend)

**Interfaces:**
- Produces: `engine.ErrStagedMediaPresent` (sentinel, `error`); `engine.DownloaderOptions.DiscardStaged bool`; `(*engine.SegmentDownloader).discardStagedMedia(reason string) error` (unexported, used by Task 4); `truncateForResume(path string, size int64) error` and the test seam `var truncateRetrySleep = time.Sleep` (both unexported, package `engine`).
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_notruncate_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stagedFile writes n bytes of staged "recording" to a fresh temp file and
// returns its path. Every row below starts from a non-empty staged file with
// NO resume sidecar beside it — the exact shape ENGINE-1/ENGINE-5 destroy.
func stagedFile(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video_stream")
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	return path
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestStartRefusesToTruncateStagedMedia pins the shared no-truncate guard:
// a segmented download that finds staged bytes it cannot resume must return
// ErrStagedMediaPresent with the file untouched, never open it O_TRUNC.
//
// Mutant: restoring the bare `flags |= os.O_TRUNC` else-branch in Start (i.e.
// deleting the guard) — Start returns nil and the staged file is 0 bytes.
func TestStartRefusesToTruncateStagedMedia(t *testing.T) {
	path := stagedFile(t, 1<<20)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile: path,
	})

	err := d.Start(context.Background())
	if !errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want ErrStagedMediaPresent", err)
	}
	if got := sizeOf(t, path); got != 1<<20 {
		t.Fatalf("staged file is %d bytes after Start, want 1048576 (the guard must not truncate)", got)
	}
}

// TestStartDiscardStagedMediaOptIn pins the escape hatch: a caller that has
// explicitly decided the staged bytes are disposable still gets the fresh
// O_TRUNC file it asked for.
//
// Mutant: dropping the `!d.opts.DiscardStaged` term from the guard — Start
// returns ErrStagedMediaPresent and the manifest-free post-live restart,
// which MUST begin at sq=0, can never run.
func TestStartDiscardStagedMediaOptIn(t *testing.T) {
	path := stagedFile(t, 4096)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:       "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile:    path,
		DiscardStaged: true,
		MaxRetries:    1,
	})
	d.delays = fastDelays()

	// The open mode is decided before the first fetch, so the download's own
	// failure against a dead address is irrelevant — only the file is.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Start(ctx); errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want the guard to stand down for an explicit discard", err)
	}
	if got := sizeOf(t, path); got != 0 {
		t.Fatalf("staged file is %d bytes after an explicit discard, want 0", got)
	}
}

// TestStartDirectURLKeepsLegacyTruncate pins the guard's scope: whole-file
// direct downloads are NOT segmented staged media and keep their pre-arc
// restart-from-byte-0 behaviour (their partial loss is bounded by the
// 50 MB sidecar cadence, and Task 4 removes the truncation they actually hit).
//
// Mutant: widening the guard to IsDirectURL — a half-downloaded VOD whose
// sidecar was lost errors instead of restarting.
func TestStartDirectURLKeepsLegacyTruncate(t *testing.T) {
	path := stagedFile(t, 4096)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile:  path,
		IsDirectURL: true,
		MaxRetries:  1,
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Start(ctx); errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want the guard to ignore IsDirectURL downloads", err)
	}
}

// TestTruncateForResumeRetriesThenFails pins the retry ladder that replaces
// ENGINE-5's silent `starting fresh` fallback: the truncate is re-attempted
// through the Windows AV/indexer sharing-violation window, and the last error
// is RETURNED rather than swallowed into an O_TRUNC.
//
// Mutant: making truncateForResume a single os.Truncate call — attempts is 1
// and a window that clears on the third try is never seen.
func TestTruncateForResumeRetriesThenFails(t *testing.T) {
	path := stagedFile(t, 1024)

	prevSleep := truncateRetrySleep
	prevTrunc := truncateFile
	t.Cleanup(func() { truncateRetrySleep = prevSleep; truncateFile = prevTrunc })
	truncateRetrySleep = func(time.Duration) {}

	attempts := 0
	truncateFile = func(name string, size int64) error {
		attempts++
		if attempts < 3 {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return os.Truncate(name, size)
	}

	if err := truncateForResume(path, 512); err != nil {
		t.Fatalf("truncateForResume = %v, want nil once the window clears", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (the ladder must retry, not give up on the first refusal)", attempts)
	}
	if got := sizeOf(t, path); got != 512 {
		t.Fatalf("file is %d bytes, want 512", got)
	}
}
```

Add to `internal/worker/strategy_youtube_manifestless_dash_test.go`:

```go
// TestManifestlessDiscardStagedOnlyForNonLiveRestart pins the ONE caller that
// opts into engine.DownloaderOptions.DiscardStaged: a manifest-free post-live
// capture whose segments carry ftyp+moov only at sq=0 must be allowed to
// restart from 0 over staged bytes; a LIVE capture, and any part that
// force-starts at an orchestrator-provided seq, must not.
//
// Mutant: setting DiscardStaged unconditionally — a live manifest-free
// recording's staged file is O_TRUNC'd on every restart.
func TestManifestlessDiscardStagedOnlyForNonLiveRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status youtube.StreamStatus
		forced bool
		want   bool
	}{
		{"post-live restart discards", youtube.StreamPostLive, false, true},
		{"live restart never discards", youtube.StreamLive, false, false},
		{"forced start seq never discards", youtube.StreamPostLive, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manifestlessDiscardStaged(tc.status, tc.forced); got != tc.want {
				t.Errorf("manifestlessDiscardStaged(%v, %v) = %v, want %v", tc.status, tc.forced, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestStart(RefusesToTruncate|DiscardStaged|DirectURL)|TestTruncateForResume' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestManifestlessDiscardStaged ./internal/worker/
```

Expected: both FAIL to compile — `undefined: ErrStagedMediaPresent`, `unknown field DiscardStaged`, `undefined: truncateForResume`, `undefined: truncateRetrySleep`, `undefined: truncateFile`, `undefined: manifestlessDiscardStaged`.

- [ ] **Step 3: Add the sentinel and the option**

In `internal/engine/downloader.go`, immediately after the `ErrGapDetected` block (`:33-41`):

```go
// ErrStagedMediaPresent signals that Start found non-empty staged media at
// OutputFile that it could neither resume from (no usable sidecar, no DB
// position) nor was told to discard. Destroying it was the previous
// behaviour: an implicit `O_TRUNC` over a complete multi-hour recording
// (sweep-2 ENGINE-1/ENGINE-5). The rule is now the one the StopOnGap path
// always had — never truncate non-empty staged media unless the CALLER
// explicitly asked to discard it — with the decision handed back to the
// orchestrator, which can mux what is staged instead.
//
// StopOnGap callers get ErrGapDetected instead: they have a richer recovery
// (close this file as a finished part, continue in a fresh one).
var ErrStagedMediaPresent = errors.New("staged media present with no usable resume state")
```

In `DownloaderOptions`, immediately after `StopOnGap` (`:169-175`):

```go
	// DiscardStaged tells Start that any bytes already at OutputFile are
	// disposable, so the no-truncate guard (ErrStagedMediaPresent) stands
	// down and the file is opened O_TRUNC. This is the ONLY way a caller
	// destroys staged media: every implicit truncate is now a guard trip.
	//
	// The one production setter is the manifest-free DASH strategy's
	// post-live restart: those segments carry their ftyp+moov init inline at
	// sq=0 only, so a finished stream genuinely must begin again at 0 and
	// the partial file cannot be appended to. Deliberate discards that
	// REMOVE the media before constructing the downloader (the quality-split
	// short-segment rule) never need this — the guard only looks at bytes
	// that are still there.
	DiscardStaged bool
```

- [ ] **Step 4: Replace the guard and the truncate fallback**

In `Start`, replace the `StopOnGap no-truncate guard` block (`:764-772`) with:

```go
	// Shared no-truncate guard (sweep-2 ENGINE-1/5/6, verifier merge M2).
	// Staged data with no usable resume state — a corrupt/stale/identity-
	// rejected sidecar, a restart that re-probed the stream as post-live and
	// re-seeded seq 0, a sidecar the natural end already cleared — must never
	// be truncated. Truncating destroys a recording that finalize-time
	// recovery can still mux; the decision belongs to the caller.
	//
	// StopOnGap callers have the richer answer (close this file as a finished
	// part and continue in a fresh one), so they keep ErrGapDetected.
	// IsDirectURL is out of scope here: a whole-file VOD download is not
	// segmented staged media, its partial is bounded by the 50 MB sidecar
	// cadence, and the truncation it actually suffered (the streaming
	// fallback) is removed at its own call site instead.
	if !resuming && !d.opts.DiscardStaged && !d.opts.IsDirectURL {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			if d.opts.StopOnGap {
				d.logger.Warn("[Downloader] Staged data present but resume state unusable — splitting instead of truncating",
					"file", d.opts.OutputFile, "size", info.Size())
				return ErrGapDetected
			}
			d.logger.Error("[Downloader] Staged data present but resume state unusable — refusing to truncate",
				"file", d.opts.OutputFile, "size", info.Size())
			return fmt.Errorf("%w: %s holds %d bytes", ErrStagedMediaPresent, d.opts.OutputFile, info.Size())
		}
	}
```

Replace the truncate block inside `if resuming { … }` (`:785-810`) with:

```go
			if truncErr := truncateForResume(d.opts.OutputFile, state.BytesWritten); truncErr != nil {
				if d.opts.StopOnGap {
					// Same contract as the no-truncate guard above: a failed
					// truncate must not fall back to O_TRUNC and destroy the
					// staged recording (transient sharing violations from AV
					// scans hit exactly this window on Windows). Split
					// instead — the caller muxes the file as a finished part.
					d.logger.Warn("[Downloader] Truncate-for-resume failed — splitting instead of starting fresh",
						"file", d.opts.OutputFile, "err", truncErr)
					return ErrGapDetected
				}
				// ENGINE-5: the old branch here logged a Warn, cleared the
				// resume state and opened the file O_TRUNC — losing hours of
				// footage to a transient sharing violation. The retry ladder
				// above has already ridden out that window; anything left is
				// a real filesystem failure, and returning it keeps staging
				// and the sidecar intact for a later Resume.
				d.logger.Error("[Downloader] Truncate-for-resume failed after retries",
					"file", d.opts.OutputFile, "err", truncErr)
				return fmt.Errorf("truncate for resume: %w", truncErr)
			}
```

Delete the now-unreachable `resuming = false` / `flags = … O_TRUNC` / `state = nil` / `d.bytesWritten.Store(0)` / `hlsInit*` reset lines that followed it.

Add below `Start` (or beside the other file helpers at the end of `downloader.go`):

```go
// truncateForResume shrinks a staged recording to its fsync'd resume offset,
// retrying through the Windows AV/indexer window that briefly holds a freshly
// written file open. Same ladder as utils.ReplaceFile's rename retry (8
// attempts, 10 ms doubling to 400 ms, just over a second in total); it is
// spelled out here rather than reused because that helper renames and this
// one truncates.
func truncateForResume(path string, size int64) error {
	delay := 10 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := truncateFile(path, size)
		if err == nil || attempt >= 8 {
			return err
		}
		truncateRetrySleep(delay)
		if delay < 400*time.Millisecond {
			delay *= 2
		}
	}
}

// Seams for the tests: the truncate itself and the pause. Production never
// reassigns them (mirrors utils.ReplaceFile's renameFile/replaceFileSleep).
var (
	truncateFile        = os.Truncate
	truncateRetrySleep  = time.Sleep
)
```

- [ ] **Step 5: Rename the direct-download reset to the explicit discard**

In `internal/engine/downloader_direct.go`, rename `resetForStreamingFallback` to `discardStagedMedia(reason string) error`, keeping the body and prepending the log line. Update both existing call sites (`:56`, `:103`) to pass a reason — `"Range probe returned no size"` and `"server ignored the Range mid-download"`. Task 4 rewrites those two sites again; this step only preserves compilation.

```go
// discardStagedMedia is the ONLY place staged media is destroyed on purpose.
// It reopens OutputFile O_TRUNC — not d.outputFile.Truncate, because Windows
// refuses ftruncate on an O_APPEND handle ("Access is denied") and reopening
// also drops the append flag so writes land from byte 0 — zeroes the byte
// counter and clears the resume sidecar. Start's deferred Close reads
// d.outputFile at exit, so reassigning it is safe. No-op when nothing has
// been written yet.
//
// reason is logged: every discard must be attributable, because the guard in
// Start (ErrStagedMediaPresent) exists precisely so that nothing else can do
// this silently.
func (d *SegmentDownloader) discardStagedMedia(reason string) error {
	if d.bytesWritten.Load() == 0 {
		return nil
	}
	d.logger.Warn("[Downloader] Discarding staged media", "file", d.opts.OutputFile,
		"bytes", d.bytesWritten.Load(), "reason", reason)
	d.outputFile.Close()
	f, err := os.OpenFile(d.opts.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("discard staged media: %w", err)
	}
	d.outputFile = f
	d.bytesWritten.Store(0)
	d.ClearResume()
	return nil
}
```

- [ ] **Step 6: Wire the one production setter**

In `internal/worker/strategy_youtube_manifestless_dash.go`, add beside `dbResumeSeq` (`:380-395`):

```go
// manifestlessDiscardStaged reports whether this strategy's downloader may
// truncate staged bytes it is about to restart over. It is the mirror image
// of dbResumeSeq's rule: a non-live manifest-free capture deliberately begins
// again at sq=0 (segments carry their ftyp+moov init inline only there, so a
// partial file cannot be appended to), which means the engine's no-truncate
// guard must stand down for exactly that case. A LIVE capture resumes from
// the DB seq or the sidecar and must never discard; a part that force-starts
// at an orchestrator-provided seq owns its own fresh file.
func manifestlessDiscardStaged(streamStatus youtube.StreamStatus, forcedStartSeq bool) bool {
	return streamStatus != youtube.StreamLive && !forcedStartSeq
}
```

Add `DiscardStaged: manifestlessDiscardStaged(videoInfo.StreamStatus, forceVideoSeq),` to the video `engine.DownloaderOptions` literal at `:282` and `DiscardStaged: manifestlessDiscardStaged(videoInfo.StreamStatus, forceAudioSeq),` to the audio one at `:341`.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/worker/
```

Expected: PASS. Then verify one mutant by execution: delete the `!d.opts.DiscardStaged` term from the guard and confirm `TestStartDiscardStagedMediaOptIn` fails; restore the file byte-identically (`git diff --stat` must be back to the intended change).

- [ ] **Step 8: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/ ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/ ./internal/worker/
```

- [ ] **Step 9: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_direct.go internal/engine/downloader_notruncate_test.go internal/worker/strategy_youtube_manifestless_dash.go internal/worker/strategy_youtube_manifestless_dash_test.go
git commit -m "fix(engine): never truncate non-empty staged media without an explicit discard

ENGINE-1/ENGINE-5 (report #1, verifier merge M2): one shared guard in Start
returns ErrStagedMediaPresent instead of opening a staged recording O_TRUNC,
the resume truncate rides out the Windows sharing-violation window before
returning its error, and the only deliberate discard is now a named call.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader.go internal/engine/downloader_direct.go internal/engine/downloader_notruncate_test.go internal/worker/strategy_youtube_manifestless_dash.go internal/worker/strategy_youtube_manifestless_dash_test.go
```

---

## Task 2: A restart from Muxing muxes what is staged (O-B)

Report row #1 door (a). Owner decision O-B: "On restart, a job found in Muxing MUXES WHAT IS STAGED (the Mux action's `muxFromStaging` path), never re-downloads; the VOD-refresh loop still runs when `incomplete_tail` is set."

**Files:**
- Modify: `internal/worker/worker.go:426-449` (`enqueueExistingJobs`)
- Test: `internal/worker/restart_muxing_test.go` (new)

**Interfaces:**
- Consumes: `HasSegmentFiles(stagingBase, jobID string) bool` (`internal/worker/staging.go:24`), `(*DownloadWorker).MuxJob(jobID string) error` (`internal/worker/worker.go:1441`), `(*DownloadWorker).readConfig` (`:213`).
- Produces: `muxOnRestart(job *database.Job, stagingBase string) bool` (unexported, package `worker`).

- [ ] **Step 1: Write the failing test**

Create `internal/worker/restart_muxing_test.go`:

```go
package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// stageMediaFor drops a recognised media file into <stagingBase>/<jobID> so
// HasSegmentFiles answers true, and returns the staging base.
func stageMediaFor(t *testing.T, stagingBase, jobID string) {
	t.Helper()
	dir := filepath.Join(stagingBase, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video.ts"), []byte("staged"), 0o644); err != nil {
		t.Fatalf("write staged media: %v", err)
	}
}

// TestMuxOnRestart pins owner decision O-B's predicate: an interrupted Muxing
// row whose staging still holds recognised media is re-muxed from what is
// there, EXCEPT when incomplete_tail is set — that row still needs the
// post-live VOD-refresh loop, so it goes back through Downloading.
//
// Mutants, one per row:
//   - dropping the IncompleteTail term: an incomplete-tail row is muxed short
//     and its missing tail is never refreshed.
//   - dropping the HasSegmentFiles term: a Muxing row with empty staging is
//     handed to muxFromStaging, which fails with "no segment files found".
//   - dropping the status term: every job on disk is muxed at boot.
func TestMuxOnRestart(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"muxing with staged media", &database.Job{ID: "staged", Status: database.StatusMuxing}, true},
		{"muxing with incomplete tail", &database.Job{ID: "staged", Status: database.StatusMuxing, IncompleteTail: true}, false},
		{"muxing with empty staging", &database.Job{ID: "empty", Status: database.StatusMuxing}, false},
		{"downloading with staged media", &database.Job{ID: "staged", Status: database.StatusDownloading}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxOnRestart(tc.job, base); got != tc.want {
				t.Errorf("muxOnRestart(%s/%s) = %v, want %v", tc.job.Status, tc.job.ID, got, tc.want)
			}
		})
	}
}

// TestEnqueueExistingJobsRoutesMuxingRow pins the routing itself: a Muxing row
// with staged media is NOT reset to Downloading and NOT put on the queue (the
// mux runs off-queue via MuxJob), while an incomplete-tail Muxing row is.
//
// Mutant: restoring the unconditional "reset interrupted mux job" block — the
// staged row reappears as Downloading in the pending set and re-downloads
// from sq=0 over the complete recording.
func TestEnqueueExistingJobsRoutesMuxingRow(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	staged := &database.Job{ID: "j-staged", VideoID: "v1", Platform: "twitch", Status: database.StatusMuxing}
	tail := &database.Job{ID: "j-tail", VideoID: "v2", Platform: "youtube", Status: database.StatusMuxing, IncompleteTail: true}
	for _, j := range []*database.Job{staged, tail} {
		if err := db.AddJob(j); err != nil {
			t.Fatalf("AddJob %s: %v", j.ID, err)
		}
		stageMediaFor(t, stagingBase, j.ID)
	}

	w.enqueueExistingJobs()

	if fresh, _ := db.GetJob("j-tail"); fresh == nil || fresh.Status != database.StatusDownloading {
		t.Fatalf("incomplete-tail Muxing row = %v, want Downloading (the VOD-refresh loop must still run)", fresh)
	}
	if !w.queue.isPending("j-tail") {
		t.Error("incomplete-tail row was not enqueued")
	}
	if w.queue.isPending("j-staged") {
		t.Error("a Muxing row with staged media must not be enqueued for re-download — it muxes off-queue")
	}
}
```

Imports: `os`, `path/filepath`, `testing`, `internal/config`, `internal/database`. `testWorkerSetup` and `discardLogger` already live in `internal/worker/worker_test.go:51-76`.

Add the tiny read-only probe the test needs to `internal/worker/queue.go` (it is the first user, per the Global Constraints rule on helpers):

```go
// isPending reports whether jobID is waiting in the backlog. Test-facing
// read of state Enqueue owns; kept here so the mutex stays private.
func (q *JobQueue) isPending(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.pendingSet[jobID]
	return ok
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestMuxOnRestart|TestEnqueueExistingJobsRoutesMuxingRow' ./internal/worker/
```

Expected: FAIL — `undefined: muxOnRestart`, and once that compiles, `TestEnqueueExistingJobsRoutesMuxingRow` fails with `j-staged` pending.

- [ ] **Step 3: Implement the routing**

In `internal/worker/worker.go`, replace the `if job.Status == database.StatusMuxing { … }` block inside `enqueueExistingJobs` (`:430-443`) with:

```go
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	for _, job := range jobs {
		if job.Status == database.StatusMuxing {
			if muxOnRestart(job, stagingBase) {
				// Owner decision O-B: mux what is staged. The previous reset
				// to Downloading re-probed the (now post-live) stream, routed
				// it to the manifest-free strategy whose dbResumeSeq seeds 0,
				// and truncated the complete recording (sweep-2 ENGINE-1).
				// The Mux action's path needs no network and no re-download.
				w.logger.Info("resuming interrupted mux from staged media", "jobID", job.ID)
				w.db.UpdateJobFields(job.ID, map[string]any{"error": ""})
				if err := w.MuxJob(job.ID); err != nil {
					// Only reachable if staging vanished between the check and
					// the call; fall back to the historical reset.
					w.logger.Warn("re-mux from staging refused; falling back to re-processing",
						"jobID", job.ID, "err", err)
				} else {
					continue
				}
			}
			// No staged media, or incomplete_tail: there is nothing to mux, or
			// the post-live VOD-refresh loop still has a tail to fetch. Reset
			// to Downloading and clear any stale error string so the UI does
			// not show a prior error alongside the fresh state.
			w.logger.Info("resetting interrupted mux job", "jobID", job.ID)
			w.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusDownloading,
				"error":  "",
			})
			job.Status = database.StatusDownloading
		}
		if ShouldProcess(job) {
			w.queue.Enqueue(job.ID, job.Status)
		}
	}
```

Add beside it:

```go
// muxOnRestart reports whether an interrupted Muxing row should be re-muxed
// from what is already staged instead of re-processed from the start (owner
// decision O-B). Three terms, each load-bearing: the row must actually be in
// Muxing; its staging must still hold recognised media or seg_N parts (else
// muxFromStaging has nothing to work with); and it must NOT be flagged
// incomplete_tail — that row's recording is known to be short, so it still
// needs the post-live VOD-refresh loop that only the download path runs.
func muxOnRestart(job *database.Job, stagingBase string) bool {
	if job == nil || job.Status != database.StatusMuxing || job.IncompleteTail {
		return false
	}
	return HasSegmentFiles(stagingBase, job.ID)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
```

Expected: PASS. Verify the mutant: restore the unconditional reset block and confirm `TestEnqueueExistingJobsRoutesMuxingRow` fails on `j-staged` being pending; then restore.

- [ ] **Step 5: Gates and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/worker/
git add internal/worker/worker.go internal/worker/queue.go internal/worker/restart_muxing_test.go
git commit -m "fix(worker): a restart from Muxing muxes what is staged instead of re-downloading

Owner decision O-B (report #1, ENGINE-1): enqueueExistingJobs routes an
interrupted Muxing row with staged media through the Mux action's
muxFromStaging path. Rows flagged incomplete_tail still reset to Downloading
so the post-live VOD-refresh loop can fetch the missing tail.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/worker.go internal/worker/queue.go internal/worker/restart_muxing_test.go
```

---

## Task 3: A read-progress deadline replaces the 30 s total

Report row #3 (ENGINE-4). `SegmentTimeout` is a TOTAL deadline on the derived context, so any transfer below bytes/30 s fails regardless of progress — reproduced by the verifier with a continuously progressing 10 KB/s transfer killed at the deadline with 0 bytes, and arithmetically a 24 Mbit/s link floor at the default 12 workers × ~7.5 MB Twitch VOD segments. The 30 s stays, as the **idle** bound.

**Files:**
- Modify: `internal/engine/downloader_fetch.go` (`engineHTTPClient:49-54`, `fetchSegment:215-260`, `fetchChunk:642-…`)
- Modify: `internal/engine/downloader.go:111-117` (`SegmentTimeout` doc)
- Test: `internal/engine/downloader_idle_deadline_test.go` (new)

**Interfaces:**
- Produces: `errFetchIdle` (sentinel); `idleBody` (`io.ReadCloser`); `withReadProgressDeadline(parent context.Context, idle time.Duration) (context.Context, *time.Timer, context.CancelFunc)`; `idleFetchError(ctx context.Context, idle time.Duration, err error) error`. All unexported, package `engine`; Task 4 consumes all four.
- Consumes: nothing.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_idle_deadline_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// trickleServer writes total bytes in `chunks` writes spaced `gap` apart,
// flushing each one, so the transfer is continuously progressing but takes
// far longer overall than the idle bound.
func trickleServer(t *testing.T, chunks int, gap time.Duration, chunkSize int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, chunkSize)
		for range chunks {
			w.Write(buf)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(gap)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchSegmentSurvivesSlowButProgressingTransfer pins ENGINE-4's fix: a
// transfer that keeps delivering bytes must complete however long it takes in
// total. Ten 20 ms gaps against a 60 ms idle bound is 200+ ms of wall clock —
// more than three times the deadline — with no gap ever reaching it.
//
// Mutant: restoring `ctx, cancel := context.WithTimeout(parent, SegmentTimeout)`
// in fetchSegment — the fetch dies at 60 ms with 0 bytes and
// context.DeadlineExceeded.
func TestFetchSegmentSurvivesSlowButProgressingTransfer(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 60 * time.Millisecond

	srv := trickleServer(t, 10, 20*time.Millisecond, 1024)
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})

	start := time.Now()
	data, status, err := d.fetchSegment(context.Background(), srv.URL+"/seg")
	if err != nil {
		t.Fatalf("fetchSegment = %v, want nil for a continuously progressing transfer", err)
	}
	if status != http.StatusOK || len(data) != 10*1024 {
		t.Fatalf("fetchSegment = %d bytes/status %d, want 10240/200", len(data), status)
	}
	if elapsed := time.Since(start); elapsed < SegmentTimeout {
		t.Fatalf("transfer took %s — shorter than the idle bound, so it never exercised the deadline", elapsed)
	}
}

// TestFetchSegmentFailsOnIdleStall pins the other half: the 30 s value is
// still a deadline, just an idle one. A server that sends headers and then
// nothing must fail, and the error must name the stall rather than read as a
// caller cancellation.
//
// Mutant: dropping the timer entirely (a plain context.WithCancel) — the
// fetch hangs until the test's own 2 s guard fires.
func TestFetchSegmentFailsOnIdleStall(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 80 * time.Millisecond

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	done := make(chan error, 1)
	go func() {
		_, _, err := d.fetchSegment(context.Background(), srv.URL+"/seg")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errFetchIdle) {
			t.Fatalf("fetchSegment = %v, want errFetchIdle", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchSegment never returned — the idle deadline did not fire")
	}
}

// TestFetchSegmentCallerCancelIsNotAnIdleStall pins the distinction the
// cause carries: a caller that cancels must still see a cancellation, so
// reportFetchFailure's parent guard and the loops' cancelErr keep working.
//
// Mutant: returning errFetchIdle for every context error instead of
// consulting context.Cause — a clean shutdown is reported as a network stall
// and drags the connectivity oracle toward "offline".
func TestFetchSegmentCallerCancelIsNotAnIdleStall(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 5 * time.Second

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	_, _, err := d.fetchSegment(ctx, srv.URL+"/seg")
	if errors.Is(err, errFetchIdle) {
		t.Fatalf("fetchSegment = %v, want a cancellation, not an idle stall", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchSegment = %v, want context.Canceled", err)
	}
}
```

These three rows mutate the package var `SegmentTimeout`, so none of them calls `t.Parallel()`.

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFetchSegment ./internal/engine/
```

Expected: FAIL — `undefined: errFetchIdle`; after stubbing, `TestFetchSegmentSurvivesSlowButProgressingTransfer` fails with `context deadline exceeded`.

- [ ] **Step 3: Add the idle-deadline machinery**

In `internal/engine/downloader_fetch.go`, above `fetchSegment`:

```go
// errFetchIdle is the CAUSE a fetch's derived context carries when the
// read-progress deadline cancelled it, as opposed to the caller cancelling.
// The distinction matters twice: reportFetchFailure must still count a stall
// as network evidence, and a clean shutdown must not be reported as one.
var errFetchIdle = errors.New("no data received within the idle deadline")

// idleBody wraps a response body so that every Read delivering bytes pushes
// the fetch's deadline out again. SegmentTimeout used to be a TOTAL deadline
// on the derived context (sweep-2 ENGINE-4): a 7.5 MB Twitch VOD segment then
// needed 250 KB/s to survive, so twelve default workers imposed a 24 Mbit/s
// link floor below which every segment timed out, retried five times and left
// a permanent gap. moonarchive uses per-read timeouts of 2x the target
// duration for the same reason. The 30 s value is unchanged — it is now what
// it always read like, an IDLE bound.
type idleBody struct {
	rc    io.ReadCloser
	timer *time.Timer
	idle  time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		// Reset on an already-fired AfterFunc timer simply schedules it
		// again; if it has fired the context is already cancelled and this
		// Read's error path takes over, so there is nothing to undo.
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// withReadProgressDeadline derives a context that is cancelled with
// errFetchIdle once `idle` elapses with no progress. The returned timer is
// handed to idleBody so body reads can push it out; the connect-and-headers
// phase runs under the same single arming, which is exactly the old
// behaviour for a server that never answers.
func withReadProgressDeadline(parent context.Context, idle time.Duration) (context.Context, *time.Timer, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := time.AfterFunc(idle, func() { cancel(errFetchIdle) })
	return ctx, timer, func() { timer.Stop(); cancel(nil) }
}

// idleFetchError re-labels a context error that the read-progress deadline
// caused, so callers and logs see a stall rather than a bare cancellation.
// Any other error passes through untouched.
func idleFetchError(ctx context.Context, idle time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errFetchIdle) {
		return fmt.Errorf("stalled: %w for %s", errFetchIdle, idle)
	}
	return err
}
```

- [ ] **Step 4: Convert the two body-reading fetches**

`fetchSegment` (`:215` onward) — note that `defer resp.Body.Close()` evaluates `resp.Body` at defer time, so the wrap must come first:

```go
func (d *SegmentDownloader) fetchSegment(parent context.Context, segURL string) ([]byte, int, error) {
	idle := SegmentTimeout
	ctx, idleTimer, cancel := withReadProgressDeadline(parent, idle)
	defer cancel()

	// Apply GVS PO token to segment URL (query mode: ?pot=token)
	segURL = applyPoTokenQuery(segURL, d.getPoToken())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
		return nil, 0, idleFetchError(ctx, idle, err)
	}
	reportSuccess("engine/fetch")
	// Wrap BEFORE the deferred Close: `defer resp.Body.Close()` binds the
	// receiver at defer time, so wrapping afterwards would close the raw body
	// and leak the timer.
	resp.Body = &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	defer resp.Body.Close()
	…unchanged noteHeadSeqFromResponse / 4xx snippet block…
	data, err := readBody(resp, maxSegmentBodyBytes)
	if err != nil {
		return nil, resp.StatusCode, idleFetchError(ctx, idle, err)
	}
	return data, resp.StatusCode, nil
}
```

`fetchChunk` (`:642` onward) gets the identical four edits: the `withReadProgressDeadline` header, `idleFetchError` on the `Do` error, the `resp.Body` wrap before `defer resp.Body.Close()`, and `idleFetchError` on the body-read error.

- [ ] **Step 5: Drop the client-level ceiling**

The 5-minute `http.Client.Timeout` on `engineHTTPClient` covers the whole body, so it would reimpose a total deadline on exactly the slow-but-progressing transfers this task exists to keep alive (a 7.5 MB segment at 10 KB/s needs 750 s), and it is also ENGINE-6's hard cap on streaming-fallback VOD size. Every user of this client now carries its own deadline: `fetchSegment`/`fetchChunk` the idle one, `probeFileSize`/`probeHeadAt`/the eviction probe an explicit `context.WithTimeout`, and Task 4's streaming fallback the idle one. Replace the `Timeout:` paragraph and the call at `:49-54` with:

```go
// Timeout: none. Every call site carries its own deadline — fetchSegment and
// fetchChunk a read-progress (idle) deadline, the probes an explicit
// context.WithTimeout — and a client-level Timeout covers the whole body, so
// it would re-impose the total deadline sweep-2 ENGINE-4 removed and cap the
// streaming fallback's VOD size at whatever fits in five minutes
// (ENGINE-6). Built via httpx.NewTransport so the keep-alive tuning stays in
// sync with the rest of the codebase.
var engineHTTPClient = httpx.ClientWithTransport(
	0,
	httpx.NewTransport(httpx.TransportOptions{
		MaxIdleConnsPerHost: engineMaxIdleConnsPerHost,
	}),
)
```

In `internal/engine/downloader.go:111-117`, rewrite `SegmentTimeout`'s doc comment to say it is an idle bound:

```go
// SegmentTimeout is the READ-PROGRESS (idle) deadline on a single segment or
// chunk fetch: the fetch is cancelled only after this long with no bytes
// arriving, so a slow-but-moving transfer runs as long as it keeps
// progressing (sweep-2 ENGINE-4). Consumers: fetchSegment and fetchChunk in
// downloader_fetch.go, and the streaming fallback in downloader_direct.go.
// A package var rather than a const so tests can shorten it; production
// never reassigns it.
var SegmentTimeout = 30 * time.Second
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
```

Expected: PASS, including the existing `downloader_fetch_cancel_test.go` and `downloader_fetch_403_test.go` rows. Verify the mutant: restore `context.WithTimeout(parent, SegmentTimeout)` in `fetchSegment` and confirm `TestFetchSegmentSurvivesSlowButProgressingTransfer` fails with `context deadline exceeded`; restore.

- [ ] **Step 7: Gates and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
git add internal/engine/downloader.go internal/engine/downloader_fetch.go internal/engine/downloader_idle_deadline_test.go
git commit -m "fix(engine): SegmentTimeout becomes a read-progress deadline, not a total one

ENGINE-4 (report #3): a fetch is cancelled only after 30 s with no bytes
arriving, so a slow-but-progressing transfer completes instead of timing out,
retrying five times and leaving a permanent gap. The engine client's
5-minute ceiling goes with it — every call site now carries its own deadline.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader.go internal/engine/downloader_fetch.go internal/engine/downloader_idle_deadline_test.go
```

---

## Task 4: The direct-download door — probe retry and a Range-preserving fallback

Report row #1 door (c) / ENGINE-6, plus row #50 (ENGINE-14). A single transient failure of the one-shot 10 s Range probe routes a resumed VOD to the streaming fallback, which truncates the file and clears the sidecar; the verifier reproduced `after ONE failed Range probe: file 12 bytes (was 1048576), sidecar present=false`.

**Files:**
- Modify: `internal/engine/downloader_direct.go` (`runDirectDownload:48-60`, the 200-mid-download branch `:96-110`, `runDirectDownloadFallback:184-240`)
- Modify: `internal/engine/downloader_fetch.go` (`probeFileSize`'s 206 drain `:592`; new `probeFileSizeWithRetry`)
- Test: `internal/engine/downloader_direct_fallback_test.go` (new)

**Interfaces:**
- Consumes: `withReadProgressDeadline`, `idleBody`, `idleFetchError`, `errFetchIdle` (Task 3); `discardStagedMedia` (Task 1).
- Produces: `(*SegmentDownloader).probeFileSizeWithRetry(ctx context.Context) int64`.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_direct_fallback_test.go`:

```go
package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeFileSizeRetriesBeforeFallback pins ENGINE-6(a): one transient
// failure of the Range probe must not condemn a resumed VOD to the
// from-byte-0 streaming fallback.
//
// Mutant: calling probeFileSize once (the pre-arc shape) — the first 500 wins
// and the function returns 0, so runDirectDownload falls back and discards.
func TestProbeFileSizeRetriesBeforeFallback(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	d.delays = fastDelays()

	if got := d.probeFileSizeWithRetry(context.Background()); got != 4096 {
		t.Fatalf("probeFileSizeWithRetry = %d, want 4096 after one transient failure (calls=%d)", got, calls.Load())
	}
}

// TestStreamingFallbackKeepsResumeOffset pins ENGINE-6's other half: when the
// fallback runs on a partially downloaded file it resumes with a Range header
// and APPENDS, instead of truncating the staged bytes away.
//
// Mutant: restoring the unconditional reset (bytesWritten=0 + O_TRUNC before
// the GET) — the output is 8 bytes of tail only and the leading 8 are gone.
func TestStreamingFallbackKeepsResumeOffset(t *testing.T) {
	const head, tail = "HEADHEAD", "TAILTAIL"
	var gotRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		gotRange.Store(rng)
		if strings.HasPrefix(rng, "bytes=8-") {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 8-15/%d", len(head)+len(tail)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(tail))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(head + tail))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte(head), 0o644); err != nil {
		t.Fatalf("seed staged bytes: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open staged file: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f
	d.bytesWritten.Store(int64(len(head)))

	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil", err)
	}
	f.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != head+tail {
		t.Fatalf("file = %q, want %q (the fallback must append from the resume offset)", got, head+tail)
	}
	if r, _ := gotRange.Load().(string); r != "bytes=8-" {
		t.Fatalf("Range header = %q, want \"bytes=8-\"", r)
	}
}

// TestStreamingFallbackDiscardsWhenRangeIgnored pins the one explicit discard
// left on this path: a server that answers 200 to a resume Range is sending
// the file from byte 0, so the staged bytes must go or the output splices.
//
// Mutant: appending the 200 body at the current offset — the file is
// head+head+tail, a doubled, corrupt archive.
func TestStreamingFallbackDiscardsWhenRangeIgnored(t *testing.T) {
	const head, whole = "HEADHEAD", "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Range deliberately ignored
		w.Write([]byte(whole))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	os.WriteFile(path, []byte(head), 0o644)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f
	d.bytesWritten.Store(int64(len(head)))

	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != whole {
		t.Fatalf("file = %q, want %q (an ignored Range must discard, not splice)", got, whole)
	}
}

// TestProbeFileSizeDrainIsBounded pins ENGINE-14 (report #50): the 1-byte
// 206 drain is capped like every other drain in the file, so a server that
// answers a 1-byte range with megabytes cannot be pulled in full.
//
// Mutant: restoring the bare io.Copy(io.Discard, resp.Body) — served counts
// far past maxDrainBytes.
func TestProbeFileSizeDrainIsBounded(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.WriteHeader(http.StatusPartialContent)
		buf := make([]byte, 64<<10)
		for range 16 { // 1 MiB, 16x maxDrainBytes
			n, err := w.Write(buf)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	if got := d.probeFileSize(context.Background()); got != 4096 {
		t.Fatalf("probeFileSize = %d, want 4096", got)
	}
	// Give the server goroutine a moment to notice the closed body.
	time.Sleep(50 * time.Millisecond)
	if n := served.Load(); n > 4*maxDrainBytes {
		t.Fatalf("drained %d bytes, want at most %d — the 206 drain is unbounded", n, 4*maxDrainBytes)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProbeFileSize|TestStreamingFallback' ./internal/engine/
```

Expected: FAIL — `undefined: probeFileSizeWithRetry`; then the two fallback rows fail on a truncated/spliced file.

- [ ] **Step 3: Add the probe retry and bound the drain**

In `internal/engine/downloader_fetch.go`, change the 206 drain (`:592`) to:

```go
	// 1-byte body; safe to drain so the connection can be reused for the
	// real chunked download that follows — but bounded like every other
	// drain here (sweep-2 ENGINE-14): a server that answers a 1-byte range
	// with megabytes must not be pulled in full just to reclaim a socket.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
```

Add below `probeFileSize`:

```go
// probeFileSizeWithRetry re-asks for the file size before the caller gives up
// on Range support. probeFileSize returns 0 for BOTH "this server does not do
// Range" and "that one request failed", and the caller's answer to 0 is the
// from-byte-0 streaming fallback — so a single transient failure used to cost
// a resumed VOD its staged bytes and its sidecar (sweep-2 ENGINE-6). Three
// attempts with a 2 s/4 s backoff (delays.genericRetry, so tests scale it)
// cost a genuinely non-Range server six seconds, once per download.
func (d *SegmentDownloader) probeFileSizeWithRetry(ctx context.Context) int64 {
	const attempts = 3
	for i := range attempts {
		if size := d.probeFileSize(ctx); size > 0 {
			return size
		}
		if d.isCancelled() || ctx.Err() != nil {
			return 0
		}
		if i < attempts-1 {
			d.logger.Debug("[Downloader] Range probe returned no size; retrying", "attempt", i+1)
			if err := utils.Sleep(ctx, d.delays.genericRetry<<i); err != nil {
				return 0
			}
		}
	}
	return 0
}
```

- [ ] **Step 4: Rewrite the two direct-download call sites**

In `internal/engine/downloader_direct.go`, `runDirectDownload`'s prologue:

```go
	// Probe total file size via Range: bytes=0-0, retried so one transient
	// failure cannot route a resumable download into the streaming fallback.
	totalSize := d.probeFileSizeWithRetry(ctx)

	if totalSize <= 0 {
		// The server really does not support Range requests — stream it.
		// No reset here: the fallback resumes from d.bytesWritten with its
		// own Range header and discards only if the server ignores it.
		return d.runDirectDownloadFallback(ctx)
	}
```

and the 200-mid-download branch (`:96-110`) loses its reset for the same reason:

```go
		if statusCode == http.StatusOK {
			d.logger.Warn("[Downloader] direct chunk got 200 (Range ignored) mid-download; restarting via streaming",
				"offset", offset)
			return d.runDirectDownloadFallback(ctx)
		}
```

Replace `runDirectDownloadFallback` with:

```go
// runDirectDownloadFallback streams the file when Range chunking is not
// available. It still SENDS a Range from the resume offset: the fallback used
// to open at byte 0 unconditionally, so a transient probe failure on a
// resumed VOD threw the staged bytes away (sweep-2 ENGINE-6). Only a server
// that answers 200 to that Range — i.e. one that is sending from byte 0 —
// forces a discard, and that discard is explicit.
func (d *SegmentDownloader) runDirectDownloadFallback(parent context.Context) error {
	idle := SegmentTimeout
	ctx, idleTimer, cancel := withReadProgressDeadline(parent, idle)
	defer cancel()

	offset := d.bytesWritten.Load()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getBaseURL(), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	d.setCommonHeaders(req, uaAndroid)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		return idleFetchError(ctx, idle, fmt.Errorf("download: %w", err))
	}
	resp.Body = &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Range honoured — the body continues where the file stops.
	case http.StatusOK:
		if offset > 0 {
			if derr := d.discardStagedMedia("server answered 200 to the resume Range — the body starts at byte 0"); derr != nil {
				return derr
			}
		}
	default:
		bodySnippet := make([]byte, 1024)
		n, _ := resp.Body.Read(bodySnippet)
		d.logger.Debug("[Downloader] direct URL failed",
			"status", resp.StatusCode,
			"url_prefix", truncateURL(d.getBaseURL(), 120),
			"body_snippet", string(bodySnippet[:n]),
		)
		return fmt.Errorf("HTTP %d downloading direct URL", resp.StatusCode)
	}

	buf := make([]byte, 64*1024) // 64KB buffer
	var lastProgressTime time.Time
	for {
		if d.isCancelled() || parent.Err() != nil {
			return d.cancelErr(parent)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			d.noteFetch(n)
			written, writeErr := d.outputFile.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			d.bytesWritten.Add(int64(written))

			if d.OnProgress != nil && time.Since(lastProgressTime) >= ProgressThrottle {
				lastProgressTime = time.Now()
				d.OnProgress(DownloadProgress{Bytes: d.bytesWritten.Load()})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return idleFetchError(ctx, idle, fmt.Errorf("read: %w", readErr))
		}
	}

	return nil
}
```

- [ ] **Step 5: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/
```

Expected: PASS, including the existing `downloader_direct_resume_test.go` and `downloader_direct_validate_test.go`. Mutant to execute: drop the `Range` header from the fallback request and confirm `TestStreamingFallbackKeepsResumeOffset` fails; restore.

```bash
git add internal/engine/downloader_direct.go internal/engine/downloader_fetch.go internal/engine/downloader_direct_fallback_test.go
git commit -m "fix(engine): the direct-download fallback resumes instead of truncating

ENGINE-6 (report #1 door c) and ENGINE-14 (report #50): the Range probe is
retried before the streaming fallback is chosen, the fallback carries the
resume offset in a Range header and discards only when the server answers
200 to it, and the 206 probe drain is capped at maxDrainBytes.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader_direct.go internal/engine/downloader_fetch.go internal/engine/downloader_direct_fallback_test.go
```

---

## Task 5: The Twitch end verdict (O-C)

Report row #2 (ENGINE-3 = TWITCH-2, plus ENGINE-7), verifier merges **M1**, **M3** and **M4**. One GQL sample of `Stream == nil` finalizes a live recording as Finished mid-broadcast; the verifier executed the real `runHlsLoop` and got `GQL samples taken=1; streamEnded=true`. M4 says ENGINE-7 must ship in the SAME change, because fixing the verdict routes more traffic down the exit that finalizes Finished and deletes staging.

**Sites this covers.** The two-sample confirmation goes in the WORKER closures (`worker.go:641-652`), not in `consultStreamEnd` (M1), because `variant.CheckStreamFn` is the single source every consult reads through:

| Consult site | Route |
|---|---|
| `downloader_hls.go:344` playlist 404/410 | `consultStreamEnd` → `opts.CheckStreamStatus` → `orchestrator_twitch.go:247` → `CheckStreamFn` |
| `downloader_hls.go:382` consecutive-fetch escalation | same |
| `downloader_hls.go:430` consecutive-parse escalation | same |
| `downloader_hls.go:629` consecutive stuck skips | raw `opts.CheckStreamStatus` → `CheckStreamFn` |
| `downloader_hls.go:677` init-segment exhaustion | raw `opts.CheckStreamStatus` → `CheckStreamFn` |
| **`downloader_hls.go:741-746` stale window** | raw `ended, _ := d.opts.CheckStreamStatus(ctx)`, error discarded → `CheckStreamFn`. The site ENGINE-3 calls most reachable, and the one a fix inside `consultStreamEnd` would have missed. |
| `orchestrator_twitch.go:1079` post-outage recheck | `RecheckStreamFn` |

**Files:**
- Modify: `internal/twitch/api.go:452-477` (the `GetStreamInfo` body — **nothing else in `internal/twitch`**)
- Modify: `internal/worker/worker.go:641-652` (both closures + the shared helper)
- Modify: `internal/worker/stream_processor_twitch.go:463-469` (`processTwitchLive`'s fetch)
- Modify: `internal/worker/orchestrator_twitch.go:663-667` (the unknown-verdict exit) and the session-loop epilogue
- Modify: `internal/engine/downloader_hls.go:345-355` (the comment ENGINE-16 calls false)
- Test: `internal/worker/twitch_endverdict_test.go` (new)
- Test: `internal/twitch/api_streaminfo_test.go` (new)

**Interfaces:**
- Produces: `confirmTwitchLiveness(ctx context.Context, sample func() (*twitch.TwitchStreamInfo, error)) (*twitch.TwitchStreamInfo, error)`; `(*DownloadWorker).confirmTwitchStreamInfo(ctx context.Context, login string) (*twitch.TwitchStreamInfo, error)`; `twitchEndConfirmDelay` (package var test seam, `worker`); `collapseStreamInfoError(info *TwitchStreamInfo, err error) (*TwitchStreamInfo, error)` (package `twitch`).
- Consumes: `twitch.Service.GetStreamInfo`, `utils.Sleep`, `(*DownloadOrchestrator).resolveChatOutcome`, `(*DownloadOrchestrator).recordChatOutcome`.

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/api_streaminfo_test.go`:

```go
package twitch

import (
	"errors"
	"testing"
)

// TestGetStreamInfoErrorCollapse pins O-C's propagate-only change: a
// transient StreamMetadata slot failure must reach the caller as an ERROR so
// the end verdict defers, while a login that does not resolve keeps the
// historical (nil, nil) "offline" contract every other caller relies on.
//
// Mutants, one per row:
//   - restoring errStreamSlotUnavailable to the collapse list: a GQL flap
//     reads as "the channel went offline" and finalizes a live recording.
//   - dropping ErrChannelNotFound from it: a renamed/banned login errors out
//     of processTwitchLive and waitForTwitchLive instead of parking offline.
func TestGetStreamInfoErrorCollapse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        error
		wantErr   error
		wantIsNil bool
	}{
		{"slot failure propagates", errStreamSlotUnavailable, errStreamSlotUnavailable, false},
		{"channel not found collapses to offline", ErrChannelNotFound, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := collapseStreamInfoError(nil, tc.in)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantIsNil && info != nil {
				t.Fatalf("info = %v, want nil", info)
			}
		})
	}
}
```

Create `internal/worker/twitch_endverdict_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// fakeStreamInfoSource replays a scripted sequence of GetStreamInfo answers
// and counts the calls.
type fakeStreamInfoSource struct {
	answers []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	calls int
}

func (f *fakeStreamInfoSource) next() (*twitch.TwitchStreamInfo, error) {
	i := f.calls
	f.calls++
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i].info, f.answers[i].err
}

func live() *twitch.TwitchStreamInfo  { return &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"} }

// TestConfirmTwitchStreamInfo pins O-C's two-sample rule.
//
// Mutants, one per row:
//   - returning the first sample unconditionally: ONE nil reads as "ended"
//     and truncates a live broadcast (the sweep-2 ENGINE-3 defect).
//   - sampling twice even when the first says live: every consult on a
//     healthy stream pays an extra GQL round trip.
//   - swallowing the second sample's error: an unreachable API reads as
//     "ended" instead of deferring the verdict.
func TestConfirmTwitchStreamInfo(t *testing.T) {
	type answer = struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	slotErr := errors.New("twitch stream metadata slot unavailable")

	for _, tc := range []struct {
		name      string
		answers   []answer
		wantCalls int
		wantLive  bool
		wantErr   bool
	}{
		{"live on the first sample stops there", []answer{{live(), nil}}, 1, true, false},
		{"one nil is not a verdict", []answer{{nil, nil}, {live(), nil}}, 2, true, false},
		{"two nils confirm the end", []answer{{nil, nil}, {nil, nil}}, 2, false, false},
		{"first-sample error defers", []answer{{nil, slotErr}}, 1, false, true},
		{"second-sample error defers", []answer{{nil, nil}, {nil, slotErr}}, 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeStreamInfoSource{answers: tc.answers}
			prev := twitchEndConfirmDelay
			t.Cleanup(func() { twitchEndConfirmDelay = prev })
			twitchEndConfirmDelay = time.Millisecond

			info, err := confirmTwitchLiveness(context.Background(), src.next)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := info != nil && info.IsLive; got != tc.wantLive {
				t.Errorf("live = %v, want %v", got, tc.wantLive)
			}
			if src.calls != tc.wantCalls {
				t.Errorf("GetStreamInfo calls = %d, want %d", src.calls, tc.wantCalls)
			}
		})
	}
}

// TestConfirmTwitchStreamInfoHonoursCancellation pins that a cancelled wait
// between the two samples defers rather than concluding "ended" — a shutdown
// must never be read as the end of a broadcast.
//
// Mutant: ignoring utils.Sleep's error — a shutdown landing in the 5 s gap
// finalizes the recording.
func TestConfirmTwitchStreamInfoHonoursCancellation(t *testing.T) {
	prev := twitchEndConfirmDelay
	t.Cleanup(func() { twitchEndConfirmDelay = prev })
	twitchEndConfirmDelay = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeStreamInfoSource{answers: []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}{{nil, nil}}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	if _, err := confirmTwitchLiveness(ctx, src.next); err == nil {
		t.Fatal("confirmTwitchLiveness = nil error on a cancelled wait, want the cancellation")
	}
	if src.calls != 1 {
		t.Errorf("GetStreamInfo calls = %d, want 1 (the second sample must not run)", src.calls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestGetStreamInfoErrorCollapse ./internal/twitch/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestConfirmTwitch ./internal/worker/
```

Expected: FAIL — `undefined: collapseStreamInfoError`, `undefined: confirmTwitchLiveness`, `undefined: twitchEndConfirmDelay`.

- [ ] **Step 3: Propagate the slot error (the only `internal/twitch` hunk)**

In `internal/twitch/api.go`, replace the tail of `GetStreamInfo` (`:469-476`) with a call to a named helper so the rule is testable and so Arc T's later `api.go` work has an obvious seam:

```go
	return collapseStreamInfoError(a.parseStreamInfo(channelLogin, results[0], results[1]))
}

// collapseStreamInfoError applies the single-call contract to parseStreamInfo's
// result. ErrChannelNotFound still reads as offline — every caller
// (processTwitchLive, waitForTwitchLive, the manual-add route) relies on that,
// and a login that does not resolve IS a settled answer.
//
// errStreamSlotUnavailable no longer does (owner decision O-C, sweep-2
// ENGINE-3/TWITCH-2). A per-element GQL error or null data is a TRANSIENT
// partial-batch failure, and collapsing it into "offline" is what let ONE
// sample finalize a live recording as Finished mid-broadcast: the worker's
// CheckStreamFn read (nil, nil) as "the stream ended". Propagated as an
// error it becomes a deferred verdict at every consult site instead.
func collapseStreamInfoError(info *TwitchStreamInfo, err error) (*TwitchStreamInfo, error) {
	if errors.Is(err, ErrChannelNotFound) {
		return nil, nil
	}
	return info, err
}
```

- [ ] **Step 4: Two-sample confirmation in the worker closures**

In `internal/worker/worker.go`, add above `processJob`:

```go
// twitchEndConfirmDelay is the gap between the two GetStreamInfo samples that
// must agree before a Twitch broadcast is declared over (owner decision O-C).
// A package var so tests can shrink it; production never reassigns it.
var twitchEndConfirmDelay = 5 * time.Second

// confirmTwitchLiveness answers "is this broadcast still live?" from TWO
// samples ~twitchEndConfirmDelay apart, and only when they agree that it is
// not. One sample was enough to finalize a live recording as Finished
// mid-broadcast (sweep-2 ENGINE-3): GetStreamInfo's `Stream == nil` covers a
// real offline AND the documented StreamMetadata flap that twitch_hint.go
// exists to dodge, and every consult site in the HLS loop reads this answer.
//
// The cost is paid only on the answer that ends a recording: a live first
// sample returns immediately, so a healthy stream pays nothing. An error from
// either sample is returned as-is, which the engine classifies as
// verdictUnknown and defers on — a failed check is not a verdict. A
// cancellation in the gap is likewise an error, never an "ended".
//
// sample is a function rather than the service so the rule is testable
// without a Twitch client.
func confirmTwitchLiveness(ctx context.Context, sample func() (*twitch.TwitchStreamInfo, error)) (*twitch.TwitchStreamInfo, error) {
	first, err := sample()
	if err != nil {
		return nil, err
	}
	if first != nil && first.IsLive {
		return first, nil
	}
	if err := utils.Sleep(ctx, twitchEndConfirmDelay); err != nil {
		return nil, err
	}
	return sample()
}

// confirmTwitchStreamInfo binds confirmTwitchLiveness to this worker's Twitch
// service for one channel login.
func (w *DownloadWorker) confirmTwitchStreamInfo(ctx context.Context, login string) (*twitch.TwitchStreamInfo, error) {
	return confirmTwitchLiveness(ctx, func() (*twitch.TwitchStreamInfo, error) {
		return w.tw.GetStreamInfo(ctx, login)
	})
}
```

Replace the two closures at `:641-652`:

```go
			variant.CheckStreamFn = func(innerCtx context.Context) (bool, error) {
				info, err := w.confirmTwitchStreamInfo(innerCtx, login)
				if err != nil {
					return false, err
				}
				return info != nil && info.IsLive, nil
			}
			variant.RecheckStreamFn = func(innerCtx context.Context) (*twitch.TwitchStreamInfo, error) {
				return w.confirmTwitchStreamInfo(innerCtx, login)
			}
```

- [ ] **Step 5: Keep the slot error from erroring a job at startup**

`processTwitchLive` turns any `GetStreamInfo` error into a job error, and a slot failure is now one. Before O-C that flap parked the job with `TwitchOfflineErrMsg`, which the monitor's `twitch_recover.go` auto-recovers; an arbitrary error string is not recovered. Apply the same two-sample rule at `stream_processor_twitch.go:463-469`:

```go
	if streamInfo == nil {
		// Two samples, same rule as the download closures (owner decision
		// O-C): a transient StreamMetadata slot failure now reaches us as an
		// error, and erroring the job on one of those would strand a row the
		// Twitch recovery only picks up when it carries TwitchOfflineErrMsg.
		fetched, err := confirmTwitchLiveness(ctx, func() (*twitch.TwitchStreamInfo, error) {
			return sp.tw.GetStreamInfo(ctx, login)
		})
		if err != nil {
			return nil, fmt.Errorf("twitch stream info: %w", err)
		}
		streamInfo = fetched
	}
```

`waitForTwitchLive` needs no change: it already routes every error through `applyProbeError`, whose budget tolerates transients and resets on any success.

- [ ] **Step 6: Error, not Finished, on the unknown-verdict exit (ENGINE-7, merge M3)**

In `internal/worker/orchestrator_twitch.go`, declare beside `outageFinalize` (before `sessionLoop`):

```go
	// unconfirmedEndErr latches the HLS loop's unknown-verdict exit: the
	// download failed while nothing said the broadcast was over. Finalizing
	// there marked the job Finished and processJob then deleted the staging
	// dir — with the resume sidecar in it (sweep-2 ENGINE-7). Returning the
	// error instead leaves the job in Error with staging intact, which is
	// what a Retry or a monitor re-enqueue needs. Not "resumable": /resume
	// refuses every non-YouTube job.
	var unconfirmedEndErr error
```

Replace the "Normal stop" block (`:663-667`) with:

```go
			// Normal stop. A nil error is the loop's clean end. A non-nil one
			// is the unknown-verdict exit: re-verify once (the closure takes
			// two samples ~5 s apart) and only fall through to finalize when
			// the broadcast is CONFIRMED over.
			if dlErr != nil && ctx.Err() == nil {
				o.logger.Error("Twitch HLS download error", "err", dlErr, "jobID", jobCtx.Job.ID)
				if !isVod && variant.CheckStreamFn != nil {
					stillLive, checkErr := variant.CheckStreamFn(ctx)
					if checkErr != nil || stillLive {
						o.logger.Warn("Twitch download ended without a confirmed stream end — keeping staging for recovery",
							"jobID", jobCtx.Job.ID, "stillLive", stillLive, "checkErr", checkErr)
						unconfirmedEndErr = dlErr
					}
				}
			}
			break
```

After `sessionLoop` ends and before the `muxCtx` block (`:846-850`), add:

```go
	// Unknown-verdict exit: stop chat, record its verdict so the row is
	// honest, and return the download error. processJob's setJobError path
	// returns before os.RemoveAll(jobCtx.StagingDir), so staging AND the
	// resume sidecar survive.
	if unconfirmedEndErr != nil && ctx.Err() == nil {
		if twitchChatDl != nil {
			if twitchChatDl.IsRunning() {
				twitchChatDl.Stop()
			}
			outcome := o.resolveChatOutcome(twitchChatDl, &chatRec, chatDone, 2*time.Second, 2*time.Second)
			o.recordChatOutcome(jobCtx, twitchChatDl.MessageCount(), outcome)
		}
		return unconfirmedEndErr
	}
```

- [ ] **Step 7: Correct the engine comment ENGINE-16 names**

In `internal/engine/downloader_hls.go`, the fall-through comment at `:353-355` says the job "finalizes whatever was captured with streamEnded FALSE, so the resume sidecar survives for a later Resume". On Twitch that was false. Replace the last sentence with:

```go
				// exits with its "N consecutive errors" error. On YouTube the
				// orchestrator finalizes what was captured and the sidecar
				// survives for a later Resume; on Twitch the orchestrator
				// re-verifies and, absent a confirmed end, returns the error
				// so the job lands in Error with its staging and sidecar
				// intact (see ExecuteTwitch's unconfirmedEndErr).
```

- [ ] **Step 8: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/worker/ ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/ ./internal/worker/ ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/twitch/ ./internal/worker/ ./internal/engine/
```

Expected: PASS, including `internal/worker/stream_processor_twitch_credentials_test.go` and `twitch_hint_test.go`. Mutant to execute: make `confirmTwitchLiveness` return `first, nil` unconditionally and confirm the `one nil is not a verdict` row fails; restore.

`collapseStreamInfoError` is a new Go symbol, so this task also gates the citation test:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```

```bash
git add internal/twitch/api.go internal/twitch/api_streaminfo_test.go internal/worker/worker.go internal/worker/stream_processor_twitch.go internal/worker/orchestrator_twitch.go internal/worker/twitch_endverdict_test.go internal/engine/downloader_hls.go
git commit -m "fix(twitch,worker): two samples must agree before a broadcast is declared over

Owner decision O-C (report #2, ENGINE-3/TWITCH-2/ENGINE-7; verifier merges
M1/M3/M4): GetStreamInfo propagates errStreamSlotUnavailable instead of
collapsing it to offline, the worker closures confirm an end from two samples
~5 s apart so every engine consult site is covered, and the unknown-verdict
exit re-verifies and otherwise returns an error so staging and the resume
sidecar survive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/twitch/api.go internal/twitch/api_streaminfo_test.go internal/worker/worker.go internal/worker/stream_processor_twitch.go internal/worker/orchestrator_twitch.go internal/worker/twitch_endverdict_test.go internal/engine/downloader_hls.go
```

---

## Task 6: A bounded HLS VOD reorder buffer

Report row #21 (ENGINE-2). `runHlsVodParallel`'s `buffer := make(map[int][]byte)` has no byte ceiling: the verifier measured 399 of 400 segments held while segment 0 was blocked, and the arithmetic gives 0.6–2.5 GB on a 100 Mbit/s link with the default 12 workers and ~7.5 MB Twitch VOD segments. The DASH catch-up twin already has `catchUpBufferBytes`; this task reuses its `reorderBuffer`.

**Files:**
- Modify: `internal/engine/downloader_hls.go:822-1060` (`runHlsVodParallel`)
- Test: `internal/engine/downloader_hls_vodbuffer_test.go` (new)

**Interfaces:**
- Consumes: `newReorderBuffer(limit, head int) *reorderBuffer`, `(*reorderBuffer).admit/take/setHead/release/residentBytes` (`internal/engine/downloader_parallel.go:65-190`), `catchUpBufferBytes` (`downloader.go:100-108`).
- Produces: `hlsVodBufferBytes` (package var, test seam).

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_hls_vodbuffer_test.go`:

```go
package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHlsVodParallelBufferIsBounded pins ENGINE-2: while the head-of-order
// segment is blocked, the workers must stop fetching once the reorder buffer
// reaches its byte ceiling instead of racing the whole VOD into RAM.
//
// Mutant: restoring `buffer := make(map[int][]byte)` (no ceiling) — all 40
// segments are fetched while segment 0 is blocked, which is the unbounded
// growth the row measured at 0.6-2.5 GB in the field.
func TestHlsVodParallelBufferIsBounded(t *testing.T) {
	const (
		totalSegs = 40
		segSize   = 64 << 10
		workers   = 8
	)
	prev := hlsVodBufferBytes
	t.Cleanup(func() { hlsVodBufferBytes = prev })
	hlsVodBufferBytes = 2 * segSize // room for two non-head segments

	release := make(chan struct{})
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/playlist.m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := range totalSegs {
				fmt.Fprintf(&b, "#EXTINF:2.0,\n/seg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			w.Write([]byte(b.String()))
			return
		}
		if r.URL.Path == "/seg0.ts" {
			<-release
		}
		fetched.Add(1)
		w.Write(make([]byte, segSize))
	}))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/playlist.m3u8",
		OutputFile:     out,
		IsHls:          true,
		SegmentWorkers: workers,
	})
	d.delays = fastDelays()

	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	// Let the pool saturate against the blocked head.
	time.Sleep(300 * time.Millisecond)
	held := fetched.Load()
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return after the head segment was released — the buffer deadlocked")
	}

	// Ceiling (2 segments) + one in-flight slice per worker + the head.
	if maxHeld := int32(2 + workers + 1); held > maxHeld {
		t.Fatalf("%d segments fetched while segment 0 was blocked, want at most %d — the reorder buffer is unbounded", held, maxHeld)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() != int64(totalSegs*segSize) {
		t.Fatalf("output = %v/%v, want %d bytes — every segment must still land", info, err, totalSegs*segSize)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestHlsVodParallelBufferIsBounded ./internal/engine/
```

Expected: FAIL — `undefined: hlsVodBufferBytes`; with the var stubbed at the const, `held` is 39.

- [ ] **Step 3: Back the consumer with `reorderBuffer`**

In `internal/engine/downloader_hls.go`, above `runHlsVodParallel`:

```go
// hlsVodBufferBytes caps the RAM the VOD reorder buffer holds while the
// head-of-order segment retries. The map this replaced had no bound at all
// (sweep-2 ENGINE-2): fetchSegmentWithRetry sleeps 5+10+15+20 s plus up to
// five idle deadlines, and the other workers spent that window racing the
// rest of the playlist into RAM — 0.6-2.5 GB on a 100 Mbit/s link at the
// default 12 workers and ~7.5 MB Twitch VOD segments, capped only by VOD
// size. Same ceiling as the DASH catch-up twin, which has had one since Arc
// 3. A package var so tests can shrink it; production never reassigns it.
var hlsVodBufferBytes = catchUpBufferBytes
```

Inside `runHlsVodParallel`: drop the `data` field from `segResult` (the reorder buffer now carries the bytes and the channel is a readiness signal), and add the buffer beside the `done` channel:

```go
	type segResult struct {
		idx int
	}
	…
	// Byte-bounded reorder buffer. admit() blocks a non-head segment once
	// resident bytes reach the ceiling and ALWAYS admits the head, so
	// nothing can wedge: the consumer's next write always has its segment
	// available, and setHead() below wakes the blocked workers as the window
	// slides. Failures stay nil GAP SENTINELS handled by the consumer — this
	// path never calls markFailed, whose "nothing flushes past a permanent
	// hole" rule is a catch-up invariant, not a VOD one.
	rb := newReorderBuffer(hlsVodBufferBytes, 0)
	// release() frees every blocked worker if the consumer returns early
	// (a write error): nothing will ever read from the buffer again.
	defer rb.release()
```

The worker body becomes:

```go
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				// admit blocks while the buffer is full and this is not the
				// head segment; a false return means the consumer is gone.
				if !rb.admit(item.idx, fetchItem(item)) {
					return
				}
				select {
				case results <- segResult{idx: item.idx}:
				case <-done:
					return
				}
			}
```

And the consumer's flush loop reads from the buffer rather than a local map:

```go
	for range results {
		// Flush consecutive entries (segments and gap sentinels) in order.
		for {
			data, ok := rb.take(nextIdx)
			if !ok {
				break
			}
			…unchanged gap-sentinel / ensureHlsInit / Write / OnProgress body…
			nextIdx++
			// Slide the ceiling's exemption to the new head and wake any
			// worker blocked waiting for room.
			rb.setHead(nextIdx)
			…unchanged periodic saveResume…
		}
	}
```

Delete `buffer := make(map[int][]byte)` and the `delete(buffer, nextIdx)` line, and move each `nextIdx++` above the `rb.setHead(nextIdx)` call in both the gap-sentinel arm and the write arm.

Rewrite the comment block at `:944-952` that ENGINE-16 names false:

```go
	// Stream write: out-of-order segments land in a byte-bounded reorder
	// buffer (see hlsVodBufferBytes) and are written in ascending order as
	// they arrive. Every index produces exactly one entry — segment data or a
	// nil GAP SENTINEL from a failed worker — so the consumer never wedges on
	// a missing index, and the ceiling stops the remaining workers from
	// holding the rest of the VOD in RAM while the head retries. gapStart
	// coalesces consecutive missing indices into one OnGap range and persists
	// across the outer loop.
```

- [ ] **Step 4: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/
```

Expected: PASS, including `downloader_hls_vodgap_test.go` (the gap-sentinel path) and `downloader_hls_fmp4_test.go` (the inline re-init path). Mutant to execute: raise `hlsVodBufferBytes` to `catchUpBufferBytes` inside the test and confirm the bound assertion fails; restore.

```bash
git add internal/engine/downloader_hls.go internal/engine/downloader_hls_vodbuffer_test.go
git commit -m "fix(engine): bound the HLS VOD reorder buffer by bytes

ENGINE-2 (report #21): runHlsVodParallel's consumer is backed by the existing
reorderBuffer under catchUpBufferBytes, so workers wait for room instead of
racing the rest of the VOD into RAM while the head-of-order segment retries.
The comment claiming the map was bounded by the in-flight window goes with it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader_hls.go internal/engine/downloader_hls_vodbuffer_test.go
```

---

## Task 7: The VOD chat wait, the slot release and the sidecar keep (O-A, M5)

Report row #8 (TWITCH-3). Owner decision O-A: "VOD jobs WAIT for chat paging to finish before finalizing, releasing the download slot first (bounded by the VOD's remaining duration or a large ceiling); live jobs keep the 2-minute cut; the chat sidecar is kept when `chat_status == incomplete` regardless." Verifier merge **M5**: the sidecar-keeping half is a one-line condition beside the `IncompleteTail` preservation and ships either way.

**Bound.** A VOD's chat paging is roughly proportional to the video's own length, so the allowance is the video's duration, floored at 30 minutes (short VODs with dense chat) and capped at 6 hours (the "much larger ceiling" O-A permits). `resolveChatOutcome` is unchanged: its post-timeout rule — the result is never nil once the first wait expired — is exactly what must still hold.

**Files:**
- Modify: `internal/worker/orchestrator_chat.go` (the bound, beside `chatStatusIncomplete`)
- Modify: `internal/worker/orchestrator.go:566-569` (the YouTube chat wait)
- Modify: `internal/worker/orchestrator_twitch.go:872-875` (the Twitch chat wait)
- Modify: `internal/worker/worker.go:700-717` (the staging keep)
- Modify: `internal/worker/orphans.go:86-92` (`jobNeedsStaging`)
- Test: `internal/worker/vod_chat_wait_test.go` (new)

**Interfaces:**
- Produces: `vodChatWaitTimeout(job *database.Job) time.Duration`; `vodChatWaitFloor`, `vodChatWaitCeiling` (consts).
- Consumes: `chatStatusIncomplete` (`orchestrator_chat.go:200`), `(*JobQueue).ReleaseDownloadSlot` (`queue.go:185`), `incompleteStagingExpired` (`orphans.go:104`).

- [ ] **Step 1: Write the failing test**

Create `internal/worker/vod_chat_wait_test.go`:

```go
package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func intPtr(n int) *int { return &n }

// TestVodChatWaitTimeout pins owner decision O-A's bound: a finished VOD
// download waits for chat paging roughly as long as the video itself runs,
// floored so a short chat-heavy VOD still gets a real allowance and capped so
// a stalled pager cannot hold a job forever.
//
// Mutants, one per row:
//   - returning chatWaitTimeout: a chat-heavy VOD is cut at two minutes, which
//     is the defect.
//   - dropping the floor: a 3-minute VOD with 40k comments is cut at 3 minutes.
//   - dropping the ceiling: an absurd length_seconds holds the job open for
//     days.
func TestVodChatWaitTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *database.Job
		want time.Duration
	}{
		{"no length falls back to the floor", &database.Job{}, vodChatWaitFloor},
		{"short VOD gets the floor", &database.Job{LengthSeconds: intPtr(180)}, vodChatWaitFloor},
		{"long VOD gets its own duration", &database.Job{LengthSeconds: intPtr(4 * 3600)}, 4 * time.Hour},
		{"absurd length is capped", &database.Job{LengthSeconds: intPtr(90 * 3600)}, vodChatWaitCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vodChatWaitTimeout(tc.job); got != tc.want {
				t.Errorf("vodChatWaitTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestStagingKeptForIncompleteChat pins merge M5's cheap half: a Finished job
// whose chat_status is "incomplete" keeps its staging dir, because the chat
// resume sidecar lives there and deleting it makes the truncation
// unrecoverable.
//
// Mutant: dropping the chat_status term from jobNeedsStaging — the orphan
// scanner offers the preserved dir for deletion and the tail is gone for good.
func TestStagingKeptForIncompleteChat(t *testing.T) {
	_, db := testWorkerSetup(t)
	cfg := &config.MoomboxConfig{}

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"incomplete chat keeps staging", &database.Job{ID: "a", Status: database.StatusFinished, ChatStatus: chatStatusIncomplete}, true},
		{"finished chat does not", &database.Job{ID: "b", Status: database.StatusFinished, ChatStatus: "finished"}, false},
		{"unavailable chat does not", &database.Job{ID: "c", Status: database.StatusFinished, ChatStatus: "unavailable"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobNeedsStaging(db, cfg, tc.job, t.TempDir()); got != tc.want {
				t.Errorf("jobNeedsStaging(chat_status=%q) = %v, want %v", tc.job.ChatStatus, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestVodChatWaitTimeout|TestStagingKeptForIncompleteChat' ./internal/worker/
```

Expected: FAIL — `undefined: vodChatWaitTimeout`, then the incomplete-chat row returns false.

- [ ] **Step 3: Add the bound**

In `internal/worker/orchestrator_chat.go`, below `chatStatusIncomplete`:

```go
const (
	// vodChatWaitFloor and vodChatWaitCeiling bracket how long a finished VOD
	// download waits for its chat source to finish paging (owner decision
	// O-A). Live jobs keep chatWaitTimeout: a live chat stops when the
	// broadcast does, so two minutes of drain is the right shape there.
	//
	// A VOD's chat is different. Twitch pages VOD comments a screenful per
	// GQL round trip while the video downloads at link speed, so a chat-heavy
	// VOD on a fast link predictably still has minutes of paging left when
	// the video completes — and the two-minute cut then Stop()'d it, recorded
	// "incomplete", and deleted the preserved sidecar with staging
	// (sweep-2 TWITCH-3). The allowance scales with the video's own length
	// because the comment count does; the floor covers a short VOD with dense
	// chat and the ceiling stops a stalled pager holding a job open forever.
	vodChatWaitFloor   = 30 * time.Minute
	vodChatWaitCeiling = 6 * time.Hour
)

// vodChatWaitTimeout is the first-wait bound resolveChatOutcome is given for a
// VOD job. The download slot is released before this wait begins — the job is
// no longer downloading, and holding a slot through it would starve the pool.
func vodChatWaitTimeout(job *database.Job) time.Duration {
	wait := vodChatWaitFloor
	if job != nil && job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		if length := time.Duration(*job.LengthSeconds) * time.Second; length > wait {
			wait = length
		}
	}
	return min(wait, vodChatWaitCeiling)
}
```

Add the `database` import to `orchestrator_chat.go` if it is not already present.

- [ ] **Step 4: Use it at both chat-wait sites**

In `internal/worker/orchestrator.go`, replace the `outcome := …` line inside `if chatDl != nil { … }` (`:568`) with:

```go
		// Owner decision O-A: a VOD waits for its chat to finish paging, with
		// the download slot released first so the pool is not held through a
		// wait that is no longer downloading anything. ReleaseDownloadSlot is
		// idempotent, so the release below the mux still runs correctly.
		chatWait := chatWaitTimeout
		if isVod {
			chatWait = vodChatWaitTimeout(jobCtx.Job)
			if o.queue != nil {
				o.queue.ReleaseDownloadSlot(jobCtx.Job.ID)
			}
			o.logger.Debug("waiting for VOD chat to finish paging",
				"jobID", jobCtx.Job.ID, "bound", chatWait)
		}
		outcome := o.resolveChatOutcome(chatDl, &chatRec, chatDone, chatWait, 2*time.Second)
```

Apply the identical block at `internal/worker/orchestrator_twitch.go:873`, with `twitchChatDl` in place of `chatDl`.

- [ ] **Step 5: Keep the staging dir when chat is incomplete (M5)**

In `internal/worker/worker.go`'s cleanup block (`:703-717`):

```go
		fresh, _ := w.db.GetJob(job.ID)
		preserveForTail := fresh != nil && fresh.IncompleteTail
		// A chat capture that ended without completing leaves its resume
		// sidecar in staging; deleting the dir turns a recoverable truncation
		// into a permanent one (sweep-2 TWITCH-3, verifier merge M5). Same
		// shape as the incomplete_tail preservation above it.
		preserveForChat := fresh != nil && fresh.ChatStatus == chatStatusIncomplete
		if w.hasUnmuxedParts(job.ID, jobCtx.StagingDir) {
			…unchanged…
		} else if preserveForTail {
			…unchanged…
		} else if preserveForChat {
			w.logger.Warn("preserving staging dir: chat capture incomplete; the chat resume sidecar stays for a later Retry",
				"path", jobCtx.StagingDir, "jobID", job.ID)
		} else if err := os.RemoveAll(jobCtx.StagingDir); err != nil {
			…unchanged…
		}
```

And in `internal/worker/orphans.go`, extend `jobNeedsStaging` and its doc's "two reasons" sentence to three:

```go
	return (job.IncompleteTail && !incompleteStagingExpired(cfg, job)) ||
		(job.ChatStatus == chatStatusIncomplete && !incompleteStagingExpired(cfg, job)) ||
		hasUnmuxedPartsForJob(db, job.ID, jobStagingDir)
```

- [ ] **Step 6: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/worker/
```

Expected: PASS, including `chat_status_outcome_test.go` and `orphans_test.go`. Mutant to execute: drop the chat term from `jobNeedsStaging` and confirm the `incomplete chat keeps staging` row fails; restore.

```bash
git add internal/worker/orchestrator_chat.go internal/worker/orchestrator.go internal/worker/orchestrator_twitch.go internal/worker/worker.go internal/worker/orphans.go internal/worker/vod_chat_wait_test.go
git commit -m "fix(worker): a VOD waits for its chat, slot-free, and keeps an incomplete sidecar

Owner decision O-A and verifier merge M5 (report #8, TWITCH-3): VOD jobs get a
chat wait scaled to the video's own length (30 min floor, 6 h ceiling) with
the download slot released first, live jobs keep the two-minute cut, and a
Finished job whose chat_status is incomplete keeps its staging dir at finalize
and in the orphan scanner.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/orchestrator_chat.go internal/worker/orchestrator.go internal/worker/orchestrator_twitch.go internal/worker/worker.go internal/worker/orphans.go internal/worker/vod_chat_wait_test.go
```

---

## Task 8: Mux lifecycle — cancel on Stop, verify the output, use `ReplaceFile`

Three rows that all live on the mux path: #22 (ENGINE-8 + O-E), #23 (ENGINE-9) and #10 (TOOL-2's archive-facing rename sites in worker + engine).

O-E: "`DownloadWorker.Stop` CANCELS in-flight background/final mux contexts; partial part files are re-muxed by the restarted child." The muxes hang off `context.Background()` (`quality_split_common.go:105`, `orchestrator_twitch.go:850`), so `Stop`'s 10 s backstop — which `cmd/moombox/shutdown.go:27-30` says "fires routinely" — exits the child while FFmpeg keeps running inside the LAUNCHER's job object on Windows, and the respawned child re-muxes the same part with `-y`.

**Files:**
- Modify: `internal/worker/orchestrator.go` (`DownloadOrchestrator` fields + `NewDownloadOrchestrator`) — declared file-set deviation
- Modify: `internal/worker/quality_split_common.go:105` (`muxRoot()`, `CancelMuxes`, the background mux context)
- Modify: `internal/worker/orchestrator_twitch.go:846-850` (the outage-finalize mux context)
- Modify: `internal/worker/worker.go:1175-1197` (`Stop`)
- Modify: `internal/worker/orchestrator_mux.go:173-180` and `:745-752` (the duration check), `:455`/`:476` (renames)
- Modify: `internal/worker/mux_finalize.go:53`, `:97` (renames)
- Modify: `internal/engine/downloader_resume.go:190` (rename)
- Test: `internal/worker/mux_lifecycle_test.go` (new)

**Interfaces:**
- Produces: `(*DownloadOrchestrator).muxRoot() context.Context`; `(*DownloadOrchestrator).CancelMuxes()`; `muxedOutputIsShort(inputSec, outputSec float64) bool`; `(*DownloadOrchestrator).verifyMuxedDuration(ctx context.Context, jobID string, outputSec float64, inputs ...string)`; `muxShortfallTolerance`, `muxDurationCheckFloor`, `muxCancelGrace` (consts, package `worker`).
- Consumes: `(*DownloadOrchestrator).runFFprobe` (`orchestrator_utils.go:47`), `utils.ReplaceFile` (`internal/utils/replacefile.go:16`).

- [ ] **Step 1: Write the failing test**

Create `internal/worker/mux_lifecycle_test.go`:

```go
package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCancelMuxesCutsTheMuxRoot pins owner decision O-E: every background and
// final mux descends from one cancellable root, so the worker's shutdown can
// kill FFmpeg instead of leaving it writing into a staging dir the respawned
// child is about to re-mux with -y.
//
// Mutant: restoring context.Background() in launchBackgroundSegmentMux —
// muxRoot() is never consulted, CancelMuxes cancels nothing, and the orphan
// keeps writing after the child exits.
func TestCancelMuxesCutsTheMuxRoot(t *testing.T) {
	o := NewDownloadOrchestrator(nil, nil, "ffmpeg", discardLogger{}, nil, nil, nil, nil, nil)

	root := o.muxRoot()
	if root.Err() != nil {
		t.Fatalf("muxRoot().Err() = %v before cancellation, want nil", root.Err())
	}
	o.CancelMuxes()
	select {
	case <-root.Done():
	default:
		t.Fatal("CancelMuxes did not cancel the mux root context")
	}
}

// TestMuxRootOnZeroValueOrchestrator pins the nil guard: tests and the
// standalone Mux action construct orchestrators without the constructor, and
// a nil root must degrade to Background rather than panic.
//
// Mutant: returning o.muxRootCtx unguarded — a struct-literal orchestrator
// hands a nil context to context.WithTimeout and panics.
func TestMuxRootOnZeroValueOrchestrator(t *testing.T) {
	o := &DownloadOrchestrator{}
	if got := o.muxRoot(); got == nil {
		t.Fatal("muxRoot() = nil on a zero-value orchestrator, want context.Background()")
	}
	o.CancelMuxes() // must not panic
}

// TestMuxedOutputIsShort pins ENGINE-9: `-c copy` stops at the first
// undemuxable fragment and exits 0, so a truncated .mp4 used to finish as a
// clean job. The check only fires when the INPUT probe produced a plausible
// duration, so a fragmented raw stream whose container metadata reports 0
// (or a couple of seconds) can never raise a false alarm.
//
// Mutants, one per row:
//   - dropping the tolerance: normal container jitter flags every archive.
//   - dropping the floor: a 30-second clip's rounding flags it.
//   - comparing the wrong way round: a LONGER output is flagged.
func TestMuxedOutputIsShort(t *testing.T) {
	for _, tc := range []struct {
		name         string
		input, output float64
		want          bool
	}{
		{"truncated at the first bad fragment", 7200, 300, true},
		{"container jitter within tolerance", 7200, 7195, false},
		{"input too short to judge", 30, 5, false},
		{"input duration unknown", 0, 300, false},
		{"output longer than input", 3600, 3605, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxedOutputIsShort(tc.input, tc.output); got != tc.want {
				t.Errorf("muxedOutputIsShort(in=%v, out=%v) = %v, want %v", tc.input, tc.output, got, tc.want)
			}
		})
	}
}

```

TOOL-2's swap gets no new test on purpose. `utils.ReplaceFile` is `os.Rename`
plus a retry on two Windows sharing-violation codes, so on an uncontended file
the two are behaviourally identical and any test would pass under the mutant —
duplicating `rename_single_part_test.go` and `mux_finalize_test.go`, which
already pin that the renames happen. The swap is verified by inspection in
Step 6 instead, with an exact expected output.

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestCancelMuxes|TestMuxRoot|TestMuxedOutputIsShort' ./internal/worker/
```

Expected: FAIL — `undefined: muxRoot`, `undefined: CancelMuxes`, `undefined: muxedOutputIsShort`.

- [ ] **Step 3: Add the mux root context (O-E)**

In `internal/worker/orchestrator.go`, add to the `DownloadOrchestrator` struct:

```go
	// muxRootCtx parents every mux that must OUTLIVE its job's own context —
	// the background part muxes and the connectivity-outage finalize, both of
	// which used context.Background() and were therefore unreachable from
	// DownloadWorker.Stop. muxRootCancel is what Stop cuts (owner decision
	// O-E): the 10 s shutdown backstop "fires routinely" while a part mux
	// drains, and on Windows the orphaned FFmpeg then lives on in the
	// LAUNCHER's kill-on-close job object while the respawned child re-muxes
	// the same output with -y. Partial part files are re-muxed on restart, so
	// cancelling costs nothing.
	muxRootCtx    context.Context
	muxRootCancel context.CancelFunc
```

In `NewDownloadOrchestrator`:

```go
func NewDownloadOrchestrator(db *database.Database, queue *JobQueue, ffmpegPath string, logger logger, cs *cipher.GojaResolver, routedCs cipher.Solver, pp *bgutils.PotProvider, nm *notifications.Manager, conn Connectivity) *DownloadOrchestrator {
	muxRootCtx, muxRootCancel := context.WithCancel(context.Background())
	return &DownloadOrchestrator{
		muxer:         engine.NewMuxer(ffmpegPath, logger),
		ffmpegPath:    ffmpegPath,
		db:            db,
		queue:         queue,
		cipherSolver:  cs,
		routedCipher:  routedCs,
		potProvider:   pp,
		notifier:      nm,
		conn:          conn,
		logger:        logger,
		muxRootCtx:    muxRootCtx,
		muxRootCancel: muxRootCancel,
	}
}
```

In `internal/worker/quality_split_common.go`, above `launchBackgroundSegmentMux`:

```go
// muxRoot is the parent context for muxes that outlive their job's context.
// Falls back to Background for an orchestrator built by struct literal (the
// standalone Mux action's helpers and the package's own tests), so nothing
// ever hands a nil context to context.WithTimeout.
func (o *DownloadOrchestrator) muxRoot() context.Context {
	if o.muxRootCtx == nil {
		return context.Background()
	}
	return o.muxRootCtx
}

// CancelMuxes cancels every in-flight background and final mux. Called by
// DownloadWorker.Stop once the in-flight wait has run out (owner decision
// O-E) so FFmpeg dies with the child rather than outliving it.
func (o *DownloadOrchestrator) CancelMuxes() {
	if o.muxRootCancel != nil {
		o.muxRootCancel()
	}
}
```

and change the goroutine's context:

```go
		muxCtx, muxCancel := context.WithTimeout(o.muxRoot(), 2*time.Hour)
```

In `internal/worker/orchestrator_twitch.go:846-850`:

```go
	// After the fall-through from connectivity loss, use the mux root for
	// muxing: the job's own context is cancelled, but a shutdown must still
	// be able to reach this FFmpeg (owner decision O-E).
	muxCtx := ctx
	if outageFinalize {
		muxCtx = o.muxRoot()
	}
```

In `internal/worker/worker.go`'s `Stop`:

```go
// muxCancelGrace is how long Stop waits for FFmpeg to die after the mux root
// is cancelled, before giving up and exiting anyway.
const muxCancelGrace = 2 * time.Second
```

```go
	select {
	case <-done:
		w.logger.Info("download worker: all in-flight jobs finished")
	case <-time.After(10 * time.Second):
		// Owner decision O-E: the jobs still running at this point are almost
		// always draining a mux, and exiting now would leave FFmpeg writing
		// into a staging dir the restarted child re-muxes with -y. Cancel the
		// mux root so those processes die with us; their partial part files
		// are re-muxed on restart.
		w.logger.Warn("download worker: timed out waiting for in-flight jobs; cancelling in-flight muxes")
		if w.orchestrator != nil {
			w.orchestrator.CancelMuxes()
		}
		select {
		case <-done:
			w.logger.Info("download worker: in-flight jobs finished after mux cancellation")
		case <-time.After(muxCancelGrace):
			w.logger.Warn("download worker: in-flight jobs still running after mux cancellation")
		}
	}
```

- [ ] **Step 4: Check the muxed output's duration (ENGINE-9)**

In `internal/worker/orchestrator_mux.go`, above `muxAndFinalize`:

```go
const (
	// muxShortfallTolerance is how much shorter than its input a muxed output
	// may be before it is treated as truncated. Two Twitch VOD segments'
	// worth (10 s each) — the coarsest segment duration in play — so ordinary
	// container-metadata rounding never trips it.
	muxShortfallTolerance = 20 * time.Second
	// muxDurationCheckFloor is the shortest input the check judges at all. A
	// fragmented raw stream's container metadata is unreliable and often
	// reports a fraction of the real length, so only a plausibly long input
	// is compared; anything shorter is skipped rather than guessed at.
	muxDurationCheckFloor = 60 * time.Second
)

// muxedOutputIsShort reports whether a muxed output is short enough to mean
// FFmpeg's `-c copy` stopped at the first undemuxable fragment. It exits 0
// when that happens, so a truncated .mp4 used to finish as a clean job with
// nothing comparing it against what went in (sweep-2 ENGINE-9).
//
// Both durations are seconds as ffprobe reports them. The check is
// deliberately one-sided and conservative: an input the probe could not read
// (0) or one too short to judge is never flagged, and an output LONGER than
// its input never is either.
func muxedOutputIsShort(inputSec, outputSec float64) bool {
	if inputSec < muxDurationCheckFloor.Seconds() || outputSec <= 0 {
		return false
	}
	return inputSec-outputSec > muxShortfallTolerance.Seconds()
}

// verifyMuxedDuration probes the longest input beside the already-probed
// output and, when the output is short, warns and flags the job
// incomplete_tail — the flag that keeps staging (jobNeedsStaging) and shows
// the honest "may be missing its tail" badge, so the archive is recoverable
// by Retry instead of silently truncated.
func (o *DownloadOrchestrator) verifyMuxedDuration(ctx context.Context, jobID string, outputSec float64, inputs ...string) {
	var longest float64
	for _, in := range inputs {
		if in == "" {
			continue
		}
		if probe := o.runFFprobe(ctx, in); probe != nil && probe.DurationSec > longest {
			longest = probe.DurationSec
		}
	}
	if !muxedOutputIsShort(longest, outputSec) {
		return
	}
	o.logger.Warn("muxed output is shorter than its input — the copy may have stopped at a bad fragment",
		"jobID", jobID, "inputSeconds", longest, "outputSeconds", outputSec)
	o.db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": true})
}
```

Call it in `muxAndFinalize`, immediately after `probeData := o.runFFprobe(ctx, outputFile)` (`:178`) — before `keepIncompleteTailProgress` reads the fresh row, so the honest progress line survives:

```go
	if probeData != nil {
		o.verifyMuxedDuration(ctx, jobCtx.Job.ID, probeData.DurationSec, videoPath, audioPath)
	}
```

and in `muxSegment`, immediately after `probeData := o.runFFprobe(ctx, outputPath)` (`:750`):

```go
	if probeData != nil {
		o.verifyMuxedDuration(ctx, jobCtx.Job.ID, probeData.DurationSec, videoPath, audioPath)
	}
```

- [ ] **Step 5: Swap the archive-facing renames (TOOL-2)**

Five sites, all of them renaming a file the archive points at:

| File:line | Change |
|---|---|
| `internal/worker/orchestrator_mux.go:455` | `os.Rename(seg.FilePath, plainVideo)` → `utils.ReplaceFile(seg.FilePath, plainVideo)` |
| `internal/worker/orchestrator_mux.go:476` | `os.Rename(seg.ChatFile, plainChat)` → `utils.ReplaceFile(seg.ChatFile, plainChat)` |
| `internal/worker/mux_finalize.go:53` | `return os.Rename(tmpPath, outputPath)` → `return utils.ReplaceFile(tmpPath, outputPath)` |
| `internal/worker/mux_finalize.go:97` | same |
| `internal/engine/downloader_resume.go:190` | `os.Rename(tmpFile, d.opts.ResumeFile)` → `utils.ReplaceFile(tmpFile, d.opts.ResumeFile)` |

Add the `internal/utils` import where missing. Every surrounding comment and recovery branch stays as written — `ReplaceFile` is `os.Rename` plus a retry on the two Windows sharing-violation codes, so the "destination already exists" re-entry logic at `:455` is unaffected. Leave `orchestrator_mux.go:32` (`writeDescriptionAtomic`) alone: TOOL-19 folds it into `utils.WriteFileAtomic` in Arc X.

Note in the commit body that TOOL-2's other half — the false "every atomic write" claim in `utils.ReplaceFile`'s doc comment — belongs to Arc X, which owns `internal/utils/**`.

- [ ] **Step 6: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/ ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/worker/ ./internal/engine/
```

Expected: PASS, including `rename_single_part_test.go`, `mux_finalize_test.go` and `downloader_resume_test.go`. Mutant to execute: restore `context.Background()` in `launchBackgroundSegmentMux` and confirm a scratch assertion on the derived context's `Done()` fails — or, more cheaply, delete the `o.muxRootCancel != nil` guard's body and confirm `TestCancelMuxesCutsTheMuxRoot` fails; restore.

Verify the rename swap by inspection:

```bash
grep -n "os\.Rename" internal/worker/orchestrator_mux.go internal/worker/mux_finalize.go internal/engine/downloader_resume.go
```

Expected: exactly ONE line — `internal/worker/orchestrator_mux.go:32` inside `writeDescriptionAtomic`, which TOOL-19 folds into `utils.WriteFileAtomic` in Arc X. Any other hit is an unswapped site.

```bash
git add internal/worker/orchestrator.go internal/worker/quality_split_common.go internal/worker/orchestrator_twitch.go internal/worker/worker.go internal/worker/orchestrator_mux.go internal/worker/mux_finalize.go internal/worker/mux_lifecycle_test.go internal/engine/downloader_resume.go
git commit -m "fix(worker,engine): cancel muxes on Stop, verify the output, rename via ReplaceFile

Owner decision O-E plus reports #23 and #10: background and final muxes now
descend from one cancellable root the worker's Stop cuts after its 10 s wait,
a muxed output materially shorter than its input flags the job incomplete_tail
instead of finishing clean, and the five archive-facing renames go through
utils.ReplaceFile's Windows AV/indexer retry. TOOL-2's doc-comment half is
Arc X's, which owns internal/utils.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/orchestrator.go internal/worker/quality_split_common.go internal/worker/orchestrator_twitch.go internal/worker/worker.go internal/worker/orchestrator_mux.go internal/worker/mux_finalize.go internal/worker/mux_lifecycle_test.go internal/engine/downloader_resume.go
```

---

## Task 9: The lifecycle slot is taken only when `ShouldDownload` says so (O-F)

Report row #24 (ENGINE-10). The verifier's refinement matters and belongs in the commit message: these are **two separate hundreds**. At 100 held lifecycle slots `Dequeue` BLOCKS, so a newly live stream is never started (not dropped, not logged); the drop Warn and its 1-line-per-minute-per-job flood additionally needs the pending list to reach 100, i.e. ~200 tracked jobs. O-F: "The 100-slot lifecycle slot is taken only when `ShouldDownload` says so; the wait phase (Upcoming, manual offline Twitch) runs slot-free; a drop is logged once per job."

**Files:**
- Modify: `internal/worker/queue.go` (struct, `NewJobQueue`, `Enqueue`, `Dequeue`, `Complete`, new acquire/release)
- Modify: `internal/worker/worker.go:600-611` (`processJob`)
- Test: `internal/worker/queue_lifecycle_test.go` (new)

**Interfaces:**
- Produces: `(*JobQueue).AcquireLifecycleSlot(ctx context.Context, jobID string) bool`; `(*JobQueue).releaseLifecycleSlotLocked(jobID string)` (unexported).
- Consumes: `(*JobQueue).isPending` (Task 2).

- [ ] **Step 1: Write the failing test**

Create `internal/worker/queue_lifecycle_test.go`:

```go
package worker

import (
	"context"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDequeueDoesNotTakeALifecycleSlot pins owner decision O-F: the wait phase
// (Upcoming, a manually-added offline Twitch channel) runs slot-free, so a
// hundred waiting jobs cannot stop a newly live stream from being dequeued.
//
// Mutant: restoring `q.activeLifecycle < q.maxLifecycle` to Dequeue's
// condition and the `q.activeLifecycle++` beside it — the 101st Dequeue
// blocks and the live job is never started.
func TestDequeueDoesNotTakeALifecycleSlot(t *testing.T) {
	q := NewJobQueue(10)
	ctx := context.Background()

	for i := range 101 {
		q.Enqueue(jobIDf(i), database.StatusUpcoming)
	}
	for range 100 {
		if _, _, ok := q.Dequeue(ctx); !ok {
			t.Fatal("Dequeue = false while jobs were pending")
		}
	}

	got := make(chan bool, 1)
	go func() {
		_, _, ok := q.Dequeue(ctx)
		got <- ok
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("Dequeue = false for the 101st waiting job")
		}
	case <-time.After(time.Second):
		t.Fatal("Dequeue blocked on the 101st waiting job — the wait phase is still holding lifecycle slots")
	}
}

// TestAcquireLifecycleSlotGatesDownloads pins the other half: the slot is
// taken at the ShouldDownload decision, and released by Complete.
//
// Mutants, one per assertion:
//   - never taking the slot: the cap is gone entirely and the 101st download
//     starts.
//   - not releasing it in Complete: the cap leaks and the pool wedges after
//     100 finished jobs.
func TestAcquireLifecycleSlotGatesDownloads(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 2
	ctx := context.Background()

	for i := range 3 {
		q.Enqueue(jobIDf(i), database.StatusLive)
		q.Dequeue(ctx)
	}
	if !q.AcquireLifecycleSlot(ctx, jobIDf(0)) || !q.AcquireLifecycleSlot(ctx, jobIDf(1)) {
		t.Fatal("the first two AcquireLifecycleSlot calls must succeed")
	}

	blocked := make(chan bool, 1)
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	go func() { blocked <- q.AcquireLifecycleSlot(waitCtx, jobIDf(2)) }()
	if ok := <-blocked; ok {
		t.Fatal("AcquireLifecycleSlot succeeded past maxLifecycle")
	}

	q.Complete(jobIDf(0))
	if !q.AcquireLifecycleSlot(ctx, jobIDf(2)) {
		t.Fatal("Complete did not release the lifecycle slot")
	}
}

// TestEnqueueDropLogsOncePerJob pins the last clause of O-F: the 60 s
// heartbeat re-offers every dropped job forever, and the Warn used to fire
// every time — one line per minute per job.
//
// Mutant: dropping the droppedLogged bookkeeping — warns is 3 instead of 1.
func TestEnqueueDropLogsOncePerJob(t *testing.T) {
	q := NewJobQueue(10)
	counter := &countingLogger{}
	q.SetLogger(counter)

	for i := range 100 {
		q.Enqueue(jobIDf(i), database.StatusUpcoming)
	}
	for range 3 { // three simulated heartbeats re-offering the same job
		q.Enqueue("overflow", database.StatusUpcoming)
	}
	if counter.warns != 1 {
		t.Fatalf("drop warns = %d, want 1 per job", counter.warns)
	}
}

func jobIDf(i int) string { return "job-" + strconv.Itoa(i) }

type countingLogger struct{ warns int }

func (c *countingLogger) Debug(string, ...any) {}
func (c *countingLogger) Info(string, ...any)  {}
func (c *countingLogger) Warn(string, ...any)  { c.warns++ }
func (c *countingLogger) Error(string, ...any) {}
```

Imports: `context`, `strconv`, `testing`, `time`, `internal/database`.

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestDequeueDoesNotTake|TestAcquireLifecycleSlot|TestEnqueueDropLogsOnce' ./internal/worker/
```

Expected: FAIL — `undefined: AcquireLifecycleSlot`; `TestDequeueDoesNotTakeALifecycleSlot` times out on the 101st dequeue; `TestEnqueueDropLogsOncePerJob` reports 3 warns.

- [ ] **Step 3: Move the gate**

In `internal/worker/queue.go`, add to the struct and constructor:

```go
	holdingLifecycle map[string]bool   // jobs holding a lifecycle slot
	droppedLogged    map[string]struct{} // jobs whose backlog drop has been logged
	lifeNotify       chan struct{}     // signals a freed lifecycle slot
```

```go
		holdingLifecycle: make(map[string]bool),
		droppedLogged:    make(map[string]struct{}),
		lifeNotify:       make(chan struct{}, 1),
```

Rewrite `Enqueue`'s backlog-limit branch and success tail:

```go
	// Backlog limit to prevent unbounded growth (matches TS queue.size >= 100)
	if len(q.pending) >= 100 {
		// Once per job, not once per offer: the 60 s heartbeat re-enqueues
		// every ShouldProcess job forever, so the old unconditional Warn was
		// one line per minute per dropped job (sweep-2 ENGINE-10).
		if _, logged := q.droppedLogged[jobID]; !logged {
			q.droppedLogged[jobID] = struct{}{}
			if q.logger != nil {
				q.logger.Warn("job queue full, dropping job", "jobID", jobID, "limit", 100)
			}
		}
		return
	}

	delete(q.droppedLogged, jobID)
	q.pending = append(q.pending, pendingJob{ID: jobID, Priority: calculatePriority(status)})
```

Change `Dequeue`'s condition — the lifecycle slot no longer gates it:

```go
		// No lifecycle gate here (owner decision O-F): the slot is claimed at
		// the ShouldDownload decision instead. Gating the DEQUEUE meant every
		// Upcoming job and every manually-added offline Twitch channel held
		// one of the 100 slots for its whole wait — hours to days — and at
		// 100 waiters a newly live stream was never started at all.
		if len(q.pending) > 0 {
```

and delete the `q.activeLifecycle++` line from its body.

Add the acquire/release pair:

```go
// AcquireLifecycleSlot blocks until one of the maxLifecycle slots is free,
// then claims it for jobID. Called once stream processing has decided the job
// will actually download (owner decision O-F), so the cap now bounds
// CONCURRENT DOWNLOADS rather than concurrent waits — a limit no realistic
// install approaches, which is the point: the wait phase is unbounded except
// by per-job goroutine cost. Returns false if ctx is cancelled first.
func (q *JobQueue) AcquireLifecycleSlot(ctx context.Context, jobID string) bool {
	for {
		q.mu.Lock()
		if q.activeLifecycle < q.maxLifecycle {
			q.activeLifecycle++
			q.holdingLifecycle[jobID] = true
			stillFree := q.activeLifecycle < q.maxLifecycle
			q.mu.Unlock()
			if stillFree {
				select {
				case q.lifeNotify <- struct{}{}:
				default:
				}
			}
			return true
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-q.lifeNotify:
		}
	}
}

// releaseLifecycleSlotLocked frees jobID's lifecycle slot if it holds one.
// Caller holds q.mu.
func (q *JobQueue) releaseLifecycleSlotLocked(jobID string) {
	if !q.holdingLifecycle[jobID] {
		return
	}
	delete(q.holdingLifecycle, jobID)
	q.activeLifecycle--
	select {
	case q.lifeNotify <- struct{}{}:
	default:
	}
}
```

In `Complete`, replace the `q.activeLifecycle--` line inside the `processing` branch with `q.releaseLifecycleSlotLocked(jobID)` (moved OUT of that branch, just above it, so a job that acquired a slot and then had its row deleted still releases), and add `delete(q.droppedLogged, jobID)` beside the existing `delete(q.cancelled, jobID)`.

- [ ] **Step 4: Claim the slot at the ShouldDownload decision**

In `internal/worker/worker.go`'s `processJob`, immediately before `acquireDownloadSlot` (`:603`):

```go
	// Lifecycle slot (owner decision O-F): claimed HERE, once stream
	// processing has decided this job downloads. Dequeue used to claim it,
	// which meant the wait phase held one for hours.
	if !w.queue.AcquireLifecycleSlot(ctx, jobID) {
		w.handleCancellation(job)
		return
	}

	// Acquire download slot — for VODs, blocks until a slot is available;
	// broadcasts pass through ungated (see acquireDownloadSlot).
	if !w.acquireDownloadSlot(ctx, jobID, result.IsVod) {
```

- [ ] **Step 5: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/worker/
```

Expected: PASS, including the existing `queue_test.go` rows (`TestNewJobQueue` asserts `maxLifecycle == 100`, which is unchanged). Mutant to execute: remove the `delete(q.droppedLogged, jobID)`/lookup pair and confirm `TestEnqueueDropLogsOncePerJob` reports 3; restore.

```bash
git add internal/worker/queue.go internal/worker/worker.go internal/worker/queue_lifecycle_test.go
git commit -m "fix(worker): the lifecycle slot is claimed at ShouldDownload, not at Dequeue

Owner decision O-F (report #24, ENGINE-10). Two separate hundreds were in
play: at 100 held lifecycle slots Dequeue BLOCKED, so a newly live stream was
never started; the drop Warn's one-line-per-minute-per-job flood additionally
needed ~200 tracked jobs. The wait phase now runs slot-free and a backlog drop
is logged once per job.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/queue.go internal/worker/worker.go internal/worker/queue_lifecycle_test.go
```

---

## Task 10: Engine durability and the Low engine rows

Report rows #25 (ENGINE-11 + O-G), #49 (ENGINE-13), #51 (ENGINE-15) and #48 (ENGINE-12). O-G: "`outputFile.Sync()` immediately before each `saveResume` (HLS ~4/min; catch-up ≤ ~18/min per track)."

**Files:**
- Modify: `internal/engine/downloader_resume.go:146-197` (`saveResume`)
- Modify: `internal/engine/connectivity_wait.go:20-46` (`callIsOnline`)
- Modify: `internal/engine/muxer.go` (`ffmpegPathArg`, applied in `buildArgs`)
- Test: `internal/engine/downloader_durability_test.go` (new)
- Test: `internal/engine/muxer_test.go` (extend)

**Interfaces:**
- Produces: `ffmpegPathArg(p string) string` and `ffmpegPathOS` (package var, test seam); no new exported symbols.
- Consumes: `(*SegmentDownloader).outputFile`, `bytesWritten`, `currentSeq`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/downloader_durability_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveResumeFsyncsMediaFirst pins owner decision O-G: the sidecar (and
// the DB's last_*_seq behind it) are durable while the media bytes were
// never fsynced, so after a power loss the persisted position could LEAD the
// durable media and the next run appended after a torn or zero-filled tail.
//
// Mutant: dropping the syncMediaFile call — syncs is 0 and the persisted
// position can outrun the bytes it describes.
func TestSaveResumeFsyncsMediaFirst(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video.ts")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	f.Write([]byte("mediabytes"))

	syncs := 0
	prev := syncMediaFile
	t.Cleanup(func() { syncMediaFile = prev })
	syncMediaFile = func(file *os.File) error { syncs++; return file.Sync() }

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(5)
	d.bytesWritten.Store(10)

	d.saveResume()
	if syncs != 1 {
		t.Fatalf("media syncs = %d, want 1 — saveResume must fsync the media first", syncs)
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
}

// TestSaveResumeSkipsSaveWhenFsyncFails pins the failure rule: a position
// that cannot be backed by durable bytes is not written at all, so the
// PREVIOUS sidecar — which does describe bytes on disk — stands.
//
// Mutant: logging the error and saving anyway — the sidecar advances past
// data that may never reach the platter.
func TestSaveResumeSkipsSaveWhenFsyncFails(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video.ts")
	f, _ := os.Create(out)
	t.Cleanup(func() { f.Close() })
	f.Write([]byte("mediabytes"))

	prev := syncMediaFile
	t.Cleanup(func() { syncMediaFile = prev })
	syncMediaFile = func(*os.File) error { return errors.New("disk is on fire") }

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(5)
	d.bytesWritten.Store(10)

	d.saveResume()
	if _, err := os.Stat(out + ".resume.json"); err == nil {
		t.Fatal("a sidecar was written although the media fsync failed")
	}
}

// TestSaveResumeSkipsZeroByteHunt pins ENGINE-13 (report #49): the
// first-segment hunt advances currentSeq up to 20 with nothing written, and
// persisting that position made every Resume hunt 20 further — and from the
// second attempt CurrentSeq exceeded maxEvictionHuntAdvance, silencing
// diagnoseEvictedStart.
//
// Mutant: dropping the bytesWritten guard — a sidecar appears with
// LastSeq > 0 and BytesWritten 0.
func TestSaveResumeSkipsZeroByteHunt(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "video_stream")
	f, _ := os.Create(out)
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: out})
	d.outputFile = f
	d.currentSeq.Store(12) // mid-hunt
	d.bytesWritten.Store(0)

	d.saveResume()
	if _, err := os.Stat(out + ".resume.json"); err == nil {
		t.Fatal("a sidecar was written for a hunt that has produced no bytes")
	}
}
```

Imports: `errors`, `os`, `path/filepath`, `testing`.

Add to `internal/engine/connectivity_wait_test.go`:

```go
// TestCallIsOnlineSingleFlight pins ENGINE-15 (report #51): a hung IsOnline
// probe used to leak one goroutine per 5 s poll for the whole outage, and
// every live downloader polls independently. Concurrent callers now share the
// one in-flight probe.
//
// Mutant: restoring the per-call `go func()` — starts counts 5 instead of 1.
func TestCallIsOnlineSingleFlight(t *testing.T) {
	var starts atomic.Int32
	block := make(chan struct{})
	probe := func() bool {
		starts.Add(1)
		<-block
		return true
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); callIsOnline(probe) }()
	}
	wg.Wait() // every caller gives up at isOnlineProbeTimeout
	close(block)

	if n := starts.Load(); n != 1 {
		t.Fatalf("probe invocations = %d, want 1 — callIsOnline is not single-flighted", n)
	}
}
```

Add to `internal/engine/muxer_test.go`:

```go
// TestFFmpegPathArg pins ENGINE-12 (report #48): Go opens a >260-character
// path through \\?\, FFmpeg and ffprobe receive the raw string and fail on a
// Windows host without the LongPathsEnabled policy. Short paths are untouched
// so the common case carries no risk at all.
//
// Mutants, one per row:
//   - prefixing unconditionally: every ordinary path grows a \\?\ FFmpeg may
//     not parse.
//   - never prefixing: the long path reaches FFmpeg raw, which is the bug.
//   - prefixing on non-Windows: POSIX paths are corrupted.
func TestFFmpegPathArg(t *testing.T) {
	long := `C:\out\` + strings.Repeat("a", 300) + ".mp4"
	short := `C:\out\clip.mp4`

	prev := ffmpegPathOS
	t.Cleanup(func() { ffmpegPathOS = prev })

	ffmpegPathOS = "windows"
	if got := ffmpegPathArg(short); got != short {
		t.Errorf("ffmpegPathArg(short) = %q, want it unchanged", got)
	}
	if got := ffmpegPathArg(long); !strings.HasPrefix(got, `\\?\`) {
		t.Errorf("ffmpegPathArg(long) = %q, want a \\\\?\\ prefix", got[:12])
	}
	if got := ffmpegPathArg(`\\?\` + long); strings.HasPrefix(got, `\\?\\\?\`) {
		t.Error("ffmpegPathArg double-prefixed an already-prefixed path")
	}

	ffmpegPathOS = "linux"
	if got := ffmpegPathArg(long); got != long {
		t.Errorf("ffmpegPathArg(long) on linux = %q, want it unchanged", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestSaveResume|TestCallIsOnlineSingleFlight|TestFFmpegPathArg' ./internal/engine/
```

Expected: FAIL — `undefined: ffmpegPathArg`; `starts = 5`; the sidecar appears for the zero-byte hunt; no extra media Sync.

- [ ] **Step 3: fsync the media and skip the zero-byte save**

In `internal/engine/downloader_resume.go`, at the top of `saveResume`:

```go
func (d *SegmentDownloader) saveResume() {
	seq := int(d.currentSeq.Load())
	if seq <= 0 {
		return // Nothing downloaded yet — no useful state to persist.
	}
	// ENGINE-13: the first-segment hunt advances currentSeq up to 20 with
	// nothing written, and loadResume accepts a LastSeq>0/BytesWritten=0
	// sidecar — so every Resume hunted 20 further and, from the second
	// attempt, CurrentSeq exceeded maxEvictionHuntAdvance and silenced
	// diagnoseEvictedStart. A position that describes no bytes is not a
	// position.
	if d.bytesWritten.Load() == 0 {
		return
	}
	// Owner decision O-G: fsync the MEDIA before the sidecar. The sidecar is
	// written with fsync+rename and the DB's last_*_seq under FULL sync, so
	// both durable positions could lead the durable media after a power loss
	// — Start would then reject the sidecar on its size check and the DB
	// fallback would append after a torn or zero-filled (NTFS valid-data-
	// length) tail. Bounded cost at the existing cadence: ~4/min on HLS live,
	// up to ~18/min per track during catch-up. A failed Sync skips THIS save
	// rather than writing a position it cannot back: the previous sidecar
	// still points at bytes that are definitely on disk.
	if f := d.outputFile; f != nil {
		if syncErr := syncMediaFile(f); syncErr != nil {
			if !d.mediaSyncWarned {
				d.mediaSyncWarned = true
				d.logger.Warn("[Downloader] Media fsync failed; resume position not advanced",
					"file", d.opts.OutputFile, "error", syncErr)
			}
			return
		}
	}
	…unchanged marshal / tmp write / utils.ReplaceFile…
```

and beside the other seams in the same file:

```go
// syncMediaFile is os.File.Sync, swappable in tests. Production never
// reassigns it.
var syncMediaFile = (*os.File).Sync
```

Add `mediaSyncWarned bool` to the `SegmentDownloader` struct beside the other loop-goroutine-owned fields, with a comment saying `saveResume` and every media write run on the same goroutine, so no atomic is needed. (The struct lives in `internal/engine/downloader.go`, already in this task's file list.)

- [ ] **Step 4: Single-flight the connectivity probe**

In `internal/engine/connectivity_wait.go`, replace `callIsOnline` with a single-flighted version:

```go
// onlineProbe is one in-flight IsOnline call. ok is written by the probe
// goroutine strictly before done closes, so every waiter that receives from
// done sees the finished value — and because each flight owns its own struct,
// a later flight can never overwrite an earlier one's answer.
type onlineProbe struct {
	done chan struct{}
	ok   bool
}

// isOnlineInFlight is the probe currently running, if any, under isOnlineMu.
// A hung IsOnline used to leak one goroutine per 5 s poll for the whole
// outage, and every live downloader polls independently (sweep-2 ENGINE-15).
var (
	isOnlineMu       sync.Mutex
	isOnlineInFlight *onlineProbe
)

// callIsOnline invokes the caller-supplied probe with a hard timeout. If the
// probe doesn't return within isOnlineProbeTimeout, we treat it as offline
// (returns false) so the polling loop keeps trying rather than wedging on a
// hung callback. ctx is not honored inside the goroutine — the probe runs to
// completion in the background — but while one is running no second probe is
// started, so a hung callback costs ONE goroutine for the whole outage rather
// than one per poll per downloader.
func callIsOnline(isOnline func() bool) bool {
	if isOnline == nil {
		return true
	}

	isOnlineMu.Lock()
	p := isOnlineInFlight
	if p == nil {
		p = &onlineProbe{done: make(chan struct{})}
		isOnlineInFlight = p
		go func() {
			defer func() {
				// A panic inside the caller's probe must not kill the engine;
				// p.ok is already false, so the flight simply reads offline.
				if r := recover(); r != nil {
					p.ok = false
				}
				isOnlineMu.Lock()
				isOnlineInFlight = nil
				isOnlineMu.Unlock()
				close(p.done)
			}()
			p.ok = isOnline()
		}()
	}
	isOnlineMu.Unlock()

	select {
	case <-p.done:
		return p.ok
	case <-time.After(isOnlineProbeTimeout):
		return false
	}
}
```

Add the `sync` import.

- [ ] **Step 5: Prefix long paths for FFmpeg (ENGINE-12)**

In `internal/engine/muxer.go`:

```go
// ffmpegPathOS is runtime.GOOS, overridable in tests so the Windows branch of
// ffmpegPathArg is exercised on every platform. Production never reassigns it.
var ffmpegPathOS = runtime.GOOS

// ffmpegPathArg prepares a file path for FFmpeg's or ffprobe's argv. A
// composed output path — output dir + channel subdir + a 200-rune title +
// " - partN.chat.json" — can exceed Windows' 260-character MAX_PATH. Go opens
// such a path through the \\?\ extended-length prefix automatically; FFmpeg
// receives the raw string and fails on a host without the LongPathsEnabled
// policy (sweep-2 ENGINE-12). Only paths that actually need the prefix get it,
// so the ordinary case is byte-identical to before and carries no risk from
// FFmpeg's own path parsing.
func ffmpegPathArg(p string) string {
	if ffmpegPathOS != "windows" || p == "" || len(p) < 260 || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if strings.HasPrefix(abs, `\\`) {
		// A UNC path takes the \\?\UNC\ form, not \\?\\\server.
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	return `\\?\` + abs
}
```

Add the `runtime` import. Apply it to the three path arguments in `buildArgs` — each `-i <videoPath>`, `-i <audioPath>` and the trailing output path become `ffmpegPathArg(...)` — and to `filePath` in `runFFprobe`'s `exec.CommandContext` argument list (`internal/worker/orchestrator_utils.go` is NOT in the file set, so leave ffprobe alone here and record it as a residual in the arc report: the mux is the path that fails first, and the probe's failure is a Warn with a documented fallback).

- [ ] **Step 6: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
```

Expected: PASS, including `downloader_hls_resumecadence_test.go` (the 15 s floor and the `onResumeSaved` seam are untouched) and `muxer_concatcopy_test.go`. Mutant to execute: delete the `bytesWritten == 0` guard and confirm `TestSaveResumeSkipsZeroByteHunt` fails; restore.

```bash
git add internal/engine/downloader_resume.go internal/engine/downloader.go internal/engine/connectivity_wait.go internal/engine/connectivity_wait_test.go internal/engine/muxer.go internal/engine/muxer_test.go internal/engine/downloader_durability_test.go
git commit -m "fix(engine): fsync media before each resume save; three Low engine rows

Owner decision O-G (report #25) plus ENGINE-13/15/12 (reports #49/#51/#48):
saveResume fsyncs the media so the persisted position can never lead durable
bytes and skips a position that describes none, callIsOnline is single-flighted
so a hung probe leaks one goroutine instead of one per poll, and FFmpeg
receives >260-character Windows paths through the extended-length prefix.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader_resume.go internal/engine/downloader.go internal/engine/connectivity_wait.go internal/engine/connectivity_wait_test.go internal/engine/muxer.go internal/engine/muxer_test.go internal/engine/downloader_durability_test.go
```

---

## Task 11: The Low worker rows and the fast-delays seam

Report rows #52 (ENGINE-17), #53 (ENGINE-18) and #47's engine half (TOOL-6 + O-Q). O-Q: "the engine fast-delays seam is exposed across the package boundary for the worker interruption test; production values unchanged."

**Files:**
- Create: nothing
- Modify: `internal/database/database_extras.go` (`GetAllTrims`) — declared file-set deviation
- Modify: `internal/worker/orphans.go:496-516` (`scanTrimOrphans`)
- Modify: `internal/worker/stream_processor_twitch.go:618-707` (`waitForTwitchLive`)
- Modify: `internal/engine/delays.go` (`SetFastDelaysForTests`)
- Modify: `internal/worker/interruption_test.go:291-344` (`newInterruptedTestDownloader`)
- Test: `internal/worker/orphans_test.go` (extend), `internal/worker/twitch_hint_test.go` (extend)

**Interfaces:**
- Produces: `(*database.Database).GetAllTrims() ([]TrimRecord, error)`; `(*engine.SegmentDownloader).SetFastDelaysForTests(scale int)`.
- Consumes: `twitchHintCache.take` (`internal/worker/twitch_hint.go`), `engine.defaultDelays` (`internal/engine/delays.go:27`).

- [ ] **Step 1: Write the failing tests**

Add to `internal/worker/orphans_test.go`:

```go
// TestScanTrimOrphansIssuesOneQuery pins ENGINE-17 (report #52): the scan used
// one GetTrimsForJob per job (N+1) on every orphan sweep.
//
// Mutant: restoring the per-job loop — queries counts once per job instead of
// once per scan.
func TestScanTrimOrphansIssuesOneQuery(t *testing.T) {
	_, db := testWorkerSetup(t)

	for i := range 3 {
		jobID := "j" + strconv.Itoa(i)
		if err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, Status: database.StatusFinished}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		if err := db.AddTrim(&database.TrimRecord{
			ID: jobID + "-t", JobID: jobID, StartTime: 0, EndTime: 10,
			Filename: jobID + "/trim/clip.mp4", CreatedAt: "2026-09-17T00:00:00Z", Duration: 10,
		}); err != nil {
			t.Fatalf("AddTrim: %v", err)
		}
	}

	trims, err := db.GetAllTrims()
	if err != nil {
		t.Fatalf("GetAllTrims: %v", err)
	}
	if len(trims) != 3 {
		t.Fatalf("GetAllTrims returned %d records, want 3 — the scan would miss a referenced trim and offer it for deletion", len(trims))
	}
	seen := map[string]bool{}
	for _, tr := range trims {
		seen[tr.JobID] = true
	}
	for i := range 3 {
		if !seen["j"+strconv.Itoa(i)] {
			t.Errorf("GetAllTrims omitted job j%d's trim", i)
		}
	}
}
```

Imports: `strconv`, `testing`, `internal/database`. `AddTrim` is `func (db *Database) AddTrim(trim *TrimRecord) error` (`internal/database/database_jobs.go:479`). Step 6's `grep` proves `scanTrimOrphans` no longer calls `GetTrimsForJob`.

Add to `internal/worker/twitch_hint_test.go`:

```go
// TestWaitForTwitchLiveConsumesHint pins ENGINE-18 (report #53): a manually
// added offline channel polled GQL every 15-20 s while the monitor
// batch-polled the same channel and stashed a hint for the very same job —
// two GQL streams for one answer.
//
// Mutant: dropping the take() from the wait loop — the stash is never
// consumed and the poll still pays a GetStreamInfo round trip.
func TestWaitForTwitchLiveConsumesHint(t *testing.T) {
	c := newTwitchHintCache()
	c.stash("job1", &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"})

	got := takeLiveHint(c, "job1")
	if got == nil || got.StreamID != "s1" {
		t.Fatalf("takeLiveHint = %v, want the stashed live info", got)
	}
	if again := takeLiveHint(c, "job1"); again != nil {
		t.Error("takeLiveHint returned the same hint twice — take-once semantics are broken")
	}
	c.stash("job2", &twitch.TwitchStreamInfo{IsLive: false})
	if got := takeLiveHint(c, "job2"); got != nil {
		t.Error("takeLiveHint returned a non-live hint — the wait must keep polling")
	}
}
```

Add to `internal/engine/delays_test.go`:

```go
// TestSetFastDelaysForTests pins O-Q's exported seam: internal/worker's
// interruption test pays the production retry ladder because the delays seam
// is unexported and unreachable from there (TOOL-6, 10.1 s of the suite).
//
// Mutant: making the setter a no-op — the scaled values equal the production
// ones and the boundary test regains its ten seconds.
func TestSetFastDelaysForTests(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{})
	d.SetFastDelaysForTests(fastScale)
	if d.delays != fastDelays() {
		t.Fatalf("delays = %+v, want fastDelays() = %+v", d.delays, fastDelays())
	}
	if d.delays == defaultDelays() {
		t.Fatal("SetFastDelaysForTests left the production values in place")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestScanTrimOrphansIssuesOneQuery|TestWaitForTwitchLiveConsumesHint|TestSetFastDelaysForTests' ./internal/worker/ ./internal/engine/
```

Expected: FAIL — `undefined: GetAllTrims`, `undefined: takeLiveHint`, `undefined: SetFastDelaysForTests`.

- [ ] **Step 3: One query for the trim scan**

In `internal/database/database_extras.go`:

```go
// GetAllTrims returns every trim record in the database, ordered by job. The
// orphan scanner needs the complete set to decide which files on disk are
// referenced, and asking per job made that an N+1 query on every sweep
// (sweep-2 ENGINE-17).
func (db *Database) GetAllTrims() ([]TrimRecord, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(), `SELECT id, job_id, start_time, end_time, filename, created_at, duration, file_size
		FROM trims ORDER BY job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trims []TrimRecord
	for rows.Next() {
		var tr TrimRecord
		if err := rows.Scan(&tr.ID, &tr.JobID, &tr.StartTime, &tr.EndTime,
			&tr.Filename, &tr.CreatedAt, &tr.Duration, &tr.FileSize); err != nil {
			if db.logger != nil {
				db.logger.Warn("GetAllTrims: scan error", "err", err)
			}
			continue
		}
		trims = append(trims, tr)
	}
	return trims, rows.Err()
}
```

In `internal/worker/orphans.go`, replace the `jobs, err := db.GetAllJobs()` block and the per-job loop with:

```go
	// One query, not one per job (sweep-2 ENGINE-17).
	trims, err := db.GetAllTrims()
	if err != nil {
		return nil, err
	}

	knownTrimFiles := make(map[string]bool, len(trims))
	for _, tr := range trims {
		// Resolve trim path: relative to output dir
		trimAbs := tr.Filename
		if !filepath.IsAbs(trimAbs) {
			trimAbs = filepath.Join(absOutputDir, trimAbs)
		}
		knownTrimFiles[normalizePath(trimAbs)] = true
	}
```

- [ ] **Step 4: Let the manual Twitch wait read the hint cache**

In `internal/worker/stream_processor_twitch.go`, above `waitForTwitchLive`:

```go
// takeLiveHint consumes a monitor-stashed TwitchStreamInfo for jobID when it
// says the channel is live. The Twitch monitor batch-polls every configured
// channel and stashes its result for the job it belongs to, so a manually
// added job waiting on a channel the monitor also watches had two GQL streams
// answering one question (sweep-2 ENGINE-18). Take-once, so a hint consumed
// here is not re-read by processTwitchLive.
func takeLiveHint(c *twitchHintCache, jobID string) *twitch.TwitchStreamInfo {
	if c == nil {
		return nil
	}
	info := c.take(jobID)
	if info == nil || !info.IsLive {
		return nil
	}
	return info
}
```

and inside the poll loop, immediately before the offline-oracle floor check:

```go
		if hint := takeLiveHint(sp.twitchHints, job.ID); hint != nil {
			sp.logger.Info("twitch channel is now live (monitor hint)", "channel", login)
			sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
			return hint, nil
		}
```

- [ ] **Step 5: Expose the fast-delays seam (O-Q)**

In `internal/engine/delays.go`:

```go
// SetFastDelaysForTests divides every retry and backoff wait by scale. It
// exists for tests in OTHER packages: internal/worker's
// TestFinalizeIncompleteTailInterruption drives a real SegmentDownloader
// through a Tier-2 finalize and paid the production retry ladder — 10.1 s,
// a quarter of the whole suite's wall time — because this struct is
// unexported and its in-package fastDelays() helper unreachable from there
// (sweep-2 TOOL-6, owner decision O-Q).
//
// Production never calls this: NewSegmentDownloader installs defaultDelays()
// and nothing else writes the field, so the shipped timings are unchanged.
// A scale of 0 or less is ignored.
func (d *SegmentDownloader) SetFastDelaysForTests(scale int) {
	if scale <= 0 {
		return
	}
	n := time.Duration(scale)
	p := defaultDelays()
	d.delays = delays{
		singleGoneRetry:        p.singleGoneRetry / n,
		interruptionStallRetry: p.interruptionStallRetry / n,
		transientFailureRetry:  p.transientFailureRetry / n,
		genericRetry:           p.genericRetry / n,
		hlsPlaylistRetry:       p.hlsPlaylistRetry / n,
		hlsStuckRetry:          p.hlsStuckRetry / n,
		connectivityPoll:       p.connectivityPoll / n,
		atEdgeBackoffUnit:      p.atEdgeBackoffUnit / n,
		hlsReloadUnit:          p.hlsReloadUnit / n,
		hlsResumeSave:          p.hlsResumeSave / n,
	}
}
```

In `internal/worker/interruption_test.go`'s `newInterruptedTestDownloader`, immediately after `NewSegmentDownloader(...)`:

```go
	// O-Q: run the production retry ladder at 1/20 scale. Without this the
	// test waits out singleGoneRetryDelay's real 500 ms x the 403 attempt
	// budget, which is 10 s of the suite's wall time for one boundary.
	d.SetFastDelaysForTests(20)
```

and drop the `20 * time.Second` guard in the same helper to `5 * time.Second`, since the ladder no longer takes ten.

- [ ] **Step 6: Run the tests, verify a mutant, gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/worker/ ./internal/database/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFinalizeIncompleteTailInterruption -v ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/ ./internal/worker/ ./internal/database/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./internal/engine/ ./internal/worker/ ./internal/database/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```

Expected: PASS, with `TestFinalizeIncompleteTailInterruption`'s reported time well under its previous ~10 s. Record the before/after in the review package. `grep -n "GetTrimsForJob" internal/worker/orphans.go` must return nothing. `GetAllTrims`, `takeLiveHint` and `SetFastDelaysForTests` are new Go symbols, so the citation gate runs.

```bash
git add internal/database/database_extras.go internal/worker/orphans.go internal/worker/orphans_test.go internal/worker/stream_processor_twitch.go internal/worker/twitch_hint_test.go internal/engine/delays.go internal/engine/delays_test.go internal/worker/interruption_test.go
git commit -m "perf(worker,engine): one trim query, a hint-fed Twitch wait, a cross-package delays seam

Reports #52/#53 and #47's engine half (owner decision O-Q): the orphan scan
asks for every trim once instead of once per job, a manually added Twitch job
waiting on a monitored channel consumes the monitor's stashed hint instead of
polling GQL in parallel with it, and SegmentDownloader exposes a test-only
fast-delays setter so the worker's interruption boundary stops paying the
production retry ladder. Production timings are unchanged.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/database/database_extras.go internal/worker/orphans.go internal/worker/orphans_test.go internal/worker/stream_processor_twitch.go internal/worker/twitch_hint_test.go internal/engine/delays.go internal/engine/delays_test.go internal/worker/interruption_test.go
```

---

## Task 12: ENGINE-16's three false sentences, and the plan deletes itself

Report row #102 (ENGINE-16): three statements are false — the HLS VOD buffer is "bounded by the in-flight window", "Mux is idempotent — partial output is overwritten" on restart, and the unknown-verdict exit's "sidecar survives for a later Resume" on Twitch. The first and third live in code comments and were corrected by Tasks 6 and 5; this task corrects `docs/spec/architecture.md` and adds the VOD buffer's ceiling to the doc, which never described it at all.

**Files:**
- Modify: `docs/spec/architecture.md:411` (the HLS end-verdict bullet), the HLS mode bullet list (a new VOD-buffer line), `:719-723` (the Muxing-not-terminal note)
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-e-engine.md`

**Interfaces:**
- Consumes: `muxFromStaging` (declared in `internal/worker/orchestrator_mux.go`), `enqueueExistingJobs` (declared in `internal/worker/worker.go`), `ErrStagedMediaPresent` (declared in `internal/engine/downloader.go`), `runHlsVodParallel` and `hlsVodBufferBytes` (declared in `internal/engine/downloader_hls.go`). The citation test requires each cited symbol's DECLARING file, so every pairing above is exact.

- [ ] **Step 1: Run the docs gate to confirm it is green before the edit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```

Expected: PASS. This is the baseline the edit must preserve; the citation test is this task's whole verification, so record the output.

- [ ] **Step 2: Correct the Muxing-not-terminal note**

Replace `docs/spec/architecture.md:719-723` with:

```markdown
Note: `Muxing` is **not** terminal. If muxing was interrupted by shutdown (the
muxer process was killed mid-encode), `enqueueExistingJobs` in
`internal/worker/worker.go` re-muxes the job from what is already staged —
the Mux action's own path, `muxFromStaging` in
`internal/worker/orchestrator_mux.go` — rather than re-processing it. A row
flagged `incomplete_tail` is the exception: it resets to `Downloading` so the
post-live VOD-refresh loop can still fetch the tail the live capture missed.

Muxing is NOT idempotent against the recording itself, which is what the
previous reset assumed: re-processing re-probed the stream as post-live, the
manifest-free strategy seeded sequence 0, and the engine truncated the
complete staged file. The engine now refuses that outright —
`ErrStagedMediaPresent` in `internal/engine/downloader.go` — so no path
truncates non-empty staged media unless the caller explicitly asked to
discard it.
```

- [ ] **Step 3: Correct the end-verdict bullet's last clause**

In the HLS live mode bullet at `docs/spec/architecture.md:411`, the sentence "so they exit with their fetch/parse failure and leave `streamEnded` unset, keeping the resume sidecar for a later Resume" is true on YouTube and was false on Twitch. Replace it with:

```markdown
so they exit with their fetch/parse failure and leave `streamEnded` unset. On
YouTube the orchestrator finalizes what was captured and the resume sidecar
survives for a later Resume. On Twitch that exit used to finalize the job
Finished, after which the staging dir — and the sidecar in it — was deleted;
`ExecuteTwitch` in `internal/worker/orchestrator_twitch.go` now re-verifies
the broadcast once and, absent a confirmed end, returns the download error so
the job lands in Error with its staging intact. The verdict itself comes from
two `GetStreamInfo` samples ~5 s apart, taken in the worker's `CheckStreamFn`
closure (`internal/worker/worker.go`), so one transient StreamMetadata flap
can no longer end a live recording at any of the six consult sites.
```

- [ ] **Step 4: Document the VOD buffer's ceiling**

Add one bullet to the HLS mode list, immediately after the "no parallel catch-up path" line:

```markdown
- VOD mode (`runHlsVodParallel` in `internal/engine/downloader_hls.go`): a
  fixed worker pool fetches the whole playlist in parallel and a byte-bounded
  reorder buffer writes the segments in ascending order as they land. The
  ceiling (`hlsVodBufferBytes` in `internal/engine/downloader_hls.go`, the
  same 256 MB the DASH catch-up path uses) is what stops the other workers
  holding the rest of the VOD in RAM while the head-of-order segment works
  through its retry ladder; workers wait for room instead. Failed segments
  become nil gap sentinels so the consumer never wedges on a missing index.
```

- [ ] **Step 5: Verify the citation gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```

Expected: PASS. If a pairing is rejected, the failure names the file that actually declares the symbol — fix the citation, never the allowlist.

- [ ] **Step 6: Delete the plan and commit**

```bash
git rm docs/superpowers/plans/2026-09-17-sweep2-e-engine.md
git add docs/spec/architecture.md
git commit -m "docs(architecture): correct ENGINE-16's three false sentences; delete the Arc E plan

Report #102 (ENGINE-16): the Muxing-not-terminal note now describes the
mux-from-staging restart and the no-truncate guard, the HLS end-verdict bullet
distinguishes YouTube's sidecar survival from Twitch's Error exit and names
the two-sample confirmation, and the VOD reorder buffer's byte ceiling is
documented for the first time.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/spec/architecture.md docs/superpowers/plans/2026-09-17-sweep2-e-engine.md
```

---

## Arc close (controller)

1. Merge `main` into `sweep2-e-engine`.
2. Merge-candidate gates in full (see Global Constraints → Arc E specifics). Arc E has **no live gate**.
3. Fable arc-completion review over `merge-base..HEAD`, then one fix wave and a scoped re-review.
4. Merge `--no-ff` into `main` without asking; post-merge gates; delete the worktree **and** the branch.
5. Ledger entries plus the residuals below.

### Residuals to carry into the arc report

| Residual | Owner |
|---|---|
| TOOL-2's doc-comment half: `utils.ReplaceFile`'s claim to be "the last step of every atomic write in Moombox" is still false. | Arc X (`internal/utils/**`) |
| ENGINE-12's ffprobe half: `runFFprobe` (`internal/worker/orchestrator_utils.go:51-57`) still passes the raw path. The mux fails first and the probe's failure is a Warn with a documented fallback, but the fix is one `ffmpegPathArg` call. | Owner report |
| `cmd/moombox/adapters.go:125` (`FetchStreamMetadata`) now surfaces a transient StreamMetadata slot failure as an error instead of "offline" — a 500 on a manual Twitch add during a GQL flap, where the user previously saw "not live". Arguably the truthful answer; not changed here because `cmd/moombox` is Arc C's. | Owner report |
| O-F's residual risk: with the lifecycle cap moved to the download decision, 100 concurrent downloads would block the 101st — including a live one. No realistic install approaches it, and the alternative (the pre-arc gate) blocked live streams at 100 *waiters*, which field installs do reach. | Owner report |

