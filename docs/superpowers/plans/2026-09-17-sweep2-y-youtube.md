# Arc Y — YouTube extraction + YouTube live chat + BotGuard sidecar — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the sixteen verified sweep-2 findings that live in YouTube extraction, YouTube live chat and the BotGuard sidecar — a substituted-video acceptance, DRM/DRC/dubbed-track blindness, two unmapped age-gate shapes, a sidecar that never restarts, two per-poll request-fan-out wastes, a zero-delay chat recovery loop, and seven Low-tier rows — without changing any protected behaviour.

**Architecture:** Three independent surfaces meet in one arc because they share `cmd/moombox/services.go`'s sidecar hunk and the `internal/youtube` package: (1) the Innertube cascade in `internal/youtube/player_api_{parsing,strategy}.go` gains a video-ID identity check, upstream's `(itag, audioTrackID, isDrc)` format identity, upstream's age-gate reason match, and two request-shedding rules; (2) `internal/bgutils/sidecar` gains an in-place `Restart` plus a `Supervisor` that drives it on a backoff ladder, with a package-level health snapshot both UIs read; (3) `internal/chat/downloader.go` gains a floor under repeated stale-continuation recoveries and honours the watch page's `isReplay` flip.

**Tech Stack:** Go 1.27 (no CGo, pure-Go deps), `modernc/sqlite`, `chi/v5`, Charm bubbletea/lipgloss for the TUI, vanilla ES modules + Shoelace v2.16 for the dashboard, Node `--test` with jsdom for the front-end suite. Upstream reference for every extraction change: `references/yt-dlp/yt_dlp/extractor/youtube/_video.py` and `_base.py` (pinned at `bbc809a11`).

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` (§0 decisions O-H, O-I, O-R and "Minter age = no change"; §2 constraints; §3 "Arc Y"; §4 order; §5 rulings). Evidence rows: `reports/sweep-2026-09-15b.md` #4, #5, #9, #12, #13, #26, #27, #54–#60, #102; area reports `reports/sweep-2026-09-15b/youtube.md` and `twitch.md`; verifier reports `reports/sweep-2026-09-15b/_verify-youtube.md` and `_verify-engine-twitch.md` (**the verifiers' corrections override the area reports wherever they differ**).

---

## Global Constraints

Copied verbatim from spec §2 — every task's requirements implicitly include this section.

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

### Arc-Y additions

- **Branch / worktree.** `git worktree add -b sweep2-y-youtube .worktrees/sweep2-y-youtube main`, then copy `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and `internal/cipher/testdata/*.js` from the main checkout, then `cd web/tests && npm ci --no-audit --no-fund`.
- **Standing rulings carried in** (spec §2 memory-level list + `.superpowers/sdd/2026-09-15-sweep-3-youtube/progress.md`): android_vr stays in the roster; VISIONOS live results are split-adaptive and need no DASH manifest; the homepage ytcfg+ytAtN minting pair stays; **no candidate cap** on the `ytInitialData` / `ytInitialPlayerResponse` candidate iterator; `WatchPageResult.AttestationChallenge` **stays** (only its doc comment is wrong); the cookie-jar `(size, mtime)` memo is untouched by this arc. **Minter age: no code change** — regenerating the BotGuard minter on every 403 refresh is kept.
- **Cross-arc file call-outs.** Spec §3 allows same-file edits across arcs when they are in different functions and are called out. This arc touches five files outside its own §3 list. Each is one hunk, in a function no other arc's rows name:
  | File | Hunk | Other arc that owns the file | Why this arc must touch it |
  |---|---|---|---|
  | `internal/web/routes/jobs.go` | `StatusRoute` only (the `resp` map) | Arc W (wave 2); Arc C owns `:86-134` | Spec §5: "YOUTUBE-5's fix must give a user-visible signal (dashboard + TUI)". `/api/status` is the dashboard's existing health payload. |
  | `web/public/app.js` | `loadStatus` + the `warningItems` list in `updateStatusBar` | Arc W (wave 2) | Same ruling; `#status-warnings` already exists in `index.html`, so no HTML change. |
  | `cmd/moombox/tui_wiring.go` | one `SubscribeHealth` registration beside `unsubConnTUI` | Arc C (wave 2) | Same ruling; the TUI never imports `bgutils`, so the flag travels as a plain `bool` through the existing `app.Send` callback shape (`ConnectivityMsg`'s). |
  | `internal/tui/{app.go, app_update.go, status_bar.go}` | one msg type, one `case`, one alert chip | Arc C (wave 2) | Same ruling. |
  | `internal/worker/orchestrator_chat.go` | `setupChatDownloader` only | Arc E (wave 1) — owns `resolveChatOutcome`'s VOD branch | Row #56 (YOUTUBE-10) names `orchestrator_chat.go:27` as one of the two chat-setup sites. |
  | `docs/spec/architecture.md` | the `runLiveStreamDownload` bullet at `:367` | Arc E (ENGINE-16 sentences), Arc M (§10) | O-H changes exactly what that bullet describes. |
  Wave-2 arcs merge `main` before their merge candidates (spec §4), so these land first. Arc E is wave 1 and parallel: the two `orchestrator_chat.go` hunks and the two `architecture.md` paragraphs are disjoint; if git still conflicts, take both sides.
- **Live gates (controller, at the merge candidate):**
  ```bash
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_YT_TEST=1 \
    go test -count=1 -run 'TestLivePublicExtraction|TestLiveLoginMarkersPresent' ./internal/youtube/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_CIPHER_TEST=1 \
    go test -count=1 -timeout 180s -run TestSidecarSolver ./internal/cipher/
  ```
- **Per-task gates** (run in the worktree, one package at a time, never `./...`):
  ```bash
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/chat/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/bgutils/... ./internal/cipher/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/constants/ ./internal/utils/ ./internal/docs/
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
  ```
- **Commit shape, every task:**
  ```bash
  git add <files> && git commit -m "<msg>" -- <same files>
  ```
  with the two trailer lines LAST, exactly as written in Global Constraints. They take precedence over any attribution reminder in the implementer's own context, whatever model name that reminder shows.

---

## File Structure

| File | Responsibility after this arc |
|---|---|
| `internal/bgutils/sidecar/sidecar.go` | Child-process lifecycle. **New:** `Config.OnUnhealthy`, `Sidecar.Restart` (in-place), a nil-stdin guard in `writeRequest`. |
| `internal/bgutils/sidecar/supervisor.go` *(new)* | `Supervisor` — the backoff-ladder restart loop and its `OnUp` re-wiring hook. |
| `internal/bgutils/sidecar/health.go` *(new)* | `Health` snapshot + `PublishHealth` / `CurrentHealth` / `SubscribeHealth`: one package-level store both UIs read, mirroring `routes.SharedDiskStatus`. |
| `internal/cipher/solver_composite.go` | Routing policy. **New:** `SidecarSwappable` + `compositeSolver.SetSidecar`, so a restart installs a fresh sidecar solver. |
| `cmd/moombox/services.go` | Sidecar hunk (§7b) + the cipher hunk (§8) construct, wire and run the supervisor. |
| `internal/youtube/player_api_parsing.go` | `parsePlayerResponse` (now video-ID checked), `parseFormats` (DRM/DRC/audioTrack + diagnostics), `deduplicateFormats` (upstream's 3-part key), `parsePlayabilityStatus` (age-gate reasons). |
| `internal/youtube/player_api_strategy.go` | The two cascades: mismatch tally + `finishExtraction`, O-I short-circuit and android_vr reuse, O-R probe variant, YOUTUBE-12 mirror, Innertube error bodies, the `fetchWatchPage` test seam. |
| `internal/youtube/types.go` | `Format` (track/DRC fields), `VideoInfo` (`FormatDiag`, `ChatSource`). |
| `internal/youtube/format_selector.go` | Audio track preference (original > default > unlabelled > descriptive; non-DRC wins the twin). |
| `internal/youtube/watch_page.go` | `bpctr`/`has_verified` query params; the corrected `AttestationChallenge` doc comment; single-pass `ytInitialData` acceptance. |
| `internal/youtube/channel_membership.go` | `extractYtInitialData` becomes decode-as-acceptance. |
| `internal/youtube/browse.go` | Browse HTTP errors carry YouTube's own message. |
| `internal/utils/jsoncandidates.go` | `IsNonEmptyJSONBody` (the non-empty half, without the extra `json.Valid` scan). |
| `internal/constants/constants.go` | Client roster minus `IOSClient` / `WebRemixClient`. |
| `internal/chat/downloader.go` | Stale-continuation floor, consecutive-recovery cap, live↔replay flip. |
| `internal/worker/orchestrator_youtube.go` | O-H quality probe (`probeVideoInfo` + its narrow client interface). |
| `internal/worker/orchestrator_chat.go` | `setupChatDownloader` reuses the carried chat source. |
| `internal/worker/stream_processor_youtube.go` | `tryStartEarlyChat` reuses the carried chat source. |
| `internal/web/routes/jobs.go` | `StatusRoute` publishes `botguardSidecar`. |
| `web/public/app.js`, `internal/tui/{app,app_update,status_bar}.go`, `cmd/moombox/tui_wiring.go` | The sidecar-down alert in both UIs. |
| `docs/spec/platform-services.md`, `docs/spec/architecture.md` | Prose brought back in line with the code. |

---

## Task 1: BotGuard sidecar supervisor

Row #5 (YOUTUBE-5, raised to H). The sidecar is started once and never restarted; after a crash or a V8 OOM-abort `markUnhealthy` latches for the rest of a 24/7 run, and because `compositeSolver.Sig` is sidecar-ONLY every signature-ciphered format becomes unresolvable while PO tokens fall to the goja path, which rejects the websafe-only response.

**Files:**
- Modify: `internal/bgutils/sidecar/sidecar.go` (`Config`, `markUnhealthy`, `writeRequest`, new `Restart`)
- Create: `internal/bgutils/sidecar/health.go`
- Create: `internal/bgutils/sidecar/supervisor.go`
- Create: `internal/bgutils/sidecar/supervisor_test.go`
- Modify: `internal/cipher/solver_composite.go`
- Modify: `internal/cipher/solver_composite_test.go`
- Modify: `cmd/moombox/services.go` (§7b sidecar hunk + the §8 cipher wiring hunk)

**Interfaces:**
- Produces: `sidecar.Config.OnUnhealthy func(reason string)`; `func (s *Sidecar) Restart(ctx context.Context) error`; `type Health struct { Healthy bool; Reason string; Restarts uint64; Since time.Time }`; `func PublishHealth(h Health)`; `func CurrentHealth() (Health, bool)`; `type SupervisorConfig struct { Restart func(context.Context) error; StartTimeout time.Duration; Backoff []time.Duration; Logger Logger }`; `func NewSupervisor(cfg SupervisorConfig) *Supervisor`; `func (s *Supervisor) Notify(reason string)`; `func (s *Supervisor) SetOnUp(fn func())`; `func (s *Supervisor) Run(ctx context.Context)`; `var DefaultSupervisorBackoff []time.Duration`; `cipher.SidecarSwappable` (a `Solver` with `SetSidecar(Solver)`), returned by `cipher.NewCompositeSolver`.
- Consumes: nothing from earlier tasks (this is the first task).

- [ ] **Step 1: Write the failing composite-solver test**

Append to `internal/cipher/solver_composite_test.go`:

```go
// TestCompositeSetSidecarSwapsTheSigPath pins the seam the BotGuard sidecar
// supervisor needs. sig is sidecar-ONLY (the routing policy), so a process
// whose sidecar died and came back must be able to install the REBUILT
// sidecar solver — the old one's per-player "already sent" map describes the
// dead child's memory. Before this, the sidecar half was captured at
// construction and a restarted sidecar was unreachable for the rest of the run.
//
// Mutants this kills:
//   - SetSidecar implemented as a no-op          → the first Sig still errors
//   - Sig reading a value snapshotted in the ctor → the first Sig still errors
//   - SetSidecar(nil) not clearing the slot       → the last Sig returns "S"
func TestCompositeSetSidecarSwapsTheSigPath(t *testing.T) {
	goja := &fakeSolver{sig: "goja-should-never-serve-sig"}
	c := NewCompositeSolver(nil, goja)

	if _, err := c.Sig(context.Background(), "p1", "x"); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Sig with no sidecar: err = %v, want ErrSidecarUnavailable", err)
	}

	c.SetSidecar(&fakeSolver{sig: "S"})
	got, err := c.Sig(context.Background(), "p1", "x")
	if err != nil || got != "S" {
		t.Fatalf("Sig after SetSidecar = (%q, %v), want (\"S\", nil)", got, err)
	}

	c.SetSidecar(nil)
	if _, err := c.Sig(context.Background(), "p1", "x"); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Sig after SetSidecar(nil): err = %v, want ErrSidecarUnavailable", err)
	}
}
```

Read the top of the existing file first and reuse its fake-solver type and helpers rather than adding new ones; the type used above is named `fakeSolver` with a `sig` field — if the file's fake has a different name or shape, use that one and keep the assertions identical. Add `"context"` and `"errors"` to the test file's imports if they are not already there.

- [ ] **Step 2: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestCompositeSetSidecarSwapsTheSigPath ./internal/cipher/
```
Expected: FAIL to compile — `c.SetSidecar undefined (type Solver has no field or method SetSidecar)`.

- [ ] **Step 3: Make the composite's sidecar half swappable**

In `internal/cipher/solver_composite.go`, add `"sync"` to the imports, then replace the type, constructor and the three methods' sidecar reads:

```go
// SidecarSwappable is the composite solver's own interface: a Solver whose
// sidecar half can be replaced while the process runs. The BotGuard sidecar
// supervisor installs a freshly BUILT sidecar solver after every restart —
// the old one carries per-player "already sent" state that describes the dead
// child's memory, so it must be discarded rather than reused.
type SidecarSwappable interface {
	Solver
	SetSidecar(sidecar Solver)
}

type compositeSolver struct {
	// mu guards sidecarSolver only. goja is fixed at construction.
	mu            sync.RWMutex
	sidecarSolver Solver
	goja          Solver
}

// NewCompositeSolver wraps two underlying solvers with the routing
// policy. Pass nil for sidecar when the BotGuard sidecar is disabled
// or failed to start — the supervisor can install one later.
func NewCompositeSolver(sidecar, goja Solver) SidecarSwappable {
	return newCompositeSolverWith(sidecar, goja)
}

func newCompositeSolverWith(sidecar, goja Solver) *compositeSolver {
	return &compositeSolver{sidecarSolver: sidecar, goja: goja}
}

// SetSidecar installs (or, with nil, removes) the sidecar half. Safe to call
// while other goroutines are solving.
func (c *compositeSolver) SetSidecar(sidecar Solver) {
	c.mu.Lock()
	c.sidecarSolver = sidecar
	c.mu.Unlock()
}

func (c *compositeSolver) sidecar() Solver {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sidecarSolver
}
```

Then rewrite the three method bodies to take ONE snapshot each (so a swap mid-call cannot make a method use two different solvers):

```go
func (c *compositeSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	sc := c.sidecar()
	if sc == nil {
		return "", ErrSidecarUnavailable
	}
	return sc.Sig(ctx, playerID, encryptedSig)
}

func (c *compositeSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	if sc := c.sidecar(); sc != nil {
		out, err := sc.N(ctx, playerID, encryptedN)
		if err == nil {
			return out, nil
		}
		// fall through to goja
	}
	if c.goja == nil {
		return "", errors.New("cipher: no solver available for n")
	}
	return c.goja.N(ctx, playerID, encryptedN)
}
```

and in `Batch`, replace `if c.sidecar != nil {` / `} else if len(sigs) > 0 {` with a snapshot taken once at the top:

```go
	sc := c.sidecar()
	if sc != nil {
		sr, nr, err := sc.Batch(ctx, playerID, sigs, ns)
		if err == nil {
			// sidecarSolver.Batch already validated completeness; trust it.
			return sr, nr, nil
		}
		// Sidecar failed. Sig is sidecar-only; if any sigs were
		// requested, surface the error. n falls back to goja per-element.
		if len(sigs) > 0 {
			return nil, nil, err
		}
	} else if len(sigs) > 0 {
		// No sidecar configured but sig requested.
		return nil, nil, ErrSidecarUnavailable
	}
```

Leave `var _ Solver = (*compositeSolver)(nil)` and add `var _ SidecarSwappable = (*compositeSolver)(nil)` beside it.

- [ ] **Step 4: Run the cipher package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cipher/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race -run TestComposite ./internal/cipher/
```
Expected: PASS, no data race.

- [ ] **Step 5: Write the failing supervisor + health tests**

Create `internal/bgutils/sidecar/supervisor_test.go`:

```go
package sidecar

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger satisfies the package's Logger interface without writing anywhere.
type testLogger struct{}

func (testLogger) Debug(string, ...any) {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Error(string, ...any) {}

// resetHealth clears the package-level snapshot between tests. These tests
// share process state on purpose (so does production: one sidecar per
// process), so none of them calls t.Parallel().
func resetHealth(t *testing.T) {
	t.Helper()
	sharedHealth.Store(nil)
	t.Cleanup(func() { sharedHealth.Store(nil) })
}

// TestSupervisorRestartsOnTheBackoffLadder is the whole point of the row: a
// sidecar that died must come back, and the process must re-wire the two
// consumers that hold a reference to it.
//
// Mutants this kills:
//   - the restart loop giving up after one failure   → restarts == 0
//   - a fixed retry delay instead of the ladder      → delays != [5s 15s 60s]
//   - OnUp never called                              → onUp == 0
//   - health never republished as healthy            → CurrentHealth().Healthy false
func TestSupervisorRestartsOnTheBackoffLadder(t *testing.T) {
	resetHealth(t)

	var attempts atomic.Int64
	var delays []time.Duration
	var onUp atomic.Int64

	sup := NewSupervisor(SupervisorConfig{
		Logger: testLogger{},
		Restart: func(context.Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("node exited 1")
			}
			return nil
		},
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }
	sup.SetOnUp(func() { onUp.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	sup.Notify("stdout EOF")

	deadline := time.After(5 * time.Second)
	for onUp.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("supervisor never completed a restart (attempts=%d)", attempts.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	if got := attempts.Load(); got != 3 {
		t.Errorf("Restart attempts = %d, want 3", got)
	}
	want := []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
	h, ok := CurrentHealth()
	if !ok || !h.Healthy || h.Restarts != 1 {
		t.Errorf("CurrentHealth() = (%+v, %v), want a healthy snapshot with Restarts 1", h, ok)
	}
}

// TestSupervisorLadderRepeatsItsLastStep: the ladder is four entries and a
// 24/7 archiver must keep trying past them.
//
// Mutants this kills:
//   - indexing the ladder without clamping  → panic (index out of range)
//   - wrapping back to the first entry      → delays[4] == 5s, not 5m
func TestSupervisorLadderRepeatsItsLastStep(t *testing.T) {
	resetHealth(t)

	var attempts atomic.Int64
	var delays []time.Duration
	sup := NewSupervisor(SupervisorConfig{
		Logger: testLogger{},
		Restart: func(context.Context) error {
			if attempts.Add(1) < 6 {
				return errors.New("still dead")
			}
			return nil
		},
	})
	sup.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("readPump panic")

	deadline := time.After(5 * time.Second)
	for attempts.Load() < 6 {
		select {
		case <-deadline:
			t.Fatalf("supervisor stalled at %d attempts", attempts.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	if len(delays) < 6 {
		t.Fatalf("delays = %v, want at least 6 entries", delays)
	}
	for i := 3; i < 6; i++ {
		if delays[i] != 5*time.Minute {
			t.Errorf("delays[%d] = %v, want the repeated ceiling 5m", i, delays[i])
		}
	}
}

// TestSupervisorNotifyNeverBlocks: Notify runs on readPump, the goroutine that
// also drains the sidecar's stdout. A blocking send there would wedge the pump
// and with it every pending RPC.
//
// Mutant this kills: an unbuffered notices channel (or a blocking send) →
// the second Notify never returns and the test times out.
func TestSupervisorNotifyNeverBlocks(t *testing.T) {
	resetHealth(t)

	sup := NewSupervisor(SupervisorConfig{Logger: testLogger{}, Restart: func(context.Context) error { return nil }})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			sup.Notify("stdout EOF")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked with no Run goroutine draining the channel")
	}
}

// TestSupervisorStopsWhenTheProcessShutsDown: Run must return on ctx.Done even
// while the restart ladder is sleeping, or shutdown waits on it.
//
// Mutant this kills: restartLoop ignoring ctx.Err() after the sleep →
// Run never returns and the test times out.
func TestSupervisorStopsWhenTheProcessShutsDown(t *testing.T) {
	resetHealth(t)

	ctx, cancel := context.WithCancel(context.Background())
	sup := NewSupervisor(SupervisorConfig{
		Logger:  testLogger{},
		Restart: func(context.Context) error { return errors.New("dead") },
	})
	sup.sleep = func(c context.Context, _ time.Duration) { cancel() }

	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	sup.Notify("stdout EOF")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestCurrentHealthIsEmptyUntilPublished: "no snapshot" is how a
// sidecar-disabled process looks to both UIs, and it must NOT read as
// "unhealthy" — that would put a permanent red alert on a correct config.
//
// Mutant this kills: CurrentHealth returning (Health{}, true) for a nil
// pointer → ok is true and both UIs would draw the alert.
func TestCurrentHealthIsEmptyUntilPublished(t *testing.T) {
	resetHealth(t)

	if h, ok := CurrentHealth(); ok {
		t.Fatalf("CurrentHealth() before any publish = (%+v, true), want ok=false", h)
	}
	PublishHealth(Health{Healthy: true, Restarts: 2})
	h, ok := CurrentHealth()
	if !ok || !h.Healthy || h.Restarts != 2 {
		t.Fatalf("CurrentHealth() = (%+v, %v), want the published snapshot", h, ok)
	}
}
```

- [ ] **Step 6: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestSupervisor|TestCurrentHealth' ./internal/bgutils/sidecar/
```
Expected: FAIL to compile — `undefined: NewSupervisor`, `undefined: sharedHealth`, `undefined: PublishHealth`.

- [ ] **Step 7: Create the health store**

Create `internal/bgutils/sidecar/health.go`:

```go
package sidecar

import (
	"sync/atomic"
	"time"
)

// Health is the BotGuard sidecar's liveness as both user interfaces report it.
//
// It is a PACKAGE-LEVEL snapshot rather than a field on Sidecar, mirroring
// routes.SharedDiskStatus and routes.SharedUpdateInfo: one value the producer
// writes and each UI's wiring reads, so neither the web routes nor
// cmd/moombox's TUI wiring needs a handle on the Sidecar itself — and
// internal/tui keeps importing neither this package nor internal/bgutils
// (it receives a plain bool through the same app.Send shape ConnectivityMsg
// uses).
type Health struct {
	// Healthy is false from the moment the child dies until a supervisor
	// restart succeeds.
	Healthy bool
	// Reason is why it last went down ("stdout EOF", "readPump panic", a
	// failed initial start). Empty while healthy.
	Reason string
	// Restarts counts SUCCESSFUL supervisor restarts this process.
	Restarts uint64
	// Since is when Healthy last changed.
	Since time.Time
}

// sharedHealth holds the last published snapshot. nil means "never published",
// which is exactly what a process with `[bgutils] use_sidecar = false` looks
// like — and is deliberately NOT the same as "unhealthy", so a correct
// sidecar-disabled config never draws an alert.
var sharedHealth atomic.Pointer[Health]

// PublishHealth records the current sidecar health for both UIs.
func PublishHealth(h Health) {
	sharedHealth.Store(&h)
}

// CurrentHealth returns the last published snapshot. ok is false until the
// first publish.
func CurrentHealth() (Health, bool) {
	if h := sharedHealth.Load(); h != nil {
		return *h, true
	}
	return Health{}, false
}
```

- [ ] **Step 8: Create the supervisor**

Create `internal/bgutils/sidecar/supervisor.go`:

```go
package sidecar

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// DefaultSupervisorBackoff is the restart ladder: 5 s, 15 s, 60 s, then 5 min
// repeating. The first retry is quick because the common death is a V8
// OOM-abort under a BotGuard burst (the shipped V8HardLimitMB, 512, sits just
// above the documented 400-500 MB burst) and the next mint usually fits; the
// 5-minute ceiling is what stops a permanently broken install from spinning
// for the rest of a 24/7 run.
var DefaultSupervisorBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	60 * time.Second,
	5 * time.Minute,
}

// defaultSupervisorStartTimeout bounds ONE restart attempt. It matches the
// 60 s startup budget cmd/moombox gives the first Start: the work is the same
// (blob extraction on a cold cache, then jsdom's module load inside V8).
const defaultSupervisorStartTimeout = 60 * time.Second

// SupervisorConfig wires a Supervisor. Restart is required; every other field
// has a working default.
type SupervisorConfig struct {
	// Restart brings the sidecar back on the same handle — Sidecar.Restart in
	// production. It is called with a per-attempt deadline.
	Restart func(ctx context.Context) error
	// StartTimeout bounds one attempt. Zero uses defaultSupervisorStartTimeout.
	StartTimeout time.Duration
	// Backoff is the delay before each attempt; the LAST entry repeats for
	// every attempt past it. Nil uses DefaultSupervisorBackoff.
	Backoff []time.Duration
	Logger  Logger
}

// Supervisor restarts a Sidecar whose child process died.
//
// Before this existed the sidecar was started once: a crash or an OOM-abort
// flipped markUnhealthy and nothing ever flipped it back, so for the rest of
// the process signature-ciphered formats were unresolvable (sig has no
// fallback — the routing policy is sidecar-only) and PO tokens fell to the
// goja path, which rejects the websafe-only response. Restarting is the fix;
// the health snapshot beside it is what makes the outage visible while it
// lasts.
type Supervisor struct {
	cfg      SupervisorConfig
	notices  chan string
	onUp     atomic.Pointer[func()]
	restarts atomic.Uint64
	// sleep is a test seam. Production leaves it as sleepCtx.
	sleep func(ctx context.Context, d time.Duration)
}

// NewSupervisor builds a Supervisor. It does not start anything; call Run in a
// goroutine with an inline recover.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultSupervisorStartTimeout
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = DefaultSupervisorBackoff
	}
	return &Supervisor{
		cfg: cfg,
		// Buffered: Notify runs on readPump and must never block. One slot is
		// enough — a second death cannot happen before the first restart, and
		// a notice that arrives DURING a restart is already covered by it.
		notices: make(chan string, 1),
		sleep:   sleepCtx,
	}
}

// SetOnUp installs the callback run after every successful restart. It is what
// re-wires the consumers that hold the sidecar: PotProvider and the composite
// cipher solver. Safe to call before or after Run starts.
func (s *Supervisor) SetOnUp(fn func()) {
	s.onUp.Store(&fn)
}

// Notify records that the sidecar died. Safe to call from readPump: the send
// is non-blocking, so a notice arriving while a restart is already running is
// dropped rather than stalling the pump that drains the child's stdout.
func (s *Supervisor) Notify(reason string) {
	select {
	case s.notices <- reason:
	default:
	}
}

// Run drives the restart loop until ctx ends. Call it in a goroutine that
// carries its own inline recover.
func (s *Supervisor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reason := <-s.notices:
			s.cfg.Logger.Error(
				"BotGuard sidecar died — PO tokens and signature-ciphered formats are unavailable until it restarts",
				"reason", reason)
			PublishHealth(Health{
				Healthy:  false,
				Reason:   reason,
				Restarts: s.restarts.Load(),
				Since:    time.Now(),
			})
			if !s.restartLoop(ctx, reason) {
				return
			}
		}
	}
}

// restartLoop retries Restart on the ladder until it succeeds. Returns false
// only when ctx ended, which is the one case Run must stop on.
func (s *Supervisor) restartLoop(ctx context.Context, reason string) bool {
	for attempt := 0; ; attempt++ {
		s.sleep(ctx, s.cfg.Backoff[min(attempt, len(s.cfg.Backoff)-1)])
		if ctx.Err() != nil {
			return false
		}

		startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
		err := s.cfg.Restart(startCtx)
		cancel()
		if err != nil {
			s.cfg.Logger.Warn("BotGuard sidecar restart failed",
				"attempt", attempt+1, "err", err, "deadReason", reason)
			continue
		}

		n := s.restarts.Add(1)
		s.cfg.Logger.Info("BotGuard sidecar restarted",
			"attempt", attempt+1, "restarts", n)
		PublishHealth(Health{Healthy: true, Restarts: n, Since: time.Now()})
		if fn := s.onUp.Load(); fn != nil {
			s.callOnUp(*fn)
		}
		// A death notice queued while we were restarting describes the child
		// we just replaced. Drop it rather than immediately tearing down a
		// healthy sidecar.
		select {
		case <-s.notices:
		default:
		}
		return true
	}
}

// callOnUp isolates a faulty re-wiring callback: the sidecar is already back,
// and a panic here must not take the supervisor loop with it.
func (s *Supervisor) callOnUp(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("BotGuard sidecar OnUp panic", "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// sleepCtx waits d, or returns early when ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
```

- [ ] **Step 9: Run the supervisor tests green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestSupervisor|TestCurrentHealth' ./internal/bgutils/sidecar/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race -run 'TestSupervisor' ./internal/bgutils/sidecar/
```
Expected: PASS, no data race.

- [ ] **Step 10: Add the `OnUnhealthy` hook and the in-place `Restart`**

In `internal/bgutils/sidecar/sidecar.go`, add the field to `Config` (after `ExposeGC`, before `Logger`):

```go
	// OnUnhealthy, when non-nil, is called ONCE each time the sidecar goes
	// from healthy to unhealthy for a reason other than Stop — stdout EOF
	// after a crash or a V8 OOM-abort, or a readPump panic. It runs ON THE
	// readPump GOROUTINE with the health flag already flipped and the pending
	// requests already drained, so it MUST NOT block: the Supervisor wired to
	// it does a non-blocking channel send and nothing else. A panic in the
	// callback is recovered and logged rather than taking the pump down.
	OnUnhealthy func(reason string)
```

Replace `markUnhealthy`:

```go
// markUnhealthy flips the healthy flag and drains pending requests with an
// error so callers blocked on a response wake up promptly, then hands the
// reason to OnUnhealthy so a supervisor can bring the child back.
func (s *Sidecar) markUnhealthy(reason string) {
	if !s.healthy.CompareAndSwap(true, false) {
		return
	}
	s.cfg.Logger.Warn("sidecar marked unhealthy", "reason", reason)
	s.drainPending(reason)
	if s.cfg.OnUnhealthy != nil {
		s.callOnUnhealthy(reason)
	}
}

// callOnUnhealthy isolates the callback from readPump: a panic in a
// supervisor's notify path must not kill the goroutine draining stdout.
func (s *Sidecar) callOnUnhealthy(reason string) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("sidecar: OnUnhealthy panic", "panic", fmt.Sprint(r))
		}
	}()
	s.cfg.OnUnhealthy(reason)
}
```

Guard `writeRequest` against the window between a crash and a restart — replace its locked section:

```go
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		// Between a crash and the supervisor's Restart there is no pipe. A
		// caller that passed the healthy check microseconds before
		// markUnhealthy flipped it must get an error, not a nil dereference.
		return errors.New("sidecar: not running")
	}
	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("stdin write: %w", err)
	}
	return nil
```

Add `Restart` directly after `Stop`:

```go
// Restart brings a dead sidecar back ON THE SAME HANDLE: it stops whatever is
// left of the old child, resets the per-process state, and runs Start again.
//
// The handle's identity is preserved deliberately. cmd/moombox stores one
// *Sidecar on its run state for shutdown and hands the same pointer to
// PotProvider, so swapping in a fresh instance would leave both pointing at a
// corpse — the shutdown path would stop the dead one and leak the live one.
// What DOES have to be rebuilt is the cipher sidecar solver: its per-player
// "already sent" map describes the memory of the child that just died. That
// rebuild belongs to the caller (Supervisor.SetOnUp).
//
// Callers must serialise Restart against itself; the Supervisor is the only
// production caller and its loop is single-threaded. Concurrent RPC callers
// are safe: healthy is false for the whole window, call() short-circuits on
// it, and writeRequest refuses a nil stdin.
func (s *Sidecar) Restart(ctx context.Context) error {
	// Stop is idempotent and, when a child existed, waits for both pumps —
	// so after it returns nothing else touches the fields reset below.
	_ = s.Stop()

	s.writeMu.Lock()
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.stderr = nil
	s.job = nil
	s.readyOnce = sync.Once{}
	s.readyCh = nil
	s.readyErr = nil
	s.stopOnce = sync.Once{}
	s.stopping.Store(false)
	s.healthy.Store(false)
	s.writeMu.Unlock()

	s.pendingMu.Lock()
	s.pending = make(map[uint64]chan rpcResponse)
	s.pendingMu.Unlock()

	return s.Start(ctx)
}
```

- [ ] **Step 11: Pin `Restart` with a test**

Append to `internal/bgutils/sidecar/supervisor_test.go`:

```go
// TestRestartResetsTheStartGuard: Start refuses a handle that already has a
// cmd ("sidecar: already started"). Restart must clear that state or the
// supervisor's first attempt fails forever with a message about the CORPSE.
//
// This exercises the reset without a real Node child: a Sidecar whose cacheDir
// cannot be resolved fails Start early, and the assertion is that the SECOND
// failure is the same early one and not "already started".
//
// Mutant this kills: Restart calling Start without zeroing s.cmd (or without
// resetting stopOnce) → err mentions "already started".
func TestRestartResetsTheStartGuard(t *testing.T) {
	s := New(Config{Logger: testLogger{}, CacheDir: string([]byte{0})})

	first := s.Start(context.Background())
	if first == nil {
		t.Fatal("Start with an unusable cache dir unexpectedly succeeded")
	}
	second := s.Restart(context.Background())
	if second == nil {
		t.Fatal("Restart with an unusable cache dir unexpectedly succeeded")
	}
	if got := second.Error(); got == "sidecar: already started" {
		t.Fatalf("Restart did not reset the start guard: %v", got)
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after a failed Restart")
	}
}
```

If `CacheDir: string([]byte{0})` does not make `Start` fail early on this platform, substitute a path that cannot be created — read `resolveCacheDir` and `extractIfNeeded` and pick a value that fails there (for example a path whose parent is an existing regular file created with `t.TempDir()` + `os.WriteFile`). The assertion must stay: a second `Restart` never reports `"sidecar: already started"`.

- [ ] **Step 12: Run the bgutils packages green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/bgutils/... ./internal/cipher/
```
Expected: PASS.

- [ ] **Step 13: Wire the supervisor in `cmd/moombox/services.go` (§7b)**

Replace the whole `if cfg.Bgutils.UseSidecar { … } else { … }` block (the one that begins `bgSidecar := sidecar.New(sidecar.Config{`) with:

```go
	// The sidecar is SUPERVISED. Started once and never restarted, a crashed
	// or OOM-aborted child latched unhealthy for the rest of a 24/7 run,
	// taking every signature-ciphered format (sig is sidecar-only) and every
	// real PO token with it.
	var bgSupervisor *sidecar.Supervisor
	if cfg.Bgutils.UseSidecar {
		// `sup` is assigned BEFORE Start, and Start is what creates the
		// readPump goroutine that fires OnUnhealthy — so the closure below
		// never reads a nil pointer and never races the write (goroutine
		// creation is the happens-before edge).
		var sup *sidecar.Supervisor
		bgSidecar := sidecar.New(sidecar.Config{
			Logger:        log,
			V8HardLimitMB: cfg.Memory.SidecarHardLimitMB,
			// ExposeGC is required for the periodic TriggerGC call that
			// enforces the soft sidecar limit. Always on when the sidecar
			// is enabled — the cost is a function global no caller invokes
			// unless we ask for it.
			ExposeGC:    true,
			OnUnhealthy: func(reason string) { sup.Notify(reason) },
		})
		sup = sidecar.NewSupervisor(sidecar.SupervisorConfig{
			Restart: bgSidecar.Restart,
			Logger:  log,
		})
		bgSupervisor = sup
		// Stored UNCONDITIONALLY, unlike before: the supervisor can bring the
		// child up minutes after a failed first start, and shutdown.go's Stop
		// must reach whatever is running by then. Stop on a never-started
		// handle is a no-op, and main.go's memory log already gates on
		// IsHealthy().
		s.bgSidecar = bgSidecar

		sCtx, sCancel := context.WithTimeout(s.ctx, 60*time.Second)
		startErr := bgSidecar.Start(sCtx)
		sCancel()
		if startErr != nil {
			log.Warn("BotGuard sidecar failed to start; using goja until the supervisor gets it up",
				slog.String("error", startErr.Error()))
			sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: startErr.Error(), Since: time.Now()})
			// A first start that never succeeded cannot reach markUnhealthy
			// (the CAS needs healthy==true), so the supervisor is told by
			// hand. Same outcome for the operator either way: PO tokens and
			// sig are unavailable until a child comes up.
			sup.Notify("initial start failed: " + startErr.Error())
		} else {
			potProvider.SetSidecar(bgSidecar)
			sidecar.PublishHealth(sidecar.Health{Healthy: true, Since: time.Now()})
			log.Info("BotGuard sidecar ready", slog.String("cacheDir", bgSidecar.CacheDir()))
		}
	} else {
		log.Info("BotGuard sidecar disabled in config; using goja fallback only")
	}
```

- [ ] **Step 14: Re-wire on restart in `cmd/moombox/services.go` (§8)**

Replace the `var sidecarCipher cipher.Solver { … }` block and add the supervisor start right after `ytService.PlayerAPI.SetPotProvider(potProvider)`:

```go
	var sidecarCipher cipher.Solver
	if s.bgSidecar != nil && s.bgSidecar.IsHealthy() {
		sidecarCipher = cipher.NewSidecarSolver(s.bgSidecar, gojaSolver)
	}
	cipherSolver := cipher.NewCompositeSolver(sidecarCipher, gojaSolver)
	s.cipherSolver = gojaSolver
	s.routedCipher = cipherSolver
```

…and, after `ytService.PlayerAPI.SetPotProvider(potProvider)`:

```go
	// Re-wire both consumers every time the supervisor brings the child back.
	// PotProvider holds the same handle, so SetSidecar is idempotent there —
	// but it is also what installs it after a FAILED first start. The cipher
	// sidecar solver is REBUILT rather than reused: its per-player "already
	// sent" map describes the memory of the child that just died.
	if bgSupervisor != nil {
		bgSup := bgSupervisor
		sc := s.bgSidecar
		bgSup.SetOnUp(func() {
			potProvider.SetSidecar(sc)
			cipherSolver.SetSidecar(cipher.NewSidecarSolver(sc, gojaSolver))
		})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("BotGuard sidecar supervisor panic", slog.Any("panic", r))
				}
			}()
			// Returns on s.ctx cancellation. A Restart already in flight at
			// shutdown is harmless: the child is pinned to the parent (Job
			// Object on Windows, PR_SET_PDEATHSIG on Linux) and dies with us.
			bgSup.Run(s.ctx)
		}()
	}
```

- [ ] **Step 15: Build and run the affected packages**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./cmd/moombox ./internal/bgutils/... ./internal/cipher/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/bgutils/... ./internal/cipher/ ./cmd/moombox/
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./cmd/moombox ./internal/bgutils/...
```
Expected: all PASS.

- [ ] **Step 16: Commit**

```bash
git add internal/bgutils/sidecar/sidecar.go internal/bgutils/sidecar/health.go \
        internal/bgutils/sidecar/supervisor.go internal/bgutils/sidecar/supervisor_test.go \
        internal/cipher/solver_composite.go internal/cipher/solver_composite_test.go \
        cmd/moombox/services.go
git commit -m "fix(bgutils): supervise the BotGuard sidecar — restart in place on a backoff ladder

A crashed or V8 OOM-aborted child latched markUnhealthy for the rest of a
24/7 run: sig has no fallback (the routing policy is sidecar-only) so
ciphered formats became unresolvable, and PO tokens fell to the goja path,
which rejects the websafe-only response. Sidecar.Restart resets the handle
in place (preserving the pointer cmd/moombox and PotProvider hold) and
Supervisor drives it on 5s/15s/60s/5min. The composite cipher solver gained
SetSidecar so the rebuilt sidecar solver replaces the one holding the dead
child's sent-map. Reports #5 / YOUTUBE-5.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/bgutils/sidecar/sidecar.go internal/bgutils/sidecar/health.go \
     internal/bgutils/sidecar/supervisor.go internal/bgutils/sidecar/supervisor_test.go \
     internal/cipher/solver_composite.go internal/cipher/solver_composite_test.go \
     cmd/moombox/services.go
```

---

## Task 2: Sidecar-down alert in both UIs

Spec §5: "YOUTUBE-5's fix must give a user-visible signal (dashboard + TUI) in addition to the supervisor." The dashboard's existing health payload is `/api/status`; the TUI's existing health channel is `app.Send(...)` into the status bar (exactly what `ConnectivityMsg` / the `OFFLINE` chip use). The flag travels as a plain `bool`, so `internal/tui` still imports neither `internal/bgutils` nor `internal/web/routes`.

**Files:**
- Modify: `internal/bgutils/sidecar/health.go` (subscriber fan-out)
- Modify: `internal/bgutils/sidecar/supervisor_test.go` (fan-out test)
- Modify: `internal/web/routes/jobs.go` (`StatusRoute` only)
- Create: `internal/web/routes/sidecar_status_test.go`
- Modify: `web/public/app.js` (`loadStatus` + `updateStatusBar`)
- Create: `web/tests/sidecar-warning.test.mjs`
- Modify: `internal/tui/app.go`, `internal/tui/app_update.go`, `internal/tui/status_bar.go`
- Create: `internal/tui/sidecar_alert_test.go`
- Modify: `cmd/moombox/tui_wiring.go`

**Interfaces:**
- Consumes (Task 1): `sidecar.Health`, `sidecar.PublishHealth`, `sidecar.CurrentHealth`.
- Produces: `func SubscribeHealth(fn func(Health)) (unsubscribe func())`; `func ResetHealthForTesting()`; the `/api/status` key `botguardSidecar` → `{"healthy": bool, "reason": string, "restarts": number}`; `tui.SidecarStatusMsg struct{ Healthy bool }`.

- [ ] **Step 1: Write the failing fan-out test**

Append to `internal/bgutils/sidecar/supervisor_test.go`:

```go
// TestSubscribeHealthFansOutAndUnsubscribes: the TUI is PUSH-driven (it draws
// on messages, it does not poll a package global), so the health store has to
// call it. The immediate first call matters as much as the updates — a TUI
// that starts after the sidecar died would otherwise show a healthy bar until
// the next transition, which for a permanently dead sidecar is never.
//
// Mutants this kills:
//   - no immediate call on subscribe    → got[0] is missing, len(got) == 1
//   - PublishHealth not fanning out     → len(got) == 1
//   - unsubscribe not removing the fn   → len(got) == 3
func TestSubscribeHealthFansOutAndUnsubscribes(t *testing.T) {
	resetHealth(t)
	PublishHealth(Health{Healthy: false, Reason: "stdout EOF"})

	var got []Health
	unsub := SubscribeHealth(func(h Health) { got = append(got, h) })

	PublishHealth(Health{Healthy: true, Restarts: 1})
	unsub()
	PublishHealth(Health{Healthy: false, Reason: "again"})

	if len(got) != 2 {
		t.Fatalf("callback ran %d times (%+v), want 2: the current value then one update", len(got), got)
	}
	if got[0].Healthy || got[0].Reason != "stdout EOF" {
		t.Errorf("first call = %+v, want the snapshot that already existed", got[0])
	}
	if !got[1].Healthy || got[1].Restarts != 1 {
		t.Errorf("second call = %+v, want the published update", got[1])
	}
}
```

Extend `resetHealth` (added in Task 1) so it clears subscribers too:

```go
func resetHealth(t *testing.T) {
	t.Helper()
	ResetHealthForTesting()
	t.Cleanup(ResetHealthForTesting)
}
```

- [ ] **Step 2: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestSubscribeHealth ./internal/bgutils/sidecar/
```
Expected: FAIL to compile — `undefined: SubscribeHealth`, `undefined: ResetHealthForTesting`.

- [ ] **Step 3: Add the fan-out and the reset to `health.go`**

Add `"sync"` to the imports and append:

```go
// healthSubs are the PUSH consumers — today exactly one, cmd/moombox's TUI
// wiring, which turns each call into an app.Send. The Web dashboard polls
// CurrentHealth from /api/status instead and is not in this list.
var (
	healthSubsMu sync.Mutex
	healthSubs   []*func(Health)
)

// SubscribeHealth registers fn for every health change and calls it IMMEDIATELY
// with the current snapshot (when one exists), so a subscriber that starts
// after the sidecar died still draws the alert. Returns an unsubscribe func;
// mirrors connectivity's OnStateChange, which cmd/moombox unsubscribes the
// same way at TUI exit.
//
// fn runs on the publisher's goroutine (the supervisor loop, or startup) and
// must not block. A panic in it is recovered so one bad subscriber cannot take
// the supervisor down.
func SubscribeHealth(fn func(Health)) (unsubscribe func()) {
	p := &fn
	healthSubsMu.Lock()
	healthSubs = append(healthSubs, p)
	healthSubsMu.Unlock()

	if h, ok := CurrentHealth(); ok {
		callHealthSub(p, h)
	}

	return func() {
		healthSubsMu.Lock()
		defer healthSubsMu.Unlock()
		for i, q := range healthSubs {
			if q == p {
				healthSubs = append(healthSubs[:i], healthSubs[i+1:]...)
				return
			}
		}
	}
}

func callHealthSub(p *func(Health), h Health) {
	defer func() { _ = recover() }()
	(*p)(h)
}

// ResetHealthForTesting clears the package-level snapshot AND every
// subscriber. Exported because internal/web/routes needs it: the snapshot is
// process-wide state, and a route test that left one published would change
// what the next test sees. Production never calls it.
func ResetHealthForTesting() {
	sharedHealth.Store(nil)
	healthSubsMu.Lock()
	healthSubs = nil
	healthSubsMu.Unlock()
}
```

and extend `PublishHealth`:

```go
// PublishHealth records the current sidecar health for both UIs and pushes it
// to every SubscribeHealth consumer.
func PublishHealth(h Health) {
	sharedHealth.Store(&h)

	healthSubsMu.Lock()
	subs := make([]*func(Health), len(healthSubs))
	copy(subs, healthSubs)
	healthSubsMu.Unlock()

	// Fan out off the lock: a subscriber that unsubscribes from inside its own
	// callback (the TUI's exit path can) would otherwise deadlock.
	for _, p := range subs {
		callHealthSub(p, h)
	}
}
```

- [ ] **Step 4: Run it green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race ./internal/bgutils/sidecar/
```
Expected: PASS.

- [ ] **Step 5: Write the failing `/api/status` test**

Create `internal/web/routes/sidecar_status_test.go`:

```go
package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// getSidecarStatusBody registers StatusRoute on a bare router and returns the
// decoded body.
func getSidecarStatusBody(t *testing.T) map[string]any {
	t.Helper()
	r := chi.NewRouter()
	StatusRoute(r, &StatusRouteDeps{Version: "test", StartTime: time.Now()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	return body
}

// TestStatusCarriesSidecarHealth is the dashboard half of YOUTUBE-5's
// user-visible signal. The ABSENT case is asserted first and on purpose: a
// process with `[bgutils] use_sidecar = false` publishes nothing, and the key
// must then be absent rather than present-and-false — otherwise a correct
// config would paint a permanent warning in the header.
//
// Mutants this kills:
//   - emitting the key unconditionally    → the first subtest finds it
//   - dropping the key entirely           → the second subtest finds nothing
//   - reporting healthy while it is not   → healthy == true in the second subtest
func TestStatusCarriesSidecarHealth(t *testing.T) {
	t.Run("absent when the sidecar was never started", func(t *testing.T) {
		sidecar.ResetHealthForTesting()
		t.Cleanup(sidecar.ResetHealthForTesting)
		if _, ok := getSidecarStatusBody(t)["botguardSidecar"]; ok {
			t.Error("botguardSidecar is present although no health was ever published")
		}
	})

	t.Run("present and false once the supervisor reports a death", func(t *testing.T) {
		sidecar.ResetHealthForTesting()
		sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF", Restarts: 2})
		t.Cleanup(sidecar.ResetHealthForTesting)

		raw, ok := getSidecarStatusBody(t)["botguardSidecar"]
		if !ok {
			t.Fatal("botguardSidecar missing from /api/status")
		}
		got, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("botguardSidecar = %T, want an object", raw)
		}
		if got["healthy"] != false {
			t.Errorf("healthy = %v, want false", got["healthy"])
		}
		if got["reason"] != "stdout EOF" {
			t.Errorf("reason = %v, want %q", got["reason"], "stdout EOF")
		}
		if got["restarts"] != float64(2) {
			t.Errorf("restarts = %v, want 2", got["restarts"])
		}
	})
}
```

- [ ] **Step 6: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestStatusCarriesSidecarHealth ./internal/web/routes/
```
Expected: FAIL — `botguardSidecar missing from /api/status`.

- [ ] **Step 7: Add the route key**

In `internal/web/routes/jobs.go`, add the import `"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"` and, inside `StatusRoute`'s handler, after the `deps.GetChannelHealth` block and before `jsonResponse(rw, resp)`:

```go
		// BotGuard sidecar liveness. ABSENT (not false) when nothing was ever
		// published — that is what `[bgutils] use_sidecar = false` looks like,
		// and a correct config must not paint a warning. Read from the package
		// snapshot rather than a StatusRouteDeps closure for the same reason
		// SharedDiskStatus is read that way: the producer is cmd/moombox and
		// the consumers are both UIs.
		if h, ok := sidecar.CurrentHealth(); ok {
			resp["botguardSidecar"] = map[string]any{
				"healthy":  h.Healthy,
				"reason":   h.Reason,
				"restarts": h.Restarts,
			}
		}
```

- [ ] **Step 8: Run it green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/ ./internal/bgutils/sidecar/
```
Expected: PASS.

- [ ] **Step 9: Write the failing dashboard test**

Create `web/tests/sidecar-warning.test.mjs`:

```js
// The dashboard half of YOUTUBE-5's user-visible signal: when /api/status says
// the BotGuard sidecar is down, the header warning list says so. It rides the
// EXISTING #status-warnings list (the same one the re-login prompts use), so
// there is no new markup — only a new item and the action-less shape that item
// introduces.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = "jsdom not installed - run npm ci in web/tests (" + e.code + ")";
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

function warningLabels(document) {
  return [...document.getElementById("status-warnings").children].map((el) => el.textContent);
}

test("a dead BotGuard sidecar raises a header warning", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": { version: "test", botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 } },
    },
  });

  await h.app.loadStatus();

  // Mutant: the warningItems push removed -> the list is empty.
  assert.deepEqual(warningLabels(h.document), ["PO tokens: sidecar down"]);

  // Mutant: span.dataset.action = w.action left unguarded -> "undefined".
  const span = h.document.getElementById("status-warnings").firstElementChild;
  assert.equal(span.dataset.action, undefined, "a sidecar warning is not clickable and must carry no data-action");
  assert.match(span.title, /BotGuard sidecar/);

  // Mutant: the icon's dataset.action left unguarded -> "undefined" there too.
  const icon = h.document.getElementById("status-warnings-icon");
  assert.ok(icon.classList.contains("active"), "the mobile warning icon stayed inactive");
  assert.equal(icon.dataset.action, undefined);
});

test("a healthy sidecar raises nothing, and so does a payload without the key", { skip }, async () => {
  const healthy = await harness.makeApp({
    routes: { "GET /api/status": { version: "test", botguardSidecar: { healthy: true, reason: "", restarts: 1 } } },
  });
  await healthy.app.loadStatus();
  // Mutant: treating any present botguardSidecar as an alarm -> one item.
  assert.deepEqual(warningLabels(healthy.document), []);

  const absent = await harness.makeApp({ routes: { "GET /api/status": { version: "test" } } });
  await absent.app.loadStatus();
  // Mutant: !status.botguardSidecar?.healthy (true when the key is absent)
  // -> a sidecar-disabled install shows a permanent warning.
  assert.deepEqual(warningLabels(absent.document), []);
});
```

`makeApp`'s `routes` option takes the same `"METHOD /path": body` shape the rest of the suite uses (read `web/tests/helpers/app-dom.mjs`, `makeHttp`). If registering routes needs the array/`on()` form in this harness version, use that form; the assertions stay identical.

- [ ] **Step 10: Run it red**

```bash
cd web/tests && node --test sidecar-warning.test.mjs
```
Expected: FAIL — the first assertion finds `[]`.

- [ ] **Step 11: Add the dashboard warning**

In `web/public/app.js`, in `loadStatus`, beside the other `status.*` reads (after `this.activePlatforms = status.activePlatforms || {};`):

```js
        // Tri-state on purpose. `null` means the server published nothing —
        // `[bgutils] use_sidecar = false` — and must read as "no opinion", not
        // as a fault; only an explicit `healthy: false` raises the warning.
        this.sidecarHealthy = status.botguardSidecar ? status.botguardSidecar.healthy === true : null;
```

In `updateStatusBar`, immediately after the two re-login pushes:

```js
    // The BotGuard sidecar is an ALERT, not a status: while it is down,
    // signature-ciphered formats cannot be resolved at all (sig has no
    // fallback) and PO tokens fall to the goja path, which errors. It carries
    // no action — the supervisor is already retrying and there is nothing for
    // the operator to click.
    if (this.sidecarHealthy === false)
      warningItems.push({
        label: "PO tokens: sidecar down",
        title: "The BotGuard sidecar is not running — signature-ciphered formats and PO tokens are unavailable. Moombox is retrying.",
      });
```

and make the two renderers tolerate an item with no `action`:

```js
      for (const w of warningItems) {
        const span = document.createElement("span");
        span.className = "status-warning";
        if (w.action) span.dataset.action = w.action;
        span.title = w.title || "Click to re-login";
        span.textContent = w.label;
        warningsEl.appendChild(span);
      }
```

```js
        warningsIcon.title = warningItems.map(w => w.label).join(", ");
        if (warningItems[0].action) {
          warningsIcon.dataset.action = warningItems[0].action;
        } else {
          delete warningsIcon.dataset.action;
        }
```

Initialise the field beside the other status fields in the `MoomboxApp` constructor — search for `this.autoCookieReloginRequired` there and add `this.sidecarHealthy = null;` next to it.

- [ ] **Step 12: Run the front-end gates**

```bash
cd web/tests && node --test *.test.mjs
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/
```
Expected: all PASS — the whole node suite, not just the new file, because `app.test.mjs` renders the same header.

- [ ] **Step 13: Write the failing TUI test**

Create `internal/tui/sidecar_alert_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

// TestStatusBarShowsSidecarDown is the TUI half of YOUTUBE-5's user-visible
// signal, and it is an ALERT: while the sidecar is down, sig-ciphered formats
// cannot be resolved at all. So it must survive the same tier squeeze OFFLINE
// survives (see TestStatusBarAlertsOutliveCounters), abbreviating rather than
// disappearing.
//
// Mutants this kills:
//   - the chip never rendered                     → the wide case finds nothing
//   - the chip gated on t <= tierCompact          → the narrow case finds nothing
//   - the chip rendered when sidecarDown is false → the healthy case finds it
func TestStatusBarShowsSidecarDown(t *testing.T) {
	wide := NewStatusBarModel()
	wide.sidecarDown = true
	wide.SetWidth(200)
	if got := stripANSI(wide.View()); !strings.Contains(got, "SIDECAR DOWN") {
		t.Errorf("wide bar has no sidecar alert: %q", got)
	}

	narrow := NewStatusBarModel()
	narrow.sidecarDown = true
	narrow.SetWidth(46)
	if got := stripANSI(narrow.View()); !strings.Contains(got, "POT") {
		t.Errorf("sidecar alert dropped at width 46: %q", got)
	}

	healthy := NewStatusBarModel()
	healthy.SetWidth(200)
	if got := stripANSI(healthy.View()); strings.Contains(got, "SIDECAR") || strings.Contains(got, "POT") {
		t.Errorf("healthy bar drew a sidecar alert: %q", got)
	}
}
```

Then pin the message handler, in the same file. `NewApp()` (`internal/tui/app.go:675`) takes no arguments and is what `app_layout_test.go` already uses:

```go
// TestSidecarStatusMsgReachesTheBar pins the wiring end of the same signal:
// cmd/moombox turns a sidecar.Health into this message, and nothing else does.
// The `case` in app_update.go is the only thing connecting the two halves, so
// it is pinned by execution rather than by reading the source.
//
// Mutant this kills: the app_update case dropped (or inverted) → the bar's
// flag stays false and the alert never appears.
func TestSidecarStatusMsgReachesTheBar(t *testing.T) {
	a := NewApp()

	a.Update(SidecarStatusMsg{Healthy: false})
	if !a.statusBar.sidecarDown {
		t.Error("SidecarStatusMsg{Healthy:false} did not raise the status bar flag")
	}

	a.Update(SidecarStatusMsg{Healthy: true})
	if a.statusBar.sidecarDown {
		t.Error("SidecarStatusMsg{Healthy:true} did not clear the status bar flag")
	}
}
```

`App.Update` returns `(tea.Model, tea.Cmd)`; discard both — the handler mutates `a.statusBar` in place, exactly as the `ConnectivityMsg` case does. If the package's `Update` is defined on a value receiver so the mutation would not stick, assert on the returned model instead (`m, _ := a.Update(…); m.(*App).statusBar.sidecarDown`).

- [ ] **Step 14: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestStatusBarShowsSidecarDown|TestSidecarStatusMsg' ./internal/tui/
```
Expected: FAIL to compile — `wide.sidecarDown undefined`, `undefined: SidecarStatusMsg`.

- [ ] **Step 15: Add the TUI message, handler and chip**

`internal/tui/app.go`, directly after `ConnectivityMsg`:

```go
// SidecarStatusMsg is sent when the BotGuard sidecar's liveness changes.
// A plain bool by design: internal/tui must not import internal/bgutils, so
// cmd/moombox's wiring projects sidecar.Health onto this and nothing else
// crosses the boundary.
type SidecarStatusMsg struct {
	Healthy bool
}
```

`internal/tui/app_update.go`, directly after the `ConnectivityMsg` case (`nil`, not `a.listenForUpdates()`, because this message arrives via `program.Send` like `ConnectivityMsg` and not through the update channel):

```go
	case SidecarStatusMsg:
		a.statusBar.sidecarDown = !msg.Healthy
		return a, nil
```

`internal/tui/status_bar.go`, in `StatusBarModel` beside `offline`:

```go
	// sidecarDown indicates that the BotGuard sidecar is not running. An
	// alert, not a status: while it is down, signature-ciphered formats
	// cannot be resolved at all (sig has no goja fallback) and PO tokens
	// fall to a path that errors.
	sidecarDown bool
```

and in `renderMetrics`, immediately after the connectivity block:

```go
	// BotGuard sidecar — an alert for the same reason OFFLINE is one: it names
	// a capability that is GONE, not a number. So it abbreviates instead of
	// disappearing.
	if m.sidecarDown {
		if t >= tierTight {
			parts = append(parts, statusBarRedStyle.Render("POT"))
		} else {
			parts = append(parts, statusBarRedStyle.Render("SIDECAR DOWN"))
		}
	}
```

- [ ] **Step 16: Run the TUI package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/
```
Expected: PASS — including `TestStatusBarNeverExceedsWidth` and `TestStatusBarTiersNarrowMonotonically`, which the new chip must not break.

- [ ] **Step 17: Subscribe the TUI**

In `cmd/moombox/tui_wiring.go`, add the import `"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"` and, immediately after the `unsubConnTUI := s.connMon.OnStateChange(...)` block:

```go
	// Wire BotGuard sidecar liveness to the TUI (program.Send, like
	// connectivity above). SubscribeHealth calls back immediately with the
	// current snapshot, so a TUI started after a dead sidecar still draws the
	// alert; a process with the sidecar disabled published nothing and gets no
	// callback at all, which is why the bar stays quiet there.
	unsubSidecarTUI := sidecar.SubscribeHealth(func(h sidecar.Health) {
		app.Send(tui.SidecarStatusMsg{Healthy: h.Healthy})
	})
```

and add `unsubSidecarTUI()` to the cleanup list, directly after `unsubConnTUI()`.

- [ ] **Step 18: Build and run every affected gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./cmd/moombox ./internal/tui/ ./internal/web/routes/ ./internal/bgutils/sidecar/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/ ./internal/web/routes/ ./internal/bgutils/sidecar/ ./cmd/moombox/
cd web/tests && node --test *.test.mjs
```
Expected: all PASS.

- [ ] **Step 19: Commit**

```bash
git add internal/bgutils/sidecar/health.go internal/bgutils/sidecar/supervisor_test.go \
        internal/web/routes/jobs.go internal/web/routes/sidecar_status_test.go \
        web/public/app.js web/tests/sidecar-warning.test.mjs \
        internal/tui/app.go internal/tui/app_update.go internal/tui/status_bar.go \
        internal/tui/sidecar_alert_test.go cmd/moombox/tui_wiring.go
git commit -m "feat(ui): surface a dead BotGuard sidecar in both dashboards

/api/status gains botguardSidecar (absent when the sidecar is disabled, so a
correct config draws nothing) and the dashboard raises it in the existing
status-warnings list; the TUI gets a red SIDECAR DOWN / POT chip through the
same app.Send shape ConnectivityMsg uses, so internal/tui still imports
neither internal/bgutils nor internal/web/routes. Spec 2026-09-17 section 5.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/bgutils/sidecar/health.go internal/bgutils/sidecar/supervisor_test.go \
     internal/web/routes/jobs.go internal/web/routes/sidecar_status_test.go \
     web/public/app.js web/tests/sidecar-warning.test.mjs \
     internal/tui/app.go internal/tui/app_update.go internal/tui/status_bar.go \
     internal/tui/sidecar_alert_test.go cmd/moombox/tui_wiring.go
```

---

## Task 3: Reject a substituted video (row #4 / YOUTUBE-1)

No client response is checked for `videoDetails.videoId == requested`. yt-dlp's `_invalid_player_response` (`_video.py:3022-3025`, applied at `:3038` and `:3122`) skips such a client because **a blocked or rate-limited source IP is served a SUBSTITUTE video's player response** (TeamNewPipe/NewPipe#8713, quoted verbatim in upstream's own comment). Moombox takes the substitute's status and formats: the waiting-room probe (`stream_processor_youtube.go:218-220` → `:367` → `:389`) reads a VOD substitute as "became VOD" and `completeStreamTransition` archives the wrong video under this job, after `updateJobMetadata` has already rewritten its title. Reproduced by both the reviewer and the verifier.

**Files:**
- Modify: `internal/youtube/player_api_parsing.go` (error types + the check in `parsePlayerResponse`)
- Modify: `internal/youtube/player_api_strategy.go` (thread `videoID`; `mismatchTally`; `finishExtraction`; the `fetchWatchPage` seam)
- Modify: `internal/youtube/player_api_parsing_test.go`
- Modify: `internal/youtube/player_api_strategy_test.go`

**Interfaces:**
- Produces: `type VideoIDMismatchError struct { Requested, Got string }` with `Error() string`; `var ErrAllClientsMismatched error`; `parsePlayerResponse(ctx, data, playerURL, ytcfg, requestedVideoID string)`; `doRetryRequest(ctx, apiURL, body, headers, ytcfg, clientLabel, videoID string)`; `var fetchWatchPage = FetchWatchPage` (test seam used again in Task 7).
- Consumes: nothing from Tasks 1–2.

- [ ] **Step 1: Write the failing parse tests**

Append to `internal/youtube/player_api_parsing_test.go` (add `"context"` and `"encoding/json"` to its imports if absent):

```go
// decodePlayerJSON is a small helper for the parse tests: the parser takes the
// already-decoded map YouTube's body unmarshals into.
func decodePlayerJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return m
}

// TestParsePlayerResponseRejectsASubstituteVideo pins yt-dlp's
// _invalid_player_response (_video.py:3022-3025). A blocked source IP is
// served a DIFFERENT video's player response; accepting it hands this job the
// substitute's status and formats, and the waiting-room probe then archives
// the wrong video under this job's ID.
//
// Mutants this kills:
//   - the comparison dropped entirely      → err is nil
//   - the mismatch only logged, not raised → err is nil
//   - the error not naming both IDs        → the two Contains checks fail
func TestParsePlayerResponseRejectsASubstituteVideo(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "OTHERvideo1", "title": "Substitute Video", "author": "Other Ch"},
		"streamingData": {"adaptiveFormats": [
			{"itag": 137, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
			{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
		]}
	}`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "REQUESTEDvid")
	if err == nil {
		t.Fatalf("parsePlayerResponse accepted a substitute video: %+v", info)
	}
	var mm *VideoIDMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %T (%v), want *VideoIDMismatchError", err, err)
	}
	if mm.Requested != "REQUESTEDvid" || mm.Got != "OTHERvideo1" {
		t.Errorf("mismatch = %+v, want Requested REQUESTEDvid / Got OTHERvideo1", mm)
	}
	if !strings.Contains(err.Error(), "REQUESTEDvid") || !strings.Contains(err.Error(), "OTHERvideo1") {
		t.Errorf("error text %q names neither both IDs", err.Error())
	}
	if info != nil {
		t.Errorf("a rejected response still returned info: %+v", info)
	}
}

// TestParsePlayerResponseKeepsAResponseWithNoVideoID pins the DELIBERATE
// divergence from upstream. yt-dlp treats an ABSENT videoDetails.videoId as a
// mismatch (None != video_id). Moombox must not: TV is this cascade's
// playability AUTHORITY and some of its refusals arrive as a playabilityStatus
// with no videoDetails at all, and rejecting those would throw away the verdict
// every downstream error string is built from.
//
// Mutants this kills:
//   - implementing upstream's literal rule (absent == mismatch) → err non-nil
//   - skipping the check when requestedVideoID is non-empty but the response
//     carries a DIFFERENT non-empty id                          → caught above
func TestParsePlayerResponseKeepsAResponseWithNoVideoID(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})

	noDetails := decodePlayerJSON(t, `{"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Join this channel to get access"}}`)
	info, err := p.parsePlayerResponse(context.Background(), noDetails, "", nil, "REQUESTEDvid")
	if err != nil {
		t.Fatalf("a members-only verdict with no videoDetails was rejected: %v", err)
	}
	if info.PlayabilityError != PlayabilityMembersOnly {
		t.Errorf("PlayabilityError = %q, want members_only — the verdict must survive", info.PlayabilityError)
	}

	emptyID := decodePlayerJSON(t, `{"playabilityStatus": {"status": "OK"}, "videoDetails": {"videoId": "", "title": "t"}}`)
	if _, err := p.parsePlayerResponse(context.Background(), emptyID, "", nil, "REQUESTEDvid"); err != nil {
		t.Fatalf("an empty videoId was treated as a mismatch: %v", err)
	}

	matching := decodePlayerJSON(t, `{"playabilityStatus": {"status": "OK"}, "videoDetails": {"videoId": "REQUESTEDvid", "title": "t"}}`)
	if _, err := p.parsePlayerResponse(context.Background(), matching, "", nil, "REQUESTEDvid"); err != nil {
		t.Fatalf("a matching videoId was rejected: %v", err)
	}
}
```

Add `"errors"` and `"strings"` to the test file's imports if absent.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestParsePlayerResponse ./internal/youtube/
```
Expected: FAIL to compile — `too many arguments in call to p.parsePlayerResponse`, `undefined: VideoIDMismatchError`.

- [ ] **Step 3: Add the error types and the check**

In `internal/youtube/player_api_parsing.go`, add `"errors"` and `"fmt"` to the imports and insert above `parsePlayerResponse`:

```go
// VideoIDMismatchError reports a player response whose videoDetails.videoId is
// not the video that was asked for. yt-dlp calls this an "invalid player
// response" (_video.py:3022-3025) and its only attested cause is a blocked or
// rate-limited source IP being served a SUBSTITUTE video
// (TeamNewPipe/NewPipe#8713). It is returned rather than logged because the
// cascade's error handling is what skips the client.
type VideoIDMismatchError struct {
	Requested string
	Got       string
}

func (e *VideoIDMismatchError) Error() string {
	return fmt.Sprintf("player response is for video %q, not %q (YouTube served a substitute — the source IP may be rate-limited or blocked)",
		e.Got, e.Requested)
}

// ErrAllClientsMismatched is upstream's terminal verdict when nothing survived
// the check: `raise ExtractorError('All player responses are invalid. Your IP
// is likely being blocked by Youtube')` (_video.py:3186-3188).
var ErrAllClientsMismatched = errors.New("every Innertube client returned a player response for a different video — this IP is likely being blocked by YouTube")
```

Change the signature and insert the check immediately after `videoDetails, _ := data["videoDetails"].(map[string]any)`:

```go
func (p *PlayerAPI) parsePlayerResponse(ctx context.Context, data map[string]any, playerURL string, ytcfg *YtcfgData, requestedVideoID string) (*VideoInfo, error) {
	videoDetails, _ := data["videoDetails"].(map[string]any)

	// yt-dlp's _invalid_player_response (_video.py:3022-3025, applied at :3038
	// and :3122): "YouTube may return a different video player response than
	// expected." Taking a substitute's response would hand this job the
	// substitute's status AND formats — the waiting-room probe reads a VOD
	// substitute as "became VOD" and the orchestrator archives the wrong video.
	//
	// DELIBERATE DIVERGENCE: upstream treats an ABSENT videoDetails.videoId as
	// a mismatch too (None != video_id). Moombox does not. TV is this
	// cascade's playability AUTHORITY and several of its refusals arrive as a
	// playabilityStatus with no videoDetails at all; rejecting those would
	// discard the verdict every downstream error string is built from. Only a
	// NON-EMPTY id that differs is a substitute.
	if requestedVideoID != "" {
		if got := getStr(videoDetails, "videoId"); got != "" && got != requestedVideoID {
			return nil, &VideoIDMismatchError{Requested: requestedVideoID, Got: got}
		}
	}

	streamingData, _ := data["streamingData"].(map[string]any)
```

- [ ] **Step 4: Thread `videoID` through the request path**

In `internal/youtube/player_api_strategy.go`:

- `doRetryRequest` gains a trailing `videoID string` parameter, and its final decode becomes `return p.parsePlayerResponse(ctx, data, playerURL, ytcfg, videoID)`. Extend its doc comment with:
  ```go
  // videoID is the video that was ASKED for; parsePlayerResponse rejects a
  // response about any other one (yt-dlp's _invalid_player_response).
  ```
- `fetchWithClient`: `return p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "Innertube", videoID)`.
- `fetchWithCookielessClient`: `return p.doRetryRequest(ctx, apiURL, body, headers, nil, client.ClientName, videoID)`.
- `fetchWithEmbedded`: `return p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "WEB_EMBEDDED", videoID)`.

- [ ] **Step 5: Add the watch-page seam, the tally and the single exit**

Still in `player_api_strategy.go`, add above `withAttestation`:

```go
// fetchWatchPage is FetchWatchPage behind a package var, purely so the cascade
// tests can exercise the whole client chain without a real watch-page round
// trip (the playerRetryBackoffBase seam below exists for the same reason).
// Production never writes it.
var fetchWatchPage = FetchWatchPage

// mismatchTally counts the client player responses one extraction rejected
// because YouTube answered about a different video. Upstream skips such a
// client with a warning and fails the whole extraction only when NOTHING
// survived (_video.py:3180-3188); the tally is how this cascade reproduces
// that "and nothing else survived" condition.
type mismatchTally struct {
	attempts   int
	mismatched int
}

// note records one client attempt and passes its result through unchanged, so
// call sites read `result, err := tally.note(p.fetchWithClient(...))`.
func (t *mismatchTally) note(info *VideoInfo, err error) (*VideoInfo, error) {
	t.attempts++
	var mm *VideoIDMismatchError
	if errors.As(err, &mm) {
		t.mismatched++
	}
	return info, err
}

// allMismatched reports the IP-block shape: at least one client was tried and
// every single one answered about a different video.
func (t *mismatchTally) allMismatched() bool {
	return t.attempts > 0 && t.attempts == t.mismatched
}

// finishExtraction is the single exit both cascades take. It applies
// withAttestation and raises the IP-block verdict when every client answered
// about a different video AND the watch page produced nothing usable either.
// wpParsed is the survivor test: upstream keeps the watch page's own player
// response in `prs` and only raises when `prs` is empty.
func (p *PlayerAPI) finishExtraction(info *VideoInfo, wp *WatchPageResult, videoID string, tally *mismatchTally, wpParsed *VideoInfo) (*VideoInfo, error) {
	if tally.allMismatched() && wpParsed == nil {
		p.logger.Warn("[PlayerApi] every Innertube client answered about a different video",
			"videoID", videoID, "clients", tally.attempts)
		return nil, ErrAllClientsMismatched
	}
	return withAttestation(info, wp, videoID), nil
}
```

Add `"errors"` to the file's imports.

- [ ] **Step 6: Apply the seam, the tally and the exit in both cascades**

In `GetVideoInfoAuthenticated` and `GetVideoInfoPublic`:

1. Replace `FetchWatchPage(ctx, videoID, …)` with `fetchWatchPage(ctx, videoID, …)`.
2. Declare `tally := &mismatchTally{}` immediately after `formatPool := []Format{}`.
3. Wrap every client fetch in the tally. Authenticated path:
   ```go
   authEmb, authEmbErr := tally.note(p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, false))
   result, err := tally.note(p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, sts))
   webResult, webErr := tally.note(p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, sts))
   webResult, webErr = tally.note(p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts))
   vrResult, vrErr := tally.note(p.fetchWithAndroidVR(ctx, videoID, ytcfg.VisitorData))
   embResult, embErr = tally.note(p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, true))
   wcResult, wcErr := tally.note(p.fetchWithClient(ctx, videoID, constants.WebCreatorClient, ytcfg, sts))
   ```
   Public path: the TV, web_embedded and android_vr calls the same way.
4. `tryCookielessFallbacks` gains a trailing `tally *mismatchTally` parameter and wraps its own fetch:
   ```go
   fbResult, fbErr := tally.note(p.fetchWithCookielessClient(ctx, videoID, visitorData, fb.client))
   ```
   Both call sites pass `tally`. Document the new parameter on the function:
   ```go
   // tally counts the video-ID mismatches this chain contributes, so a
   // cascade in which EVERY client was served a substitute can be reported
   // as the IP block it is rather than as "no formats".
   ```
5. Make the watch-page parse honour the check and log its rejection. In both cascades replace `wpParsed, _ = p.parsePlayerResponse(...)` with:
   ```go
   	var wpErr error
   	wpParsed, wpErr = p.parsePlayerResponse(ctx, wp.PlayerResponse, ytcfg.PlayerURL, ytcfg, videoID)
   	if wpErr != nil {
   		// Today the only error this can be is a video-ID mismatch: the page
   		// itself was served for another video, which upstream also drops
   		// (_video.py:3038). Nothing downstream may use it.
   		p.logger.Warn("[PlayerApi] watch-page player response rejected", slog.String("error", wpErr.Error()))
   		wpParsed = nil
   	} else if wpParsed != nil {
   		collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPageAuth)
   	}
   ```
   (the public path uses `wp.Ytcfg` and `AuthLevelWatchPagePublic`).
6. Replace **every** `return withAttestation(X, wp, videoID), nil` in both cascades with `return p.finishExtraction(X, wp, videoID, tally, wpParsed)`. There are five in `GetVideoInfoAuthenticated` (the age-restricted success, the cookieless success, the all-clients-exhausted watch-page return, the `wcResult` return, the final `finalizeVideoInfo` return) and five in `GetVideoInfoPublic` (the TV-failure watch-page return, the age-restricted success, the cookieless success, the exhausted watch-page return, the final return). Grep to confirm none is left:
   ```bash
   grep -n "return withAttestation" internal/youtube/player_api_strategy.go
   ```
   must print nothing.

- [ ] **Step 7: Write the failing cascade tests**

Append to `internal/youtube/player_api_strategy_test.go`:

```go
// substituteBody is what a blocked IP gets: a well-formed 200 about ANOTHER
// video. The itags differ from adequateOKBody's so a leak into the pool is
// visible.
const substituteBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "OTHERvideo1", "title": "Substitute Video", "author": "Other Ch"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 248, "url": "https://substitute/v", "mimeType": "video/webm; codecs=\"vp9\"", "width": 1920, "height": 1080},
		{"itag": 251, "url": "https://substitute/a", "mimeType": "audio/webm; codecs=\"opus\""}
	]}
}`

// stubWatchPage points the cascades at an empty watch page so the tests
// exercise the CLIENT chain without a real HTTP round trip.
func stubWatchPage(t *testing.T) {
	t.Helper()
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		return &WatchPageResult{Ytcfg: DefaultYtcfg()}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// TestCascadeSkipsASubstitutingClient: TV is served the substitute, VISIONOS
// answers correctly, and the result must be VISIONOS's — with none of the
// substitute's formats in the pool.
//
// Mutants this kills:
//   - the mismatch check removed        → title is "Substitute Video"
//   - the mismatch not erroring the call → itags 248/251 appear in the pool
func TestCascadeSkipsASubstitutingClient(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, substituteBody},  // TV_DOWNGRADED
		"101": {http.StatusOK, adequateOKBody},  // VISIONOS
		"28":  {http.StatusOK, adequateOKBody},  // ANDROID_VR
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	if info.Title == "Substitute Video" {
		t.Fatal("the cascade adopted the substitute video's metadata")
	}
	for _, f := range info.Formats {
		if f.Itag == 248 || f.Itag == 251 {
			t.Errorf("a substitute format reached the pool: itag %d from %s", f.Itag, f.Source)
		}
	}
}

// TestEveryClientSubstitutedIsReportedAsAnIPBlock mirrors upstream's
// "All player responses are invalid. Your IP is likely being blocked by
// Youtube" (_video.py:3186-3188). Reporting "no formats" instead would send
// the operator hunting for a format problem that does not exist.
//
// Mutants this kills:
//   - finishExtraction not raising          → err is nil
//   - allMismatched using > instead of ==   → err is nil
//   - raising even though the watch page
//     produced a usable response            → covered by the second subtest
func TestEveryClientSubstitutedIsReportedAsAnIPBlock(t *testing.T) {
	all := map[string]struct {
		status int
		body   string
	}{
		"7": {http.StatusOK, substituteBody}, "101": {http.StatusOK, substituteBody},
		"28": {http.StatusOK, substituteBody}, "56": {http.StatusOK, substituteBody},
	}

	t.Run("no watch page survivor", func(t *testing.T) {
		stubWatchPage(t)
		tr := &clientKeyedTransport{responses: all}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		_, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567")
		if !errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("err = %v, want ErrAllClientsMismatched", err)
		}
	})

	t.Run("a valid watch page survives", func(t *testing.T) {
		origFetch := fetchWatchPage
		fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
			return &WatchPageResult{
				Ytcfg:          DefaultYtcfg(),
				PlayerResponse: decodePlayerJSON(t, adequateOKBody),
			}, nil
		}
		t.Cleanup(func() { fetchWatchPage = origFetch })

		tr := &clientKeyedTransport{responses: all}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		info, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("a usable watch page must survive a full client sweep: %v", err)
		}
		if info == nil || len(info.Formats) == 0 {
			t.Fatalf("info = %+v, want the watch page's formats", info)
		}
	})
}
```

Add `"errors"` to the test file's imports if absent.

- [ ] **Step 8: Run the package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
```
Expected: PASS. If an existing fixture body in the package uses a `videoId` other than the ID the test requests, fix the FIXTURE (give it the requested ID) — never weaken the check.

- [ ] **Step 9: Note the probe-classifier consequence (no code change)**

Add this note to `internal/worker/probe_classify.go`'s `classifyProbeErr` doc comment — it is the one place a reader would go looking:

```go
// A youtube.VideoIDMismatchError (and ErrAllClientsMismatched) falls to the
// asymmetric default, classNetwork: the waiting-room loop keeps waiting rather
// than counting toward its give-up budget. That is the RIGHT answer here —
// while YouTube is serving substitutes we know nothing about the real video,
// and the cost model says a missed classification must only ever delay giving
// up, never wrongly error a waiting stream.
```

Do not change the classifier logic.

- [ ] **Step 10: Run the worker package and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
```

```bash
git add internal/youtube/player_api_parsing.go internal/youtube/player_api_strategy.go \
        internal/youtube/player_api_parsing_test.go internal/youtube/player_api_strategy_test.go \
        internal/worker/probe_classify.go
git commit -m "fix(youtube): reject a player response for a different video

A blocked or rate-limited source IP is served a SUBSTITUTE video's player
response (yt-dlp _invalid_player_response, NewPipe #8713). Moombox took its
status and formats, so the waiting-room probe read a VOD substitute as
'became VOD' and archived the wrong video under the job. parsePlayerResponse
now compares videoDetails.videoId to the requested ID and errors naming both,
so the cascade skips the client; every client mismatching with no watch-page
survivor raises ErrAllClientsMismatched, upstream's IP-block verdict. An
ABSENT videoId is deliberately not a mismatch — TV's playability verdicts
arrive that way. Report #4 / YOUTUBE-1.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/player_api_parsing.go internal/youtube/player_api_strategy.go \
     internal/youtube/player_api_parsing_test.go internal/youtube/player_api_strategy_test.go \
     internal/worker/probe_classify.go
```

---

## Task 4: DRM, DRC and dubbed audio tracks (row #12 / YOUTUBE-2)

`drmFamilies`, `isDrc` and `audioTrack` are never read and `deduplicateFormats` keys on itag ALONE. Two consequences, both reproduced: a TV-tier DRM-wrapped format wins the same-itag tie over WEB's clean one (`AuthLevelTVAuth` 1 < `AuthLevelWeb` 5) and encrypted samples get muxed; and on an auto-dubbed video several itag-140 entries from the SAME client differ only by `audioTrack.id`, so whichever is listed first survives and the original-language track is silently discarded. Upstream's identity is `get_stream_id` = `(itag, audioTrack.id, isDrc)` (`_video.py:3396-3397`); it drops DRM formats outright (`has_drm` at `:3418`, filtered in `YoutubeDL.py:2930`) and ranks tracks with `get_language_code_and_preference` (`:3277-3291`, `ORIGINAL_LANG_VALUE = 10`, `DEFAULT_LANG_VALUE = 5`, descriptive `-10`, everything else `-1`).

**Files:**
- Modify: `internal/youtube/types.go` (`Format` fields)
- Modify: `internal/youtube/player_api_parsing.go` (`parseFormats`, `deduplicateFormats`, a `getBool` helper)
- Modify: `internal/youtube/format_selector.go` (audio track preference)
- Modify: `internal/youtube/player_api_parsing_test.go`, `internal/youtube/format_selector_test.go`
- Modify: `docs/spec/platform-services.md` (the dedup sentence, step 10 of the authenticated flow)

**Interfaces:**
- Produces: `Format.AudioTrackID`, `Format.AudioTrackName`, `Format.AudioIsDefault`, `Format.IsDrc`; `func audioTrackScore(f *Format) int`.
- Consumes (Task 3): `parsePlayerResponse`'s new signature (unchanged by this task).

- [ ] **Step 1: Write the failing parse/dedup tests**

Append to `internal/youtube/player_api_parsing_test.go`:

```go
// TestParseFormatsSkipsDRMAndKeepsTrackIdentity pins both halves of upstream's
// format identity. DRM formats are dropped at parse — yt-dlp reports them as
// skipped (_video.py:3418-3426, the tv-client DRM experiment, issue #12563)
// and YoutubeDL.py:2930 filters them out — because muxing encrypted samples
// produces an unplayable archive. The three track fields are kept because
// they are two thirds of upstream's stream identity.
//
// Mutants this kills:
//   - drmFamilies ignored        → the DRM itag 137 survives
//   - audioTrack not parsed      → AudioTrackID is ""
//   - isDrc not parsed           → IsDrc is false for the DRC entry
func TestParseFormatsSkipsDRMAndKeepsTrackIdentity(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	sd := decodePlayerJSON(t, `{"adaptiveFormats": [
		{"itag": 137, "url": "https://tv/v-drm", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080, "drmFamilies": ["WIDEVINE"]},
		{"itag": 136, "url": "https://tv/v-clean", "mimeType": "video/mp4; codecs=\"avc1.4d401f\"", "width": 1280, "height": 720},
		{"itag": 140, "url": "https://tv/a-orig", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}},
		{"itag": 140, "url": "https://tv/a-drc", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "isDrc": true, "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}}
	]}`)

	formats := p.parseFormats(sd)

	for _, f := range formats {
		if f.Itag == 137 {
			t.Errorf("a DRM-protected format survived parse: %+v", f)
		}
	}
	var orig, drc *Format
	for i := range formats {
		switch {
		case formats[i].Itag == 140 && formats[i].IsDrc:
			drc = &formats[i]
		case formats[i].Itag == 140:
			orig = &formats[i]
		}
	}
	if orig == nil || drc == nil {
		t.Fatalf("want both itag-140 renditions, got %+v", formats)
	}
	if orig.AudioTrackID != "en.4" || orig.AudioTrackName != "English original" || !orig.AudioIsDefault {
		t.Errorf("track fields = %+v, want id en.4 / name \"English original\" / default true", orig)
	}
	if drc.IsDrc != true {
		t.Errorf("the isDrc rendition parsed with IsDrc=false: %+v", drc)
	}
}

// TestDeduplicateFormatsKeysOnUpstreamsStreamIdentity pins get_stream_id
// (_video.py:3396-3397). Keying on itag alone silently discards every dubbed
// track but the first-listed one — the more reachable half of the row,
// because both entries come from the SAME client at the SAME auth level.
//
// Mutants this kills:
//   - keying on itag alone       → only one itag-140 survives
//   - dropping isDrc from the key → the DRC rendition evicts the clean one
//   - dropping audioTrackID      → the ja dub evicts the en original
func TestDeduplicateFormatsKeysOnUpstreamsStreamIdentity(t *testing.T) {
	lvl := AuthLevelTVAuth
	mk := func(track string, drc bool, url string) Format {
		return Format{Itag: 140, URL: url, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
			AudioTrackID: track, IsDrc: drc, Source: "tv_auth", AuthLevel: &lvl}
	}
	pool := []Format{
		mk("en.4", false, "https://x/en"),
		mk("ja.3", false, "https://x/ja"),
		mk("en.4", true, "https://x/en-drc"),
	}

	got := deduplicateFormats(pool)
	if len(got) != 3 {
		t.Fatalf("dedup kept %d of 3 distinct streams: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.AudioTrackID+"/"+fmt.Sprint(f.IsDrc)] = true
	}
	for _, want := range []string{"en.4/false", "ja.3/false", "en.4/true"} {
		if !seen[want] {
			t.Errorf("dedup dropped the %s rendition: %+v", want, got)
		}
	}
}
```

Add `"fmt"` to the test file's imports if absent.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParseFormatsSkipsDRM|TestDeduplicateFormatsKeys' ./internal/youtube/
```
Expected: FAIL to compile — `f.AudioTrackID undefined`.

- [ ] **Step 3: Add the Format fields**

In `internal/youtube/types.go`, inside `Format` after `AudioSampleRate`:

```go
	// AudioTrackID is `audioTrack.id` — the per-language identity of a dubbed
	// audio rendition ("en.4", "ja.3"). Two thirds of upstream's stream
	// identity live here and in IsDrc: a dubbed video lists SEVERAL itag-140
	// entries from the same client that differ only by this field.
	AudioTrackID string `json:"audioTrackId,omitempty"`
	// AudioTrackName is `audioTrack.displayName`. The only place upstream can
	// read "original" or "descriptive" from, so it is what ranks the tracks.
	AudioTrackName string `json:"audioTrackName,omitempty"`
	// AudioIsDefault mirrors `audioTrack.audioIsDefault` — YouTube's own pick
	// for this viewer, upstream's DEFAULT_LANG_VALUE.
	AudioIsDefault bool `json:"audioIsDefault,omitempty"`
	// IsDrc marks a Dynamic Range Compression (loudness-normalised) rendition.
	// A separate STREAM upstream, not a variant of the clean one, so it must
	// not evict its twin — and the clean one is preferred when both exist.
	IsDrc bool `json:"isDrc,omitempty"`
```

- [ ] **Step 4: Parse them, drop DRM, and key the dedup on all three**

In `internal/youtube/player_api_parsing.go`, add a bool getter beside `getInt`:

```go
func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}
```

In `parseFormats`, declare `drmSkipped := 0` above the `for _, key := range …` loop, and immediately after the `f, ok := item.(map[string]any)` guard:

```go
			// DRM formats are dropped rather than ranked. yt-dlp reports them
			// as skipped (_video.py:3418-3426 — an account-level experiment
			// applies DRM to ALL videos on the tv client, issue #12563) and
			// YoutubeDL.py:2930 filters them out, because muxing encrypted
			// samples produces an unplayable archive. Dropping here rather
			// than in the selector means a clean same-itag format from
			// another client still wins the dedup tie it used to LOSE.
			if drm, ok := f["drmFamilies"].([]any); ok && len(drm) > 0 {
				drmSkipped++
				continue
			}
```

After `format.TargetDurationSec` is set, add:

```go
			if at, ok := f["audioTrack"].(map[string]any); ok {
				format.AudioTrackID = getStr(at, "id")
				format.AudioTrackName = getStr(at, "displayName")
				format.AudioIsDefault = getBool(at, "audioIsDefault")
			}
			format.IsDrc = getBool(f, "isDrc")
```

and after the outer loop, before `return formats`:

```go
	if drmSkipped > 0 {
		p.logger.Warn("[PlayerApi] skipped DRM-protected formats",
			"count", drmSkipped,
			"note", "a YouTube account experiment applies DRM to all videos on the tv client — yt-dlp issue #12563")
	}
```

Replace `deduplicateFormats`'s key:

```go
// formatKey is yt-dlp's get_stream_id (_video.py:3396-3397): itag alone is not
// an identity. A dubbed video lists several itag-140 entries from ONE client
// differing only by audioTrack.id, and the DRC rendition of a track is a
// separate stream, not a variant — keying on itag alone kept whichever was
// listed first and silently discarded the rest.
type formatKey struct {
	itag         int
	audioTrackID string
	isDrc        bool
}

func deduplicateFormats(pool []Format) []Format {
	byStream := make(map[formatKey]Format)
	for _, f := range pool {
		if f.URL == "" {
			continue
		}
		key := formatKey{itag: f.Itag, audioTrackID: f.AudioTrackID, isDrc: f.IsDrc}
		existing, exists := byStream[key]
		if !exists {
			byStream[key] = f
			continue
		}
		fAuth := authLevelOf(&f)
		eAuth := authLevelOf(&existing)
		if fAuth < eAuth {
			byStream[key] = f
		}
	}

	result := make([]Format, 0, len(byStream))
	for _, f := range byStream {
		result = append(result, f)
	}
	// Ordered by the whole key so the output is deterministic across map
	// iterations — the itag-only sort stopped being total the moment one itag
	// could appear more than once.
	slices.SortFunc(result, func(a, b Format) int {
		if c := cmp.Compare(a.Itag, b.Itag); c != 0 {
			return c
		}
		if c := cmp.Compare(a.AudioTrackID, b.AudioTrackID); c != 0 {
			return c
		}
		return cmp.Compare(boolOrder(a.IsDrc), boolOrder(b.IsDrc))
	})
	return result
}

// boolOrder gives false < true, so the clean rendition sorts before its DRC twin.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 5: Run the parse tests green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParseFormats|TestDeduplicateFormats' ./internal/youtube/
```
Expected: PASS.

- [ ] **Step 6: Write the failing selection test**

Append to `internal/youtube/format_selector_test.go`:

```go
// TestSelectBestAudioPrefersTheOriginalNonDRCTrack pins upstream's language
// preference (_video.py:3277-3291): original (10) > default (5) > unlabelled
// (-1) > descriptive (-10), with the clean rendition beating its DRC twin.
// Track identity is decided BEFORE codec and bitrate: archiving the wrong
// LANGUAGE is a worse outcome than archiving a slightly worse encoder.
//
// Mutants this kills:
//   - no track score at all             → the first-listed ja dub wins
//   - the score applied after bitrate   → the higher-bitrate dub wins
//   - descriptive not penalised         → the descriptive track wins
//   - DRC not penalised                 → the DRC twin wins
func TestSelectBestAudioPrefersTheOriginalNonDRCTrack(t *testing.T) {
	mk := func(id, name string, def, drc bool, bitrate int, url string) Format {
		return Format{Itag: 140, URL: url, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
			Bitrate: bitrate, AudioTrackID: id, AudioTrackName: name, AudioIsDefault: def, IsDrc: drc}
	}
	formats := []Format{
		mk("ja.3", "Japanese", true, false, 200000, "https://x/ja-default"),
		mk("en.9", "English descriptive", false, false, 300000, "https://x/en-desc"),
		mk("en.4", "English original", false, true, 256000, "https://x/en-orig-drc"),
		mk("en.4", "English original", false, false, 128000, "https://x/en-orig"),
	}

	got := SelectBestFormats(formats, 1080, true)
	if got.Audio == nil {
		t.Fatal("no audio format selected")
	}
	if got.Audio.URL != "https://x/en-orig" {
		t.Errorf("selected %q (track %q, drc=%v), want the clean original https://x/en-orig",
			got.Audio.URL, got.Audio.AudioTrackName, got.Audio.IsDrc)
	}
}
```

`SelectBestFormats(formats, maxResolution, prefer60fps)` is the exported wrapper over `selectBestFormatsImpl` (`format_selector.go:49`) and is the entry point the file's existing tests already use; `SelectedFormats` carries `.Video` and `.Audio`.

- [ ] **Step 7: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestSelectBestAudioPrefers ./internal/youtube/
```
Expected: FAIL — the ja default (listed first, nothing to unseat it) is selected.

- [ ] **Step 8: Rank the tracks**

In `internal/youtube/format_selector.go`, add above `selectBestFormatsImpl`:

```go
// audioTrackScore ranks one audio format's TRACK against the others. Ported
// from yt-dlp's get_language_code_and_preference (_video.py:3277-3291), whose
// constants are ORIGINAL_LANG_VALUE = 10 and DEFAULT_LANG_VALUE = 5, with
// 'descriptive' at -10 and everything else at -1.
//
// The DRC penalty is Moombox's own and is applied as a TIE-BREAK inside a
// track, never across tracks: a loudness-normalised rendition is a processed
// copy of the same audio, so given both we archive the untouched one.
func audioTrackScore(f *Format) int {
	name := strings.ToLower(f.AudioTrackName)
	score := -1
	switch {
	case strings.Contains(name, "descriptive"):
		score = -10
	case strings.Contains(name, "original"):
		score = 10
	case f.AudioIsDefault:
		score = 5
	}
	// ×2 so the DRC penalty can never move a format across a track boundary.
	score *= 2
	if f.IsDrc {
		score--
	}
	return score
}
```

and make it the FIRST audio tie-break — in the audio branch of `selectBestFormatsImpl`, immediately after the `if bestAudio == nil { … }` block:

```go
			// Track identity first: archiving the wrong LANGUAGE is worse than
			// archiving a lesser encoder, so this outranks codec and bitrate.
			fTrack := audioTrackScore(f)
			bestTrack := audioTrackScore(bestAudio)
			if fTrack != bestTrack {
				if fTrack > bestTrack {
					bestAudio = f
					bestAudioCodecScore = cachedAudioCodecScore(f.MimeType)
				}
				continue
			}
```

- [ ] **Step 9: Run the package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/ ./internal/worker/
```
Expected: PASS.

- [ ] **Step 10: Correct the spec prose**

In `docs/spec/platform-services.md`, replace step 10 of the authenticated flow:

```markdown
10. **Deduplicate formats** -- Across all collected format pools, deduplicate by yt-dlp's stream identity `(itag, audioTrack.id, isDrc)` (`get_stream_id`, `_video.py:3396-3397`) — NOT by itag alone: one client lists several itag-140 entries for a dubbed video that differ only by track, and a DRC rendition is a separate stream rather than a variant. When the same stream appears from multiple clients, keep the one with the lowest auth level. DRM-protected formats (`drmFamilies`) never reach the pool: they are dropped in `parseFormats` with a counted warning, because an account-level experiment applies DRM to every video on the tv client (yt-dlp issue #12563) and muxing encrypted samples yields an unplayable archive.
```

and add to the Format Selection section (beside the codec-order prose), a sentence naming the new rule:

```markdown
Audio track identity is decided before codec and bitrate: `audioTrackScore` ports yt-dlp's `get_language_code_and_preference` (original > YouTube's default > unlabelled > descriptive), and within one track the clean rendition beats its `isDrc` (loudness-normalised) twin.
```

- [ ] **Step 11: Gate the docs and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/youtube/
```

```bash
git add internal/youtube/types.go internal/youtube/player_api_parsing.go \
        internal/youtube/format_selector.go internal/youtube/player_api_parsing_test.go \
        internal/youtube/format_selector_test.go docs/spec/platform-services.md
git commit -m "fix(youtube): key formats on upstream's stream identity; drop DRM

drmFamilies, isDrc and audioTrack were never read and dedup keyed on itag
alone, so a TV-tier DRM format outranked WEB's clean twin (encrypted samples
muxed) and a dubbed video kept whichever itag-140 was listed first. parse
now carries the track fields, DRM formats are dropped with a counted warning
(yt-dlp #12563), dedup keys on (itag, audioTrackID, isDrc) per get_stream_id,
and audio selection ranks tracks original > default > unlabelled >
descriptive with the clean rendition beating its DRC twin. Report #12.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/types.go internal/youtube/player_api_parsing.go \
     internal/youtube/format_selector.go internal/youtube/player_api_parsing_test.go \
     internal/youtube/format_selector_test.go docs/spec/platform-services.md
```

---

## Task 5: Classify every age-gate shape (row #13 / YOUTUBE-6)

`AGE_CHECK_REQUIRED` and `UNPLAYABLE` + "inappropriate for some users" fall through to `PlayabilityUnknown`, so the web_embedded age bypass (`player_api_strategy.go:294` authenticated, `:444` public — both gate literally on `PlayabilityAgeRestricted`) never fires for those shapes. Verifier ruling, which overrides the area report: **the reason-substring match is the load-bearing half**, because upstream's `_is_agegated` status entries are lower-case (`'age_check_required'`) and are substring-matched against the raw upper-case `status`, so in practice upstream detects these through the REASON. Upstream: `_video.py:2893-2904`, which checks `desktopLegacyAgeGateReason` first and then the substrings `'confirm your age'`, `'age-restricted'`, `'inappropriate'`.

**Files:**
- Modify: `internal/youtube/player_api_parsing.go` (`parsePlayabilityStatus`)
- Modify: `internal/youtube/player_api_parsing_test.go`
- Modify: `docs/spec/platform-services.md` (the playability table)

**Interfaces:**
- Produces: `func isAgeGateReason(reasonLower string) bool`, `func hasDesktopLegacyAgeGate(status map[string]any) bool`.
- Consumes (Tasks 3–4): nothing beyond the file they share.

- [ ] **Step 1: Write the failing test**

Append to `internal/youtube/player_api_parsing_test.go`:

```go
// TestParsePlayabilityStatusRecognisesEveryAgeGateShape ports yt-dlp's
// _is_agegated (_video.py:2893-2904). The bypass gate downstream
// (player_api_strategy.go:294 and :444) matches PlayabilityAgeRestricted
// LITERALLY, so an "unknown" verdict never reaches the web_embedded age path
// at all.
//
// The reason substrings are the load-bearing half: upstream's status entries
// are lower-case and substring-matched against the raw upper-case `status`, so
// upstream in practice recognises these responses through their REASON.
//
// Mutants this kill:
//   - the AGE_CHECK_REQUIRED arm dropped        → that row reports "unknown"
//   - the reason substrings dropped             → the inappropriate/confirm rows report "unknown"
//   - desktopLegacyAgeGateReason not consulted  → that row reports "unknown"
//   - the age match placed above the upcoming
//     check, or above the members-only arm      → the last two rows regress
func TestParsePlayabilityStatusRecognisesEveryAgeGateShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   PlayabilityError
	}{
		{"AGE_CHECK_REQUIRED", `{"status": "AGE_CHECK_REQUIRED", "reason": "Sign in to confirm your age"}`, PlayabilityAgeRestricted},
		{"AGE_VERIFICATION_REQUIRED", `{"status": "AGE_VERIFICATION_REQUIRED", "reason": "This video may be inappropriate for some users."}`, PlayabilityAgeRestricted},
		{"UNPLAYABLE inappropriate", `{"status": "UNPLAYABLE", "reason": "This video may be inappropriate for some users."}`, PlayabilityAgeRestricted},
		{"UNPLAYABLE age-restricted", `{"status": "UNPLAYABLE", "reason": "This video is age-restricted and can only be watched on YouTube."}`, PlayabilityAgeRestricted},
		{"LOGIN_REQUIRED confirm your age", `{"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm your age"}`, PlayabilityAgeRestricted},
		{"desktopLegacyAgeGateReason", `{"status": "UNPLAYABLE", "reason": "", "desktopLegacyAgeGateReason": 1}`, PlayabilityAgeRestricted},

		// Regressions the new match must NOT cause.
		{"members only stays members only", `{"status": "LOGIN_REQUIRED", "reason": "Join this channel to get access to members-only content"}`, PlayabilityMembersOnly},
		{"upcoming stays ok", `{"status": "LIVE_STREAM_OFFLINE", "reason": "Premieres in 3 hours"}`, PlayabilityOK},
		{"private stays private", `{"status": "UNPLAYABLE", "reason": "This video is private."}`, PlayabilityPrivate},
		{"plain unplayable stays unknown", `{"status": "UNPLAYABLE", "reason": "Playback on other websites has been disabled"}`, PlayabilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parsePlayabilityStatus(decodePlayerJSON(t, tc.status))
			if got != tc.want {
				t.Errorf("parsePlayabilityStatus(%s) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestParsePlayabilityStatusRecognises ./internal/youtube/
```
Expected: FAIL on the `AGE_CHECK_REQUIRED`, `UNPLAYABLE inappropriate`, `UNPLAYABLE age-restricted` and `desktopLegacyAgeGateReason` rows, each reporting `"unknown"`.

- [ ] **Step 3: Implement upstream's `_is_agegated`**

In `internal/youtube/player_api_parsing.go`, add beside `isUpcomingFromPlayability`:

```go
// ageGateReasons are yt-dlp's AGE_GATE_REASONS reason substrings
// (_video.py:2899-2901). They are the load-bearing half of the match:
// upstream's status entries in the same tuple are lower-case and are
// substring-matched against the raw upper-case `status`, so upstream in
// practice detects an age gate through the REASON text.
var ageGateReasons = []string{"confirm your age", "age-restricted", "inappropriate"}

// isAgeGateReason reports whether a playability reason (already lower-cased)
// names an age gate.
func isAgeGateReason(reasonLower string) bool {
	for _, r := range ageGateReasons {
		if strings.Contains(reasonLower, r) {
			return true
		}
	}
	return false
}

// hasDesktopLegacyAgeGate mirrors upstream's first test,
// `traverse_obj(player_response, ('playabilityStatus',
// 'desktopLegacyAgeGateReason'))` (_video.py:2894-2895) — a TRUTHINESS test,
// so a key present but zero/empty/false is not an age gate.
func hasDesktopLegacyAgeGate(status map[string]any) bool {
	switch v := status["desktopLegacyAgeGateReason"].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	default:
		return true
	}
}
```

In `parsePlayabilityStatus`, insert immediately after the `isUpcomingFromPlayability` block and before `switch statusCode {`:

```go
	// Age gates, ported from _is_agegated (_video.py:2893-2904). This runs
	// BEFORE the status switch because the shapes it catches are spread across
	// three different status codes (AGE_CHECK_REQUIRED, UNPLAYABLE,
	// LOGIN_REQUIRED) — and AFTER the upcoming check, because a waiting room
	// is not an error and must never be classified as one.
	//
	// It cannot steal a members-only verdict: none of the three substrings
	// appears in YouTube's membership reason text, which says "Join this
	// channel to get access to members-only content".
	if hasDesktopLegacyAgeGate(status) || isAgeGateReason(reasonLower) {
		return PlayabilityAgeRestricted, reason
	}
```

and add the missing status arm beside its twin:

```go
	case "AGE_VERIFICATION_REQUIRED", "AGE_CHECK_REQUIRED":
		return PlayabilityAgeRestricted, reason
```

Delete the now-dead `if strings.Contains(reasonLower, "age") { return PlayabilityAgeRestricted, reason }` inside the `LOGIN_REQUIRED` arm ONLY if the test above still passes without it; upstream has no bare-"age" rule and the substring list is stricter, so removing it is the parity move. If any existing test in the package depends on a bare "age" reason, keep the arm and say so in the commit body.

- [ ] **Step 4: Run it green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
```
Expected: PASS.

- [ ] **Step 5: Correct the playability table**

In `docs/spec/platform-services.md`, replace the `AGE_VERIFICATION_REQUIRED` row and add the reason rows, so the table reads (in this order — the table documents evaluation order):

```markdown
| Status Code | Condition | PlayabilityError |
|-------------|-----------|-----------------|
| `OK` | -- | `ok` |
| `LIVE_STREAM_OFFLINE` | -- | `ok` (upcoming, not an error) |
| `UNPLAYABLE` + "live event will begin" | -- | `ok` (upcoming) |
| any status | `desktopLegacyAgeGateReason` is truthy | `age_restricted` |
| any status | reason contains "confirm your age" / "age-restricted" / "inappropriate" | `age_restricted` |
| `AGE_VERIFICATION_REQUIRED`, `AGE_CHECK_REQUIRED` | -- | `age_restricted` |
| `LOGIN_REQUIRED` + "member"/"join" in reason | -- | `members_only` |
| `LOGIN_REQUIRED` | -- | `login_required` |
| `UNPLAYABLE` + "member" in reason | -- | `members_only` |
| `UNPLAYABLE` + "private" in reason | -- | `private` |
| `UNPLAYABLE` + "country"/"region"/"not available in your" | -- | `region_blocked` |
| `UNPLAYABLE` + "unavailable" | -- | `unavailable` |
| `ERROR` + "private"/"unavailable" | -- | `unavailable` |
| Anything else | -- | `unknown` |

The two age rows sit above the status switch because the shapes they catch are spread across `AGE_CHECK_REQUIRED`, `UNPLAYABLE` and `LOGIN_REQUIRED`, and below the upcoming rows because a waiting room is not an error. They port yt-dlp's `_is_agegated` (`_video.py:2893-2904`); the reason substrings are the load-bearing half, since upstream's lower-case status entries are substring-matched against the raw upper-case `status` and so only ever match through the reason. The verdict matters because the web_embedded age bypass gates literally on `age_restricted`.
```

- [ ] **Step 6: Gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/youtube/
```

```bash
git add internal/youtube/player_api_parsing.go internal/youtube/player_api_parsing_test.go \
        docs/spec/platform-services.md
git commit -m "fix(youtube): recognise every age-gate shape yt-dlp does

AGE_CHECK_REQUIRED and 'inappropriate for some users' classified as unknown,
so the web_embedded age bypass — which gates literally on age_restricted —
never fired for them. Ports _is_agegated: desktopLegacyAgeGateReason plus the
'confirm your age' / 'age-restricted' / 'inappropriate' reason substrings (the
load-bearing half upstream actually matches on), evaluated after the upcoming
check and before the status switch. Report #13 / YOUTUBE-6.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/player_api_parsing.go internal/youtube/player_api_parsing_test.go \
     docs/spec/platform-services.md
```

---

## Task 6: Quality-monitor probe via TV-with-cookies (row #26 / YOUTUBE-3, owner decision O-H)

> **O-H (verbatim):** The 30 s quality monitor probes auth-walled/recovery streams via `ProbeVideoStatusAuthenticated` (one TV-with-cookies call); the full `GetVideoInfo` cascade only when that returns neither DASH nor split-adaptive; live-check once.

The probe runs the FULL authenticated cascade (a 1–5 MB cookied watch page plus 3–7 player calls) every 30 s for auth-walled streams — and, per the verifier's correction, the **dominant** path is the other branch: `orchestrator_youtube.go:694`, where any public stream whose android_vr probe returned neither DASH nor split-adaptive pays the wasted probe *plus* the whole cascade, every tick. `ProbeVideoStatusAuthenticated` (`player_api_strategy.go:87-93`: one TV_DOWNGRADED call with cookies, no watch page, no STS, no POT) already returns the pool the probe selects from.

**Files:**
- Modify: `internal/worker/orchestrator_youtube.go`
- Create: `internal/worker/quality_probe_test.go`
- Modify: `docs/spec/architecture.md` (the `runLiveStreamDownload` bullet at `:367` — cross-arc, called out in Global Constraints)

**Interfaces:**
- Produces: `type youtubeProbeClient interface { ProbeVideoStatus / ProbeVideoStatusAuthenticated / GetVideoInfo (ctx, videoID) (*youtube.VideoInfo, error) }`; `func probeVideoInfo(ctx context.Context, yt youtubeProbeClient, videoID string, requiresAuth bool) (*youtube.VideoInfo, error)`.
- Consumes (Task 3): `youtube.VideoIDMismatchError` reaches this path as an ordinary error; no special handling (see Task 3 Step 9).

- [ ] **Step 0: Run the live check O-H asks for, and record the answer**

O-H's "live-check once" is a VERIFICATION obligation, not a loop bound: confirm that TV-with-cookies at `sts = 0` carries `adaptiveFormats` for a members-only live stream, because that is the response the probe will now select from.

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_YT_TEST=1 \
  go test -count=1 -v -run 'TestLive' ./internal/youtube/
```

Read `internal/youtube/extraction_live_test.go` first: `TestLiveAuthenticatedAccountProbe` is the authenticated case and it SKIPS when no cookie file is configured (it has in every prior arc). Two acceptable outcomes, and the implementer must write one of them into the commit body:

1. A members-only-shaped live video is reachable with the configured cookies → add a temporary (NOT committed) subtest that calls `svc.ProbeVideoStatusAuthenticated(ctx, <id>)` and log `len(info.Formats)`, `HasSplitAdaptiveFormats(info.Formats)` and `info.DashManifestURL != ""`. Record the numbers.
2. It is not reachable (no cookie file, or no members-only live to point at) → record exactly that, and note that the fallback in Step 3 is what makes the unknown safe: a TV probe that yields neither DASH nor split-adaptive falls through to the full cascade, which is today's behaviour. **The fallback is non-negotiable for this reason** — do not make it conditional on `!requiresAuth`.

- [ ] **Step 1: Write the failing probe test**

Create `internal/worker/quality_probe_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeProbeClient counts which of the three entry points the quality probe
// chose. Nothing here touches the network: the row is about REQUEST SHAPE, so
// the assertion has to be on the calls themselves.
type fakeProbeClient struct {
	probe     int
	probeAuth int
	full      int

	probeInfo     *youtube.VideoInfo
	probeAuthInfo *youtube.VideoInfo
	fullInfo      *youtube.VideoInfo
	fullErr       error
}

func (f *fakeProbeClient) ProbeVideoStatus(context.Context, string) (*youtube.VideoInfo, error) {
	f.probe++
	return f.probeInfo, nil
}

func (f *fakeProbeClient) ProbeVideoStatusAuthenticated(context.Context, string) (*youtube.VideoInfo, error) {
	f.probeAuth++
	return f.probeAuthInfo, nil
}

func (f *fakeProbeClient) GetVideoInfo(context.Context, string) (*youtube.VideoInfo, error) {
	f.full++
	return f.fullInfo, f.fullErr
}

// splitAdaptive is a format pool the manifest-free path can address: split
// video + audio, no contentLength.
func splitAdaptive() []youtube.Format {
	w, h := 1920, 1080
	return []youtube.Format{
		{Itag: 137, URL: "https://x/v", MimeType: `video/mp4; codecs="avc1.640028"`, Width: &w, Height: &h},
		{Itag: 140, URL: "https://x/a", MimeType: `audio/mp4; codecs="mp4a.40.2"`},
	}
}

// TestProbeVideoInfoUsesTheCheapAuthenticatedProbe is owner decision O-H. The
// 30 s monitor used to run the FULL authenticated cascade — a 1-5 MB cookied
// watch page plus 3-7 player calls — for every auth-walled stream, which at
// this cadence is ~120 watch pages and >=360 player calls per hour per job.
// ProbeVideoStatusAuthenticated is one TV-with-cookies call and already
// returns the pool the probe selects from.
//
// Mutants this kills:
//   - requiresAuth still routed to GetVideoInfo  → full == 1, probeAuth == 0
//   - requiresAuth routed to the COOKIELESS probe → probe == 1 (android_vr 401s
//     on members-only content, so this must not happen)
func TestProbeVideoInfoUsesTheCheapAuthenticatedProbe(t *testing.T) {
	f := &fakeProbeClient{probeAuthInfo: &youtube.VideoInfo{Formats: splitAdaptive()}}

	info, err := probeVideoInfo(context.Background(), f, "vid", true)
	if err != nil {
		t.Fatalf("probeVideoInfo: %v", err)
	}
	if info == nil || len(info.Formats) != 2 {
		t.Fatalf("info = %+v, want the probe's two formats", info)
	}
	if f.probeAuth != 1 || f.full != 0 || f.probe != 0 {
		t.Errorf("calls: probeAuth=%d probe=%d full=%d; want exactly one authenticated probe",
			f.probeAuth, f.probe, f.full)
	}
}

// TestProbeVideoInfoFallsBackOnceForBothProbeKinds is the other half of O-H
// and the verifier's correction: the DOMINANT waste path is the PUBLIC stream
// whose android_vr probe returns neither DASH nor split-adaptive, which paid
// the probe AND the whole cascade every tick. The fallback stays — it is what
// makes the authenticated probe safe when TV-with-cookies turns out to carry
// no adaptiveFormats — but it fires ONCE per tick, for either probe kind.
//
// Mutants this kills:
//   - the fallback gated on !requiresAuth again  → the auth subtest sees full == 0
//   - the fallback looping or retrying           → full > 1
//   - the fallback firing when the probe already
//     had split-adaptive formats                 → covered by the test above
func TestProbeVideoInfoFallsBackOnceForBothProbeKinds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		requiresAuth bool
	}{
		{"public recovery path", false},
		{"authenticated probe with no adaptive formats", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			empty := &youtube.VideoInfo{} // no DASH URL, no formats
			f := &fakeProbeClient{
				probeInfo:     empty,
				probeAuthInfo: empty,
				fullInfo:      &youtube.VideoInfo{Formats: splitAdaptive()},
			}

			info, err := probeVideoInfo(context.Background(), f, "vid", tc.requiresAuth)
			if err != nil {
				t.Fatalf("probeVideoInfo: %v", err)
			}
			if len(info.Formats) != 2 {
				t.Fatalf("info = %+v, want the fallback's formats", info)
			}
			if f.full != 1 {
				t.Errorf("GetVideoInfo called %d times, want exactly 1", f.full)
			}
		})
	}
}

// TestProbeVideoInfoSurfacesANilInfoAsAnError keeps the defensive arm that
// exists because GetVideoInfo / ProbeVideoStatus can return (nil, nil) during
// a context-cancel shutdown race; the next dereference would panic the
// monitor goroutine.
//
// Mutants this kills: the nil guard removed → panic instead of an error.
func TestProbeVideoInfoSurfacesANilInfoAsAnError(t *testing.T) {
	f := &fakeProbeClient{}
	if _, err := probeVideoInfo(context.Background(), f, "vid", false); err == nil {
		t.Fatal("a nil info with no error was accepted")
	}

	g := &fakeProbeClient{probeInfo: &youtube.VideoInfo{}, fullErr: errors.New("boom")}
	if _, err := probeVideoInfo(context.Background(), g, "vid", false); err == nil {
		t.Fatal("a failing fallback was accepted")
	}
}
```

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestProbeVideoInfo ./internal/worker/
```
Expected: FAIL to compile — `undefined: probeVideoInfo`.

- [ ] **Step 3: Extract and rewrite the probe**

In `internal/worker/orchestrator_youtube.go`, add above `buildYouTubeProbeFn`:

```go
// youtubeProbeClient is the narrow slice of *youtube.Service the quality probe
// uses. Named as an interface so the probe's ROUTING — which is all owner
// decision O-H changes — is testable without a network round trip;
// *youtube.Service satisfies it.
type youtubeProbeClient interface {
	ProbeVideoStatus(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	ProbeVideoStatusAuthenticated(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	GetVideoInfo(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
}

// probeVideoInfo fetches the VideoInfo one quality-monitor tick selects from.
//
// Owner decision O-H. Before it, an auth-walled stream ran the FULL
// authenticated cascade every 30 s — a 1-5 MB cookied watch page plus three to
// seven player calls, roughly 120 pages and 360+ player calls per hour per job
// — and the dominant waste was the OTHER branch: a public stream whose
// cookieless android_vr probe returned neither DASH nor split-adaptive paid
// that probe AND the whole cascade, every tick.
//
// Both kinds now start cheap: ANDROID_VR when nothing is walled (cookieless,
// no POT, no watch page), TV_DOWNGRADED-with-cookies when something is
// (ProbeVideoStatusAuthenticated — one player call, no watch page, no STS, no
// POT; android_vr would 401 on members-only content).
//
// The full cascade remains as a ONE-SHOT fallback for either kind, fired only
// when the cheap probe produced neither a DASH manifest nor a split-adaptive
// pool — i.e. nothing the monitor could select from. That single retry is what
// makes the cheap authenticated probe safe: if TV-with-cookies at sts = 0 ever
// stops carrying adaptiveFormats for some shape, the tick still resolves,
// exactly as it does today, at the old cost.
//
// The monitor only needs the format pool in memory; nothing on this path
// consumes the watch-page metadata the cascade used to refresh (O-H's
// acknowledged, visible change: less log volume and a smaller request
// signature toward YouTube).
func probeVideoInfo(ctx context.Context, yt youtubeProbeClient, videoID string, requiresAuth bool) (*youtube.VideoInfo, error) {
	var info *youtube.VideoInfo
	var err error
	if requiresAuth {
		info, err = yt.ProbeVideoStatusAuthenticated(ctx, videoID)
	} else {
		info, err = yt.ProbeVideoStatus(ctx, videoID)
	}
	if err != nil {
		return nil, err
	}
	// Defensive: both probes can return (nil, nil) when every client fails
	// without surfacing a hard error (notably during a context-cancel shutdown
	// race). Treat that as a transient probe error rather than letting the
	// next dereference panic the monitor goroutine.
	if info == nil {
		return nil, fmt.Errorf("probe returned nil info without error")
	}

	if info.DashManifestURL != "" || HasManifestlessDashFormats(info.Formats) {
		return info, nil
	}

	info, err = yt.GetVideoInfo(ctx, videoID)
	if err != nil {
		return nil, fmt.Errorf("probe fallback to the full fetch: %w", err)
	}
	if info == nil {
		return nil, fmt.Errorf("probe fallback returned nil info")
	}
	return info, nil
}
```

Then replace the head of the closure `buildYouTubeProbeFn` returns — everything from `var info *youtube.VideoInfo` down to the end of the `:694` recovery block — with:

```go
	return func(ctx context.Context) (*QualityInfo, error) {
		info, err := probeVideoInfo(ctx, jobCtx.YT, jobCtx.Job.VideoID, requiresAuth)
		if err != nil {
			return nil, err
		}
```

and update the function's doc comment:

```go
// buildYouTubeProbeFn creates a quality probe function for YouTube streams.
// The probe re-fetches the format pool and selects the best stream, returning
// the quality that would be selected under current preferences.
//
// requiresAuth: when true the probe uses ProbeVideoStatusAuthenticated
// (TV_DOWNGRADED with cookies) — required for members-only, age-restricted and
// login-required streams, where the cookieless ANDROID_VR probe 401s. When
// false it uses ProbeVideoStatus (ANDROID_VR, cookieless, no POT). See
// probeVideoInfo for the one-shot full-fetch fallback behind both, and owner
// decision O-H for why the full cascade is no longer the FIRST call on either.
```

- [ ] **Step 4: Run the worker package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
```
Expected: PASS. If `fmt` is not already imported by `orchestrator_youtube.go`, add it.

- [ ] **Step 5: Correct `architecture.md`**

Replace bullet 1 of "**Live stream download loop (`runLiveStreamDownload`):**" in `docs/spec/architecture.md`:

```markdown
1. Starts quality monitor (30-second probe interval) if user hasn't manually selected itags. Both probe kinds are ONE player call (`probeVideoInfo`, `internal/worker/orchestrator_youtube.go`): `ProbeVideoStatus` (ANDROID_VR, cookieless, no POT, no watch page) for public streams, and `ProbeVideoStatusAuthenticated` (TV_DOWNGRADED with cookies, no watch page, no STS, no POT) for members-only / age-restricted / login-required ones, where the cookieless probe would 401. If the cheap probe returns neither a `DashManifestURL` nor a split-adaptive format pool — nothing the monitor can select from — it falls back ONCE per tick to the full `GetVideoInfo` cascade, for either kind. Before owner decision O-H the authenticated case ran that cascade on EVERY tick (a 1-5 MB cookied watch page plus three to seven player calls, ~120 pages/hour/job), and so did any public stream whose android_vr probe came back without addressable formats.
```

This is a cross-arc edit — one bullet, in a section no Arc E or Arc M row names. See the call-out table in Global Constraints.

- [ ] **Step 6: Gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/worker/
```

```bash
git add internal/worker/orchestrator_youtube.go internal/worker/quality_probe_test.go \
        docs/spec/architecture.md
git commit -m "perf(worker): probe stream quality with one player call (O-H)

The 30 s quality monitor ran the full authenticated cascade for every
auth-walled stream, and — the dominant path the verifier identified — for
every public stream whose android_vr probe returned neither DASH nor
split-adaptive: a 1-5 MB cookied watch page plus 3-7 player calls, ~120
pages and 360+ player calls per hour per job. probeVideoInfo now starts with
ProbeVideoStatusAuthenticated (one TV-with-cookies call) or ProbeVideoStatus,
and falls back to GetVideoInfo exactly once per tick when neither DASH nor a
split-adaptive pool came back. Nothing on this path consumed the watch-page
metadata the cascade refreshed. Report #26 / YOUTUBE-3, owner decision O-H.

<the live-check answer from Step 0 goes here, verbatim>

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/worker/orchestrator_youtube.go internal/worker/quality_probe_test.go \
     docs/spec/architecture.md
```

---

## Task 7: Waiting-room polls (row #27 / YOUTUBE-4, owner decision O-I)

> **O-I (verbatim):** Waiting-room polls: reuse the first ANDROID_VR result in `tryCookielessFallbacks` (never fetch it twice) AND skip WEB_CREATOR/VISIONOS/ANDROID_VR when the TV authority says StreamUpcoming with PlayabilityOK. android_vr stays in the roster everywhere else.

On a TV `upcoming` verdict the cascade still runs WEB_CREATOR, VISIONOS and ANDROID_VR — and ANDROID_VR **twice**: the `:263` DASH fallback fires for `StreamUpcoming`, then `tryCookielessFallbacks` re-fetches it at `:687` because VISIONOS fails `hasAdequateFormats` on an upcoming stream so the loop never breaks. That is 1 page + 7 player calls, repeated every 30 s for the whole waiting-room window (the android_vr probe reads YouTube's slate as live in the imminent band, which the codebase documents at `stream_processor_youtube.go:352`).

**Files:**
- Modify: `internal/youtube/player_api_strategy.go`
- Modify: `internal/youtube/player_api_strategy_test.go`
- Modify: `docs/spec/platform-services.md` (authenticated flow steps 7–8, public flow steps 3–4)

**Interfaces:**
- Consumes (Task 3): `fetchWatchPage`, `mismatchTally`, `finishExtraction`.
- Produces: `type cookielessPrefetch struct { clientName string; result *VideoInfo; err error; pooled bool }`; `tryCookielessFallbacks(ctx, videoID, visitorData string, formatPool *[]Format, tally *mismatchTally, prefetched *cookielessPrefetch) *VideoInfo`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/youtube/player_api_strategy_test.go`:

```go
// upcomingTVBody is what TV answers during a waiting room: a healthy
// playability verdict, no formats, isUpcoming set.
const upcomingTVBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isUpcoming": true, "isLiveContent": true},
	"streamingData": {}
}`

// TestUpcomingPollSkipsTheFormatHuntingFallbacks is owner decision O-I. An
// upcoming stream has no formats BY DEFINITION, so the three clients the
// cascade consults to go find some cannot succeed — yet on every 30 s
// waiting-room poll it fetched WEB_CREATOR, VISIONOS and ANDROID_VR, the last
// of them TWICE.
//
// android_vr stays in the roster everywhere else; this rule is about one
// verdict, StreamUpcoming with PlayabilityOK from the TV authority.
//
// Mutants this kills:
//   - the short-circuit dropped        → 62/101/28 appear in the call list
//   - the short-circuit not requiring
//     PlayabilityOK                    → covered by the members-only subtest
func TestUpcomingPollSkipsTheFormatHuntingFallbacks(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, upcomingTVBody},
		"56":  {http.StatusOK, upcomingTVBody},
		"62":  {http.StatusOK, adequateOKBody},
		"101": {http.StatusOK, adequateOKBody},
		"28":  {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	if info.StreamStatus != StreamUpcoming {
		t.Fatalf("StreamStatus = %q, want upcoming", info.StreamStatus)
	}
	for _, c := range tr.calls {
		if c == "62" || c == "101" || c == "28" {
			t.Errorf("format-hunting client %s was called on an upcoming TV verdict: calls=%v", c, tr.calls)
		}
	}
}

// TestUpcomingWithAWalledVerdictStillRunsTheFallbacks: the short-circuit is
// gated on PlayabilityOK, because an upcoming MEMBERS-ONLY stream is exactly
// the case the fallback chain exists for.
//
// Mutant this kills: short-circuiting on StreamUpcoming alone → no fallback
// client is called and a members-only waiting room loses its chain.
func TestUpcomingWithAWalledVerdictStillRunsTheFallbacks(t *testing.T) {
	stubWatchPage(t)
	const walled = `{
		"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you are not a bot"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isUpcoming": true, "isLiveContent": true},
		"streamingData": {}
	}`
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, walled},
		"101": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	if _, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567"); err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	var sawVisionOS bool
	for _, c := range tr.calls {
		if c == "101" {
			sawVisionOS = true
		}
	}
	if !sawVisionOS {
		t.Errorf("a login-required upcoming stream skipped the cookieless chain: calls=%v", tr.calls)
	}
}

// TestCookielessFallbacksReuseAPrefetchedClient is O-I's no-behaviour-change
// half: the caller that already fetched ANDROID_VR for its DASH manifest hands
// the result in rather than paying for it twice.
//
// Mutants this kills:
//   - the prefetch ignored          → calls == [101 28], two round trips
//   - the prefetch re-pooled        → the pool carries the same format twice
func TestCookielessFallbacksReuseAPrefetchedClient(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"101": {http.StatusOK, audioOnlyOKBody}, // inadequate, so the chain goes on
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := NewPlayerAPI(nil, noopLogger{})
	var pool []Format
	prefetchedVR, prefetchedErr := p.fetchWithCookielessClient(context.Background(), "test1234567", "vd", constants.AndroidVRClient)
	_ = prefetchedErr
	// Simulate the :263 caller: it pooled what it fetched.
	vrFormats := 0
	if prefetchedVR != nil {
		collectFormats(&pool, prefetchedVR.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
		vrFormats = len(prefetchedVR.Formats)
	}
	before := len(tr.calls)

	tally := &mismatchTally{}
	p.tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, tally,
		&cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName, result: prefetchedVR, err: prefetchedErr, pooled: true})

	for _, c := range tr.calls[before:] {
		if c == "28" {
			t.Errorf("ANDROID_VR was fetched again although a result was handed in: calls=%v", tr.calls)
		}
	}
	vrInPool := 0
	for _, f := range pool {
		if f.Source == "android_vr" || f.Source == "android_vr_dash_fallback" {
			vrInPool++
		}
	}
	if vrInPool != vrFormats {
		t.Errorf("android_vr formats in the pool = %d, want %d — a pooled prefetch must not be collected twice", vrInPool, vrFormats)
	}
}
```

Add `"github.com/vampiricwulf/Moombox/internal/constants"` to the test file's imports if absent.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestUpcoming|TestCookielessFallbacksReuse' ./internal/youtube/
```
Expected: FAIL to compile — `undefined: cookielessPrefetch`; and once that is added, the first test fails on the three extra calls.

- [ ] **Step 3: Teach `tryCookielessFallbacks` to reuse a prefetched client**

In `internal/youtube/player_api_strategy.go`, add above `tryCookielessFallbacks`:

```go
// cookielessPrefetch carries a cookieless client result the CALLER already
// fetched, so the chain reuses it instead of paying a second round trip for
// the same client in the same extraction. Owner decision O-I's
// no-behaviour-change half: the ANDROID_VR DASH fallback above already fetched
// android_vr, and on an upcoming stream VISIONOS fails hasAdequateFormats, so
// the chain never broke before reaching android_vr a second time.
//
// pooled says the caller ALREADY collected result.Formats into the pool. The
// chain must not collect them again: dedup would keep one either way, but a
// pool that carries known duplicates makes every later count a lie.
type cookielessPrefetch struct {
	clientName string
	result     *VideoInfo
	err        error
	pooled     bool
}
```

Change the signature and the fetch inside the loop:

```go
// tryCookielessFallbacks runs the cookieless fallback chain — VISIONOS, then
// ANDROID_VR — collecting every fetched format into the pool at its tier.
// …(keep the whole existing doc comment)…
//
// tally counts the video-ID mismatches this chain contributes, so a cascade in
// which EVERY client was served a substitute is reported as the IP block it is
// rather than as "no formats". prefetched, when non-nil, supplies one client's
// result instead of fetching it.
func (p *PlayerAPI) tryCookielessFallbacks(ctx context.Context, videoID, visitorData string, formatPool *[]Format, tally *mismatchTally, prefetched *cookielessPrefetch) *VideoInfo {
	var chosen *VideoInfo
	for _, fb := range []struct {
		client constants.YouTubeClientConfig
		label  string
		level  int
	}{
		{constants.VisionOSClient, "visionos", AuthLevelVisionOS},
		{constants.AndroidVRClient, "android_vr", AuthLevelAndroidVR},
	} {
		var fbResult *VideoInfo
		var fbErr error
		alreadyPooled := false
		if prefetched != nil && prefetched.clientName == fb.client.ClientName {
			fbResult, fbErr, alreadyPooled = prefetched.result, prefetched.err, prefetched.pooled
		} else {
			fbResult, fbErr = tally.note(p.fetchWithCookielessClient(ctx, videoID, visitorData, fb.client))
		}
		if fbErr != nil {
			p.logger.Debug("[PlayerApi] cookieless fallback failed",
				slog.String("client", fb.label), slog.String("error", fbErr.Error()))
			continue
		}
		if fbResult == nil {
			continue
		}
		if !alreadyPooled {
			collectFormats(formatPool, fbResult.Formats, fb.label, fb.level)
		}
		// …the rest of the loop body is unchanged…
```

Update the two call sites to pass `tally` and the prefetch. In `GetVideoInfoPublic` (no prefetch available there — the chain runs before that path's android_vr enrichment):

```go
		if cfResult := p.tryCookielessFallbacks(ctx, videoID, wp.Ytcfg.VisitorData, &formatPool, tally, nil); cfResult != nil {
```

In `GetVideoInfoAuthenticated`, hoist a `var vrPrefetch *cookielessPrefetch` before the `:256` android_vr block and fill it inside:

```go
		vrResult, vrErr := tally.note(p.fetchWithAndroidVR(ctx, videoID, ytcfg.VisitorData))
		vrPrefetch = &cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName, result: vrResult, err: vrErr}
		if vrErr != nil {
			p.logger.Debug("[PlayerApi] ANDROID_VR DASH fallback failed",
				slog.String("error", vrErr.Error()))
		} else if vrResult.PlayabilityError == PlayabilityOK && vrResult.DashManifestURL != "" {
			// …unchanged logging + adoption…
			collectFormats(&formatPool, vrResult.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
			vrPrefetch.pooled = true
		}
```

and pass it:

```go
				if cfResult := p.tryCookielessFallbacks(ctx, videoID, ytcfg.VisitorData, &formatPool, tally, vrPrefetch); cfResult != nil {
```

- [ ] **Step 4: Add the upcoming short-circuit**

Still in `player_api_strategy.go`, add above `GetVideoInfoAuthenticated`:

```go
// tvSaysWaitingRoom reports the one verdict owner decision O-I short-circuits
// on: the TV authority says the stream is UPCOMING and playability is fine.
//
// An upcoming stream has no formats BY DEFINITION, so the three clients the
// cascade consults to go find some (WEB_CREATOR, VISIONOS, ANDROID_VR — the
// last of them twice) cannot succeed, and a waiting room re-runs them every
// 30 s for its whole duration. PlayabilityOK is required because an upcoming
// MEMBERS-ONLY or login-required stream is precisely what the chain exists
// for.
//
// This does NOT touch the protected "android_vr retained" ruling: that ruling
// keeps android_vr in the client roster against upstream dropping it, and it
// stays there for every other verdict. The cost of the rule is bounded and
// was accepted: up to one 30 s poll of delay in the rare case a cookieless
// client sees the stream live before TV does.
func tvSaysWaitingRoom(result *VideoInfo) bool {
	return result != nil && result.StreamStatus == StreamUpcoming && result.PlayabilityError == PlayabilityOK
}
```

In `GetVideoInfoAuthenticated`, gate both the `:256` android_vr block and the WEB_CREATOR block:

```go
	waitingRoom := tvSaysWaitingRoom(result)

	// …ANDROID_VR DASH-only enrichment…
	if !waitingRoom &&
		result.DashManifestURL == "" &&
		(webErr != nil || webResult.DashManifestURL == "") &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		…unchanged…
```

```go
	// If TV fails, try WEB_CREATOR. Skipped outright on a waiting-room verdict
	// (owner decision O-I): len(result.Formats) == 0 is TRUE for every
	// upcoming stream, which is what used to drag the whole fallback chain
	// into every 30 s poll.
	if !waitingRoom && (result.PlayabilityError == PlayabilityMembersOnly ||
		result.PlayabilityError == PlayabilityLoginRequired ||
		len(result.Formats) == 0) {
```

In `GetVideoInfoPublic`, apply the same rule to its two equivalents:

```go
	waitingRoom := tvSaysWaitingRoom(result)

	if !waitingRoom && (result.PlayabilityError == PlayabilityLoginRequired || len(result.Formats) == 0 || !hasAdequateFormats(result)) {
```

```go
	if !waitingRoom &&
		result.DashManifestURL == "" &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		…unchanged…
```

Declare `waitingRoom` in each function right after the TV result is logged, so both gates read the same value.

- [ ] **Step 5: Run the package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/ ./internal/worker/
```
Expected: PASS, including the pre-existing `TestTryCookielessFallbacks` and `TestCookielessFallbackDashOnlyWhenNeeded` (update their calls to pass `&mismatchTally{}, nil`).

- [ ] **Step 6: Correct `platform-services.md`**

Replace step 7's lead-in and step 8, and the public-flow steps 3–4:

```markdown
7. **Evaluate TV result** -- Skipped ENTIRELY when TV says the stream is upcoming with `PlayabilityOK` (owner decision O-I, 2026-09-17): an upcoming stream has no formats by definition, so the clients below cannot find any, and a waiting room re-ran all of them every 30 seconds. Otherwise, if TV returned `members_only`, `login_required`, or zero formats:
   a. **Try WEB_CREATOR** -- POST with WEB_CREATOR client. Collect formats with `AuthLevelWebCreator`.
   b. **If WEB_CREATOR also fails** (and the error is not `members_only`): **run the cookieless chain** (`tryCookielessFallbacks`) -- VISIONOS then ANDROID_VR, POSTed without cookies but with visitorData, collected at their respective auth levels. An ANDROID_VR result the DASH enrichment below already fetched is handed in rather than re-fetched (`cookielessPrefetch`); before that, an upcoming stream fetched ANDROID_VR twice per poll, because VISIONOS fails `hasAdequateFormats` on an upcoming stream so the chain never broke early.
   c. **If all API clients fail**: Fall back to the watch page player response if available.
8. **ANDROID_VR DASH-only enrichment** -- Skipped on the same waiting-room verdict as step 7. Otherwise, if after WEB_EMBEDDED+TV+WEB no client returned a `DashManifestURL` and the stream is live or upcoming AND not members-only / age-restricted / login-required, fetch ANDROID_VR (cookieless). On success, adopt its `DashManifestURL` and merge its formats into the pool with auth-level dedup. This is a workaround for the YouTube account-based experiment that strips `dashManifestUrl` from cookied clients (yt-dlp issue #15274). ANDROID_VR remains the client here because VISIONOS returns no live `dashManifestUrl` and anonymous TV / WEB / WEB_EMBEDDED refuse live streams outright, and it remains in the roster for every verdict other than the waiting room. Note this step only matters for pools without split adaptive URLs — anything with them takes the manifest-free path and never reads the manifest.
```

```markdown
3. If TV fails or returns inadequate formats, run the cookieless chain (VISIONOS then ANDROID_VR) — unless TV says upcoming with `PlayabilityOK`, which short-circuits both this step and step 4 (owner decision O-I).
4. Apply the same DASH-only ANDROID_VR enrichment as the authenticated path when TV returned no `DashManifestURL` for a live/upcoming stream.
```

- [ ] **Step 7: Gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/youtube/
```

```bash
git add internal/youtube/player_api_strategy.go internal/youtube/player_api_strategy_test.go \
        docs/spec/platform-services.md
git commit -m "perf(youtube): stop hunting formats during a waiting room (O-I)

On a TV upcoming verdict the cascade still ran WEB_CREATOR, VISIONOS and
ANDROID_VR — the last twice, because VISIONOS fails hasAdequateFormats on an
upcoming stream so the chain never broke — for 1 page + 7 player calls every
30 s of the waiting-room window. tryCookielessFallbacks now accepts the
ANDROID_VR result the DASH enrichment already fetched, and both cascades skip
the three format-hunting clients when TV says StreamUpcoming with
PlayabilityOK. android_vr stays in the roster for every other verdict.
Report #27 / YOUTUBE-4, owner decision O-I.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/player_api_strategy.go internal/youtube/player_api_strategy_test.go \
     docs/spec/platform-services.md
```

---

## Task 8: A floor under stale-continuation recovery, and the `isReplay` flip (row #9 / TWITCH-1)

Despite its ID this row is YouTube live chat: `internal/chat/downloader.go`. A watch page that keeps serving a continuation the live endpoint immediately reports complete — the post-live case, where the page flips to `isReplay: true` and starts handing out a REPLAY token that `fetchOne` still posts to `get_live_chat`, because `opts.IsReplay` is fixed at construction and `FetchFreshContinuation`'s `isReplay` return is discarded — re-fetches the ~5 MB watch page and re-polls **with zero delay** until `MarkStreamEnded`/`Stop`. Reproduced twice independently against loopback (2,543 page fetches + 2,544 polls in 300 ms; the verifier measured 2,009 of each in the same window). `maxStaleContinuationAttempts` never applies because every recovery "succeeds" on its first call and `contRetries` is per-call.

Verifier note kept: production reachability turns on YouTube answering a replay token on the live endpoint with 200-and-no-continuation rather than 4xx (a 4xx takes the bounded 5 s×n error path). The mechanism is confirmed either way, and neither half of the fix depends on which answer YouTube gives.

**Files:**
- Modify: `internal/chat/downloader.go`
- Create: `internal/chat/stale_recovery_test.go`

**Interfaces:**
- Produces: `func (cd *ChatDownloader) isReplay() bool`; `func (cd *ChatDownloader) adoptFreshContinuation(token string, isReplay bool)`; `func staleRecoveryDelay(n int) time.Duration`; `const liveChatPollDefault`, `const maxStaleRecoveryDelay`.
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing tests**

Create `internal/chat/stale_recovery_test.go`:

```go
package chat

import (
	"context"
	"testing"
	"time"
)

// TestStaleRecoveryDelayLadder pins the floor's shape. The FIRST recovery is
// not delayed — a genuinely expired mid-stream token must be replaced at once
// — and every consecutive one after it doubles from the live poll default to a
// 5-minute ceiling.
//
// Mutants this kills:
//   - a flat delay              → n=2 and n=4 both return 5s
//   - no ceiling                → n=10 returns 2560s
//   - the ladder starting at 0  → n=1 returns 0
func TestStaleRecoveryDelayLadder(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{10, 5 * time.Minute},
	} {
		if got := staleRecoveryDelay(tc.n); got != tc.want {
			t.Errorf("staleRecoveryDelay(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestAdoptFreshContinuationHonoursTheReplayFlip is the other half of the row.
// A live broadcast that has just ended flips its watch page to isReplay:true
// and starts serving a REPLAY token; posting that to get_live_chat is exactly
// what makes the loop spin — the endpoint answers 200 with no continuation, so
// the loop recovers again, immediately, forever.
//
// Mutants this kills:
//   - the isReplay return still discarded  → isReplay() stays false
//   - the flag flipped unconditionally     → the second call flips it back
func TestAdoptFreshContinuationHonoursTheReplayFlip(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v", IsReplay: false, IsLiveOrUpcoming: true})
	if cd.isReplay() {
		t.Fatal("a live downloader started in replay mode")
	}

	cd.adoptFreshContinuation("tok-replay", true)
	if !cd.isReplay() {
		t.Error("the watch page's isReplay flip was discarded")
	}
	if cd.continuation != "tok-replay" {
		t.Errorf("continuation = %q, want the recovered token", cd.continuation)
	}

	// A page that is still live must not flip it back and forth for free.
	cd.adoptFreshContinuation("tok-live", false)
	if cd.isReplay() {
		t.Error("a live page did not flip the mode back")
	}
}

// TestRunChatLoopFloorsRepeatedStaleRecoveries is the reproduction, bounded.
// Against a watch page that keeps handing out a token the live endpoint
// reports complete, the loop used to re-fetch the ~5 MB page and re-poll with
// NO delay until Stop: 2,543 page fetches and 2,544 polls in 300 ms, measured.
//
// Mutants this kills:
//   - no floor at all                    → recoveries explodes past the cap
//   - the counter reset on every recovery → same
//   - no consecutive cap                  → the loop never exits
func TestRunChatLoopFloorsRepeatedStaleRecoveries(t *testing.T) {
	// Scale the ladder down so the test does not sleep for real minutes.
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	recoveries := 0
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v", IsLiveOrUpcoming: true, InitialContinuation: "tok"})
	cd.running = true
	cd.continuation = "tok"
	cd.testRecoveryOverride = func(context.Context) bool {
		recoveries++
		cd.continuation = "tok"
		return true
	}
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
	}

	done := make(chan struct{})
	go func() { defer close(done); cd.runChatLoop(context.Background(), false) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cd.Stop()
		t.Fatal("runChatLoop never exited: the consecutive-recovery cap is missing")
	}

	if recoveries > maxStaleContinuationAttempts {
		t.Errorf("recoveries = %d, want at most maxStaleContinuationAttempts (%d)",
			recoveries, maxStaleContinuationAttempts)
	}
	if recoveries < 2 {
		t.Errorf("recoveries = %d — the first recovery must not be delayed away", recoveries)
	}
}
```

`testRecoveryOverride` already exists (`handleEndOfStream` consults it). `testFetchOverride` does not — add it in Step 3 beside its twin, with the same "tests only" doc comment shape, and route `fetchOne` through it. `liveChatPollDefaultForTesting` / `maxStaleRecoveryDelayForTesting` are the package vars Step 3 introduces; if the existing package already has a seam of this kind for timing, reuse that name instead and keep the assertions identical.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestStaleRecovery|TestAdoptFresh|TestRunChatLoopFloors' ./internal/chat/
```
Expected: FAIL to compile — `undefined: staleRecoveryDelay`, `cd.isReplay undefined`, `undefined: testFetchOverride`.

- [ ] **Step 3: Make the replay mode mutable and add the ladder**

In `internal/chat/downloader.go`:

Add to the const block:

```go
	// liveChatPollDefault is the live endpoint's own fallback poll interval —
	// what computePollDelay uses when YouTube sends no usable TimeoutMs. It is
	// also the FLOOR under repeated stale-continuation recovery: a recovery
	// that produces a token the next poll reports complete is not progress,
	// and must not be allowed to cost less than an ordinary poll.
	liveChatPollDefault = 5 * time.Second
	// maxStaleRecoveryDelay caps that floor's doubling.
	maxStaleRecoveryDelay = 5 * time.Minute
```

and beside them the two test seams (vars, because a const cannot be scaled down):

```go
// liveChatPollDefaultForTesting / maxStaleRecoveryDelayForTesting are the
// values staleRecoveryDelay actually reads. Vars rather than consts purely so
// tests can scale the ladder down instead of sleeping for real minutes — the
// playerRetryBackoffBase seam in internal/youtube exists for the same reason.
// Production never writes them.
var (
	liveChatPollDefaultForTesting  = liveChatPollDefault
	maxStaleRecoveryDelayForTesting = maxStaleRecoveryDelay
)
```

Add the replay flag to the struct (beside the other mutable state) and initialise it in `NewChatDownloader`:

```go
	// replay is the LIVE-vs-REPLAY endpoint choice. It starts at
	// opts.IsReplay but is mutable, because a broadcast that ends mid-run
	// flips its watch page to isReplay:true and starts serving replay tokens;
	// continuing to post those to get_live_chat is what span the recovery
	// loop. opts.IsReplay is never read again after construction.
	replay atomic.Bool
```

```go
	cd.replay.Store(opts.IsReplay)
```

(`NewChatDownloader` builds and returns `cd`; add the store before the return. Add `"sync/atomic"` to the imports if absent.)

Add the accessor and the adopter:

```go
// isReplay reports which chat endpoint this run is currently using. Read this,
// never opts.IsReplay: the mode can change mid-run (see adoptFreshContinuation).
func (cd *ChatDownloader) isReplay() bool { return cd.replay.Load() }

// adoptFreshContinuation installs a token recovered from the watch page and
// follows the PAGE's verdict about which endpoint it belongs to.
//
// A live broadcast that has just ended flips its watch page to isReplay:true
// and starts serving a replay token. Posting that to get_live_chat is what
// makes the recovery loop spin: the live endpoint answers 200 with no
// continuation, runChatLoop calls handleEndOfStream again, the page hands out
// the same replay token, and nothing in that circuit ever sleeps.
func (cd *ChatDownloader) adoptFreshContinuation(token string, isReplay bool) {
	cd.continuation = token
	if isReplay == cd.isReplay() {
		return
	}
	cd.replay.Store(isReplay)
	cd.logInfo("chat: watch page switched the chat endpoint",
		"videoID", cd.opts.VideoID, "replay", isReplay)
}

// staleRecoveryDelay is the floor under REPEATED stale-continuation recovery.
// n is how many consecutive recoveries have happened, counting this one, so
// n == 1 (the first, which is usually a genuinely expired mid-stream token)
// waits one ordinary poll and each one after that doubles to the ceiling.
func staleRecoveryDelay(n int) time.Duration {
	d := liveChatPollDefaultForTesting
	for range max(n-1, 0) {
		d *= 2
		if d >= maxStaleRecoveryDelayForTesting {
			return maxStaleRecoveryDelayForTesting
		}
	}
	return d
}
```

Replace the three `cd.opts.IsReplay` reads with `cd.isReplay()`:
- `noteLivePollResult`: `if cd.isReplay() || !hasContinuation {`
- `fetchOne`: `if cd.isReplay() {`
- `computePollDelay`: `if cd.isReplay() { return 0 }` and `waitMs = int(liveChatPollDefault / time.Millisecond)` in place of the literal `5000`.

Honour the flip in `recoverStaleContinuation` — both the first call and the retry loop:

```go
	fresh, freshIsReplay, freshErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
	if freshErr == nil && fresh != "" {
		cd.adoptFreshContinuation(fresh, freshIsReplay)
		return true
	}
```

```go
		retry, retryIsReplay, retryErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
		if retryErr == nil && retry != "" {
			cd.adoptFreshContinuation(retry, retryIsReplay)
			return true
		}
```

Add the fetch seam beside `testRecoveryOverride` (read its declaration and mirror its comment style):

```go
	// testFetchOverride replaces the network fetch in runChatLoop. Tests only,
	// exactly like testRecoveryOverride beside it; production leaves it nil.
	testFetchOverride func(ctx context.Context) (*ChatApiResponse, error)
```

and route `fetchOne` through it:

```go
func (cd *ChatDownloader) fetchOne(ctx context.Context) (*ChatApiResponse, error) {
	if cd.testFetchOverride != nil {
		return cd.testFetchOverride(ctx)
	}
	if cd.isReplay() {
		return cd.api.FetchChatReplay(ctx, cd.continuation)
	}
	return cd.api.FetchLiveChat(ctx, cd.continuation)
}
```

- [ ] **Step 4: Put the floor in `runChatLoop`**

Declare `staleRecoveries := 0` beside `consecutiveErrors` at the top of `runChatLoop`, and replace the end-of-stream branch:

```go
		// Handle end-of-stream / stale continuation
		if resp.IsComplete || resp.NextContinuation == "" {
			if !cd.isStreamActive() {
				break // VOD/replay complete
			}
			if !cd.handleEndOfStream(ctx) {
				break
			}
			staleRecoveries++
			// A recovered token the very NEXT poll reports complete is not a
			// recovery — it is the same stale state arriving under a new name.
			// Without a floor this circuit re-fetched the ~5 MB watch page and
			// re-polled with zero delay until Stop (measured: 2,543 page
			// fetches and 2,544 polls in 300 ms). Count it as a failed attempt
			// and sleep the ladder, so repeated "successful" recoveries back
			// off exactly like repeated failed ones do inside
			// recoverStaleContinuation. The FIRST one is not delayed: a
			// genuinely expired mid-stream token must be replaced at once, and
			// staleRecoveries is reset by any poll that actually produces a
			// continuation.
			if staleRecoveries > 1 {
				cd.sleep(ctx, staleRecoveryDelay(staleRecoveries-1))
				if cd.shouldStop() || ctx.Err() != nil {
					break
				}
			}
			if staleRecoveries >= maxStaleContinuationAttempts {
				// maxStaleContinuationAttempts never applied before: every
				// recovery "succeeded" on its first call, and contRetries is
				// per-call. This is the outer cap it was written to be.
				cd.logInfo("chat: giving up after repeated stale-continuation recoveries",
					"videoID", cd.opts.VideoID, "recoveries", staleRecoveries)
				break
			}
			switchedToAllChat = false // Fresh token defaults to Top Chat — re-trigger switch
			continue
		}

		// A poll that produced a real continuation is progress: the streak
		// that the floor above measures starts over.
		staleRecoveries = 0
		cd.continuation = resp.NextContinuation
```

- [ ] **Step 5: Run the chat package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/chat/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race -run 'TestStaleRecovery|TestAdoptFresh|TestRunChatLoopFloors' ./internal/chat/
```
Expected: PASS, no data race. `cd.running` is set under `cd.mu` in production; if the new test's direct write trips `-race`, set it through whatever the package's existing loop tests use instead.

- [ ] **Step 6: Commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
```

```bash
git add internal/chat/downloader.go internal/chat/stale_recovery_test.go
git commit -m "fix(chat): floor the stale-continuation recovery and honour the replay flip

A post-live watch page flips to isReplay:true and serves a replay token;
fetchOne kept posting it to get_live_chat because opts.IsReplay was fixed at
construction and FetchFreshContinuation's isReplay return was discarded. The
live endpoint answers 200 with no continuation, so the loop re-fetched the
~5 MB page and re-polled with ZERO delay until Stop — 2,543 fetches and 2,544
polls in 300 ms, reproduced twice. Consecutive recoveries now carry a 5s→5min
ladder and maxStaleContinuationAttempts finally applies as the outer cap, and
the endpoint follows the page. Report #9 / TWITCH-1.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/chat/downloader.go internal/chat/stale_recovery_test.go
```

---

## Task 9: Low batch A — Innertube diagnostics and two parity gaps (rows #54, #55, #57, #58, #59)

Five Low-tier rows that all live in the request/response path:

| Row | Finding | Fix |
|---|---|---|
| #54 YOUTUBE-7 | Innertube HTTP failures discard the body; YouTube answers 400/401/403 with `{"error":{"message","status"}}` and the operator sees only `HTTP 403`. The worker classifier keys on the `"HTTP <code>"` substring, so the prefix must survive. | Append `status: message` after the existing text, bounded. |
| #55 YOUTUBE-8 | A client forced onto SABR (URL-less formats + `streamingData.serverAbrStreamingUrl`) logs as "formats 0", indistinguishable from empty streamingData. `grep -rn serverAbrStreamingUrl --include=*.go internal/` returns nothing at all. | Count both and print them in each per-client result line. |
| #57 YOUTUBE-11 (**O-R**) | A PLAYER PO token is minted for every WEB-family call including the monitor's date probes, parking a 6 h session-cache entry every later mint sweeps. yt-dlp's WEB `PLAYER_PO_TOKEN_POLICY` is `required=False` (`_base.py:90`). | Skip the token on probe-only calls; keep it on real download player calls. |
| #58 YOUTUBE-12 | Public path: a TV failure after retries with no watch-page player response returns the error without trying VISIONOS/ANDROID_VR; the authenticated path logs and continues. | Mirror the authenticated shape. |
| #59 YOUTUBE-14 | The watch page is fetched without yt-dlp's `bpctr=9999999999&has_verified=1` (`_video.py:3809`), so an age-restricted video returns the age-gate shell and its embedded player response is lost. | Append the two parameters. |

> **O-R (verbatim):** The PLAYER PO token is skipped on probe-only WEB-family calls (the monitor's date probes) and kept on real download player calls.

**Files:**
- Modify: `internal/youtube/player_api_strategy.go`
- Modify: `internal/youtube/browse.go`
- Modify: `internal/youtube/watch_page.go`
- Modify: `internal/youtube/types.go`
- Modify: `internal/youtube/player_api_parsing.go`
- Modify: `internal/youtube/player_api_strategy_test.go`, `internal/youtube/watch_page_test.go`
- Modify: `docs/spec/platform-services.md` (the Player-API tokens paragraph, flow step 1)

**Interfaces:**
- Produces: `func innertubeErrorDetail(body []byte) string`; `type FormatDiag struct { URLlessFormats int; SabrForced bool }` on `VideoInfo`; `func (p *PlayerAPI) fetchWithClientProbe(...)`.
- Consumes (Tasks 3, 4, 7): `doRetryRequest`'s `videoID` parameter, `parseFormats`'s DRM skip, `tally`/`finishExtraction`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/youtube/player_api_strategy_test.go`:

```go
// TestInnertubeErrorCarriesYouTubesOwnMessage: YouTube explains a 400/401/403
// in the body ("Precondition check failed", "Request is missing required
// authentication credential"), and the operator only ever saw "HTTP 403".
// The "HTTP <code>" substring must SURVIVE, because worker/probe_classify.go
// keys on it to decide whether a probe error is transient.
//
// Mutants this kills:
//   - the detail not appended            → the message check fails
//   - the "HTTP %d" prefix replaced      → the prefix check fails
//   - a non-JSON body crashing or leaking → the third subtest fails
func TestInnertubeErrorCarriesYouTubesOwnMessage(t *testing.T) {
	t.Run("json error body", func(t *testing.T) {
		got := innertubeErrorDetail([]byte(`{"error":{"code":403,"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`))
		if !strings.Contains(got, "FAILED_PRECONDITION") || !strings.Contains(got, "Precondition check failed.") {
			t.Errorf("innertubeErrorDetail = %q, want YouTube's status and message", got)
		}
	})

	t.Run("non-json body yields nothing", func(t *testing.T) {
		if got := innertubeErrorDetail([]byte("<html>502 Bad Gateway</html>")); got != "" {
			t.Errorf("innertubeErrorDetail on HTML = %q, want \"\"", got)
		}
	})

	t.Run("the classifier's prefix survives", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"28": {http.StatusForbidden, `{"error":{"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`},
		}}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		_, err := NewPlayerAPI(nil, noopLogger{}).ProbeVideoStatus(context.Background(), "test1234567", "vd")
		if err == nil {
			t.Fatal("a 403 was not reported as an error")
		}
		if !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("err = %q — probe_classify.go keys on the \"HTTP <code>\" substring", err)
		}
		if !strings.Contains(err.Error(), "Precondition check failed.") {
			t.Errorf("err = %q, want YouTube's own message appended", err)
		}
	})
}

// TestFormatDiagCountsTheSABRSignal: a client forced onto SABR returns formats
// with neither url nor signatureCipher plus a serverAbrStreamingUrl. Today
// that logs as "formats 0", indistinguishable from an empty streamingData —
// and serverAbrStreamingUrl appears nowhere in the codebase at all.
//
// Mutants this kills:
//   - URL-less formats not counted   → URLlessFormats == 0
//   - serverAbrStreamingUrl not read → SabrForced == false
func TestFormatDiagCountsTheSABRSignal(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
		"streamingData": {
			"serverAbrStreamingUrl": "https://rr1---sn-x.googlevideo.com/videoplayback?...",
			"adaptiveFormats": [
				{"itag": 137, "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
				{"itag": 140, "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
			]
		}
	}`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "test1234567")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	if len(info.Formats) != 0 {
		t.Fatalf("Formats = %+v, want none (they carry no URL)", info.Formats)
	}
	if info.FormatDiag.URLlessFormats != 2 {
		t.Errorf("URLlessFormats = %d, want 2", info.FormatDiag.URLlessFormats)
	}
	if !info.FormatDiag.SabrForced {
		t.Error("SabrForced = false although streamingData carried serverAbrStreamingUrl")
	}
}

// TestProbeVideoDateMintsNoPlayerToken is owner decision O-R. yt-dlp's WEB
// PLAYER_PO_TOKEN_POLICY is required=False / recommended=False (_base.py:90),
// so upstream mints none; Moombox minted one per PROBED video ID and parked a
// 6 h session-cache entry that every later mint sweeps.
//
// Mutants this kills:
//   - the probe still routed through fetchWithClient → minted == 1
//   - the skip applied to real player calls too      → the second half fails
func TestProbeVideoDateMintsNoPlayerToken(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"1": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	prov := &countingPotProvider{token: "POT"}
	p := NewPlayerAPI(nil, noopLogger{})
	p.SetPotProvider(prov)

	if _, _, err := p.ProbeVideoDate(context.Background(), "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
	if prov.calls != 0 {
		t.Errorf("ProbeVideoDate minted %d PLAYER PO tokens, want 0", prov.calls)
	}

	ytcfg := DefaultYtcfg()
	ytcfg.VisitorData = "vd"
	if _, err := p.fetchWithClient(context.Background(), "test1234567", constants.WebSafariClient, ytcfg, 0); err != nil {
		t.Fatalf("fetchWithClient: %v", err)
	}
	if prov.calls != 1 {
		t.Errorf("a real WEB player call minted %d tokens, want 1 — the skip is for probes only", prov.calls)
	}
}

// TestPublicPathContinuesAfterATVFailure is row #58. The authenticated path
// logs and carries on into WEB_CREATOR / the cookieless chain; the public path
// returned the TV error outright whenever the watch page had no player
// response, so an anonymous extraction gave up with clients left untried.
//
// Mutant this kills: the early `return nil, err` restored → err is non-nil
// and VISIONOS is never called.
func TestPublicPathContinuesAfterATVFailure(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusNotFound, `{"error":{"message":"not found","status":"NOT_FOUND"}}`},
		"101": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := NewPlayerAPI(nil, noopLogger{}).GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic gave up after the TV failure: %v", err)
	}
	if info == nil || len(info.Formats) == 0 {
		t.Fatalf("info = %+v, want VISIONOS's formats", info)
	}
}
```

`SetPotProvider` takes `youtube.PotTokenProvider` (`player_api.go:34`), a one-method interface, so the counter is four lines:

```go
// countingPotProvider counts PLAYER PO-token mints. PotTokenProvider
// (player_api.go:34) has exactly one method, so this is the whole fake.
type countingPotProvider struct {
	token string
	calls int
}

func (c *countingPotProvider) GeneratePoTokenString(context.Context, string, bool) (string, error) {
	c.calls++
	return c.token, nil
}
```

And append to `internal/youtube/watch_page_test.go`:

```go
// TestWatchPageURLCarriesTheAgeGateBypass is row #59: without yt-dlp's
// bpctr / has_verified pair (_video.py:3809) an age-restricted video's watch
// page returns the age-gate shell, and with it the ScheduledStartTime source
// and the WatchPage format tier both vanish.
//
// Mutant this kills: the two parameters dropped → the URL check fails.
func TestWatchPageURLCarriesTheAgeGateBypass(t *testing.T) {
	got := watchPageURL("dQw4w9WgXcQ")
	for _, want := range []string{"v=dQw4w9WgXcQ", "bpctr=9999999999", "has_verified=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("watchPageURL = %q, missing %q", got, want)
		}
	}
}
```

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestInnertubeError|TestFormatDiag|TestProbeVideoDateMints|TestPublicPathContinues|TestWatchPageURL' ./internal/youtube/
```
Expected: FAIL to compile — `undefined: innertubeErrorDetail`, `undefined: watchPageURL`, `info.FormatDiag undefined`.

- [ ] **Step 3: #59 — the watch-page query parameters**

In `internal/youtube/watch_page.go`, extract the URL builder and use it:

```go
// watchPageURL builds the watch-page URL. bpctr and has_verified are yt-dlp's
// age-gate bypass pair (_video.py:3809, `query = {'bpctr': '9999999999',
// 'has_verified': '1'}`): without them an age-restricted video answers with
// the age-gate shell instead of the page, so its embedded player response —
// the watch-page ScheduledStartTime source and the WatchPage format tier —
// is lost for exactly the videos that need every source they can get.
func watchPageURL(videoID string) string {
	return fmt.Sprintf("%s?v=%s&bpctr=9999999999&has_verified=1", constants.YouTubeURLs.Watch, videoID)
}
```

```go
	url := watchPageURL(videoID)
```

- [ ] **Step 4: #54 — the Innertube error body**

In `internal/youtube/player_api_strategy.go`, add above `doRetryRequest`:

```go
// innertubeErrorDetailMax bounds what a failure message may quote back from
// YouTube's body. yt-dlp peeks 512 bytes for the same purpose.
const innertubeErrorDetailMax = 512

// innertubeErrorDetail extracts YouTube's own explanation of a non-200
// Innertube response — `{"error":{"message":"…","status":"…"}}`, e.g.
// "Precondition check failed." / "FAILED_PRECONDITION", or "Request is missing
// required authentication credential". Returns "" for any body that is not
// that shape, so a CDN's HTML error page is never quoted into a log line.
//
// The body is DECODED rather than truncated first: chopping at 512 bytes would
// break the JSON and lose the message this exists to surface. The bound is
// applied to the OUTPUT instead.
func innertubeErrorDetail(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	detail := strings.TrimSpace(strings.TrimPrefix(e.Error.Status+": "+e.Error.Message, ": "))
	detail = strings.TrimSuffix(detail, ":")
	if detail == "" {
		return ""
	}
	if len(detail) > innertubeErrorDetailMax {
		detail = detail[:innertubeErrorDetailMax]
	}
	return detail
}

// innertubeHTTPError formats a non-200 Innertube failure. The "<label> API
// error: HTTP <code>" prefix is load-bearing: worker/probe_classify.go matches
// on the "HTTP <code>" substring to decide whether a probe error is transient,
// so YouTube's own text is APPENDED to it, never substituted for it.
func innertubeHTTPError(clientLabel string, status int, body []byte) error {
	if detail := innertubeErrorDetail(body); detail != "" {
		return fmt.Errorf("%s API error: HTTP %d — %s", clientLabel, status, detail)
	}
	return fmt.Errorf("%s API error: HTTP %d", clientLabel, status)
}
```

Add `"strings"` to the file's imports, and use the helper in both branches of `doRetryRequest`:

```go
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = innertubeHTTPError(clientLabel, resp.StatusCode, respBody)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, innertubeHTTPError(clientLabel, resp.StatusCode, respBody)
		}
```

In `internal/youtube/browse.go`, read a bounded body on the error branch instead of discarding it:

```go
	if resp.StatusCode != http.StatusOK {
		// Read a bounded body rather than discarding it: YouTube explains a
		// 400/401/403 in `error.message`, and "browse API error: HTTP 403"
		// alone tells an operator nothing. The prefix is kept for the same
		// reason doRetryRequest keeps its own.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if detail := innertubeErrorDetail(errBody); detail != "" {
			return nil, fmt.Errorf("browse API error: HTTP %d — %s", resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("browse API error: HTTP %d", resp.StatusCode)
	}
```

- [ ] **Step 5: #55 — the SABR diagnostic**

In `internal/youtube/types.go`, add to `VideoInfo` beside `SessionAuth`:

```go
	// FormatDiag carries parse-time counters the per-client result log lines
	// print. Diagnostic only — nothing downloads differently because of it —
	// but it is what distinguishes "this client was forced onto SABR" from
	// "streamingData was empty", which both used to read as "formats 0".
	FormatDiag FormatDiag `json:"-"`
```

and, beside `Format`:

```go
// FormatDiag records why a client's format list came out the size it did.
//
// A client YouTube has forced onto SABR returns formats with neither `url`
// nor `signatureCipher` plus a `streamingData.serverAbrStreamingUrl` — yt-dlp
// says so out loud ("YouTube is forcing SABR streaming for this client",
// _video.py:3527-3542). Moombox skipped those formats silently, so the death
// of a client looked exactly like an empty response.
type FormatDiag struct {
	// URLlessFormats counts entries skipped for carrying no fetchable URL.
	URLlessFormats int
	// SabrForced is true when streamingData carried serverAbrStreamingUrl.
	SabrForced bool
}
```

In `internal/youtube/player_api_parsing.go`, change `parseFormats` to return the diagnostics alongside the formats:

```go
func (p *PlayerAPI) parseFormats(streamingData map[string]any) ([]Format, FormatDiag) {
	var diag FormatDiag
	if streamingData == nil {
		return nil, diag
	}
	diag.SabrForced = getStr(streamingData, "serverAbrStreamingUrl") != ""
	…
```

increment `diag.URLlessFormats++` on the `if formatURL == "" { continue }` arm **and** on the `signatureCipher` parse-failure arms (all three `continue`s that mean "no fetchable URL"), and return `formats, diag`. In `parsePlayerResponse`, take both and set the field:

```go
	formats, formatDiag := p.parseFormats(streamingData)
```
```go
		FormatDiag:         formatDiag,
```

Update the existing call in the Task-4 test to the two-value form. Then print them in the per-client result lines — add `"urllessFormats", result.FormatDiag.URLlessFormats, "sabrForced", result.FormatDiag.SabrForced` to the TV, web, WEB_CREATOR, TV-public and cookieless-fallback Debug lines in `player_api_strategy.go` (grep for `"formats", len(` to find all five).

- [ ] **Step 6: #57 (O-R) — no PLAYER token on the date probe**

In `internal/youtube/player_api_strategy.go`, split `fetchWithClient`:

```go
// fetchWithClientProbe is fetchWithClient for PROBE-ONLY calls — the monitor's
// date probes. Owner decision O-R: no PLAYER PO token is minted.
//
// yt-dlp's WEB PLAYER_PO_TOKEN_POLICY is required=False, recommended=False
// (_base.py:90), so upstream mints none for these at all. Moombox minted one
// per probed VIDEO ID and parked a 6 h sessionCache entry that every later
// mint sweeps O(n) — pure waste for a video that is never downloaded. Real
// download player calls keep their token.
func (p *PlayerAPI) fetchWithClientProbe(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	return p.fetchWithClientOpts(ctx, videoID, client, ytcfg, sts, true)
}

func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	return p.fetchWithClientOpts(ctx, videoID, client, ytcfg, sts, false)
}

func (p *PlayerAPI) fetchWithClientOpts(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int, probeOnly bool) (*VideoInfo, error) {
	// …the existing body, with the PO-token guard gaining one clause…
```

and extend that guard:

```go
	if !probeOnly && p.potProvider != nil && clientAcceptsPlayerPoToken(client) && ytcfg != nil && ytcfg.VisitorData != "" {
```

Point `ProbeVideoDate` at the probe variant:

```go
	info, err := p.fetchWithClientProbe(ctx, videoID, constants.WebSafariClient, ytcfg, 0)
```

Leave `ProbeVideoStatusAuthenticated` on `fetchWithClient`: TV_DOWNGRADED is not WEB-family, so `clientAcceptsPlayerPoToken` already returns false there and nothing is minted either way.

- [ ] **Step 7: #58 — the public path continues**

In `GetVideoInfoPublic`, replace the TV failure arm:

```go
	result, err := tally.note(p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, wp.Ytcfg, stsPublic))
	if err != nil {
		// Mirror the authenticated path (:166-171): log and carry on into the
		// cookieless chain rather than giving up with clients untried. The
		// watch-page fallback below still applies when nothing else produces
		// anything.
		p.logger.Warn("[PlayerApi] TV client failed (public), will try other clients", slog.String("error", err.Error()))
		result = &VideoInfo{}
	} else {
		collectFormats(&formatPool, result.Formats, "tv_public", AuthLevelTVPublic)
		…the existing Debug line…
	}
```

The `if result.PlayabilityError == PlayabilityLoginRequired || len(result.Formats) == 0 || !hasAdequateFormats(result)` block below then runs (an empty `VideoInfo` has no formats), which is the whole point.

- [ ] **Step 8: Run the package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/ ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
```
Expected: PASS.

- [ ] **Step 9: Correct `platform-services.md`**

Step 1 of the authenticated flow:

```markdown
1. **Fetch watch page** -- GET `https://www.youtube.com/watch?v={videoID}&bpctr=9999999999&has_verified=1` with cookies. The two extra parameters are yt-dlp's age-gate bypass (`_video.py:3809`): without them an age-restricted video answers with the age-gate shell and its embedded player response is lost. Extracts `YtcfgData` (playerURL, visitorData, sessionIndex, delegatedSessionID) and `ytInitialPlayerResponse`.
```

And in the Player-API tokens paragraph, after "…the 'session established' precondition it always was.", insert:

```markdown
PROBE-ONLY calls mint none (`fetchWithClientProbe`, owner decision O-R, 2026-09-17): yt-dlp's WEB `PLAYER_PO_TOKEN_POLICY` is `required=False, recommended=False` (`_base.py:90`), and Moombox was minting one per probed video ID on the monitor's date probes — parking a 6 h session-cache entry, for a video that may never be downloaded, that every later mint then sweeps.
```

- [ ] **Step 10: Gate and commit**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/youtube/
```

```bash
git add internal/youtube/player_api_strategy.go internal/youtube/browse.go \
        internal/youtube/watch_page.go internal/youtube/types.go \
        internal/youtube/player_api_parsing.go internal/youtube/player_api_strategy_test.go \
        internal/youtube/watch_page_test.go docs/spec/platform-services.md
git commit -m "fix(youtube): Innertube error bodies, SABR signal, probe tokens, two parity gaps

YOUTUBE-7: a non-200 Innertube response now appends YouTube's own
error.status/message after the 'HTTP <code>' prefix probe_classify.go keys on
(browse too). YOUTUBE-8: URL-less formats and serverAbrStreamingUrl are
counted into VideoInfo.FormatDiag and printed in every per-client result line,
so a client forced onto SABR stops reading as 'formats 0'. YOUTUBE-11 (O-R):
the monitor's date probe mints no PLAYER PO token, matching upstream's
required=False WEB policy. YOUTUBE-12: the public path logs a TV failure and
continues into the cookieless chain like the authenticated one. YOUTUBE-14:
the watch page carries bpctr=9999999999&has_verified=1. Rows #54-55, #57-59.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/player_api_strategy.go internal/youtube/browse.go \
     internal/youtube/watch_page.go internal/youtube/types.go \
     internal/youtube/player_api_parsing.go internal/youtube/player_api_strategy_test.go \
     internal/youtube/watch_page_test.go docs/spec/platform-services.md
```

---

## Task 10: Low batch B — reuse the watch page for chat, and stop scanning it twice (rows #56, #60)

| Row | Finding | Fix |
|---|---|---|
| #56 YOUTUBE-10 | Chat setup re-fetches the 1–5 MB authenticated watch page that `GetVideoInfo` fetched moments earlier. `FetchWatchPage` already extracts `ChatContinuation` / `ChatIsReplay` / `ChatErr` on every call and the strategy throws them away, so job start and every early-chat (re)start pay a second page. Both sites confirmed by the verifier: `orchestrator_chat.go:27` and `stream_processor_youtube.go:480`. | Carry the chat facts on `VideoInfo`; the two sites use them when fresh and fetch only as a fallback. |
| #60 YOUTUBE-15 | The `ytInitialData` candidate is accepted by `json.Valid` (one full scan of the ~4.4 MB literal) and then decoded into the typed envelope (a second full scan). Measured 10.8 ms/fetch on a 4.7 MB page. The citation `jsoncandidates.go:264-270` is out of range — the real site is `:123-134` (verifier). | Let the typed decode BE the acceptance, as `extractPlayerResponse` already does, keeping the non-empty check. |

Ruling carried in: **no candidate cap** (Arc 3 fix-wave re-review). The iterator keeps trying every occurrence; only the acceptance predicate changes.

**Files:**
- Modify: `internal/youtube/types.go` (`ChatSource` on `VideoInfo`)
- Modify: `internal/youtube/player_api_strategy.go` (`withAttestation`)
- Modify: `internal/utils/jsoncandidates.go` (`IsNonEmptyJSONBody`)
- Modify: `internal/youtube/channel_membership.go` (`extractYtInitialData`)
- Modify: `internal/youtube/watch_page.go` (`extractChatContinuation`)
- Modify: `internal/worker/orchestrator_chat.go` (`setupChatDownloader` only — cross-arc, called out)
- Modify: `internal/worker/stream_processor_youtube.go` (`tryStartEarlyChat`)
- Create: `internal/worker/chat_source_test.go`
- Modify: `internal/utils/jsoncandidates_test.go` (or the package's existing candidate test file)

**Interfaces:**
- Produces: `type ChatSource struct { FetchedAt time.Time; Continuation string; IsReplay bool; Err error; VisitorData string }` with `func (c ChatSource) Usable() bool`; `VideoInfo.Chat ChatSource`; `func utils.IsNonEmptyJSONBody(obj []byte) bool`; `func extractYtInitialDataInto(data []byte, decode func([]byte) bool) bool`; `var fetchWatchPageForChat = youtube.FetchWatchPage` (worker seam); `func chatSourceFor(ctx, info, videoID, cookieHeader) (youtube.ChatSource, error)`.
- Consumes (Tasks 3–9): `withAttestation` / `finishExtraction` already in place.

- [ ] **Step 1: Write the failing worker test**

Create `internal/worker/chat_source_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestChatSourceForReusesAFreshPage is row #56. FetchWatchPage already
// extracts the chat continuation on EVERY call, and GetVideoInfo had just
// fetched the page — so both chat-start sites were paying for a second 1-5 MB
// authenticated page they did not need.
//
// Mutants this kills:
//   - the carried source ignored     → fetched == 1
//   - the freshness gate dropped     → the stale subtest also sees fetched == 0
//   - an empty continuation accepted → the empty subtest sees fetched == 0
func TestChatSourceForReusesAFreshPage(t *testing.T) {
	fetched := 0
	orig := fetchWatchPageForChat
	fetchWatchPageForChat = func(context.Context, string, string) (*youtube.WatchPageResult, error) {
		fetched++
		return &youtube.WatchPageResult{
			Ytcfg:            youtube.DefaultYtcfg(),
			ChatContinuation: "from-a-second-page",
		}, nil
	}
	t.Cleanup(func() { fetchWatchPageForChat = orig })

	t.Run("fresh source is reused", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{
			FetchedAt:    time.Now(),
			Continuation: "carried",
			IsReplay:     true,
			VisitorData:  "vd",
		}}
		got, err := chatSourceFor(context.Background(), info, "vid", "")
		if err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 0 {
			t.Errorf("the watch page was fetched %d times although the info carried a fresh source", fetched)
		}
		if got.Continuation != "carried" || !got.IsReplay || got.VisitorData != "vd" {
			t.Errorf("got %+v, want the carried source", got)
		}
	})

	t.Run("a stale source is re-fetched", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{
			FetchedAt:    time.Now().Add(-10 * time.Minute),
			Continuation: "carried",
		}}
		got, err := chatSourceFor(context.Background(), info, "vid", "")
		if err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 1 || got.Continuation != "from-a-second-page" {
			t.Errorf("fetched=%d continuation=%q, want one fetch and the fresh token", fetched, got.Continuation)
		}
	})

	t.Run("no continuation means no source", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{FetchedAt: time.Now()}}
		if _, err := chatSourceFor(context.Background(), info, "vid", ""); err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 1 {
			t.Errorf("fetched=%d, want 1 — an empty continuation is not a usable source", fetched)
		}
	})

	t.Run("a nil info still works", func(t *testing.T) {
		fetched = 0
		if _, err := chatSourceFor(context.Background(), nil, "vid", ""); err != nil {
			t.Fatalf("chatSourceFor(nil): %v", err)
		}
		if fetched != 1 {
			t.Errorf("fetched=%d, want 1", fetched)
		}
	})

	t.Run("a fetch failure is surfaced", func(t *testing.T) {
		boom := errors.New("network down")
		fetchWatchPageForChat = func(context.Context, string, string) (*youtube.WatchPageResult, error) {
			return nil, boom
		}
		if _, err := chatSourceFor(context.Background(), nil, "vid", ""); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the fetch error", err)
		}
	})
}
```

- [ ] **Step 2: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestChatSourceFor ./internal/worker/
```
Expected: FAIL to compile — `undefined: chatSourceFor`, `youtube.ChatSource undefined`.

- [ ] **Step 3: Carry the chat facts on `VideoInfo`**

In `internal/youtube/types.go`, add `"time"` to the imports if absent and add beside `VideoInfo`:

```go
// ChatSource carries the chat-setup facts the WATCH PAGE this VideoInfo was
// extracted from already yielded. FetchWatchPage extracts them on every call
// and the strategies used to discard them, so both chat-start sites paid for a
// SECOND 1-5 MB authenticated page moments after the first.
//
// The zero value means "no page was parsed for this info" — which is exactly
// what a cookieless ANDROID_VR probe returns — and a consumer must fall back
// to FetchWatchPage then.
type ChatSource struct {
	// FetchedAt is when the page was fetched. A continuation is short-lived,
	// so a consumer checks Usable rather than trusting the token forever.
	FetchedAt time.Time
	// Continuation is the chat continuation token. Empty means the page had
	// no chat.
	Continuation string
	// IsReplay says which chat endpoint the token belongs to.
	IsReplay bool
	// Err is why extraction failed, for the consumer's debug log. Non-nil with
	// an empty Continuation means "no chat available" with context.
	Err error
	// VisitorData is the page's own visitor data, which the chat poller sends
	// as X-Goog-Visitor-Id.
	VisitorData string
}

// chatSourceMaxAge bounds how long a carried continuation is trusted. Short,
// because the token is short-lived; generous enough to cover the gap between
// GetVideoInfo and the orchestrator actually starting chat. Past it the
// consumer re-fetches, which is the old behaviour.
const chatSourceMaxAge = 2 * time.Minute

// Usable reports whether this source is recent enough to start a chat
// downloader from without re-fetching the watch page.
func (c ChatSource) Usable() bool {
	return c.Continuation != "" && !c.FetchedAt.IsZero() && time.Since(c.FetchedAt) <= chatSourceMaxAge
}
```

and the field on `VideoInfo`, beside `SessionAuth`:

```go
	// Chat carries what the watch page already said about live chat, so chat
	// setup does not fetch that page a second time. In-process hand-off only.
	Chat ChatSource `json:"-"`
```

Fill it in `withAttestation` (the one function every return of both cascades passes through, and the only one holding both the info and the page):

```go
	if wp != nil {
		// Carry the chat facts the page already yielded, so setupChatDownloader
		// and tryStartEarlyChat do not fetch this 1-5 MB page again moments
		// from now (report #56 / YOUTUBE-10).
		info.Chat = ChatSource{
			FetchedAt:    time.Now(),
			Continuation: wp.ChatContinuation,
			IsReplay:     wp.ChatIsReplay,
			Err:          wp.ChatErr,
		}
		if wp.Ytcfg != nil {
			info.Chat.VisitorData = wp.Ytcfg.VisitorData
		}
		// …the existing SessionAuth + GvsBinding lines…
```

Add `"time"` to `player_api_strategy.go`'s imports (it is already there).

- [ ] **Step 4: Use it at both chat-start sites**

In `internal/worker/orchestrator_chat.go`, add above `setupChatDownloader`:

```go
// fetchWatchPageForChat is youtube.FetchWatchPage behind a package var so the
// chat-source tests can assert that a FRESH carried source costs no round
// trip. Production never writes it.
var fetchWatchPageForChat = youtube.FetchWatchPage

// chatSourceFor returns the continuation, replay flag and visitor data to
// start a chat downloader from.
//
// It prefers what the VideoInfo already carries: FetchWatchPage extracts those
// three on every call, and GetVideoInfo fetched the page moments ago, so
// fetching it again cost a second 1-5 MB authenticated download at job start
// and at every early-chat restart (report #56 / YOUTUBE-10). A source that is
// missing (a cookieless probe never parsed a page) or older than
// chatSourceMaxAge falls back to the fetch, which is the old behaviour.
func chatSourceFor(ctx context.Context, info *youtube.VideoInfo, videoID, cookieHeader string) (youtube.ChatSource, error) {
	if info != nil && info.Chat.Usable() {
		return info.Chat, nil
	}
	wp, err := fetchWatchPageForChat(ctx, videoID, cookieHeader)
	if err != nil {
		return youtube.ChatSource{}, err
	}
	src := youtube.ChatSource{
		FetchedAt:    time.Now(),
		Continuation: wp.ChatContinuation,
		IsReplay:     wp.ChatIsReplay,
		Err:          wp.ChatErr,
	}
	if wp.Ytcfg != nil {
		src.VisitorData = wp.Ytcfg.VisitorData
	}
	return src, nil
}
```

Rewrite the head of `setupChatDownloader` to use it:

```go
	src, err := chatSourceFor(ctx, videoInfo, jobCtx.Job.VideoID, cookieHeader)
	if err != nil {
		o.logger.Warn("failed to fetch watch page for chat", "err", err, "videoID", jobCtx.Job.VideoID)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}
	if src.Continuation == "" {
		o.logger.Debug("no chat continuation available", "videoID", jobCtx.Job.VideoID, "err", src.Err)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}
	continuation, isReplay, visitorData := src.Continuation, src.IsReplay, src.VisitorData
```

Delete the now-dead `watchResult`/`continuation`/`isReplay`/`visitorData` block it replaces, and keep the rest of the function (the `opts` literal already reads those three locals).

In `internal/worker/stream_processor_youtube.go`, rewrite the head of `tryStartEarlyChat` the same way:

```go
	src, err := chatSourceFor(ctx, info, job.VideoID, cookieHeader)
	if err != nil {
		sp.logger.Debug("failed to fetch watch page for early chat", "err", err, "videoID", job.VideoID)
		return nil, nil
	}
	if src.Continuation == "" {
		sp.logger.Debug("no chat continuation for early chat", "videoID", job.VideoID, "err", src.Err)
		return nil, nil
	}
	continuation, isReplay, visitorData := src.Continuation, src.IsReplay, src.VisitorData
```

Note that this site is usually called with a `probeInfo` from `ProbeVideoStatus` (ANDROID_VR, no watch page), whose `Chat` is the zero value — so it falls back and behaves exactly as before. That is correct and is the reason the freshness gate is cheap insurance rather than the main saving.

- [ ] **Step 5: Run the worker package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./internal/youtube/
```
Expected: PASS. The `orchestrator_chat.go` edit is confined to `setupChatDownloader`; leave `resolveChatOutcome` untouched (Arc E owns it).

- [ ] **Step 6: Write the failing single-pass test**

Append to the utils package's candidate test file (`internal/utils/jsoncandidates_test.go`, or whichever file holds the `FindJSONObjectCandidate` tests):

```go
// TestIsNonEmptyJSONBodyDoesNotValidate pins the split behind row #60. The
// emptiness half is load-bearing on its own — `{}` scans and decodes
// perfectly well, so without it a forged empty object wins the search exactly
// as a forged non-object cannot — but the json.Valid scan is a SECOND full
// pass over a multi-megabyte literal whose caller is about to decode it
// anyway.
//
// Mutants this kills:
//   - IsNonEmptyJSONBody still calling json.Valid → the malformed case fails
//   - the emptiness check dropped                  → the "{}" case fails
func TestIsNonEmptyJSONBodyDoesNotValidate(t *testing.T) {
	if IsNonEmptyJSONBody([]byte(`{}`)) {
		t.Error("an empty object was accepted")
	}
	if IsNonEmptyJSONBody([]byte(`{`)) {
		t.Error("a one-byte fragment was accepted")
	}
	if !IsNonEmptyJSONBody([]byte(`{"a":1}`)) {
		t.Error("a real object was rejected")
	}
	// Deliberately malformed: the caller's typed decode is what rejects this
	// now, so this predicate must NOT.
	if !IsNonEmptyJSONBody([]byte(`{"a":}`)) {
		t.Error("IsNonEmptyJSONBody validated the body — that is the decode's job now")
	}
}
```

And append to `internal/youtube/channel_membership_test.go`:

```go
// TestExtractYtInitialDataDecodeIsTheAcceptance is row #60's behaviour half:
// a candidate that the CONSUMER cannot decode must not end the search, so a
// forged literal before the real assignment denies nothing. (No candidate cap
// — the Arc 3 fix-wave ruling: a cap re-opens the denial the iterator exists
// to close.)
//
// Mutants this kills:
//   - the decode result ignored by the predicate → the forged literal wins
//   - the iterator stopping at the first match   → same
func TestExtractYtInitialDataDecodeIsTheAcceptance(t *testing.T) {
	page := []byte(`<meta name="description" content="x">` +
		`<script>var ytInitialData = {"contents":"not-an-object"};</script>` +
		`<script>var ytInitialData = {"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[]}}};</script>`)

	var env ytInitialTabs
	ok := extractYtInitialDataInto(page, func(obj []byte) bool {
		var cand ytInitialTabs
		if json.Unmarshal(obj, &cand) != nil {
			return false
		}
		env = cand
		return true
	})
	if !ok {
		t.Fatal("the real ytInitialData was not found past the undecodable one")
	}
	if env.Contents.TwoColumnBrowseResultsRenderer.Tabs == nil {
		t.Errorf("decoded the wrong candidate: %+v", env)
	}
}
```

Adjust the fixture's shape to whatever `ytInitialTabs` actually requires — read it in `channel_membership.go` and make the first candidate one that FAILS to decode into it while being valid JSON, and the second one that succeeds with a non-nil `Tabs`. Add `"encoding/json"` to the test file's imports if absent.

- [ ] **Step 7: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestIsNonEmptyJSONBody' ./internal/utils/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestExtractYtInitialDataDecode' ./internal/youtube/
```
Expected: FAIL to compile — `undefined: IsNonEmptyJSONBody`, `undefined: extractYtInitialDataInto`.

- [ ] **Step 8: Make the typed decode the acceptance**

In `internal/utils/jsoncandidates.go`, add beside `IsNonEmptyJSONObject`:

```go
// IsNonEmptyJSONBody is IsNonEmptyJSONObject without the json.Valid scan: it
// checks ONLY that the literal has something between its braces.
//
// For a caller that decodes the literal into a typed envelope immediately
// afterwards, the decode IS the validity test — and json.Valid is a second
// full pass over a multi-megabyte literal (measured at 10.8 ms per watch-page
// fetch on a 4.7 MB page, both passes together). extractPlayerResponse has
// always worked this way. The emptiness half stays: `{}` scans and decodes
// perfectly well, so without it a forged empty object would win the search
// exactly as a forged non-object cannot.
//
// A caller that does NOT decode afterwards must keep using
// IsNonEmptyJSONObject.
func IsNonEmptyJSONBody(obj []byte) bool {
	if len(obj) < 2 {
		return false
	}
	// obj always comes from ScanBalancedJSONObject, so it is at least `{}`.
	return len(bytes.TrimSpace(obj[1:len(obj)-1])) > 0
}
```

In `internal/youtube/channel_membership.go`, replace `extractYtInitialData` with the decode-driven form:

```go
// extractYtInitialDataInto locates the ytInitialData object and offers each
// candidate to decode, which is BOTH the consumer's typed decode and the
// acceptance test — the shape extractPlayerResponse has always used. Reports
// whether any candidate was accepted.
//
// Candidates are iterated, not first-matched (utils.FindJSONObjectCandidate),
// and there is NO cap on how many are tried: a cap re-opens the denial the
// iterator exists to close (Arc 3 fix-wave ruling, 2026-09-15). The acceptance
// is now the decode itself rather than a json.Valid scan followed by that
// decode, which was two full passes over a ~4.4 MB literal per fetch.
func extractYtInitialDataInto(data []byte, decode func(obj []byte) bool) bool {
	_, ok := utils.FindJSONObjectCandidate(data, ytInitialDataAnchors, func(obj []byte) bool {
		return utils.IsNonEmptyJSONBody(obj) && decode(obj)
	})
	return ok
}
```

Rewrite `parseMembershipTab`'s head:

```go
	var env ytInitialTabs
	if !extractYtInitialDataInto(data, func(obj []byte) bool {
		var cand ytInitialTabs
		if json.Unmarshal(obj, &cand) != nil {
			return false
		}
		env = cand
		return true
	}) {
		return nil, false
	}
```
and delete the now-dead `raw, ok := …` + `json.Unmarshal(raw, &env)` lines.

In `internal/youtube/watch_page.go`, rewrite `extractChatContinuation`'s head the same way, preserving its token-FIRST error precedence exactly (Arc 3's I1 ruling — the token must survive a partial type error elsewhere in the renderer):

```go
	var env watchNextChatEnvelope
	var decodeErr error
	if !extractYtInitialDataInto(page, func(obj []byte) bool {
		var cand watchNextChatEnvelope
		err := json.Unmarshal(obj, &cand)
		// Accept a candidate that produced the renderer even when a type error
		// elsewhere in it made Unmarshal return non-nil: the token is what
		// this function exists for, and both consumers skip chat archiving on
		// an empty one, so a YouTube type drift must not cost us chat capture.
		if len(cand.Contents.TwoColumnWatchNextResults.ConversationBar.LiveChatRenderer) == 0 {
			return false
		}
		env, decodeErr = cand, err
		return true
	}) {
		return "", false, fmt.Errorf("ytInitialData with a liveChatRenderer not found")
	}
	rendererRaw := env.Contents.TwoColumnWatchNextResults.ConversationBar.LiveChatRenderer
	if string(rendererRaw) == "null" {
		if decodeErr != nil {
			return "", false, fmt.Errorf("parse ytInitialData: %w", decodeErr)
		}
		return "", false, fmt.Errorf("no liveChatRenderer found")
	}
```

then continue with the existing token-reading body unchanged, substituting `decodeErr` wherever it used `err`.

- [ ] **Step 9: Run every affected package green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/utils/ ./internal/youtube/ ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
```
Expected: PASS — `watch_page_test.go`'s existing chat-continuation shape table must still pass unchanged; it is the pin on the error precedence above.

- [ ] **Step 10: Commit**

```bash
git add internal/youtube/types.go internal/youtube/player_api_strategy.go \
        internal/youtube/watch_page.go internal/youtube/channel_membership.go \
        internal/youtube/channel_membership_test.go internal/utils/jsoncandidates.go \
        internal/utils/jsoncandidates_test.go internal/worker/orchestrator_chat.go \
        internal/worker/stream_processor_youtube.go internal/worker/chat_source_test.go
git commit -m "perf(youtube,worker): reuse the watch page for chat; one pass over ytInitialData

YOUTUBE-10: FetchWatchPage already extracts the chat continuation, replay flag
and visitor data on every call and both cascades threw them away, so job start
and every early-chat restart fetched a second 1-5 MB authenticated page.
VideoInfo now carries them (ChatSource, 2-minute freshness) and both chat-start
sites fall back to a fetch only when the source is missing or stale.
YOUTUBE-15: the ytInitialData candidate was json.Valid-scanned and THEN
typed-decoded, two full passes over a ~4.4 MB literal (10.8 ms/fetch measured);
the typed decode is now the acceptance, as extractPlayerResponse has always
done. No candidate cap. Rows #56, #60.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/youtube/types.go internal/youtube/player_api_strategy.go \
     internal/youtube/watch_page.go internal/youtube/channel_membership.go \
     internal/youtube/channel_membership_test.go internal/utils/jsoncandidates.go \
     internal/utils/jsoncandidates_test.go internal/worker/orchestrator_chat.go \
     internal/worker/stream_processor_youtube.go internal/worker/chat_source_test.go
```

---

## Task 11: Delete the dead clients, fix the stale doc comment, close the plan (row #102: YOUTUBE-13 + YOUTUBE-9's doc half)

Row #102's two Arc-Y items:

- **YOUTUBE-13** — `IOSClient` and `WebRemixClient` (`constants.go:236-270`) have no production caller; `grep` finds them only in `constants_test.go`. Their comments cite an audit proposal ("reports/youtube.md T2") that was never wired, and their pinned versions rot silently.
- **YOUTUBE-9's doc half** — `WatchPageResult.AttestationChallenge`'s comment still claims the value is "used to mint session-coherent GVS PO tokens". It is not: the Arc 3 owner ruling R1 deleted that path, and the field's **only** readers are two Debug lines that test it for emptiness and report `AttestationReason`. **The field itself STAYS** (Arc 3 ruling, explicitly re-affirmed) — only the comment is wrong. `platform-services.md:926` already says the right thing.

This task also closes the arc: it makes the last doc pass and deletes this plan.

**Files:**
- Modify: `internal/constants/constants.go`
- Modify: `internal/constants/constants_test.go`
- Modify: `internal/youtube/watch_page.go` (the `AttestationChallenge` comment)
- Modify: `docs/spec/platform-services.md` (the sidecar supervisor paragraph)
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-y-youtube.md`

**Interfaces:**
- Removes: `constants.IOSClient`, `constants.WebRemixClient`, `TestIOSClientInternallyConsistent`.
- Consumes: everything from Tasks 1–10 is already in place.

- [ ] **Step 1: Prove they have no caller**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp git grep -n "IOSClient\|WebRemixClient" -- '*.go' '*.md' | grep -v docs/superpowers/
```
Expected: only `internal/constants/constants.go` and `internal/constants/constants_test.go`. If anything else appears, STOP and report it — the row's premise would be wrong.

- [ ] **Step 2: Write the failing test**

Append to `internal/constants/constants_test.go`:

```go
// TestClientRosterIsExactlyTheWiredClients pins the roster against re-growing
// dead weight. IOSClient and WebRemixClient sat in this file for months with
// no production caller, citing an audit proposal ("reports/youtube.md T2")
// that was never wired, their pinned clientVersions rotting silently — which
// is worse than absent, because a reader takes a pinned version for a
// maintained one.
//
// The list below is every client with a real call site in internal/youtube.
// Adding one here without wiring it, or reinstating either deleted constant,
// makes this test and the grep in the task's Step 1 disagree.
//
// Mutant this kills: a count or a name changed without a call site → the
// length assertion fails, or the file no longer compiles.
func TestClientRosterIsExactlyTheWiredClients(t *testing.T) {
	wired := map[string]YouTubeClientConfig{
		"TVDowngradedClient": TVDowngradedClient,
		"WebCreatorClient":   WebCreatorClient,
		"WebClient":          WebClient,
		"WebSafariClient":    WebSafariClient,
		"WebEmbeddedClient":  WebEmbeddedClient,
		"VisionOSClient":     VisionOSClient,
		"AndroidVRClient":    AndroidVRClient,
	}
	if len(wired) != 7 {
		t.Fatalf("the roster lists %d clients, want 7 — every one must have a production call site", len(wired))
	}
	for name, c := range wired {
		if c.ClientName == "" || c.ClientID == "" || c.ClientVersion == "" {
			t.Errorf("%s is incompletely defined: %+v", name, c)
		}
		if c.Context["clientName"] != c.ClientName {
			t.Errorf("%s: Context[clientName] = %v, want %q", name, c.Context["clientName"], c.ClientName)
		}
	}
}
```

- [ ] **Step 3: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestClientRoster ./internal/constants/
```
Expected: PASS (the seven wired clients are all correctly defined today). This test is a GUARD, not a red-then-green step: the deletion in Step 4 is what this task actually proves, and the proof is that `go build ./...` and the grep in Step 1 still agree afterwards. Run Step 4, then Step 8, and confirm both.

- [ ] **Step 4: Delete the two clients**

In `internal/constants/constants.go`, delete the `IOSClient` and `WebRemixClient` blocks in full, comments included.

In `internal/constants/constants_test.go`, delete `TestIOSClientInternallyConsistent` in full. **Keep** `TestNativeAppUserAgentsNotStale` and keep `UserAgents.IOS`: that test is the only pin on the native-app UA majors and reads `UserAgents.IOS` and `UserAgents.Android` directly, so the UA strings are still referenced and still checked for staleness. Adjust `TestWebClientInternallyConsistent`'s doc comment, which opens by naming `TestIOSClientInternallyConsistent` as the shape it mirrors — replace that sentence with a self-contained one:

```go
// TestWebClientInternallyConsistent, TestWebSafariClientInternallyConsistent,
// and TestWebEmbeddedClientInternallyConsistent each assert that a client
// struct's ClientVersion field agrees with its own Innertube
// Context["clientVersion"] entry, or YouTube sees a contradictory fingerprint
// from the same process.
//
// They intentionally omit a UA-contains-version check. That only makes sense
// for native-app clients, whose UserAgent literally embeds the app version
// yt-dlp's clientVersion tracks (e.g. "com.google.ios.youtube/21.26.4"); the
// WEB-family clients' UserAgent is a real browser UA whose version numbering
// is unrelated to the Innertube WEB build number, so there is nothing for such
// an assertion to pin here. TestNativeAppUserAgentsNotStale covers the native
// UA strings themselves.
```

- [ ] **Step 5: Fix the `AttestationChallenge` doc comment**

In `internal/youtube/watch_page.go`, replace the `AttestationChallenge` field comment:

```go
	// AttestationChallenge is the compact JSON of the BotGuard bgChallenge
	// YouTube embedded in this page load via window.ytAtN(...) — the session's
	// own attestation challenge (moonarchive 96344fe parity). Empty when the
	// page did not carry one or it failed to parse.
	//
	// NOT used to mint anything. The challenge-sourced GVS mint it was
	// extracted for was deleted by owner ruling R1 (2026-09-15); the field's
	// only readers are the two "no attestation challenge from watch page"
	// Debug lines in player_api_strategy.go, which test it for emptiness and
	// report AttestationReason. It is kept deliberately (that same ruling) so
	// restoring the path is a one-line call, and because the reason string
	// beside it is the diagnostic a premiere's 403s would be read from.
	AttestationChallenge string
```

- [ ] **Step 6: Document the supervisor in `platform-services.md`**

In the `#### Configuration` subsection of the BotGuard part, after "Disabling the sidecar reverts to the websafe-fallback-only path…", add:

```markdown
#### Supervision

The sidecar is supervised (`internal/bgutils/sidecar/supervisor.go`). A child that crashes or is OOM-aborted by V8 — the shipped `sidecar_hard_limit_mb` of 512 sits just above the documented 400-500 MB BotGuard burst — fires `Config.OnUnhealthy` from `readPump`, and `Supervisor` restarts it IN PLACE (`Sidecar.Restart`, which preserves the handle `cmd/moombox` holds for shutdown and `PotProvider` holds for minting) on a 5 s / 15 s / 60 s / 5 min ladder whose last step repeats. After each success the two consumers are re-wired: `PotProvider.SetSidecar` and a freshly built `cipher.NewSidecarSolver` installed through `cipher.SidecarSwappable.SetSidecar` — rebuilt rather than reused, because the old solver's per-player "already sent" map describes the dead child's memory.

Without it the failure was permanent for the life of a 24/7 process: `markUnhealthy` latched, and because sig is sidecar-only (no goja fallback) every signature-ciphered format became unresolvable while PO tokens fell to the goja path, which rejects the websafe-only response. The outage is now visible while it lasts — `sidecar.PublishHealth` feeds `/api/status`'s `botguardSidecar` object (absent when the sidecar is disabled) and, through `SubscribeHealth`, the TUI status bar's red `SIDECAR DOWN` / `POT` alert.
```

- [ ] **Step 7: Delete this plan**

```bash
git rm docs/superpowers/plans/2026-09-17-sweep2-y-youtube.md
```

The spec it argues from (`docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md`) stays; the ledger under `.superpowers/sdd/` records what happened. An implemented plan is deleted, not archived — git history is the archive.

- [ ] **Step 8: Run the full per-task gate set**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/constants/ ./internal/youtube/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
```
Expected: PASS. `./internal/docs/` is the gate that matters here — two symbols were deleted and three spec docs edited.

- [ ] **Step 9: Commit**

```bash
git add internal/constants/constants.go internal/constants/constants_test.go \
        internal/youtube/watch_page.go docs/spec/platform-services.md \
        docs/superpowers/plans/2026-09-17-sweep2-y-youtube.md
git commit -m "chore(youtube): delete the unwired clients, fix the attestation doc, close Arc Y

IOSClient and WebRemixClient had no production caller and cited an audit
proposal that was never wired, so their pinned clientVersions rotted silently
— worse than absent, because a pinned version reads as a maintained one.
WatchPageResult.AttestationChallenge's comment still claimed it mints
session-coherent GVS tokens; the field STAYS (Arc 3 ruling R1) but its only
readers test it for emptiness. platform-services.md documents the new sidecar
supervision. Plan deleted — git history is the archive. Row #102
(YOUTUBE-13, YOUTUBE-9's doc half).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
  -- internal/constants/constants.go internal/constants/constants_test.go \
     internal/youtube/watch_page.go docs/spec/platform-services.md \
     docs/superpowers/plans/2026-09-17-sweep2-y-youtube.md
```

---

## Merge-candidate checklist (controller, after Task 11)

1. `git merge main` into `sweep2-y-youtube` first.
2. `gofmt -l ./cmd ./internal ./tools ./web` — must print nothing.
3. `GOTMPDIR=… go vet ./...` on Windows **and** `GOOS=linux GOARCH=amd64 GOTMPDIR=… go vet ./...`.
4. `staticcheck ./...` — a hard gate; U1000 catches any helper this plan left unused.
5. Three builds: Windows x64, `GOOS=linux GOARCH=amd64`, `GOOS=linux GOARCH=arm64`.
6. `cd web/tests && node --test *.test.mjs`.
7. ONE `GOTMPDIR=… go test -count=1 ./...`.
8. The arc's live gates:
   ```bash
   GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_YT_TEST=1 \
     go test -count=1 -run 'TestLivePublicExtraction|TestLiveLoginMarkersPresent' ./internal/youtube/
   GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_CIPHER_TEST=1 \
     go test -count=1 -timeout 180s -run TestSidecarSolver ./internal/cipher/
   ```
   The live extraction gate asserts CAPABILITIES, not mechanisms: a public VOD yields formats, a public live yields a segment-addressable pool, login markers are present. Tasks 3, 4 and 7 all change what the cascade does, so a change in the CLIENT a format came from is not a regression — a lost capability is.
9. `MOOMBOX_LIVE_BG_TEST` is **not** a gate: the goja BotGuard live test is a known, long-standing failure (the sidecar is the default path). Use the cipher + sidecar gates above instead.
