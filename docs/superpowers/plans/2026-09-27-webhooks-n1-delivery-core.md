# Arc N1 — Discord webhook delivery core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the notification delivery core so an embed can never be silently lost, reordered, or rejected by Discord for being too long — and give every producer a seam a test can substitute. Nine tasks: a `Sender`/`Notifier` interface plus a `notificationtest.Recorder`; `SendOptions` grown to carry author / platform / job id / tier / mention; rune-boundary clamps to every Discord limit plus `EscapeMarkdown`; a per-target FIFO sender goroutine replacing the 16-slot semaphore; rate-bucket awareness and single-attempt shutdown sends; four silent defects fixed at their sites (`muxing` for multi-part jobs, `scheduled` only when upcoming, the auth all-clear with nothing parked, `connectivity_pause` retired into the resume embed); and the docs made truthful.

**Architecture:** The manager stops being a fan-out of goroutines and becomes a fan-out of **queues**. `buildTargets` still resolves the config into `notificationTarget` values (sender + event filter + the resolved webhook URL as a dedupe key); `Manager.applyTargets` then turns that list into `*targetQueue` values, each with its own bounded FIFO and one goroutine draining it. `Send` snapshots the queue slice under an RWMutex, filters, and appends — never blocking the caller, never spawning. Ordering per target is free (one goroutine), and the overflow policy can choose its victim because the queue is a slice under a mutex rather than a channel. `Reload` diffs by resolved URL so a surviving target keeps its goroutine, its pending items **and** the rate bucket its `DiscordWebhook` learned; a removed target discards its queue after its in-flight item. Rate-limit awareness lives in `DiscordWebhook` rather than in the manager, because the sender goroutine's blocking `Send` call absorbs the sleep either way and the manager stays transport-agnostic. Payload hardening is one function, `clampEmbed`, applied inside `buildPayload` so every one of the 36 send sites is covered without touching any of them.

The test seam is two interfaces in `internal/notifications`: `Sender` (the one method 36 producers call) and `Notifier` (owner surface: `Sender` plus `HasTargets`/`Reload`/`BeginShutdown`/`Wait`, which only `cmd/moombox` drives). `*Manager` satisfies both; the new `internal/notifications/notificationtest.Recorder` satisfies both too, so a worker test, a routes test and a `cmd/moombox` test can all assert on the embed a real Discord target would have received.

**Tech Stack:** Go 1.27, module `github.com/vampiricwulf/Moombox`. Standard library only for everything this arc adds (`sync`, `sync/atomic`, `time`, `strconv`, `strings`, `unicode/utf8`, `net/http`, `net/http/httptest`). No new dependency, no config key, no REST route, no database change, no `web/tests` suite change beyond one deleted line of vocabulary in `web/public/modules/settings.js`.

**Spec:** `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` (§0 rulings, §1 Arc N1, §5 order) at commit `1d2df1d4`. The read-only audit behind it is `.superpowers/sdd/2026-09-26-webhooks/audit.md` (gitignored): §1 the 46-row inventory + the delivery-mechanics table, §2 A4/A7, §3 C6/C8 + Content quality + Reliability, §4 the config/UI/docs/test seams.

---

## Global Constraints

Every task's requirements implicitly include this section.

**From the spec's §1 Constraints:**

- **The DB layer is untouched.** No migration, no schema change, no new column, no change to `UpdateJobFields` cadence or subscriber fan-out. Arc N3 adds the `notification_msgs` column; N1 adds nothing. (`feedback_db_durability_full`: the owner declined all database-layer changes.)
- **The update path is untouched.** Nothing in `cmd/moombox/launcher*.go`, `internal/updater/`, the exit codes, or the swap artifacts changes. The only `cmd/moombox/shutdown.go` edit is one added `BeginShutdown()` call **before** the existing force-exit timer's window is spent; the 10 s timer, its exit codes and every `stopService` call keep their exact shapes.
- **Every event key that exists today keeps its meaning, except `connectivity_pause`** — retired from `EventGroups`, kept working as a legacy filter entry through `eventAliases["connectivity_resume"] = "connectivity_pause"`, and kept out of the "unknown event" warning by `KnownEvents` including alias values.
- **No new config keys in N1.** `network.public_url`, per-target `enabled` / `mention` / `mention_events` and `mode` are N2b's and N3's. N1 defines the `SendOptions.Mention` / `MentionAllowed` **fields** and the `content` + `allowed_mentions` **payload shape**; nothing sets them yet.
- **The TUI import fence.** `internal/tui` may not import `internal/web` or `internal/bgutils` (`internal/tui/openbrowser_windows.go:37`, `internal/tui/app.go:377`). No task here adds an import to `internal/tui` at all — the TUI's notification event editor derives from `notifications.EventGroups` (`internal/tui/settings.go:270-283`), so Task 7's vocabulary change reaches it with no TUI edit.
- **LF line endings** on every file created or edited (`.gitattributes` pins `*.go`, `*.js`, `*.md`).
- **The citation gate.** Any backticked symbol in a doc that is immediately followed by a backticked repo path must be **declared** in that file (`internal/docs/citations_test.go` `citationProblem`), and every backticked repo path must exist. Run `go test ./internal/docs/` after any doc edit.

**Standing project rules:**

- **Go 1.27, no CGo.** `go.mod` says `go 1.27` / `toolchain go1.27.1`; nothing here changes either.
- **Three builds must stay green:** `go build ./...` (host), and the two cross targets
  ```bash
  GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/moombox
  GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/moombox
  ```
- **`go mod tidy -diff` must print nothing.** No task adds a module requirement.
- **Logger interface stays anonymous.** Any struct needing a logger repeats the four-method anonymous interface inline (`Debug`/`Info`/`Warn`/`Error`, each `(msg string, args ...any)`). Do **not** extract a named interface. Task 4's `targetQueue` repeats it; that is deliberate.
- **Every goroutine gets an inline `defer func() { if r := recover(); ... }()`.** Task 4 starts one goroutine per target: `targetQueue.run` carries a top-level recover **and** `targetQueue.deliver` carries its own, so a panic in one send does not kill the goroutine and strand the queue.
- **One commit per task, with a pathspec.** `git commit -F <message-file> -- <exact files>`. Never a bare `git commit` — a shared working tree has swept another task's staged files before (`feedback_pathspec_on_commit`). Write the message file under `.superpowers/gotmp/` (gitignored) or the scratchpad, never inside the tracked tree.
- **Commit trailers, verbatim, on every commit in this plan:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
  ```
- **Implementers never run the full suite.** No `go test ./...`. Run only the narrow package the task touches, always with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Task 9 runs the arc's gate set, and it is still package-scoped.
- **Never `python3 -` heredocs** (they hang forever under the uv shim). Edit files with the Write/Edit tools, not with `sed -i`/`perl -i`. `ls` is aliased — use `command ls`.
- **Docs are edited in the task that makes them true** where the doc is *about that task's code*; the three docs whose whole Notifications section is rewritten (operations.md, README.md, SPEC.md) are Task 8, because six earlier tasks all move the same paragraphs.

---

## Task 1: The `Sender` / `Notifier` seam and `notificationtest.Recorder`

**Files:**
- Modify: `internal/notifications/manager.go:130-136` (the `SendOptions` doc block is where the two interfaces go, just above it), `:381-382` (`Send`'s signature line — add the nil-receiver guard)
- Create: `internal/notifications/notificationtest/recorder.go`
- Modify: `internal/worker/orchestrator.go:69` (field), `:95` (constructor parameter)
- Modify: `internal/worker/stream_processor.go:92` (field), `:154` (`SetNotifier` parameter)
- Modify: `internal/worker/trim.go:27` (field), `:54` (`SetNotifier` parameter)
- Modify: `internal/worker/worker.go:218` (field), `:282` (`DownloadWorkerDeps.Notifier`), `:301` (`var nm`)
- Modify: `internal/web/routes/jobs.go:187` (`JobRoutes`' last parameter)
- Modify: `cmd/moombox/runstate.go:68` (`notifyMgr` field)
- Modify: `cmd/moombox/helpers.go:121` (`checkAndBroadcastUpdate`'s `notifyMgr` parameter)
- Test: `internal/worker/notifier_seam_test.go` (new)

**Interfaces:**
- Consumes: nothing from other tasks (this is the first task).
- Produces, and every later task depends on them:
  ```go
  // in package notifications
  type Sender interface {
      Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions)
  }
  type Notifier interface {
      Sender
      HasTargets() bool
      Reload(cfg *config.MoomboxConfig)
      BeginShutdown()
      Wait()
  }
  ```
  ```go
  // in package notifications — declared EMPTY here so Notifier compiles;
  // Task 4 replaces the body.
  func (m *Manager) BeginShutdown()
  ```
  ```go
  // in package notificationtest
  type Call struct {
      Title       string
      Description string
      Type        notifications.NotificationType
      Fields      []notifications.Field
      Opts        notifications.SendOptions
  }
  func (c Call) Field(name string) (string, bool)  // Task 7's resume-embed test reads it
  func New() *Recorder
  func (r *Recorder) Send(title, description string, ntype notifications.NotificationType, fields []notifications.Field, opts notifications.SendOptions)
  func (r *Recorder) Calls() []Call
  func (r *Recorder) ByEvent(event string) []Call
  func (r *Recorder) Reset()
  func (r *Recorder) HasTargets() bool
  func (r *Recorder) Reload(*config.MoomboxConfig)
  func (r *Recorder) BeginShutdown()
  func (r *Recorder) Wait()
  ```
  Changed signatures other tasks and packages must use:
  ```go
  func NewDownloadOrchestrator(db *database.Database, queue *JobQueue, ffmpegPath string, logger logger, cs *cipher.GojaResolver, routedCs cipher.Solver, pp *bgutils.PotProvider, nm notifications.Sender, conn Connectivity) *DownloadOrchestrator
  func (sp *StreamProcessor) SetNotifier(nm notifications.Sender)
  func (ts *TrimService) SetNotifier(nm notifications.Sender)
  func JobRoutes(r chi.Router, db *database.Database, store *config.Store, w *worker.DownloadWorker, rl *web.RateLimiter, twitchFetcher TwitchMetadataFetcher, ytFetcher YouTubeMetadataFetcher, notifier notifications.Sender)
  func checkAndBroadcastUpdate(ctx context.Context, upd *updater.Updater, wsHub *web.WebSocketHub, notifyMgr notifications.Notifier, tuiCh chan<- tui.UpdateStatusMsg, log *logger.Logger, configStore *config.Store, lastNotifiedTag *string)
  ```

**Why two interfaces and not one.** The spec writes `Sender` with exactly one method, and 36 of the 37 send sites need exactly that. But `cmd/moombox` also calls `HasTargets()` (`helpers.go:171`, `main.go:783`, `monitor_callbacks.go:1367`, `:1458`), `Reload` (`routes_wiring.go:84`, `tui_wiring.go:418`) and `Wait` (`shutdown.go:75`) on the same field, so typing `runState.notifyMgr` as a bare `Sender` does not compile. `Notifier` is a faithful superset — it embeds `Sender` verbatim, so the spec's interface is the one every producer holds, and the owner surface is named separately.

**Why `Manager.Send` needs a nil-receiver guard now.** Today `worker.go:301` is `var nm *notifications.Manager`, and every consumer guards with `if x.notifier != nil`. Once the field is an interface, a `deps.Notifier` holding a **typed nil** `*Manager` makes `!= nil` true and the guard useless — `internal/worker/worker.go:313`'s `if nm != nil { sp.SetNotifier(nm) }` would wave one through. (An *untyped* `nil` assigned to an interface yields a genuinely nil interface and is caught; only a typed nil is not. `internal/web/routes/jobs_test.go:95` passes the untyped form and is fine.) The spec's rule — "`Send` on a nil `*Manager` stays a no-op" — is what makes the typed case safe, and it is one line.

**Why `BeginShutdown` is declared empty here.** `Notifier` names it, and `cmd/moombox/runstate.go:68` becomes `notifications.Notifier` in this task — so without the method, `cmd/moombox/services.go:827` (`s.notifyMgr = notifyMgr`) fails with *"missing method BeginShutdown"* and neither Step 6's build nor `TestRecorderSatisfiesBothInterfaces` can pass. Task 4 replaces the empty body with the atomic store; nothing calls it until Task 5 wires `shutdown.go`.

- [ ] **Step 1: Write the failing test**

Create `internal/worker/notifier_seam_test.go`:

```go
package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestStreamProcessorNotifiesThroughTheSenderSeam is the whole point of the
// seam: 43 of the 46 trigger rows the audit inventoried have no test because
// every producer holds the CONCRETE *notifications.Manager, whose only
// constructor builds targets from Discord webhook URLs. Nothing outside the
// notifications package can substitute a recorder, so nothing asserts what an
// operator actually receives.
//
// This test does not assert the embed's content — that is Task 6's job. It
// asserts that a recorder can be INSTALLED at all, which is the change.
//
// THE MUTANT: revert any one of the field/parameter types to
// *notifications.Manager and this file stops compiling.
func TestStreamProcessorNotifiesThroughTheSenderSeam(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rec := notificationtest.New()
	sp := &StreamProcessor{db: db, notifier: rec}

	job := &database.Job{ID: "yt_seam", VideoID: "seam", Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob("yt_seam")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	sp.updateJobMetadata(stored, &youtube.VideoInfo{
		Title:              "A Scheduled Stream",
		ChannelName:        "A Channel",
		ScheduledStartTime: "2026-10-01T12:00:00Z",
		IsUpcoming:         true,
	}, false)

	if got := rec.ByEvent("scheduled"); len(got) != 1 {
		t.Fatalf("recorded %d \"scheduled\" notifications, want 1 — the seam did not carry the send: %+v", len(got), rec.Calls())
	}
}

// TestRecorderSatisfiesBothInterfaces pins the two seams by assignment. A
// Recorder that stops satisfying Notifier cannot stand in for runState's
// notifyMgr, which is where cmd/moombox's defect tests install it (Task 6).
func TestRecorderSatisfiesBothInterfaces(t *testing.T) {
	var _ notifications.Sender = notificationtest.New()
	var _ notifications.Notifier = notificationtest.New()
	var _ notifications.Sender = (*notifications.Manager)(nil)
	var _ notifications.Notifier = (*notifications.Manager)(nil)
}
```

Add `"github.com/vampiricwulf/Moombox/internal/notifications"` to that file's import block (the second test needs it).

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestStreamProcessorNotifiesThroughTheSenderSeam ./internal/worker/`

Expected: a **build failure**, not a test failure —
```
# github.com/vampiricwulf/Moombox/internal/worker [github.com/vampiricwulf/Moombox/internal/worker.test]
internal/worker/notifier_seam_test.go:31:34: cannot use rec (variable of type *notificationtest.Recorder) as *notifications.Manager value in struct literal
FAIL	github.com/vampiricwulf/Moombox/internal/worker [build failed]
```
(plus `undefined: notifications.Sender` / `undefined: notifications.Notifier` from the second test, and `package .../notificationtest is not in std` before Step 3 creates it).

- [ ] **Step 3: Declare the two interfaces and nil-guard `Send`**

In `internal/notifications/manager.go`, insert immediately **above** the `// SendOptions provides optional parameters for a notification.` comment (currently `:130`):

```go
// Sender is the one method every notification producer needs, and the seam
// every producer holds instead of *Manager.
//
// Before it existed, worker, routes and cmd each held the concrete manager,
// whose only constructor resolves Discord webhook URLs — so 43 of the 46
// trigger rows an audit inventoried had no test that could assert the embed,
// and the three that did were pure renderers that never reached a Send.
// internal/notifications/notificationtest.Recorder is the test implementation.
type Sender interface {
	Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions)
}

// Notifier is the OWNER surface: a Sender plus the three lifecycle calls only
// cmd/moombox makes — the cost gate before building an embed (HasTargets), the
// config hot-apply (Reload), and the shutdown pair (BeginShutdown then Wait).
//
// Separate from Sender on purpose. A producer that could call Reload could
// reload the targets from a download goroutine; a producer that could call
// Wait could block a hot path on Discord. Handing every producer the narrow
// half is what keeps that impossible.
type Notifier interface {
	Sender
	HasTargets() bool
	Reload(cfg *config.MoomboxConfig)
	BeginShutdown()
	Wait()
}
```

Then, in `Send` (currently `:382`), add the guard as the first statement:

```go
// Send dispatches a notification to all matching targets asynchronously.
//
// A nil *Manager is a no-op. Every consumer field is the Sender interface now,
// and a TYPED nil — a (*Manager)(nil) assigned into one — produces a NON-nil
// interface holding a nil pointer, which the `if x.notifier != nil` guards at
// ~20 call sites wave straight through (internal/worker/worker.go:313 is the
// live example). Every production assignment today passes a real *Manager, so
// this is insurance against a future typed nil rather than a live bug — and it
// is still required, because nothing else would catch one.
func (m *Manager) Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) {
	if m == nil {
		return
	}
```

Then, immediately below `Send`, declare the shutdown flag's method with an **empty body**:

```go
// BeginShutdown puts every target into single-attempt mode.
//
// Declared here rather than with the queue it drives (Task 4), because
// Notifier names it and cmd/moombox's runState field is typed Notifier from
// this task onward — without it *Manager does not satisfy the interface and
// cmd/moombox/services.go:827 stops compiling. Task 4 gives it the atomic flag.
func (m *Manager) BeginShutdown() {}
```

- [ ] **Step 4: Create the recorder package**

Create `internal/notifications/notificationtest/recorder.go`:

```go
// Package notificationtest provides a Recorder that stands in for
// notifications.Sender (and notifications.Notifier) in tests outside
// internal/notifications.
//
// It is an ordinary package rather than a _test.go helper because the
// consumers live in four packages — internal/worker, internal/web/routes,
// cmd/moombox and internal/notifications' own tests — and a test-only file
// cannot be imported across package boundaries. Nothing in production imports
// it, so the linker drops it from the binary. internal/webtest is the
// precedent.
package notificationtest

import (
	"sync"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// Call is one delivered notification, recorded exactly as the manager would
// have handed it to a Discord target.
type Call struct {
	Title       string
	Description string
	Type        notifications.NotificationType
	Fields      []notifications.Field
	Opts        notifications.SendOptions
}

// Field returns the value of the named embed field and whether it was present.
// Assertions read better as `got, ok := call.Field("Channel")` than as a loop
// at every site.
func (c Call) Field(name string) (string, bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

// Recorder implements notifications.Notifier (and therefore Sender) by
// recording every Send. Safe for concurrent use: producers notify from monitor,
// worker and HTTP goroutines, and a test that installs one recorder across
// several of them would otherwise race.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

// New returns an empty Recorder.
func New() *Recorder { return &Recorder{} }

// Send records the notification. It never blocks and never fails — the whole
// point is that a producer's hot path behaves in a test exactly as it does in
// production, where Send is a queue append.
func (r *Recorder) Send(title, description string, ntype notifications.NotificationType,
	fields []notifications.Field, opts notifications.SendOptions,
) {
	// Copy the fields slice: producers build it with append and several reuse
	// a FieldBuilder, so keeping the caller's backing array would let a later
	// send rewrite an earlier recorded call.
	cp := append([]notifications.Field(nil), fields...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{
		Title:       title,
		Description: description,
		Type:        ntype,
		Fields:      cp,
		Opts:        opts,
	})
}

// Calls returns a copy of everything recorded so far, in send order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// ByEvent returns the recorded calls whose SendOptions.Event equals event, in
// send order. Exact match, deliberately: alias resolution is the manager's
// filter concern, and a test asserting "the producer emitted disk_critical"
// must not pass because a disk_warning went out.
func (r *Recorder) ByEvent(event string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if c.Opts.Event == event {
			out = append(out, c)
		}
	}
	return out
}

// Reset drops everything recorded. For a table test that drives one producer
// through several states with one recorder.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// HasTargets reports true so a producer's `if notifier.HasTargets()` cost gate
// does not skip the send under test. A test that wants the gate closed passes
// no notifier at all.
func (r *Recorder) HasTargets() bool { return true }

// Reload, BeginShutdown and Wait are the owner-surface no-ops that let a
// Recorder stand in for runState.notifyMgr, which is typed notifications.Notifier.
func (r *Recorder) Reload(*config.MoomboxConfig) {}

// BeginShutdown is a no-op: a Recorder has no delivery to degrade.
func (r *Recorder) BeginShutdown() {}

// Wait is a no-op: a Recorder has no queue to drain.
func (r *Recorder) Wait() {}
```

- [ ] **Step 5: Retype every consumer**

Seven files, nine declarations. Each is a type swap only — no logic moves.

`internal/worker/orchestrator.go:69`:
```go
	notifier     notifications.Sender
```
`internal/worker/orchestrator.go:95` (the parameter in `NewDownloadOrchestrator`):
```go
func NewDownloadOrchestrator(db *database.Database, queue *JobQueue, ffmpegPath string, logger logger, cs *cipher.GojaResolver, routedCs cipher.Solver, pp *bgutils.PotProvider, nm notifications.Sender, conn Connectivity) *DownloadOrchestrator {
```

`internal/worker/stream_processor.go:92`:
```go
	notifier    notifications.Sender
```
`internal/worker/stream_processor.go:154`:
```go
func (sp *StreamProcessor) SetNotifier(nm notifications.Sender) {
```

`internal/worker/trim.go:27`:
```go
	notifier  notifications.Sender
```
`internal/worker/trim.go:54`:
```go
func (ts *TrimService) SetNotifier(nm notifications.Sender) {
```

`internal/worker/worker.go:218`:
```go
	notifier    notifications.Sender
```
`internal/worker/worker.go:282` (inside `DownloadWorkerDeps`):
```go
	Notifier      notifications.Sender
```
`internal/worker/worker.go:301`:
```go
	var nm notifications.Sender
```

`internal/web/routes/jobs.go:187`:
```go
func JobRoutes(r chi.Router, db *database.Database, store *config.Store, w *worker.DownloadWorker, rl *web.RateLimiter, twitchFetcher TwitchMetadataFetcher, ytFetcher YouTubeMetadataFetcher, notifier notifications.Sender) {
```
(`internal/web/routes/jobs_test.go:95` passes an untyped `nil` for this argument and keeps compiling — an untyped nil is a valid nil interface, and the three `if notifier != nil` guards at `:771`, `:859` and `:953` still read false.)

`cmd/moombox/runstate.go:68`:
```go
	notifyMgr notifications.Notifier
```

`cmd/moombox/helpers.go:121`:
```go
	notifyMgr notifications.Notifier,
```

Nothing else changes. `cmd/moombox/services.go:826-827` assigns a `*notifications.Manager` into the interface field; `cmd/moombox/main.go:243`'s `notifyMgr = s.notifyMgr` infers `notifications.Notifier` and keeps both its `HasTargets` (`:783`) and its `Send` calls; `cmd/moombox/addvideo.go:59` keeps its own local `*Manager` (it constructs one and `Wait`s on it) and is out of scope.

- [ ] **Step 6: Run the test to verify it passes**

Run:
```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestStreamProcessorNotifiesThroughTheSenderSeam|TestRecorderSatisfiesBothInterfaces' ./internal/worker/
```
Expected:
```
ok  	github.com/vampiricwulf/Moombox/internal/worker	<time>
```

Then the packages whose signatures moved:
```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/... ./internal/web/routes/ ./cmd/moombox/
```
Expected: three `ok` lines. (`internal/notifications/notificationtest` has no test file and reports `?   ... [no test files]` — that is expected, not a failure.)

- [ ] **Step 7: Commit**

```bash
cat > .superpowers/gotmp/n1-t1.msg <<'EOF'
feat(notifications): add the Sender/Notifier seam and notificationtest.Recorder

Every producer held the concrete *notifications.Manager, whose only
constructor resolves Discord webhook URLs, so no test outside the package
could substitute a recorder: 43 of 46 trigger rows were unasserted.

Sender is the one method producers need; Notifier is the owner surface
cmd/moombox drives (HasTargets/Reload/BeginShutdown/Wait). Manager.Send now
no-ops on a nil receiver, because an interface field holding a typed-nil
*Manager defeats the `!= nil` guards at ~20 call sites.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t1.msg -- \
  internal/notifications/manager.go \
  internal/notifications/notificationtest/recorder.go \
  internal/worker/orchestrator.go \
  internal/worker/stream_processor.go \
  internal/worker/trim.go \
  internal/worker/worker.go \
  internal/worker/notifier_seam_test.go \
  internal/web/routes/jobs.go \
  cmd/moombox/runstate.go \
  cmd/moombox/helpers.go
```

---
## Task 2: `SendOptions` grows, and the payload gains author, footer and mention

**Files:**
- Modify: `internal/notifications/manager.go:130-136` (`SendOptions`) — and the stale example event in its `Event` comment
- Modify: `internal/notifications/discord.go:61-88` (payload structs), `:90-123` (`buildPayload`)
- Create: `internal/notifications/payload_test.go`
- Modify: `internal/worker/orchestrator_mux.go:1043`, `:1283` (the two `Image:` lines) and add one helper beside `sendFinishedNotification`
- Test: `internal/worker/orchestrator_mux_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `Sender` (nothing else — this task is inside the package plus two producer lines).
- Produces, read by Tasks 3, 4, 5 and 8:
  ```go
  type Author struct {
      Name    string
      IconURL string
      URL     string
  }

  type Tier int
  const (
      TierUnset  Tier = iota // derive from Event
      TierNormal
      TierLow
  )

  type AllowedMentions struct {
      Parse []string `json:"parse"`
      Roles []string `json:"roles,omitempty"`
      Users []string `json:"users,omitempty"`
  }

  type SendOptions struct {
      URL            string
      Event          string
      Thumbnail      string
      Image          string
      Author         *Author
      Platform       string
      JobID          string
      Tier           Tier
      Mention        string
      MentionAllowed *AllowedMentions
  }

  func MentionParse(mention string) *AllowedMentions // exported; Arc N2b resolves with it
  func effectiveTier(opts SendOptions) Tier          // unexported; Task 4's Send calls it
  func footerText(opts SendOptions) string           // unexported; buildPayload calls it
  ```
  In `internal/worker`: `func finishedImage(job *database.Job) string`.

**Scope note.** Nothing in N1 *sets* `Author`, `Platform`, `JobID`, `Mention` or `MentionAllowed` — N2a's builders fill the first three and N2b's config fills the last two. N1 defines the fields, the payload shape they produce, and the tests that pin that shape, so N2a/N2b can be pure producer/config work. `Tier` is the exception: Task 4's overflow policy reads it on every send, derived from `Event`.

**`MentionAllowed` is the OBJECT, not a bool** — controller pin, and N2b's plan is written against it. The per-(target, event) decision N2b makes is not "may this ping?" but "*what* may it ping": a role, a user, or `@everyone`, each a different `allowed_mentions` shape. Carrying the resolved `*AllowedMentions` makes the nil case the gate (no object, no ping) and puts the text→object mapping in one exported resolver, `MentionParse`, that N2b calls once at config-resolve time instead of `buildPayload` re-deriving it on every send.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/payload_test.go`:

```go
package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodePayload unmarshals what buildPayload produced, so every assertion is
// against the JSON Discord actually receives rather than against the Go
// structs. A field that loses its tag, or gains an `omitempty` it should not
// have, is invisible to a struct-level check and obvious here.
func decodePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, body)
	}
	return got
}

func firstEmbed(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	embeds, ok := p["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("want exactly 1 embed, got %v", p["embeds"])
	}
	e, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed is not an object: %v", embeds[0])
	}
	return e
}

// TestPayloadCarriesTheAuthorBlock pins the channel identity an operator reads
// first. Job.ChannelAvatarURL has existed since the rewrite and reached no
// embed: discordEmbed had no author field at all, so every job notification
// identified its channel only inside a "Channel" field halfway down.
//
// THE MUTANT: dropping the `author` key, or serialising it when Name is empty
// (Discord rejects an author object with no name — a permanent 400).
func TestPayloadCarriesTheAuthorBlock(t *testing.T) {
	body, err := buildPayload("T", "D", 0x1, nil, SendOptions{
		Author: &Author{
			Name:    "Some Channel",
			IconURL: "https://example.invalid/avatar.jpg",
			URL:     "https://example.invalid/channel",
		},
	})
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	e := firstEmbed(t, decodePayload(t, body))
	author, ok := e["author"].(map[string]any)
	if !ok {
		t.Fatalf("no author object in the embed: %v", e)
	}
	if author["name"] != "Some Channel" {
		t.Errorf("author.name = %v, want %q", author["name"], "Some Channel")
	}
	if author["icon_url"] != "https://example.invalid/avatar.jpg" {
		t.Errorf("author.icon_url = %v", author["icon_url"])
	}
	if author["url"] != "https://example.invalid/channel" {
		t.Errorf("author.url = %v", author["url"])
	}

	t.Run("an author with no name is omitted entirely", func(t *testing.T) {
		body, err := buildPayload("T", "D", 0x1, nil, SendOptions{Author: &Author{IconURL: "https://example.invalid/a.jpg"}})
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		if _, present := firstEmbed(t, decodePayload(t, body))["author"]; present {
			t.Error("a nameless author was serialised — Discord rejects it with a permanent 400")
		}
	})
}

// TestFooterNamesThePlatformAndJob pins the footer contract. "Moombox Go" was
// a rewrite-era suffix that told an operator nothing; with several channels and
// several jobs in one Discord channel, the platform and the job id are what
// make an embed identifiable at a glance — and JobID is the key Arc N3's
// edit-in-place mode reads.
func TestFooterNamesThePlatformAndJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts SendOptions
		want string
	}{
		{"neither", SendOptions{}, "Moombox"},
		{"platform and job", SendOptions{Platform: "youtube", JobID: "yt_abc"}, "Moombox · youtube · yt_abc"},
		{"platform only", SendOptions{Platform: "twitch"}, "Moombox · twitch"},
		{"job only", SendOptions{JobID: "yt_abc"}, "Moombox · yt_abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildPayload("T", "D", 0x1, nil, tc.opts)
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			footer, ok := firstEmbed(t, decodePayload(t, body))["footer"].(map[string]any)
			if !ok {
				t.Fatalf("no footer in the embed")
			}
			if footer["text"] != tc.want {
				t.Errorf("footer.text = %v, want %q", footer["text"], tc.want)
			}
		})
	}
}

// TestMentionPayloadShape pins what N2b will drive. Per the Discord API docs
// (resources/message.mdx) allowed_mentions governs "mentions in the message
// content, or components" — embeds never mention on their own — and the webhook
// default is {"parse": ["users"]}. So a role ping needs BOTH content
// "<@&id>" AND allowed_mentions {roles: [id]}, and an empty parse list is what
// stops the default from silently widening a role ping into a user ping.
//
// THE MUTANT: setting content without allowed_mentions (the ping silently does
// nothing for a role), or leaving parse unset (the default re-enters).
func TestMentionPayloadShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mention   string
		wantRoles []any
		wantUsers []any
		wantParse []any
	}{
		{"role", "<@&123456789012345678>", []any{"123456789012345678"}, nil, []any{}},
		{"user", "<@987654321098765432>", nil, []any{"987654321098765432"}, []any{}},
		{"nickname user form", "<@!987654321098765432>", nil, []any{"987654321098765432"}, []any{}},
		{"everyone", "@everyone", nil, nil, []any{"everyone"}},
		{"here", "@here", nil, nil, []any{"everyone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildPayload("T", "D", 0x1, nil, SendOptions{
				Mention:        tc.mention,
				MentionAllowed: MentionParse(tc.mention),
			})
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			p := decodePayload(t, body)
			if p["content"] != tc.mention {
				t.Errorf("content = %v, want %q", p["content"], tc.mention)
			}
			am, ok := p["allowed_mentions"].(map[string]any)
			if !ok {
				t.Fatalf("no allowed_mentions: %v", p)
			}
			eq := func(key string, want []any) {
				got, present := am[key]
				if want == nil {
					if present {
						t.Errorf("allowed_mentions.%s = %v, want absent", key, got)
					}
					return
				}
				gotList, _ := got.([]any)
				if len(gotList) != len(want) {
					t.Errorf("allowed_mentions.%s = %v, want %v", key, got, want)
					return
				}
				for i := range want {
					if gotList[i] != want[i] {
						t.Errorf("allowed_mentions.%s = %v, want %v", key, got, want)
						return
					}
				}
			}
			eq("roles", tc.wantRoles)
			eq("users", tc.wantUsers)
			if _, present := am["parse"]; !present {
				t.Errorf("allowed_mentions has no parse key — the webhook default {\"parse\":[\"users\"]} re-enters")
			}
			eq("parse", tc.wantParse)
		})
	}

	t.Run("no mention means no content and no allowed_mentions", func(t *testing.T) {
		body, err := buildPayload("T", "D", 0x1, nil, SendOptions{})
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		p := decodePayload(t, body)
		if _, present := p["content"]; present {
			t.Errorf("content was serialised for a send with no mention: %v", p["content"])
		}
		if _, present := p["allowed_mentions"]; present {
			t.Errorf("allowed_mentions was serialised for a send with no mention")
		}
	})

	t.Run("a mention the target is not allowed for is not sent", func(t *testing.T) {
		body, err := buildPayload("T", "D", 0x1, nil, SendOptions{Mention: "@everyone", MentionAllowed: nil})
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		if _, present := decodePayload(t, body)["content"]; present {
			t.Error("a nil MentionAllowed still pinged — it is the per-event gate N2b sets from mention_events and it must be honoured here")
		}
	})

	// MentionParse is where the "unrecognised form is dropped" rule lives now
	// that the object travels in SendOptions: N2b resolves the configured text
	// through it, so a config string that is not a mention must resolve to nil
	// rather than to an unrestricted ping.
	t.Run("MentionParse rejects everything that is not a mention form", func(t *testing.T) {
		for _, junk := range []string{"", "everyone", "@chan", "<@&>", "<@>", "<@!>", "<#123>", "please ping <@&1>"} {
			if got := MentionParse(junk); got != nil {
				t.Errorf("MentionParse(%q) = %+v, want nil", junk, got)
			}
		}
		if got := MentionParse("<@&123>"); got == nil || len(got.Roles) != 1 || got.Roles[0] != "123" {
			t.Errorf("MentionParse(\"<@&123>\") = %+v, want Roles [123]", got)
		}
	})
}

// TestEffectiveTierDerivesFromTheEvent pins the overflow policy's input. A
// backfill sweep produces hundreds of `found`; an `error` must never lose its
// place in the queue to one. An explicit Tier wins so a caller can promote a
// normally-low event.
func TestEffectiveTierDerivesFromTheEvent(t *testing.T) {
	for _, tc := range []struct {
		opts SendOptions
		want Tier
	}{
		{SendOptions{Event: "found"}, TierLow},
		{SendOptions{Event: "added"}, TierLow},
		{SendOptions{Event: "scheduled"}, TierLow},
		{SendOptions{Event: "rescheduled"}, TierLow},
		{SendOptions{Event: "error"}, TierNormal},
		{SendOptions{Event: "auth"}, TierNormal},
		{SendOptions{Event: "disk_critical"}, TierNormal},
		{SendOptions{Event: "finished"}, TierNormal},
		{SendOptions{}, TierNormal},
		{SendOptions{Event: "found", Tier: TierNormal}, TierNormal},
		{SendOptions{Event: "error", Tier: TierLow}, TierLow},
	} {
		if got := effectiveTier(tc.opts); got != tc.want {
			t.Errorf("effectiveTier(event=%q, tier=%v) = %v, want %v", tc.opts.Event, tc.opts.Tier, got, tc.want)
		}
	}
}

// TestSendOptionsStillCarriesTheOldKeys guards against a rename sweeping away
// a field 36 producers set.
func TestSendOptionsStillCarriesTheOldKeys(t *testing.T) {
	body, err := buildPayload("T", "D", 0x1, nil, SendOptions{
		URL:       "https://example.invalid/watch",
		Thumbnail: "https://example.invalid/t.jpg",
		Image:     "https://example.invalid/i.jpg",
	})
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	e := firstEmbed(t, decodePayload(t, body))
	if e["url"] != "https://example.invalid/watch" {
		t.Errorf("url = %v", e["url"])
	}
	for _, key := range []string{"thumbnail", "image"} {
		obj, ok := e[key].(map[string]any)
		if !ok || !strings.HasPrefix(obj["url"].(string), "https://example.invalid/") {
			t.Errorf("%s = %v", key, e[key])
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestPayload|TestFooter|TestMention|TestEffectiveTier|TestSendOptions' ./internal/notifications/`

Expected: a build failure —
```
# github.com/vampiricwulf/Moombox/internal/notifications [github.com/vampiricwulf/Moombox/internal/notifications.test]
internal/notifications/payload_test.go:NN:NN: unknown field Author in struct literal of type SendOptions
internal/notifications/payload_test.go:NN:NN: undefined: Author
internal/notifications/payload_test.go:NN:NN: unknown field Platform in struct literal of type SendOptions
internal/notifications/payload_test.go:NN:NN: unknown field Mention in struct literal of type SendOptions
internal/notifications/payload_test.go:NN:NN: undefined: MentionParse
internal/notifications/payload_test.go:NN:NN: undefined: effectiveTier
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

- [ ] **Step 3: Grow `SendOptions`**

Replace `internal/notifications/manager.go:130-136` entirely:

```go
// Author is the embed's author line: the channel that produced the job,
// rendered above the title with its avatar. Job.ChannelAvatarURL has existed
// since the rewrite and reached no embed until this field did.
type Author struct {
	Name    string // required by Discord — an author object without one is a 400
	IconURL string
	URL     string
}

// Tier ranks a notification for the per-target queue's overflow policy
// (queue.go). It is NOT a delivery priority: the queue is strictly FIFO, and
// the tier is consulted only when the queue is full and something has to go.
type Tier int

const (
	// TierUnset lets the manager derive the tier from SendOptions.Event. It is
	// the zero value so that the ~36 existing send sites, none of which set a
	// tier, keep getting the right answer.
	TierUnset Tier = iota
	// TierNormal is never dropped while any TierLow entry is queued.
	TierNormal
	// TierLow is the high-volume discovery family. A backfill re-scan (R B)
	// creates a `found` per catalogue row; a dead cookie parks N jobs. Those
	// are what a full queue sheds, never an alert.
	TierLow
)

// lowTierEvents is TierUnset's derivation table. Deliberately small: only the
// four events a single operation can produce in the dozens.
var lowTierEvents = map[string]bool{
	"found":       true,
	"added":       true,
	"scheduled":   true,
	"rescheduled": true,
}

// effectiveTier resolves the tier the queue should use for one send.
func effectiveTier(opts SendOptions) Tier {
	if opts.Tier != TierUnset {
		return opts.Tier
	}
	if lowTierEvents[opts.Event] {
		return TierLow
	}
	return TierNormal
}

// AllowedMentions is Discord's allowed_mentions object: exactly what the
// message `content` is permitted to ping. Parse is NOT omitempty and is always
// non-nil on a sent object — the webhook default is {"parse": ["users"]}, so an
// omitted list silently re-widens a role ping into "every user id in the text".
//
// Exported because Arc N2b resolves one per (target, event) and puts it in
// SendOptions; MentionParse (discord.go) is the resolver.
type AllowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
	Users []string `json:"users,omitempty"`
}

// SendOptions provides optional parameters for a notification.
type SendOptions struct {
	URL       string // Link URL for the embed title
	Event     string // Event name for filtering (e.g. "finished")
	Thumbnail string // Thumbnail image URL
	Image     string // Full-width image URL

	// Author is the embed's author line (channel name + avatar + channel page).
	Author *Author
	// Platform and JobID feed the footer ("Moombox · {platform} · {job id}").
	// JobID is also the key Arc N3's edit-in-place mode stores a Discord
	// message id against, which is why it is an option rather than a footer
	// string: a caller must not be able to spell it differently.
	Platform string
	JobID    string
	// Tier ranks this send for the queue's overflow policy. Leave it
	// TierUnset to derive it from Event.
	Tier Tier

	// Mention is the literal ping text ("<@&id>", "<@id>", "@everyone",
	// "@here") a target is configured with, and MentionAllowed is the resolved
	// allowed_mentions object for it — nil when THIS event is not in that
	// target's mention_events, which is what stops the ping. Both are filled
	// by Arc N2b (MentionParse resolves the object from the configured text);
	// N1 defines the fields and the payload shape they produce. Embeds never
	// mention on their own (per Discord API docs), so a ping needs the message
	// `content` plus a matching `allowed_mentions` — see buildPayload.
	Mention        string
	MentionAllowed *AllowedMentions
}
```

(The `Event` comment's example changes from the non-existent `"download_start"` to `"finished"` — audit R7's first stale comment. R7's second, the "30 req/min" claim at `:141`, lives in the `maxInflightNotifications` doc block that Task 4 deletes outright.)

- [ ] **Step 4: Teach the payload the new shape**

In `internal/notifications/discord.go`, replace the payload structs (`:61-88`):

```go
type discordPayload struct {
	// Content is the only place a mention can live: per Discord API docs
	// (resources/message.mdx) allowed_mentions governs "mentions in the
	// message content, or components", so an embed can never ping anyone.
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds"`
	// AllowedMentions is the EXPORTED notifications.AllowedMentions
	// (manager.go), not a payload-private twin: Arc N2b resolves one per
	// (target, event) and hands it over in SendOptions, so the wire shape and
	// the option are the same type by construction.
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

type discordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"`
	URL         string         `json:"url,omitempty"`
	Author      *discordAuthor `json:"author,omitempty"`
	Fields      []discordField `json:"fields,omitempty"`
	Thumbnail   *discordImage  `json:"thumbnail,omitempty"`
	Image       *discordImage  `json:"image,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

// discordField is a type alias for Field so the Discord JSON encoder can
// share the same struct without keeping a parallel duplicate definition.
// Field carries the JSON tags directly. Audit reports/small-packages.md.
type discordField = Field

type discordAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}
```

Add, just above `buildPayload`:

```go
// footerText renders the embed footer. "Moombox Go" was a rewrite-era suffix
// that identified nothing; with several channels and several jobs landing in
// one Discord channel, the platform and the job id are what let an operator
// tell two embeds apart without opening the link.
func footerText(opts SendOptions) string {
	text := "Moombox"
	if opts.Platform != "" {
		text += " · " + opts.Platform
	}
	if opts.JobID != "" {
		text += " · " + opts.JobID
	}
	return text
}

// MentionParse maps a configured mention to the allowed_mentions object that
// makes it actually ping, or nil for a form we do not recognise — which keeps
// an unvalidated config string from becoming an unrestricted ping. Arc N2b
// calls it when it fills SendOptions.
func MentionParse(mention string) *AllowedMentions {
	switch {
	case mention == "@everyone" || mention == "@here":
		return &AllowedMentions{Parse: []string{"everyone"}}
	case strings.HasPrefix(mention, "<@&") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@&"), ">")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Roles: []string{id}}
	case strings.HasPrefix(mention, "<@") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@"), ">")
		// <@!id> is the legacy nickname form; Discord still accepts it in
		// content and the id is what allowed_mentions needs either way.
		id = strings.TrimPrefix(id, "!")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Users: []string{id}}
	}
	return nil
}
```

Then rewrite `buildPayload`'s body (`:90-123`) — the clamp call is added by Task 3; this step only adds author, footer and mention:

```go
// buildPayload assembles the embed JSON shared by Send and SendOnce.
func buildPayload(title, description string, color int, fields []Field, opts SendOptions) ([]byte, error) {
	embed := discordEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Footer:      &discordFooter{Text: footerText(opts)},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	if opts.URL != "" {
		embed.URL = opts.URL
	}

	// Discord rejects an author object with no name (a permanent 400), so a
	// half-filled Author is dropped rather than sent.
	if opts.Author != nil && opts.Author.Name != "" {
		embed.Author = &discordAuthor{
			Name:    opts.Author.Name,
			URL:     opts.Author.URL,
			IconURL: opts.Author.IconURL,
		}
	}

	if opts.Thumbnail != "" {
		embed.Thumbnail = &discordImage{URL: opts.Thumbnail}
	}

	if opts.Image != "" {
		embed.Image = &discordImage{URL: opts.Image}
	}

	if len(fields) > 0 {
		// discordField is a type alias for Field, so this is a direct copy
		// rather than an element-by-element conversion. It MUST stay a copy:
		// the clamp below rewrites values in place, and the caller's slice is
		// often a FieldBuilder's buffer reused for a later send.
		embed.Fields = append([]discordField(nil), fields...)
	}

	payload := discordPayload{Embeds: []discordEmbed{embed}}

	// A mention rides the message content, never the embed. A nil
	// MentionAllowed is the per-event gate N2b fills from mention_events:
	// no object, no ping. MentionParse is where an unrecognised form becomes
	// that nil.
	if opts.MentionAllowed != nil && opts.Mention != "" {
		payload.Content = opts.Mention
		payload.AllowedMentions = opts.MentionAllowed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal discord payload: %w", err)
	}
	return body, nil
}
```

`discord.go` already imports `strings` (`discordErrSnippet` uses it), so no import change.

- [ ] **Step 5: Drop the dead Twitch finished image**

Per the §0 ruling: Twitch preview URLs 404 once the broadcast ends (`internal/worker/orchestrator_twitch.go:163-164`), so the full-width `Image` on a Twitch "Download Finished" embed renders broken every time. Multipart upload of the saved `thumbnail_file` is a later option, not this arc.

In `internal/worker/orchestrator_mux.go`, add above `sendFinishedNotification` (currently `:1194`):

```go
// finishedImage is the full-width image URL for a "Download Finished" embed,
// or "" for Twitch.
//
// Twitch preview URLs are live-only: they 404 as soon as the broadcast ends
// (see the thumbnail note in orchestrator_twitch.go), and "Download Finished"
// is by definition sent after it did — so every Twitch finished embed carried
// an image that could not load. Owner ruling: drop it. Uploading the saved
// thumbnail_file as a multipart attachment is the option that would restore
// one, and is deliberately not this arc.
func finishedImage(job *database.Job) string {
	if job == nil || job.Platform == "twitch" {
		return ""
	}
	return job.ThumbnailURL
}
```

Then at `:1043` (multi-segment finalize) and `:1283` (`sendFinishedNotification`), replace

```go
				Image: jobCtx.Job.ThumbnailURL,
```
with
```go
				Image: finishedImage(jobCtx.Job),
```
(keeping each site's own indentation — `:1043` is one tab deeper than `:1283`).

- [ ] **Step 6: Add the producer-side test**

Append to `internal/worker/orchestrator_mux_test.go`:

```go
// TestFinishedImageIsDroppedForTwitch pins the §0 ruling. A Twitch preview URL
// 404s the moment the broadcast ends, and "Download Finished" is sent after it
// did, so the full-width image on every Twitch finished embed was permanently
// broken. YouTube thumbnails outlive the stream and keep theirs.
//
// THE MUTANT: reverting either call site to jobCtx.Job.ThumbnailURL.
func TestFinishedImageIsDroppedForTwitch(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     string
	}{
		{"youtube", "https://i.ytimg.com/vi/x/maxresdefault.jpg"},
		{"twitch", ""},
		{"", "https://i.ytimg.com/vi/x/maxresdefault.jpg"},
	} {
		job := &database.Job{
			Platform:     tc.platform,
			ThumbnailURL: "https://i.ytimg.com/vi/x/maxresdefault.jpg",
		}
		if got := finishedImage(job); got != tc.want {
			t.Errorf("finishedImage(platform=%q) = %q, want %q", tc.platform, got, tc.want)
		}
	}
	if got := finishedImage(nil); got != "" {
		t.Errorf("finishedImage(nil) = %q, want \"\"", got)
	}
}
```

`orchestrator_mux_test.go` is `package worker`, but its import block is only `os` / `path/filepath` / `testing` — **add `github.com/vampiricwulf/Moombox/internal/database` here**. (Task 3 adds `strings` + `unicode/utf8` + `internal/notifications` to the same file, and Task 6 adds `fmt` + `internal/notifications/notificationtest`. Each task adds what its own append needs; none of them can assume an earlier one already did.)

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFinishedImageIsDroppedForTwitch ./internal/worker/
```
Expected: two `ok` lines. `TestDiscordWebhookSendsValidPayload` (`discord_test.go:20`) asserts `strings.Contains(e.Footer.Text, "Moombox")` and so passes against the new "Moombox" footer unchanged.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t2.msg <<'EOF'
feat(notifications): author line, platform/job footer, and the mention payload

SendOptions grows Author, Platform, JobID, Tier, Mention and MentionAllowed.
The embed gains an `author` block (channel name + avatar + channel page — 
Job.ChannelAvatarURL reached no embed before), the footer becomes
"Moombox · {platform} · {job id}", and a mention rides message `content` with
a matching `allowed_mentions` (embeds never ping; the webhook default
{"parse":["users"]} is overridden with an explicit empty list).

N2b fills Mention/MentionAllowed and N2a fills Author/Platform/JobID; this is
the shape they write into. Tier is live now — Task 4's overflow policy reads it.

Also drops the full-width Image from Twitch "Download Finished" embeds: a
Twitch preview URL 404s once the broadcast ends, so it never rendered.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t2.msg -- \
  internal/notifications/manager.go \
  internal/notifications/discord.go \
  internal/notifications/payload_test.go \
  internal/worker/orchestrator_mux.go \
  internal/worker/orchestrator_mux_test.go
```

---
## Task 3: Rune-boundary clamps, `EscapeMarkdown`, and `discordapp.com`

**Files:**
- Create: `internal/notifications/limits.go`
- Modify: `internal/notifications/discord.go` (`buildPayload` — one added call)
- Modify: `internal/notifications/manager.go:16` (`discordWebhookRe`), `:190-212` (`parseTarget`)
- Create: `internal/notifications/limits_test.go`
- Modify: `internal/worker/orchestrator_mux.go:1266-1276` (the byte cut — `:1265` closes the Trimmed Range `if` and must survive; `:1275` is `fb.Add("Description", desc)` and `:1276` its closing brace)
- Modify: `internal/worker/worker.go:1332` (the unbounded `Error` field), `internal/worker/orchestrator.go:657` (same), `cmd/moombox/monitor_callbacks.go:1606` (`Last Error`)
- Test: `internal/notifications/limits_test.go`, `internal/worker/orchestrator_mux_test.go` (append)

**Interfaces:**
- Consumes: Task 2's `SendOptions` fields (`clampEmbed` clamps the author name and the footer).
- Produces:
  ```go
  // exported — producers use both
  func ClampRunes(s string, limit int) string
  func EscapeMarkdown(s string) string
  // unexported — buildPayload calls it
  func clampEmbed(e *discordEmbed)
  ```
  Limit constants (unexported): `limitTitle 256`, `limitDescription 4096`, `limitFieldName 256`, `limitFieldValue 1024`, `limitFooter 2048`, `limitAuthorName 256`, `limitFields 25`, `limitTotal 6000`.

**Why one place.** Per Discord API docs (`resources/message.mdx`) an over-limit embed is a **400**, and `discord.go`'s ladder treats non-429 4xx as permanent — so today an embed one character over 1024 in a field is dropped after one attempt with only "discord webhook returned 400" in the log. Clamping inside `buildPayload` covers all 36 send sites without editing one of them. The longest known producers are the mux error tail (≈540 chars, `internal/engine/muxer.go:270-272`) and four unbounded strings: `Error` (`worker.go:1332`), `Error` (`orchestrator.go:657`), `Last Error` (`monitor_callbacks.go:1606`) and `Output Directory`/`Marker` paths.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/limits_test.go`:

```go
package notifications

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// jp repeats a 3-byte Japanese rune n times. Japanese metadata is the norm for
// this project (cmd/moombox/job_progress_test.go), and a byte-indexed cut
// splits one of these mid-rune — encoding/json then emits U+FFFD and the
// operator reads a mojibake title.
func jp(n int) string { return strings.Repeat("あ", n) }

// TestClampRunesCutsOnRuneBoundaries is the helper both the payload clamp and
// the worker's Description excerpt go through.
//
// THE MUTANT: `s[:limit-1]` instead of a rune walk. Every multi-byte subtest
// then produces invalid UTF-8.
func TestClampRunesCutsOnRuneBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"under the limit is untouched", "abc", 10, "abc"},
		{"exactly at the limit is untouched", "abcde", 5, "abcde"},
		{"ascii over the limit", "abcdef", 5, "abcd…"},
		{"japanese over the limit", jp(6), 5, jp(4) + "…"},
		{"japanese exactly at the limit", jp(5), 5, jp(5)},
		{"limit 1 is just the marker", "abcdef", 1, "…"},
		{"limit 0 is empty", "abcdef", 0, ""},
		{"negative limit is empty", "abcdef", -3, ""},
		{"empty input", "", 10, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampRunes(tc.in, tc.limit)
			if got != tc.want {
				t.Errorf("ClampRunes(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ClampRunes(%q, %d) produced invalid UTF-8: %q", tc.in, tc.limit, got)
			}
			if n := utf8.RuneCountInString(got); tc.limit >= 0 && n > tc.limit {
				t.Errorf("ClampRunes(%q, %d) returned %d runes", tc.in, tc.limit, n)
			}
		})
	}
}

// TestClampEmbedHonoursEveryDiscordLimit walks the per-field caps from the
// Discord API docs (resources/message.mdx): title 256, description 4096, field
// name 256, field value 1024, footer 2048, author name 256, 25 fields.
// Every one of them is a permanent 400 when exceeded, and nothing clamped.
func TestClampEmbedHonoursEveryDiscordLimit(t *testing.T) {
	e := &discordEmbed{
		Title:       jp(400),
		Description: jp(5000),
		Author:      &discordAuthor{Name: jp(400)},
		Footer:      &discordFooter{Text: jp(3000)},
	}
	for i := range 40 {
		e.Fields = append(e.Fields, discordField{Name: jp(400), Value: jp(2000)})
		_ = i
	}
	clampEmbed(e)

	if n := utf8.RuneCountInString(e.Title); n != limitTitle {
		t.Errorf("title = %d runes, want %d", n, limitTitle)
	}
	if n := utf8.RuneCountInString(e.Author.Name); n != limitAuthorName {
		t.Errorf("author name = %d runes, want %d", n, limitAuthorName)
	}
	if n := utf8.RuneCountInString(e.Footer.Text); n != limitFooter {
		t.Errorf("footer = %d runes, want %d", n, limitFooter)
	}
	if len(e.Fields) > limitFields {
		t.Errorf("fields = %d, want <= %d", len(e.Fields), limitFields)
	}
	for i, f := range e.Fields {
		if n := utf8.RuneCountInString(f.Name); n > limitFieldName {
			t.Errorf("field %d name = %d runes", i, n)
		}
		if n := utf8.RuneCountInString(f.Value); n > limitFieldValue {
			t.Errorf("field %d value = %d runes", i, n)
		}
	}
	if !utf8.ValidString(e.Description) {
		t.Error("description is not valid UTF-8 after the clamp")
	}
}

// TestClampEmbedTrimsToTheTotalBudget pins the 6000-rune whole-embed cap and
// its order: trailing FIELDS go first, then the description is trimmed, and the
// title is never touched. The title is the only part an operator can identify
// the embed by in a notification popup.
//
// THE MUTANT: trimming the title, or trimming the description before dropping
// fields (the embed then loses its body while keeping 25 near-empty fields).
func TestClampEmbedTrimsToTheTotalBudget(t *testing.T) {
	title := jp(100)
	e := &discordEmbed{Title: title, Description: jp(4000)}
	for range 5 {
		e.Fields = append(e.Fields, discordField{Name: jp(100), Value: jp(1000)})
	}
	// 100 + 4000 + 5*(100+1000) = 9600 runes, well over 6000.
	clampEmbed(e)

	if e.Title != title {
		t.Errorf("the title was trimmed to %q — it is the one part that must survive", e.Title)
	}
	if total := embedRunes(e); total > limitTotal {
		t.Errorf("embed totals %d runes, want <= %d", total, limitTotal)
	}
	if len(e.Fields) == 5 {
		t.Error("no field was dropped — trailing fields are what goes first")
	}
	if !utf8.ValidString(e.Description) {
		t.Error("the description trim split a rune")
	}
}

// TestEscapeMarkdown is the table for the helper producers apply to
// job-supplied text. Per Discord API docs (reference.mdx) embed descriptions
// and field values render Discord's markdown subset, so a stream title with
// `_`, `*`, `~~`, `||`, a backtick or a `<t:…>`-shaped fragment re-renders as
// formatting. Pings are impossible from an embed, so this is formatting only.
//
// THE MUTANT: escaping the backslash LAST instead of first — every other
// escape's own backslash then gets doubled.
func TestEscapeMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain title", "plain title"},
		{"a_b", `a\_b`},
		{"*bold*", `\*bold\*`},
		{"~~strike~~", `\~\~strike\~\~`},
		{"a||b", `a\|\|b`},
		{"`code`", "\\`code\\`"},
		{"<t:123:R>", `\<t:123:R\>`},
		{`back\slash`, `back\\slash`},
		{"# heading", `\# heading`},
		{"- item", `\- item`},
		{"> quote", `\> quote`},
		// # and - are special ONLY at the start of a line (spec §1.3 says
		// "leading"), so a mid-line one stays literal. > is escaped everywhere
		// because it also opens <t:…>, <@…> and <:emoji:id>.
		{"mid # hash", "mid # hash"},
		{"a - b", "a - b"},
		{"line1\n# line2", "line1\n\\# line2"},
		{"\n- second line", "\n\\- second line"},
		{"あ_い", `あ\_い`},
	} {
		if got := EscapeMarkdown(tc.in); got != tc.want {
			t.Errorf("EscapeMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseTargetAcceptsTheLegacyDiscordappHost is audit R6. discordapp.com is
// Discord's legacy domain and still serves webhooks; the regex rejected it
// outright, so a webhook pasted from an old bookmark was warn-skipped at
// startup with a message that did not say why.
//
// The resolved URL is CANONICALISED to discord.com, which is what makes the
// dedupe correct: the two spellings of one webhook must collapse to one target
// (buildTargets keys on the resolved URL), and it also avoids a redirect —
// Go's http.Client turns a 301/302 on POST into a GET, silently losing the body.
func TestParseTargetAcceptsTheLegacyDiscordappHost(t *testing.T) {
	const id, tok = "123456789012345678", "tok-en_ABC"
	for _, tc := range []struct {
		in      string
		wantURL string
	}{
		{"https://discordapp.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
		{"https://ptb.discordapp.com/api/webhooks/" + id + "/" + tok, "https://ptb.discord.com/api/webhooks/" + id + "/" + tok},
		{"https://discord.com/api/webhooks/" + id + "/" + tok, "https://discord.com/api/webhooks/" + id + "/" + tok},
	} {
		s, err := parseTarget(tc.in)
		if err != nil {
			t.Fatalf("parseTarget(%q): %v", tc.in, err)
		}
		d, ok := s.(*DiscordWebhook)
		if !ok {
			t.Fatalf("parseTarget(%q) did not return a DiscordWebhook", tc.in)
		}
		if d.URL != tc.wantURL {
			t.Errorf("parseTarget(%q).URL = %q, want %q", tc.in, d.URL, tc.wantURL)
		}
	}

	t.Run("a malformed discordapp URL is still rejected with the specific message", func(t *testing.T) {
		_, err := parseTarget("http://discordapp.com/api/webhooks/abc/")
		if err == nil || !strings.Contains(err.Error(), "must be HTTPS") {
			t.Errorf("err = %v, want the specific Discord-webhook rejection", err)
		}
	})

	t.Run("the two spellings collapse to one target", func(t *testing.T) {
		cfg := notifConfigWithURLs(
			"https://discordapp.com/api/webhooks/"+id+"/"+tok,
			"https://discord.com/api/webhooks/"+id+"/"+tok,
		)
		if got := len(buildTargets(cfg, testLogger{})); got != 1 {
			t.Errorf("built %d targets, want 1 — the legacy spelling must dedupe against the current one", got)
		}
	})
}
```

Add this helper to `internal/notifications/manager_test.go` (it keeps the config import in one place and Tasks 4 and 5 reuse it):

```go
// notifConfigWithURLs builds a config whose only content is unfiltered
// notification targets, in order.
func notifConfigWithURLs(urls ...string) *config.MoomboxConfig {
	cfg := &config.MoomboxConfig{}
	for _, u := range urls {
		cfg.Notifications = append(cfg.Notifications, config.NotificationConfig{URL: u})
	}
	return cfg
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestClamp|TestEscapeMarkdown|TestParseTargetAccepts' ./internal/notifications/`

Expected: a build failure —
```
# github.com/vampiricwulf/Moombox/internal/notifications [github.com/vampiricwulf/Moombox/internal/notifications.test]
internal/notifications/limits_test.go:NN:NN: undefined: ClampRunes
internal/notifications/limits_test.go:NN:NN: undefined: clampEmbed
internal/notifications/limits_test.go:NN:NN: undefined: limitTitle
internal/notifications/limits_test.go:NN:NN: undefined: embedRunes
internal/notifications/limits_test.go:NN:NN: undefined: EscapeMarkdown
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

- [ ] **Step 3: Add the limits file**

Create `internal/notifications/limits.go`:

```go
package notifications

import (
	"strings"
	"unicode/utf8"
)

// Discord's embed limits, per the Discord API docs (resources/message.mdx).
// Exceeding ANY of them is a 400, and discord.go treats a non-429 4xx as
// permanent — so before these existed, one over-long error string dropped the
// whole alert after a single attempt with nothing in the log but the status.
const (
	limitTitle       = 256
	limitDescription = 4096
	limitFieldName   = 256
	limitFieldValue  = 1024
	limitFooter      = 2048
	limitAuthorName  = 256
	limitFields      = 25
	// limitTotal is the sum across title, description, every field name and
	// value, the footer text and the author name, in characters.
	limitTotal = 6000
)

// clampMarker is appended to anything ClampRunes shortens. One rune, so it
// costs one of the budget it is protecting.
const clampMarker = "…"

// ClampRunes shortens s to at most limit CHARACTERS, cutting on a rune
// boundary and marking the cut with "…".
//
// Exported because producers need the same cut for their own excerpts — the
// job Description excerpt in internal/worker/orchestrator_mux.go was cut by
// BYTE (desc[:297]), which splits a Japanese description mid-rune and makes
// encoding/json emit U+FFFD. Discord counts characters, not bytes, so a
// byte-based cut is also the wrong unit for the limit it was aiming at.
func ClampRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	kept := 0
	for i := range s {
		if kept == limit-1 {
			return s[:i] + clampMarker
		}
		kept++
	}
	// Unreachable for a string longer than limit runes, but a loop that can
	// fall through must still answer.
	return s + clampMarker
}

// markdownEscaper escapes the characters Discord's markdown subset gives
// meaning to inside an embed description or field value (per the Discord API
// docs, reference.mdx). The BACKSLASH IS FIRST and that ordering is
// load-bearing: strings.NewReplacer matches at each position in one pass, so
// the replacements never re-scan each other's output — but a hand-rolled
// sequential ReplaceAll that escaped the backslash last would double every
// escape it had just written.
//
// `<` and `>` are here because Discord's <t:…>, <@…>, <#…> and <:emoji:id>
// forms all open with one; a stream title containing "<3" would otherwise
// start something that swallows the rest of the line.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"*", `\*`,
	"_", `\_`,
	"~", `\~`,
	"|", `\|`,
	"`", "\\`",
	"<", `\<`,
	">", `\>`,
)

// EscapeMarkdown neutralises Discord markdown in job-supplied text.
//
// APPLY IT TO job-supplied strings — stream titles, error text, file paths,
// channel names. Do NOT apply it to static titles Moombox writes itself, and
// never to a field that carries <t:…> markup ON PURPOSE (the Scheduled For,
// Starts At, Old/New Time and Outage Alert fields): escaping those turns a
// live relative timestamp into literal text.
//
// This is formatting safety, not injection safety. An embed can never mention
// anyone (see buildPayload's mention handling), so the worst an unescaped
// title can do is render italic.
func EscapeMarkdown(s string) string {
	if s == "" {
		return ""
	}
	escaped := markdownEscaper.Replace(s)
	// #, - and > are special only at the START of a line (heading, list item,
	// block quote). > is already escaped everywhere by the replacer above;
	// these two are not, because escaping every hyphen in a title would be
	// noise an operator reads.
	lines := strings.Split(escaped, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "-") {
			lines[i] = `\` + ln
		}
	}
	return strings.Join(lines, "\n")
}

// embedRunes is the character count Discord applies its 6000 total against:
// title, description, every field name and value, the footer text and the
// author name. URLs, the colour and the timestamp are not counted.
func embedRunes(e *discordEmbed) int {
	total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	if e.Footer != nil {
		total += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		total += utf8.RuneCountInString(e.Author.Name)
	}
	return total
}

// clampEmbed brings one embed inside every Discord limit, in place.
//
// The ORDER of the total-budget pass is the decision: trailing fields go
// first, then the description is trimmed, and the title is never touched. A
// reader identifies an embed by its title in a notification popup before they
// open Discord at all, so the title is the last thing worth spending; fields
// are the cheapest because the ones that overflow are the trailing optional
// enrichments, not the Channel/ID pair at the front.
func clampEmbed(e *discordEmbed) {
	e.Title = ClampRunes(e.Title, limitTitle)
	e.Description = ClampRunes(e.Description, limitDescription)
	if e.Author != nil {
		e.Author.Name = ClampRunes(e.Author.Name, limitAuthorName)
	}
	if e.Footer != nil {
		e.Footer.Text = ClampRunes(e.Footer.Text, limitFooter)
	}
	if len(e.Fields) > limitFields {
		e.Fields = e.Fields[:limitFields]
	}
	for i := range e.Fields {
		e.Fields[i].Name = ClampRunes(e.Fields[i].Name, limitFieldName)
		e.Fields[i].Value = ClampRunes(e.Fields[i].Value, limitFieldValue)
	}

	for embedRunes(e) > limitTotal && len(e.Fields) > 0 {
		e.Fields = e.Fields[:len(e.Fields)-1]
	}
	if over := embedRunes(e) - limitTotal; over > 0 {
		e.Description = ClampRunes(e.Description, utf8.RuneCountInString(e.Description)-over)
	}
}
```

- [ ] **Step 4: Call it from `buildPayload`**

In `internal/notifications/discord.go`, insert immediately before `payload := discordPayload{Embeds: []discordEmbed{embed}}`:

```go
	// One clamp for all ~36 send sites. Over ANY Discord limit is a 400, and
	// the ladder below treats a non-429 4xx as permanent, so an unclamped
	// embed is a silently dropped alert.
	clampEmbed(&embed)
```

- [ ] **Step 5: Accept `discordapp.com`**

In `internal/notifications/manager.go`, replace the regex at `:16`:

```go
// discordWebhookRe validates standard Discord webhook URLs (HTTPS only).
// discordapp.com is Discord's legacy domain and still serves webhooks; a URL
// pasted from an old bookmark was rejected outright before it was accepted here
// (audit R6). parseTarget canonicalises it to discord.com.
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord(?:app)?\.com/api/webhooks/\d+/[\w-]+`)
```

and in `parseTarget` replace the two Discord-URL arms (`:203-207`):

```go
	case discordWebhookRe.MatchString(url):
		// Canonicalise the legacy host. Two reasons, both load-bearing:
		// buildTargets dedupes on the RESOLVED URL, so the two spellings of
		// one webhook would otherwise build two targets and post every embed
		// twice; and Go's http.Client turns a 301/302 on a POST into a GET,
		// so following discordapp.com's redirect would drop the body.
		return &DiscordWebhook{URL: strings.Replace(url, "discordapp.com", "discord.com", 1)}, nil

	case strings.Contains(url, "discord.com/api/webhooks"), strings.Contains(url, "discordapp.com/api/webhooks"):
		return nil, fmt.Errorf("invalid Discord webhook URL: must be HTTPS with a numeric ID and token")
```

- [ ] **Step 6: Fix the byte cut and escape the four unbounded producer strings**

`internal/worker/orchestrator_mux.go:1266-1276` — replace the whole Description-excerpt block, starting at the `// Description excerpt` comment and ending at the closing brace of the `if finishedJob.Description != ""` body. Do **not** start at `:1265`: that line closes the Trimmed Range `if` above it, and taking it deletes a brace.

```go
	// Description excerpt. Cut on a RUNE boundary through the notifications
	// clamp: the old desc[:descMaxLen-3] was a byte slice, and a Japanese
	// description (the norm here) splits mid-rune, after which encoding/json
	// emits U+FFFD and the operator reads mojibake. Escaped too — a
	// description is job-supplied text and renders Discord markdown.
	const descMaxLen = 300
	if finishedJob.Description != "" {
		fb.Add("Description", notifications.ClampRunes(notifications.EscapeMarkdown(finishedJob.Description), descMaxLen))
	}
```

`internal/worker/worker.go:1332` — inside the `NewFieldBuilder()` chain, replace `Add("Error", errMsg).` with:

```go
				Add("Error", notifications.EscapeMarkdown(errMsg)).
```

`internal/worker/orchestrator.go:657` — replace the Error field:

```go
								{Name: "Error", Value: notifications.EscapeMarkdown(trimErr.Error())},
```

`cmd/moombox/monitor_callbacks.go:1606` — replace the Last Error field:

```go
					{Name: "Last Error", Value: notifications.EscapeMarkdown(lastErr)},
```

Those four are the unbounded, job- or remote-supplied strings the audit named. Stream TITLES are deliberately **not** escaped here: they live in the embed descriptions that Arc N2a rewrites into `internal/notifications/builders.go`, and escaping them twice (once here, once in the builder) would show an operator a literal backslash. N2a applies `EscapeMarkdown` in the builders.

- [ ] **Step 7: Add the producer-side test**

Append to `internal/worker/orchestrator_mux_test.go`:

```go
// TestDescriptionExcerptCutsOnARuneBoundary is the fix for the byte slice at
// the Description excerpt. A Japanese description — the norm for this project's
// archives — was cut mid-rune, and encoding/json then replaced the broken tail
// with U+FFFD.
//
// WHAT THIS PINS: the helper COMPOSITION and its 300-rune budget, not the call
// site — it calls the helpers directly and never reaches
// sendFinishedNotification, so reverting that line to desc[:descMaxLen-3] would
// not fail here. The call site is pinned separately, by the "Description" field
// a notificationtest.Recorder reads off a finished send in Task 6's fixture;
// ClampRunes' own boundary behaviour is pinned exhaustively in
// internal/notifications/limits_test.go.
func TestDescriptionExcerptCutsOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("あ", 500)
	got := notifications.ClampRunes(notifications.EscapeMarkdown(long), 300)
	if !utf8.ValidString(got) {
		t.Fatalf("the excerpt is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 300 {
		t.Errorf("excerpt = %d runes, want 300", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt does not end with the clamp marker: %q", got[len(got)-12:])
	}
}
```

Add `"strings"`, `"unicode/utf8"` and `"github.com/vampiricwulf/Moombox/internal/notifications"` to that file's imports if they are not already present.

- [ ] **Step 8: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestDescriptionExcerpt|TestFinishedImage' ./internal/worker/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```
Expected: two `ok` lines and a silent build.

- [ ] **Step 9: Commit**

```bash
cat > .superpowers/gotmp/n1-t3.msg <<'EOF'
fix(notifications): clamp every Discord limit on rune boundaries

Nothing clamped. Over ANY Discord embed limit is a 400, and the delivery
ladder treats a non-429 4xx as permanent, so one long error string dropped
the whole alert after a single attempt. clampEmbed runs inside buildPayload,
so all ~36 send sites are covered without touching one of them; the total
budget drops trailing fields first, then trims the description, and never
the title.

ClampRunes and EscapeMarkdown are exported for producers. The job Description
excerpt was cut by BYTE and split Japanese mid-rune (json then emits U+FFFD);
it and the three unbounded error strings now go through both.

parseTarget accepts (and canonicalises) the legacy discordapp.com host, so the
two spellings of one webhook dedupe instead of posting twice.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t3.msg -- \
  internal/notifications/limits.go \
  internal/notifications/limits_test.go \
  internal/notifications/discord.go \
  internal/notifications/manager.go \
  internal/notifications/manager_test.go \
  internal/worker/orchestrator_mux.go \
  internal/worker/orchestrator_mux_test.go \
  internal/worker/worker.go \
  internal/worker/orchestrator.go \
  cmd/moombox/monitor_callbacks.go
```

---
## Task 4: The per-target FIFO sender replaces the 16-slot semaphore

**Files:**
- Create: `internal/notifications/queue.go`
- Modify: `internal/notifications/manager.go:138-146` (delete `maxInflightNotifications`), `:152-178` (the `Manager` struct, `notificationTarget`, the `sender` interface), `:241-345` (`buildTargets` — add the key), `:347-367` (`NewManager`), `:369-379` (`Reload`), `:381-437` (`Send`), `:448-484` (`Wait`, `HasTargets`), `:223-239` (`SendTest`)
- Modify: `internal/notifications/discord.go:125-144` (`sendOnce` → exported `SendOnce`)
- Create: `internal/notifications/queue_test.go`
- Modify: `internal/notifications/manager_test.go` (retype the three struct literals; add `SendOnce` to `recordingSender`; add the test constructor)
- Modify: `internal/notifications/manager_dispatch_test.go` (retype; add `SendOnce` to `senderFunc`; replace the semaphore test)

**Interfaces:**
- Consumes: Task 2's `effectiveTier` / `Tier`.
- Produces, read by Task 5 and Task 8:
  ```go
  // queue.go
  const notificationQueueCap = 256
  type queued struct { title, description string; color int; fields []Field; opts SendOptions; tier Tier }
  type targetQueue struct { /* sender, key, events, items, wake, done, ... */ }
  func newTargetQueue(t notificationTarget, logger <anonymous 4-method>, shuttingDown *atomic.Bool) *targetQueue
  func (q *targetQueue) run()
  func (q *targetQueue) enqueue(it queued)
  func (q *targetQueue) allows(event string) bool
  func (q *targetQueue) setEvents(events map[string]bool)
  func (q *targetQueue) stopDiscard()
  func (q *targetQueue) closeDrain()

  // manager.go
  type sender interface {
      Send(title, description string, color int, fields []Field, opts SendOptions) error
      SendOnce(title, description string, color int, fields []Field, opts SendOptions) error
  }
  type notificationTarget struct { sender sender; events map[string]bool; key string }
  func (m *Manager) applyTargets(built []notificationTarget)
  func (m *Manager) BeginShutdown()   // body replaced; Task 1 declared it empty

  // discord.go
  func (d *DiscordWebhook) SendOnce(title, description string, color int, fields []Field, opts SendOptions) error
  ```
  `Manager.targets` changes type from `[]notificationTarget` to `[]*targetQueue`; the field NAME is kept so the three existing tests that read `m.targets[0].events` need only a literal retype.

**What is wrong today** (audit §1 Delivery mechanics, R1-R4): `Send` spawns one goroutine per (event × target) behind a global 16-slot semaphore (`manager.go:414-435`). There is no queue, so the 17th concurrent send is **dropped** regardless of importance — a `Job Failed` loses to sixteen `Stream Found`s parked in Retry-After sleeps. There is no ordering, so a 5xx-retried "Muxing Starting" can land after "Download Finished". And sixteen concurrent POSTs to ONE webhook bucket self-inflict 429s that each cost an attempt.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/queue_test.go`:

```go
package notifications

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateSender blocks each delivery on a per-call gate so a test can hold the
// queue's head still while it fills the tail, and records what was delivered
// and by which method.
type gateSender struct {
	gate chan struct{} // closed to release every delivery

	mu     sync.Mutex
	titles []string
	once   []string // titles delivered via SendOnce
}

func newGateSender() *gateSender { return &gateSender{gate: make(chan struct{})} }

func (g *gateSender) Send(title, _ string, _ int, _ []Field, _ SendOptions) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, title)
	return nil
}

func (g *gateSender) SendOnce(title, _ string, _ int, _ []Field, _ SendOptions) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, title)
	g.once = append(g.once, title)
	return nil
}

func (g *gateSender) release() { close(g.gate) }

func (g *gateSender) delivered() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.titles...)
}

func (g *gateSender) singleAttempts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.once...)
}

// waitFor polls cond until it holds or the deadline passes. The queue is
// asynchronous by construction, so every assertion about delivery needs one;
// a fixed sleep either flakes on a loaded CI box or wastes the wall clock.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after 5s waiting for %s", what)
}

// TestQueueDeliversInFIFOOrder is R3. Every send was its own goroutine, so two
// embeds for one job raced and a retried one landed after a later one — an
// operator could read "Download Finished" above "Muxing Starting". One
// goroutine per target makes the order free.
//
// THE MUTANT: `go q.deliver(it, ...)` inside the run loop. The three titles
// then arrive in an arbitrary order and the assertion fails (not every run —
// run it with -count=3, which the gate set does).
func TestQueueDeliversInFIFOOrder(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, time.Second, notificationTarget{sender: g, key: "k1"})

	for _, title := range []string{"first", "second", "third"} {
		m.Send(title, "", TypeInfo, nil, SendOptions{Event: "finished"})
	}
	g.release()
	waitFor(t, "three deliveries", func() bool { return len(g.delivered()) == 3 })

	want := []string{"first", "second", "third"}
	got := g.delivered()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v — a job's embeds must never reorder", got, want)
		}
	}
}

// TestQueueOverflowDropsTheOldestLowTier is R2. The old semaphore dropped the
// NEWEST notification when 16 were in flight, so a backfill sweep's finds
// could shut out the Job Failed that followed them.
//
// THE MUTANT: dropping the newest unconditionally (the `error` never arrives),
// or dropping the oldest unconditionally (a queue full of alerts starts
// shedding alerts).
func TestQueueOverflowDropsTheOldestLowTier(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	// One send is popped immediately and blocks on the gate; the next
	// notificationQueueCap fill the queue exactly.
	m.Send("found-0", "", TypeInfo, nil, SendOptions{Event: "found"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("found-"+itoa(i), "", TypeInfo, nil, SendOptions{Event: "found"})
	}
	if got := m.targets[0].pending(); got != notificationQueueCap {
		t.Fatalf("queue holds %d, want %d before the overflow", got, notificationQueueCap)
	}

	// The overflow: an alert arrives at a full queue.
	m.Send("the-alert", "", TypeError, nil, SendOptions{Event: "error"})

	g.release()
	waitFor(t, "the queue to drain", func() bool { return len(g.delivered()) == notificationQueueCap+1 })

	got := g.delivered()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "the-alert") {
		t.Errorf("the alert was dropped at a full queue — that is exactly the old semaphore's bug: %v", got)
	}
	if strings.Contains(joined, "found-1,") {
		t.Errorf("found-1 survived — the OLDEST queued low-tier entry is the one that goes: %v", got[:5])
	}
	if !strings.Contains(joined, "found-0") {
		t.Errorf("found-0 was the in-flight item and must still be delivered: %v", got[:5])
	}
}

// TestQueueOverflowDropsTheNewestWhenNothingIsLowTier is the other half of the
// policy: with nothing sheddable queued, the arrival is what goes, and it says
// so in a Warn. Silently dropping an older ALERT to make room for a newer one
// would lose the first symptom of an incident to its second.
func TestQueueOverflowDropsTheNewestWhenNothingIsLowTier(t *testing.T) {
	g := newGateSender()
	lg := &countingLogger{}
	m := newTestManagerWithLogger(t, lg, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	m.Send("error-0", "", TypeError, nil, SendOptions{Event: "error"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("error-"+itoa(i), "", TypeError, nil, SendOptions{Event: "error"})
	}
	m.Send("the-newest", "", TypeError, nil, SendOptions{Event: "error"})

	g.release()
	waitFor(t, "the queue to drain", func() bool { return len(g.delivered()) == notificationQueueCap+1 })

	if joined := strings.Join(g.delivered(), ","); strings.Contains(joined, "the-newest") {
		t.Error("the newest arrival was kept — with nothing low-tier queued it is what must go")
	}
	if !lg.sawWarnContaining("the-newest") {
		t.Errorf("no Warn named the dropped notification; warns = %v", lg.warns)
	}
}

// TestReloadKeepsSurvivorsAndDiscardsRemovedTargets pins the hot-reload diff.
// A target that survives keeps its goroutine, its pending items and the rate
// bucket its sender learned — restarting it would re-learn the bucket from
// scratch on every config save. A removed one finishes what it is doing and
// exits, reporting the count it dropped.
//
// THE MUTANT: rebuilding every queue on Reload (the survivor's pointer
// changes), or leaving a removed target's goroutine running (its done channel
// never closes).
func TestReloadKeepsSurvivorsAndDiscardsRemovedTargets(t *testing.T) {
	const tok = "tok-en_ABC"
	keep := "https://discord.com/api/webhooks/111111111111111111/" + tok
	drop := "https://discord.com/api/webhooks/222222222222222222/" + tok

	lg := &countingLogger{}
	m := NewManager(notifConfigWithURLs(keep, drop), lg)
	t.Cleanup(m.Wait)
	if len(m.targets) != 2 {
		t.Fatalf("built %d targets, want 2", len(m.targets))
	}
	survivor := m.targets[0]
	removed := m.targets[1]

	m.Reload(notifConfigWithURLs(keep))

	if len(m.targets) != 1 {
		t.Fatalf("after reload: %d targets, want 1", len(m.targets))
	}
	if m.targets[0] != survivor {
		t.Error("the surviving target was rebuilt — its queue, its in-flight item and its learned rate bucket are all thrown away")
	}
	select {
	case <-removed.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the removed target's goroutine is still running 5s after Reload")
	}
}

// TestReloadSwapsTheFilterOnASurvivingTarget: keeping the goroutine must not
// mean keeping the old allowlist.
func TestReloadSwapsTheFilterOnASurvivingTarget(t *testing.T) {
	const url = "https://discord.com/api/webhooks/333333333333333333/tok-en_ABC"
	cfgWith := func(events ...string) *config.MoomboxConfig {
		return &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url, Events: events}}}
	}
	m := NewManager(cfgWith("found"), testLogger{})
	t.Cleanup(m.Wait)
	q := m.targets[0]
	if !q.allows("found") || q.allows("error") {
		t.Fatalf("initial filter wrong: found=%v error=%v", q.allows("found"), q.allows("error"))
	}
	m.Reload(cfgWith("error"))
	if q.allows("found") || !q.allows("error") {
		t.Errorf("after reload the surviving target kept the old filter: found=%v error=%v", q.allows("found"), q.allows("error"))
	}
}

// TestWaitDrainsTheQueue is what makes shutdown step 3 mean something: the
// embeds a worker stop produced must still go out.
func TestWaitDrainsTheQueue(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})
	for _, title := range []string{"a", "b", "c"} {
		m.Send(title, "", TypeInfo, nil, SendOptions{Event: "finished"})
	}
	g.release()
	m.Wait()
	if got := len(g.delivered()); got != 3 {
		t.Errorf("Wait returned with %d of 3 delivered — it must drain the queue", got)
	}
}

// TestBeginShutdownSendsSingleAttempt is the §0 ruling: the process force-exits
// 10s after shutdown begins, and the worker stop ahead of it can eat the whole
// window, so a three-attempt ladder with a 2s+5s backoff cannot finish. One
// attempt per embed is what fits, and the docs say so.
//
// THE MUTANT: dropping the shuttingDown check in the run loop. The delivery
// then goes through Send and the SendOnce list stays empty.
func TestBeginShutdownSendsSingleAttempt(t *testing.T) {
	g := newGateSender()
	g.release() // nothing blocks; we only care which method is used
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	m.BeginShutdown()
	m.Send("shutting-down", "", TypeError, nil, SendOptions{Event: "error"})
	m.Wait()

	if got := g.singleAttempts(); len(got) != 1 || got[0] != "shutting-down" {
		t.Errorf("single-attempt deliveries = %v, want [shutting-down] — a shutdown send must not run the retry ladder", got)
	}
}

// TestSendOnANilManagerIsANoOp pins the guard the interface seam depends on: a
// deps struct carrying a typed-nil *Manager defeats every `!= nil` check at the
// producers, so the method itself has to be safe.
func TestSendOnANilManagerIsANoOp(t *testing.T) {
	var m *Manager
	m.Send("t", "d", TypeInfo, nil, SendOptions{Event: "finished"}) // must not panic
}
```

Add these helpers to `internal/notifications/manager_test.go`:

```go
// itoa avoids pulling strconv into the queue tests for one call.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// newTestManager starts a Manager over ready-made senders exactly as
// NewManager does — applyTargets is the only path that creates queues and
// starts goroutines, so a test that built the slice by hand would be testing a
// Manager production never produces.
func newTestManager(t *testing.T, waitTimeout time.Duration, targets ...notificationTarget) *Manager {
	t.Helper()
	return newTestManagerWithLogger(t, testLogger{}, waitTimeout, targets...)
}

func newTestManagerWithLogger(t *testing.T, lg interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, waitTimeout time.Duration, targets ...notificationTarget,
) *Manager {
	t.Helper()
	m := &Manager{logger: lg, waitTimeout: waitTimeout}
	m.applyTargets(targets)
	t.Cleanup(func() {
		for _, q := range m.targets {
			q.stopDiscard()
		}
	})
	return m
}
```

and extend `countingLogger` (same file, currently `:296-306`) so the overflow test can read the Warns. **It needs a mutex now**: a `countingLogger` handed to a `Manager` is written from the queue's goroutine (`pop`'s discard Warn, `deliver`'s Error) at the same time as the test goroutine's `enqueue` Warn and its `sawWarnContaining` read — one `-race -count=3` away from a flake, and Task 9 runs exactly that.

```go
type countingLogger struct {
	mu    sync.Mutex
	infos []string
	args  [][]any
	warns []string
}

func (l *countingLogger) Debug(string, ...any) {}

func (l *countingLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
	l.args = append(l.args, args)
}

func (l *countingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprint(args...))
}

func (l *countingLogger) Error(string, ...any) {}

// sawWarnContaining reports whether any Warn line (message or args) carries s.
func (l *countingLogger) sawWarnContaining(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}
```
This REPLACES the existing `countingLogger` declaration and its four methods wholesale (`manager_test.go:296-306`). Add `"sync"` and `"time"` to the file's imports; `fmt` and `strings` are already there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/`

Expected: a build failure —
```
# github.com/vampiricwulf/Moombox/internal/notifications [github.com/vampiricwulf/Moombox/internal/notifications.test]
internal/notifications/queue_test.go:NN:NN: undefined: newTestManager
internal/notifications/queue_test.go:NN:NN: undefined: notificationQueueCap
internal/notifications/queue_test.go:NN:NN: m.targets[0].pending undefined (type notificationTarget has no field or method pending)
internal/notifications/queue_test.go:NN:NN: m.BeginShutdown undefined (type *Manager has no field or method BeginShutdown)
internal/notifications/manager_test.go:NN:NN: unknown field key in struct literal of type notificationTarget
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

- [ ] **Step 3: Create the queue**

Create `internal/notifications/queue.go`:

```go
package notifications

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// notificationQueueCap bounds one target's pending FIFO.
//
// 256 is deliberately generous: the burst this exists for is a backfill
// re-scan (R B), which creates a `found` per catalogue row across every
// configured channel, and a dead cookie, which parks N jobs at once. At
// roughly one delivery per second against a healthy webhook, 256 is over four
// minutes of backlog — long enough that reaching the cap means Discord is
// down, not that Moombox is busy.
const notificationQueueCap = 256

// queued is one embed waiting for one target. The fields mirror Send's
// parameters; the tier is resolved once at enqueue so the overflow policy
// never has to re-derive it.
type queued struct {
	title       string
	description string
	color       int
	fields      []Field
	opts        SendOptions
	tier        Tier
}

// targetQueue is one destination, its FIFO, and the single goroutine that
// drains it.
//
// ONE goroutine is the whole design. It buys three things the old
// goroutine-per-send could not: a job's embeds can never reorder (R3); a burst
// can never put sixteen concurrent POSTs into one webhook's rate bucket and
// self-inflict 429s that each cost a delivery attempt (R1); and a full queue
// can choose WHICH entry to shed instead of always shedding the arrival (R2).
type targetQueue struct {
	sender sender
	// key is the RESOLVED webhook URL — the same identity buildTargets dedupes
	// on. Reload matches on it so a surviving target keeps this queue, its
	// pending items, and the rate bucket its sender has learned.
	key string
	// shuttingDown is the Manager's flag, shared by pointer. When it is set,
	// deliveries make a single attempt instead of running the retry ladder.
	shuttingDown *atomic.Bool
	logger       interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// mu guards everything below. A plain mutex over a slice rather than a
	// buffered channel: a channel cannot drop its OLDEST element, which is
	// exactly what the overflow policy has to do.
	mu      sync.Mutex
	events  map[string]bool // nil means all events
	items   []queued
	closing bool // drain what is queued, then exit (Wait)
	discard bool // drop what is queued, then exit (a removed target)

	// wake carries one buffered token, so a signal sent while the goroutine is
	// between pop and receive is remembered rather than lost.
	wake chan struct{}
	// done closes when the goroutine has exited.
	done chan struct{}
}

func newTargetQueue(t notificationTarget, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, shuttingDown *atomic.Bool,
) *targetQueue {
	return &targetQueue{
		sender:       t.sender,
		key:          t.key,
		events:       t.events,
		shuttingDown: shuttingDown,
		logger:       logger,
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
}

// signal nudges the draining goroutine without ever blocking the caller —
// Send runs on worker, monitor and HTTP goroutines and must not wait on
// Discord for anything.
func (q *targetQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// allows reports whether this target's filter admits the event.
//
// An event that split from a broader legacy name (eventAliases) also matches
// targets allowlisting the old name, so pre-split filters keep working after
// an upgrade. The alias lookup needs the ok-check: a bare map miss yields "",
// and a garbage events=[""] entry would then match EVERY non-aliased event,
// inverting the allowlist.
func (q *targetQueue) allows(event string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.events == nil || event == "" {
		return true
	}
	if q.events[event] {
		return true
	}
	alias, hasAlias := eventAliases[event]
	return hasAlias && q.events[alias]
}

// setEvents swaps the filter on a target that survived a Reload.
func (q *targetQueue) setEvents(events map[string]bool) {
	q.mu.Lock()
	q.events = events
	q.mu.Unlock()
}

// pending is the queue depth, for tests and for the discard report.
func (q *targetQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// enqueue appends it, or applies the overflow policy.
//
// OVERFLOW: the OLDEST TierLow entry goes, not the arrival. The old semaphore
// dropped the newest, so a backfill sweep's sixteen in-flight `found`s shut out
// the `Job Failed` behind them — the exact inversion of what an operator needs.
// With nothing low-tier queued, the arrival is what goes: an older alert is the
// FIRST symptom of an incident and a newer one is usually its echo.
func (q *targetQueue) enqueue(it queued) {
	q.mu.Lock()
	if q.closing || q.discard {
		q.mu.Unlock()
		q.logger.Warn("dropping notification — the target is shutting down",
			"event", it.opts.Event, "title", it.title)
		return
	}
	if len(q.items) < notificationQueueCap {
		q.items = append(q.items, it)
		q.mu.Unlock()
		q.signal()
		return
	}
	victim := -1
	for i := range q.items {
		if q.items[i].tier == TierLow {
			victim = i
			break
		}
	}
	if victim < 0 {
		q.mu.Unlock()
		q.logger.Warn("notification queue full — dropping the newest",
			"cap", notificationQueueCap, "event", it.opts.Event, "title", it.title)
		return
	}
	dropped := q.items[victim]
	q.items = append(q.items[:victim], q.items[victim+1:]...)
	q.items = append(q.items, it)
	q.mu.Unlock()
	q.logger.Warn("notification queue full — dropped the oldest low-priority notification",
		"cap", notificationQueueCap, "event", dropped.opts.Event, "title", dropped.title)
	q.signal()
}

// pop takes the head. ok=false means nothing is queued; exit=true means the
// goroutine should return.
func (q *targetQueue) pop() (it queued, ok, exit bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.discard {
		// A removed target reports the count and nothing else. The queued
		// items belong to a webhook the operator has just deleted from their
		// config; delivering them after the fact would be the opposite of what
		// the edit asked for.
		if n := len(q.items); n > 0 {
			q.logger.Warn("notification target removed — discarding its queued notifications", "dropped", n)
			q.items = nil
		}
		return queued{}, false, true
	}
	if len(q.items) == 0 {
		return queued{}, false, q.closing
	}
	it = q.items[0]
	q.items = q.items[1:]
	return it, true, false
}

// stopDiscard retires a target removed by a Reload: it finishes the item it is
// delivering (cancelling mid-POST would leave Discord's side ambiguous) and
// then exits, discarding the rest.
func (q *targetQueue) stopDiscard() {
	q.mu.Lock()
	q.discard = true
	q.mu.Unlock()
	q.signal()
}

// closeDrain retires a target at shutdown: no new items are accepted, what is
// queued is delivered, then the goroutine exits.
func (q *targetQueue) closeDrain() {
	q.mu.Lock()
	q.closing = true
	q.mu.Unlock()
	q.signal()
}

// run is the single draining goroutine.
func (q *targetQueue) run() {
	defer close(q.done)
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("panic in notification sender loop", "panic", fmt.Sprint(r))
		}
	}()
	for {
		it, ok, exit := q.pop()
		if exit {
			return
		}
		if !ok {
			<-q.wake
			continue
		}
		q.deliver(it)
	}
}

// deliver posts one item. Its OWN recover, separate from run's: a panic inside
// one send must not kill the goroutine and strand every later notification for
// this target behind a queue nothing drains.
func (q *targetQueue) deliver(it queued) {
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("panic in notification sender", "panic", fmt.Sprint(r))
		}
	}()
	var err error
	if q.shuttingDown != nil && q.shuttingDown.Load() {
		// Owner ruling: shutdown sends are single-attempt and the 10s
		// force-exit stays. A 2s+5s retry ladder cannot finish inside a window
		// the worker stop may already have spent.
		err = q.sender.SendOnce(it.title, it.description, it.color, it.fields, it.opts)
	} else {
		err = q.sender.Send(it.title, it.description, it.color, it.fields, it.opts)
	}
	if err != nil {
		q.logger.Error("notification send failed", "err", err)
	}
}
```

- [ ] **Step 4: Rewrite the manager around it**

`internal/notifications/manager.go`:

**(a) Delete** the whole `maxInflightNotifications` block (`:138-146`, doc comment included). Its comment carried audit R7's second stale claim — "Discord rate-limits each webhook to 30 req/min" — which is not in Discord's docs at all (buckets are header-discovered); deleting the constant is the fix.

**(b) Replace** the `Manager` struct, `notificationTarget` and the `sender` interface (`:152-178`):

```go
// Manager dispatches notifications to configured targets.
//
// One QUEUE per target, one goroutine per queue. Send is a non-blocking append
// — it is called from worker, monitor and HTTP goroutines and must never wait
// on Discord.
type Manager struct {
	// targetsMu guards targets and byKey: Reload (config hot-apply) rebuilds
	// them while Send/HasTargets read from worker goroutines.
	targetsMu sync.RWMutex
	targets   []*targetQueue
	// byKey indexes targets by resolved webhook URL so Reload can tell a
	// surviving target from a new one.
	byKey map[string]*targetQueue
	// shuttingDown flips once, in BeginShutdown; every queue holds a pointer
	// to it and reads it per delivery.
	shuttingDown atomic.Bool
	// waitTimeout bounds Wait; zero means defaultWaitTimeout (test literals
	// omit it). Set once at construction, never written afterwards.
	waitTimeout time.Duration
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// notificationTarget is one destination as buildTargets resolved it, before
// the manager gives it a queue and a goroutine.
type notificationTarget struct {
	sender sender
	events map[string]bool // nil means all events
	// key is the RESOLVED webhook URL: the dedupe identity, and what Reload
	// matches a surviving target on.
	key string
}

// sender is one delivery destination.
//
// Send runs the full retry ladder; SendOnce makes exactly one attempt — used
// during shutdown (the 10s force-exit cannot accommodate a 2s+5s ladder) and by
// SendTest, where an interactive caller wants the immediate outcome.
type sender interface {
	Send(title, description string, color int, fields []Field, opts SendOptions) error
	SendOnce(title, description string, color int, fields []Field, opts SendOptions) error
}
```

Add `"sync/atomic"` to the import block.

**(c) In `buildTargets`**, set the key on the appended target. Replace the append (`:331-334`) with:

```go
		targets = append(targets, notificationTarget{
			sender: s,
			events: events,
			key:    key,
		})
```
(`key` is already computed at `:306-309` for the dedupe; it just was not carried.)

**(d) Add `applyTargets`** immediately above `NewManager`:

```go
// applyTargets installs built as the live target set.
//
// The DIFF is on the resolved webhook URL. A target that is still configured
// keeps its existing queue — and therefore its pending items, its in-flight
// delivery and the rate bucket its sender has learned — because a config save
// that touches an unrelated section must not cost every webhook its bucket
// state and its backlog. A target that is gone is told to discard after its
// in-flight item; a new one gets a goroutine.
func (m *Manager) applyTargets(built []notificationTarget) {
	m.targetsMu.Lock()
	previous := m.byKey
	next := make([]*targetQueue, 0, len(built))
	byKey := make(map[string]*targetQueue, len(built))
	for _, t := range built {
		if q, survives := previous[t.key]; survives && t.key != "" {
			q.setEvents(t.events)
			next = append(next, q)
			byKey[t.key] = q
			delete(previous, t.key)
			continue
		}
		q := newTargetQueue(t, m.logger, &m.shuttingDown)
		next = append(next, q)
		if t.key != "" {
			byKey[t.key] = q
		}
		go q.run()
	}
	retired := make([]*targetQueue, 0, len(previous))
	for _, q := range previous {
		retired = append(retired, q)
	}
	m.targets = next
	m.byKey = byKey
	m.targetsMu.Unlock()

	// Outside the lock: stopDiscard takes the queue's own mutex and logs.
	for _, q := range retired {
		q.stopDiscard()
	}
}
```

**(e) `NewManager`** (`:355-366`) becomes:

```go
	m := &Manager{
		logger:      logger,
		waitTimeout: defaultWaitTimeout,
	}
	m.applyTargets(buildTargets(cfg, logger))

	if len(m.targets) > 0 {
		logger.Info("notifications initialized", "targets", len(m.targets))
	}

	return m
```

**(f) `Reload`** (`:373-379`) becomes:

```go
func (m *Manager) Reload(cfg *config.MoomboxConfig) {
	if m == nil {
		return
	}
	m.applyTargets(buildTargets(cfg, m.logger))
	m.targetsMu.RLock()
	n := len(m.targets)
	m.targetsMu.RUnlock()
	m.logger.Info("notification targets reloaded", "targets", n)
}
```

**(g) `Send`** (`:381-437`) becomes — the nil guard from Task 1 is kept:

```go
func (m *Manager) Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) {
	if m == nil {
		return
	}
	// Snapshot under RLock so a concurrent Reload can't swap the slice
	// mid-iteration. The slice is replaced wholesale, never mutated in
	// place, so iterating the snapshot after release is safe.
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()
	if len(targets) == 0 {
		return
	}

	it := queued{
		title:       title,
		description: description,
		color:       ntype.Color(),
		fields:      fields,
		opts:        opts,
		tier:        effectiveTier(opts),
	}
	for _, q := range targets {
		if !q.allows(opts.Event) {
			continue
		}
		q.enqueue(it)
	}
}

**Replace Task 1's empty `BeginShutdown` body** (it was declared there so `Notifier` would compile) with the real one:

```go
// BeginShutdown puts every target into single-attempt mode.
//
// Owner ruling: the 10s force-exit (cmd/moombox/shutdown.go) stays, and a
// worker stop ahead of it can legitimately spend the whole window, so a
// three-attempt ladder with a 2s+5s backoff simply does not fit. One attempt
// per embed is what can be delivered, and operations.md says so rather than
// promising a drain that cannot happen.
func (m *Manager) BeginShutdown() {
	if m == nil {
		return
	}
	m.shuttingDown.Store(true)
}
```

**(h) `Wait`** (`:448-477`) becomes:

```go
// Wait stops accepting new notifications for every target, drains what is
// already queued, and returns when the last goroutine has exited or the wait
// timeout (defaultWaitTimeout, 30 s, unless injected) expires.
//
// **Single-call**: after Wait returns, every queue is closed and later Sends
// are dropped with a Warn. The graceful-shutdown sequence in cmd/moombox stops
// the worker — the dominant Send caller — before invoking Wait, so this is the
// correct ordering. In practice the process's own 10 s force-exit, not this
// timeout, is what bounds the drain; BeginShutdown is what makes the attempts
// fit inside it.
func (m *Manager) Wait() {
	if m == nil {
		return
	}
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()

	timeout := m.effectiveWaitTimeout()
	deadline := time.After(timeout)
	for _, q := range targets {
		q.closeDrain()
	}
	for _, q := range targets {
		select {
		case <-q.done:
		case <-deadline:
			if m.logger != nil {
				m.logger.Warn("notification wait timed out", "after", timeout)
			}
			return
		}
	}
}
```

**(i) `HasTargets`** (`:480-484`) gains the nil guard:

```go
func (m *Manager) HasTargets() bool {
	if m == nil {
		return false
	}
	m.targetsMu.RLock()
	defer m.targetsMu.RUnlock()
	return len(m.targets) > 0
}
```

**(j) `SendTest`** (`:227-239`) loses its type assertion, because `SendOnce` is on the interface now:

```go
func SendTest(url string) error {
	s, err := parseTarget(url)
	if err != nil {
		return err
	}
	return s.SendOnce("Test Notification",
		"Moombox notifications are configured correctly",
		TypeSuccess.Color(),
		[]Field{{Name: "Status", Value: "Working", Inline: true}},
		SendOptions{})
}
```

- [ ] **Step 5: Export `sendOnce`**

In `internal/notifications/discord.go`, rename the method (`:125-128`) and update its doc:

```go
// SendOnce performs a single delivery attempt with NO retries.
//
// Two callers: SendTest, where an interactive settings flow wants the
// immediate outcome (surfacing a 429 beats sleeping through its Retry-After),
// and the per-target queue during shutdown, where the 10s force-exit leaves no
// room for the ladder. Bounded by the single-attempt request timeout (~15s).
func (d *DiscordWebhook) SendOnce(title, description string, color int, fields []Field, opts SendOptions) error {
```
The body is unchanged.

- [ ] **Step 6: Update the two existing test files**

`internal/notifications/manager_dispatch_test.go`:
- Add `SendOnce` to `senderFunc` (below its `Send`):
  ```go
  func (f senderFunc) SendOnce(title, description string, color int, fields []Field, opts SendOptions) error {
  	return f(title, description, color, fields, opts)
  }
  ```
- `TestManagerWaitTimesOut`: replace the `&Manager{...}` literal with `m := newTestManager(t, injected, notificationTarget{sender: hanging, key: "hang"})`, and delete the now-unused `semaphore:` line. Keep every assertion.
- **Delete `TestManagerSemaphoreBoundsConcurrency` entirely** — the semaphore it pins no longer exists, and `TestQueueDeliversInFIFOOrder` plus the two overflow tests are what replace it. Delete the now-unused `var _ sync.Mutex` line and the `sync`/`sync/atomic` imports if nothing else in the file uses them.

`internal/notifications/manager_test.go`:
- Add `SendOnce` to `recordingSender`:
  ```go
  func (r *recordingSender) SendOnce(title, description string, color int, fields []Field, opts SendOptions) error {
  	return r.Send(title, description, color, fields, opts)
  }
  ```
- `TestEventAliasRoutesToLegacyFilter` and `TestEmptyEventFilterEntryMatchesNothing`'s second half: replace each hand-built `&Manager{...}` with `newTestManager(t, time.Second, notificationTarget{sender: rec, events: map[string]bool{...}, key: "k"})`.
- `TestEmptyEventFilterEntryMatchesNothing`'s first half and `TestNewManagerEventFilter` / `TestNewManagerNoEventsFilterPassesAll` read `m.targets[0].events` — these keep compiling unchanged, because `targetQueue` has an `events` field of the same type.
- `TestHasTargetsWithTarget`: `&Manager{targets: []notificationTarget{{sender: nil}}}` becomes `&Manager{targets: []*targetQueue{{}}}`.
- `TestBuildTargetsDedupesByResolvedURL` and `TestBuildTargetsLogsTheCollapsedCountOnce` call `buildTargets` directly and are unchanged.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -race -count=3 ./internal/notifications/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```
Expected:
```
ok  	github.com/vampiricwulf/Moombox/internal/notifications	<time>
```
`-count=3` is not decoration here: `TestQueueDeliversInFIFOOrder` is an ordering assertion, and a `go q.deliver(...)` mutation passes a single run often enough to be missed.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t4.msg <<'EOF'
feat(notifications): per-target FIFO queues replace the 16-slot semaphore

Send spawned one goroutine per (event x target) behind a global 16-slot
semaphore. The 17th send was DROPPED regardless of importance — a Job Failed
lost to sixteen Stream Founds parked in Retry-After sleeps — there was no
ordering (a 5xx-retried "Muxing Starting" could land after "Download
Finished"), and sixteen concurrent POSTs to one webhook bucket self-inflicted
429s that each cost an attempt.

Now: one bounded FIFO (256) per target with one goroutine draining it.
Ordering is free. Overflow sheds the OLDEST low-tier entry (found/added/
scheduled/rescheduled) and never an alert; with nothing low-tier queued the
arrival goes, with a Warn naming it. Reload diffs on the resolved webhook URL,
so a surviving target keeps its goroutine, its backlog and its rate bucket.
BeginShutdown makes deliveries single-attempt, which is what fits inside the
process's 10 s force-exit.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t4.msg -- \
  internal/notifications/queue.go \
  internal/notifications/queue_test.go \
  internal/notifications/manager.go \
  internal/notifications/manager_test.go \
  internal/notifications/manager_dispatch_test.go \
  internal/notifications/discord.go
```

---
## Task 5: Rate-bucket awareness, and shutdown declares itself

**Files:**
- Modify: `internal/notifications/discord.go:25-33` (two comment rewrites — no const is added), `:56-59` (`DiscordWebhook` gains bucket state), `:125-144` (`SendOnce`), `:146-203` (`Send` — the bucket calls plus the third stale `semaphore` comment), `:234-272` (`post` returns the rate headers)
- Create: `internal/notifications/ratelimit_test.go`
- Modify: `cmd/moombox/shutdown.go:65` (one added line)

**Interfaces:**
- Consumes: Task 4's `sender` interface and `BeginShutdown`.
- Produces (all unexported, inside `internal/notifications`):
  ```go
  type discordResponse struct {
      status     int
      retryAfter string
      rateRemain string
      rateReset  string
      snippet    string
  }
  func (d *DiscordWebhook) post(body []byte) (discordResponse, error)
  func (d *DiscordWebhook) waitForBucket()
  func (d *DiscordWebhook) noteBucket(r discordResponse)
  ```

**Why the bucket lives in `DiscordWebhook` and not in the manager.** The spec says "the sender reads `X-RateLimit-Remaining` / `X-RateLimit-Reset-After` and sleeps pre-emptively". With one goroutine per target (Task 4), a sleep inside `DiscordWebhook.Send` blocks exactly that target's queue and nothing else — the same effect as sleeping in the queue loop — while keeping the manager transport-agnostic and leaving Task 4's test doubles on the plain 5-argument `sender` shape. It also means the bucket survives a `Reload` for free: `applyTargets` keeps the surviving queue, and the queue holds the same `*DiscordWebhook`.

Per Discord API docs (`topics/rate-limits.mdx`) the bucket is discoverable **only** from the headers — there is no published numeric cap — which is why the deleted `maxInflightNotifications` comment's "30 req/min" was wrong (audit R7).

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/ratelimit_test.go`:

```go
package notifications

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// arrivalRecorder timestamps each request so a test can measure the gap
// between two deliveries. Wall-clock, deliberately: the thing under test is a
// real time.Sleep, and a fake clock would pin the arithmetic while leaving the
// sleep itself unexercised.
type arrivalRecorder struct {
	mu   sync.Mutex
	when []time.Time
}

func (a *arrivalRecorder) mark() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.when = append(a.when, time.Now())
}

func (a *arrivalRecorder) gap() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.when) < 2 {
		return 0
	}
	return a.when[1].Sub(a.when[0])
}

// TestSleepsBeforeAnEmptyBucket is R1. Sixteen concurrent goroutines hammered
// one webhook's bucket and the X-RateLimit-* headers were never read, so
// Moombox produced its own 429s and each one cost a delivery attempt. A
// response saying "Remaining: 0, Reset-After: N" is Discord telling us exactly
// when the next request may go; honouring it pre-emptively costs one sleep and
// saves an attempt.
//
// THE MUTANT: dropping the waitForBucket call at the top of the attempt loop.
// The second request then arrives immediately and the gap assertion fails.
func TestSleepsBeforeAnEmptyBucket(t *testing.T) {
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rec.mark()
		rw.Header().Set("X-RateLimit-Remaining", "0")
		rw.Header().Set("X-RateLimit-Reset-After", "0.15")
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.Send("first", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := d.Send("second", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if got := rec.gap(); got < 140*time.Millisecond {
		t.Errorf("the second request arrived %v after the first — the empty-bucket sleep did not happen (want >= ~150ms)", got)
	}
}

// TestDoesNotSleepWhenTheBucketHasRoom is the premise: a sleep on every send
// would throttle a healthy webhook to one embed per window for nothing.
func TestDoesNotSleepWhenTheBucketHasRoom(t *testing.T) {
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rec.mark()
		rw.Header().Set("X-RateLimit-Remaining", "4")
		rw.Header().Set("X-RateLimit-Reset-After", "5")
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	for range 2 {
		if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := rec.gap(); got > 500*time.Millisecond {
		t.Errorf("the second request waited %v although the bucket reported 4 remaining", got)
	}
}

// TestBucketClearsWhenTheWindowRefills: a stale "Remaining: 0" must not keep
// sleeping forever once Discord reports room again.
func TestBucketClearsWhenTheWindowRefills(t *testing.T) {
	var n int
	var mu sync.Mutex
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset-After", "0.05")
		} else {
			rec.mark()
			rw.Header().Set("X-RateLimit-Remaining", "9")
			rw.Header().Set("X-RateLimit-Reset-After", "5")
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	for range 3 {
		if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := rec.gap(); got > 500*time.Millisecond {
		t.Errorf("the third request waited %v — the bucket was not cleared by a Remaining > 0 response", got)
	}
}

// TestA429DoesNotAlsoArmTheBucketSleep: Discord sets Remaining: 0 ON a 429, and
// the ladder already honours its Retry-After. Arming the pre-emptive sleep from
// the same response would wait the window twice and burn the cumulative-sleep
// budget on the double.
func TestA429DoesNotAlsoArmTheBucketSleep(t *testing.T) {
	shrinkBackoff(t)
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			rw.Header().Set("Retry-After", "0.05")
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset-After", "20")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	start := time.Now()
	if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send took %v — the 429's Retry-After and its X-RateLimit-Reset-After were both slept", elapsed)
	}
}

// TestSendOnceMakesExactlyOneAttempt is what BeginShutdown depends on.
func TestSendOnceMakesExactlyOneAttempt(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.SendOnce("t", "", 0, nil, SendOptions{}); err == nil {
		t.Fatal("SendOnce against a 500: want an error, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("SendOnce made %d attempts, want exactly 1 — a shutdown send must not run the ladder", hits)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestSleepsBefore|TestDoesNotSleep|TestBucketClears|TestA429Does|TestSendOnceMakes' ./internal/notifications/`

Expected:
```
--- FAIL: TestSleepsBeforeAnEmptyBucket (0.01s)
    ratelimit_test.go:NN: the second request arrived 1.2ms after the first — the empty-bucket sleep did not happen (want >= ~150ms)
FAIL
```
(the other four pass already: nothing reads the headers, so there is nothing to double-sleep, and `SendOnce` exists from Task 4.)

- [ ] **Step 3: Give `post` the headers**

In `internal/notifications/discord.go`, replace `post` (`:234-272`) and add the response struct above it:

```go
// discordResponse is what one attempt learned. Grouped rather than returned as
// five bare values: the rate-limit pair has to travel with the status, and a
// six-value signature is where a caller starts transposing arguments.
type discordResponse struct {
	status     int    // 0 on a transport error
	retryAfter string // Retry-After, 429 only
	rateRemain string // X-RateLimit-Remaining
	rateReset  string // X-RateLimit-Reset-After, in seconds
	snippet    string // sanitised body prefix, >= 400 only
}

// post performs one webhook POST attempt.
func (d *DiscordWebhook) post(body []byte) (discordResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return discordResponse{}, fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body (the queue's failure
		// log, SendTest's route response, retry-loop wrap all flow through
		// here).
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return discordResponse{}, fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return discordResponse{}, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	out := discordResponse{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		rateRemain: resp.Header.Get("X-RateLimit-Remaining"),
		rateReset:  resp.Header.Get("X-RateLimit-Reset-After"),
	}
	if resp.StatusCode >= 400 {
		// Read the reason BEFORE the deferred drain throws the rest away.
		// The read error is intentionally ignored: a partial read (e.g. the
		// connection drops mid-body) still yields whatever prefix arrived,
		// which is a usable snippet — better than discarding it outright.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		out.snippet = discordErrSnippet(b)
	}
	return out, nil
}
```

Add `"sync"` to the import block.

- [ ] **Step 4: Add the bucket to `DiscordWebhook`**

Replace the type (`:56-59`):

```go
// DiscordWebhook sends notifications via Discord webhook.
//
// One instance per target, held by that target's queue (manager.go), so the
// bucket state below is learned once and reused for every embed to that
// webhook — including across a config hot-reload, because applyTargets keeps a
// surviving target's queue and therefore this pointer.
type DiscordWebhook struct {
	URL string

	// bucketMu guards bucketRefillsAt. One sender goroutine per target means
	// there is no contention in production; the mutex is for SendTest (which
	// builds its own instance) and for the race detector.
	bucketMu sync.Mutex
	// bucketRefillsAt is when this webhook's rate bucket next has room, set
	// from an X-RateLimit-Remaining: 0 response. Zero means "not known empty".
	bucketRefillsAt time.Time
}
```

Add, near `discordStatusErr`:

```go
// waitForBucket honours what Discord last told us about this webhook's rate
// bucket: an X-RateLimit-Remaining of 0 means the NEXT request 429s until the
// window resets, so sleeping through it costs one wait and saves an attempt
// out of the three this notification has. Per Discord API docs
// (topics/rate-limits.mdx) the bucket is discoverable only from these headers —
// there is no published numeric cap.
//
// Capped by discordRetryAfterCap for the same reason a 429's Retry-After is:
// an absurd reset must not park a target's whole queue into next week.
func (d *DiscordWebhook) waitForBucket() {
	d.bucketMu.Lock()
	until := d.bucketRefillsAt
	d.bucketMu.Unlock()
	if until.IsZero() {
		return
	}
	wait := time.Until(until)
	if wait <= 0 {
		return
	}
	if wait > discordRetryAfterCap {
		wait = discordRetryAfterCap
	}
	time.Sleep(wait)
}

// noteBucket records (or clears) the empty-bucket deadline from one response.
//
// A 429 is deliberately EXCLUDED: Discord sets Remaining: 0 on one, and the
// ladder already honours its Retry-After, so arming the pre-emptive sleep from
// the same response would wait the window twice and spend the cumulative-sleep
// budget on the duplicate.
func (d *DiscordWebhook) noteBucket(r discordResponse) {
	if r.status == http.StatusTooManyRequests || r.rateRemain == "" {
		return
	}
	remaining, err := strconv.Atoi(r.rateRemain)
	if err != nil {
		return
	}
	if remaining > 0 {
		d.bucketMu.Lock()
		d.bucketRefillsAt = time.Time{}
		d.bucketMu.Unlock()
		return
	}
	// Validate in FLOAT space before converting, for the reason the 429 arm
	// documents: a value past ~9.2e9s (or Inf) overflows time.Duration to a
	// NEGATIVE on amd64. !(secs > 0) is deliberately NaN-proof.
	secs, err := strconv.ParseFloat(r.rateReset, 64)
	if err != nil || !(secs > 0) {
		return
	}
	if secs > discordRetryAfterCap.Seconds() {
		secs = discordRetryAfterCap.Seconds()
	}
	d.bucketMu.Lock()
	d.bucketRefillsAt = time.Now().Add(time.Duration(secs * float64(time.Second)))
	d.bucketMu.Unlock()
}
```

- [ ] **Step 5: Thread it through both delivery paths**

In `SendOnce` (`:128-144`), replace the body's first half so it uses the struct and the bucket:

```go
	body, err := buildPayload(title, description, color, fields, opts)
	if err != nil {
		return err
	}
	d.waitForBucket()
	r, err := d.post(body)
	d.noteBucket(r)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case r.status < 400:
		return nil
	case r.status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
	default:
		return discordStatusErr(r.status, r.snippet)
	}
```

In `Send` (`:159-202`), replace the loop head and the switch's field reads — the retry ladder's NUMBERS are untouched (3 attempts, 2 s / 5 s, the 30 s Retry-After ceiling, the 30 s cumulative-sleep budget):

```go
	var lastErr error
	var slept time.Duration
	for attempt := 1; ; attempt++ {
		// Pre-emptive: if the last response said the bucket was empty, wait
		// out its window instead of spending one of three attempts on the 429
		// Discord has already promised.
		d.waitForBucket()
		r, err := d.post(body)
		d.noteBucket(r)

		var delay time.Duration
		switch {
		case err != nil:
			lastErr = fmt.Errorf("discord webhook request: %w", err)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		case r.status < 400:
			return nil
		case r.status == http.StatusTooManyRequests:
			// Validate in FLOAT space before converting: values past ~9.2e9s
			// (or Inf) overflow time.Duration to a NEGATIVE on amd64, which
			// would slip past a Duration-space cap check and turn the sleep
			// into a zero-delay hammer. !(secs > 0) is deliberately NaN-proof.
			secs, parseErr := strconv.ParseFloat(r.retryAfter, 64)
			if parseErr != nil || !(secs > 0) || secs > discordRetryAfterCap.Seconds() {
				// Missing, malformed, or absurd Retry-After — surface the
				// 429 directly rather than guessing a sleep.
				return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			}
			lastErr = fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			delay = time.Duration(secs * float64(time.Second))
		case r.status >= 500:
			lastErr = discordStatusErr(r.status, r.snippet)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return discordStatusErr(r.status, r.snippet)
		}
```
The rest of the loop (the `attempt == discordMaxAttempts` bound, the `discordMaxSleepTotal` budget, `slept += delay`, `time.Sleep(delay)`) is unchanged.

Also update the stale wording in the `discordMaxSleepTotal` comment (`:29-33`), which still speaks of a semaphore slot:

```go
	// discordMaxSleepTotal caps CUMULATIVE inter-attempt sleep, bounding one
	// notification's worst-case hold on its target's queue at ~3x15s requests
	// + 30s sleep. Two large Retry-After waits would exceed it — a webhook
	// still rate-limited after one honored wait gives up instead.
```
and `discordRetryAfterCap`'s (`:25-28`):
```go
	// discordRetryAfterCap rejects absurd 429 Retry-After values — and bounds
	// the pre-emptive empty-bucket sleep — rather than parking a target's
	// whole queue into next week.
```

And the **third** stale `semaphore` reference, inside `Send`'s `if slept+delay > discordMaxSleepTotal` branch (`:194-199`) — the one Task 9 Step 5's grep would otherwise catch:

```
old:  // Retry-After) — bound the semaphore-slot hold instead of
new:  // Retry-After) — bound this target queue's hold instead of
```

- [ ] **Step 6: Declare the shutdown**

In `cmd/moombox/shutdown.go`, insert between the `stopService` closure (ends `:64`) and `// 1. Stop monitors` (`:66`):

```go
	// Single-attempt notifications from here on. The force-exit above fires
	// 10 s from now and routinely does (a worker stop can legitimately spend
	// the whole window draining a segment mux), so the three-attempt ladder
	// with its 2 s + 5 s backoff cannot finish — an embed emitted during the
	// stop would be retried into a process that is already gone. One attempt
	// is what fits; operations.md documents the cap rather than promising a
	// drain that cannot happen (owner ruling).
	//
	// Through stopService like every other step: it is the only thing in this
	// function with panic recovery, and a shutdown path that can panic its way
	// past the remaining teardown is exactly what that closure exists to
	// prevent — cheap here, since the call is one atomic store.
	stopService("Notifications (single-attempt mode)", s.notifyMgr.BeginShutdown)

```
Nothing else in the file moves: the 10 s timer, its two exit codes, and step 3's `stopService("Notifications", s.notifyMgr.Wait)` keep their exact shapes.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -race -count=3 ./internal/notifications/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./cmd/moombox/
```
Expected: `ok` for `internal/notifications`, a silent build, `ok` for `cmd/moombox`.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t5.msg <<'EOF'
feat(notifications): honour the Discord rate bucket, and declare shutdown

X-RateLimit-Remaining / X-RateLimit-Reset-After were never read, so Moombox
produced its own 429s and each one cost one of three delivery attempts. A
response reporting an empty bucket now arms a pre-emptive sleep (capped at
30 s) on that webhook's sender, which the per-target queue absorbs; a 429 is
excluded, because its Retry-After is already honoured and arming from the same
response would wait the window twice. The retry ladder's numbers are unchanged.

shutdown() now calls BeginShutdown() before stopping anything, so every embed
emitted during the stop makes one attempt instead of a ladder that cannot
finish inside the 10 s force-exit.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t5.msg -- \
  internal/notifications/discord.go \
  internal/notifications/ratelimit_test.go \
  cmd/moombox/shutdown.go
```

---
## Task 6: Three silent defects — `muxing` for multi-part jobs, `scheduled` when already live, the auth all-clear with nothing parked

**Files:**
- Modify: `internal/worker/orchestrator_mux.go:675-713` (hoist the "Muxing Starting" send into a helper above the branch) and `:1466` (`muxFromStaging`'s direct `finalizeMultiSegmentJob` arm — the third shape, which bypasses `muxAndFinalize` entirely)
- Modify: `internal/worker/stream_processor.go:574` (one condition)
- Modify: `cmd/moombox/monitor_callbacks.go:306-310` (a second body const), `:335` + `:378-397` (`wireCredentialRepairCallbacks` gains `wasNotified`), `:597-610` (`withAuthFailureCooldown` returns it), `:1046` + `:1098` (the wiring)
- Modify: `cmd/moombox/monitor_callbacks_recovery_test.go:246`, `:294` (two-value receive)
- Modify: `cmd/moombox/monitor_callbacks_twitch_reauth_test.go:74-101` (`repairCallbackState` gains the predicate, and a recorder variant joins it)
- Test: `internal/worker/orchestrator_mux_test.go` (append), `internal/worker/notifier_seam_test.go` (append), `cmd/moombox/monitor_callbacks_twitch_reauth_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `notificationtest.Recorder` and the `Sender`-typed fields.
- Produces:
  ```go
  // internal/worker
  func (o *DownloadOrchestrator) sendMuxingStarting(jobCtx *JobContext)
  // cmd/moombox
  func withAuthFailureCooldown(send authFailureNotifier) (authFailureNotifier, func(platform string) bool)
  func (s *runState) wireCredentialRepairCallbacks(broadcast func() int, clearMembershipMemo func() int, wasNotified func(platform string) bool)
  const authRecoveredNoParkedBody = "..."
  ```

**The three defects, from the audit.** (A7) `muxAndFinalize` returns into `finalizeMultiSegmentJob` at `orchestrator_mux.go:675`, **28 lines before** the "Muxing Starting" send at `:703` — so every quality-split and gap-split job silently skips a documented event, and a subscriber sees it for some jobs with no pattern they can detect. (C6) `stream_processor.go:574` fires "YouTube Start Time Confirmed" when `IsUpcoming || IsLive`, so a stream first observed **already live** gets a "Scheduled:" embed seconds before "YouTube Download Starting" — which itself carries a "Scheduled For" field. (A4) "Authentication Recovered" fires only when `resumed > 0` (`monitor_callbacks.go:378-379`), so a platform whose cookies died *between* recordings gets the loudest alert family Moombox sends — Error, 30-minute cooldown — and then, after the operator fixes it, **nothing**.

- [ ] **Step 1: Write the failing tests**

Append to `internal/worker/orchestrator_mux_test.go`:

```go
// TestMuxingStartingFiresForAMultiPartJob is A7. muxAndFinalize returns into
// finalizeMultiSegmentJob 28 lines before the "Muxing Starting" send, so every
// quality-split and gap-split job skipped the event operations.md documents as
// "FFmpeg mux step begins". A subscriber received it for single-file jobs and
// not for split ones, with no pattern visible from the outside.
//
// THE MUTANT: moving the send back below the `len(segments) > 0` branch. The
// multi-part subtest then records zero muxing notifications.
func TestMuxingStartingFiresForAMultiPartJob(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments int
	}{
		{"single-file finalize", 0},
		{"multi-part finalize", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := notificationtest.New()
			o, jobCtx := muxTestOrchestrator(t, rec, tc.segments)

			o.sendMuxingStarting(jobCtx)

			got := rec.ByEvent("muxing")
			if len(got) != 1 {
				t.Fatalf("recorded %d muxing notifications, want 1: %+v", len(got), rec.Calls())
			}
			if got[0].Title != "Muxing Starting" {
				t.Errorf("title = %q, want %q", got[0].Title, "Muxing Starting")
			}
		})
	}
}

// TestMuxAndFinalizeAnnouncesTheMuxBeforeChoosingAShape is the placement
// assertion the helper test above cannot make: sendMuxingStarting must be
// reached on the path that RETURNS into finalizeMultiSegmentJob, not only on
// the one that falls through to the single-file mux.
//
// Driving the whole of muxAndFinalize needs FFmpeg and staged media, so this
// asserts on the source instead: the send call must appear BEFORE the
// `finalizeMultiSegmentJob` return in the file. A source assertion is weak on
// its own — paired with the behavioural test above, it is what pins the one
// thing that broke.
func TestMuxAndFinalizeAnnouncesTheMuxBeforeChoosingAShape(t *testing.T) {
	src, err := os.ReadFile("orchestrator_mux.go")
	if err != nil {
		t.Fatalf("read orchestrator_mux.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *DownloadOrchestrator) muxAndFinalize(")
	if start < 0 {
		t.Fatal("muxAndFinalize not found")
	}
	body = body[start:]
	sendAt := strings.Index(body, "o.sendMuxingStarting(jobCtx)")
	branchAt := strings.Index(body, "return o.finalizeMultiSegmentJob(")
	if sendAt < 0 {
		t.Fatal("muxAndFinalize does not call sendMuxingStarting")
	}
	if branchAt < 0 {
		t.Fatal("the multi-segment branch is gone — re-read this test before changing it")
	}
	if sendAt > branchAt {
		t.Error("sendMuxingStarting is BELOW the multi-segment return — every split job skips the muxing event again (A7)")
	}
}

// TestMuxFromStagingAnnouncesTheMux is the THIRD shape. muxFromStaging is the
// off-queue mux verb (`A M` in the TUI, POST /api/jobs/{id}/mux); for a
// post-split job with no root media it calls finalizeMultiSegmentJob directly,
// bypassing muxAndFinalize — so fixing A7 in muxAndFinalize alone would still
// leave the manual mux silent. A manual mux is a mux, and the documented event
// is "FFmpeg mux step begins".
//
// The job is Twitch on purpose: finalizeMultiSegmentJob's Tier-4 part merge is
// YouTube-gated, and that is the one branch in this path that would reach the
// (nil) muxer. The finalize's own outcome is not asserted — only that the
// announcement went out before it.
//
// THE MUTANT: dropping the o.sendMuxingStarting(jobCtx) line above the direct
// finalizeMultiSegmentJob return.
func TestMuxFromStagingAnnouncesTheMux(t *testing.T) {
	rec := notificationtest.New()
	o, jobCtx := muxTestOrchestrator(t, rec, 2)
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"platform": "twitch"})
	jobCtx.Job.Platform = "twitch"
	jobCtx.StagingDir = t.TempDir() // no root media -> the direct-finalize arm
	jobCtx.OutputDir = t.TempDir()
	jobCtx.Filename = "a-job"

	_ = o.muxFromStaging(context.Background(), jobCtx)

	if got := len(rec.ByEvent("muxing")); got != 1 {
		t.Errorf("the off-queue mux recorded %d muxing notifications, want 1: %+v", got, rec.Calls())
	}
}
```

Add `"context"` to `orchestrator_mux_test.go`'s imports alongside MINOR-1's `database` and Task 3's additions.

Add a fixture helper to the same file:

```go
// muxTestOrchestrator builds an orchestrator over a real temp database with a
// recorder installed, plus a JobContext for a job carrying `segments` recorded
// parts.
func muxTestOrchestrator(t *testing.T, rec *notificationtest.Recorder, segments int) (*DownloadOrchestrator, *JobContext) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "mux.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	job := &database.Job{
		ID:       "yt_mux",
		VideoID:  "mux",
		Title:    "A Job",
		Platform: "youtube",
		Status:   database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i := range segments {
		// Segment's fields are SegmentIndex/Quality/Filename
		// (internal/database/types.go:198-216) — there is no PartNumber.
		if err := db.AddSegment(&database.Segment{
			JobID:        job.ID,
			SegmentIndex: i,
			Quality:      "1080p60",
			Filename:     fmt.Sprintf("part%d.mp4", i+1),
		}); err != nil {
			t.Fatalf("AddSegment: %v", err)
		}
	}
	stored, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	o := &DownloadOrchestrator{db: db, notifier: rec, logger: muxTestLogger{}}
	return o, &JobContext{Job: stored}
}

type muxTestLogger struct{}

func (muxTestLogger) Debug(string, ...any) {}
func (muxTestLogger) Info(string, ...any)  {}
func (muxTestLogger) Warn(string, ...any)  {}
func (muxTestLogger) Error(string, ...any) {}
```

> Before writing the fixture, read `internal/worker/orchestrator_mux_test.go`'s existing helpers and `internal/database`'s `AddSegment` signature (`internal/database/segments.go`) — if the file already provides an orchestrator fixture or a no-op logger, reuse it and delete the duplicate rather than adding a second one.

Append to `internal/worker/notifier_seam_test.go`:

```go
// TestScheduledDoesNotFireForAStreamAlreadyLive is C6. The condition was
// `IsUpcoming || IsLive`, so a stream first observed already live produced a
// "Scheduled: <title>" embed seconds before "YouTube Download Starting" —
// which carries the same start time in its own "Scheduled For" field. Two
// embeds for one moment, the first of them announcing a schedule for a stream
// that had already begun.
//
// THE MUTANT: restoring the ||. The "already live" subtest then records one.
func TestScheduledDoesNotFireForAStreamAlreadyLive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isUpcoming bool
		isLive     bool
		want       int
	}{
		{"upcoming", true, false, 1},
		{"already live on first sight", true, true, 0},
		{"live, not upcoming", false, true, 0},
		{"neither (a VOD)", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "c6.db"))
			if err != nil {
				t.Fatalf("database.Open: %v", err)
			}
			t.Cleanup(func() { db.Close() })

			rec := notificationtest.New()
			sp := &StreamProcessor{db: db, notifier: rec}
			job := &database.Job{ID: "yt_c6", VideoID: "c6", Status: database.StatusUpcoming}
			if _, err := db.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			stored, _ := db.GetJob("yt_c6")

			sp.updateJobMetadata(stored, &youtube.VideoInfo{
				Title:              "A Stream",
				ScheduledStartTime: "2026-10-01T12:00:00Z",
				IsUpcoming:         tc.isUpcoming,
				IsLive:             tc.isLive,
			}, false)

			if got := len(rec.ByEvent("scheduled")); got != tc.want {
				t.Errorf("recorded %d \"scheduled\" notifications, want %d: %+v", got, tc.want, rec.Calls())
			}
		})
	}
}
```

Append to `cmd/moombox/monitor_callbacks_twitch_reauth_test.go`:

```go
// TestAuthRecoveredFiresWithNothingParked is A4. The close was gated on
// resumed > 0, so a platform whose cookies died BETWEEN recordings got the
// loudest family Moombox sends — "Cookie Auto-Refresh Failed", Error, a
// 30-minute cooldown — and then, once the operator repaired it, silence. The
// failure that opened the incident had no close.
//
// THE MUTANT: restoring `if resumed > 0`. The first subtest records nothing.
func TestAuthRecoveredFiresWithNothingParked(t *testing.T) {
	t.Run("a notified failure gets its close even with no parked jobs", func(t *testing.T) {
		s, rec := repairCallbackStateWithNotice(t, func(string) bool { return true })
		s.cookieRefresh.OnAuthRecovered("youtube")

		got := rec.ByEvent("auth")
		if len(got) != 1 {
			t.Fatalf("recorded %d auth notifications, want 1: %+v", len(got), rec.Calls())
		}
		if got[0].Title != "Authentication Recovered" {
			t.Errorf("title = %q, want \"Authentication Recovered\"", got[0].Title)
		}
		if !strings.Contains(got[0].Description, "no jobs were parked") {
			t.Errorf("description = %q — with nothing resumed it must say so rather than claim 0 jobs were resumed", got[0].Description)
		}
	})

	t.Run("no prior failure means no close", func(t *testing.T) {
		s, rec := repairCallbackStateWithNotice(t, func(string) bool { return false })
		s.cookieRefresh.OnAuthRecovered("youtube")
		if got := rec.Calls(); len(got) != 0 {
			t.Errorf("recorded %d notifications for a recovery nobody was told about: %+v", len(got), got)
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestMuxing|TestMuxAndFinalize|TestScheduledDoesNot' ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestAuthRecoveredFires ./cmd/moombox/
```
Expected, in order: a build failure in `internal/worker` (`o.sendMuxingStarting undefined (type *DownloadOrchestrator has no field or method sendMuxingStarting)`); then, once that compiles, **two** failing subtests, because the old `IsUpcoming || IsLive` is true for both —
```
--- FAIL: TestScheduledDoesNotFireForAStreamAlreadyLive/already_live_on_first_sight
    notifier_seam_test.go:NN: recorded 1 "scheduled" notifications, want 0
--- FAIL: TestScheduledDoesNotFireForAStreamAlreadyLive/live,_not_upcoming
    notifier_seam_test.go:NN: recorded 1 "scheduled" notifications, want 0
```
and in `cmd/moombox` a build failure (`undefined: repairCallbackStateWithNotice`) followed, once the fixture exists, by `recorded 0 auth notifications, want 1`.

- [ ] **Step 3: Fix A7 — hoist the muxing announcement**

In `internal/worker/orchestrator_mux.go`, extract the send into a method placed just above `muxAndFinalize`:

```go
// sendMuxingStarting announces the mux for BOTH finalize shapes.
//
// It used to sit inline in muxAndFinalize, 28 lines BELOW the
// `len(segments) > 0` branch that returns into finalizeMultiSegmentJob — so
// every quality-split and gap-split job silently skipped the `muxing` event,
// which operations.md documents as "FFmpeg mux step begins". A subscriber got
// it for some jobs and not others with no pattern they could see. Called
// before the branch now; owner ruling keeps `muxing` as its own event rather
// than folding it into `finished`.
//
// The job is re-read rather than taken from the status write that follows,
// because the two finalize shapes write that status in different places and
// none of the fields below depends on it.
func (o *DownloadOrchestrator) sendMuxingStarting(jobCtx *JobContext) {
	if o.notifier == nil {
		return
	}
	job := jobCtx.Job
	if fresh, err := o.db.GetJob(jobCtx.Job.ID); err == nil && fresh != nil {
		job = fresh
	}

	fb := notifications.NewFieldBuilder()
	if job.LastVideoSeq != nil {
		fb.AddInline("Video Segments", fmt.Sprintf("%d", *job.LastVideoSeq))
	}
	if job.LastAudioSeq != nil {
		fb.AddInline("Audio Segments", fmt.Sprintf("%d", *job.LastAudioSeq))
	}
	if job.TotalChatMessages != nil {
		fb.AddInline("Chat Messages", fmt.Sprintf("%d", *job.TotalChatMessages))
	}
	if job.DownloadStartedAt != "" {
		if startTime, err := time.Parse(time.RFC3339, job.DownloadStartedAt); err == nil {
			fb.AddInline("Download Time", formatDurationHuman(time.Since(startTime)))
		}
	}
	o.notifier.Send("Muxing Starting",
		fmt.Sprintf("Download complete, muxing: %s", jobCtx.Job.Title),
		notifications.TypeMuxing,
		fb.Build(),
		notifications.SendOptions{
			URL:       jobCtx.Job.URL,
			Thumbnail: jobCtx.Job.ThumbnailURL,
			Event:     "muxing",
		},
	)
}
```

In `muxAndFinalize`, insert the call immediately **above** the segments branch (currently `:669-675`, right after `o.muxUnrecordedSegments(ctx, jobCtx)`):

```go
	// A7: announce the mux BEFORE the shape is chosen. The branch below
	// returns into finalizeMultiSegmentJob, which never reached the inline
	// send this replaces.
	o.sendMuxingStarting(jobCtx)

	// Multi-segment path: if the job has segments (from part splitting),
```

and delete the whole inline `if o.notifier != nil { ... }` block that followed the single-file status write — **`:685-713`**, from the `// Send "Muxing Starting" notification…` comment through its closing brace (`:684` is blank and `:714` starts the output-path resolution; both survive). Because `freshJob` was read only by that block, collapse the status write with it, leaving:

```go
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})
```

> Confirm with `grep -n freshJob internal/worker/orchestrator_mux.go` that `muxAndFinalize`'s range has no other reader before collapsing, and do **not** leave a `_ = freshJob` behind — `staticcheck` would not flag it and the next reader would wonder what it was for.

Then the **third** shape. `muxFromStaging` (`:1450`) is the off-queue `A M` / `/mux` verb, and for a post-split job with no root media it calls `finalizeMultiSegmentJob` **directly** at `:1466`, bypassing `muxAndFinalize` entirely — so that path would still announce nothing while its sibling `return o.muxAndFinalize(...)` at `:1471` now does. A manual mux is a mux, and the documented event is "FFmpeg mux step begins". Insert the call immediately above that return:

```go
		if segments, err := o.db.GetSegments(jobCtx.Job.ID); err == nil && len(segments) > 0 {
			// The off-queue mux verb on a post-split job: it reaches
			// finalizeMultiSegmentJob without passing through muxAndFinalize,
			// so it needs its own announcement. A manual mux is a mux.
			o.sendMuxingStarting(jobCtx)
			return o.finalizeMultiSegmentJob(ctx, jobCtx, segments)
		}
```

- [ ] **Step 4: Fix C6 — one condition**

`internal/worker/stream_processor.go:574`:

```go
	// IsUpcoming && !IsLive, not ||: a stream first observed ALREADY LIVE
	// produced a "Scheduled:" embed seconds before "YouTube Download
	// Starting", which carries the same time in its own "Scheduled For"
	// field. Two embeds for one moment, the first of them announcing a
	// schedule for a stream that had already begun.
	if notifyStartTimeConfirmed && info.IsUpcoming && !info.IsLive && sp.notifier != nil {
```

- [ ] **Step 5: Fix A4 — the cooldown exposes what it announced**

`cmd/moombox/monitor_callbacks.go`, add beside the two existing body constants (`:306-310`):

```go
	// The close when the repair found nothing to resume. Its own sentence
	// rather than the resumed one with a 0 in it: "Resumed 0 job(s)" reads as
	// a failure to resume, when what happened is that nothing needed it.
	authRecoveredNoParkedBody = "%s cookies are working again — no jobs were parked, so nothing needed resuming"
```

Change `withAuthFailureCooldown` (`:597-610`) to return the predicate as well:

```go
// ... (existing doc comment kept verbatim, plus:)
//
// It returns the wrapped notifier AND a wasNotified predicate. A4: the
// "Authentication Recovered" close used to fire only when the repair resumed a
// job, so a platform whose cookies died BETWEEN recordings got the failure
// family — the loudest thing Moombox sends — and no close at all. This map is
// the only record of whether the operator was ever told, so the close reads it.
func withAuthFailureCooldown(send authFailureNotifier) (authFailureNotifier, func(platform string) bool) {
	var mu sync.Mutex
	last := make(map[string]time.Time)
	notify := func(platform, title, desc string, ntype notifications.NotificationType) {
		mu.Lock()
		if time.Since(last[platform]) < 30*time.Minute {
			mu.Unlock()
			return
		}
		last[platform] = time.Now()
		mu.Unlock()
		send(platform, title, desc, ntype)
	}
	// A non-zero stamp means a failure was ANNOUNCED for this platform in this
	// process. It is deliberately never cleared: the close is the answer to
	// that announcement whenever it arrives, hours later included.
	wasNotified := func(platform string) bool {
		mu.Lock()
		defer mu.Unlock()
		return !last[platform].IsZero()
	}
	return notify, wasNotified
}
```

Change `wireCredentialRepairCallbacks`'s signature (`:335`) and its `OnAuthRecovered` send (`:378-397`):

```go
func (s *runState) wireCredentialRepairCallbacks(broadcast func() int, clearMembershipMemo func() int, wasNotified func(platform string) bool) {
```

```go
		resumed := resumeCookieParkedJobs(s.db, s.log, s.schedulerWake(), platform, "")
		if resumed > 0 {
			s.log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
		}
		// A4: the close fires whenever a failure was ANNOUNCED for this
		// platform, not only when a job happened to be parked. A platform
		// whose cookies died between recordings produced "Cookie Auto-Refresh
		// Failed" and then, after the operator fixed it, nothing.
		//
		// Still gated, not unconditional: OnAuthRecovered also fires on the
		// first successful validate of a perfectly healthy process, and a
		// "recovered" embed for a platform that was never reported broken is
		// noise that teaches an operator to ignore the family.
		announced := wasNotified != nil && wasNotified(platform)
		if resumed > 0 || announced {
			desc := fmt.Sprintf(authRecoveredResumedBody, resumed, platform)
			if resumed == 0 {
				desc = fmt.Sprintf(authRecoveredNoParkedBody, platform)
			}
			// Event "auth" pairs with the worker's "Authentication Required"
			// emit — an empty Event would bypass every target's allowlist
			// (unfilterable) since the filter only applies when Event != "".
			s.notifyMgr.Send("Authentication Recovered",
				desc,
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
```

Wire it at `:1046` and `:1098`:

```go
	notifyAuthFailure, authFailureAnnounced := withAuthFailureCooldown(func(platform, title, desc string, ntype notifications.NotificationType) {
```
```go
	s.wireCredentialRepairCallbacks(s.dlWorker.ReauthenticateTwitchChats, s.feedMon.ResetMembershipMemo, authFailureAnnounced)
```

`OnCredentialsChanged` (`:457`) is **unchanged** — it is not a close for an announced failure, it is a report that parked jobs were re-evaluated, and firing it with zero resumed would say nothing.

- [ ] **Step 6: Update the three existing call sites in tests**

`cmd/moombox/monitor_callbacks_recovery_test.go:246` and `:294`:
```go
	notify, _ := withAuthFailureCooldown(recoveryNotifier(sent))
```

`cmd/moombox/monitor_callbacks_twitch_reauth_test.go` — `repairCallbackState` (`:74-101`) gains the third argument, and a variant that installs a recorder joins it. Replace the helper's tail (the `s := &runState{...}` block onward) with:

```go
	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
	}
	calls := 0
	memoClears := 0
	s.wireCredentialRepairCallbacks(
		func() int { calls++; return 1 },
		func() int { memoClears++; return 2 },
		// No failure was ever announced in these fixtures, so the A4 close
		// stays shut and notifyMgr is never reached — which is what lets it
		// stay nil here.
		func(string) bool { return false },
	)
	return s, &calls, &memoClears
}

// repairCallbackStateWithNotice is repairCallbackState with a recorder
// installed as the notifier and a caller-chosen wasNotified, for the tests that
// assert on the embed rather than on the broadcast counters.
func repairCallbackStateWithNotice(t *testing.T, wasNotified func(string) bool) (*runState, *notificationtest.Recorder) {
	t.Helper()
	log, err := logger.New(filepath.Join(t.TempDir(), "repair.log"), "error", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)

	db, err := database.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rec := notificationtest.New()
	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
		notifyMgr:     rec,
	}
	s.wireCredentialRepairCallbacks(func() int { return 0 }, func() int { return 0 }, wasNotified)
	return s, rec
}
```
Add `"strings"` and `"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"` to that file's imports.

Also update the comment at `repairCallbackState`'s doc (`:71-74`), which currently explains that "the empty DB is load-bearing … so notifyMgr is never touched and may stay nil" — that is still true, but only because the fixture's `wasNotified` answers false. Append one sentence saying so.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./cmd/moombox/
```
Expected: two `ok` lines. The pre-existing `TestDeclinedRecoveryDoesNotSpendTheCooldown` and `TestAuthFailureCooldownStillSuppressesARepeat` must still pass — they pin the window the A4 predicate reads.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t6.msg <<'EOF'
fix(notifications): three silent defects at their producer sites

A7: "Muxing Starting" sat 28 lines below the branch that returns into
finalizeMultiSegmentJob, so every quality-split and gap-split job skipped the
documented `muxing` event. Hoisted above the branch as sendMuxingStarting, so
both finalize shapes announce the mux — and called again from muxFromStaging's
direct finalizeMultiSegmentJob arm, which the off-queue A M / /mux verb reaches
without passing through muxAndFinalize at all. A manual mux is a mux.

C6: "YouTube Start Time Confirmed" fired on IsUpcoming || IsLive, so a stream
first seen already live got a "Scheduled:" embed seconds before "Download
Starting" — which carries the same time in its own field. Now && !IsLive.

A4: "Authentication Recovered" fired only when a job was resumed, so a platform
whose cookies died between recordings got the Error-severity failure family and
no close at all. withAuthFailureCooldown now exposes whether it announced a
failure for that platform, and the close fires on that edge with its own
"no jobs were parked" wording.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t6.msg -- \
  internal/worker/orchestrator_mux.go \
  internal/worker/orchestrator_mux_test.go \
  internal/worker/stream_processor.go \
  internal/worker/notifier_seam_test.go \
  cmd/moombox/monitor_callbacks.go \
  cmd/moombox/monitor_callbacks_recovery_test.go \
  cmd/moombox/monitor_callbacks_twitch_reauth_test.go
```

---
## Task 7: `connectivity_pause` retired into the resume embed, and the Trim Failed label

**Files:**
- Modify: `internal/notifications/events.go:21-32` (`EventGroups`), `:34-45` (`KnownEvents`), `:47-55` (`eventAliases`)
- Modify: `internal/worker/orchestrator_twitch.go:790-801` (stamp, drop the send), `:900-904` (the resume embed), `:1122-1147` (the helper gains extra fields)
- Modify: `internal/worker/orchestrator.go:656` (`IDLabel`)
- Modify: `web/public/modules/settings.js:41` (delete the vocabulary entry)
- Test: `internal/notifications/events_test.go` (new), `internal/worker/notifier_seam_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `Sender`/`Recorder`, Task 4's `targetQueue.allows` (which is where the alias is read).
- Produces:
  ```go
  // internal/worker
  func (o *DownloadOrchestrator) sendTwitchSessionNotification(
      jobCtx *JobContext, title, desc string, ntype notifications.NotificationType,
      event string, quality QualityInfo, partNo int, extra ...notifications.Field)
  ```
  `notifications.EventGroups` loses `"connectivity_pause"`; `eventAliases` gains `"connectivity_resume": "connectivity_pause"`; `KnownEvents` gains every alias VALUE.

**Why (audit C8).** The pause embed is sent **while the machine is offline**. `discord.go` gives it three attempts inside ≈7 s of backoff plus per-attempt dial timeouts, so it delivers only if the outage is shorter than the send itself — the same fallacy the `connectivity_lost` removal already fixed (`events.go:23-28`). Then the resume embed and the global "Outage Alert" follow: three embeds for one outage, one of which usually never arrives. The §0 ruling: drop the send, stamp the pause instant where `offlineCancelled` is consumed, and let the resume embed carry it. The KEY is retired through the alias mechanism, so a config filtering `connectivity_pause` keeps receiving the resume for one release and stops being warned about at startup.

**How each UI strips it — confirmed, and one half is N2b's.** The **TUI** rebuilds a target's `Events` strictly from `allNotifEvents` on save (`internal/tui/settings_notifications.go:157-173`), which derives from `notifications.EventGroups` (`internal/tui/settings.go:270-283`) — so removing the key from the vocabulary makes the TUI both stop offering it and strip it on the next save, with **no TUI edit at all**. The **web** UI is only half there: deleting the entry from `NOTIFICATION_EVENT_GROUPS` (`web/public/modules/settings.js:41`) removes the chip and drops the key from `ALL_EVENT_IDS`, so the "Filter…" rebuild (`enableNotificationFilter`, `:1969-1981`) strips it — but `toggleNotificationEvent` (`:1940-1967`) splices the STORED array and `config_routes.go:750-768` stores any string, so a stale entry survives a chip toggle. Stripping unknown keys server-side in `validateConfigUpdates` is audit §4's fix and is explicitly **Arc N2b's** item 1. Record that in the commit message; do not pull it forward.

- [ ] **Step 1: Write the failing tests**

Create `internal/notifications/events_test.go`:

```go
package notifications

import "testing"

// TestRetiredConnectivityPauseIsAliasedNotForgotten is C8's migration
// contract, and all three halves matter.
//
// The key leaves the vocabulary, so neither UI offers it and the TUI strips it
// from a hand-edited config on the next save. It stays KNOWN, so an operator
// who has not opened the settings UI since upgrading is not warned at every
// startup about a key Moombox itself told them to use. And it aliases to
// connectivity_resume, so their filter keeps receiving the embed that now
// carries the pause — for one release, after which the alias entry goes.
//
// THE MUTANT: deleting the key without the alias. A target filtered to
// ["connectivity_pause"] then goes silent through every outage.
func TestRetiredConnectivityPauseIsAliasedNotForgotten(t *testing.T) {
	for _, g := range EventGroups {
		for _, e := range g.Events {
			if e == "connectivity_pause" {
				t.Errorf("connectivity_pause is still in EventGroups group %q — both UIs would go on offering a key nothing produces", g.Name)
			}
		}
	}
	if !KnownEvents["connectivity_pause"] {
		t.Error("connectivity_pause is not in KnownEvents — an unmigrated config warns at every startup about a key we told them to use")
	}
	if got := eventAliases["connectivity_resume"]; got != "connectivity_pause" {
		t.Errorf("eventAliases[connectivity_resume] = %q, want connectivity_pause", got)
	}
	// The pre-existing alias must survive the change.
	if got := eventAliases["disk_critical"]; got != "disk_warning" {
		t.Errorf("eventAliases[disk_critical] = %q, want disk_warning", got)
	}
}

// TestKnownEventsCoversEveryAliasValue is the rule rather than the instance: an
// alias target is by construction a key that used to exist, so a config naming
// it is a MIGRATED config, not a typo.
func TestKnownEventsCoversEveryAliasValue(t *testing.T) {
	for newKey, legacy := range eventAliases {
		if !KnownEvents[newKey] {
			t.Errorf("alias key %q is not a known event — it can never be produced", newKey)
		}
		if !KnownEvents[legacy] {
			t.Errorf("alias value %q is not in KnownEvents — a config listing it warns at startup although it still works", legacy)
		}
	}
}

// TestALegacyPauseFilterStillReceivesTheResume drives the whole path: a target
// configured before the retirement gets the folded embed.
func TestALegacyPauseFilterStillReceivesTheResume(t *testing.T) {
	rec := &recordingSender{}
	m := newTestManager(t, time.Second, notificationTarget{
		sender: rec,
		events: map[string]bool{"connectivity_pause": true},
		key:    "legacy",
	})
	m.Send("Twitch Download Resumed", "", TypeDownload, nil, SendOptions{Event: "connectivity_resume"})
	m.Send("Download Finished", "", TypeSuccess, nil, SendOptions{Event: "finished"})
	m.Wait()

	got := rec.titles()
	if len(got) != 1 || got[0] != "Twitch Download Resumed" {
		t.Fatalf("delivered %v, want only the resume embed — a legacy connectivity_pause filter must keep receiving it", got)
	}
}
```
(add `"time"` to the file's imports).

Append to `internal/worker/notifier_seam_test.go`:

```go
// TestTwitchResumeEmbedCarriesThePause is C8's payload half. The pause embed
// was sent WHILE THE MACHINE WAS OFFLINE — three attempts inside ~7s of backoff
// plus dial timeouts, so it arrived only when the outage was shorter than the
// send — and the resume that followed said nothing about how long the download
// had been down. One embed now carries both.
//
// THE MUTANT: restoring the pause send (the pause subtest records two), or
// dropping the Paused field (the resume subtest's field lookup fails).
func TestTwitchResumeEmbedCarriesThePause(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{notifier: rec, logger: muxTestLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "tw_1", Title: "A Stream", ChannelName: "chan", Platform: "twitch"}}
	pausedAt := time.Now().Add(-4 * time.Minute)

	o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
		"Connectivity restored, resuming download: "+jobCtx.Job.Title,
		notifications.TypeDownload, "connectivity_resume", QualityInfo{Label: "1080p60"}, 2,
		twitchOutageField(pausedAt))

	if got := len(rec.ByEvent("connectivity_pause")); got != 0 {
		t.Errorf("recorded %d connectivity_pause notifications — the undeliverable pause embed is retired", got)
	}
	got := rec.ByEvent("connectivity_resume")
	if len(got) != 1 {
		t.Fatalf("recorded %d resume notifications, want 1", len(got))
	}
	paused, ok := got[0].Field("Paused")
	if !ok {
		t.Fatalf("the resume embed carries no Paused field: %+v", got[0].Fields)
	}
	if !strings.Contains(paused, fmt.Sprintf("<t:%d:R>", pausedAt.Unix())) {
		t.Errorf("Paused = %q, want a <t:%d:R> relative timestamp", paused, pausedAt.Unix())
	}
	if !strings.Contains(paused, "resumed after") {
		t.Errorf("Paused = %q, want the outage duration", paused)
	}
	if part, _ := got[0].Field("Part"); part != "2" {
		t.Errorf("Part = %q, want 2", part)
	}
}
```

Add `fmt`, `strings` and `time` to `notifier_seam_test.go`'s import block — this append is the file's first use of all three (`time.Now`, `strings.Contains`, `fmt.Sprintf`), and Task 6's append to the same file added none of them.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestRetiredConnectivity|TestKnownEventsCovers|TestALegacyPause' ./internal/notifications/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestTwitchResumeEmbed ./internal/worker/
```
Expected:
```
--- FAIL: TestRetiredConnectivityPauseIsAliasedNotForgotten (0.00s)
    events_test.go:NN: connectivity_pause is still in EventGroups group "Connectivity" — both UIs would go on offering a key nothing produces
    events_test.go:NN: eventAliases[connectivity_resume] = "", want connectivity_pause
--- FAIL: TestALegacyPauseFilterStillReceivesTheResume (0.00s)
    events_test.go:NN: delivered [], want only the resume embed — a legacy connectivity_pause filter must keep receiving it
FAIL
```
and a build failure in `internal/worker`: `undefined: twitchOutageField`, plus `too many arguments in call to o.sendTwitchSessionNotification`.

- [ ] **Step 3: Retire the key in the vocabulary**

`internal/notifications/events.go` — the Connectivity group (`:23-29`):

```go
	// connectivity_restored fires the post-restore "Outage Alert" (start/
	// end/duration). There is deliberately NO connectivity_lost event: a
	// lost-connectivity webhook has no connectivity to deliver over, so the
	// restore-time alert carries the whole story. (The key was removed from
	// this vocabulary in v2.8; stale configs listing it warn at startup and
	// the UIs strip it on their next save — the designed cleanup path.)
	//
	// connectivity_pause was retired for the SAME reason and by the same
	// reading, one release later: it was sent while the machine was offline,
	// so the sender's three attempts and ~7 s of backoff delivered it only
	// when the outage was shorter than the send. connectivity_resume now
	// carries the pause instant and the outage duration. Unlike
	// connectivity_lost, the retired key is ALIASED (below) rather than simply
	// dropped, so a filter naming it keeps receiving the folded embed for one
	// release instead of going silent through every outage.
	{"Connectivity", []string{"connectivity_resume", "connectivity_split", "connectivity_restored"}},
```

`KnownEvents` (`:37-45`):

```go
// KnownEvents is the flat membership set derived from EventGroups, PLUS every
// retired key an alias still maps onto.
//
// NewManager warns when a configured Events filter names an event outside this
// set (a typo would otherwise silently filter forever) — but a retired key in a
// config is not a typo, it is a filter Moombox itself told the operator to
// write, and it still works through the alias. Warning about it at every
// startup until they happen to re-save their settings would be noise about our
// own migration.
var KnownEvents = func() map[string]bool {
	m := make(map[string]bool)
	for _, g := range EventGroups {
		for _, e := range g.Events {
			m[e] = true
		}
	}
	for _, legacy := range eventAliases {
		m[legacy] = true
	}
	return m
}()
```
(Package-level initialisation order is by dependency, so `eventAliases` is built before `KnownEvents` reads it.)

`eventAliases` (`:53-55`):

```go
var eventAliases = map[string]string{
	"disk_critical": "disk_warning",
	// C8: the pause embed was undeliverable by construction (it was sent
	// during the outage it reported), so the resume embed now carries the
	// pause instant and the duration. A target that filtered on the pause key
	// keeps receiving that folded embed. DELETE THIS ENTRY one release after
	// the retirement ships — it is a migration, not a permanent mapping.
	"connectivity_resume": "connectivity_pause",
}
```

- [ ] **Step 4: Stamp the pause and fold it into the resume**

`internal/worker/orchestrator_twitch.go` — replace **`:794-800`**, from `offlineCancelled.Store(false)` through the closing paren of the pause send. Stop at `:800`: `:801` opens the `// Progress line for the pause:` comment and its `tracker.SetWaitActivity(engine.ActivityReconnecting)` call, both of which must survive — the tracker line is the only thing writing the job card during the outage.

```go
		offlineCancelled.Store(false)
		// The pause instant, stamped where the flag is consumed. NOT sent:
		// a "download paused, connectivity lost" embed has no connectivity to
		// travel over, so its three attempts and ~7 s of backoff delivered it
		// only when the outage was shorter than the send itself (audit C8 —
		// the same fallacy the connectivity_lost removal fixed). The resume
		// embed below carries it, and an outage that never resumes is covered
		// by connectivity_split.
		pausedAt := time.Now()

		o.logger.Warn("Twitch download paused — connectivity lost; waiting to resume same broadcast",
			"part", segmentIndex+1, "jobID", jobCtx.Job.ID)
```

Replace the resume send (`:902-904`):

```go
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
			fmt.Sprintf("Connectivity restored, resuming download: %s", jobCtx.Job.Title),
			notifications.TypeDownload, "connectivity_resume", currentQuality, segmentIndex+1,
			twitchOutageField(pausedAt))
```

Add the field builder and widen the helper (`:1122-1147`):

```go
// twitchOutageField renders the outage an embed is reporting the end of: when
// it began, as a Discord relative timestamp that keeps counting in the client,
// and how long it lasted. This is what the retired connectivity_pause embed
// used to carry, folded into the one embed that can actually be delivered.
func twitchOutageField(pausedAt time.Time) notifications.Field {
	return notifications.Field{
		Name: "Paused",
		Value: fmt.Sprintf("<t:%d:R> · resumed after %s",
			pausedAt.Unix(), formatDurationHuman(time.Since(pausedAt))),
		Inline: true,
	}
}

// sendTwitchSessionNotification delivers a session-lifecycle notification
// (outage resume, finalize-after-outage) with the shared channel/quality/part
// field shape, plus any extra fields the caller adds. No-op when the notifier
// is nil.
func (o *DownloadOrchestrator) sendTwitchSessionNotification(
	jobCtx *JobContext,
	title, desc string,
	ntype notifications.NotificationType,
	event string,
	quality QualityInfo,
	partNo int,
	extra ...notifications.Field,
) {
	if o.notifier == nil {
		return
	}
	fields := []notifications.Field{
		{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
		{Name: "Quality", Value: quality.Label, Inline: true},
		{Name: "Part", Value: fmt.Sprintf("%d", partNo), Inline: true},
	}
	fields = append(fields, extra...)
	o.notifier.Send(title, desc, ntype, fields,
		notifications.SendOptions{
			URL:       jobCtx.Job.URL,
			Thumbnail: jobCtx.Job.ThumbnailURL,
			Event:     event,
		},
	)
}
```
The `connectivity_split` call at `:979` passes no `extra` and is unchanged.

> `pausedAt`'s scope is not a question: `:794` and `:902` sit at the same brace depth in the `sessionLoop` body, and `recoverLoop` opens at `:813` and closes at `:860`, entirely between them. The declaration at `:794` is visible at `:902` as written.

- [ ] **Step 5: The Trim Failed label**

`internal/worker/orchestrator.go:656` — the hardcoded "Video ID" is wrong for a Twitch job, which carries a stream ID; `notifications.IDLabel` is the shared answer every other site already uses (`worker.go:1309`, `:1331`, `routes/jobs.go:965`):

```go
								{Name: notifications.IDLabel(jobCtx.Job.Platform), Value: jobCtx.Job.VideoID, Inline: true},
```

- [ ] **Step 6: Drop the key from the web vocabulary mirror**

`web/public/modules/settings.js:41` — delete the line

```js
      { id: "connectivity_pause", label: "Download Paused (Offline)" },
```

leaving the Connectivity group with its three remaining entries. Nothing else in the file references the id (`grep -n connectivity_pause web/public/`), `ALL_EVENT_IDS` derives from the groups, and no `web/tests` suite asserts on the vocabulary — so the node suite's counts are unchanged and `web/tests/README.md` needs no recount.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -race -count=3 ./internal/notifications/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./internal/tui/ && \
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```
Expected: three `ok` lines and a silent build. `internal/tui` is in the list because its filter editor derives from `EventGroups` and `internal/tui/settings_notif_click_test.go` indexes into `notifEventGroups` — a vocabulary change is exactly what could shift it.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t7.msg <<'EOF'
fix(notifications): retire connectivity_pause into the resume embed

The pause embed was sent WHILE THE MACHINE WAS OFFLINE: three attempts inside
~7 s of backoff plus dial timeouts, so it arrived only when the outage was
shorter than the send — the same fallacy the connectivity_lost removal fixed.
Then the resume embed and the global Outage Alert followed: three embeds for
one outage, one of which usually never landed.

The send is gone; the pause instant is stamped where offlineCancelled is
consumed and the resume embed carries "Paused <t:x:R> · resumed after {dur}".
The key leaves EventGroups (both UIs stop offering it; the TUI strips it on
save because it rebuilds from the vocabulary) but stays in KnownEvents through
a new alias connectivity_resume -> connectivity_pause, so an unmigrated filter
keeps receiving the folded embed and stops being warned about at startup.
Delete the alias one release after this ships.

Stripping unknown keys on a WEB save is audit §4's other half and stays with
Arc N2b: toggleNotificationEvent splices the stored array, so a stale entry
survives a chip toggle until validateConfigUpdates strips it server-side.

Also: "Trim Failed" hardcoded "Video ID" for Twitch jobs; it uses IDLabel now.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t7.msg -- \
  internal/notifications/events.go \
  internal/notifications/events_test.go \
  internal/worker/orchestrator_twitch.go \
  internal/worker/orchestrator.go \
  internal/worker/notifier_seam_test.go \
  web/public/modules/settings.js
```

---
## Task 8: Docs truth

**Files:**
- Modify: `docs/spec/operations.md:392` (shutdown step 3), `:461-488` (the event table; rows start at `:463`), `:517-527` (Dispatch Behavior)
- Modify: `README.md:67`, `:673-683`
- Modify: `SPEC.md:875`, `:885`, `:887`, `:889`
- Test: `go test ./internal/docs/` (the citation gate — no new test file)

**Interfaces:** consumes every earlier task's declared symbols; produces nothing code reads.

**What the audit found overstated** (§4, and R4): `muxing` says "FFmpeg mux step begins" but did not fire for multi-part jobs; `scheduled` says "Upcoming stream detected" but also fired when first seen live; `connectivity_pause` documents an embed that is undeliverable by construction; "Graceful shutdown: blocks until all in-flight notifications complete" is contradicted by the 10 s force-exit; `README.md:67` advertises "any webhook-compatible service (Discord, Slack, ntfy, etc.)" when `parseTarget` is Discord-only **by design**; `README.md:682` lists a `live` event that does not exist (it would warn "unknown event" at startup) and omits 16 real ones; `SPEC.md:885` lists 20 keys and misses six. Two more that this arc itself made stale and that a task named "Docs truth" must not leave behind: `SPEC.md:875`'s "dispatches asynchronously via `sync.WaitGroup`" and `SPEC.md:887`'s embed-format sentence.

- [ ] **Step 1: `docs/spec/operations.md` — shutdown step 3**

Replace `:392`:

```markdown
3. **Flush notifications** — `notifyMgr.BeginShutdown()` ran before step 1, so every embed emitted during the stop is a SINGLE attempt; `notifyMgr.Wait()` then closes each target's queue and drains what is already in it. The real bound is the process's own 10-second force-exit, not `Wait`'s 30-second timeout: step 2 can legitimately spend the whole window draining a segment mux, so the drain gets 0–10 s. An embed emitted during a shutdown with a slow Discord is lost, by design (owner ruling) — extending the force-exit would trade a hung shutdown for one embed.
```

- [ ] **Step 2: `docs/spec/operations.md` — the event table**

In the table at `:461-488` (the header is `:461-462`, the rows `:463-488`):

- `muxing` row → `| `muxing` | FFmpeg mux step begins — for every mux, including a manual one (`A M` / `POST /api/jobs/{id}/mux`), and for both finalize shapes, single-file and multi-part (quality/gap-split). Until v2.9 the multi-part path returned before the send and silently skipped it |`
- `scheduled` row → `| `scheduled` | An UPCOMING stream's scheduled start time was confirmed (`IsUpcoming && !IsLive`). A stream first observed already live does not fire it — "Download Starting" carries the same time in its own "Scheduled For" field |`
- **Delete** the `connectivity_pause` row (`:475`).
- `connectivity_resume` row → `| `connectivity_resume` | Connectivity restored; the same Twitch job resumed. Carries the pause instant as a relative timestamp and the outage duration ("Paused `<t:x:R>` · resumed after 4m12s"), which is what the retired `connectivity_pause` event used to say on its own. That embed was sent WHILE the machine was offline and so mostly never arrived; a target still filtering on the old key receives this one through the manager's event alias, for one release |`

Then, after the table's trailing paragraph about `notifications.EventGroups` (`:490-497`), append:

```markdown
`connectivity_pause` is **retired**. It is absent from `EventGroups`, so
neither UI offers it and the TUI strips it from a hand-edited config on the
next save; it remains in `KnownEvents` (so no startup warning) and is the
target of `eventAliases`' `connectivity_resume` entry (so an unmigrated filter
keeps receiving the folded embed). The alias is a migration and is removed one
release after v2.9.
```

- [ ] **Step 3: `docs/spec/operations.md` — Dispatch Behavior**

Replace the whole bullet list at `:519-527`:

```markdown
- **Per-target FIFO queues.** One bounded queue (256 entries, `notificationQueueCap`) and one draining goroutine per target, created by `applyTargets` (`internal/notifications/manager.go`). `Send` snapshots the target list under an RWMutex, applies each filter, and appends — it never blocks the caller and never spawns. Because one goroutine drains a target, a job's embeds can never reorder, and a burst can never put several concurrent POSTs into one webhook's rate bucket.
- **Drop policy.** On a full queue the OLDEST low-tier entry goes — `found`, `added`, `scheduled`, `rescheduled` (`Tier`, `internal/notifications/manager.go`) — with a Warn naming the event and title. With nothing low-tier queued, the ARRIVAL is dropped instead, so an older alert is never displaced by a newer one. Alerts (`error`, `auth`, everything in System) are never the victim.
- **Panic recovery:** each queue's goroutine carries a top-level `recover`, and each delivery carries its own — a panic in one send cannot strand every later notification for that target.
- **Event filtering:** if a target has an event filter list, only matching events are sent. Targets with no filter receive everything. An event that split from a broader legacy name also matches targets allowlisting the old name (`eventAliases`, `internal/notifications/events.go`).
- **Timeout:** Discord webhook HTTP requests have a 15-second timeout per attempt.
- **Retry:** bounded delivery loop, max 3 attempts total — transport errors and Discord 5xx back off 2s/5s; 429 honors a validated `Retry-After` (≤30s); other 4xx are permanent. Cumulative sleep is capped at 30s, bounding one notification's hold on its target's queue at ~75s worst case.
- **Rate bucket:** `X-RateLimit-Remaining` and `X-RateLimit-Reset-After` are read from every non-429 response. A remaining count of 0 arms a pre-emptive sleep (capped at 30s) on that webhook's sender, so the next embed waits out the window instead of spending one of its three attempts on a 429 Discord has already promised. Per Discord's rate-limit docs the bucket is discoverable only from these headers — there is no published numeric cap.
- **Embed limits:** every embed is clamped on rune boundaries inside `buildPayload` before it is sent — title 256, description 4096, field name 256, field value 1024, footer 2048, author name 256, at most 25 fields and 6000 characters in total, with a `…` marker. Over any one of them is a permanent 400, so an unclamped embed was a silently dropped alert. Producers use `ClampRunes` (`internal/notifications/limits.go`) for their own excerpts and `EscapeMarkdown` (same file) for job-supplied text.
- **Hot-reload:** notification config edits apply immediately — the web config route fires `OnNotificationsChange` → `Manager.Reload`, and the TUI save path calls `Reload` directly. The diff is on the resolved webhook URL: a target that is still configured keeps its goroutine, its queued backlog and its learned rate bucket; a removed one finishes its in-flight delivery and exits, discarding the rest with one Warn naming the count. No restart required.
- **Save-time validation:** webhook URLs are validated at save (web `validateConfigUpdates` + TUI editor) via `notifications.ValidateURL`; `POST /api/notifications/test {url}` sends a single-attempt test embed (used by the web Test buttons and the TUI `T` action, including for unsaved URLs). Both `discord.com` and the legacy `discordapp.com` host are accepted; the latter is canonicalised, so the two spellings of one webhook collapse to one target.
- **Graceful shutdown:** `BeginShutdown` switches every target to single-attempt delivery, then `Wait` drains the queues — see the Shutdown Sequence above for the 10-second cap that actually bounds it.
- **Embed format:** Discord rich embeds with title, description, color (by notification type), optional fields, an author line (channel name, avatar, channel page), thumbnail, image, footer (`Moombox · {platform} · {job id}`, or just `Moombox`), and an ISO 8601 timestamp. A mention, when a target is configured for one, rides the message `content` with a matching `allowed_mentions` — embeds never mention on their own.
```

> The mention bullet describes a shape N1 builds and N2b fills. Keep the wording conditional ("when a target is configured for one") so it is not a promise of a config key that does not exist yet.

- [ ] **Step 4: `README.md`**

`:67` — Discord only:
```markdown
- **Discord webhook notifications** — Rich embeds for every stream and system event, with a per-target event filter
```

`:673-683` — the section body:
```markdown
Send Discord webhook notifications for stream and system events:

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN"
events = ["found", "finished", "error"]  # Optional filter (default: all events)
```

Discord webhooks only — the shorthand `discord://ID/TOKEN` works too. The full
list of event keys, and what each one fires on, is the event table in
[docs/spec/operations.md](docs/spec/operations.md#notifications-discord-webhooks);
both settings UIs offer the same list as checkboxes, so you rarely need to
write one by hand.
```

- [ ] **Step 5: `SPEC.md`**

`:875` — the async claim is no longer true:
```markdown
Discord webhooks with queued dispatch. The `NotificationManager` validates webhook URLs, formats Discord embeds with color-coded types (Info=blue, Success=green, Warning=yellow, Error=red, Download=teal, Muxing=purple, Cancelled=orange), and hands each one to a per-target FIFO queue drained by a single goroutine. Supports event-based filtering per webhook target — see the event list below and the table in `docs/spec/operations.md`.
```

`:885` — every key, `connectivity_pause` gone and the six missing ones added:
```markdown
**Notification events:** `found`, `added`, `scheduled`, `rescheduled`, `downloading`, `quality_split`, `gap_split`, `muxing`, `finished`, `error`, `cancelled`, `auth`, `connectivity_resume`, `connectivity_split`, `connectivity_restored`, `trim_created`, `trim_deleted`, `trim_error`, `disk_warning`, `disk_critical`, `update_available`, `update_applied`, `update_failed`, `crash_recovered`, `channel_unhealthy` — see `docs/spec/operations.md` for the per-event table; new events must be registered in both UI filter registries. `connectivity_pause` was retired in v2.9 and is kept working as a legacy filter entry by an event alias.
```

`:887` — the embed-format sentence, which Task 2 left stale (it names neither the author line, the footer, the full-width image, nor the message-level mention):
```markdown
**Discord embed format:** Title, description, colored sidebar (type-specific), fields (inline key-value pairs), optional URL link, an author line (channel name + avatar + channel page), an optional thumbnail and full-width image, and a footer (`Moombox · {platform} · {job id}`). When a target is configured for one, the MESSAGE — not the embed — also carries a mention in `content` with a matching `allowed_mentions`; embeds never mention on their own. Every string is clamped to Discord's limits on a rune boundary before it is sent.
```

`:889` — the dispatch paragraph:
```markdown
Dispatch is queued, not fire-and-forget: `Manager.Send()` returns immediately after appending to each matching target's bounded FIFO, and one goroutine per target delivers in order. `BeginShutdown()` switches to single-attempt delivery and `Wait()` drains the queues, both called during shutdown; the process's 10-second force-exit is what actually bounds the drain.
```

- [ ] **Step 6: The `moombox-settings` skill**

Read `.claude/skills/moombox-settings/SKILL.md`. Its only notification mention is `:58` — "`OnNotificationsChange()` → `notifyMgr.Reload()` — the notification targets follow the save" — which stays true (`Reload` survives, unchanged in name and role) and lists no notification config FIELD. **No edit.** Record the check in the commit message so the next reader does not redo it.

- [ ] **Step 7: Run the citation gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```
Expected: `ok  github.com/vampiricwulf/Moombox/internal/docs`.

If it names a symbol/path pair, the rule is: a backticked symbol immediately followed by a backticked repo path must be **declared** in that file. `notificationQueueCap`, `targetQueue` and `Tier` are declared in `internal/notifications/queue.go` and `manager.go` respectively — check which, and cite the declaring file, not a caller.

- [ ] **Step 8: Commit**

```bash
cat > .superpowers/gotmp/n1-t8.msg <<'EOF'
docs: make the notifications contract true again

operations.md: the delivery model is per-target FIFO queues with a drop policy
and a rate bucket, not a goroutine per send; the shutdown drain is bounded by
the process's 10 s force-exit and sends single-attempt, which is what step 3
now says instead of "blocks until all in-flight notifications complete"; the
muxing and scheduled rows no longer overstate what fires; connectivity_pause is
documented as retired and aliased; the embed format carries the author line,
the new footer and the mention shape.

README: Discord only (parseTarget is Discord-by-design, not a TODO), and the
event list — which named a `live` event that does not exist and omitted 16 real
ones — is replaced by a pointer to the operations table.

SPEC.md: all 25 live keys, the dispatch paragraph describes the queue, and the
embed-format sentence names the author line, the footer and the message-level
mention this arc added.

The moombox-settings skill was checked and needs no edit: its only notification
line is OnNotificationsChange -> notifyMgr.Reload, which is still true.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t8.msg -- \
  docs/spec/operations.md \
  README.md \
  SPEC.md
```

---

## Task 9: Gates, and the plan deletes itself

**Files:**
- Delete: `docs/superpowers/plans/2026-09-27-webhooks-n1-delivery-core.md`

**Interfaces:** none — this task writes no code.

This is the arc's verification pass. It runs the gates CI runs, scoped to the packages N1 touched, and nothing else. **Do not run `go test ./...`.**

- [ ] **Step 1: Formatting and static analysis**

```bash
cd /d/Git/Moombox
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy -diff
```
Expected: `gofmt -l` prints **nothing**; `go vet` prints nothing; `go mod tidy -diff` prints nothing.

If `staticcheck` is not on PATH: `go install honnef.co/go/tools/cmd/staticcheck@2026.2.1`, then
```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./...
```
Expected: no findings. Watch for `U1000` on anything left unused after Task 4's deletions (the old `semaphore` field, `maxInflightNotifications`) — if one is reported, the deletion was incomplete, not the check wrong.

- [ ] **Step 2: The three builds**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
```
Expected: three silent successes.

- [ ] **Step 3: The package tests**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -race -count=3 ./internal/notifications/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./cmd/moombox/ ./internal/web/routes/ ./internal/tui/ ./internal/docs/
```
Expected: `ok` for each. `-race -count=3` on `internal/notifications` is the arc's own gate — the FIFO ordering assertion and the queue's concurrency are what this arc introduced, and a single un-raced run is not evidence for either.

- [ ] **Step 4: The node suite**

Task 7 deleted one line from `web/public/modules/settings.js` and added no test, so the suite's counts are unchanged and `web/tests/README.md` needs **no** recount. Confirm rather than assume:

```bash
cd /d/Git/Moombox/web/tests
node --test ./*.test.mjs 2>&1 | tail -12
```
Expected: `tests 291` / `pass 291` / `fail 0` with jsdom installed (or 291 with 160 skipped without it) — the same numbers `web/tests/README.md:66-72` records. If the total moved, something in this arc touched the frontend beyond the one deleted vocabulary line: find it before editing the README.

- [ ] **Step 5: Confirm the arc's own shape**

```bash
cd /d/Git/Moombox
git log --oneline main..HEAD
grep -rn "maxInflightNotifications\|semaphore" internal/notifications/
grep -rn "connectivity_pause" internal/ web/public/ --include=*.go --include=*.js
```
Expected: eight commits (Tasks 1–8), each with both trailers; **no** hits for the semaphore (the const and its "30 req/min" comment go in Task 4, and all three of `discord.go`'s "semaphore-slot" comments are rewritten in Task 5 — a surviving hit means one of those comment edits was skipped, not that the deletion was incomplete); and `connectivity_pause` in exactly **three** files — `internal/notifications/events.go` (the alias entry and its two comments), `internal/notifications/events_test.go`, and `internal/worker/notifier_seam_test.go` (Task 7's "no pause embed" assertion, which must name the retired key to assert its absence). A hit anywhere else is a producer that was missed.

- [ ] **Step 6: Delete this plan**

```bash
cd /d/Git/Moombox
git rm docs/superpowers/plans/2026-09-27-webhooks-n1-delivery-core.md
cat > .superpowers/gotmp/n1-t9.msg <<'EOF'
chore: remove the Arc N1 plan, implemented

Gates green: gofmt, go vet, staticcheck, go mod tidy -diff, three builds,
go test -race -count=3 ./internal/notifications/, and the worker / cmd /
routes / tui / docs packages. Node suite unchanged (291) — Task 7's one-line
settings.js edit added no test.

The design lives on in docs/superpowers/specs/2026-09-27-discord-webhooks-design.md;
git history is the archive for the plan.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
git commit -F .superpowers/gotmp/n1-t9.msg -- docs/superpowers/plans/2026-09-27-webhooks-n1-delivery-core.md
```

---

## Spec coverage

Every clause of the spec's §1, and where it lands.

| Spec §1 clause | Task |
|---|---|
| 1. `Sender` interface; `*Manager` satisfies it; nil `*Manager` Send is a no-op | 1 |
| 1. Every consumer field/parameter becomes the interface (7 files, 9 declarations) | 1 |
| 1. `internal/notifications/notificationtest` with `Recorder`, `Call`, `Calls`/`ByEvent`/`Reset`, concurrency-safe | 1 |
| 2. `SendOptions` gains `Author`, `Platform`, `JobID`, `Tier`, `Mention`, `MentionAllowed` (the exported `*AllowedMentions` object, resolved by `MentionParse` — controller pin; N2b's plan is written against it) | 2 |
| 2. Footer "Moombox · {platform} · {job id}" / "Moombox" | 2 |
| 2. `TierLow` for found/added/scheduled/rescheduled, derived from `Event` when unset | 2 |
| 2. Payload shape: `author`, `content` + `allowed_mentions` | 2 |
| 2. `Image` removed from Twitch finished sends | 2 |
| 3. Rune clamps to every Discord limit, drop-fields-then-description-never-title, "…" marker | 3 |
| 3. `EscapeMarkdown` exported, with the never-escape rule documented | 3 |
| 3. The byte cut at `orchestrator_mux.go:1273` becomes a rune cut through the same helper | 3 |
| 3. Accept `discordapp.com` in `parseTarget` | 3 |
| 3. The two stale comments (`manager.go:133`, `:141`) | 2 (`:133`) / 4 (`:141`, deleted with the constant) |
| 4. Per-target FIFO, 256 entries, in-order, retries in place | 4 |
| 4. Overflow drops the oldest `TierLow`, else the newest, with a Warn naming event + title | 4 |
| 4. `Reload` starts new goroutines; a removed target finishes in-flight then discards with one Warn naming the count | 4 |
| 4. `Wait` drains to the budget; `BeginShutdown` makes sends single-attempt | 1 declares it empty (so `Notifier` compiles), 4 gives it the body, 5 wires the call |
| 4. The 16-slot semaphore is deleted | 4 |
| 4. Rate headers read, pre-emptive sleep; 429 `Retry-After` ≤ 30 s; 2 s / 5 s × 3; other 4xx permanent | 5 |
| ↳ *Deliberate divergence:* the spec says an out-of-range 429 is "dropped with a **Warn**". The item IS dropped, but `targetQueue.deliver` logs every delivery failure at `Error` — one line for all of them. Splitting it would mean string-matching `DiscordWebhook.Send`'s error text to tell a rate limit from a revoked webhook, which is a worse mechanism than a slightly loud log line. Recorded here rather than silently done differently. | 4/5 |
| 5a. `muxing` fires for multi-part jobs (the send moves above the branch) — and for the third shape the spec does not name, `muxFromStaging`'s direct `finalizeMultiSegmentJob` arm at `orchestrator_mux.go:1466`, which the off-queue `A M` / `/mux` verb reaches without passing through `muxAndFinalize` | 6 |
| 5b. `scheduled` only for `IsUpcoming && !IsLive` | 6 |
| 5c. "Authentication Recovered" on the edge whenever a failure was notified; "no jobs were parked" wording | 6 |
| 5d. `connectivity_pause` send removed, pause stamped, resume embed carries it, key aliased, `KnownEvents` includes alias values, UIs strip | 7 |
| 5. `orchestrator.go:656` uses `IDLabel` | 7 |
| 6. Package tests: FIFO under a retry, overflow, pre-emptive sleep, 429 both ways, `Reload` mid-queue, `Wait` budget, `BeginShutdown`, rune clamps incl. Japanese, `EscapeMarkdown` table, `discordapp.com`, alias-known, footer/author shape, mention shape | 2, 3, 4, 5, 7 |
| 6. Consumer tests with `Recorder`: multi-part records `muxing`; first-seen-live records no `scheduled`; recovery with zero parked records the all-clear; a Twitch outage records one resume carrying the pause and no pause | 6, 7 |
| 7. Docs truth: operations.md, README.md, SPEC.md:885, the settings skill — plus `SPEC.md:875`/`:887`, which this arc itself made stale | 8 |
| Constraints: DB untouched, update path untouched, no new config keys, TUI fence, LF, citation gate | Global Constraints + 8, 9 |

**Type consistency across tasks.** `Sender` (Task 1) is what Tasks 6 and 7's fixtures assign a `Recorder` to, and `Call.Field` (Task 1) is what Task 7's resume-embed assertion reads. `Manager.BeginShutdown` is **declared empty in Task 1** — `Notifier` names it and `runstate.go:68` is typed `Notifier` from that task on, so without the declaration `services.go:827` does not compile — and Task 4 replaces the body; no task before 5 calls it. `AllowedMentions`/`MentionParse` (Task 2) are exported because Arc N2b resolves the object and puts it in `SendOptions.MentionAllowed`; `buildPayload` consumes it as-is rather than re-deriving, so the option type and the wire type are the same declaration. `Tier`/`effectiveTier` (Task 2) is what Task 4's `Send` and `enqueue` read. `clampEmbed` (Task 3) is called from the `buildPayload` body Task 2 rewrote — Task 3 inserts one line into it and must not restructure it. The `sender` interface gains `SendOnce` in Task 4, and Task 5 is what gives `DiscordWebhook.SendOnce` its bucket-aware body; Task 4's test doubles (`gateSender`, `senderFunc`, `recordingSender`) each implement both methods from Task 4 onward. `notificationTarget.key` (Task 4) is set by `buildTargets` and read by `applyTargets`; Task 3's `discordapp.com` canonicalisation is what makes that key correct for the legacy spelling. `sendTwitchSessionNotification` gains its variadic `extra` in Task 7 only — Task 6 does not touch it.
