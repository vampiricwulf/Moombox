# Arc N3 — Edit-in-Place Lifecycle Messages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each notification target an opt-in `mode = "edit"` under which one Discord message per job is created once and then rewritten in place for every later lifecycle event, with the message id persisted on the job row so a restart keeps editing the same message.

**Architecture:** A new `jobs.notification_msgs` TEXT column (nullable JSON, `{"<target key>": "<message id>"}`, schema 19 → 20) holds the ids; the notifier reaches it through a two-method `MessageStore` interface so `internal/notifications` still imports no database package. All edit-mode behaviour lives in one new file, `internal/notifications/lifecycle.go`: the lifecycle event set, the per-(job, target) id store, the POST-or-PATCH decision, the Status/History embed rewrite, and the 404 → re-POST fallback. The per-target FIFO sender that Arc N1 built gains exactly one changed line — the call that hands a queue item to `target.sender` routes through `Manager.deliver` instead — so the merge surface against N1/N2a/N2b stays minimal. The Discord wire gains `postWait` (`?wait=true`, parse the returned `id`) and `patchMessage` in a second new file, `internal/notifications/discord_edit.go`, driven by the same bounded delivery loop that ordinary sends use.

**Tech Stack:** Go 1.27 (module `github.com/vampiricwulf/Moombox`), modernc.org/sqlite (no CGo), chi/v5, Charmbracelet bubbletea/bubbles/lipgloss, vanilla ES modules + Shoelace v2.16, `node:test` + jsdom for the frontend suites, `net/http/httptest` for the fake Discord.

**Spec:** `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` (commit `1d2df1d4`) — §0 rulings (persistence, terminal states, batching), §4 Arc N3, §5 order. Companion audit: `.superpowers/sdd/2026-09-26-webhooks/audit.md` §3 "One message per job, edited in place" (gitignored; the Discord API facts it cites are restated inline in this plan so an executor never needs it).

## Global Constraints

Every task's requirements implicitly include this section.

**From the spec (§4 Constraints + §0 rulings):**
- **Producers are unchanged.** They already pass `SendOptions.JobID` (Arc N2a). No file under `internal/worker/`, `cmd/moombox/monitor_callbacks.go`, `cmd/moombox/main.go` or `internal/web/routes/jobs.go` changes in this arc except the two one-line `SetMessageStore` wirings in Task 3.
- **The silent single-column write is the ONLY new database write.** `updateSingleColumnSilent` — no `updated_at` bump, no `OnJobUpdate`/`OnJobsChange` fan-out, no UI frame. One write per (job, target), on the first successful POST only. The DB layer's cadence ruling (2026-07-03: no database-layer perf/cadence changes) is otherwise untouched.
- **Batching stays separate-mode only.** An edit-mode target's `found`/`added`/`auth` embeds are never coalesced — for an edit-mode target the `found` embed *is* the job's lifecycle message.
- **Every N1/N2a/N2b test stays green.** This arc adds behaviour behind a default-off per-target key; with `mode` absent or `"separate"`, byte-for-byte the same requests go out as before.
- **Edits share the webhook's rate bucket** (per Discord API docs, `topics/rate-limits.mdx`: the bucket's top-level resource is `webhook_id + webhook_token`). PATCH gets the same rate-limit handling, retry schedule and FIFO position as POST. There is no separate budget.
- **No progress edits, ever.** Only the lifecycle event set below triggers a PATCH.
- **`error` / `cancelled` produce two messages**, by ruling: the lifecycle message is edited to its terminal look AND the separate embed is posted (it carries the mention).

- **Shutdown degrades the edit path too.** A Manager told it is shutting down (N1's `BeginShutdown` → the queue's single-attempt `SendOnce` path) must not run the three-attempt loop for a lifecycle POST or PATCH either — the owner's 10 s force-exit cap is the whole point of that ruling, and one edit-mode job could otherwise burn it. **Adaptation point:** if N1 spells the seam differently (a different method name, or a flag on the queue rather than `BeginShutdown`), use N1's spelling; the requirement is one request per verb while shutting down, never a retry loop.

**Standing project rules:**
- **Merge main before Task 1.** N1, N2a and N2b are on main by the time this arc runs, and `config.example.toml`, `internal/notifications/{manager.go,discord.go}`, `web/public/modules/settings.js`, `internal/tui/settings_notifications.go`, `docs/spec/operations.md`, `docs/spec/data-and-storage.md` and `README.md` all carry their edits. Resolve the merge first, then re-read every file this plan touches before editing it.
- Go 1.27 floor (`go.mod` says `go 1.27`, `toolchain go1.27.1`). No CGo. Pure-Go dependencies only.
- Three platforms build: windows/amd64, linux/amd64, linux/arm64. Nothing added here is platform-conditional.
- `go mod tidy -diff` must stay clean (no new module dependencies — everything used here is stdlib).
- **LF line endings.** `.gitattributes` pins them; never introduce CRLF into a tracked file.
- **The citation gate** (`internal/docs/citations_test.go`): every symbol and path named in `docs/spec/{architecture,data-and-storage,operations,platform-services,security,user-interfaces}.md`, `SPEC.md`, `CLAUDE.md`, `README.md` and the seven `.claude/skills/*/SKILL.md` files must exist. Doc edits in Task 7 are checked by `go test ./internal/docs/`. Do not add allowlist entries to fix rot — fix the doc.
- **The TUI import fence:** `internal/tui` must not import `internal/web` or `internal/bgutils`. It may import `internal/config` and `internal/notifications` (it already does both).
- **One commit per task, always with a pathspec**: `git commit -m "…" -- <exact files>`. A bare `git commit` has swept another worker's staged files before (fedf98c5).
- **Every commit message ends with these two lines, verbatim:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
  ```
- **Implementers never run the full suite.** Never `go test ./...`. Run only the named packages, and always with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` in the environment.
- Never invoke `python3 -` heredocs (they hang forever under the uv shim). Use the Write/Edit tools for file content; `ls` is aliased — use `command ls`.
- Read the code before every step. Line numbers in this plan are verified against main at the time of writing for `internal/database/*` (stable) and are given as *anchors, not coordinates* for files Arcs N1/N2a/N2b are rewriting (`internal/notifications/manager.go`, `internal/notifications/discord.go`, `web/public/modules/settings.js`, `internal/tui/settings_notifications.go`). For those, locate the named symbol and edit there.

## Review Focus

Six conditions the spec implies that no task's happy path exercises. Each has a test added to the task that owns the code.

1. **The job row is gone when the id is written** (job deleted mid-download). `UpdateNotificationMsgs` must return false and the send must complete anyway — never error, never retry the write. → Task 1 test `TestUpdateNotificationMsgsUnknownJob`, Task 3 test `TestRecordSurvivesMissingRow`.
2. **A stored id that Discord does not know** (hand-edited DB, a message deleted in the channel, a truncated snowflake). The PATCH must 404 once, re-POST, overwrite the id, and not loop. → Task 3 test `TestPatch404RePostsAndOverwrites`, Task 6 e2e row.
3. **A target flipped `edit` → `separate` (and back) by hot reload mid-job.** Later events must post separately without touching the stored id; flipping back must resume editing the same message. → Task 6 test `TestModeFlipMidJob`.
4. **Malformed or half-written JSON in the column.** `decodeNotificationMsgs` must answer nil and the job must post a new message — a corrupt value may never fail a job scan, because every job read in the program goes through it. → Task 1 test `TestDecodeNotificationMsgsGarbage`.
5. **Two jobs editing through the same target concurrently, while `Reload` swaps targets.** The tracker map is reached from more than the one per-target goroutine. → Task 3 test `TestTrackerConcurrentAccess`, run with `-race`.
6. **The tracker map growing for the life of a 24/7 process.** One entry per job an edit-mode target ever touched, each holding a msgs map and a ~1000-rune History per target. A job's entry must be released once its story is told, and capped for jobs that never reach a terminal event. → Task 3 tests `TestTrackerReleasesFinishedJobs`, `TestTrackerCapsTrackedJobs`.

---

## Task 1: Schema 20 — the `notification_msgs` column

**Files:**
- Modify: `internal/database/migrations.go:26` (`schemaVersion`), `:151` (the last `jobs` column in `createSchema`), `:693-700` (the `version < 19` block — append a `version < 20` block after it), and append `migrateV20` after `migrateV19` (`:813-821`)
- Modify: `internal/database/types.go:149-157` (add the field after `ParkIdentity`, before `IncompleteTail`)
- Modify: `internal/database/database.go:4-16` (imports), `:261` (the `stmtGetJob` SELECT list), `:434-437` (`silentColumns`), `:495-523` (`scanJobRow`), and append the two new methods after `UpdateChatOffset` (`:482-484`)
- Modify: `internal/database/database_jobs.go:132` (the `getAllJobsUnlocked` SELECT list)
- Create: `internal/database/migrations_v20_test.go`
- Modify: `docs/spec/data-and-storage.md` — deferred to Task 7 (docs land together)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  // internal/database
  type Job struct {
      // ...
      NotificationMsgs map[string]string `json:"-"`
  }
  func (db *Database) UpdateNotificationMsgs(jobID string, msgs map[string]string) bool
  func (db *Database) NotificationMsgs(jobID string) map[string]string
  ```
  Task 3's `notifications.MessageStore` is exactly these two methods, so `*database.Database` satisfies it with no adapter.

- [ ] **Step 1: Write the failing test**

Create `internal/database/migrations_v20_test.go`:

```go
package database

import (
	"testing"
)

// TestMigrationV20 mirrors TestMigrationV19: newTestDB runs createSchema at
// the current schemaVersion, so this pins the fresh-install side — a
// legacy-shaped row (INSERT omitting notification_msgs) must read back NULL.
//
//	notification_msgs TEXT   (nullable — no default)
func TestMigrationV20(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy20','legacy20','u','t','Finished','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	// Read narrowly, as TestMigrationV19 does: a legacy-shaped row leaves
	// stream_start_time NULL, and Job.StreamStartTime is a plain string, so
	// GetJob cannot scan such a row at all (pre-existing, and not what this
	// test is about). The scanJobRow decode path is covered by
	// TestNotificationMsgsRoundTrip's real AddJob row.
	if got := db.NotificationMsgs("legacy20"); got != nil {
		t.Fatalf("legacy row NotificationMsgs = %v, want nil", got)
	}
}

// TestMigrationV20Idempotent: re-running the guarded ALTER on an
// already-migrated DB must be a no-op, not an error — a crash mid-block
// re-runs the whole block on next startup, because user_version is last.
func TestMigrationV20Idempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV20(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV20(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestNotificationMsgsRoundTrip pins the whole column: the silent write, both
// read paths (the narrow single-column reader the notifier uses on its hot
// path, and the full scan every UI read goes through), and the no-op that a
// nil map must be.
func TestNotificationMsgsRoundTrip(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm1", VideoID: "nm1", URL: "https://youtube.com/watch?v=nm1", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got := db.NotificationMsgs("yt_nm1"); got != nil {
		t.Fatalf("fresh job NotificationMsgs = %v, want nil", got)
	}

	want := map[string]string{"3f6a1b2c9d0e4f57": "1234567890123456789"}
	if !db.UpdateNotificationMsgs("yt_nm1", want) {
		t.Fatal("UpdateNotificationMsgs returned false for an existing job")
	}

	got := db.NotificationMsgs("yt_nm1")
	if len(got) != 1 || got["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
		t.Fatalf("NotificationMsgs = %v, want %v", got, want)
	}

	full, err := db.GetJob("yt_nm1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if full.NotificationMsgs["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
		t.Fatalf("GetJob NotificationMsgs = %v, want %v", full.NotificationMsgs, want)
	}

	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatalf("GetAllJobs: %v", err)
	}
	var seen bool
	for _, j := range all {
		if j.ID == "yt_nm1" {
			seen = true
			if j.NotificationMsgs["3f6a1b2c9d0e4f57"] != "1234567890123456789" {
				t.Fatalf("GetAllJobs NotificationMsgs = %v, want %v", j.NotificationMsgs, want)
			}
		}
	}
	if !seen {
		t.Fatal("GetAllJobs did not return yt_nm1")
	}
}

// TestUpdateNotificationMsgsSilent: the ids feed the notifier and no UI reads
// them, so the write must not bump updated_at (which would put a frame on the
// WebSocket and the TUI for every job that reaches an edit-mode target).
//
// All THREE job subscribers are watched, not just OnJobUpdate:
// updateSingleColumnSilent bypasses OnJobChange and OnJobsChange too, and those
// are the richer paths a future edit is likelier to wire up by accident.
func TestUpdateNotificationMsgsSilent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm2", VideoID: "nm2", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	before, err := db.GetJob("yt_nm2")
	if err != nil {
		t.Fatal(err)
	}

	fired := make(chan struct{}, 8)
	db.OnJobUpdate(func(*Job) { fired <- struct{}{} })
	db.OnJobChange(func(*JobChange) { fired <- struct{}{} })
	db.OnJobsChange(func([]*Job) { fired <- struct{}{} })

	db.UpdateNotificationMsgs("yt_nm2", map[string]string{"k": "1"})

	after, err := db.GetJob("yt_nm2")
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved from %q to %q — the write must be silent", before.UpdatedAt, after.UpdatedAt)
	}
	select {
	case <-fired:
		t.Error("a job subscriber fired — the write must wake none of them")
	default:
	}
}

// TestUpdateNotificationMsgsUnknownJob (Review Focus 1): the job row can be
// deleted between the POST and its response. The write must answer false and
// leave the caller free to carry on — never error, never retry.
func TestUpdateNotificationMsgsUnknownJob(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if db.UpdateNotificationMsgs("yt_gone", map[string]string{"k": "1"}) {
		t.Error("UpdateNotificationMsgs returned true for a job that does not exist")
	}
	if got := db.NotificationMsgs("yt_gone"); got != nil {
		t.Errorf("NotificationMsgs for a missing job = %v, want nil", got)
	}
}

// TestUpdateNotificationMsgsEmptyStoresNULL: an empty map is "this job has no
// lifecycle message anywhere", which is SQL NULL, not the string "{}".
func TestUpdateNotificationMsgsEmptyStoresNULL(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm3", VideoID: "nm3", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	db.UpdateNotificationMsgs("yt_nm3", map[string]string{"k": "1"})
	db.UpdateNotificationMsgs("yt_nm3", nil)

	var raw any
	if err := db.db.QueryRow(`SELECT notification_msgs FROM jobs WHERE id='yt_nm3'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Errorf("notification_msgs = %v, want NULL", raw)
	}
}

// TestDecodeNotificationMsgsGarbage (Review Focus 4): a half-written or
// hand-mangled value must read as nil so the job simply posts a new message.
// A scan that failed here would break EVERY read of that job — the dashboard,
// the TUI and the worker all go through scanJobRow.
func TestDecodeNotificationMsgsGarbage(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_nm4", VideoID: "nm4", URL: "u", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for _, bad := range []string{`{"k":`, `[]`, `"not an object"`, ``, `   `} {
		if _, err := db.db.Exec(`UPDATE jobs SET notification_msgs = ? WHERE id = 'yt_nm4'`, bad); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetJob("yt_nm4")
		if err != nil {
			t.Fatalf("GetJob with stored %q: %v", bad, err)
		}
		if got.NotificationMsgs != nil {
			t.Errorf("stored %q decoded to %v, want nil", bad, got.NotificationMsgs)
		}
		if narrow := db.NotificationMsgs("yt_nm4"); narrow != nil {
			t.Errorf("stored %q read narrowly as %v, want nil", bad, narrow)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/ -run 'TestMigrationV20|TestNotificationMsgs|TestUpdateNotificationMsgs|TestDecodeNotificationMsgsGarbage' -count=1
```
Expected: FAIL to compile — `db.migrateV20 undefined`, `db.NotificationMsgs undefined`, `db.UpdateNotificationMsgs undefined`, `got.NotificationMsgs undefined (type *Job has no field or method NotificationMsgs)`.

- [ ] **Step 3: Bump the schema version and add the migration**

`internal/database/migrations.go:26`:
```go
const schemaVersion = 20
```

`internal/database/migrations.go:151` — the `jobs` table in `createSchema` ends with `park_identity TEXT NOT NULL DEFAULT ''` followed by `);`. Add a comma and the new column:
```sql
    park_identity TEXT NOT NULL DEFAULT '',
    notification_msgs TEXT
);
```

`internal/database/migrations.go` — after the `if version < 19 { … }` block (`:693-700`), before `return nil`:
```go
	if version < 20 {
		if err := db.migrateV20(); err != nil {
			return err
		}
		if err := db.writeUserVersion(20); err != nil {
			return err
		}
	}
```

After `migrateV19` (`:813-821`), append:
```go
// migrateV20 adds notification_msgs to jobs: the per-target Discord message
// ids an edit-mode notification target rewrites in place for this job. See
// the Job.NotificationMsgs doc comment in types.go.
//
// Nullable with NO default, unlike every other column added since v13: the
// absence of a lifecycle message is SQL NULL, and an empty string would be a
// second spelling of it that every reader would then have to know about.
// (Prose, not code: a bare '' pair in a doc comment is rewritten by gofmt's
// doc-comment normalisation and trips `gofmt -l`.)
//
//	notification_msgs TEXT
func (db *Database) migrateV20() error {
	ctx := db.getCtx()
	// Guarded ALTER: a crash mid-block re-runs the whole block (user_version is
	// written last), so a duplicate-column error is expected and benign.
	if _, err := db.db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN notification_msgs TEXT`); err != nil && !isDuplicateColumnErr(err) {
		return fmt.Errorf("v20 alter: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Add the Job field**

`internal/database/types.go` — after the `ParkIdentity string \`json:"-"\`` line (`:149`), before the `IncompleteTail` comment:
```go
	// NotificationMsgs maps a notification target's stable key — the first 16
	// hex digits of SHA-256 over the RESOLVED webhook URL, the same key
	// notifications.buildTargets dedupes on — to the id of the one Discord
	// message that target rewrites in place for this job (per-target
	// `mode = "edit"`). Empty/absent for every job on a separate-mode install,
	// which is the default.
	//
	// Written ONCE per (job, target), on the first successful POST, through
	// UpdateNotificationMsgs: a silent single-column write that bumps no
	// updated_at and wakes no subscriber (owner ruling 2026-09-27). It is not
	// in fieldToColumn on purpose — UpdateJobFields must never be able to
	// write it, because that is the path that fans out to every UI.
	//
	// json:"-" on purpose. It pairs a fingerprint of a webhook URL (which IS
	// the credential) with a Discord message id, and no UI displays either.
	NotificationMsgs map[string]string `json:"-"`
```

- [ ] **Step 5: Wire the column through the read and write paths**

`internal/database/database.go:4-16` — add `"encoding/json"` to the import block (keep it sorted: after `"database/sql"`).

`internal/database/database.go:261` — the `stmtGetJob` SELECT list's last line is
`auto_retry_count, channel_id, queue_priority, incomplete_tail, park_reason, park_identity`. Change it to:
```
		auto_retry_count, channel_id, queue_priority, incomplete_tail, park_reason, park_identity,
		notification_msgs
```

`internal/database/database_jobs.go:132` — the same line in `getAllJobsUnlocked`'s query. Apply the identical change (column order must match `scanJobRow`).

`internal/database/database.go:495-523` — `scanJobRow`: add the local, the scan target and the decode:
```go
func scanJobRow(r rowScanner) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream, watched, incompleteTail int
	var notifMsgs sql.NullString
	err := r.Scan(
		// ... unchanged through &j.ParkIdentity ...
		&j.AutoRetryCount, &j.ChannelID, &j.QueuePriority, &incompleteTail, &j.ParkReason, &j.ParkIdentity,
		&notifMsgs,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = intToBool(isVod)
	j.ManuallyAdded = intToBool(manuallyAdded)
	j.AllowNonStream = intToBool(allowNonStream)
	j.Watched = intToBool(watched)
	j.IncompleteTail = intToBool(incompleteTail)
	j.NotificationMsgs = decodeNotificationMsgs(notifMsgs)
	return &j, nil
}
```

`internal/database/database.go:434-437` — add the column to `silentColumns`:
```go
var silentColumns = map[string]struct{}{
	"resume_position":   {},
	"chat_offset":       {},
	"notification_msgs": {},
}
```

`internal/database/database.go` — after `UpdateChatOffset` (`:482-484`), append:
```go
// decodeNotificationMsgs turns the stored JSON object into the per-target
// message-id map. NULL, an empty/blank string, malformed JSON, a non-object
// and an empty object all read as nil.
//
// Deliberately total: this runs inside scanJobRow, which is the ONE path every
// job read in the program goes through. A corrupt value here must degrade to
// "this job has no lifecycle message, post a new one" — never fail the scan
// and take the dashboard, the TUI and the worker down with it.
func decodeNotificationMsgs(v sql.NullString) map[string]string {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(v.String), &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// UpdateNotificationMsgs stores a job's per-target lifecycle message ids
// WITHOUT bumping updated_at or notifying subscribers. The notifier is the
// only reader and no UI displays the value, so a subscriber frame per write
// would be pure noise (owner ruling 2026-09-27: one write per job per target,
// on the first successful POST only).
//
// An empty or nil map stores SQL NULL rather than "{}" — the absence of a
// lifecycle message has exactly one spelling. Returns true when a row was
// actually updated, so a job deleted between the POST and its response
// answers false instead of silently reporting success.
func (db *Database) UpdateNotificationMsgs(jobID string, msgs map[string]string) bool {
	var value any
	if len(msgs) > 0 {
		b, err := json.Marshal(msgs)
		if err != nil {
			if db.logger != nil {
				db.logger.Error("UpdateNotificationMsgs: marshal failed", "jobID", jobID, "err", err)
			}
			return false
		}
		value = string(b)
	}
	return db.updateSingleColumnSilent(jobID, "notification_msgs", value, "UpdateNotificationMsgs")
}

// NotificationMsgs reads just the lifecycle message-id map for one job.
//
// A narrow single-column read on purpose: the notifier needs this on the first
// lifecycle event of every job, and GetJob would drag the gaps and trims joins
// along for a column nothing else on that path uses. An unknown job, a read
// error and a corrupt value all answer nil.
func (db *Database) NotificationMsgs(jobID string) map[string]string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var raw sql.NullString
	err := db.db.QueryRowContext(db.getCtx(),
		"SELECT notification_msgs FROM jobs WHERE id = ?", jobID).Scan(&raw)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && db.logger != nil {
			db.logger.Warn("NotificationMsgs: read failed", "jobID", jobID, "err", err)
		}
		return nil
	}
	return decodeNotificationMsgs(raw)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/ -count=1
```
Expected: `ok github.com/vampiricwulf/Moombox/internal/database`. Three pre-existing tests matter here and must stay green:
- `TestFreshSchemaMatchesMigratedSchema` (`database_test.go:1359`) — rewinds a database to `user_version = 1`, replays every migration and diffs the inventory against a fresh `createSchema`. **This is the real guard that Step 3's two halves agree**, and it is what the spec's "the migration up … on a fixture DB" clause is satisfied by.
- `TestFieldToColumnCoverage` — stays green with no excluded-set entry, because it skips `json:"-"` fields.
- `TestMigrateRefusesNewerSchema` — the downgrade guard; it derives its bumped version from `schemaVersion+1`, so 20 needs no edit there.

- [ ] **Step 7: Verify nothing else reads the jobs column list**

```bash
grep -rn "park_identity" --include=*.go internal/ cmd/ | grep -v _test
```
Expected: 14 lines. The ones that matter are the two full-row SELECTs (`database.go:261`, `database_jobs.go:132`), the `fieldToColumn` entry (`database.go:73`), the scan (`database.go`) and the `migrations.go` sites — every one of those must now also name `notification_msgs` (except `fieldToColumn`, which deliberately must not). The rest — `internal/worker/worker.go` ×5, `cmd/moombox/monitor_callbacks.go:148`, `internal/cookies/refresh.go:226` — are `UpdateJobFields` keys and comments, expected noise. **If a THIRD full-row SELECT has appeared, add `notification_msgs` there too.**

> The `moombox-database-migrations` skill's header still reads "Current schema version: **v16**" — stale since v17 and knowingly unmaintained. Its *idiom* (the `if version < N` block, the guarded ALTER, `writeUserVersion` last, the `createSchema` half, the `fieldToColumn` rule) is what this task follows and is current. Task 8 bumps the header line.

- [ ] **Step 8: Commit**

```bash
git add internal/database/migrations.go internal/database/migrations_v20_test.go internal/database/types.go internal/database/database.go internal/database/database_jobs.go
git commit -m "feat(database): schema 20 — jobs.notification_msgs for edit-in-place message ids

Nullable JSON column mapping a notification target's key to the Discord
message it rewrites in place for this job. Written only through
UpdateNotificationMsgs, a silent single-column write (no updated_at bump,
no subscriber fan-out), per the 2026-09-27 ruling. NotificationMsgs reads
it narrowly for the notifier's hot path; scanJobRow decodes it totally so
a corrupt value can never fail a job read.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/database/migrations.go internal/database/migrations_v20_test.go internal/database/types.go internal/database/database.go internal/database/database_jobs.go
```

---

## Task 2: The Discord wire — `postWait`, `patchMessage`, Unknown-Message detection

**Files:**
- Create: `internal/notifications/discord_edit.go`
- Create: `internal/notifications/discord_edit_test.go`
- Modify: `internal/notifications/discord.go` — five edits, located by symbol (Arc N1 rewrote this file; do not trust line numbers):
  1. `post` → `do(method, endpoint string, body []byte, wantBody bool)`, keeping every return N1 left on it and adding `respBody []byte`
  2. the bounded delivery loop inside `Send` → extracted to `deliver(method, endpoint string, body []byte, wantBody bool) ([]byte, error)`; `Send` becomes a one-line call
  3. `discordStatusErr` returns `*discordHTTPError` (declared in the new file) instead of a bare `fmt.Errorf`
  4. the other `d.post` call sites move to `d.do(...)` — `sendOnce` on main, plus N1's exported `SendOnce` (the shutdown single-attempt path the `sender` interface gained)
  5. a new `discordMessageBodyBytes` constant beside `discordErrBodyBytes`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces (package-internal, used by Task 3):
  ```go
  func (d *DiscordWebhook) postWait(body []byte) (messageID string, err error)
  func (d *DiscordWebhook) patchMessage(messageID string, body []byte) error
  func (d *DiscordWebhook) postWaitOnce(body []byte) (messageID string, err error)
  func (d *DiscordWebhook) patchMessageOnce(messageID string, body []byte) error
  func (d *DiscordWebhook) messageURL(messageID string) string
  var ErrUnknownMessage error
  type discordHTTPError struct{ Status int; Snippet string }
  ```

**Discord API facts this task encodes** (per the Discord API docs, quoted in the audit §3): `POST /webhooks/{id}/{token}?wait=true` "waits for server confirmation of message send before response, and returns the created message body"; without `?wait=true` the webhook answers `204` with no body. `PATCH /webhooks/{id}/{token}/messages/{message.id}` "edits a previously-sent webhook message from the same token, returns a message object". A missing message answers `404` with `{"message": "Unknown Message", "code": 10008}`. A revoked webhook answers `404` with `{"message": "Unknown Webhook", "code": 10015}` — which is **not** recoverable by re-posting. Rate limits are per-route with `webhook_id + webhook_token` as the top-level resource, so PATCH and POST share one bucket.

**Shutdown:** both verbs also get a single-attempt variant (`postWaitOnce` / `patchMessageOnce`), for the same reason `SendOnce` exists — the owner's 10 s force-exit cap. A post-N1 re-evaluation must re-check that `BeginShutdown` reaches these two as well as `SendOnce`.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/discord_edit_test.go`:

```go
package notifications

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedReq is one request the fake Discord saw.
type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   discordPayload
}

// fakeDiscord is an httptest server that behaves like a webhook endpoint:
// it records every request and answers from a per-call script. Shared by this
// suite and lifecycle_state_test.go (Task 6).
type fakeDiscord struct {
	mu   sync.Mutex
	reqs []recordedReq
	srv  *httptest.Server
	// handler answers one request; n is the 0-based call index.
	handler func(n int, r recordedReq, rw http.ResponseWriter)
}

func newFakeDiscord(t *testing.T, handler func(n int, r recordedReq, rw http.ResponseWriter)) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		rec := recordedReq{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery}
		_ = json.Unmarshal(raw, &rec.Body)
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()
		f.handler(n, rec, rw)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) URL() string { return f.srv.URL }

func (f *fakeDiscord) calls() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedReq, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// okCreated answers every POST with a created message carrying id, and every
// PATCH with 204.
func okCreated(id string) func(int, recordedReq, http.ResponseWriter) {
	return func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"id":"`+id+`","type":0,"content":""}`)
	}
}

// TestPostWaitAsksForTheMessageAndReturnsItsID: without ?wait=true Discord
// answers 204 with no body (per the API docs), so the id would be
// unrecoverable and edit mode could never start.
//
// MUTANT: drop the "?wait=true" suffix — the fake sees an empty query and this
// fails on the Query assertion before the id one even runs.
func TestPostWaitAsksForTheMessageAndReturnsItsID(t *testing.T) {
	f := newFakeDiscord(t, okCreated("1418889000000000001"))
	d := &DiscordWebhook{URL: f.URL()}

	body, err := buildPayload("t", "d", 0x1abc9c, nil, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.postWait(body)
	if err != nil {
		t.Fatalf("postWait: %v", err)
	}
	if id != "1418889000000000001" {
		t.Errorf("message id = %q, want 1418889000000000001", id)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 request, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPost {
		t.Errorf("method = %s, want POST", calls[0].Method)
	}
	if calls[0].Query != "wait=true" {
		t.Errorf("query = %q, want wait=true", calls[0].Query)
	}
}

// TestPostWaitRejectsAResponseWithNoID: a 2xx whose body carries no id means
// we would store "" and every later PATCH would hit /messages/ — better to
// fail the send and let the next event post again.
func TestPostWaitRejectsAResponseWithNoID(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"type":0}`)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
	if _, err := d.postWait(body); err == nil {
		t.Fatal("postWait accepted a response with no id")
	}
}

// TestPatchMessageTargetsTheMessageRoute pins the edit endpoint shape from the
// Discord API docs: PATCH /webhooks/{id}/{token}/messages/{message_id}.
func TestPatchMessageTargetsTheMessageRoute(t *testing.T) {
	f := newFakeDiscord(t, okCreated("x"))
	// A realistic webhook path on the fake's host, so the asserted route is
	// the full .../webhooks/ID/TOKEN/messages/ID shape Discord publishes.
	d := &DiscordWebhook{URL: f.URL() + "/api/webhooks/123/tok"}

	body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
	if err := d.patchMessage("999", body); err != nil {
		t.Fatalf("patchMessage: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 request, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", calls[0].Method)
	}
	if calls[0].Path != "/api/webhooks/123/tok/messages/999" {
		t.Errorf("path = %q, want /api/webhooks/123/tok/messages/999", calls[0].Path)
	}
	if calls[0].Query != "" {
		t.Errorf("PATCH carried query %q — the edit route takes none", calls[0].Query)
	}
}

// TestPatchMessageUnknownMessageIsRecoverable: 404 + code 10008 is the ONE 4xx
// on the edit path that is not permanent — the caller posts a new message and
// overwrites the id.
//
// MUTANT: treat any 404 as recoverable — the Unknown Webhook row below then
// also reports ErrUnknownMessage, and a revoked webhook would be re-POSTed to
// forever.
func TestPatchMessageUnknownMessageIsRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantErrUnknown bool
	}{
		{"unknown message", http.StatusNotFound, `{"message":"Unknown Message","code":10008}`, true},
		{"unknown webhook", http.StatusNotFound, `{"message":"Unknown Webhook","code":10015}`, false},
		{"bare 404", http.StatusNotFound, ``, false},
		{"bad request", http.StatusBadRequest, `{"message":"Invalid Form Body","code":50035}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
				rw.WriteHeader(tc.status)
				io.WriteString(rw, tc.body)
			})
			d := &DiscordWebhook{URL: f.URL()}
			body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
			err := d.patchMessage("999", body)
			if err == nil {
				t.Fatal("patchMessage accepted a refusal")
			}
			if got := errors.Is(err, ErrUnknownMessage); got != tc.wantErrUnknown {
				t.Errorf("errors.Is(err, ErrUnknownMessage) = %v, want %v (err = %v)", got, tc.wantErrUnknown, err)
			}
			if n := len(f.calls()); n != 1 {
				t.Errorf("permanent refusal retried: %d requests, want 1", n)
			}
		})
	}
}

// TestEditPathSharesTheRetrySchedule: a 5xx on either verb must be retried on
// the same bounded loop ordinary sends use — the edit rides the same bucket
// and the same three-attempt budget, not a second one.
func TestEditPathSharesTheRetrySchedule(t *testing.T) {
	old := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = old })

	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 0 {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		okCreated("42")(n, r, rw)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
	if err := d.patchMessage("999", body); err != nil {
		t.Fatalf("patchMessage did not retry a 502: %v", err)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("attempts = %d, want 2 (one 502 then one success)", n)
	}
}

// TestEditOnceVariantsMakeExactlyOneRequest: the shutdown twins must never
// retry. The owner's ruling caps a graceful shutdown at 10 s; one lifecycle
// edit running the 2 s/5 s ladder against a wedged Discord would spend it.
//
// MUTANT: point either Once variant at d.deliver instead of d.do — the 502
// rows below each make three requests and this fails on the count.
func TestEditOnceVariantsMakeExactlyOneRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*DiscordWebhook, []byte) error
	}{
		{"postWaitOnce", func(d *DiscordWebhook, b []byte) error { _, err := d.postWaitOnce(b); return err }},
		{"patchMessageOnce", func(d *DiscordWebhook, b []byte) error { return d.patchMessageOnce("999", b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
				rw.WriteHeader(http.StatusBadGateway)
			})
			d := &DiscordWebhook{URL: f.URL()}
			body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
			if err := tc.call(d, body); err == nil {
				t.Fatal("a 502 was reported as success")
			}
			if n := len(f.calls()); n != 1 {
				t.Errorf("requests = %d, want exactly 1", n)
			}
		})
	}
}

// TestPatchMessageOnceStillRecognisesUnknownMessage: the shutdown path must
// classify a 404/10008 the same way the retrying one does, or a re-post
// decision would depend on when the process happened to be stopping.
func TestPatchMessageOnceStillRecognisesUnknownMessage(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusNotFound)
		io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload("t", "d", 0, nil, SendOptions{})
	if err := d.patchMessageOnce("999", body); !errors.Is(err, ErrUnknownMessage) {
		t.Errorf("err = %v, want ErrUnknownMessage", err)
	}
}

// TestSeparateSendStillPostsPlain: the generalised loop must not change what an
// ordinary send looks like on the wire — no ?wait=true, no body read.
func TestSeparateSendStillPostsPlain(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusNoContent)
	})
	d := &DiscordWebhook{URL: f.URL()}
	if err := d.Send("t", "d", 0x3498db, nil, SendOptions{Event: "finished"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "" {
		t.Fatalf("ordinary Send changed shape: %+v", calls)
	}
}
```

The import block of this file is exactly: `encoding/json`, `errors`, `io`, `net/http`, `net/http/httptest`, `sync`, `testing`, `time`. (No `strings` — nothing in the finished file uses it.) Drop any the final file does not use — staticcheck is a hard gate.

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -run 'TestPostWait|TestPatchMessage|TestEditPathShares|TestEditOnceVariants|TestSeparateSendStillPostsPlain' -count=1
```
Expected: FAIL to compile — `d.postWait undefined`, `d.patchMessage undefined`, `d.postWaitOnce undefined`, `d.patchMessageOnce undefined`, `undefined: ErrUnknownMessage`.

- [ ] **Step 3: Generalise the request helper in `discord.go`**

Locate `func (d *DiscordWebhook) post(body []byte)`. Rename it and widen its parameters, keeping **every return value Arc N1 left on it** (N1 added the `X-RateLimit-*` reads; those stay) and adding `respBody`:

```go
// do performs one webhook request attempt against method+endpoint, returning
// the HTTP status (0 on transport error), the Retry-After header value, the
// rate-limit headers, a sanitised prefix of a >=400 response body, and — when
// wantBody is set — the 2xx response body itself.
//
// The method and target are parameters rather than the fixed POST d.URL because
// the edit path issues PATCH against the per-message route; per the Discord API
// docs both share the webhook's rate-limit bucket, so they must share this one
// request path and the loop above it.
//
// The parameter is named endpoint, not url: the body reaches for *url.Error to
// redact the token out of a transport error, and a parameter named url would
// shadow the net/url import.
func (d *DiscordWebhook) do(method, endpoint string, body []byte, wantBody bool) (status int, retryAfter, snippet string, respBody []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", nil, fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body.
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return 0, "", "", nil, fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return 0, "", "", nil, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		snippet = discordErrSnippet(b)
	} else if wantBody {
		// Only the ?wait=true POST needs this; every other call leaves the
		// body to the deferred drain.
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, discordMessageBodyBytes))
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), snippet, respBody, nil
}
```
Keep whatever additional header returns N1 added in their existing positions; add `respBody` immediately before `err`, and update the call sites accordingly.

> **Expected post-N1 shape.** N1 collapses these returns into a struct: `func (d *DiscordWebhook) post(body []byte) (discordResponse, error)`. If that is what you find, the edit is *simpler* than the snippet above — make it `do(method, endpoint string, body []byte, wantBody bool) (discordResponse, error)` and add a `Body []byte` field to `discordResponse`. Same parameter names, same reason for `endpoint`.

Add the new constant beside `discordErrBodyBytes`:
```go
	// discordMessageBodyBytes bounds the ?wait=true response read. A webhook
	// message object with one embed is a few KB; 64 KiB is slack, and the only
	// field parsed out of it is "id".
	discordMessageBodyBytes = 64 << 10
```

- [ ] **Step 4: Extract the delivery loop**

In `discord.go`, rename the bounded loop that `Send` runs today to `deliver`, give it the same four parameters, and return the successful attempt's body:

```go
// deliver runs the bounded delivery loop against one method+URL and returns
// the successful attempt's response body (nil unless wantBody).
//
// Transport errors and Discord 5xx retry on the fixed backoff schedule, 429
// honors a validated Retry-After, other 4xx are permanent. POST and PATCH go
// through this ONE loop on purpose: per the Discord API docs the bucket's
// top-level resource is webhook_id + webhook_token, so an edit spends the same
// budget as a post and must obey the same schedule.
func (d *DiscordWebhook) deliver(method, endpoint string, body []byte, wantBody bool) ([]byte, error) {
	// ... the existing loop verbatim, with:
	//   status, retryAfter, snippet, respBody, err := d.do(method, endpoint, body, wantBody)
	//   case status < 400:  return respBody, nil
	//   every `return err` becoming `return nil, err`
}
```
`Send` collapses to:
```go
func (d *DiscordWebhook) Send(title, description string, color int, fields []Field, opts SendOptions) error {
	body, err := buildPayload(title, description, color, fields, opts)
	if err != nil {
		return err
	}
	_, err = d.deliver(http.MethodPost, d.URL, body, false)
	return err
}
```
Every other `d.post(body)` call becomes `d.do(http.MethodPost, d.URL, body, false)` (discard the new `respBody` return). On main that is `sendOnce`; after N1 it is also the exported `SendOnce` the queue calls while shutting down. Grep `d.post(` and `\.post(` before claiming this step is done.

The retry loop stays inside `DiscordWebhook` — that is where N1 leaves it, and it is why `deliver`, `postWait` and `patchMessage` have the shape they do.

- [ ] **Step 5: Change `discordStatusErr` to return the typed error**

```go
func discordStatusErr(status int, snippet string) error {
	return &discordHTTPError{Status: status, Snippet: snippet}
}
```

- [ ] **Step 6: Write the new file**

Create `internal/notifications/discord_edit.go`:

```go
package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrUnknownMessage is what patchMessage answers when Discord refuses an edit
// with 404 / code 10008 ("Unknown Message"): the message this webhook created
// is gone — deleted in the channel, or the channel purged. It is the ONE 4xx
// on the edit path that is not permanent; the caller posts a fresh message and
// overwrites the stored id.
//
// A 404 naming "Unknown Webhook" (code 10015) is deliberately NOT this: the
// webhook itself was revoked, and re-posting would fail the same way forever.
var ErrUnknownMessage = errors.New("discord: unknown message")

// waitQuery makes a webhook POST answer with the created message instead of a
// bare 204. Per the Discord API docs (resources/webhook.mdx), ?wait=true
// "waits for server confirmation of message send before response, and returns
// the created message body" — the only way to learn the id an edit needs.
const waitQuery = "?wait=true"

// discordHTTPError carries the status and sanitised body prefix of a Discord
// refusal so a caller can branch on WHICH refusal it got. Only the edit path
// needs that today (404/10008 apart from every other 4xx), but the type is
// what discordStatusErr returns everywhere, so the information is never lost
// on the way up.
type discordHTTPError struct {
	Status  int
	Snippet string
}

func (e *discordHTTPError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("discord webhook returned %d", e.Status)
	}
	return fmt.Sprintf("discord webhook returned %d: %s", e.Status, e.Snippet)
}

// isUnknownMessage reports whether err is Discord's "this message no longer
// exists" refusal. The snippet is required: a bare 404 with no body could be a
// revoked webhook or a proxy, and re-posting on it would be a guess.
func isUnknownMessage(err error) bool {
	var he *discordHTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.Status != http.StatusNotFound {
		return false
	}
	return strings.Contains(he.Snippet, "Unknown Message") || strings.Contains(he.Snippet, "10008")
}

// messageURL is the per-message edit endpoint,
// PATCH /webhooks/{id}/{token}/messages/{message_id} (Discord API docs,
// resources/webhook.mdx). d.URL is already the resolved
// https://discord.com/api/webhooks/ID/TOKEN form, so the route is a suffix.
func (d *DiscordWebhook) messageURL(messageID string) string {
	return strings.TrimRight(d.URL, "/") + "/messages/" + messageID
}

// postWait creates a message and returns its id, for a target that will later
// rewrite it in place. Same loop, same retry schedule and same rate-limit
// bucket as an ordinary Send — the only differences are ?wait=true and that
// the response body is read.
func (d *DiscordWebhook) postWait(body []byte) (string, error) {
	respBody, err := d.deliver(http.MethodPost, d.URL+waitQuery, body, true)
	if err != nil {
		return "", err
	}
	return parseCreatedID(respBody)
}

// patchMessage rewrites a message this webhook created. A 404/10008 comes back
// as ErrUnknownMessage so the caller can re-post; every other refusal is
// permanent and reaches the caller unchanged.
func (d *DiscordWebhook) patchMessage(messageID string, body []byte) error {
	return asEditRefusal(messageID, mustErr(d.deliver(http.MethodPatch, d.messageURL(messageID), body, false)))
}

// postWaitOnce and patchMessageOnce are the shutdown twins of the two above:
// ONE attempt, no backoff, no Retry-After sleep. The owner's ruling caps a
// graceful shutdown at 10 s, and a single edit-mode job running the full
// three-attempt loop could spend all of it — the same reason SendOnce exists
// beside Send. The queue selects these while it is shutting down.
func (d *DiscordWebhook) postWaitOnce(body []byte) (string, error) {
	status, _, snippet, respBody, err := d.do(http.MethodPost, d.URL+waitQuery, body, true)
	switch {
	case err != nil:
		return "", fmt.Errorf("discord webhook request: %w", err)
	case status >= 400:
		return "", discordStatusErr(status, snippet)
	}
	return parseCreatedID(respBody)
}

func (d *DiscordWebhook) patchMessageOnce(messageID string, body []byte) error {
	status, _, snippet, _, err := d.do(http.MethodPatch, d.messageURL(messageID), body, false)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case status >= 400:
		return asEditRefusal(messageID, discordStatusErr(status, snippet))
	}
	return nil
}

// asEditRefusal maps Discord's "this message no longer exists" onto
// ErrUnknownMessage and passes every other refusal through unchanged. One
// helper so the retrying and single-attempt edit paths cannot disagree about
// which 404 is recoverable.
func asEditRefusal(messageID string, err error) error {
	if err == nil {
		return nil
	}
	if isUnknownMessage(err) {
		return fmt.Errorf("%w (message %s): %v", ErrUnknownMessage, messageID, err)
	}
	return err
}

// mustErr drops deliver's unused body return at the PATCH call sites.
func mustErr(_ []byte, err error) error { return err }

// parseCreatedID pulls the id out of a ?wait=true response.
func parseCreatedID(respBody []byte) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("discord: parse created message: %w", err)
	}
	if created.ID == "" {
		// Storing "" would make every later edit target /messages/ — fail the
		// send instead and let the next lifecycle event post again.
		return "", errors.New("discord: created message carried no id")
	}
	return created.ID, nil
}
```


- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -count=1
```
Expected: `ok`. Every Arc N1 test in the package must still pass — `TestDiscordWebhookSendsValidPayload` in particular pins the unchanged POST shape.

- [ ] **Step 8: Commit**

```bash
git add internal/notifications/discord_edit.go internal/notifications/discord_edit_test.go internal/notifications/discord.go
git commit -m "feat(notifications): Discord message create-and-edit wire

postWait posts with ?wait=true and parses the created message id;
patchMessage rewrites it through PATCH /webhooks/{id}/{token}/messages/{id}.
Both ride the one bounded delivery loop, because per the Discord API docs
POST and PATCH share the webhook's rate-limit bucket. 404/10008 surfaces as
ErrUnknownMessage (recoverable by re-posting); 404/10015 and every other 4xx
stay permanent.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/notifications/discord_edit.go internal/notifications/discord_edit_test.go internal/notifications/discord.go
```

---

## Task 3: `lifecycle.go` — the event set, the id store, the decision, the embed rewrite

**Files:**
- Create: `internal/notifications/lifecycle.go`
- Create: `internal/notifications/lifecycle_test.go`
- Modify: `internal/notifications/manager.go` — four small edits, located by symbol:
  1. `notificationTarget` gains `mode string` and `msgKey string`
  2. `buildTargets` sets both (the resolved-URL `key` it already computes feeds `msgKey`)
  3. `Manager` gains `lifecycle *lifecycleTracker` and `trackerOnce sync.Once`
  4. N1's per-target queue gains a bound `dispatch` func and its single-embed send path calls it instead of `sender.Send`/`SendOnce` — **this is the one decision point** (see Step 5; the queue holds no `*Manager`, so it is a bound closure, not a reach upwards)
- Modify: `internal/config/types.go:450-453` — `NotificationConfig` gains `Mode`. The field must land HERE, not in Task 4: `buildTargets` reads `nc.Mode` in Step 4, so without it this task's own `go build ./...` cannot pass and its commit would leave the tree broken. The validator, the API arms, both editors and the docs all stay in Task 4.
- Modify: `cmd/moombox/services.go:826-827` (add `notifyMgr.SetMessageStore(db)`), `cmd/moombox/addvideo.go:59` (same, the CLI side process opens the same DB)

**Interfaces:**
- Consumes: Task 1's `(*database.Database).NotificationMsgs` / `.UpdateNotificationMsgs`; Task 2's `postWait` / `patchMessage` / `postWaitOnce` / `patchMessageOnce` / `ErrUnknownMessage`.
- Produces:
  ```go
  // internal/notifications
  type MessageStore interface {
      NotificationMsgs(jobID string) map[string]string
      UpdateNotificationMsgs(jobID string, msgs map[string]string) bool
  }
  func (m *Manager) SetMessageStore(s MessageStore)
  func (m *Manager) dispatchOne(t notificationTarget, title, description string, color int, fields []Field, opts SendOptions, once bool) error
  func targetMsgKey(resolvedURL string) string
  func normalizeTargetMode(mode string) string   // "" | "separate" -> "separate"; "edit" -> "edit"
  const ModeSeparate = "separate"
  const ModeEdit     = "edit"
  var lifecycleEvents map[string]bool
  ```

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/lifecycle_test.go`:

```go
package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is a MessageStore backed by a map, standing in for the jobs table.
type memStore struct {
	mu      sync.Mutex
	rows    map[string]map[string]string
	writes  int
	missing map[string]bool // job ids that answer "row gone"
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]map[string]string{}, missing: map[string]bool{}}
}

func (s *memStore) NotificationMsgs(jobID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.rows[jobID]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *memStore) UpdateNotificationMsgs(jobID string, msgs map[string]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.missing[jobID] {
		return false
	}
	cp := make(map[string]string, len(msgs))
	for k, v := range msgs {
		cp[k] = v
	}
	s.rows[jobID] = cp
	return true
}

func (s *memStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// TestTargetMsgKeyIsStableAndShort: the key must be derived from the RESOLVED
// webhook URL (the dedupe key buildTargets already uses), so the two spellings
// of one webhook share one message id, and it must never be the URL itself —
// the path IS the credential and this value lands in the database.
func TestTargetMsgKeyIsStableAndShort(t *testing.T) {
	const resolved = "https://discord.com/api/webhooks/123/abcTOKEN"
	k := targetMsgKey(resolved)
	if len(k) != 16 {
		t.Errorf("key length = %d, want 16", len(k))
	}
	if k != targetMsgKey(resolved) {
		t.Error("key is not stable across calls")
	}
	if strings.Contains(k, "abcTOKEN") || strings.Contains(k, "discord") {
		t.Errorf("key %q leaks the webhook URL", k)
	}
	if targetMsgKey(resolved) == targetMsgKey(resolved+"x") {
		t.Error("two different webhooks share a key")
	}
	if targetMsgKey("") != "" {
		t.Error("an empty resolved URL must yield an empty key (lifecycle disabled)")
	}
}

// TestLifecycleEventSet pins the spec's §4.2 partition. A key in the wrong half
// either silently stops mentioning (an alert folded into an edit) or doubles a
// job's message count.
func TestLifecycleEventSet(t *testing.T) {
	want := []string{
		"found", "added", "scheduled", "rescheduled", "downloading",
		"quality_split", "gap_split", "connectivity_resume",
		"connectivity_split", "muxing", "finished",
	}
	for _, e := range want {
		if !lifecycleEvents[e] {
			t.Errorf("%q must be a lifecycle event", e)
		}
	}
	for _, e := range []string{"error", "cancelled", "auth", "trim_created", "trim_deleted", "trim_error",
		"disk_warning", "disk_critical", "update_available", "update_applied", "update_failed",
		"crash_recovered", "channel_unhealthy", "connectivity_restored", "connectivity_pause"} {
		if lifecycleEvents[e] {
			t.Errorf("%q must NOT be a lifecycle event — it is posted separately", e)
		}
	}
	if len(lifecycleEvents) != len(want) {
		t.Errorf("lifecycleEvents has %d entries, want exactly %d", len(lifecycleEvents), len(want))
	}
	// Every lifecycle event and both terminal events must render a label, or
	// the Status field would read empty for that state.
	for e := range lifecycleEvents {
		if lifecycleLabels[e] == "" {
			t.Errorf("no Status label for lifecycle event %q", e)
		}
	}
	for _, e := range []string{"error", "cancelled"} {
		if lifecycleLabels[e] == "" {
			t.Errorf("no Status label for terminal event %q", e)
		}
	}
}

// TestPlanRoutesSeparateModeUntouched: with mode unset (the default) nothing
// about delivery changes — this is what keeps every N1/N2 test green.
func TestPlanRoutesSeparateModeUntouched(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{mode: ModeSeparate, msgKey: "abc"}
	p := m.planLifecycle(tgt, SendOptions{Event: "downloading", JobID: "yt_1"})
	if p.Manage {
		t.Error("a separate-mode target must never manage a lifecycle message")
	}
}

// TestPlanNeedsJobIDAndKey: a lifecycle send with no JobID (or a transport with
// no resolved URL) is treated as separate — spec §4.4.
func TestPlanNeedsJobIDAndKey(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	for _, tc := range []struct {
		name string
		tgt  notificationTarget
		opts SendOptions
	}{
		{"no job id", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{Event: "downloading"}},
		{"no target key", notificationTarget{mode: ModeEdit}, SendOptions{Event: "downloading", JobID: "yt_1"}},
		{"non-lifecycle event", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{Event: "trim_created", JobID: "yt_1"}},
		{"no event at all (SendTest)", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{JobID: "yt_1"}},
	} {
		if p := m.planLifecycle(tc.tgt, tc.opts); p.Manage {
			t.Errorf("%s: plan managed a lifecycle message, want separate", tc.name)
		}
	}
}

// TestPlanTerminalEditsThenPosts: on error/cancelled the message is edited to
// its terminal look AND the separate embed is still posted, because the
// separate one carries the mention (owner ruling 2026-09-27).
func TestPlanTerminalEditsThenPosts(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{mode: ModeEdit, msgKey: "abc"}
	m.tracker().remember("yt_1", "abc", "555")

	for _, e := range []string{"error", "cancelled"} {
		p := m.planLifecycle(tgt, SendOptions{Event: e, JobID: "yt_1"})
		if !p.Manage || !p.AlsoSeparate || p.MessageID != "555" {
			t.Errorf("%s: plan = %+v, want Manage+AlsoSeparate on message 555", e, p)
		}
	}
	// With no message yet, a terminal event must NOT create one — it is only
	// ever a closing edit of a message some earlier event opened.
	p := m.planLifecycle(tgt, SendOptions{Event: "error", JobID: "yt_unknown"})
	if p.Manage {
		t.Error("a terminal event with no existing message must post separately only")
	}
}

// TestHistoryAppendsAndClamps: the History field grows one `<t:x:R> Label` line
// per allowed event and, at the field limit, drops the OLDEST lines. Dropping
// the newest would freeze the field at the first few states.
func TestHistoryAppendsAndClamps(t *testing.T) {
	tr := newLifecycleTracker(nil)
	at := time.Unix(1758960000, 0)
	got := tr.appendHistory("yt_1", "abc", "Downloading", at)
	if got != "<t:1758960000:R> Downloading" {
		t.Fatalf("first history line = %q", got)
	}
	got = tr.appendHistory("yt_1", "abc", "Muxing", at.Add(time.Minute))
	if got != "<t:1758960000:R> Downloading\n<t:1758960060:R> Muxing" {
		t.Fatalf("second history = %q", got)
	}

	// 200 lines is far past the 1000-rune budget; the newest must survive and
	// the oldest must be gone.
	for i := range 200 {
		got = tr.appendHistory("yt_1", "abc", "Quality split", at.Add(time.Duration(i)*time.Second))
	}
	if len([]rune(got)) > historyFieldMax {
		t.Errorf("history is %d runes, want <= %d", len([]rune(got)), historyFieldMax)
	}
	if strings.Contains(got, "Downloading") {
		t.Error("the clamp dropped from the wrong end — the first line survived")
	}
	if !strings.HasSuffix(got, "Quality split") {
		t.Errorf("the newest line is missing: %q", got)
	}
}

// TestLifecycleFieldsLeadWithStatusAndHistory: Status and History come FIRST,
// then the event's own fields.
//
// MUTANT: append them instead. N1's clampEmbed enforces Discord's 6000-char
// embed total by dropping TRAILING fields, so a fat producer would delete the
// History and then the Status — a lifecycle message with no state line, which
// is the one thing it exists to show. The oversized row below is what catches
// it: after the clamp, Status and History must both survive.
func TestLifecycleFieldsLeadWithStatusAndHistory(t *testing.T) {
	tr := newLifecycleTracker(nil)
	base := []Field{{Name: "Channel", Value: "c", Inline: true}}
	out := tr.rewriteFields("yt_1", "abc", "muxing", base, time.Unix(1758960000, 0))
	if len(out) != 3 {
		t.Fatalf("want 3 fields, got %d (%+v)", len(out), out)
	}
	if out[0].Name != statusFieldName || out[0].Value != "Muxing" {
		t.Errorf("field 0 = %+v, want {Status Muxing}", out[0])
	}
	if out[1].Name != historyFieldName || !strings.Contains(out[1].Value, "Muxing") {
		t.Errorf("field 1 = %+v, want the History", out[1])
	}
	if out[2].Name != "Channel" {
		t.Errorf("the event's own fields must follow, got %q", out[2].Name)
	}
	// The caller's slice must not be aliased — a producer reuses its builder.
	if len(base) != 1 {
		t.Errorf("rewriteFields mutated the caller's slice: %+v", base)
	}
}

// TestStatusAndHistorySurviveTheTotalClamp: an oversized producer field list
// (20 × 900-rune values, far past Discord's 6000-char embed total) must not
// cost the message its state line.
func TestStatusAndHistorySurviveTheTotalClamp(t *testing.T) {
	tr := newLifecycleTracker(nil)
	fat := make([]Field, 0, 20)
	for i := range 20 {
		fat = append(fat, Field{Name: fmt.Sprintf("Stat %d", i), Value: strings.Repeat("x", 900)})
	}
	out := tr.rewriteFields("yt_1", "abc", "downloading", fat, time.Unix(1758960000, 0))
	body, err := buildPayload("Downloading", "d", TypeDownload.Color(), out, SendOptions{Event: "downloading", JobID: "yt_1"})
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p discordPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	names := fieldNames(p)
	if len(names) < 2 || names[0] != statusFieldName || names[1] != historyFieldName {
		t.Fatalf("after the clamp the fields are %v — Status and History must lead and survive", names)
	}
}

// TestRememberWritesOncePerJobPerTarget: the ruling allows exactly one silent
// write per (job, target), on the first successful POST. A second remember for
// the same pair (or a re-read) must not write again.
func TestRememberWritesOncePerJobPerTarget(t *testing.T) {
	st := newMemStore()
	tr := newLifecycleTracker(st)

	tr.remember("yt_1", "abc", "111")
	if st.writeCount() != 1 {
		t.Fatalf("writes after first remember = %d, want 1", st.writeCount())
	}
	tr.remember("yt_1", "abc", "111")
	if st.writeCount() != 1 {
		t.Errorf("re-remembering the same id wrote again (%d writes)", st.writeCount())
	}
	// A second target for the same job is a second (job, target) pair, so it
	// does write — and it must carry BOTH keys, not replace the first.
	tr.remember("yt_1", "def", "222")
	if st.writeCount() != 2 {
		t.Errorf("writes after a second target = %d, want 2", st.writeCount())
	}
	row := st.NotificationMsgs("yt_1")
	if row["abc"] != "111" || row["def"] != "222" {
		t.Errorf("stored row = %v, want both targets", row)
	}
}

// TestLoadsFromTheStoreOnce: the row is read on the first lifecycle send for a
// job after boot and never again — a per-event SELECT on the notifier's path
// would be a database read per embed.
func TestLoadsFromTheStoreOnce(t *testing.T) {
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{"abc": "999"}
	tr := newLifecycleTracker(st)

	if id, ok := tr.messageID("yt_1", "abc"); !ok || id != "999" {
		t.Fatalf("messageID = %q,%v — the stored id was not loaded", id, ok)
	}
	// Mutate the store behind the tracker's back: a second lookup must NOT
	// re-read it.
	st.rows["yt_1"] = map[string]string{"abc": "different"}
	if id, _ := tr.messageID("yt_1", "abc"); id != "999" {
		t.Errorf("messageID re-read the store: got %q", id)
	}
}

// TestRecordSurvivesMissingRow (Review Focus 1): the job can be deleted between
// the POST and its response. remember must keep the id in memory (so the edits
// still work for the rest of this process) and log rather than fail.
func TestRecordSurvivesMissingRow(t *testing.T) {
	st := newMemStore()
	st.missing["yt_gone"] = true
	tr := newLifecycleTracker(st)

	tr.remember("yt_gone", "abc", "111")
	if id, ok := tr.messageID("yt_gone", "abc"); !ok || id != "111" {
		t.Errorf("in-memory id lost when the row write failed: %q,%v", id, ok)
	}
}

// TestTrackerReleasesFinishedJobs (Review Focus 6): the tracker must not be a
// map that only grows. A delivered terminal edit drops the job's entry; the
// STORED id is untouched, so a Retry reloads it once and keeps editing the
// same message — which is exactly what the owner's "never closed" ruling asks
// for. Releasing the cache is not closing the entry.
func TestTrackerReleasesFinishedJobs(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M0"))
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	for _, e := range []string{"downloading", "finished"} {
		if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: e, JobID: "yt_1"}, false); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracked jobs after finished = %d, want 0", n)
	}
	// The id survived in the store, so a Retry edits the SAME message.
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "M0" {
		t.Fatalf("stored id = %q, want M0 — release must not clear the row", got)
	}
	if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: "downloading", JobID: "yt_1"}, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	last := f.calls()[len(f.calls())-1]
	if last.Method != http.MethodPatch || !strings.HasSuffix(last.Path, "/messages/M0") {
		t.Errorf("the retry did not resume the same message: %s %s", last.Method, last.Path)
	}
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want 1 — a reload must not re-write", st.writeCount())
	}
}

// TestTrackerCapsTrackedJobs: jobs that never reach a terminal event (cancelled
// outside the notifier, deleted, filtered to mid-lifecycle keys) must not
// retain an entry each, forever, in a process that runs for months.
func TestTrackerCapsTrackedJobs(t *testing.T) {
	tr := newLifecycleTracker(newMemStore())
	for i := range maxTrackedJobs + 100 {
		tr.remember(fmt.Sprintf("yt_%d", i), "abc", "1")
	}
	if n := tr.trackedJobs(); n > maxTrackedJobs {
		t.Errorf("tracked jobs = %d, want <= %d", n, maxTrackedJobs)
	}
	// The newest must have survived the eviction, the oldest must not.
	if _, ok := tr.messageID(fmt.Sprintf("yt_%d", maxTrackedJobs+99), "abc"); !ok {
		t.Error("the most recently touched job was evicted")
	}
}

// TestNonEditableTransportFallsBack: a target in edit mode whose transport
// cannot create-and-rewrite must PLAIN POST, not drop the event. There is no
// such transport today (Discord is the only one), which is exactly why the
// branch needs a test rather than a reader's confidence.
func TestNonEditableTransportFallsBack(t *testing.T) {
	var got int
	plain := senderFunc(func(string, string, int, []Field, SendOptions) error {
		got++
		return nil
	})
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{sender: plain, mode: ModeEdit, msgKey: "abc"}

	if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: "downloading", JobID: "yt_1"}, false); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	if got != 1 {
		t.Errorf("plain sends = %d, want 1 — a non-editable transport must fall back, not drop", got)
	}
	if _, ok := m.tracker().messageID("yt_1", "abc"); ok {
		t.Error("a fallback post must not record a message id")
	}
}

// TestPatch404RePostFailureForgetsTheID: when the recovery POST itself fails,
// the error must reach the caller AND the stale id must stay forgotten, so the
// job's next event creates a message instead of PATCHing a ghost forever.
func TestPatch404RePostFailureForgetsTheID(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		rw.WriteHeader(http.StatusForbidden) // the re-POST is refused too
		io.WriteString(rw, `{"message":"Missing Permissions","code":50013}`)
	})
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "STALE"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: "muxing", JobID: "yt_1"}, false); err == nil {
		t.Fatal("a refused re-POST was reported as success")
	}
	if id, ok := m.tracker().messageID("yt_1", targetMsgKey(f.URL())); ok {
		t.Errorf("the stale id %q is still held — the next event would PATCH a ghost", id)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "STALE" {
		t.Errorf("stored id = %q — a failed re-POST must not rewrite the row", got)
	}
}

// TestShutdownEditIsSingleAttempt: after the queue reports it is shutting down,
// an edit-mode lifecycle event makes exactly ONE request even against a 502 —
// the owner's 10 s force-exit cap must not be spent on a retry ladder.
func TestShutdownEditIsSingleAttempt(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusBadGateway)
	})
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: "finished", JobID: "yt_1"}, true); err == nil {
		t.Fatal("a 502 was reported as success")
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("requests while shutting down = %d, want exactly 1", n)
	}
}

// TestTrackerConcurrentAccess (Review Focus 5): two jobs and two targets touch
// the tracker from different goroutines while a third reads. Run with -race.
func TestTrackerConcurrentAccess(t *testing.T) {
	tr := newLifecycleTracker(newMemStore())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job := "yt_" + string(rune('a'+i%2))
			key := "k" + string(rune('a'+i%2))
			for range 50 {
				tr.remember(job, key, "1")
				tr.messageID(job, key)
				tr.appendHistory(job, key, "Downloading", time.Unix(1, 0))
				tr.forget(job, key)
			}
		}(i)
	}
	wg.Wait()
}

// TestPatch404RePostsAndOverwrites (Review Focus 2): a stored id Discord does
// not know must produce exactly one PATCH, then one POST, then a stored id
// equal to the new message — and no loop.
func TestPatch404RePostsAndOverwrites(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"id":"NEW777"}`)
	})

	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "STALE111"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, "Muxing Starting", "d", TypeMuxing.Color(), nil,
		SendOptions{Event: "muxing", JobID: "yt_1"}, false); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("want PATCH then POST (2 calls), got %d: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodPatch || calls[1].Method != http.MethodPost {
		t.Fatalf("call order = %s,%s — want PATCH,POST", calls[0].Method, calls[1].Method)
	}
	if calls[1].Query != "wait=true" {
		t.Errorf("the re-post did not ask for the message back: query %q", calls[1].Query)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "NEW777" {
		t.Errorf("stored id = %q, want NEW777", got)
	}
}

// TestFirstAllowedEventPostsAndStores: the creating POST carries Status and
// History already, so an operator reading the first message sees the same
// shape every later edit keeps.
func TestFirstAllowedEventPostsAndStores(t *testing.T) {
	f := newFakeDiscord(t, okCreated("ID1"))
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, "Stream Found", "d", TypeInfo.Color(),
		[]Field{{Name: "Channel", Value: "c"}},
		SendOptions{Event: "found", JobID: "yt_1"}, false); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "wait=true" {
		t.Fatalf("first allowed event did not POST with wait=true: %+v", calls)
	}
	names := fieldNames(calls[0].Body)
	if len(names) != 3 || names[0] != "Status" || names[1] != "History" {
		t.Errorf("created embed fields = %v, want [Status History Channel]", names)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "ID1" {
		t.Errorf("stored id = %q, want ID1", got)
	}
}

// fieldNames pulls the field names out of the first embed of a captured body.
func fieldNames(p discordPayload) []string {
	if len(p.Embeds) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Embeds[0].Fields))
	for _, f := range p.Embeds[0].Fields {
		out = append(out, f.Name)
	}
	return out
}

// TestNoStoreStillEdits: an install without a MessageStore (the `moombox add`
// side process before its wiring, or a test literal) must still edit within the
// process — only the restart survival is lost.
func TestNoStoreStillEdits(t *testing.T) {
	f := newFakeDiscord(t, okCreated("ID9"))
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	for _, e := range []string{"downloading", "muxing", "finished"} {
		if err := m.dispatchOne(tgt, "t", "d", 0, nil, SendOptions{Event: e, JobID: "yt_1"}, false); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPost || calls[1].Method != http.MethodPatch || calls[2].Method != http.MethodPatch {
		t.Errorf("methods = %s,%s,%s — want POST,PATCH,PATCH", calls[0].Method, calls[1].Method, calls[2].Method)
	}
}
```

The import block of this file is exactly: `encoding/json`, `fmt`, `io`, `net/http`, `strings`, `sync`, `testing`, `time`. The tests here drive `m.dispatchOne` directly rather than `m.Send`, so no target sender goroutine has to be running — that is what Task 6 covers.
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -run 'TestTargetMsgKey|TestLifecycle|TestStatusAndHistory|TestPlan|TestHistory|TestRemember|TestLoadsFromTheStore|TestRecordSurvives|TestTracker|TestNonEditableTransport|TestPatch404|TestShutdownEdit|TestFirstAllowedEvent|TestNoStoreStillEdits' -count=1
```
Expected: FAIL to compile — `undefined: targetMsgKey`, `undefined: lifecycleEvents`, `m.planLifecycle undefined`, `m.tracker undefined`, `m.SetMessageStore undefined`, `m.dispatchOne undefined`, `undefined: newLifecycleTracker`, `undefined: historyFieldMax`, `undefined: maxTrackedJobs`, `unknown field mode in struct literal of type notificationTarget`.

- [ ] **Step 3: Write `lifecycle.go`**

Create `internal/notifications/lifecycle.go`:

```go
package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Per-target delivery modes (the `mode` key on a [[notifications]] entry).
const (
	// ModeSeparate is the default: one message per event, never edited.
	ModeSeparate = "separate"
	// ModeEdit gives a job ONE message per target, created by its first
	// allowed lifecycle event and rewritten by every later one.
	ModeEdit = "edit"
)

// lifecycleEvents is the set that shares a job's one edited message (spec
// §4.2). Everything else — error, cancelled, auth, trim_* and all of System —
// is always its own post: those may carry a mention, and an edit does not
// re-notify anyone (per the Discord API docs an edit returns the updated
// message object and says nothing about re-notifying).
var lifecycleEvents = map[string]bool{
	"found":               true,
	"added":               true,
	"scheduled":           true,
	"rescheduled":         true,
	"downloading":         true,
	"quality_split":       true,
	"gap_split":           true,
	"connectivity_resume": true,
	"connectivity_split":  true,
	"muxing":              true,
	"finished":            true,
}

// terminalLifecycleEvents close a job's story. By owner ruling (2026-09-27)
// they do BOTH: edit the lifecycle message to its terminal look, then post the
// separate embed — which is the one that carries the mention. They never
// CREATE a lifecycle message; with no message open they are a plain post.
var terminalLifecycleEvents = map[string]bool{
	"error":     true,
	"cancelled": true,
}

// lifecycleLabels is the word an event puts in the Status field and in its
// History line. Short and neutral on purpose: the line is read in a column
// beside a relative timestamp.
var lifecycleLabels = map[string]string{
	"found":               "Found",
	"added":               "Added",
	"scheduled":           "Scheduled",
	"rescheduled":         "Rescheduled",
	"downloading":         "Downloading",
	"quality_split":       "Quality split",
	"gap_split":           "Gap split",
	"connectivity_resume": "Resumed",
	"connectivity_split":  "Connectivity split",
	"muxing":              "Muxing",
	"finished":            "Finished",
	"error":               "Failed",
	"cancelled":           "Cancelled",
}

const (
	// historyFieldMax bounds the History field value. Discord's own field
	// limit is 1024 (per the API docs, message.mdx) and buildPayload clamps
	// there; this sits under it so the clamp that bites is OURS — dropping
	// whole oldest lines — rather than a mid-line truncation.
	historyFieldMax = 1000
	// statusFieldName / historyFieldName are the two fields the rewrite adds.
	statusFieldName  = "Status"
	historyFieldName = "History"
)

// MessageStore persists a job's per-target lifecycle message ids. Implemented
// by *database.Database (NotificationMsgs / UpdateNotificationMsgs).
//
// An interface rather than the concrete type because internal/notifications
// imports no database package — and because the tests need a store they can
// pre-populate to stand in for "a restart with ids already on the row".
type MessageStore interface {
	// NotificationMsgs returns the stored target-key -> message-id map for a
	// job, or nil when there is none.
	NotificationMsgs(jobID string) map[string]string
	// UpdateNotificationMsgs replaces the stored map. Returns false when the
	// job row is gone.
	UpdateNotificationMsgs(jobID string, msgs map[string]string) bool
}

// targetMsgKey is the stable per-target key the persisted map is keyed on: the
// first 16 hex digits of SHA-256 over the RESOLVED webhook URL — the same
// value buildTargets dedupes targets on, so the two spellings of one webhook
// share one message.
//
// Hashed, not stored raw: the webhook path IS the credential, and this value
// lands in the database and in every backup of it. 64 bits is far past enough
// to separate the handful of webhooks one install configures.
func targetMsgKey(resolvedURL string) string {
	if resolvedURL == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(resolvedURL))
	return hex.EncodeToString(sum[:])[:16]
}

// normalizeTargetMode maps a configured mode onto the two the manager knows.
// Empty is the default; anything unrecognised is treated as the default too —
// config validation has already reported it, and a typo must not silently turn
// edit mode ON.
//
// Exact match, NOT case-insensitive: validateOrNormalize rejects "Edit" and
// rewrites it to "separate", so accepting it here would make the manager
// disagree with the value the config layer says is stored.
func normalizeTargetMode(mode string) string {
	if strings.TrimSpace(mode) == ModeEdit {
		return ModeEdit
	}
	return ModeSeparate
}

// lifecycleTracker holds the per-(job, target) message ids and history lines.
//
// Ids are loaded from the store the first time a job is touched after boot and
// written back on the first successful POST for a pair — one silent write per
// (job, target), which is the whole budget the ruling allows.
//
// History is in-memory ONLY and per (job, target): a target with an event
// filter must see a history of what IT received, not of what the job did. It
// therefore restarts empty after a reboot — the message keeps being edited
// (that is what the persisted id buys) but its History begins again at the
// first post-restart state. Persisting the lines would need a second column
// and a write per event, which the cadence ruling does not allow.
// The map is bounded: entries are released when a job's story ends (see
// release) and, for jobs that never reach a terminal event, evicted
// least-recently-touched first past maxTrackedJobs.
type lifecycleTracker struct {
	mu      sync.Mutex
	store   MessageStore
	jobs    map[string]*lifecycleJob
	touched map[string]uint64 // job id -> last-touch sequence, for eviction
	seq     uint64
	log     interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

type lifecycleJob struct {
	loaded  bool
	msgs    map[string]string   // target key -> message id
	history map[string][]string // target key -> rendered history lines
}

// maxTrackedJobs is the backstop for a job that never reaches a terminal
// event. Past it the tracker drops its least-recently-touched entries; each
// one costs a single store read to rebuild, and the message it was editing is
// unaffected because the id is on the row.
const maxTrackedJobs = 512

func newLifecycleTracker(store MessageStore) *lifecycleTracker {
	return &lifecycleTracker{
		store:   store,
		jobs:    map[string]*lifecycleJob{},
		touched: map[string]uint64{},
	}
}

// setStore attaches (or replaces) the persistence backend. Called by
// Manager.SetMessageStore, so wiring order at boot never matters.
func (l *lifecycleTracker) setStore(s MessageStore) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = s
}

// jobLocked returns the job's state, loading the persisted ids on first touch.
// Caller holds l.mu.
func (l *lifecycleTracker) jobLocked(jobID string) *lifecycleJob {
	l.seq++
	l.touched[jobID] = l.seq
	j := l.jobs[jobID]
	if j == nil {
		j = &lifecycleJob{msgs: map[string]string{}, history: map[string][]string{}}
		l.jobs[jobID] = j
		l.evictLocked()
	}
	if !j.loaded {
		j.loaded = true
		if l.store != nil {
			for k, v := range l.store.NotificationMsgs(jobID) {
				if k != "" && v != "" {
					j.msgs[k] = v
				}
			}
		}
	}
	return j
}

// messageID returns the message this target edits for this job.
func (l *lifecycleTracker) messageID(jobID, key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.jobLocked(jobID).msgs[key]
	return id, id != ""
}

// remember records a newly created message and persists the job's whole map.
// A no-op when the id is already what we hold, so the ruling's "one write per
// job per target, on the first successful POST" holds even if a caller repeats.
//
// A failed write (the row was deleted mid-download) keeps the in-memory id:
// edits still work for the rest of this process, which is all a deleted job
// could want.
func (l *lifecycleTracker) remember(jobID, key, messageID string) {
	if jobID == "" || key == "" || messageID == "" {
		return
	}
	l.mu.Lock()
	j := l.jobLocked(jobID)
	if j.msgs[key] == messageID {
		l.mu.Unlock()
		return
	}
	j.msgs[key] = messageID
	snapshot := make(map[string]string, len(j.msgs))
	for k, v := range j.msgs {
		snapshot[k] = v
	}
	store := l.store
	l.mu.Unlock()

	if store == nil {
		return
	}
	if !store.UpdateNotificationMsgs(jobID, snapshot) && l.log != nil {
		l.log.Debug("notification message id not persisted — job row is gone",
			"jobID", jobID)
	}
}

// forget drops a target's id for a job (a 404 before the re-post). It does NOT
// write: the re-post's remember writes the replacement a moment later, and a
// clearing write in between would double the budget for nothing.
func (l *lifecycleTracker) forget(jobID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.jobLocked(jobID).msgs, key)
}

// release drops a job's whole in-memory entry once its story is told — after a
// delivered `finished`, `error` or `cancelled` edit.
//
// The PERSISTED id is untouched, so this is a cache eviction, not a close: a
// Retry (or another target still mid-job) re-reads the row once and keeps
// editing the SAME message, exactly as the owner's ruling requires. Only the
// in-process History restarts, which is already what happens across a restart.
//
// Without it the tracker is a map that only ever grows — one entry per job an
// edit-mode target ever touched, each holding a msgs map and a ~1000-rune
// History per target, for the life of a 24/7 process.
func (l *lifecycleTracker) release(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.jobs, jobID)
	delete(l.touched, jobID)
}

// trackedJobs is how many jobs the cache is holding. Test-only reader for the
// eviction guarantees; nothing in the program calls it, and that is the point.
func (l *lifecycleTracker) trackedJobs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.jobs)
}

// evictLocked bounds the map for jobs that never reach a terminal event —
// cancelled outside the notifier, deleted, or filtered down to mid-lifecycle
// keys only. Drops the least-recently-touched entries; each costs one store
// read to rebuild. Caller holds l.mu.
func (l *lifecycleTracker) evictLocked() {
	for len(l.jobs) > maxTrackedJobs {
		oldestID, oldest := "", uint64(0)
		for id, seq := range l.touched {
			if oldestID == "" || seq < oldest {
				oldestID, oldest = id, seq
			}
		}
		if oldestID == "" {
			return
		}
		delete(l.jobs, oldestID)
		delete(l.touched, oldestID)
	}
}

// appendHistory adds one `<t:unix:R> Label` line and returns the whole field
// value, clamped to historyFieldMax runes by dropping the OLDEST lines. The
// newest state is the one an operator is reading for.
func (l *lifecycleTracker) appendHistory(jobID, key, label string, at time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	j := l.jobLocked(jobID)
	lines := append(j.history[key], fmt.Sprintf("<t:%d:R> %s", at.Unix(), label))
	for len(lines) > 1 && len([]rune(strings.Join(lines, "\n"))) > historyFieldMax {
		lines = lines[1:]
	}
	j.history[key] = lines
	return strings.Join(lines, "\n")
}

// rewriteFields returns Status and History FOLLOWED BY the event's own fields.
//
// The order is load-bearing, not cosmetic. N1's clampEmbed enforces Discord's
// 6000-character embed total by dropping TRAILING fields first, so a fat
// producer (a mux error, a long format-selection line, a dozen segment stats)
// would throw the History away and then the Status — leaving a lifecycle
// message with no state line at all, which is the one thing it exists to show.
// First position makes them the last two fields a clamp could ever reach.
//
// The caller's slice is never aliased — producers reuse their FieldBuilder.
func (l *lifecycleTracker) rewriteFields(jobID, key, event string, fields []Field, at time.Time) []Field {
	label := lifecycleLabels[event]
	if label == "" {
		label = event
	}
	out := make([]Field, 0, len(fields)+2)
	out = append(out, Field{Name: statusFieldName, Value: label, Inline: true})
	out = append(out, Field{Name: historyFieldName, Value: l.appendHistory(jobID, key, label, at)})
	out = append(out, fields...)
	return out
}

// lifecyclePlan is the ONE decision: post separately, create the job's message,
// or edit it — and, for a terminal event, do both.
type lifecyclePlan struct {
	// Manage is false for every ordinary send; the sender then behaves exactly
	// as it did before this arc existed.
	Manage bool
	// MessageID is the message to PATCH. Empty means "create it".
	MessageID string
	// AlsoSeparate marks a terminal event: edit the lifecycle message, then
	// post the separate embed too (it carries the mention).
	AlsoSeparate bool
}

// planLifecycle answers the POST-or-PATCH question for one queued send.
func (m *Manager) planLifecycle(t notificationTarget, opts SendOptions) lifecyclePlan {
	if t.mode != ModeEdit || t.msgKey == "" || opts.JobID == "" || opts.Event == "" {
		return lifecyclePlan{}
	}
	if terminalLifecycleEvents[opts.Event] {
		// Close an OPEN message; never open one. A job whose first word to
		// this target is "failed" has no story to rewrite.
		if id, ok := m.tracker().messageID(opts.JobID, t.msgKey); ok {
			return lifecyclePlan{Manage: true, MessageID: id, AlsoSeparate: true}
		}
		return lifecyclePlan{}
	}
	if !lifecycleEvents[opts.Event] {
		return lifecyclePlan{}
	}
	id, _ := m.tracker().messageID(opts.JobID, t.msgKey)
	return lifecyclePlan{Manage: true, MessageID: id}
}

// dispatchOne is the single decision point between the per-target FIFO sender
// and the wire: post as usual, create the job's lifecycle message, or edit it.
//
// Named dispatchOne, not deliver: after the merge there is already a
// (*targetQueue).deliver above it and a (*DiscordWebhook).deliver below it, and
// a three-deep `q.deliver -> m.deliver -> d.deliver` chain is a reading trap.
//
// It runs ON the per-target sender goroutine, so a job's states can never
// reorder and a PATCH can never overtake the POST that created its message.
// once is the queue's shutting-down flag: when set, every request on this path
// is single-attempt, because the owner's 10 s force-exit cap must not be spent
// on one lifecycle edit's retry ladder.
func (m *Manager) dispatchOne(t notificationTarget, title, description string, color int, fields []Field, opts SendOptions, once bool) error {
	plan := m.planLifecycle(t, opts)
	edit, editable := t.sender.(editableSender)
	if !plan.Manage || !editable {
		// A transport that cannot edit falls back to a plain post rather than
		// dropping the event.
		return sendPlain(t.sender, title, description, color, fields, opts, once)
	}

	tr := m.tracker()
	rewritten := tr.rewriteFields(opts.JobID, t.msgKey, opts.Event, fields, time.Now())
	body, err := buildPayload(title, description, color, rewritten, opts)
	if err != nil {
		return err
	}

	lifecycleErr := m.postOrPatch(edit, tr, opts.JobID, t.msgKey, opts.Event, plan, body, once)

	if plan.AlsoSeparate {
		// The separate embed carries the mention and must go out even if the
		// closing edit failed (owner ruling: two messages on failure).
		if sepErr := sendPlain(t.sender, title, description, color, fields, opts, once); sepErr != nil {
			return errors.Join(lifecycleErr, sepErr)
		}
	}
	// A delivered terminal edit ends this job's story for this target: drop the
	// in-memory entry (the PERSISTED id stays, so a Retry reloads it once and
	// keeps editing the same message).
	if lifecycleErr == nil && (plan.AlsoSeparate || opts.Event == "finished") {
		tr.release(opts.JobID)
	}
	return lifecycleErr
}

// sendPlain posts an ordinary embed, honouring the shutdown single-attempt
// rule. N1 put SendOnce on the sender interface for exactly this; if your
// merged tree spells it differently, use its spelling.
func sendPlain(s sender, title, description string, color int, fields []Field, opts SendOptions, once bool) error {
	if once {
		return s.SendOnce(title, description, color, fields, opts)
	}
	return s.Send(title, description, color, fields, opts)
}

// postOrPatch performs the wire half of dispatchOne, including the one
// recovery this path has: a PATCH that Discord answers "Unknown Message"
// becomes a fresh POST whose id overwrites the stored one.
func (m *Manager) postOrPatch(edit editableSender, tr *lifecycleTracker, jobID, msgKey, event string, plan lifecyclePlan, body []byte, once bool) error {
	if plan.MessageID != "" {
		var err error
		if once {
			err = edit.patchMessageOnce(plan.MessageID, body)
		} else {
			err = edit.patchMessage(plan.MessageID, body)
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrUnknownMessage) {
			return err
		}
		m.logger.Info("lifecycle message is gone — posting a new one",
			"jobID", jobID, "event", event)
		tr.forget(jobID, msgKey)
		// fall through to the create path
	}
	var (
		id  string
		err error
	)
	if once {
		id, err = edit.postWaitOnce(body)
	} else {
		id, err = edit.postWait(body)
	}
	if err != nil {
		// A re-POST that itself fails leaves the id already forgotten, so the
		// job's next event creates a message rather than PATCHing a ghost.
		return err
	}
	tr.remember(jobID, msgKey, id)
	return nil
}

// editableSender is the optional capability a transport advertises when it can
// create a message it is able to rewrite later. Only *DiscordWebhook
// implements it; anything else falls back to separate posts.
type editableSender interface {
	postWait(body []byte) (string, error)
	patchMessage(messageID string, body []byte) error
	postWaitOnce(body []byte) (string, error)
	patchMessageOnce(messageID string, body []byte) error
}
```

- [ ] **Step 4: Add the config field, then wire the tracker and the target fields into `manager.go`**

First, `internal/config/types.go`, in `NotificationConfig` (after N2b's fields, keeping `url`/`events` first) — `buildTargets` below reads it, so it cannot wait for Task 4:
```go
	// Mode is how this target delivers a job's lifecycle events: "separate"
	// (the default — one message per event, never edited) or "edit" (one
	// message per job, created by the first allowed lifecycle event and
	// rewritten by every later one). Empty means "separate".
	//
	// Opt-in per target by owner ruling (2026-09-27): filters, docs and both
	// UIs are built around one-event-one-embed, so edit mode is something an
	// operator asks for, never something they are given.
	Mode string `toml:"mode,omitempty" json:"mode,omitempty"`
```

Then locate `type Manager struct` and add two fields beside `logger`:
```go
	// lifecycle holds the per-(job, target) message ids edit-mode targets
	// rewrite. Created lazily by tracker() so the package's bare &Manager{...}
	// test literals need no extra field.
	lifecycle   *lifecycleTracker
	trackerOnce sync.Once
```

Append to `manager.go` (or keep in `lifecycle.go` — either is fine; `lifecycle.go` is preferred so `manager.go`'s diff stays at the struct fields and the one call):
```go
// tracker returns the lifecycle tracker, creating it on first use.
func (m *Manager) tracker() *lifecycleTracker {
	m.trackerOnce.Do(func() {
		if m.lifecycle == nil {
			m.lifecycle = newLifecycleTracker(nil)
		}
		m.lifecycle.log = m.logger
	})
	return m.lifecycle
}

// SetMessageStore attaches the persistence backend for edit-mode message ids.
// Wired once at boot (cmd/moombox) with the live *database.Database. Without
// it edit mode still works within a process; only restart survival is lost.
func (m *Manager) SetMessageStore(s MessageStore) {
	m.tracker().setStore(s)
}
```

Locate `type notificationTarget struct` and add:
```go
	// mode is "separate" (default) or "edit" — see lifecycle.go.
	mode string
	// msgKey is targetMsgKey(resolved webhook URL): the stable key this
	// target's lifecycle message ids are stored under. Empty for a transport
	// with no resolved URL, which disables edit mode for it.
	msgKey string
```

Locate `buildTargets`. It already computes `key` (the resolved webhook URL) for dedupe. In the `targets = append(targets, notificationTarget{...})` literal, add:
```go
			mode:   normalizeTargetMode(nc.Mode),
			msgKey: targetMsgKey(key),
```
In the duplicate-collapse branch (the `if idx, dup := seen[key]; dup` arm), leave `mode` alone: the FIRST occurrence wins, matching the existing rule that the first occurrence's sender and slot survive. Add one comment line there saying so.

- [ ] **Step 5: Route the per-target queue through `dispatchOne`**

N1's queue (`targetQueue`) holds `sender` / `logger` / `shuttingDown` — **not** a `*Manager` and not the `notificationTarget`. So give it a bound function instead of reaching upwards. Three edits:

1. `targetQueue` gains one field:
   ```go
   	// dispatch is the ONE decision point for a single-embed item: an
   	// edit-mode target's lifecycle event is created or rewritten here;
   	// everything else posts exactly as it did before edit mode existed.
   	// Bound per target in applyTargets so the queue needs no Manager.
   	dispatch func(title, description string, color int, fields []Field, opts SendOptions, once bool) error
   ```
2. Where `applyTargets` builds each queue, bind it to the target:
   ```go
   	tgt := built[i]
   	q.dispatch = func(title, description string, color int, fields []Field, opts SendOptions, once bool) error {
   		return m.dispatchOne(tgt, title, description, color, fields, opts, once)
   	}
   ```
   (`tgt` is captured per iteration — Go 1.22+ loop semantics make the range variable safe, but the explicit copy documents it.)
3. In the queue's single-embed send path, replace the direct `sender` call — both the ordinary and the shutting-down arm collapse into one:
   ```go
   -	if q.shuttingDown {
   -		err = q.sender.SendOnce(it.title, it.description, it.color, it.fields, it.opts)
   -	} else {
   -		err = q.sender.Send(it.title, it.description, it.color, it.fields, it.opts)
   -	}
   +	err = q.dispatch(it.title, it.description, it.color, it.fields, it.opts, q.shuttingDown)
   ```

Leave the **batch** path alone — a batched message is several embeds in one body and by ruling never belongs to an edit-mode target.

**Adaptation point:** the names `targetQueue`, `applyTargets`, `shuttingDown`, `SendOnce` and the item field names come from N1. Use whatever the merged tree calls them; what must hold is (a) exactly one place decides POST-or-PATCH, (b) it runs on the per-target goroutine, and (c) the shutting-down flag reaches it.

- [ ] **Step 6: Wire the store in `cmd/moombox`**

`cmd/moombox/services.go:826-827`:
```go
	notifyMgr := notifications.NewManager(cfg, log)
	// Edit-mode targets keep one Discord message per job and need its id to
	// survive a restart; the ids live on the job row (schema 20).
	notifyMgr.SetMessageStore(db)
	s.notifyMgr = notifyMgr
```

`cmd/moombox/addvideo.go:59` — the `moombox add` side process sends `added`, which is a lifecycle event, against the same database:
```go
	notifyMgr := notifications.NewManager(cfg, &nopLogger{})
	notifyMgr.SetMessageStore(db)
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -count=1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -run TestTrackerConcurrentAccess -race -count=1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```
Expected: `ok` for both test runs, no build output.

- [ ] **Step 8: Commit**

```bash
git add internal/notifications/lifecycle.go internal/notifications/lifecycle_test.go internal/notifications/manager.go internal/config/types.go cmd/moombox/services.go cmd/moombox/addvideo.go
git commit -m "feat(notifications): edit-in-place lifecycle messages

lifecycle.go holds the whole model: the eleven lifecycle events, the
per-(job, target) id store loaded from the job row on first touch and
written once on the first successful POST, the POST-or-PATCH decision, the
Status/History embed rewrite with an oldest-first clamp, and the
Unknown-Message re-post. The per-target FIFO sender gains exactly one
changed line, so a job's states can never reorder and a PATCH can never
overtake the POST that created its message. error/cancelled edit the
message to its terminal look AND post the separate embed, per the ruling.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/notifications/lifecycle.go internal/notifications/lifecycle_test.go internal/notifications/manager.go internal/config/types.go cmd/moombox/services.go cmd/moombox/addvideo.go
```

---

## Task 4: The per-target `mode` config key and the batching exclusion

**Files:**
- Verify only (the field landed in Task 3): `internal/config/types.go:450-453` (`NotificationConfig.Mode`)
- Modify: `internal/config/config.go` — `validateOrNormalize` (`:532`), a new arm after the existing `cfg.Network.*` arms
- Modify: `internal/web/routes/config_routes.go:121` (`validateConfigUpdates` — a notifications arm) and `:750-768` (`applyConfigUpdates` — read `mode`)
- Modify: `config.example.toml:319-322`
- Modify: `internal/notifications/manager.go` — the batching coalescer Arc N2b added: one guard
- Create: `internal/config/notification_mode_test.go`
- Create: `internal/web/routes/config_notification_mode_test.go`
- Create: `internal/notifications/lifecycle_batching_test.go`

**Interfaces:**
- Consumes: Task 3's `normalizeTargetMode`, `ModeEdit`, `ModeSeparate`.
- Produces:
  ```go
  // internal/config
  type NotificationConfig struct {
      URL    string   `toml:"url,omitempty" json:"url,omitempty"`
      Events []string `toml:"events,omitempty" json:"events,omitempty"`
      // ... N2b's enabled / mention / mention_events ...
      Mode   string   `toml:"mode,omitempty" json:"mode,omitempty"`
  }
  ```

**`moombox-settings` checklist mapping** (the skill's 9 steps, applied to `mode`): 1 config struct → Step 3; 2 default → none needed, `""` *is* the default and `Defaults()` seeds no notification entries; 3 `validateOrNormalize` → Step 4; 4 `validateConfigUpdates` → Step 5; 5 `applyConfigUpdates` → Step 5; 6 Web UI → Task 5; 7 TUI → Task 5; 8 hot-reload → already covered, `OnNotificationsChange` → `Manager.Reload` rebuilds targets and therefore `mode`, so **`mode` is NOT a restart-required field** and must not appear in `RESTART_REQUIRED_FIELDS` or `restartRequiredKeys`; 8b config-file-only → not applicable, it is editable in both UIs; 9 migration → none, the key is new. The skill's own text lists no notification fields, so `.claude/skills/moombox-settings/SKILL.md` needs no edit (verified: `grep -n "notification" .claude/skills/moombox-settings/SKILL.md` matches only the `OnNotificationsChange` hot-reload row, which stays true).

- [ ] **Step 1: Write the failing config test**

Create `internal/config/notification_mode_test.go`:

```go
package config

import "testing"

// TestNotificationModeValidation: "edit" and "separate" are the vocabulary;
// empty means separate. Anything else is normalised back to separate, because
// a typo must never silently switch a channel to one-message-per-job.
func TestNotificationModeValidation(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
		want    string
	}{
		{"", false, ""},
		{"separate", false, "separate"},
		{"edit", false, "edit"},
		{"Edit", true, "separate"},
		{"editing", true, "separate"},
		{"none", true, "separate"},
	} {
		cfg := Defaults()
		cfg.Notifications = []NotificationConfig{{URL: "discord://1/a", Mode: tc.in}}
		errs := Validate(cfg)
		var found bool
		for _, e := range errs {
			if containsMode(e.Error()) {
				found = true
			}
		}
		if found != tc.wantErr {
			t.Errorf("mode %q: reported error = %v, want %v (errs=%v)", tc.in, found, tc.wantErr, errs)
		}

		norm := Defaults()
		norm.Notifications = []NotificationConfig{{URL: "discord://1/a", Mode: tc.in}}
		Normalize(norm)
		if norm.Notifications[0].Mode != tc.want {
			t.Errorf("mode %q normalised to %q, want %q", tc.in, norm.Notifications[0].Mode, tc.want)
		}
	}
}

func containsMode(s string) bool {
	for i := 0; i+len("notifications[0].mode") <= len(s); i++ {
		if s[i:i+len("notifications[0].mode")] == "notifications[0].mode" {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/ -run TestNotificationModeValidation -count=1
```
Expected: FAIL to compile — `unknown field Mode in struct literal of type NotificationConfig`.

- [ ] **Step 3: (the field landed in Task 3 — verify it, do not re-add)**

```bash
grep -n "Mode " internal/config/types.go
```
Expected: the `Mode string \`toml:"mode,omitempty" json:"mode,omitempty"\`` line inside `NotificationConfig`, added by Task 3 Step 4 because `buildTargets` reads it. If it is missing, Task 3 was not completed — add it there, not here.

- [ ] **Step 4: Validate and normalise it**

`internal/config/config.go`, inside `validateOrNormalize`, after the `Connectivity` arms and before the final `return errs` (place it beside whatever notification arms N2b added):
```go
	for i := range cfg.Notifications {
		switch strings.TrimSpace(cfg.Notifications[i].Mode) {
		case "", "separate", "edit":
			// Canonicalise the empty spelling away so every reader sees one
			// value for "the default".
			if cfg.Notifications[i].Mode != "" {
				cfg.Notifications[i].Mode = strings.TrimSpace(cfg.Notifications[i].Mode)
			}
		default:
			fail("notifications[%d].mode %q must be \"separate\" or \"edit\"", i, cfg.Notifications[i].Mode)
			if !reportOnly {
				cfg.Notifications[i].Mode = "separate"
			}
		}
	}
```

- [ ] **Step 5: Validate and apply it on the API**

`internal/web/routes/config_routes.go`, in `validateConfigUpdates`, after the connectivity arm:
```go
	// Notifications: the per-target delivery mode. Matches the config-side
	// constraint in validateOrNormalize — a value the file loader would refuse
	// must not be reachable through a PUT either.
	if notifs, ok := updates["notifications"].([]any); ok {
		for i, n := range notifs {
			nm, ok := n.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := nm["mode"].(string); ok {
				switch v {
				case "", "separate", "edit":
				default:
					errs[fmt.Sprintf("notifications[%d].mode", i)] = `mode must be "separate" or "edit"`
				}
			}
		}
	}
```

In `applyConfigUpdates`'s notifications arm (`:750-768`), beside the `url` and `events` reads:
```go
				if v, ok := nm["mode"].(string); ok {
					nc.Mode = v
				}
```

> **Why this read is mandatory, not merely tidy.** `applyConfigUpdates` REPLACES `cfg.Notifications` wholesale: it builds a fresh `config.NotificationConfig{}` per entry from the payload and then assigns the slice. The PUT merges per *section*, not per key inside a notification object — so any per-target key not read here is dropped on **every** settings save from the dashboard. (Same reason N2b must read its three keys.)

- [ ] **Step 6: Write the route test**

Create `internal/web/routes/config_notification_mode_test.go`:

```go
package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestNotificationModeValidator mirrors config.Validate: the PUT must refuse
// exactly what the file loader refuses, or the dashboard could save a value
// the next boot normalises away behind the operator's back.
func TestNotificationModeValidator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    any
		wantErr bool
	}{
		{"edit", "edit", false},
		{"separate", "separate", false},
		{"empty", "", false},
		{"typo", "eidt", true},
		{"capitalised", "Edit", true},
		{"not a string", float64(1), false}, // ignored, not an error
	} {
		updates := map[string]any{"notifications": []any{
			map[string]any{"url": "discord://1/a", "mode": tc.mode},
		}}
		errs := validateConfigUpdates(updates)
		_, got := errs["notifications[0].mode"]
		if got != tc.wantErr {
			t.Errorf("%s: field error = %v, want %v (errs=%v)", tc.name, got, tc.wantErr, errs)
		}
	}
}

// TestApplyConfigUpdatesWritesMode — without this the PUT would validate the
// key and then drop it, which reads to the operator as a save that reverted.
func TestApplyConfigUpdatesWritesMode(t *testing.T) {
	cfg := config.Defaults()
	applyConfigUpdates(cfg, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/a", "mode": "edit"},
	}})
	if len(cfg.Notifications) != 1 {
		t.Fatalf("notifications = %d entries, want 1", len(cfg.Notifications))
	}
	if cfg.Notifications[0].Mode != "edit" {
		t.Errorf("Mode = %q, want edit", cfg.Notifications[0].Mode)
	}
}
```

- [ ] **Step 7: Exclude edit-mode targets from batching**

Locate the coalescer Arc N2b added in `internal/notifications/manager.go` (the function that decides whether a send joins an open 5 s window). Add the guard at its head:
```go
	// Batching is separate-mode only (owner ruling 2026-09-27): for an
	// edit-mode target the found/added embed IS the job's lifecycle message,
	// and coalescing it would defeat one-message-per-job.
	if t.mode == ModeEdit {
		return false
	}
```

Create `internal/notifications/lifecycle_batching_test.go`:

```go
package notifications

import "testing"

// TestEditModeTargetNeverBatches: the ruling excludes edit-mode targets from
// the 5 s found/added/auth window. Without this an edit-mode target's first
// two lifecycle events would arrive as one two-embed message with no id to
// edit afterwards.
func TestEditModeTargetNeverBatches(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{ModeSeparate, true},
		{"", true},
		{ModeEdit, false},
	} {
		tgt := notificationTarget{mode: normalizeTargetMode(tc.mode), msgKey: "abc"}
		if got := batchableTarget(tgt, SendOptions{Event: "found", JobID: "yt_1"}); got != tc.want {
			t.Errorf("mode %q: batchable = %v, want %v", tc.mode, got, tc.want)
		}
	}
}
```
Rename `batchableTarget` in the test to whatever N2b actually called the predicate; if N2b inlined the decision, extract it to a named predicate with this signature so it can be tested:
```go
func batchableTarget(t notificationTarget, opts SendOptions) bool
```

- [ ] **Step 8: Update `config.example.toml`**

**Append**, do not replace. N2b rewrote this block to add its `enabled` / `mention` / `mention_events` lines; a wholesale replacement would delete them. Read `config.example.toml` (the `[[notifications]]` example, `:319-322` on main) and add only these two lines to whatever is there, plus drop the dead `tags = ["important"]` line if N2b has not already:
```toml
# mode = "separate"                # "separate" (default: one message per event)
#                                  # or "edit" (one message per job, rewritten
#                                  # in place as it progresses; alerts stay separate)
```

- [ ] **Step 9: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/ ./internal/web/routes/ ./internal/notifications/ -count=1
```
Expected: `ok` for all three.

- [ ] **Step 10: Commit**

```bash
git add internal/config/config.go internal/config/notification_mode_test.go internal/web/routes/config_routes.go internal/web/routes/config_notification_mode_test.go internal/notifications/manager.go internal/notifications/lifecycle_batching_test.go config.example.toml
git commit -m "feat(config): per-target notification mode (separate | edit)

Opt-in per target, default separate. Validated identically by the file
loader and the PUT, applied by applyConfigUpdates, hot-reloaded through the
existing OnNotificationsChange -> Reload path (so it is not restart-required).
Edit-mode targets are excluded from the 5 s found/added batching window per
the ruling: their found embed IS the job's lifecycle message.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/config/config.go internal/config/notification_mode_test.go internal/web/routes/config_routes.go internal/web/routes/config_notification_mode_test.go internal/notifications/manager.go internal/notifications/lifecycle_batching_test.go config.example.toml
```

---

## Task 5: The `mode` control in both target editors

**Files:**
- Modify: `web/public/modules/settings.js` — `renderNotificationsList` (the notification card, today at `:1771-1845`; N2b will have added an `enabled` toggle and a `mention` row to the same card) and the delegated click handler; add a `setNotificationMode(index, mode)` method beside `toggleNotificationEvent` (`:1940`)
- Modify: `internal/tui/settings_notifications.go` — the edit form's head rows and the `Enter` save; `internal/tui/settings_view.go` — `renderNotifList` (the per-target line) and `renderNotifEdit` (the head rows); `internal/tui/settings.go` — the `SettingsModel` field; `internal/tui/settings_mouse.go` — the head-row count and the click map
- Modify: `internal/tui/settings_notif_click_test.go` — the two literal line expectations move by the number of head rows added
- Create: `web/tests/settings-notification-mode.test.mjs`
- Create: `internal/tui/settings_notif_mode_test.go`

**Interfaces:**
- Consumes: Task 4's `config.NotificationConfig.Mode` and the `notifications[i].mode` PUT key.
- Produces: no Go API. The web card exposes `data-notif-action="set-mode"` with `data-mode="separate|edit"`; the TUI `SettingsModel` gains `notifEditDelivery string`.

**Two collisions to respect, both verified on main:**
1. `SettingsModel.notifMode` (`internal/tui/settings.go:383`) already means the *sub-editor* mode, `"list"` or `"edit"` — the same word, a different axis. The new per-target field must **not** be called `notifEditMode`; use **`notifEditDelivery`** (values `"separate"` / `"edit"`).
2. The TUI's `Enter` save (`internal/tui/settings_notifications.go:170` on main) used to rebuild the struct from scratch — `n := config.NotificationConfig{URL: …}` — silently dropping every per-target field the editor did not know about. **N2b owns that fix** (ledger ruling 8: copy the existing target, overwrite only the edited fields). N3 keeps the pin — `TestNotifEditSavePreservesMode` — and adds one assignment.

- [ ] **Step 1: Write the failing web test**

Create `web/tests/settings-notification-mode.test.mjs`:

```js
// A notification target's `mode` decides whether a job gets one message per
// event or one message rewritten in place. It is opt-in and default-off, so
// the card must (a) always show which mode is active, (b) send the key on a
// save, and (c) never send "edit" for a target the operator did not switch.
//
// Like the other settings suites this needs jsdom, and `node --test
// web/tests/*.test.mjs` must stay green without it — so the import is probed
// first and every test is skipped (not failed) when jsdom is absent. Only an
// absent module is a skip; any other import failure must fail loudly.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const CONFIG = {
  notifications: [
    { url: "https://discord.com/api/webhooks/1/aaa" },                   // no key at all — reads as separate
    { url: "https://discord.com/api/webhooks/2/bbb", mode: "separate" }, // explicit, so "only that target" is provable
    { url: "https://discord.com/api/webhooks/3/ccc", mode: "edit" },
  ],
};

async function openSettings() {
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.renderNotificationsList();
  return h;
}

function cards(h) {
  return [...h.document.querySelectorAll(".notification-card")];
}

// MUTANT: render the control only when notif.mode is set — the first card
// then shows nothing and an operator cannot tell separate from unconfigured.
test("every card shows its delivery mode, default included", { skip }, async () => {
  const h = await openSettings();
  const [first, second, third] = cards(h);
  const activeOf = (card) =>
    [...card.querySelectorAll('[data-notif-action="set-mode"]')]
      .filter((el) => el.hasAttribute("variant"))
      .map((el) => el.dataset.mode);
  assert.deepEqual(activeOf(first), ["separate"], "a target with no mode key must read as separate");
  assert.deepEqual(activeOf(second), ["separate"]);
  assert.deepEqual(activeOf(third), ["edit"]);
});

// MUTANT: write the mode onto the wrong index — the operator switches one
// webhook and a different one changes.
test("clicking Edit sets mode on that target only", { skip }, async () => {
  const h = await openSettings();
  const btn = cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]');
  btn.click();
  await h.flush();

  assert.equal(h.app.config.notifications[0].mode, "edit");
  assert.equal(h.app.config.notifications[1].mode, "separate", "a sibling target must be untouched");
  assert.equal(h.app.config.notifications[2].mode, "edit");

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "the mode switch issued no PUT /api/config");
  assert.equal(put.body.notifications[0].mode, "edit");
  assert.equal(put.body.notifications[1].mode, "separate");
});

// MUTANT: store "" for separate — the key vanishes from the payload and the
// server keeps whatever it had, so switching back does nothing.
test("switching back to Separate is sent explicitly", { skip }, async () => {
  const h = await openSettings();
  const btn = cards(h)[2].querySelector('[data-notif-action="set-mode"][data-mode="separate"]');
  btn.click();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.equal(put.body.notifications[2].mode, "separate");
});

// MUTANT: forget that a mode switch must not disturb the event filter — the
// operator loses their allowlist by pressing a mode button.
test("a mode switch preserves the target's other keys", { skip }, async () => {
  const h = await openSettings();
  h.app.config.notifications[0].events = ["finished", "error"];
  h.app.settings.renderNotificationsList();
  cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]').click();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.deepEqual(put.body.notifications[0].events, ["finished", "error"]);
  assert.equal(put.body.notifications[0].url, "https://discord.com/api/webhooks/1/aaa");
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
node --test web/tests/settings-notification-mode.test.mjs
```
Expected: FAIL — `assert.deepEqual(activeOf(first), ["separate"])` reports `[]` (no `[data-notif-action="set-mode"]` elements exist), and the click tests throw `Cannot read properties of null (reading 'click')`.

- [ ] **Step 3: Add the web control**

In `renderNotificationsList`, build the mode row and place it above `eventsHtml`:
```js
        const mode = notif.mode === "edit" ? "edit" : "separate";
        const modeChip = (value, label) =>
          `<sl-tag size="small" ${mode === value ? 'variant="primary"' : ""} ` +
          `data-notif-action="set-mode" data-notif-index="${idx}" data-mode="${value}">${label}</sl-tag>`;
        const modeHtml = `
            <div class="notification-events">
              <div class="notification-event-group">
                <span class="notification-events-label">Delivery:</span>
                ${modeChip("separate", "Separate messages")}
                ${modeChip("edit", "One message per job")}
              </div>
            </div>`;
```
Insert `${modeHtml}` immediately before `${eventsHtml}` in the card template.

Extend the delegated handler:
```js
        else if (action === "set-mode") this.setNotificationMode(idx, el.dataset.mode);
```

Add the method beside `toggleNotificationEvent`:
```js
  /**
   * Set a target's delivery mode. "separate" is stored EXPLICITLY rather than
   * deleted. PUT /api/config merges per SECTION, not per key: applyConfigUpdates
   * rebuilds each notification entry from the payload, so a key this card omits
   * is simply lost. Writing the value out keeps the web card and the TUI editor
   * round-tripping the same states, and keeps the payload readable.
   */
  async setNotificationMode(index, mode) {
    const notif = this.app.config.notifications?.[index];
    if (!notif) return;
    const previous = notif.mode;
    notif.mode = mode === "edit" ? "edit" : "separate";
    try {
      await this._saveNotificationsOnly();
    } catch {
      notif.mode = previous;
    }
    this.renderNotificationsList();
  }
```

- [ ] **Step 4: Run the web test to verify it passes**

```bash
node --test web/tests/settings-notification-mode.test.mjs
```
Expected: `# pass 4`, `# fail 0`.

- [ ] **Step 5: Write the failing TUI test**

Create `internal/tui/settings_notif_mode_test.go`:

```go
package tui

import (
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestNotifEditLoadsDeliveryMode: opening a target for edit must show the mode
// it is actually in, or the operator toggles from a wrong starting point.
func TestNotifEditLoadsDeliveryMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:  "list",
		textInput:  textinput.New(),
		notifIndex: 0,
		notifications: []config.NotificationConfig{
			{URL: "discord://1/a", Mode: "edit"},
		},
	}
	m.handleNotifKey(keyEnter)
	if m.notifEditDelivery != "edit" {
		t.Errorf("notifEditDelivery = %q, want edit", m.notifEditDelivery)
	}

	m2 := &SettingsModel{
		notifMode:     "list",
		textInput:     textinput.New(),
		notifications: []config.NotificationConfig{{URL: "discord://1/a"}},
	}
	m2.handleNotifKey(keyEnter)
	if m2.notifEditDelivery != "separate" {
		t.Errorf("an unset mode must load as separate, got %q", m2.notifEditDelivery)
	}
}

// TestNotifEditTogglesDeliveryMode: Space on the Delivery row flips it.
func TestNotifEditTogglesDeliveryMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditEvents:   map[string]bool{},
		notifEditDelivery: "separate",
		notifEditFocus:    notifDeliveryFocusRow,
	}
	m.handleNotifEditKey(" ")
	if m.notifEditDelivery != "edit" {
		t.Fatalf("after one toggle = %q, want edit", m.notifEditDelivery)
	}
	m.handleNotifEditKey(" ")
	if m.notifEditDelivery != "separate" {
		t.Fatalf("after two toggles = %q, want separate", m.notifEditDelivery)
	}
}

// TestNotifEditSavePreservesMode is the one that matters: the Enter path
// rebuilds config.NotificationConfig from scratch, so every per-target field
// the editor does not carry is silently dropped on the next save.
func TestNotifEditSavePreservesMode(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditURL:      "discord://1/a",
		notifEditDelivery: "edit",
		notifEditEvents:   map[string]bool{},
		notifIndex:        0,
		notifications:     []config.NotificationConfig{{URL: "discord://1/a"}},
	}
	for _, e := range allNotifEvents {
		m.notifEditEvents[e] = true
	}
	m.handleNotifEditKey(keyEnter)

	if len(m.notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(m.notifications))
	}
	if m.notifications[0].Mode != "edit" {
		t.Errorf("saved Mode = %q, want edit — the Enter path dropped it", m.notifications[0].Mode)
	}
	if !m.dirty || !m.structDirty {
		t.Error("the save did not mark the form dirty")
	}
}

// TestNotifEditSaveWritesSeparateExplicitly: storing "" would be a second
// spelling of the default that the Web card does not use — the two editors
// must round-trip the same value.
func TestNotifEditSaveWritesSeparateExplicitly(t *testing.T) {
	m := &SettingsModel{
		notifMode:         "edit",
		textInput:         textinput.New(),
		notifEditURL:      "discord://1/a",
		notifEditDelivery: "separate",
		notifEditEvents:   map[string]bool{},
		notifIndex:        0,
		notifications:     []config.NotificationConfig{{URL: "discord://1/a", Mode: "edit"}},
	}
	for _, e := range allNotifEvents {
		m.notifEditEvents[e] = true
	}
	m.handleNotifEditKey(keyEnter)
	if m.notifications[0].Mode != "separate" {
		t.Errorf("saved Mode = %q, want separate", m.notifications[0].Mode)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestNotifEdit -count=1
```
Expected: FAIL to compile — `m.notifEditDelivery undefined`, `undefined: notifDeliveryFocusRow`.

- [ ] **Step 7: Add the TUI control**

`internal/tui/settings.go` — beside `notifEditURL` (`:384`):
```go
	// notifEditDelivery is the per-TARGET delivery mode being edited,
	// "separate" or "edit". Deliberately not named notifEditMode: notifMode
	// above is the SUB-EDITOR's mode ("list" / "edit"), a different axis that
	// happens to share the word.
	notifEditDelivery string
```

Beside the other notif constants in the same file, add the head-row map. Today the edit form's head is one row (URL) and `totalItems := 1 + len(allNotifEvents)`; Arc N2b adds its own head rows. Express the new row relative to whatever N2b left:
```go
// notifDeliveryFocusRow is the Delivery row's index in the edit form's focus
// order. The head rows come first (URL, then whatever the per-target scalars
// are), the event checkboxes after — notifEditHeadRows is the count both
// handleNotifEditKey and handleMouseNotifClick derive their arithmetic from.
const notifDeliveryFocusRow = notifEditHeadRows - 1
```
and define `notifEditHeadRows` as the head-row count **including** the new Delivery row (on today's main that is `2`; after N2b it is N2b's count + 1). Replace every literal `1 +` in `totalItems := 1 + len(allNotifEvents)` (`internal/tui/settings_notifications.go:129`) and `total := 1 + len(allNotifEvents)` (`internal/tui/settings_mouse.go:97`) with `notifEditHeadRows +`, and shift the event index accordingly (`eventIdx := m.notifEditFocus - notifEditHeadRows`).

**Three MORE sites hard-code today's single head row and must move in the same edit — grep `flatIdx` and `origLine` before you claim this step is done:**
- `internal/tui/settings_view.go:593` — `isFocused := m.notifEditFocus == flatIdx+1` → `flatIdx+notifEditHeadRows`. **No test covers this one**; get it right by reading it. Left stale, the view highlights event 0 while the Delivery row actually has focus.
- `internal/tui/settings_mouse.go`, `clickNotifEvent` — `m.notifEditFocus = flatIdx + 1` → `flatIdx + notifEditHeadRows`.
- `internal/tui/settings_mouse.go`, `handleMouseNotifClick` — the `origLine == 1` URL arm gains an `origLine == 2` Delivery arm (`m.notifEditFocus = notifDeliveryFocusRow`), and the event guard `if origLine >= 4 { m.clickNotifEvent(origLine - 4) }` shifts by the head rows added. Recount it from `renderNotifEdit`; do not assume +1.

`internal/tui/settings_notifications.go` — in the list-mode `Enter` and `"a"` arms, seed the field:
```go
			m.notifEditDelivery = "edit"
			if strings.TrimSpace(n.Mode) != "edit" {
				m.notifEditDelivery = "separate"
			}
```
(for `"a"`, the new-target arm, set `m.notifEditDelivery = "separate"`).

In `handleNotifEditKey`'s `case " ":` arm, handle the Delivery row before the event rows:
```go
	case " ":
		if m.notifEditFocus == notifDeliveryFocusRow {
			if m.notifEditDelivery == "edit" {
				m.notifEditDelivery = "separate"
			} else {
				m.notifEditDelivery = "edit"
			}
			return ""
		}
		if m.notifEditFocus >= notifEditHeadRows {
			eventIdx := m.notifEditFocus - notifEditHeadRows
			if eventIdx < len(allNotifEvents) {
				event := allNotifEvents[eventIdx]
				m.notifEditEvents[event] = !m.notifEditEvents[event]
			}
		}
		return ""
```

In the `keyEnter` arm: **N2b has already changed this save to copy the existing target and overwrite only the edited fields** (ledger ruling 8). Do NOT reintroduce a from-scratch literal — that is the exact defect the ruling removed. Add one line to N2b's copy:
```go
		n.Mode = m.notifEditDelivery
```
If (and only if) the save is still a from-scratch literal when you get here, N2b's task was not completed — convert it to the copy form first:
```go
		var n config.NotificationConfig
		if m.notifIndex < len(m.notifications) {
			n = m.notifications[m.notifIndex] // keep every key the editor does not own
		}
		n.URL = strings.TrimSpace(m.notifEditURL)
		n.Mode = m.notifEditDelivery
		n.Events = nil
```

`internal/tui/settings_view.go` — in `renderNotifList` (`:519-543`), append the mode to the line when it is not the default, so a list read tells the operator which targets are in edit mode:
```go
		if strings.TrimSpace(n.Mode) == "edit" {
			line += DimStyle.Render(" · one message per job")
		}
```
In `renderNotifEdit`, add the Delivery row directly below the URL row, styled like the other head rows, reading `m.notifEditDelivery` and labelled `Delivery` with the value rendered as `Separate messages` / `One message per job` and the hint `(Space to toggle)`.

`internal/tui/settings_mouse.go` — `handleMouseNotifClick` maps a content line back to a focus index; add the Delivery row to that map beside the URL row using the same `notifEditHeadRows` arithmetic.

- [ ] **Step 8: Update the click test's line expectations**

`internal/tui/settings_notif_click_test.go` pins the unscrolled/scrolled arithmetic with literal line numbers (`4` with `notifEditScrollStart: 2`, and `6` unscrolled) derived from `renderNotifEdit`'s layout: `0 title, 1 URL, 2 blank, 3 "Events:", 4 group0 blank, 5 group0 header, 6 group0 event0`. Each head row added pushes those down by one. Recount `renderNotifEdit`'s emitted lines after your edit and update **both** literals and the comment block that derives them. Do not delete the test.

- [ ] **Step 9: Run the TUI tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -count=1
```
Expected: `ok` — including `TestHandleMouseNotifClickAccountsForScroll` and `TestHandleMouseNotifClickNoScrollUnchanged` with their updated literals.

- [ ] **Step 10: Commit**

```bash
git add web/public/modules/settings.js web/tests/settings-notification-mode.test.mjs internal/tui/settings.go internal/tui/settings_notifications.go internal/tui/settings_view.go internal/tui/settings_mouse.go internal/tui/settings_notif_mode_test.go internal/tui/settings_notif_click_test.go
git commit -m "feat(ui): per-target delivery mode in both notification editors

Web: a two-chip Delivery row on the notification card, writing an EXPLICIT
\"separate\" so the merge-per-key PUT cannot swallow a switch back. TUI: a
Delivery row in the edit form (Space toggles), the mode shown on the list
line, and the Enter save now carries Mode through the struct it rebuilds —
that path silently dropped every per-target field it did not know about.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- web/public/modules/settings.js web/tests/settings-notification-mode.test.mjs internal/tui/settings.go internal/tui/settings_notifications.go internal/tui/settings_view.go internal/tui/settings_mouse.go internal/tui/settings_notif_mode_test.go internal/tui/settings_notif_click_test.go
```

---

## Task 6: The end-to-end state machine against a fake Discord

**Files:**
- Create: `internal/notifications/lifecycle_state_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-4. Adds no production code — if a row here fails, the fix belongs in `lifecycle.go`.

This task is the spec's §4.5 list, one test per row: first allowed event POSTs; later ones PATCH; filters skip without creating; restart resumes from the stored id; a removed target is ignored; a fresh process with no id POSTs; edits keep FIFO order with a retried PATCH; the terminal edit plus separate post; a mode flip mid-job; a shutdown flush that stays single-attempt; the History clamp across a real run.

- [ ] **Step 1: Write the test file**

Create `internal/notifications/lifecycle_state_test.go`:

```go
package notifications

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// editManager builds a Manager with one edit-mode target pointed at f, plus
// the given event filter (nil = all events).
//
// buildTargets accepts only Discord-shaped URLs and httptest hands out
// http://127.0.0.1:PORT, so the target is installed directly rather than
// through a config — the SAME shape buildTargets produces. Installing it goes
// through N1's own seam (installTargets below wraps applyTargets), because a
// bare `m.targets = …` leaves a queue nothing is draining.
func editManager(t *testing.T, f *fakeDiscord, st MessageStore, events []string) *Manager {
	t.Helper()
	m := &Manager{logger: testLogger{}}
	if st != nil {
		m.SetMessageStore(st)
	}
	installTargets(t, m, editTarget(f, events, ModeEdit))
	return m
}

// editTarget is one notificationTarget aimed at the fake.
func editTarget(f *fakeDiscord, events []string, mode string) notificationTarget {
	return notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		events: eventSet(events),
		mode:   mode,
		msgKey: targetMsgKey(f.URL()),
	}
}

// installTargets replaces the manager's targets and starts their sender
// goroutines, through N1's own seam — `applyTargets`, the inner half of
// Reload that builds each targetQueue and binds its dispatch closure.
//
// It must NOT be a bare `m.targets = …`: after N1 a target with no running
// sender goroutine swallows every Send. ADAPTATION POINT: if the merged tree
// spells the seam differently, use its spelling; what matters is that the
// queue and its bound dispatch exist afterwards.
func installTargets(t *testing.T, m *Manager, targets ...notificationTarget) {
	t.Helper()
	m.applyTargets(targets)
	t.Cleanup(m.Wait)
}

func eventSet(events []string) map[string]bool {
	if len(events) == 0 {
		return nil
	}
	m := make(map[string]bool, len(events))
	for _, e := range events {
		m[e] = true
	}
	return m
}

// run pushes a sequence of lifecycle events through the manager and waits for
// the FIFO to drain.
func run(t *testing.T, m *Manager, jobID string, events ...string) {
	t.Helper()
	for _, e := range events {
		m.Send(titleFor(e), "desc", typeFor(e), []Field{{Name: "Channel", Value: "c"}},
			SendOptions{Event: e, JobID: jobID})
	}
	m.Wait()
}

func titleFor(e string) string { return strings.ToUpper(e[:1]) + e[1:] }

func typeFor(e string) NotificationType {
	switch e {
	case "error":
		return TypeError
	case "cancelled":
		return TypeCancelled
	case "finished":
		return TypeSuccess
	case "muxing":
		return TypeMuxing
	default:
		return TypeDownload
	}
}

func methods(calls []recordedReq) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

func statusValue(p discordPayload) string {
	if len(p.Embeds) == 0 {
		return ""
	}
	for _, f := range p.Embeds[0].Fields {
		if f.Name == statusFieldName {
			return f.Value
		}
	}
	return ""
}

func historyValue(p discordPayload) string {
	if len(p.Embeds) == 0 {
		return ""
	}
	for _, f := range p.Embeds[0].Fields {
		if f.Name == historyFieldName {
			return f.Value
		}
	}
	return ""
}

// TestStateMachineFirstPostsThenEdits is the spine: one POST, then a PATCH per
// later lifecycle event, with Status tracking the newest state and History
// carrying every state that reached this target.
func TestStateMachineFirstPostsThenEdits(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	st := newMemStore()
	m := editManager(t, f, st, nil)

	run(t, m, "yt_1", "found", "scheduled", "downloading", "muxing", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 5 || got[0] != http.MethodPost ||
		got[1] != http.MethodPatch || got[4] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST then four PATCHes", got)
	}
	for i, c := range calls[1:] {
		if !strings.HasSuffix(c.Path, "/messages/M1") {
			t.Errorf("edit %d targeted %q, want .../messages/M1", i, c.Path)
		}
	}
	if got := statusValue(calls[4].Body); got != "Finished" {
		t.Errorf("final Status = %q, want Finished", got)
	}
	hist := historyValue(calls[4].Body)
	for _, want := range []string{"Found", "Scheduled", "Downloading", "Muxing", "Finished"} {
		if !strings.Contains(hist, want) {
			t.Errorf("History is missing %q: %q", want, hist)
		}
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "M1" {
		t.Errorf("stored id = %q, want M1", got)
	}
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want exactly 1 (one per job per target)", st.writeCount())
	}
}

// TestFilteredEventsNeitherCreateNorEdit: the allowlist semantics survive
// unchanged — the FIRST ALLOWED event creates the message, and a filtered
// event does nothing at all.
func TestFilteredEventsNeitherCreateNorEdit(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M2"))
	st := newMemStore()
	m := editManager(t, f, st, []string{"downloading", "finished"})

	run(t, m, "yt_1", "found", "scheduled", "downloading", "muxing", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 2 || got[0] != http.MethodPost || got[1] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST (downloading) then PATCH (finished)", got)
	}
	if got := statusValue(calls[0].Body); got != "Downloading" {
		t.Errorf("the creating message's Status = %q, want Downloading", got)
	}
	if hist := historyValue(calls[1].Body); strings.Contains(hist, "Muxing") {
		t.Errorf("History leaked a filtered event: %q", hist)
	}
}

// TestRestartResumesFromTheStoredID: a new Manager with the row already
// carrying an id must EDIT, not post — that is the whole point of persisting.
func TestRestartResumesFromTheStoredID(t *testing.T) {
	f := newFakeDiscord(t, okCreated("SHOULD_NOT_BE_USED"))
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "BOOT1"}

	m := editManager(t, f, st, nil)
	run(t, m, "yt_1", "downloading", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 2 || got[0] != http.MethodPatch || got[1] != http.MethodPatch {
		t.Fatalf("methods = %v, want two PATCHes", got)
	}
	if !strings.HasSuffix(calls[0].Path, "/messages/BOOT1") {
		t.Errorf("first edit targeted %q, want .../messages/BOOT1", calls[0].Path)
	}
	if st.writeCount() != 0 {
		t.Errorf("a resumed job wrote %d times, want 0 — the id was already stored", st.writeCount())
	}
	// A restart starts the History fresh; the message keeps being edited.
	if hist := historyValue(calls[1].Body); strings.Contains(hist, "Found") {
		t.Errorf("History claimed pre-restart states it cannot know: %q", hist)
	}
}

// TestFreshProcessWithNoIDPosts: the same job on an install that has never
// edit-delivered it simply posts.
func TestFreshProcessWithNoIDPosts(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M3"))
	m := editManager(t, f, newMemStore(), nil)
	run(t, m, "yt_1", "downloading")
	if got := methods(f.calls()); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("methods = %v, want one POST", got)
	}
}

// TestRemovedTargetIsIgnored: ids are keyed on the resolved URL, so a target
// the operator deleted leaves an orphaned entry that nothing ever reads — and
// a DIFFERENT webhook must not inherit it.
func TestRemovedTargetIsIgnored(t *testing.T) {
	gone := newFakeDiscord(t, okCreated("OLD"))
	live := newFakeDiscord(t, okCreated("NEW"))
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(gone.URL()): "OLDMSG"}

	m := editManager(t, live, st, nil)
	run(t, m, "yt_1", "downloading")

	if got := methods(live.calls()); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("the live target = %v, want one POST (it must not inherit the removed target's id)", got)
	}
	if n := len(gone.calls()); n != 0 {
		t.Errorf("the removed target received %d requests", n)
	}
	row := st.NotificationMsgs("yt_1")
	if row[targetMsgKey(gone.URL())] != "OLDMSG" {
		t.Errorf("the orphaned id was disturbed: %v", row)
	}
	if row[targetMsgKey(live.URL())] != "NEW" {
		t.Errorf("the live target's id = %v, want NEW", row)
	}
}

// TestTerminalEditsThenPostsSeparately (the ruling): two messages on failure —
// the lifecycle message edited to its terminal look, and the separate embed
// that carries the mention.
func TestTerminalEditsThenPostsSeparately(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M4"))
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading", "error")

	calls := f.calls()
	if got := methods(calls); len(got) != 3 ||
		got[0] != http.MethodPost || got[1] != http.MethodPatch || got[2] != http.MethodPost {
		t.Fatalf("methods = %v, want POST, PATCH (terminal edit), POST (separate embed)", got)
	}
	if got := statusValue(calls[1].Body); got != "Failed" {
		t.Errorf("terminal edit Status = %q, want Failed", got)
	}
	if statusValue(calls[2].Body) != "" {
		t.Error("the separate embed must NOT carry the Status/History rewrite")
	}
	if calls[2].Query != "" {
		t.Errorf("the separate embed asked for the message back (query %q)", calls[2].Query)
	}
}

// TestNonLifecycleEventsNeverTouchTheMessage: auth, trim_* and System keep
// posting as they always did, even on an edit-mode target.
func TestNonLifecycleEventsNeverTouchTheMessage(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M5"))
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading")
	m.Send("Authentication Required", "d", TypeWarning, nil, SendOptions{Event: "auth", JobID: "yt_1"})
	m.Send("Disk Space Warning", "d", TypeWarning, nil, SendOptions{Event: "disk_warning"})
	m.Wait()

	got := methods(f.calls())
	if len(got) != 3 || got[0] != http.MethodPost || got[1] != http.MethodPost || got[2] != http.MethodPost {
		t.Fatalf("methods = %v, want three POSTs (one creating, two separate)", got)
	}
}

// TestEditsKeepFIFOOrderAcrossARetry: with a retried PATCH in the middle, the
// later event must still land last. Before the per-target FIFO a retried embed
// could arrive after a newer one and freeze the message on a stale state.
func TestEditsKeepFIFOOrderAcrossARetry(t *testing.T) {
	old := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = old })

	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 1 { // the first PATCH (muxing) fails once
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		okCreated("M6")(n, r, rw)
	})
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading", "muxing", "finished")

	calls := f.calls()
	if len(calls) != 4 {
		t.Fatalf("want 4 requests (POST, PATCH-502, PATCH-retry, PATCH), got %d: %v", len(calls), methods(calls))
	}
	if statusValue(calls[1].Body) != "Muxing" || statusValue(calls[2].Body) != "Muxing" {
		t.Errorf("the retry did not repeat the SAME state: %q then %q",
			statusValue(calls[1].Body), statusValue(calls[2].Body))
	}
	if statusValue(calls[3].Body) != "Finished" {
		t.Errorf("last request Status = %q, want Finished — the retry reordered the states",
			statusValue(calls[3].Body))
	}
}

// TestModeFlipMidJob (Review Focus 3): a hot reload that turns edit mode off
// must leave the stored id alone and post separately; turning it back on must
// resume editing the SAME message.
func TestModeFlipMidJob(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M7"))
	st := newMemStore()
	m := editManager(t, f, st, nil)

	run(t, m, "yt_1", "downloading")

	// Hot reload to separate — through installTargets, the same path Reload
	// takes, so the target's sender goroutine is replaced rather than having a
	// field mutated underneath it.
	installTargets(t, m, editTarget(f, nil, ModeSeparate))
	run(t, m, "yt_1", "muxing")

	// ...and back to edit.
	installTargets(t, m, editTarget(f, nil, ModeEdit))
	run(t, m, "yt_1", "finished")

	got := methods(f.calls())
	if len(got) != 3 || got[0] != http.MethodPost || got[1] != http.MethodPost || got[2] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST (create), POST (separate while off), PATCH (resumed)", got)
	}
	if !strings.HasSuffix(f.calls()[2].Path, "/messages/M7") {
		t.Errorf("the resumed edit targeted %q, want .../messages/M7", f.calls()[2].Path)
	}
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want 1 — the flip must not re-write the id", st.writeCount())
	}
}

// TestShutdownFlushIsSingleAttempt: the whole path, not just dispatchOne —
// after BeginShutdown a queued lifecycle edit against a wedged Discord makes
// ONE request, so the owner's 10 s force-exit cap survives an edit-mode job.
func TestShutdownFlushIsSingleAttempt(t *testing.T) {
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPost && n == 0 {
			okCreated("MS")(n, r, rw)
			return
		}
		rw.WriteHeader(http.StatusBadGateway)
	})
	m := editManager(t, f, newMemStore(), nil)
	run(t, m, "yt_1", "downloading") // creates the message

	m.BeginShutdown()
	m.Send("Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished", JobID: "yt_1"})
	m.Wait()

	if n := len(f.calls()); n != 2 {
		t.Errorf("requests = %d, want 2 (the create, then ONE shutdown edit attempt)", n)
	}
}

// TestHistoryClampInARealRun: a job that flaps for a long time must keep the
// newest states visible and shed the oldest, never exceeding the field budget.
func TestHistoryClampInARealRun(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M8"))
	m := editManager(t, f, newMemStore(), nil)

	events := []string{"downloading"}
	for range 120 {
		events = append(events, "quality_split")
	}
	events = append(events, "finished")
	run(t, m, "yt_1", events...)

	last := f.calls()[len(f.calls())-1]
	hist := historyValue(last.Body)
	if len([]rune(hist)) > historyFieldMax {
		t.Errorf("History is %d runes, want <= %d", len([]rune(hist)), historyFieldMax)
	}
	if !strings.HasSuffix(hist, "Finished") {
		t.Errorf("History does not end on the newest state: %q", hist[max(0, len(hist)-60):])
	}
	if strings.Contains(hist, "Downloading") {
		t.Error("the clamp kept the oldest line instead of dropping it")
	}
}
```

- [ ] **Step 2: Run it**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -run 'TestStateMachine|TestFiltered|TestRestartResumes|TestFreshProcess|TestRemovedTarget|TestTerminalEdits|TestNonLifecycle|TestEditsKeepFIFO|TestModeFlip|TestShutdownFlush|TestHistoryClampInARealRun' -count=1 -v
```
Expected: every test PASSes. If `TestEditsKeepFIFOOrderAcrossARetry` fails on ordering, the bug is in the queue wiring from Task 3 Step 5 (the dispatch closure was bound outside the per-target goroutine); fix it there, not here. If `TestStateMachineFirstPostsThenEdits` reports more than one silent write, `remember`'s already-equal short-circuit is missing. If `TestShutdownFlushIsSingleAttempt` sees four requests, `q.shuttingDown` is not reaching `dispatchOne`'s `once` parameter.

- [ ] **Step 3: Run the package with the race detector**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -race -count=1
```
Expected: `ok`, no race reports.

- [ ] **Step 4: Commit**

```bash
git add internal/notifications/lifecycle_state_test.go
git commit -m "test(notifications): the edit-in-place state machine end to end

Nine rows against a recording fake Discord: first allowed event POSTs and
later ones PATCH the same message; a filter skips without creating; a
restart resumes from the stored id and writes nothing; a removed target's
id is orphaned, not inherited; a fresh process posts; error edits then
posts separately; non-lifecycle events never touch the message; a retried
PATCH keeps FIFO order; and the History clamp sheds the oldest lines.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/notifications/lifecycle_state_test.go
```

---

## Task 7: Documentation

**Files:**
- Modify: `docs/spec/operations.md` — the `## Notifications (Discord Webhooks)` section (`:442`): a new `### Delivery Modes` subsection after `### Event Types`'s vocabulary paragraph (`:490-496`), a bullet in `### Dispatch Behavior` (`:519-527`), and two rows in the `### Source Files` table (`:637-638`)
- Modify: `docs/spec/data-and-storage.md` — the `jobs` column table (after the `park_identity` row, `:249`), the schema-history table (after the `v19` row, `:382`), and the `#### [[notifications]]` table (`:613-619`)
- Modify: `docs/spec/user-interfaces.md` — the `settings_notifications.go` row (`:229`)
- Modify: `README.md` — the `## Webhook Notifications` section (`:673-684`)

**Interfaces:** none. The citation gate (`go test ./internal/docs/`) checks every symbol and path named here.

- [ ] **Step 1: operations.md — the delivery-mode subsection**

After the "The canonical event vocabulary is `notifications.EventGroups`…" paragraph, insert:

```markdown
### Delivery Modes

Each target carries a `mode`: `"separate"` (the default) or `"edit"`.

**`separate`** — one message per event, never rewritten. This is what every
target did before v2.9 and what an unset `mode` still means.

**`edit`** — one Discord message per job, per target. The first ALLOWED
lifecycle event creates it (`POST …?wait=true`, which per the Discord API docs
"waits for server confirmation of message send before response, and returns the
created message body" — the only way to learn the id); every later allowed
lifecycle event rewrites it in place with
`PATCH /webhooks/{id}/{token}/messages/{message_id}`. The rewritten embed
carries the new event's own title, description and fields plus two more: a
**Status** field naming the current state, and a **History** field of
`<t:unix:R> State` lines, one per state that reached this target, clamped to the
field budget by dropping the OLDEST lines.

The lifecycle set is `found`, `added`, `scheduled`, `rescheduled`,
`downloading`, `quality_split`, `gap_split`, `connectivity_resume`,
`connectivity_split`, `muxing`, `finished` (`lifecycleEvents`,
`internal/notifications/lifecycle.go`). Everything else — `error`, `cancelled`,
`auth`, the `trim_*` family and all of System — is always its own post: those
may carry a mention, and an edit notifies nobody.

`error` and `cancelled` do **both**: the lifecycle message is edited to its
terminal look, and the separate embed is still posted. Two messages on failure,
by design — the separate one is what pings.

There are no progress edits. A cadence-driven PATCH would spend the bucket for
nothing.

**What persists.** The created message's id is stored on the job row, in
`jobs.notification_msgs` (schema 20) — a JSON object keyed by the first 16 hex
digits of SHA-256 over the RESOLVED webhook URL, the same value target dedupe
uses. It is written ONCE per (job, target), on the first successful POST,
through `UpdateNotificationMsgs`: a silent single-column write that bumps no
`updated_at` and wakes no subscriber. A restart therefore keeps editing the same
message. The History does not persist — the message keeps being edited, but its
History begins again at the first state after the restart.

Ids are keyed on the resolved URL, so the two spellings of one webhook share one
message; a target the operator removed leaves an orphaned entry that nothing
reads; a target the operator adds starts a new message at its next allowed
event; and deleting the job drops the row and the ids with it (no DELETE is ever
sent to Discord).

The in-memory half — the message ids a running process is holding, and the
History lines — is released when a job reaches its terminal edit, and capped at
`maxTrackedJobs` for jobs that never do. That is a cache eviction, not a close:
the persisted id is what survives, so a Retry after a release re-reads the row
once and keeps editing the same message.

**During shutdown** every request on this path is single-attempt, like every
other send: the owner's ruling caps a graceful shutdown at 10 s, and one
lifecycle edit's retry ladder could spend all of it.

**When the message is gone.** A PATCH answered `404` with `Unknown Message`
(code 10008) makes the notifier post a new message and overwrite the stored id.
A `404` naming `Unknown Webhook` (10015) is not that — the webhook itself was
revoked, and it stays a permanent failure.

**Budget.** Edits buy channel quiet, not request budget: per the Discord API
docs the rate-limit bucket's top-level resource is `webhook_id + webhook_token`,
so a PATCH spends the same bucket as a POST and goes through the same
three-attempt loop, the same `Retry-After` handling and the same per-target
FIFO. That FIFO is what makes edit mode safe — a job's states can never
reorder, and a retried edit can never land after a later one.

**Batching does not apply.** The 5 s `found`/`added`/`auth` window is
separate-mode only: for an edit-mode target the `found` embed IS the job's
lifecycle message, and coalescing it would defeat one-message-per-job.
```

- [ ] **Step 2: operations.md — the dispatch bullet and the file table**

Add to `### Dispatch Behavior`:
```markdown
- **Delivery mode:** per target, `separate` (default) or `edit` — see **Delivery Modes** above. Hot-reloads with the rest of the notifications array; no restart.
```

Add to the `### Source Files` table, after the `internal/notifications/discord.go` row:
```markdown
| `internal/notifications/lifecycle.go` | Edit-in-place lifecycle messages: the event set, the per-(job, target) message-id store, the POST-or-PATCH decision, the Status/History rewrite |
| `internal/notifications/discord_edit.go` | `?wait=true` create + `PATCH …/messages/{id}` edit, and the Unknown-Message refusal |
```

- [ ] **Step 3: data-and-storage.md — the column, the schema row, the config table**

After the `park_identity` row in the `jobs` column table:
```markdown
| notification_msgs | TEXT | NULL | JSON object added v20, mapping a notification target's key (first 16 hex digits of SHA-256 over the RESOLVED webhook URL) to the id of the one Discord message that target rewrites in place for this job (`mode = "edit"`). NULL for every job on a separate-mode install, which is the default. Written once per (job, target) on the first successful POST through `UpdateNotificationMsgs` — a silent single-column write that bumps no `updated_at` and wakes no subscriber. A corrupt or half-written value decodes to nil and the job simply posts a new message. Credential-adjacent: `json:"-"`, never serialized to clients. |
```

After the `v19` row in the schema-history table:
```markdown
| v20 | Added `notification_msgs TEXT` (nullable) to `jobs`: the per-target Discord message ids an edit-mode notification target rewrites in place. No backfill — an id exists only once a message has been posted, and there is nothing to reconstruct for jobs that predate the column |
```

Add the `Mode` row to the `#### [[notifications]]` table. **N2b owns the removal of the stale `Tags` row** (a field deleted from `NotificationConfig` in 2026-07) and the `enabled` / `mention` / `mention_events` rows — after the merge they should already be there. Drop `Tags` yourself only if the merge shows it still present. The finished table:
```markdown
#### [[notifications]] (array of tables)

| Field | Type | TOML Key | Notes |
|-------|------|----------|-------|
| URL | string | `url` | Discord webhook URL (`https://discord.com/api/webhooks/ID/TOKEN` or `discord://ID/TOKEN`) |
| Events | []string | `events` | Event allowlist; absent or empty means all events |
| Mode | string | `mode` | `"separate"` (default) or `"edit"` — one message per event, or one message per job rewritten in place. See [operations.md](operations.md) § Delivery Modes |
```
(If N2b's three rows are already present, insert only the `Mode` row and leave the rest alone.)

- [ ] **Step 4: user-interfaces.md**

Replace the `settings_notifications.go` row:
```markdown
| `settings_notifications.go` | Notification sub-editor: webhook list, delivery mode (Separate messages / One message per job), per-event toggles, test send. |
```

- [ ] **Step 5: README.md**

Replace the `## Webhook Notifications` body (keep Arc N1's corrections to the Discord-only wording and the event-list pointer):

```markdown
## Webhook Notifications

Send Discord webhook notifications for stream events:

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN"
events = ["finished", "error"]  # Optional filter (default: all events)
mode = "separate"               # or "edit" — see below
```

The full event vocabulary lives in the notifications table in
[docs/spec/operations.md](docs/spec/operations.md).

**One message per job.** Set `mode = "edit"` on a target and Moombox posts one
Discord message per job and then rewrites it in place as the job progresses —
found, scheduled, downloading, splits, muxing, finished all land on the same
message, which grows a Status line and a short history instead of a new embed
each time. Failures and credential alerts stay separate posts, because those are
the ones that ping. The message id is remembered on the job, so a restart keeps
editing the same message; if someone deletes it in Discord, the next event posts
a fresh one. Default is `"separate"` — the classic one-embed-per-event
behaviour.
```

- [ ] **Step 6: Run the citation gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/ -count=1 -v
```
Expected: PASS. A failure naming a symbol means a doc cites something that does not exist — fix the doc (or the code), never the allowlist.

- [ ] **Step 7: Check for CRLF**

```bash
perl -0777 -ne 'print "CR in $ARGV\n" if tr/\r//' docs/spec/operations.md docs/spec/data-and-storage.md docs/spec/user-interfaces.md README.md config.example.toml
```
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add docs/spec/operations.md docs/spec/data-and-storage.md docs/spec/user-interfaces.md README.md
git commit -m "docs: the edit-in-place delivery mode

operations.md gains a Delivery Modes section (the lifecycle set, the
Status/History rewrite, the terminal double-send, what persists and what
does not, the Unknown-Message fallback, and that edits share the webhook's
rate bucket rather than buying budget). data-and-storage.md documents the
notification_msgs column and schema 20, and its [[notifications]] table
drops the Tags field removed in 2026-07. README gains a short paragraph.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/spec/operations.md docs/spec/data-and-storage.md docs/spec/user-interfaces.md README.md
```

---

## Task 8: Gates, the node recount, and the plan's own deletion

**Files:**
- Modify: `web/tests/README.md` (seven places, recounted from a real run)
- Modify: `.claude/skills/moombox-database-migrations/SKILL.md` (the schema-version header)
- Delete: `docs/superpowers/plans/2026-09-27-webhooks-n3-edit-in-place.md`

**Interfaces:** none.

- [ ] **Step 1: Formatting and static analysis**

```bash
gofmt -l ./cmd ./internal ./tools ./web
```
Expected: no output. If a file is listed, run `gofmt -w` on it.

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
```
Expected: no output.

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy -diff
```
Expected: no output (this arc adds no module dependency — `crypto/sha256`, `encoding/hex`, `encoding/json` are stdlib).

- [ ] **Step 2: staticcheck**

```bash
go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./...
```
Expected: no output. The likeliest finding is an unused import or an unused local left in one of the four new test files — delete it rather than suppressing it. Each test file in this plan names its exact import set; check them against what the finished file uses.

- [ ] **Step 3: Build**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
```
Expected: no output.

- [ ] **Step 4: The named packages**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/database/ ./internal/notifications/ ./internal/config/ ./internal/tui/ ./internal/web/routes/ ./internal/docs/
```
Expected: `ok` for all six. Do **not** run `go test ./...`.

- [ ] **Step 5: The node suites, both ways**

```bash
timeout 600 node --test web/tests/*.test.mjs
```
Record the `tests` / `pass` / `fail` / `skipped` line. Then, with jsdom present:
```bash
cd web/tests && npm ci && cd ../.. && timeout 600 node --test web/tests/*.test.mjs
```
Record the second set. Expected: `fail 0` in both runs. The `timeout` wrapper is not optional — a scratch `node --test` that wedges leaves an orphan process tree behind.

- [ ] **Step 6: Recount `web/tests/README.md`**

Seven places carry counts that this arc's new suite changes. Update each from the numbers Step 5 reported:
1. the opening paragraph's suite count word ("Sixteen suites drive their module inside a jsdom document") and its list — add `settings-notification-mode.test.mjs` in alphabetical position
2. **line 37**, "The sixteen suites listed above are the ones that need a DOM" — the second occurrence of the count word, easy to miss
3. the list in "### How the skip works" ("the 160 DOM tests (player 63, …)") — add `settings-notification-mode 4` in its descending-count position and raise the 160
4. the "leaving 131 tests that need no DOM" figure (unchanged if the new suite is fully jsdom-gated — verify against the run rather than assuming)
5. the fenced block:
   ```
   ℹ tests 291
   ℹ pass 131
   ℹ fail 0
   ℹ skipped 160
   ```
6. the sentence after it ("With jsdom installed the same command reports `tests 291` / `pass 291` / `skipped 0`")
7. the "Needs jsdom" table — add `settings-notification-mode.test.mjs` to the `yes` row

(Arc N2b moved these numbers too and added its own suite; recount from the run, never by arithmetic on the committed figures.)

- [ ] **Step 7: Bump the migrations skill's schema-version line**

`.claude/skills/moombox-database-migrations/SKILL.md`'s header still reads `Current schema version: **v16**` — stale since v17, and this arc is the one that moves it to 20.

- old: `Schema changes use incremental version-based migrations in `internal/database/migrations.go`. Current schema version: **v16**.`
- new: `Schema changes use incremental version-based migrations in `internal/database/migrations.go`. Current schema version: **v20**.`

The skill is inside the citation gate's `specDocs` set, so re-run `go test ./internal/docs/ -count=1` after the edit.

- [ ] **Step 8: Verify the whole arc against the spec**

```bash
grep -n "mode\|notification_msgs\|lifecycle" docs/superpowers/specs/2026-09-27-discord-webhooks-design.md | sed -n '1,40p'
```
Walk §4's five numbered items and confirm each has landed: (1) the `mode` key in config, both validators, both editors and the docs; (2) the lifecycle set, the create/edit split, the Status/History rewrite, the terminal double-send, no progress edits; (3) the column, schema 20, the silent write, the load-once read, the 404 re-post, resolved-URL keying, removed/new target behaviour, job deletion; (4) the FIFO ride and the shared bucket; (5) every test row.

- [ ] **Step 9: Delete the plan**

```bash
git rm docs/superpowers/plans/2026-09-27-webhooks-n3-edit-in-place.md
```
An implemented plan is deleted, not archived — git history is the archive.

- [ ] **Step 10: Commit**

```bash
git add web/tests/README.md .claude/skills/moombox-database-migrations/SKILL.md
git commit -m "chore(n3): gates green, node counts recounted, plan removed

gofmt / vet / tidy -diff / staticcheck / build clean; database,
notifications, config, tui, routes and docs packages green; both node runs
green with the counts in web/tests/README.md recounted from them. The
migrations skill's schema-version header moves 16 -> 20 (stale since v17).
The Arc N3 plan is deleted now that it is implemented.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- web/tests/README.md .claude/skills/moombox-database-migrations/SKILL.md docs/superpowers/plans/2026-09-27-webhooks-n3-edit-in-place.md
```
