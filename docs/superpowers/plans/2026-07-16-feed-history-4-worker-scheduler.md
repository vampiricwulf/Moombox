# Feed History 4/5 — Worker & Scheduler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The `Queued` status, the scheduler that admits backlog M-at-a-time per channel, the slot-release flips (live AND upcoming), the VOD-only download pool, and job creation by disposition.

**Architecture:** One single-goroutine scheduler owned by `DownloadWorker` — single-threaded admission makes the count-then-admit race impossible by construction (satisfying the spec's per-channel-lock requirement). `Queued` is a durable resting state: `ShouldProcess` returns false for it, so nothing but the scheduler ever moves it. The pool's arbiter stays `AcquireDownloadSlot`; it just stops gating broadcasts.

**Depends on:** Plans 1–3 (schema columns, `JobDisposition` from Plan 3 Task 5).

**Spec:** `docs/superpowers/specs/2026-07-15-feed-history.md` §10, §14 (Twitch), §15 (history). Global Constraints from Plan 1 apply.

---

### Task 1: `StatusQueued` — a resting state

**Files:**
- Modify: `internal/database/types.go:7-16` (add const), `internal/worker/queue.go:350-357` (`ShouldProcess` — verify, don't change), `internal/database/database_jobs.go:622-645` (`JobStats`)
- Modify (rendering sweep): `internal/tui/styles.go:137,:159` (color maps), `internal/tui/task_list.go:607-626` (`statusPriority` sort), `internal/tui/status_bar.go:199`, `internal/tui/job_details.go:521`, `web/public/app.js:2042,:3518-3519`, `web/public/modules/filter-engine.js:7`, `web/public/modules/stats.js:168`
- Test: `internal/worker/queue_test.go`, `internal/database/database_test.go` (extend)

**Interfaces:**
- Produces: `database.StatusQueued JobStatus = "Queued"`; `JobStats.QueuedCount int`.
- Rules: **`ShouldProcess(Queued) == false`** — `ShouldProcess` is an enqueuer, not a classifier: both callers (`worker.go:314` startup, `:349` heartbeat) feed it straight to `queue.Enqueue`, and true would sweep the backlog into `JobQueue` and past the silent 100-drop (`queue.go:93-98`). `IsTerminal(Queued) == false`. `ActiveCount` must NOT include Queued (a waiting job is not a running download).

- [ ] **Step 1: Failing tests**

```go
func TestQueuedIsRestingState(t *testing.T) {
	j := &database.Job{ID: "q1", Status: database.StatusQueued}
	if ShouldProcess(j) {
		t.Fatal("ShouldProcess(Queued) must be false — both callers Enqueue on true (worker.go:314, :349)")
	}
	if j.IsTerminal() {
		t.Fatal("Queued is waiting, not finished")
	}
}

func TestEnqueueExistingJobsIgnoresQueued(t *testing.T) {
	// Spec §17: call enqueueExistingJobs (worker.go:294) against a DB holding a
	// Queued row; assert it is neither enqueued nor mutated. Build the worker with
	// the existing test constructor (grep: grep -n "NewDownloadWorker" internal/worker/*_test.go | head -2).
}

func TestJobStatsQueuedCount(t *testing.T) {
	// Insert one Downloading, one Queued ⇒ ActiveCount 1 (unchanged), QueuedCount 1.
}
```

- [ ] **Step 2:** Verify failure → implement: add the const; `ShouldProcess`'s switch already defaults false — the test PINS it; extend the `JobStats` SQL with `SUM(status = 'Queued') AS queued_count`; rendering sweep gives Queued a muted color, sorts it below active statuses, buckets it as waiting-not-active, renders no progress/ETA.
- [ ] **Step 3:** Verify pass; commit `feat(worker): Queued status — resting, never self-enqueued`.

---

### Task 2: Job creation by disposition

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go:236-261` (`OnVideoFound` implementation), `internal/database/database_jobs.go` (`Job` struct + `insertJobExec` column list)
- Test: `internal/database/database_test.go` + a host-level test if the repo has one for callbacks (grep first; if none, DB-level assertions suffice)

**Interfaces:**
- Consumes: `monitor.JobDisposition` (Plan 3).
- Produces: `Job.ChannelID string`, `Job.QueuePriority int` persisted. Creation semantics (spec §10 table):

| Disposition | Status | queue_priority | Enqueue now? | Wake scheduler? |
|---|---|---|---|---|
| Broadcast (live/upcoming) | `Upcoming` | 0 | yes (`EnqueueJob`) | no |
| NewVOD | `Upcoming` | 0 | yes | no |
| BacklogVOD | `Queued` | 1 | **no** | **yes** |

`AddToHistory` fires for **every** disposition, exactly as today (`:260`) — it is what makes `HasProcessed` mean "a job was created" (§10/§15). `channel_id` is written for all three (feed/DECAPI jobs); Twitch/manual creation paths are untouched and keep NULL.

- [ ] **Step 1: Failing test** — create one job per disposition through the callback (or directly via `AddJob` with the fields if the callback isn't testable at this level), assert status/priority/channel_id per the table, assert the Queued job was NOT enqueued (`queue.IsProcessing == false` and no pending entry), assert history rows exist for all three.
- [ ] **Step 2:** Implement: add the two fields to `Job`, extend `insertJobExec`'s column list and placeholders; in `OnVideoFound` switch on disposition to set `Status`/`QueuePriority`, skip `s.dlWorker.EnqueueJob` for backlog and call `s.dlWorker.Scheduler().Wake()` instead (Task 3 provides it; until then leave a compile-blocking call — Tasks 2 and 3 land as one PR unit if needed, or stub `Wake()` first in Task 3 order).

**Ordering note:** implement Task 3's scheduler skeleton (types + `Wake()`) BEFORE this task's final commit if the compiler demands it; the plan orders them 1→2→3 for readability, but 2+3 may merge into one commit.

- [ ] **Step 3:** Verify pass; commit `feat(worker): job creation by disposition — only backlog is Queued`.

---

### Task 3: The scheduler

**Files:**
- Create: `internal/worker/scheduler.go`
- Modify: `internal/worker/worker.go` (own + start it in `Start` near `:201`; wake it on job completion — grep the completion path: `grep -n "queue.Complete\|Complete(" internal/worker/worker.go internal/worker/orchestrator*.go | head`)
- Modify: `internal/database/database_jobs.go` (two queries)
- Test: `internal/worker/scheduler_test.go`

**Interfaces:**

```go
type Scheduler struct { /* db, queue, updateJob func, wake chan struct{}, resolveSlots func(channelID string) int, log logger */ }
func (s *Scheduler) Wake()                    // non-blocking send, select+default
func (s *Scheduler) Run(ctx context.Context)  // single goroutine; panic-restart wrapper modeled on pollForJobs (worker.go:322-360)
// database:
func (db *Database) QueuedChannels() ([]string, error) // DISTINCT channel_id FROM jobs WHERE status='Queued' AND channel_id IS NOT NULL
func (db *Database) CountBacklogInFlight(channelID string) (int, error)
func (db *Database) NextQueuedJobs(channelID string, limit int) ([]string, error) // ordered published DESC via feed_items JOIN
```

The two queries, verbatim (spec §10):

```sql
-- M count: ALLOW-list. NOT IN would leak COOKIES? — neither Queued nor terminal —
-- and a channel whose cookies lapse would silently lose its throughput forever.
SELECT COUNT(*) FROM jobs
 WHERE channel_id = ? AND queue_priority = 1
   AND status IN ('Upcoming','Live','Downloading','Muxing');

-- Admission order: published DESC — no priority term (only backlog is ever Queued).
-- INNER JOIN is guaranteed to hit: only the archival pass creates Queued rows.
SELECT j.id FROM jobs j
  JOIN feed_items f ON f.channel_id = j.channel_id AND f.video_id = j.video_id
 WHERE j.channel_id = ? AND j.status = 'Queued'
 ORDER BY f.published DESC
 LIMIT ?;
```

Admission transition (spec §10, order is load-bearing):

```go
// 1. durable FIRST — this is what the M count observes; Enqueue touches no DB row
s.updateJob(jobID, map[string]any{"status": database.StatusUpcoming})
// 2. hand to JobQueue
s.queue.Enqueue(jobID, database.StatusUpcoming)
```

Run loop: `select { case <-ctx.Done(): return; case <-s.wake:; case <-time.After(heartbeatInterval): }` then one admission sweep: for each `QueuedChannels()` channel, `admit := resolveSlots(ch) - CountBacklogInFlight(ch)`; admit that many from `NextQueuedJobs`. Single-threaded ⇒ no count-then-admit race. Crash between the two admission steps is self-healing: the row reads `Upcoming`, `ShouldProcess` accepts it, `enqueueExistingJobs` (`worker.go:294-318`) re-enqueues on restart. `resolveSlots` is the per-channel `archive_slots` resolver injected from the host (`cmd/moombox`), keyed by `channel_id` — the host builds a map from the config store on each wake (channels are few; rebuild is cheap and picks up config edits).

- [ ] **Step 1: Failing tests**

```go
func TestScheduler_Converges300(t *testing.T) {
	// Spec §17: 300 Queued rows (one channel, feed_items rows to join), M=3,
	// stubbed queue. First sweep admits exactly 3 (status flips BEFORE Enqueue —
	// assert with an updateJob spy that records order). Completing one (status →
	// Finished) + Wake() admits exactly 1 more. Nothing ever reaches the stub
	// queue beyond admitted jobs; pending never grows past M.
}
func TestScheduler_MCountIsAllowList(t *testing.T) {
	// A priority-1 job in COOKIES? does NOT hold a slot; in Muxing DOES.
}
func TestScheduler_AdmissionOrderPublishedDesc(t *testing.T) {
	// Three Queued rows with shuffled feed_items dates ⇒ admitted newest-first.
}
func TestScheduler_IgnoresNullChannel(t *testing.T) {
	// A Queued row with NULL channel_id (defensive) is never admitted and does not wedge the sweep.
}
```

- [ ] **Step 2:** Verify failure → implement per above (inline `defer recover()` restart wrapper copied from `pollForJobs`'s shape).
- [ ] **Step 3:** Verify pass; commit `feat(worker): backlog scheduler — M per channel, allow-list count, durable admission`.

---

### Task 4: Slot-release flips — live AND upcoming

**Files:**
- Modify: `internal/worker/stream_processor.go:208` (Live write) and `:252-253` (StreamUpcoming → `waitForLive` entry)
- Test: `internal/worker/scheduler_test.go` (extend)

**Rules (spec §10):** both flips are one-way writes of `queue_priority = 0`, in the same `UpdateJobFields` call as the status write where one exists:
- `:208`: add `"queue_priority": 0` to the Live status update map.
- `:252-253`: there is NO status write on this path (the job stays `Upcoming` through the whole wait — potentially days) — insert `sp.db.UpdateJobFields(job.ID, map[string]any{"queue_priority": 0})` guarded by `if job.QueuePriority == 1` immediately before `return sp.waitForLive(ctx, job, info)`.

- [ ] **Step 1: Failing tests** — (a) a priority-1 job whose processor writes Live no longer counts in `CountBacklogInFlight`; (b) a priority-1 job routed into the upcoming path flips before the wait (assert priority 0 in DB while the wait stub blocks); (c) `Wake()` is called after each flip so the freed slot admits the next backlog VOD promptly (wire the wake beside both flips).
- [ ] **Step 2:** Implement; verify; commit `feat(worker): broadcast flips release the M slot before the wait/stream`.

---

### Task 5: VOD-only download pool

**Files:**
- Modify: `internal/worker/worker.go:444-446` (`AcquireDownloadSlot` call site)
- Test: `internal/worker/queue_test.go` (extend)

**Rules (spec §10/§14):** the exemption predicate is **`result.IsVod`** — job status disagrees with it for `not_a_stream`+`AllowNonStream` (`stream_processor.go:229-245`) and Twitch recovery (`stream_processor_twitch.go:85`). Twitch live therefore becomes unbounded — intended (§14). Release paths are already safe for never-acquired jobs (`holdingDlSlot` keyed by jobID; `Complete`'s defensive release `queue.go:224-231`) — the test proves it.

- [ ] **Step 1: Failing test** — with `maxDownloads=1` and one `IsVod` job holding the slot, a second job with `IsVod=false` proceeds past the acquire site without blocking; a second `IsVod=true` job blocks; completing the non-VOD job does not corrupt `activeDownloads` (assert `ActiveCount()==1` throughout).
- [ ] **Step 2:** Implement:

```go
if result.IsVod {
	if err := w.queue.AcquireDownloadSlot(ctx, job.ID); err != nil { /* existing error path */ }
}
```

(The matching release is keyed by `holdingDlSlot`, so no symmetric change is needed — verify by reading `ReleaseDownloadSlot`/`Complete` before trusting this note.)

- [ ] **Step 3:** Verify pass (`go test ./internal/worker/ -v` — all pre-existing pool tests must still pass); commit `feat(worker): download pool gates VODs only — a broadcast cannot be throttled`.

---

### Task 6: Channel prune helper (consumed by Plan 5)

**Files:**
- Create in: `internal/database/database_jobs.go`
- Test: `internal/database/database_test.go` (extend)

**Interfaces:**
- Produces: `func (db *Database) DeleteJobsAndHistoryForChannel(channelID string, statuses []JobStatus) (deleted int, err error)` — deletes jobs matching `(channel_id, status ∈ statuses)` **and their history rows** (`DELETE FROM history WHERE video_id IN (SELECT video_id FROM jobs WHERE ...)` executed BEFORE the jobs delete). Plan 5 calls it with `{Queued, Upcoming, COOKIES?}` (spec §11: pre-download states with nothing to lose; deleting the job without its history row manufactures exactly the orphan class that re-armed `gr-ZTohjwnQ`, and blocks re-add via `HasProcessed`).

- [ ] **Step 1: Failing test** — seed a channel with one job per status + history rows; call with the three statuses; assert those jobs AND their history rows are gone, `Live`/`Downloading`/`Finished` jobs and their history remain.
- [ ] **Step 2:** Implement (collect-then-delete under `db.mu.Lock()`; two statements, history first). Verify; commit `feat(db): channel job+history prune for pre-download states`.

## Self-check before handoff

- `grep -n "queue_priority" internal/worker/ cmd/moombox/ -r` — writers are exactly: creation (Task 2), the two flips (Task 4). Readers: the M count and `NextQueuedJobs`' absence of it (ordering has NO priority term).
- `grep -n "AcquireDownloadSlot" internal/worker/` — exactly one call site, now guarded by `result.IsVod`.
- Run the full suite: `go test ./...`.
