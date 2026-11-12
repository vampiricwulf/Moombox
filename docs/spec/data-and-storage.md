# Data and Storage

## Scope

This document specifies every data persistence layer in Moombox: the SQLite database (schema, connection tuning, the synchronous write path, pub/sub), the TOML configuration system (sections, types, migrations, validation), the cookie management subsystem (jar, refresh, auto-cookie), the logger (file rotation, ring buffer, pub/sub), and the on-disk file output conventions (staging, output templates, resume state, chat files). It is the authoritative reference for how Moombox reads, writes, and organizes persistent and transient data.

## Rules and Constraints

These are hard rules. An AI assisting with Moombox development must follow them without exception:

- **SQLite with WAL mode, 1 connection, 5s busy timeout, foreign keys on.** The DSN is `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)` — modernc.org/sqlite only honors `_pragma=...` parameters (the mattn-style `_journal_mode=...` form is silently ignored). `<path>` is `database_path` with `%`, `?` and `#` percent-escaped (`FileURI`, `internal/sqliteuri/sqliteuri.go`; a leading `//` also gets an empty authority before it): modernc opens a `file:` DSN as an SQLite URI, so unescaped they cut the path short or decoded it, and the database landed in a file nobody configured. `FileSchemaVersion` builds its read-only DSN the same way, and so do the two browser cookie readers (`queryFirefoxCookieDB`, `internal/cookies/autocookies_firefox.go`, and the DPAPI reader's `openCookieDB`, `internal/cookies/dpapi/dpapi.go`), whose unescaped paths — a profile path, or the temp directory the Firefox snapshot lands in — failed the read as a corrupt cookie database.

  An install upgraded from those releases still has its database in that other file, so while the literal `database_path` does not exist, `Open` and `FileSchemaVersion` keep using the file the raw path resolved to (`legacySQLitePath`) when it is an SQLite database holding a `jobs` table, and `Open` warns on every start naming both paths until the operator moves the file, with its `-wal` and `-shm`, into place (`legacyDatabaseFile`, same file, reached through `ResolveFile`, which `moombox add` resolves `database_path` with too, and `Database.File` names the file an `Open` used); opening the literal path instead would have started an empty database, whose empty history lets the monitors and the backfill queue every archived video again. A file there that cannot be read as a database refuses the open with an error naming both paths. Connection pool is `SetMaxOpenConns(1)` and `SetMaxIdleConns(1)`. SQLite is single-writer; do not change the pool size.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** The method accepts `map[string]any`, maps keys through `fieldToColumn` (52 entries), dynamically builds a `SET` clause, and auto-appends `updated_at` with the current UTC RFC3339 timestamp. After writing, it re-reads the full job — the row and its gaps, trims and segments, as `GetJob` reads them — to notify subscribers with a complete `*Job` object, except after a progress tick (`IsProgressOnlyChange`), whose `*Job` is the row alone. Returns the updated `*Job`.
- **`fieldToColumn` defines the allowed keys for `UpdateJobFields`.** Any key not present in this map is silently ignored. The map currently has 52 entries mapping Go field names to SQLite column names (identity mapping in all cases). Deliberately left out: `notification_msgs` (`UpdateNotificationMsgs` is its only writer), `channel_id` (set at insert — feed affiliation never changes, and a partial write of `""` would fake-empty a NULL), and the identity and bookkeeping columns `id`, `video_id`, `url`, `platform`, `created_at`, `updated_at`. Adding a new column that `UpdateJobFields` should be able to write requires adding a corresponding entry here.
- **`JobStatus` is `type JobStatus string`.** Status values are string constants, not integers or enums. Timestamps are ISO 8601 / RFC3339 strings. Optional numeric fields (sequence counters, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`).
- **Job writes are synchronous; there is no batching.** `UpdateJobFields` executes its `UPDATE` immediately under `db.mu`, re-reads the row in the same critical section, releases the lock and then notifies subscribers. There is no update channel, writer goroutine or coalescing window — the only goroutine the package starts is the `OnJobsChange` fan-out — so when nothing is being written, nothing runs and the database performs zero IO.
- **Config migrations are non-destructive.** `migrateOldFormat()` only applies a migration when the target section does not already exist in the TOML file. It never overwrites user-configured values in existing sections.
- **FlexDuration parses config values in each field's own unit.** A bare integer in `feed_check_interval` means minutes; in `hide_finished_age_days` days; in `probe_cooldown` seconds. Duration strings like `"10m"`, `"7d"` are parsed via regex and converted to the field's unit.
- **Schema migrations are versioned, idempotent, and forward-only.** Currently at v28 (`schemaVersion` in `internal/database/migrations.go`; `appendix-metrics.md` mirrors it). Each migration checks the current version before applying. Migrations run at startup in `Database.migrate()`, called from `Open()`. There is no rollback mechanism.
- **Cookie file format is Netscape.** The jar only loads cookies matching YouTube/Google domains or Twitch domains. Cookies are filtered to essential authentication cookies only.
- **Log file rotation uses numbered suffixes.** The current file is renamed to `.1`, existing `.N` files shift to `.N+1`, and excess files beyond `log_max_files` are deleted.
- **Resume state files are JSON sidecars.** Named `<output_file>.resume.json`, they store the last successful segment sequence number, bytes written, timestamp, base URL, stream ID and, for a whole-file download, the probed total size. Validated on load by IDENTITY (`resumeIdentityMismatch`: explicit StreamID first, then YouTube URL fingerprinting; opaque URLs with no identity — Twitch weaver — are deliberately TRUSTED) plus a file-size check, and cleared only on clean stream completion. Raw URL equality must NOT be used as the identity check: Twitch weaver URLs rotate every fetch, and URL-equality validation would truncate hours of recording on every daemon restart.
- **Chat files use incremental append, not full rewrite.** After the first flush, new messages are appended by seeking to the closing `]` bracket, truncating there, and writing new messages plus the closing structure. The `messageCount` field in the JSON header is padded to 20 characters so it can be updated in-place without shifting the rest of the file.

---

## Database (internal/database/)

### Connection Configuration

The database is opened via `sql.Open("sqlite", dsn)` using the `modernc.org/sqlite` pure-Go driver (no CGo).

**DSN parameters:**

| Parameter | Value | Purpose |
|-----------|-------|---------|
| `_pragma=journal_mode(WAL)` | WAL | Write-ahead logging for concurrent reads during writes |
| `_pragma=synchronous(OFF)` | tests only | Appended by `openDSN` when `testing.Testing()` reports a `go test` run: every test migrates its own database, and the per-commit fsyncs made the database package 205 s on the Windows CI runner. Production never sets it; SQLite's default (FULL in WAL mode) stands. |
| `_pragma=busy_timeout(5000)` | 5000ms | Wait up to 5 seconds for a locked database before returning SQLITE_BUSY |
| `_pragma=foreign_keys(1)` | on | Enforce foreign key constraints (gaps, trims, segments reference jobs — `ON DELETE CASCADE` fires on `DeleteJob`) |

modernc.org/sqlite recognizes only the `_pragma=name(value)` parameter form; mattn-style keys (`_journal_mode`, `_busy_timeout`, `_foreign_keys`) are silently dropped by the driver.

**Connection pool:**

| Setting | Value | Rationale |
|---------|-------|-----------|
| `SetMaxOpenConns` | 1 | SQLite is single-writer; one connection avoids lock contention |
| `SetMaxIdleConns` | 1 | Keep the single connection warm |

Source: `Open()` in `internal/database/database.go`.

### Database Struct

```go
type Database struct {
    db        *sql.DB
    mu        sync.RWMutex
    closeOnce sync.Once
    logger    dbLogger

    // Per-instance snapshot of the package-level map, taken at Open()
    fieldToColumn map[string]string

    // Pub/sub — six subscriber kinds
    onJobUpdate    []jobUpdateSub
    onJobChange    []jobChangeSub
    onJobAdded     []jobAddedSub
    onJobDeleted   []jobDeletedSub
    onTrimsChanged []trimsChangedSub
    onJobsChange   []jobsChangeSub
    nextSubID      uint64
    subMu          sync.RWMutex

    // Prepared statements
    stmtGetJob    *sql.Stmt
    preparedStmts []*sql.Stmt // everything prepared via prepareStmt, released by Close

    // Per-job in-memory log buffers
    jobLogsMu sync.RWMutex
    jobLogs   map[string][]string
    logRouted map[string]struct{} // the job IDs RouteLogToJobs scans

    // GetJobStats cache
    statsMu       sync.Mutex
    statsCached   *JobStats
    statsCachedAt time.Time
}
```

Key details:

- `mu` (sync.RWMutex) guards all database operations. Read operations acquire `RLock`; write operations acquire `Lock`.
- `fieldToColumn` is a per-instance copy of the package-level map, snapshotted at `Open()` so the map is read-only at runtime.
- `stmtGetJob` is the prepared hot-path SELECT behind `GetJob` and the row read-back in `UpdateJobFields`. `preparedStmts` tracks every statement prepared through `prepareStmt` so `Close()` can release them.
- Each subscriber slice holds `{id, fn}` entries; `nextSubID` hands out the ids that the unsubscribe closures remove by.
- `jobLogs` is an in-memory map of per-job log buffers (not persisted to SQLite). Capped at 200 lines per job; when exceeded, trimmed to the last 100. `logRouted` is the separate set of job IDs that `RouteLogToJobs` scans — only non-terminal jobs — so a finished job's buffer outlives its tracking.
- `statsCached` memoises `GetJobStats` (a full-table scan) for `jobStatsCacheTTL` (5 s); it is not invalidated on writes.

### Write Path (no batching)

Every job write is synchronous. There is no update channel, no writer goroutine and no coalescing window: `UpdateJobFields` executes its `UPDATE` on the caller's goroutine under `db.mu`, re-reads the row through `stmtGetJob` in the same critical section, releases the lock, and then notifies subscribers (see Partial Updates below for the step list).

**Where write amplification is bounded:** upstream, in `ProgressTracker` (`internal/worker/progress.go`), which writes one job row per report and reports at most once per job per configured progress interval (`downloader.progress_interval_ms`, 16 ms default), flushing gap rows at most once a second. Every other `UpdateJobFields` caller is event-driven.

**Idle cost:** zero. Nothing in the package ticks, and the only goroutine it ever starts is the `OnJobsChange` fan-out (`dispatchJobsChange`, used by the two bulk writers).

### Partial Updates (UpdateJobFields)

`UpdateJobFields(id string, fields map[string]any)` is the primary mechanism for updating specific job fields without loading and re-writing the entire job row.

**Process:**

1. Iterate `fields` map; for each key, look up the column name in `fieldToColumn`. Unknown keys are silently skipped.
2. Build dynamic `SET col1=?, col2=?, ..., updated_at=?` clause.
3. Execute the UPDATE under `db.mu.Lock`.
4. Re-read the full job row via the prepared `stmtGetJob` in the same critical section (subscribers need all fields, not just the changed ones), and its gaps, trims and segments with it (`loadChildRows`, which `GetJob` uses too), then release `db.mu` — BEFORE notifying, so a subscriber can call back into the database. Both UIs replace the row they hold with the one this delivers, so a read-back without the child rows would swap a row that had them for one that did not (`Gaps`, `Trims` and `Segments` are `omitempty`, so the dashboard's JSON would lose the keys): every status transition, rename or Mark Watched would empty the details' Parts, Trims and Gaps sections, send the trimmer to `/api/jobs/{id}/video` instead of the parts and empty the TUI trim dialog, until a reload. A progress tick is the one write that skips them: `IsProgressOnlyChange` (`internal/database/database_subscribers.go`) — every written column in `progressColumns` — is the ~60 Hz path, a tick moves no child row, and no subscriber replaces a row with a tick's `*Job` (the dashboard merges the `job_progress` frame built from it; the TUI reads it into its progress store).
5. Notify all `onJobUpdate` subscribers with the complete `*Job`, and all `onJobChange` subscribers with the job plus the list of columns written (`updated_at` excluded). If the read-back finds no row — deleted between the UPDATE and the SELECT — `notifyJobDeleted` fires instead.

**fieldToColumn map (52 entries):**

```
status, progress, percent, eta, speed, error, title, channel_name,
thumbnail_url, description, output_file, filename, output_directory,
download_started_at, stream_start_time, stream_end_time, length_seconds,
last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
total_chat_messages, chat_status, chat_filename, chat_file, thumbnail_file,
description_file, is_vod, manually_added, allow_non_stream, video_width,
video_height, video_fps, file_size, last_recheck_at, twitch_quality,
twitch_category, channel_avatar_url, selected_video_itag, selected_audio_itag,
start_time, end_time, quality_preference, watched, resume_position, chat_offset,
auto_retry_count, queue_priority, incomplete_tail, park_reason, park_identity,
announce_found, join_offset_seconds
```

All entries use identity mapping (Go key name == SQLite column name). `notification_msgs` is absent on purpose: `UpdateNotificationMsgs` writes it.

**Usage example:**

```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "(V: 1234/1300 A: 1234/1300 C: 900)",
    "percent":  42.5,
})
```

There is no full-row `UpdateJob()` counterpart: `UpdateJobFields` is the only job writer, synchronous and immediate, and it triggers subscribers directly once `db.mu` is released.

**Conditional writes.** Four forms of it apply only while the row's status allows, and report whether they did: `UpdateJobFieldsIf(id, expected, fields)` while the status is still `expected` — a compare-and-set — `UpdateJobFieldsIfIn(id, []JobStatus, fields)` while it is one of a set (an empty set matches nothing), `UpdateJobFieldsUnless(id, unwanted, fields)` while it is anything but `unwanted`, and `UpdateJobFieldsUnlessTerminal(id, fields)` while it is none of the terminal statuses (`Finished`, `Error`, `Cancelled`, the set `Job.IsTerminal` reads) (`internal/database/database.go`). The condition is ANDed into the same `UPDATE` (`updateJobFieldsWhere`, the machinery all five share), so nothing can land between the check and the write. A write that did not apply touches nothing: no `updated_at`, no read-back, no subscriber — and no `OnJobDeleted` for a row that is gone either, since the delete fired its own. They exist for a transition decided on a status read earlier, which, written unconditionally, would overwrite whatever landed in between: the backlog scheduler's `Queued` → `Upcoming` admission is `UpdateJobFieldsIf(..., Queued, ...)`, every resume out of `COOKIES?` (the credential sweeps' and the worker's after an automatic cookie refresh) is `UpdateJobFieldsIf(..., COOKIES?, ...)`, and the worker's failure write (`setJobError`) and its backlog requeue write with `UpdateJobFieldsUnless(..., Cancelled, ...)` — each written unconditionally would turn an operator's Cancel that landed between the read and the write into a download, an Error or a requeue.

The off-queue mux (`MuxJob`, `internal/worker/worker_verbs.go`) puts back the `Cancelled` its own `Muxing` write can land on top of, once the operator's Cancel has stopped its FFmpeg, with `UpdateJobFieldsIf(..., Muxing, ...)`: read and then written unconditionally, that `Cancelled` also landed on whatever came after the Cancel — a `Finished` a racing mux wrote, the operator's Resume of the cancelled row. The Cancel itself is the other way round: `CancelJob` writes `Cancelled` with `UpdateJobFieldsIfIn` over the cancellable statuses (every status short of an outcome), because it is decided on the status a UI showed, and a job that finished or failed since must not become a Cancelled one.

### Pub/Sub System

Six callback types (`internal/database/database_subscribers.go`):

| Callback | Signature | Trigger |
|----------|-----------|---------|
| `OnJobUpdate` | `func(*Job)` | After every `UpdateJobFields` write, and every `UpdateJobFieldsIf` / `UpdateJobFieldsUnless` / `UpdateJobFieldsUnlessTerminal` write that applied |
| `OnJobChange` | `func(*JobChange)` | Same moment as `OnJobUpdate`; the event carries the full job plus the list of columns written |
| `OnJobAdded` | `func(*JobAdded)` | After `AddJob` |
| `OnJobDeleted` | `func(*JobDeleted)` | After `DeleteJob`, and from `UpdateJobFields` when the row is gone at read-back |
| `OnTrimsChanged` | `func(*TrimsChanged)` | After `AddTrim`, `DeleteTrim` |
| `OnJobsChange` | `func([]*Job)` | Full-list refresh — only the two bulk writers, `BatchSetWatched` and `DeleteJobsAndHistoryForChannel`, dispatch it; a single add, delete or trim change never does |

Every registration method returns an unsubscribe function. Each subscriber slice holds `{id, fn}` entries; unsubscribing removes the entry by id, and the `shrink*Subs` helpers reallocate the slice once its capacity exceeds four times its length so steady-state memory stays reasonable.

**Panic safety:**

- `safeCallJobUpdate(fn, job)` wraps each callback in `defer func() { if r := recover(); ... }()`.
- `safeCallJobChange`, `safeCallJobAdded`, `safeCallJobDeleted`, `safeCallTrimsChanged` and `safeCallJobsChange` do the same for the other kinds.
- A panicking subscriber cannot prevent other subscribers from being notified.

**Notification flow:**

- The per-job kinds (`notifyJobUpdate`, `notifyJobAdded`, `notifyJobDeleted`, `notifyTrimsChanged`) snapshot the subscriber slice under `subMu.RLock` and call the callbacks synchronously on the writer's goroutine, after `db.mu` has been released.
- `dispatchJobsChange(jobs)` is the one asynchronous path: the caller must NOT hold `db.mu`; it snapshots the subscribers and runs them sequentially in a fresh goroutine (with its own top-level `recover`) so the bulk writer returns immediately. A nil slice (no subscribers) is a no-op.

### Schema

**Current version: 28** (`schemaVersion`, `internal/database/migrations.go`)

#### Tables

**jobs** (primary data table):

| Column | Type | Default | Notes |
|--------|------|---------|-------|
| id | TEXT | PRIMARY KEY | Video/stream ID |
| video_id | TEXT | NOT NULL | May differ from id in edge cases |
| url | TEXT | NOT NULL | Source URL |
| title | TEXT | NOT NULL, '' | |
| channel_name | TEXT | NOT NULL, '' | |
| platform | TEXT | 'youtube' | 'youtube' or 'twitch' |
| status | TEXT | NOT NULL, 'Upcoming' | JobStatus string value |
| progress | TEXT | '' | Format: "V:{n} A:{n} C:{n}" |
| percent | REAL | 0 | 0-100 |
| eta | TEXT | '' | |
| speed | TEXT | '' | |
| error | TEXT | '' | |
| created_at | TEXT | NOT NULL | RFC3339 UTC |
| updated_at | TEXT | NOT NULL | RFC3339 UTC, auto-set on every write |
| last_video_seq | INTEGER | NULL | Pointer in Go (*int) |
| last_audio_seq | INTEGER | NULL | Pointer in Go (*int) |
| total_video_seq | INTEGER | NULL | Pointer in Go (*int) |
| total_audio_seq | INTEGER | NULL | Pointer in Go (*int) |
| is_vod | INTEGER | 0 | Boolean (0/1) |
| manually_added | INTEGER | 0 | Boolean (0/1) |
| allow_non_stream | INTEGER | 0 | Boolean (0/1) |
| stream_start_time | TEXT | NULL | RFC3339 |
| stream_end_time | TEXT | NULL | RFC3339 |
| length_seconds | INTEGER | NULL | Duration in seconds |
| download_started_at | TEXT | NULL | RFC3339 |
| thumbnail_url | TEXT | NULL | |
| description | TEXT | NULL | The video's description, which the finalize writes beside the archive as `.description`. A Twitch clip's is built from its ShareClipRenderStatus answer (`twitchClipDescription`, owner decisions FE-32 and FQ-3): "Clipped by <curator>", the clip's title, a link to the source VOD at the clip's start (`https://www.twitch.tv/videos/<id>?t=<h>h<m>m<s>s`, when the answer names a VOD and an offset), and the clip's own description, a blank line between each, each left out when the clip has nothing for it (platform-services.md § Twitch Clips) |
| output_file | TEXT | NULL | Path to the final output file, as written: under the job's directory, so relative to the working directory under a relative one (see `output_directory` below) |
| filename | TEXT | NULL | The archive's path relative to the job's output directory (`output_directory` when set, else the global one), the template's subdirectories included; for a job that finalized as parts … |
| output_directory | TEXT | NULL | The job's own output directory: a per-channel or per-job override, or the global one as it stood at creation, which the monitor and an import store when the channel has none of its own. A monitor-detected row that is still waiting (`Upcoming`, `Queued`, or `Live` before its download is claimed, with no archive and no part) does not keep that value: when its download starts it is rewritten to the channel's override as configured then, else the global directory as set then, and keeps that from there on (owner ruling R2-X1, `followsOutputSetting`, `internal/worker/output_dir.go`). |
| video_width | INTEGER | NULL | Pixels |
| video_height | INTEGER | NULL | Pixels |
| video_fps | INTEGER | NULL | |
| file_size | INTEGER | NULL | Bytes (int64 in Go) |
| chat_status | TEXT | NULL | `pending` / `downloading` / `finished` / `unavailable` / `incomplete`; the terminal value comes from the downloader's OUTCOME (`chatStatusForOutcome`, `internal/worker/orchestrator_chat.go`) … |
| total_chat_messages | INTEGER | NULL | |
| chat_filename | TEXT | NULL | The chat archive's path relative to the job's output directory, resolved as `filename` is |
| chat_file | TEXT | NULL | Path as written (added v2; see `output_directory`) |
| thumbnail_file | TEXT | NULL | Path as written (added v3; see `output_directory`). A YouTube job with no thumbnail in staging saves the first size `i.ytimg.com` serves of `maxresdefault`, `hq720`, `mqdefault`, `hqdefault`, `sddefault`, `default` (`copyAssets`, `internal/worker/orchestrator_mux.go`): the two 16:9 HD sizes in yt-dlp's order, then the 16:9 `mqdefault` ahead of the larger letterboxed 4:3 sizes, which upstream ranks first. Pinned by `TestCopyAssetsTakesTheFirstUsableThumbnailSize` (`internal/worker/orchestrator_mux_test.go`) |
| description_file | TEXT | NULL | Path as written (added v3; see `output_directory`) |
| twitch_quality | TEXT | NULL | The variant the capture is recording, by its playlist name ("chunked", "720p60"): written at the capture start, live and VOD (`StreamProcessor.startTwitchVariant`), and again whenever a split moves the capture to another variant (`recordVariant` in `ExecuteTwitch`). Empty until a capture starts. Both UIs show it as "Quality". Nothing selects from it — until D-T9 it was ALSO the preference: set to it at creation, overwritten at the stream start, and read back as the preference by the next selection. The preference is `quality_preference` |
| twitch_category | TEXT | NULL | |
| channel_avatar_url | TEXT | NULL | |
| selected_video_itag | INTEGER | NULL | YouTube itag, -1 = audio-only |
| selected_audio_itag | INTEGER | NULL | YouTube itag |
| start_time | REAL | NULL | Trim start (seconds, float64) |
| end_time | REAL | NULL | Trim end (seconds, float64) |
| last_recheck_at | TEXT | NULL | RFC3339 |
| quality_preference | TEXT | '' | The quality the job was created to record, e.g. "1080p60", "best" (added v5): the channel's setting, or the manual add's. |
| watched | INTEGER | 0 | Boolean (0/1), watched status (added v8) |
| resume_position | REAL | NULL | Playback resume position in seconds (added v8) |
| chat_offset | REAL | 0 | Chat timing offset in seconds, can be negative (added v9, migrated from player_prefs) |
| auto_retry_count | INTEGER | NOT NULL, 0 | Monitor-driven Twitch flap auto-recovery attempts (added v13); reset by user-driven reinit/resume |
| channel_id | TEXT | NULL | Config channel ID of the monitor channel that created the job (added v16). NULL for manually added and pre-v16 jobs. Set at insert, never updated. |
| queue_priority | INTEGER | NOT NULL, 1 | Backlog marker (added v16): 0 = broadcast or newly discovered VOD (admitted immediately), 1 = backlog VOD (paced by the archive-slots scheduler) |
| incomplete_tail | INTEGER | NOT NULL, 0 | Boolean (0/1), added v17. Marks a `Finished` job whose recording is known to be missing tail segments (finalized behind head after the VOD-branch refresh loop's retries). Staging dir + resume sidecar are preserved instead of cleaned up; Resume is allowed on the flagged job (Retry is NOT — it deletes staging via ReinitializeJob, which would destroy exactly what the flag protects) and unconditionally rewrites this column, so a clean re-run clears it. Not set for a live capture that stopped within the end-of-stream margin of the head with the stream confirmed over (owner ruling R-T1; architecture.md, The incomplete-tail verdict), and cleared once, at the first start of a version that has the rule, on rows flagged before it (Boot leftovers, below). |
| park_reason | TEXT | NOT NULL, `''` | Why the job stopped at `COOKIES?` (added v18). |
| park_identity | TEXT | NOT NULL, `''` | Opaque fingerprint of WHICH account refused the job (added v19), recorded only for a `'membership'` park. A membership park resumes when the current account differs from this one — a durable comparison, so it survives restarts and cannot be consumed by a missed in-process transition. `''` means "parked under an unknown account" and resolves permissively (one retry, not a strand). Credential-derived: `json:"-"`, never serialized to clients. |
| notification_msgs | TEXT | NULL | JSON object added v20, mapping a notification target's key … |
| announce_found | INTEGER | NOT NULL, `0` | Added v22: the job's "Stream Found" is still owed. |
| join_offset_seconds | REAL | NULL | Added v26 (owner decision CJ-3): where in the broadcast a live YouTube capture's recording began, in seconds of the broadcast's (the video's) timeline. |
| live_edge_reason | TEXT | NULL | Added v27 (owner decision PR-R2): why a live YouTube capture recorded from the live edge instead of the broadcast's start — the stream was older than YouTube keeps its segments, its start was gone … |
| prior_output_file | TEXT | NOT NULL, `''` | Added v28 (owner ruling R2-W1): the archive an earlier recording of the job left, kept by a Reinitialize. |

##### filename

The archive's path relative to the job's output directory (`output_directory` when set, else the global one), the template's subdirectories included; for a job that finalized as parts, the base the parts share, without an extension. The player, the chat route and Open Folder join it to that directory, and so does the orphan sweep — to the current global directory as well (`rowRelativeLocations`, `internal/worker/orphans.go`), which still finds a single-file archive after the archive tree is moved and `paths.output_directory` repointed. A split job's base names no file: its parts are found through their segment rows, re-rooted the same way (`output_directory` below)

##### output_directory

The job's own output directory: a per-channel or per-job override, or the global one as it stood at creation, which the monitor and an import store when the channel has none of its own. A monitor-detected row that is still waiting (`Upcoming`, `Queued`, or `Live` before its download is claimed, with no archive and no part) does not keep that value: when its download starts it is rewritten to the channel's override as configured then, else the global directory as set then, and keeps that from there on (owner ruling R2-X1, `followsOutputSetting`, `internal/worker/output_dir.go`). `filename` and `chat_filename` are relative to it. The path columns that lie under it — `output_file`, `chat_file`, `thumbnail_file`, `description_file`, and each part's `file_path` and `chat_file` — are the path as written, this directory joined with the file's name in its own spelling: relative to the working directory when the directory is relative (`paths.output_directory`'s default `./output`, which a job with no directory of its own gets, `internal/worker/worker.go`), absolute when it is absolute; an import's are absolute (`filepath.Abs`, `internal/web/routes/import_routes.go`). A path handed to a program that resolves names against another directory is made absolute first — the part merge's concat list, which FFmpeg reads against its own temp directory (`concatIntermediates`, `internal/engine/muxer_trim.go`). Because each names the directory in its own spelling, the orphan sweep finds it under the current global directory as well after the archive tree is moved and `paths.output_directory` repointed (`rowAbsoluteLocations`, `internal/worker/orphans.go`); a column outside it is found only as stored

##### chat_status

`pending` / `downloading` / `finished` / `unavailable` / `incomplete`; the terminal value comes from the downloader's OUTCOME (`chatStatusForOutcome`, `internal/worker/orchestrator_chat.go`), not its message count — `incomplete` means the capture stopped short, or that finalize could not copy it beside the archive (`copyAssets`, `internal/worker/orchestrator_mux.go`): either way the archive lacks a whole chat, and the value is what makes the staging cleanup keep the capture rather than delete it. A part whose chat cannot be copied is not recorded at all, so its `seg_N` stays unmuxed and shielded until a retry. `pending` until the capture actually starts (both platforms; a Twitch VOD's download-slot wait included), then `downloading`. A user cancel settles a still-running value by the one settle rule (`stoppedChatStatus`, `internal/worker/worker.go`): `downloading` becomes `incomplete` and `pending` is cleared, on the Cancelled row whether the run ended on the Cancel's flag or on its row write alone, ahead of the flag; a shutdown leaves it for the capture to resume. A failed download settles it by the same rule on either platform (owner rulings R-E1, R-E1a, R-H1, `settleErrorChat`, `internal/worker/worker_failure.go`): once the row reads `Error`, `downloading` becomes `incomplete` and `pending` is cleared, through `SettleChatStatus` (`internal/database/database.go`), one statement per in-flight value (`ChatStatus.InFlight`, pending first), each conditional on the status still reading `Error` and the row still holding that value — a verdict the run recorded is kept, a Resume that moved the row on first wins, and the resumed run re-arms the chat (`pending`, written whatever the row reads). A panicked run's Error is settled the same way (`writePanicError`, the write `recordRunPanic` and `Start`'s recover share), and so is the Error a failed Mux writes (`muxJob`'s `writeFailed`), since a row can reach a Mux still in flight, and the one an automatic Twitch mux that could not start writes (`autoMuxNow`). A Twitch capture whose end was unconfirmed records its own verdict before the failure is written, which is not in flight and stands. A pending capture has no `chat.json`, so finalize writes no chat value: an Error row whose pending capture was cleared and is then muxed reaches `Finished` with no chat and its staging removed (`TestAPendingChatSettledOnErrorReachesAMuxAsNoChat`), where the first text of R-E1 (pending → `incomplete`) carried `incomplete` to `Finished` and kept the dir chat-only. Nothing writes over a settle afterwards: a `Stop` that lands before a YouTube downloader's `Start` runs is latched (`stopBeforeRun`, `internal/chat/downloader.go`), so that `Start` returns with no `OnStart`; and a run's panic and `setJobError` stop the run's downloaders and close the gate both YouTube `OnStart` writes go through (`stopRunChats`, `internal/worker/worker.go`; `whileRunning`, `internal/worker/run_chats.go`) before the settle. The values are `database.ChatStatus` constants (`internal/database/types.go`); the dashboard's badge table (`CHAT_STATUS_VARIANTS`, `web/public/modules/utils.js`) holds one entry per non-empty value, pinned by `TestChatStatusSetMatchesTheDashboard` (`cmd/moombox/vocabulary_parity_test.go`)

##### quality_preference

The quality the job was created to record, e.g. "1080p60", "best" (added v5): the channel's setting, or the manual add's. Written at insert and never overwritten. Every variant selection reads it — on Twitch the capture start's, live and VOD, and every re-selection during the capture, never `twitch_quality`. A Twitch row records `"best"` when nothing was named: the Twitch monitor (`newTwitchStreamJob`, `cmd/moombox/monitor_callbacks.go`), the Web add, and `moombox add` (`newCLITwitchJob`, `cmd/moombox/addvideo.go`), which used to record none. An empty value — on a Twitch row from before that rule, or any YouTube row with no preference — selects as `"best"`

##### park_reason

Why the job stopped at `COOKIES?` (added v18). `'auth'` = the request was not signed in (cookies missing or dead); `'membership'` = the request WAS signed in and the platform still refused, so the account simply lacks the channel's membership; `''` = not parked, or parked before this column existed. Meaningful only while status is `COOKIES?` — with five Error-row values. `'mux_after_cancel'` marks a row whose Mux after the operator's Cancel failed (`ParkReasonMuxAfterCancel`, written by `MuxJob`'s failure writes and carried while its Muxes keep failing), so the Cancel's closed gap-fill tail (owner decision FQ-2, `twitchMuxTailOpen`) outlives the Error status; a Retry or Reinitialize clears it. `'twitch_unreachable'` marks a Twitch live job whose stream processing never reached Twitch (a transport failure) on a network the connectivity monitor said was up, which the Twitch monitor re-initialises like the offline flap when it finds the same broadcast live (`ParkReasonTwitchUnreachable`; `jobstate.TwitchAutoRetryPending`, `internal/jobstate/auto.go`), the re-initialisation clearing it. `'twitch_end_unconfirmed'` marks a live Twitch capture that stopped on its unconfirmed-end latch with staging kept, the one row the Twitch monitor's automatic mux takes once it confirms the broadcast over (owner decision D-T4; `ParkReasonTwitchEndUnconfirmed`, `internal/database/types.go`), cleared whenever the staging's mux starts, the automatic one or the Mux action's (`muxJob`, `internal/worker/worker_verbs.go`), and when an automatic mux cannot start — except when it is refused because another operation holds the job's staging (`ErrStagingBusy`), which was no attempt, so the marker stays for the next poll. `'twitch_offline'` marks a monitor-created Twitch live job that found its channel offline at stream processing — the offline flap — which the Twitch monitor re-initialises when it finds the same broadcast live (`ParkReasonTwitchOffline`, the same predicate and budget as `'twitch_unreachable'`), the re-initialisation clearing it. `'twitch_ended_offline'` marks a Twitch capture interrupted mid-broadcast whose channel was offline when Moombox came back: nothing automatic follows, the operator's Mux archives the staged footage, and the marker keeps that Mux's gap-fill tail open (`ParkReasonTwitchEndedOffline`, `twitchMuxTailOpen`); the Mux's Muxing write clears it. Both are named by the stream processor's refusal beside the error text (`twitchOfflineError`, `internal/worker/stream_processor_twitch.go`) and recorded by `setJobError` in its Error write (owner ruling R-I1). Before them the two sub-states were routed by exact equality with the error text; that text is unchanged and still written, and rows from before the reasons are converted at boot (see "Twitch offline park reasons" under Schema Migrations). Every park path (and every error transition, `setJobError`) rewrites it and every un-park path clears it. Drives which recovery sweep may resume the job — see the `COOKIES?` -> `Upcoming` transition rules in `architecture.md`.

##### notification_msgs

JSON object added v20, mapping a notification target's key (first 16 hex digits of SHA-256 over the RESOLVED webhook URL) to the id of the one Discord message that target rewrites in place for this job (`mode = "edit"`). NULL for every job on a separate-mode install, which is the default. Written once per (job, target) on the first successful POST through `UpdateNotificationMsgs` — a silent single-column write that bumps no `updated_at` and wakes no subscriber. A corrupt or half-written value decodes to nil and the job simply posts a new message. A row written by 2.8.9 or 2.8.10 may key an id on a spelling of the webhook that now resolves differently (a `ptb.`/`canary.` host, a trailing slash, a bare `?`, a query's parameter order, a stray `&` or a `wait`); the notifier reads that key when the current one misses — where that spelling's old resolution named the same webhook, which a `discord://ID/TOKEN/?thread_id=…` did not: it resolved to the bare channel webhook, and the channel's ids are not the thread's — and the job's next write stores the id under the current key (`legacyResolvedURL`, `internal/notifications/manager.go`; [operations.md](operations.md)). An old key holding a different id than the current one is a second message — the webhook was configured in two spellings, which those releases ran as two targets — and stays on the row, edited beside the first. Credential-adjacent: `json:"-"`, never serialized to clients.

##### announce_found

Added v22: the job's "Stream Found" is still owed. The YouTube monitor's creator sets it in the INSERT that creates the job (`announcesJobFound` excludes a backlog VOD); the worker's first metadata fetch takes it with `TakeAnnounceFound` — one conditional UPDATE that clears it and reports whether it was set, silent like `UpdateNotificationMsgs` — and sends the embed with what the fetch learned; a cancel (`CancelJob`, or the cancel route without a worker) and every re-run (`ResumeJob`, `ReinitializeJob`, `AutoReinitializeJob`) clear it. In the row rather than in memory so a duplicate insert or a monitor's re-detection of a known job cannot touch it and a restart before the fetch keeps it owed. Notifier bookkeeping: `json:"-"`.

##### join_offset_seconds

Added v26 (owner decision CJ-3): where in the broadcast a live YouTube capture's recording began, in seconds of the broadcast's (the video's) timeline. Written by the single-file finalize (`joinOffsetSeconds`, `internal/worker/recording_start.go`), and by the split finalize from part 1's staged recording (owner decision CK-3, `splitJoinOffsetSeconds`: the staging root, or `seg_<index>` when a `seg_0` dir makes the root no part), from what the staging records: 0 when every input was recorded from sq=0 (`recording-start.json`, `recordedFromStart`); for a late join, the staged fragmented MP4's own `start_time` (its first fragment's decode time sits on the broadcast's timeline), else the input's first sequence times the format's advertised `targetDurationSec` (an MPEG-TS, whose PTS the broadcast does not share; the live HLS path's variants map to no format, so its result carries the extraction's cadence as `SegmentDurationSec`, `advertisedSegmentDuration`; a Mux action from staging has none) — each late input asked in turn; NULL when no input can say, when the start was never recorded, and for every VOD and Twitch capture. A split archive's is where part 1 began, the start of the timeline the import lays the parts end to end on; what fell between parts (an outage, a split's re-open) is not compensated, so a comment's timestamps past a gap land late by its length. A Reinitialize clears it. No backfill: nothing an existing row kept says where it joined. Read by the chapter import's pre-filled offset (`defaultChapterOffset`, § Archive Chapters).

##### live_edge_reason

Added v27 (owner decision PR-R2): why a live YouTube capture recorded from the live edge instead of the broadcast's start — the stream was older than YouTube keeps its segments, its start was gone, every DASH format was DRM-protected (also on a post-live stream, whose HLS holds only its last ~2 h), no GVS token could be minted, or the player's JS challenges could not be solved with no no-JS pair left. Written by the worker when the first setup of the job's first part falls to the live HLS playlist (a restart that resumes into a later part writes nothing: part 1 holds the start) (`recordLiveEdgeStart`, `internal/worker/live_edge.go`), with a `live_edge` notification on every fall (owner decision PE-4); cleared by a Reinitialize. Both UIs' job details show it beside `join_offset_seconds` as "Recorded from the live edge: <reason>, joined H:MM:SS in" (not when the join is 0)

##### prior_output_file

Added v28. The archive an earlier recording of the job left, which a Reinitialize keeps (owner ruling R2-W1). A reset clears `output_file`; when the row named a single-file archive that is on disk (no part rows: a part is deleted by the reset), the same write moves the path here (`resetFieldsKeepingArchive`, `internal/worker/prior_archive.go`), so the row still owns the file and the Files sweep does not offer it (`jobFileLocations`). The row owns the archive's sidecars with it, the files a single-file finalize writes under the same stem (`priorArchiveSidecars`: `.chat.json`, `.chat.raw.jsonl`, `.description` and the thumbnail), whose own columns the reset clears. The next finalize renames the archive aside before it writes anything (`setAsidePriorArchive`), to `<stem>.restart-<unix ts>.mp4` beside it, the name a recovered set-aside recording takes, stores that name here, and renames the sidecars to the same stem (`<stem>.restart-<unix ts>.chat.json`, and so on), so the earlier recording keeps its chat. Moombox never replaces or deletes these files.

What the finalize does when the archive is not where the column says: if a set-aside file with its stem is beside it, the column takes that name (a process that died between the rename and the column write); if its directory can be read and the file is not there, the column is cleared (removed by hand); otherwise (the volume is not mounted) the column is left. An archive that cannot be renamed aside is never written over: a single-file finalize that would land on it stops with the staging kept, a job's only part keeps its ` - partN` name, and a finalize that writes other names goes on (`blockedByPriorArchive`).

A reset of a row that has both an archive of its own and an earlier one still waiting to be set aside moves the column to the newer one and, only after its conditional write applied, renames the older one aside (`setOlderArchiveAside`): under the newer archive's stem when both are in one directory, where the row goes on owning it as a set-aside sibling of the archive it names; in its own directory otherwise, where no row names it and the Files tab lists it. The log names the file in both cases.

Both UIs' job details show the column as "Earlier file: <name> (kept, not replaced)", with the whole path when the file is not in the directory of `output_file` (the row can have followed a changed output directory) or the row names no archive; the Reinitialize confirm names it first (`existingArchives` in the preview). Removing a set-aside file by hand is how the operator gets rid of it: the Files sweep then clears the column of a `Finished`, `Error` or `Cancelled` row whose file is gone from a readable directory (`forgetMissingPriorArchives`), so the line goes the next time Files is listed.

**Indexes on jobs:** `idx_jobs_status(status)`, `idx_jobs_updated_at(updated_at)`, `idx_jobs_video_id(video_id)` (added v4).

**gaps:**

| Column | Type | Notes |
|--------|------|-------|
| id | INTEGER | PRIMARY KEY AUTOINCREMENT |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| gap_from | INTEGER | Start segment number |
| gap_to | INTEGER | End segment number |
| stream | TEXT | "video" or "audio" |

Index: `idx_gaps_job_id(job_id)`.

**trims:**

| Column | Type | Notes |
|--------|------|-------|
| id | TEXT | PRIMARY KEY. `trim_<job id>_<Unix ms>`, not a UUID: the ID of the encode that wrote the file, given when the trim is prepared (`prepare`, `internal/worker/trim.go`) and carried by its `trim_status` frames before the row exists |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| start_time | REAL | Seconds |
| end_time | REAL | Seconds |
| filename | TEXT | `trim/<video id> [<start>s-<end>s].mp4` under the directory of the job's own `filename`, so relative to the JOB's output directory — its `output_directory` when set, else the global one. |
| created_at | TEXT | RFC3339 |
| duration | REAL | Seconds |
| file_size | INTEGER | Bytes, nullable |

##### filename (trims)

`trim/<video id> [<start>s-<end>s].mp4` under the directory of the job's own `filename`, so relative to the JOB's output directory — its `output_directory` when set, else the global one. That column is set far more often than an override implies: a per-channel or per-job override stores its own directory, and the monitor and an import store the global directory as it stood at creation when the channel has none (a monitor's row is brought up to the current settings when its download starts, see `output_directory`). The file is written beside the job's output (or beside the part the range begins in), in `trim/`. Bounds are whole seconds, so a range that rounds to a taken name gets ` (2)`, ` (3)`… (`uniqueTrimBasename`, `internal/worker/trim.go`) rather than overwriting it. The orphan sweep resolves the row against the job's `output_directory`, the current global directory, and `trim/` beside the job's output and each of its parts, and a file any of those names is owned (`trimFileLocations`, `internal/worker/orphans.go`) — the global directory as well as the job's own, so a trim stays owned after the archive tree is moved and `paths.output_directory` repointed at it, while its job still names the old tree

Index: `idx_trims_job_id(job_id)`.

**segments** (added v5) — one row per output *part* of a multi-part job.
Parts are produced by quality splits (resolution changed mid-stream, both
platforms) and by Twitch live gap splits (segments that left the live playlist
window; each part file is internally gapless, and the gap-fill can insert the
archive VOD's footage for them as `- recoveredN` parts). An archive import of a split
recording carries its parts too: `AddJob` inserts them with the row, in its
transaction, so the `JobAdded` both UIs learn of the row by carries them:

| Column | Type | Notes |
|--------|------|-------|
| id | INTEGER | PRIMARY KEY AUTOINCREMENT |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| segment_index | INTEGER | 0-based ordering within a job; stable across restarts (maps to staging dirs: root = 0 unless `seg_0` exists, `seg_N` = N). Filenames use `segment_index + 1` as the part number. A split job whose complete from-the-start download replaces its parts loses its rows: the parts' files stay beside the archive as `.restart-` siblings (`supersedePartsWithVod`, `internal/worker/vod_supersede.go`) |
| unix_start | INTEGER | Unix timestamp |
| unix_end | INTEGER | Unix timestamp |
| quality | TEXT | e.g. "1080p60" |
| filename | TEXT | Part filename: `{resolved template} - partN.mp4` (a job finalizing with exactly one part is renamed back to the plain `{resolved template}.mp4`) |
| file_path | TEXT | The part's path as written, nullable: the job's directory joined with `filename`, so relative to the working directory under the default `./output` and absolute under an absolute directory or an import (see `jobs.output_directory`) |
| file_size | INTEGER | Bytes, nullable |
| video_width | INTEGER | Nullable |
| video_height | INTEGER | Nullable |
| video_fps | INTEGER | Nullable |
| duration_seconds | REAL | Nullable |
| chat_file | TEXT | (v15) Path of this part's chat JSON as written, as `file_path` is, `''` when the part has no chat (pre-v15 rows, chat disabled, YouTube — only Twitch live IRC chat rolls per part) |

Index: `idx_segments_job_id(job_id)`.

Every part of one recording takes the FIRST part's directory and base (`muxSegment`), so a retitle or
channel rename mid-job — a restart re-reads stream info — cannot scatter one recording across names or
folders. A finalize that keeps more than one part pins the job's own columns and assets to the same place
(`pinnedPartLocation`): `filename`, `chat_filename`, the description and the thumbnail sit beside the
parts, not under the freshly resolved template. A finalize left with a single part moves it to the fresh
template's plain name instead (`renameSinglePartToPlain`).

**client_tokens** (added v6):

| Column | Type | Notes |
|--------|------|-------|
| id | TEXT | PRIMARY KEY (UUID) |
| token_prefix | TEXT | NOT NULL, first 8 chars of token for lookup |
| token_hash | TEXT | NOT NULL, scrypt hash of full token |
| label | TEXT | User-assigned label |
| created_at | TEXT | RFC3339 |
| last_used_at | TEXT | RFC3339 |
| last_ip | TEXT | |

Index: `idx_client_tokens_prefix(token_prefix)`.

**history:**

| Column | Type | Notes |
|--------|------|-------|
| video_id | TEXT | PRIMARY KEY |
| added_at | TEXT | RFC3339 |

Capped at 10,000 entries; oldest pruned on insert.

**feed_items** (added v16) — the persistent per-channel discovery store. Every item any discovery source lists (RSS, membership tab, or the full-catalog backfill) is upserted here; the feed monitor's per-cycle walk and archive steps read their scope from this table rather than from a transient candidate list:

| Column | Type | Notes |
|--------|------|-------|
| channel_id | TEXT | PK (composite with video_id) |
| video_id | TEXT | PK (composite with channel_id) |
| title | TEXT | NOT NULL, `''` |
| published | TEXT | RFC3339 UTC. Frozen at insert; only upgraded when a better-precision date arrives |
| date_precision | TEXT | `assumed` / `coarse` / `day` / `exact` / `started` — how trustworthy `published` is; upgrades are monotonic |
| catalog_pos | INTEGER | Position within the source listing; ordering tiebreaker for equal dates |
| source | TEXT | `rss` / `membership` / `videos` / `streams` — which discovery source last claimed the row |
| status | TEXT | `unknown` / `upcoming` / `live` / `vod` / `not_a_stream` — last probe classification (`database.FeedStatus*`, `internal/database/database_feed_items.go`); a probe's `youtube.StreamStatus` is stored through `feedStatusFor` (`post_live` → `vod`) |
| first_seen | TEXT | RFC3339 — the cycle that first inserted the row (new-vs-backlog discriminator) |
| recheck_at | TEXT | NOT NULL, `''`. Added v23: when the row is next probed after a broadcast end that rests on weak signals the monitor's end check could not confirm (RFC3339 UTC; `''` = none pending). Such a row is stored `vod` — ended for announcing. Until this instant it is in the scope read only where its date or status would put any row (the restart carve-out still probes it every cycle there, without the end check), and from it wherever its date is (`FeedScope`'s Q3; [platform-services.md](platform-services.md) § Stream Status Classification) |
| recheck_step | INTEGER | NOT NULL, `0`. Added v23: re-checks scheduled so far, which picks the next wait (`endRecheckBackoff`, `internal/monitor/utils.go`: 1 h, 6 h, then daily) |

`recheck_at` and `recheck_step` are written only by `ApplyProbeToFeedItem`, in the same statement as the status they qualify: an unconfirmed end's `vod` and its re-check land together, and every other probe write clears them.

**Indexes on feed_items:** `idx_feed_items_window(channel_id, published DESC, catalog_pos ASC, video_id ASC)` (the archive-scope read), `idx_feed_items_status(channel_id, status)`, `idx_feed_items_recheck(channel_id, recheck_at, video_id)` (v23, the due-re-check read).

**twitch_gap_fills** (added v25) — the finished live Twitch jobs owed a gap-fill from their broadcast's archive VOD, and how each one's fill ended: `job_id TEXT PRIMARY KEY`, `plan TEXT` (the worker's JSON: the parts' wall-clock coverage, the spans the capture lacks, the rendition it recorded, the deadline, the attempts, the last reason the VOD was not ready, whether the capture saw its broadcast end, and, once the fill is over, its outcome — filled with N spans, or kept as recorded and why) and `next_check_at TEXT` (RFC3339 UTC, the only field the database reads, `DueGapFills`). A row is written when such a job finalizes (or, for an Error or Cancelled live row, is muxed from its staging by the Mux action or the automatic mux, owner decision FE-25). When its fill is done or its 24 h window runs out the row keeps the outcome for the job's details (the job API's `gapFill`, owner decision PR-R5), looked at weekly only to remove it once the job is gone; it is removed at once when its job is gone or no longer Finished before the fill ends, and when a later capture of the job finalizes with nothing to fill, by its own finalize or by a Mux of it (architecture.md § Twitch Archive-VOD Gap-Fill).

**feed_reprobe_owed** (added v24) — the channels still owed the upgrade's one reset of their misfiled rows: a single `channel_id TEXT PRIMARY KEY`. Before the status probe's inconclusive rule (platform-services.md § The Status Probe's Blind Refusals) a refusal that said nothing about the broadcast — the bot wall, a made-for-kids video — was stored `not_a_stream`, which the walk never probes again, so on a channel archiving live content only a stream it hid was never recorded (one that archives VODs too had ARCHIVE's refresh probe re-ask such a row every cycle while it was in the window and no history row stood against it); nothing on a row tells a misfiled one from a genuine upload. The v24 migration records every channel holding a `not_a_stream` row here, and resets nothing itself: the reset is the walk's scope's, and the scope is configuration (the cycle's cutoff, from `archive_window_days` or the channel's override, and the membership-discovery toggle), which the database never reads.

The feed monitor's next cycle for the channel settles it (`ResetMisfiledFeedItems`, called by `checkChannel` before its walk): every `not_a_stream` row inside the scope it will have as `unknown` — FeedScope's Q1 (published at or after the cycle's cutoff) and Q2's unresolved arm (an `assumed`-dated row, or an RSS row first seen inside the window whatever its `<published>`, which is only the announcement: a waiting room announced before the window, the wall reached through that arm alone), membership rows only while discovery is on — goes back to `unknown`, a never-probed row's state, and the channel's record is deleted in the same transaction. That cycle's walk then probes each such row once: a misfiled stream is found, a genuine upload answers `not_a_stream` and settles again, and a row the wall still refuses is held on the walk's backoff ladder (1 h, 6 h, daily — platform-services.md § The Status Probe's Blind Refusals), not probed every cycle. Rows outside the scope keep `not_a_stream`, and keep it afterwards — the reset is once, on the upgrade (owner decision, 2026-10-31).

A channel disabled at the upgrade keeps its record until it is walked again; a removed channel's goes with its feed history (`DeleteChannelFeedData`). A fresh install's table is empty: its `not_a_stream` rows were filed under the inconclusive rule.

**channel_state** (added v16) — per-channel bookkeeping for the feed-history feature:

| Column | Type | Notes |
|--------|------|-------|
| channel_id | TEXT | PRIMARY KEY |
| backfilled_at | TEXT | RFC3339 — when the full-catalog backfill last completed; NULL = never backfilled (sweep re-queues) |
| backfilled_window_days | INTEGER | Window depth that backfill covered — a later, wider `archive_window_days` triggers a deeper rescan |
| backfilled_with_membership | INTEGER | Boolean — whether the membership tab was included; enabling membership later triggers a rescan |
| backfill_state | TEXT | Resumable scan cursor (JSON: the window its done tabs were judged against, and per-tab continuation / next position / done); cleared on completion or deliberate restart, and discarded by a retry at a wider window; kept when the channel is disabled mid-scan, so enabling it again resumes |
| last_rss_ok_at | TEXT | RFC3339 — last successful RSS fetch; the "established channel" gate |

**Schema version:**

Tracked via SQLite's built-in `PRAGMA user_version` (since v11). Older databases created with the legacy `schema_version` table are auto-migrated on first open.

### Schema Migrations

Migrations are forward-only and run at startup in `Database.migrate()`. `PRAGMA user_version` is authoritative. On a fresh DB the PRAGMA reads 0 and `createSchema` is executed followed by a PRAGMA set to the current version. On a pre-v11 DB the PRAGMA still reads 0, so migrate() falls back to reading the legacy `schema_version` table, carries the value forward into PRAGMA, runs any pending migrations, and the v11 block drops the legacy table.

| Version | Changes |
|---------|---------|
| v1 | Initial schema (jobs, gaps, trims, history, and a last-video-per-channel table that v16 later dropped) |
| v2 | Added `chat_file` column to jobs; backfilled from `output_file` + `.chat.json` extension |
| v3 | Added `thumbnail_file` and `description_file` columns; backfilled by checking disk for `.jpg`/`.webp`/`.png` and `.description` files |
| v4 | Added `idx_jobs_video_id` index (used by `HasActiveJob`, `AddToHistory`) |
| v5 | Added `quality_preference` column to jobs; created `segments` table with index |
| v6 | Created `client_tokens` table with `idx_client_tokens_prefix` index |
| v7 | Created `player_prefs` table (video_id PK, chat_offset) — deprecated in v9 |
| v8 | Added `watched` and `resume_position` columns to jobs for watch tracking |
| v9 | Added `chat_offset` column to jobs; backfilled from `player_prefs` via video_id join |
| v10 | Dropped `player_prefs` table (superseded by `jobs.chat_offset` in v9); removed from `createSchema` for fresh installs |
| v11 | Replaced custom `schema_version` table with SQLite's built-in `PRAGMA user_version`. Existing DBs auto-migrate: legacy value carried forward, then the table is dropped |
| v12 | Added `idx_history_added_at` index to the `history` table; speeds up the `pruneHistory` ORDER BY ... LIMIT subquery that previously did a full scan on every `AddToHistory` |
| v13 | Added `auto_retry_count INTEGER NOT NULL DEFAULT 0` column to `jobs`. Tracks monitor-driven Twitch flap auto-recovery attempts (capped at `worker.MaxTwitchAutoRetries`). User-driven `ReinitializeJob`/`ResumeJob` reset to 0; auto-recovery's `AutoReinitializeJob` increments |
| v14 | Normalized NULLs in the v2/v3-added columns (`chat_file`, `thumbnail_file`, `description_file`) to `''` — pre-backfill legacy rows failed every scan and vanished from the UI. Swept orphaned `gaps`/`trims`/`segments` rows accumulated while foreign-key enforcement was silently off (the pre-fix DSN used parameters modernc ignores); job IDs are video IDs, so re-adding a deleted video would have resurrected the old job's child rows |
| v15 | Added `chat_file` column to `segments` — Twitch live jobs roll the chat file at every part boundary (gap/quality split), and each part's chat is copied beside its video and recorded on the segment row |
| v16 | Feed-history discovery store: created `feed_items` (with `idx_feed_items_window` + `idx_feed_items_status`) and `channel_state` tables; added `channel_id` and `queue_priority INTEGER NOT NULL DEFAULT 1` columns to `jobs` (backlog scheduling); dropped `last_videos` (superseded by the store). ALTERs are guarded by duplicate-column suppression — `user_version` is written after the block, so a crash mid-migration re-runs the whole block |
| v17 | Added `incomplete_tail INTEGER NOT NULL DEFAULT 0` column to `jobs`: flags a `Finished` job whose recording is known to be missing tail segments; Resume clears it on a clean re-run (Retry is gated out for a flagged Finished job — it would destroy the preserved staging) |
| v18 | Added `park_reason TEXT NOT NULL DEFAULT ''` column to `jobs`: records WHY a job parked at `COOKIES?` so the credential-recovery sweeps can tell a dead-cookie park from a not-a-member one. No backfill — nothing on a pre-v18 row says retroactively which it was, so they keep `''` and therefore their existing resume behavior |
| v19 | Added `park_identity TEXT NOT NULL DEFAULT ''` column to `jobs`: the account fingerprint a membership park was refused under, so a credential sweep can tell a real account change from a session rotation. No backfill — the value is a fingerprint of credentials as they were at park time and cannot be reconstructed afterwards |
| v20 | Added `notification_msgs TEXT` (nullable) to `jobs`: the per-target Discord message ids an edit-mode notification target rewrites in place. No backfill — an id exists only once a message has been posted, and there is nothing to reconstruct for jobs that predate the column |
| v21 | A version bump with nothing to apply. |
| v22 | Added `announce_found INTEGER NOT NULL DEFAULT 0` column to `jobs`: the job's "Stream Found" is still owed, set at insert by the YouTube monitor's creator and taken by the worker's first metadata fetch. No backfill — every existing row was announced, if at all, by the code that created it. Pinned by `TestMigrationV22UpgradesAV21Database` and `TestTakeAnnounceFoundIsOnce` (`internal/database/migrations_v22_test.go`) |
| v23 | Added `recheck_at TEXT NOT NULL DEFAULT ''` and `recheck_step INTEGER NOT NULL DEFAULT 0` columns to `feed_items`, and `idx_feed_items_recheck(channel_id, recheck_at, video_id)` … |
| v24 | Created `feed_reprobe_owed(channel_id TEXT PRIMARY KEY)` and filled it with every channel holding a `not_a_stream` feed row … |
| v25 | Created `twitch_gap_fills(job_id TEXT PRIMARY KEY, plan TEXT NOT NULL, next_check_at TEXT NOT NULL)`: one row per finished live Twitch job owed a gap-fill from its broadcast's archive VOD (owner decision D17, architecture.md § Twitch Archive-VOD Gap-Fill). Nothing to backfill: a capture finished before it has no coverage to plan from. Pinned by `TestGapFillRows` (`internal/database/database_gapfill_test.go`) |
| v26 | Added `join_offset_seconds REAL` (nullable, no default) to `jobs`: where in the broadcast a live YouTube capture's recording began (owner decision CJ-3), written at finalize. No backfill — existing rows stay NULL, the import's offset then starting at 0 as before. Pinned by `TestMigrationV26` (`internal/database/migrations_v26_test.go`) |
| v27 | Added `live_edge_reason TEXT` (nullable, no default) to `jobs`: why a live YouTube capture recorded from the live edge (owner decision PR-R2). No backfill — a row from before it never said why. Pinned by `TestMigrationV27` (`internal/database/migrations_v27_test.go`) |
| v28 | Added `prior_output_file TEXT NOT NULL DEFAULT ''` to `jobs`: the archive a Reinitialize keeps (owner ruling R2-W1). No backfill: a reset from before the column left no record of the archive it disowned. Pinned by `TestMigrationV28` (`internal/database/migrations_v28_test.go`) |

#### Twitch offline park reasons (data backfill, no version)

Owner ruling R-I1. `backfillTwitchOfflineParkReasons` (`internal/database/park_reason_backfill.go`) runs at the end of `migrate()` on every `Open` of an existing database: a read for a row to convert, and only when there is one, one statement — an `Error` row with an empty `park_reason` whose `error` is exactly one of the two frozen texts (`LegacyTwitchOfflineError`, `LegacyTwitchEndedOfflineError`, `internal/database/types.go`) is given `'twitch_offline'` or `'twitch_ended_offline'`. Nothing else on the row moves (`updated_at` included), a row another reason claimed or one no longer in `Error` is left alone, and a failure is logged, not fatal. An `Open` with nothing to convert — every one but the first after the update, and so in practice every `moombox add`, which opens the running daemon's live database — takes no write lock and does not wait for the daemon's writer. When `moombox add` is the `Open` that converts (a row an older binary wrote, then the update), the daemon is not told: it reads both shapes alike this release, and an open dashboard shows the row's reason from its next update; and since `add` opens with no logger, a failure there is silent. Pinned by `TestOpenBackfillsTwitchOfflineParkReasons`, `TestOpenWithNothingToBackfillTakesNoWriteLock` and `TestOpenSurvivesABackfillThatCannotWrite` (the failure path: the write lock held, one warning, the row untouched) (`internal/database/park_reason_backfill_test.go`).

It carries no schema-version bump, on purpose:

- **An older binary on a backfilled database behaves as before.** Nothing in the schema changed, so there is nothing for the downgrade guard to protect: the last release (v2.9.7) keys the offline flap on the error text, which the backfill and the new writers leave byte for byte as it was, and has no reader of the ended-while-offline text at all (it writes it, and has no gap-fill whose tail the text could decide); it compares `park_reason` only for equality with the values it knows (`membership`, `twitch_end_unconfirmed`, `twitch_unreachable`), so a value it has never seen reads as "none of mine" in the worker, the monitor, the cookie sweeps, the TUI and the dashboard alike. A bump would have refused a database to a binary that reads it correctly. The binaries this spares are the schema-27 builds of `main` from before this change (a self-update rolled back to one, or an older build run by hand): v2.9.7 itself is schema 23 and is already refused a schema-27 database by the v24–v27 changes, whatever this arc did. The text-keyed Mux tail and `mux_after_cancel` exist only in unreleased builds of `main`, which read an unknown value the same way.
- **A newer binary on an older database** converts the rows at its first boot, and for one release its readers also accept the bare text with no `park_reason` (`legacyOfflineFlapRow` in `internal/jobstate/auto.go` and its twin in `web/public/modules/utils.js`; `legacyEndedOfflineRow` in `internal/worker/twitch_gapfill_plan.go`), which covers a row whose backfill failed.
- **A version block runs once.** Rows an older binary writes after a rollback would keep the bare text for good; run on every `Open`, the backfill converts them at the next boot, which is what lets the reader fallback be deleted in the release after this one.

The error texts themselves must stay as they are for as long as a binary that keys on them can run against a database this one wrote; the writers' constants are held to the frozen texts by `TestLegacyOfflineTextIsTheWritersText` (`internal/jobstate/auto_test.go`).

#### v21

A version bump with nothing to apply. Development builds' v21 added a `twitch_quality_preference` column to `jobs` (D-T9), filled at startup for older Twitch rows; before any release the Twitch preference was folded back into `quality_preference`, which every row already carried and the quality-split re-selections already read, and the column and both of its backfills went. The bump stays because a database those builds migrated reads 21, which a binary at 20 would refuse as a downgrade; such a database keeps the column, unread and harmless (`NOT NULL DEFAULT ''` fills it on every insert, none of which names it).

#### v23

Added `recheck_at TEXT NOT NULL DEFAULT ''` and `recheck_step INTEGER NOT NULL DEFAULT 0` columns to `feed_items`, and `idx_feed_items_recheck(channel_id, recheck_at, video_id)`: the end re-check of a broadcast the monitor read as ended on weak signals its end check could not confirm (when the row is next probed, and how many re-checks have been scheduled). No backfill — no release before this one stored a row `vod` on a weak end, so no existing row is owed a re-check.

#### v24

Created `feed_reprobe_owed(channel_id TEXT PRIMARY KEY)` and filled it with every channel holding a `not_a_stream` feed row: the one reset of rows the status probe misfiled `not_a_stream` before its inconclusive rule, owed here and made by the feed monitor's next cycle for each channel, inside that cycle's walk scope (`ResetMisfiledFeedItems`; see **feed_reprobe_owed** above). No row is reset by the migration itself — the scope is configuration.

Each migration uses `ALTER TABLE ADD COLUMN` with duplicate-column error suppression (columns may already exist from partial migrations). Backfill queries run against existing data where applicable.

### Job Status Lifecycle

`JobStatus` is `type JobStatus string` with the following constants:

| Constant | Value | Meaning |
|----------|-------|---------|
| `StatusQueued` | `"Queued"` | Backlog VOD resting state: waits for one of its channel's `archive_slots`; only the worker's scheduler admits it (never startup recovery or the heartbeat poller) |
| `StatusUpcoming` | `"Upcoming"` | Stream is scheduled but not yet live |
| `StatusLive` | `"Live"` | Stream detected as live, waiting to start download |
| `StatusDownloading` | `"Downloading"` | Actively downloading segments |
| `StatusMuxing` | `"Muxing"` | FFmpeg muxing video + audio + chat |
| `StatusFinished` | `"Finished"` | Download and mux completed successfully |
| `StatusError` | `"Error"` | Failed with error message in `error` field |
| `StatusCancelled` | `"Cancelled"` | User-cancelled |
| `StatusCookies` | `"COOKIES?"` | Needs cookie refresh to continue (special auth-failure state) |

**Normal flow:** `Upcoming` -> `Live` -> `Downloading` -> `Muxing` -> `Finished`

A VOD skips `Live`, and stays at the status it came in with (`Upcoming` for a fresh one) while it queues for a download slot — its progress line says so — until `ExecuteWithChat` (`ExecuteTwitch` for a Twitch VOD) writes `Downloading` with the slot held.

**Backlog flow:** backlog VODs only enter as `Queued` and are admitted to `Upcoming` by the archive-slots scheduler; broadcasts and newly discovered content never wait in `Queued`. A backlog VOD returns to `Queued` from a cookie repair (its feed row or not: a removed channel's kept backlog has lost it to the departure prune, and `NextQueuedJobs` LEFT-JOINs `feed_items`) and from a transient pre-download fetch failure or a download that ran out of disk, held from re-admission for a backoff and only up to three runs in a row before it ends in `Error` (`requeueBacklogAfterTransientFailure` and `requeueBacklogAfterDiskFull`, `internal/worker/backlog_retry.go`). A YouTube rate limit — its verdict, or a 429 on every client of the cascade — sends it back to `Queued` too, held for an hour and outside that budget, so it never ends in `Error` on one (`requeueBacklogAfterRateLimit`). The scheduler admits nothing while the connectivity monitor reports offline, nor from the moment the output volume reaches `disk_critical_percent` until usage is 2 points below it (`Scheduler.diskGateClosed`, `internal/worker/scheduler.go`), nor for an hour after a YouTube rate limit on any job's fetch (`Scheduler.holdForRateLimit`, `internal/worker/rate_limit.go`).

**Error paths:** Any status -> `Error`, `Cancelled`, or `COOKIES?`

**Terminal states** (checked via `Job.IsTerminal()`): `Finished`, `Error`, `Cancelled`.

### Go Type Conventions

| SQL Type | Go Type | Notes |
|----------|---------|-------|
| TEXT (timestamps) | `string` | RFC3339 format, compared as strings |
| TEXT (status) | `JobStatus` | Type alias for string |
| INTEGER (nullable) | `*int` | nil in Go = NULL in SQLite |
| INTEGER (file_size) | `*int64` | Handles files > 2GB |
| REAL (nullable) | `*float64` | For trim times |
| INTEGER (boolean) | `bool` | Converted via `boolToInt()` on write; scanned as int on read |

### Per-Job Log Buffers

The database maintains in-memory per-job log buffers (`jobLogs map[string][]string`) for real-time log viewing in the Web UI and TUI: the dashboard's job dialog reads one through `GET /api/jobs/{id}/logs`, the TUI's `O L` Job Log overlay through `db.GetJobLogs` directly (`OnGetJobLogs`, wired in `cmd/moombox/tui_wiring.go`). These are not persisted to SQLite.

- `RouteLogToJobs(line)` is the only writer: it scans the ROUTED SET of job IDs (`logRouted`, a second map beside `jobLogs`) and appends the line to the first matching buffer (substring match on job ID in log line). Each buffer is capped at 200 lines; when exceeded, it is trimmed to the last 100.
- **A component that knows nothing of jobs gets a logger that adds the job ID.** The engine downloaders and both Twitch chat downloaders (`ChatDownloader` logs `channel=<login>`, `VodChatDownloader` `vodID=<digits>`) are built with `newScopedLogger(…, "jobID", job.ID)` (`internal/worker/scoped_logger.go`), which appends `jobID=<id>` to every line; without it a Twitch job's chat reconnects, auth downgrades and fetch errors reached the global log only. Pinned for the chat downloaders by `internal/worker/twitch_chat_job_log_test.go`.
- **It runs inside the log call**, as the logger's line router (`Logger.SetLineRouter`, wired by `wireLogForwarding` in `cmd/moombox/monitor_callbacks.go`), never on a subscriber's goroutine. The routed set changes synchronously — a status write that makes a job terminal untracks it inside `UpdateJobFields` — so a line has to be matched against the set as it stood when the line was logged. Routed by the log forwarder, a line logged just before a terminal write (`setJobError`'s "job error") would lose the race to the untrack for roughly one failed job in twelve and be missing from the very log an operator opens after a failure; the brackets `cleanupStagingAfterMux` and `RecoverAsides` put around their last lines would lose it the same way. A subscriber that falls behind does not cost a job its lines either: the logger drops lines for a full subscriber channel, not for the router.
- `TrackJobForLogs(jobID)` starts routing to a job and initializes its buffer (nil slice).
- `UntrackJobForLogs(jobID)` stops routing to a job and KEEPS its buffer — the job that just failed is the one whose log an operator opens next.
- `SyncJobLogTracking(jobs)` applies both rules to a whole list: non-terminal jobs tracked, terminal ones untracked. The boot seed calls it (`cmd/moombox/monitor_callbacks.go`); after boot routing follows each job's own events, through that file's `syncJobLogRouting` — from `OnJobAdded` (the ZIP import really does add a `Finished` job) and from `OnJobChange` whenever the `status` column was written, which is what re-routes a job that LEAVES a terminal state: `/retry`, `/resume` and auto-retry each resurrect a job with a plain `UpdateJobFields(status=…)`.
- The per-line cost is a substring scan per TRACKED id — proportional to the number of LIVE jobs, not constant, and not to the size of the `jobs` table: ~99 ns at 5 live, ~76 µs at 5,000 live (the pre-fix cost, when every row the database had ever held was tracked). Live jobs are bounded by the archive slots and `num_parallel_downloads`; a long-lived tracked set is what must never come back.
- `ClearJobLogs(jobID)` removes a specific buffer and its routing. `OnJobDeleted` drops a single deleted job's that way (`onJobDeleted`), and `DeleteJobsAndHistoryForChannel` drops each row it deleted itself, under `db.mu` before it returns (`clearJobLogsOf` over the ids its `DELETE … RETURNING id` named) — so a job `AddJob` re-creates under one of those ids waits for it, and keeps the routing its own `OnJobAdded` gives it.
- The bulk writers' `OnJobsChange` fan-out (`onJobsChange`) does nothing to routing. Neither `BatchSetWatched` nor `DeleteJobsAndHistoryForChannel` writes a status, so neither runs a `SyncJobLogTracking` over the whole table: that would untrack every terminal job — including one that `RecoverAsides` or `cleanupStagingAfterMux` was routing its last lines to, so a Mark Watched on any rows during a recovery would send the rest of its progress to no job's log. The `PruneJobLogs` it ran after that dropped every buffer and routed id missing from its list, which is read at commit and reaches the subscribers later, on a goroutine of their own: a job `AddJob` created in between — a stream a monitor found, a `Queued` row a backfill scan added — lost the routing its `OnJobAdded` had just set up, and logged nothing to its own log until its next status write.

### Auxiliary Data Operations

- **History:** `HasProcessed(videoID)` / `AddToHistory(videoID)` records the video IDs a job was created for (10,000 cap, `historyCap`: the oldest rows by `added_at` are pruned on insert, and re-recording a present ID does not refresh its date). Only the host writes it, at job creation (`cmd/moombox/monitor_callbacks.go`) — the monitors' re-add guard and both archive steps' VOD gate read it, so a row for a video that was merely skipped or failed to probe would keep it from ever being archived.
- **Feed-history store** (`database_feed_items.go`): `UpsertFeedItem` (insert-or-update, reports whether the row is new), `ApplyProbeToFeedItem` (probe writes status/title/date back, and the end re-check with them — scheduled for an unconfirmed broadcast end, cleared by every other write), `FeedScope` (the window + always-covered upcoming/live read, plus the unresolved arm: `unknown` rows at `assumed` precision, and `unknown` RSS rows first seen inside the window — an RSS `<published>` is the announcement time, so a stream scheduled further ahead than the window is older than the cutoff the moment it is first seen, and without this arm it was never probed, even once live; for the same reason a VOD-family probe of an RSS row dated before the cutoff fetches the real date (`probeRowDated`) rather than trusting the 'exact' announcement, which windowed out the VOD of a broadcast that aired inside the window; and the re-check arm, Q3: rows whose end re-check is due, wherever their date is, while a row whose re-check is still pending is in the other arms exactly as any row would be — the backoff only adds rows to scope), `SetFeedItemSource`, `RenumberCatalog`/`ListFeedOrderRows` (backfill ordering pass), `SaveBackfillCursor`/`LoadBackfillCursor`, `SetChannelBackfilled`/`GetChannelBackfill`, `SetChannelRSSOK`, `GetChannelEstablished`, `ListFeedChannelIDs`, `DeleteChannelFeedData` (channel-removal prune).
- **Client tokens:** Full CRUD operations (`AddClientToken`, `GetClientTokenByPrefix`, `ListClientTokens`, `UpdateClientTokenUsage`, `DeleteClientToken`, `DeleteAllClientTokens`).
- **Job stats:** `GetJobStats()` returns aggregate counts and sizes via a single SQL query with CASE expressions.

---

## Configuration (internal/config/)

### Format and Parsing

Configuration is TOML, parsed via `BurntSushi/toml`. The full config type is `MoomboxConfig`, a struct with nested section structs and TOML struct tags.

### File Search Order

`Load(customPath)` (`internal/config/config.go`) has two modes, and the flag decides which.

**An explicit `-config <path>` is AUTHORITATIVE.** That file is the only one considered. If it exists it
is loaded; if it does not, `Load` returns `Defaults()` and the named path stays the save target, so the
first write creates the file exactly where it was asked for. There is no fall-through — before O-Y the
search paths were appended unconditionally, so `-config /not/yet/there` silently adopted
`~/.config/moombox/config.toml` when one happened to exist, and the operator asked for one file and got
another (CORE-17).

**Without the flag** the search runs in order, first hit wins:

1. `<cwd>/config.toml`
2. `<cwd>/config/config.toml`
3. `~/.config/moombox/config.toml`
4. If none found: use `Defaults()` with no file loaded (`ConfigLoaded = false`)

**Only "does not exist" means "no config here".** A named `-config` path whose `os.Stat` fails any other
way (access denied, an I/O error, a device that is not ready) makes `Load` return an error, and the
process ends through the startup-error path (exit code 3) instead of booting a first-run install over a
config it could not reach. In the search, a candidate that fails that way is skipped with a
`warning: skipping config <path>: <error>` line on stderr, so an unreadable `~/.config` cannot stop an
install in the working directory from booting.

**Saves go back to the file that answered.** The location that was read is recorded in `LoadedFrom` (`internal/config/types.go`), and `cmd/moombox/services.go` points the run's config path at it before building the store — so a config found in `./config/` is written back to `./config/` rather than forked into a fresh `./config.toml` that would shadow it on the next boot (the first write is often the boot-time `NeedsAutoPersist` flush, so the fork would happen without anyone touching a setting). When NOTHING is found, the path that was asked for stays the target and the file is created there — `storePathFor` in `cmd/moombox/helpers.go` is that rule (with no `-config`, `<cwd>/config.toml`).

**Out-of-range values are replaced, and said so.** `loadFromFile` runs `Normalize`, which puts each
value `Validate` rejects back to its default; the issues `Validate` found first are kept in
`NormalizedOnLoad` (`internal/config/types.go`) and boot logs one Warn per issue. The default is what
the next save writes, so without the line a hand-edited value disappeared from the file unexplained.

**Retired keys are ignored, and said so.** `retiredKeys` (`internal/config/config.go`) lists keys an
older file may still hold that nothing reads — today `downloader.po_token` and `downloader.visitor_data`,
which were saved as a "manual PO token override" no code path ever consulted (PO tokens are minted per
session through `[bgutils]`). `loadFromFile` records the ones present in `IgnoredOnLoad`, boot logs one
Warn per key, and the next save leaves them out of the file.

**Unknown keys are ignored, and said so.** A key the file holds that nothing reads (a misspelt key or
section, or a key under the wrong section) leaves its setting on the default. `loadFromFile` keeps the
struct decode's list of undecoded keys, `unknownKeysIn` (`internal/config/config.go`) drops the ones in
`legacyKeys` (what `migrateOldFormat` reads, keys dropped from the struct without a migration, and the
retired keys above), and the rest go in `UnknownOnLoad` (`internal/config/types.go`). Boot logs one Warn
per key; an unknown section is named once, without its keys. Nothing else changes: the file still loads,
and the next save leaves the key out.

### Configuration Sections

#### [network]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| Port | int | 774 | `port` | Valid range: 1-65535 |
| NetworkAccess | string | "localhost" | `network_access` | "localhost", "lan", "external", or "public" — "public" is a config-file-only synonym for "external" (rejected as an API input, absent from both UIs) |
| HTTPSEnabled | bool | false | `https_enabled` | Restart-required: the listener's scheme is fixed when the web server starts. Everything local that must reach that listener — the TUI's API client, `O W`, the yt-dlp plugin status and install — asks the bound server (`Server.TLSActive`) rather than reading this field, so a save without the restart cannot point them at the wrong scheme. |
| TLSCertPath | string | "" | `tls_cert_path` | |
| TLSKeyPath | string | "" | `tls_key_path` | |
| PasswordHash | string | "" | `password_hash` | scrypt hash, omitted from JSON; a plaintext value is auto-converted on the next start |
| ClientTokenTTLDays | int | 365 | `client_token_ttl_days` | Lifetime of a "remember me" client token, enforced on the server against the token's `created_at` (not only the cookie's `Max-Age`). Valid range: 1-3650. Also enforced by `PUT /api/config` (`validateConfigUpdates`), so an out-of-range value is a field error, not a silent clamp. |
| TrustForwardedProto | bool | false | `trust_forwarded_proto` | Only behind a TLS-terminating proxy that strips the client's own header. Hot-reloadable: a config save re-applies it (`OnTrustForwardedProtoChange`). |
| TrustedProxies | []string | `[]` | `trusted_proxies` | Reverse-proxy IPs/CIDRs whose `X-Forwarded-For` is honored. Entries must parse as an IP or CIDR (invalid ones are reported and dropped). Hot-reloadable — no restart. See [security.md](security.md) |
| PublicURL | string | "" | `public_url` | Externally reachable dashboard base URL. |

##### PublicURL

Externally reachable dashboard base URL. Empty means unset. The notification manager links a job embed's title to `{public_url}/#job=<id>`; the web server's origin checks read it too — on `external`/`public` its host is a name a loopback or private peer may address the dashboard by (`externalHostRefused`), and on `localhost`/`lan` its port is one an Origin may name (`originPortServed`, `internal/web/middleware.go`; [security.md](security.md) § 4. CORSMiddleware). Must be an absolute http(s) URL with a host and no query, fragment, or userinfo — validated by `ValidatePublicURL`, which also trims a trailing slash. An unusable value is reported and cleared, never substituted. A value with an `@` anywhere in it is refused as userinfo before it is parsed, with a message that quotes nothing (a path holding an `@` goes too): net/url ends the authority at the first `/`, `?` or `#`, so a password holding one is read as host:port and url.Parse's refusal (`invalid port ":<password>" after host`) would quote it. Any other parse error carries only url.Parse's cause, never the value. The report quotes the value with everything before its last `@` cut out (`redact.URLUserinfo`, `internal/redact/url.go`): a hand-edited `https://user:password@host` is refused for exactly that part, and `Load` turns the report into a boot Warn on every start. Hot-reloadable — read at send time and per request.

#### [paths]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| DatabasePath | string | "./moombox.db" | `database_path` |
| LogFilePath | string | "./moombox.log" | `log_file_path` |
| OutputDirectory | string | "./output" | `output_directory` |
| StagingDirectory | string | "./staging" | `staging_directory` |
| FfmpegPath | string | "" | `ffmpeg_path` |

`database_path` also places `open-alerts.json`, the notifications sent and not yet closed and the backlog scheduler's two holds — the disk gate's and the YouTube rate limit's — in the same directory (`openAlertsPath`, `cmd/moombox/open_alerts.go`; see operations.md, Event Types), so a volume or a moved install carries both. The worker keeps `end-of-stream-tails.json` there as well, the record of its one-time pass over old incomplete-tail flags (Boot leftovers).

A relative `database_path` resolves against the working directory of the process that opens it, as the OS resolves it — on Unix a `..` after a symlinked working directory climbs out of the symlink's target. The daemon records the file it opened (`Database.File`, an earlier release's file when `database.ResolveFile` keeps one), absolute, beside its single-instance lock, and `moombox add` writes to that file while the daemon runs, refusing a config whose `database_path` resolves elsewhere; with no daemon running it resolves a relative one against its own working directory and refuses when no database is there (`resolveAddDatabase`, `cmd/moombox/daemon_record.go`; [architecture.md](architecture.md) § Subcommands and Flags).

`ffmpeg_path` is hot-reloadable: a save from either UI calls `TrimService.SetFfmpegPath` and `DownloadWorker.SetFfmpegPath` (the orchestrator's muxer), so new trims (a finished job's post-download one included — it runs through the trim service), muxes, probes and part merges use the new binary; operations already running keep the muxer they started with.

`staging_directory` is not restart-required, but it is read per job: `buildJobContext` snapshots it when a job starts, and the Mux and Resume lookups, `A R` / `A S` and the orphan scan all read the CURRENT value. A change therefore applies to jobs that start after it, and anything already staged under the previous directory is not found by any of them until its folder is moved across; both help texts say so.

#### [logs]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| LogLevel | string | "INFO" | `log_level` | Valid: DEBUG, INFO, WARN, ERROR |
| LogMaxFileSize | int | 10485760 (10MB) | `log_max_file_size` | Bytes |
| LogMaxFiles | int | 5 | `log_max_files` | |

#### [monitors]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| ArchiveWindowDays | int | 3 | `archive_window_days` | Valid: 1-3650. How many days back the monitor archives from the feed-history store; upcoming/live items are ALWAYS covered regardless of age. |
| ArchiveSlots | int | 3 | `archive_slots` | Valid: 1-100. Max backlog (Queued) VOD downloads per channel running at once; new/live content never waits on a slot. |
| FeedCheckInterval | FlexDuration | 10 (minutes) | `feed_check_interval` | |
| DecapiCheckInterval | *int | nil | `decapi_check_interval` | Seconds, valid: 15-3600 |
| TwitchCheckInterval | *int | nil | `twitch_check_interval` | Seconds, valid: 5-3600 |
| HideFinishedAgeDays | FlexDuration | 30 (days) | `hide_finished_age_days` | |
| ProbeCooldown | FlexDuration | 0 (seconds, disabled) | `probe_cooldown` | Min seconds between re-probing the same video's metadata. 0 = every cycle re-probes; no max. |
| MembershipDiscovery | *bool | nil (→ true) | `membership_discovery` | Members-only `/membership`-tab discovery. Absent/nil = enabled; needs YouTube auth cookies to do anything. |

#### [downloader]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| OutputTemplate | string | `${channel}/${start_date} ${title} [${id}]` | `output_template` |
| MaxVideoResolution | int | 2160 | `max_video_resolution` | Min 0; **`0` = unbounded**. |
| NumParallelDownloads | int | 10 | `num_parallel_downloads` | Min: 1. Peak concurrent VOD **jobs** across all channels — broadcasts never wait on the pool, so total concurrent downloads can reach (live streams) + this. Not to be confused with SegmentWorkers below, which gates concurrency *within* one download; a live broadcast's catch-up speed is governed entirely by SegmentWorkers, since this setting never applies to it. |
| SegmentWorkers | int | 12 | `segment_workers` | Min: 1, **no max**. |
| ReorderBufferMB | int | 1024 (256 on arm64) | `reorder_buffer_mb` | Min: 0, **no max**; `0` = unbounded. |
| ReorderBudgetMB | int | 4096 (1024 on arm64) | `reorder_budget_mb` | Min: 0, **no max**; `0` = unbounded. |
| ProgressIntervalMS | int | 16 | `progress_interval_ms` | Min: 1, **no max**. |
| DownloadChat | bool | true | `download_chat` | |
| Prefer60fps | bool | true | `prefer_60fps` | |
| PreferHDR | bool | true | `prefer_hdr` | YouTube only (owner decision D13). Ranks HDR10 above SDR at the same size and frame rate — the dynamic-range rung of the shared rank (`internal/youtube/rank.go`), which every YouTube selector and the picker's badges apply; `false` ranks SDR above HDR10. Dolby Vision ranks below both either way, as yt-dlp's `hdr:12` does. Snapshotted per job start into `JobConfig.PreferHDR` (`buildJobContext`); the picker reads it per request. Not restart-required; no migration (a file without the key reads as the default). |
| MaximumTimeout | int | 600 | `maximum_timeout` | Seconds; YouTube livestreams. Min: 30 |
| InterruptionTimeout | FlexDuration | 120 (minutes) | `interruption_timeout` | Min: 0, no max. |
| IncompleteStagingExpiryDays | FlexDuration | 7 (days) | `incomplete_staging_expiry_days` | Min: 0, no max. |

##### MaxVideoResolution

Min 0; **`0` = unbounded**. Compares the SHORTER frame dimension, so `2160` recognises both 3840x2160 and 2160x3840. Resolves to the largest rendition at or below the cap, or the closest one ABOVE it when a stream offers nothing that small — it never leaves a job with nothing to download. On a YouTube VOD it resolves over the renditions that are not damaged, whenever any are (encoded short; see Damaged Renditions in [platform-services.md](platform-services.md)), so a damaged rendition at the cap gives way to an intact one below it, or above it when nothing intact is that small. One rule for all four selection sites (`CapDimension`/`SelectByCap`, `internal/utils/resolution.go`). Both UIs offer it as a preset picker (Unbounded/480p/720p/1080p/1440p/4K/8K plus Custom); the stored value is still the integer and there is no migration.

##### SegmentWorkers

Min: 1, **no max**. Concurrent segment fetches within a single download (catch-up on a live DASH stream, parallel VOD HLS). Values above `config.SegmentWorkersWarnThreshold` (16) log a warning at startup and on every save from either UI that changes it to one (`warnSegmentWorkers`, `cmd/moombox/hot_reload.go` — a save that leaves it alone says nothing, from the TUI as from the Web) and are flagged in both UIs' help text: a wide simultaneous fan-out to YouTube is a traffic shape that attracts bot detection. Measured 2026-08-15 on a live stream mid-archive, at the pre-rewrite fixed 6-worker baseline: Moombox sustained 5.96 MB/s, against 2.86 MB/s for one `curl` connection and 11.28 MB/s for six parallel `curl` connections — the headroom the new configurable default (12 workers) and the rolling-window catch-up rewrite exist to close (a harness test of the same rewrite dropped 4.888s of batched catch-up to 0.84s). Not restart-required.

##### ReorderBufferMB

Min: 0, **no max**; `0` = unbounded. Megabytes of out-of-order segment data ONE download may hold in RAM while its head-of-order segment works through its retry ladder — the ceiling on `engine.reorderBuffer`, fed by both parallel paths (`runParallelCatchUp` on live DASH catch-up, `runHlsVodParallel` on VOD HLS). The head segment is admitted regardless, so a stalled head can put one segment over. This is the first config default that differs by platform: the value was originally chosen for an arm-class box, and owner ruling R3 (2026-09-24) scoped every arm-motivated cap to arm64 only — see `platformDefaults` (`internal/config/config.go`). Applied through `engine.ConfigureReorder` at boot and on every save; not restart-required.

##### ReorderBudgetMB

Min: 0, **no max**; `0` = unbounded. The same megabytes, summed across EVERY live reorder buffer in the process. Nothing bounded that sum before: at the per-job ceiling alone, ten concurrent VOD jobs could admit ten ceilings' worth of segments. A non-head segment waits on this ceiling as well as its own buffer's; the head is exempt from both, because nothing frees either without a flush and nothing flushes without its head. A `reorder_buffer_mb` above this budget is incoherent as written and is clamped to it at read time (`DownloaderConfig.ReorderLimitBytes`), with one warning naming both keys. Applied through `engine.ConfigureReorder` at boot and on every save; not restart-required.

##### ProgressIntervalMS

Min: 1, **no max**. Milliseconds between one job's progress reports — `ProgressTracker.maybeUpdate`'s gate (`internal/worker/progress.go`), and therefore the upstream rate limit on both the WebSocket hub (which throttles nothing of its own) and the TUI's rows. Unlike the two reorder ceilings above, `0` is NOT a documented "unbounded" value: an ungated tracker would write the database once per arriving segment callback on every download at once, so anything below 1 resets to the default. The knob exists so 8 ms — about 120 reports a second, matching the TUI's 120 fps renderer — can be tried on a fast machine without a rebuild (owner ruling F1, 2026-09-25); each report costs one `UpdateJobFields` write and one fan-out, so lower values buy smoothness with CPU and disk. Snapshotted per job start into `JobConfig.ProgressInterval` by `buildJobContext` (`internal/worker/worker.go`), like MaximumTimeout and SegmentWorkers, so a save applies to the NEXT job and a running tracker keeps the interval it started with. Not restart-required, and absent from both restart-required lists because it has no Settings row: a config-file-only key by the same ruling, like DpapiProfileDir below — no control in either UI, no API setter, so in practice it is edit-and-restart with the edit made while Moombox is stopped.

##### InterruptionTimeout

Min: 0, no max. How long a live YouTube download's MaxTimeout-backstop finalize may keep deferring while `engine.SegmentDownloader.MayResume` reports the broadcast may still resume (`stallForPossibleResume`, `internal/engine/downloader.go`) — the interruption-resume design's Tier 1 stall. `0` disables the STALL only, not Tier 2 preservation: `attachMayResume` (`internal/worker/interruption.go`) installs `MayResume` unconditionally, and every live strategy site maps the config value through `engineInterruptionTimeout` before it reaches `engine.DownloaderOptions.InterruptionTimeout` — a positive value passes through as the ceiling, `0` (or a defensive negative) maps onto the sentinel `engine.InterruptionNoStall` (`-1`). `stallForPossibleResume`'s `InterruptionNoStall` branch still consults `MayResume` once per call and still latches `finalizedDuringInterruption` when it reports true, but always returns `false` — no stall, no clock. A parallel worker-side latch, `resumeWaitLatch` (fed by `noteRefreshFailure`/`resumeEvidence` in `internal/worker/interruption.go`), gives the same treatment to the live loop's refresh failures (the `ErrQualityLost` refresh and the stream-end verify's): evidence latches `incomplete_tail` even when `shouldWaitForResume` itself never permits an actual wait. When it does permit one, this value is also that wait's ceiling (`waitDeadline`, `internal/worker/interruption.go`). So a `0` job never blocks finalize, but a genuinely-interrupted `0` job still finalizes with staging + resume data preserved exactly like an enabled one that gave up. Snapshotted per job start (`buildJobContext`), like `MaximumTimeout`/`SegmentWorkers` above; not restart-required.

##### IncompleteStagingExpiryDays

Min: 0, no max. How long the two EXPIRING staging shields hold. `jobNeedsStaging` (`internal/worker/orphans.go`) keeps a Finished job's staging out of orphan cleanup for SIX reasons — exactly the keeps `cleanupStagingAfterMux` itself applies, read from its verdict (`decideStagingCleanup`, `internal/worker/worker.go`; `stagingKeep.expires` marks the two below; parity pinned by `TestJobNeedsStagingMatchesTheCleanupVerdict`) — and this window governs two of them: the job is flagged `incomplete_tail` (the tail is Resume-able), or its chat capture ended incomplete (`database.ChatIncomplete` — no verb re-pages from the capture kept in staging, since `/retry` and `/resume` continue only the recording itself and Reinitialize starts over, but it can be the only copy of those comments when the archive's chat copy failed, an aside recovery carries it beside the recovered recording, and the operator can take it by hand). Both lapse on the one age rule, `incompleteStagingExpired` (`internal/worker/orphans.go`). The other four shields carry NO age rule at all, because each holds captured media that can exist nowhere else: staging still holding a recording the engine set aside rather than truncated (`stagedAsideRecordings`, `internal/worker/orchestrator_mux.go`) and staging still holding an unmuxed captured part (`hasUnmuxedSegmentParts`, `internal/worker/worker.go`, recoverable via the Mux action) are shielded until they are muxed or the job is deleted, and so is a staging root holding a recording the finalize did not use (`unusedRootRecording`, `internal/worker/vod_supersede.go` — the shield `cleanupStagingAfterMux` keeps the dir for, which the sweep used to offer as an ordinary orphan), and a staging tree holding a recording a mux could not carry whole (`stagedMuxGaps`, `internal/worker/mux_repair.go` — its damaged bytes left out of a repaired copy, or a copy nothing could check against its fragments; see Staging Directory). Only the disk-heavy staging shield expires — the flag (the "may be missing its tail" badge) never does: YouTube cannot resume a broadcast days later, so aged interruption staging has no resume value, while the badge stays honest indefinitely. Age is measured from the job's `updated_at`, so any activity restarts the window; unparseable timestamps preserve. After expiry the staging becomes an ordinary orphan-scanner candidate; auto-resume's staging-existence gate then falls to the silent drop and manual Reinitialize remains the recovery. `0` = preserve forever. Read live per scan; not restart-required.

#### [cookies]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| CookieFile | string | "./cookies.txt" | `cookie_file` | **Restart-required.** `AutoCookieService` is constructed from this once, at startup (`initServices`, `cmd/moombox/services.go`). |
| AutoEnabled | bool | false | `auto_enabled` | **Restart-required for half of what it owns.** It owns the headless-browser periodic timer and the `SetExpectedPlatforms` seeding (`run`, `cmd/moombox/main.go`), both decided once at boot, and the automatic recovery attempts — the monitors' one recovery attempt (`monitor_callbacks.go`) and the worker's refresh on an auth failure (`OnCookieRefreshNeeded`, `services.go`) — which read it live, as does `BrowserLaunchAllowed`. Both help texts say which half waits. See §Auto-Cookie Service for the full settled meaning. |
| BrowserProfileDir | string | "./browser-profile" | `browser_profile_dir` | **Restart-required.** The directory's *existence* is not part of the start condition — `periodicRefreshHasSource` (`internal/cookies/autocookies_periodic.go`) asks per tick. |
| BrowserPath | string | "" | `browser_path` | Explicit browser override. Only a real override when paired with `browser_type` (`browserOverrideConfigured`, `internal/cookies/autocookies_browser_resolve.go`). |
| BrowserType | string | "" | `browser_type` | Which extraction backend applies to `browser_path` — Firefox `cookies.sqlite` vs Chromium CDP. Validated against `knownBrowserTypes` (`internal/cookies/browser_validate.go`). |
| Platforms | []string | [] | `platforms` | Platforms with verified cookies. |
| ActivePlatforms | []string | unset | `active_platforms` | Explicit override for UI display; consumed by `config.GetActivePlatforms`. |
| RefreshInterval | FlexDuration | 360 (minutes = 6h) | `refresh_interval` | Valid: 10-10080 minutes. |
| DpapiFallback | bool | false | `dpapi_fallback` | Windows-only. Opt-in: reads the user's REAL Chromium-family profile via `CryptUnprotectData` when the CDP refresh cannot acquire the managed profile. Not restart-required: `AutoCookieService.DpapiFallback` reads it live on every failed refresh. |
| DpapiProfileDir | string | "" | `dpapi_profile_dir` | Windows-only, and empty by default. |
| Acquisition | string | "auto" | `acquisition` | `auto` \| `profile`. |

##### CookieFile

**Restart-required.** `AutoCookieService` is constructed from this once, at startup (`initServices`, `cmd/moombox/services.go`). Advice that names the file to replace — the auth alerts (`cookieFilePath`, `cmd/moombox/helpers.go`) and the worker's failed-refresh line (`CookieFileInUse`) — names the file the running services use (the jar's path), so a save without the restart cannot send the operator to a file nothing reads. A first-run setup that names another cookie file carries into it, before it saves, the rows of the platforms a browser login or an import in this run supplied (`CarryCookieFileTo`, `internal/cookies/cookie_files.go` — see [user-interfaces.md](user-interfaces.md) § First-Run Setup Wizard), so a browser login the wizard ran is what the restart loads; with no login it leaves that file alone, whatever the boot-time file holds.

##### Platforms

Platforms with verified cookies. Seeded by `detectCookiePlatforms` (`cmd/moombox/services.go`) — sidecar first, loose cookie-name predicates second — on the first boot that has a config file: during a first run the save would create config.toml and mark it loaded, skipping both setup wizards, so that write and `PersistPlatforms`' go through `Store.UpdateIfLoaded` (`internal/config/store.go`), which does nothing until the operator's first save. Nothing automatic ever prunes it; the sole removal path is an operator replacing the list through `PUT /api/config`. Its auth-loss consumer reads it once, at boot (`RefreshService.SetExpectedPlatforms` seeds the "previously authenticated" state before the first check), so a runtime removal changes that seed from the next boot; the running service already tracks each platform's real verdict from its own checks, and the status-bar fallback (`GetActivePlatforms`) follows the list at once.

##### ActivePlatforms

Explicit override for UI display; consumed by `config.GetActivePlatforms`. Unset (nil) means no override, and the display falls back to `platforms`, then to the enabled channels; an EMPTY list is an override too — both indicators off — so the field carries no `omitempty` (the TOML encoder skips nil on its own). The settings forms write it only once a toggle has been changed (or an override already exists): the toggles otherwise show the inferred answer, and saving that back would freeze it. Two persistence policies, both deliberate and pre-existing: both first-run wizards (`web/public/modules/setup.js`, `internal/tui/setup_wizard.go`) list a platform here on an ACCEPTED verdict — hedged verdicts included — while the `platforms` row above is fed through the `PersistPlatforms` callback (`internal/cookies/autocookies.go`) only on a VERIFIED one. The two lists answer different questions (what to display; what was proven); unifying them is an owner call nobody has asked for.

##### RefreshInterval

Valid: 10-10080 minutes. Drives `AutoCookieService.StartPeriodicRefresh` (the browser timer) only — **not** `RefreshService`, whose interval is the hardcoded 30-minute default. **Restart-required**, in both lists: the ticker is built from this value once when the loop starts (`internal/cookies/autocookies_periodic.go`) and is never `Reset`, and `cmd/moombox/main.go` reads the same value to decide whether to start the loop at all — so a save changes nothing until the next start, and up to a week (10080 minutes) can pass before that is visible.

##### DpapiProfileDir

Windows-only, and empty by default. Names a Chromium-family PROFILE directory for the DPAPI fallback to read, REPLACING the discovery walk rather than joining it — discovery knows eleven fixed `%LOCALAPPDATA%` `User Data` layouts, so a portable Chromium, a `--user-data-dir` profile and Opera are invisible to it. `dpapi.ValidateProfileDir` (`internal/cookies/dpapi/profiles.go`) checks the three structural facts the reader needs — an existing directory that is not a symlink or junction; a `Local State` that `ChromeLocalStatePath` can find *beside* it (Chromium's `User Data` root) or *inside* it (Opera's self-contained layout, the parent preferred so a stray copy cannot displace the real key); and `Cookies` or `Network/Cookies` inside — and a directory that fails them is an error rather than a fall-back to discovery, because falling back would answer "no profiles found under LOCALAPPDATA" about a setting the operator had just written. The same helper resolves the master key for `loadChromeMasterKey` (`internal/cookies/dpapi/dpapi_windows.go`), so validation and the reader cannot disagree. The launch-boundary deny-list (`dangerousProfilePathSubstrings`) is deliberately **not** applied here: it exists to stop a headless browser being *launched* against a real user profile, this value is only ever *read* (a `mode=ro` SQLite open plus a `Local State` read), and applying it refused a portable Chromium and every Opera while adding nothing, since the discovery walk reads the same real profiles with no deny-list at all. Relative values resolve against Moombox's working directory, like `paths.*`. **Not** restart-required by design: `AutoCookieService.DpapiProfileDir` reads it live at the start of every DPAPI pass, so a change applies to the next pass — though today nothing but a `config.toml` edit can change it (no UI control, no API setter), so in practice it is edit-and-restart, with the edit made while Moombox is stopped. `Validate` checks the path's shape only (no `..` traversal), never its existence — a container writes its config before the volume is mounted — and the boot-time verdict is a Warn from `LogDpapiProfileDirVerdict` (`internal/cookies/autocookies_dpapi.go`), never a boot failure; it fires off Windows, when `dpapi_fallback` is off (the default, so the likeliest case), and when the directory is unusable.

##### Acquisition

`auto` | `profile`. Decides how a REFRESH acquires credentials: `auto` (a resolvable browser launches, a host with none imports the profile — the pre-existing rule), `profile` (never launch for a refresh; read `browser_profile_dir` read-only even on a desktop with a browser). Two values by ruling — the audit's `browser` behaved exactly like `auto` and was dropped. **Not** restart-required — `AutoCookieService.AcquisitionMode` reads it live. Composes with `auto_enabled`, which still owns whether a pass may launch at all. `StartSetup` never consults it. Absent or empty means `auto` and needs no migration: `Load` decodes over `Defaults()`.

`cookie_file`, `refresh_interval`, `auto_enabled` and `browser_profile_dir` are the four cookie keys in `restartRequiredKeys` (`internal/tui/settings.go`) and in `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`). What `auto_enabled` does *not* need a restart for is the manual triggers — they read it live.

#### [disk]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| WarnPercent | int | 90 | `disk_warn_percent` | Valid: 1-99 |
| CriticalPercent | int | 95 | `disk_critical_percent` | Must be > WarnPercent. Also the backlog scheduler's admission gate: no backlog VOD is admitted from the reading at or past it until usage is 2 points below it |

#### [updates]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| AutoCheckUpdates | bool | true | `auto_check_updates` |

`runUpdateCheckLoop` (`cmd/moombox/helpers.go`) checks GitHub shortly after boot and then daily, each time only while the flag is on — re-read on every check, so turning it off stops the schedule without a restart. It also re-reads the flag every minute and checks at once when it has turned ON, so enabling it at runtime does not wait up to a day; reading the store covers every writer (both settings UIs, the setup wizard, the update banner's dismiss).

#### [memory]

Bounds steady-state memory for the Go process and the embedded BotGuard sidecar. See `docs/spec/operations.md` "Memory Limits" for the full design rationale and tuning guide.

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| GoSoftLimitMB | int | 256 | `go_soft_limit_mb` | `debug.SetMemoryLimit`. Soft cap — Go GC ramps up near the limit but allocations succeed beyond it. 0 disables. Hot-reloadable: re-applied on save (`0` clears the limit via `math.MaxInt64`). |
| SidecarSoftLimitMB | int | 200 | `sidecar_soft_limit_mb` | RSS threshold. When sidecar RSS crosses, Moombox calls `triggerGC` JSON-RPC. 0 disables. |
| SidecarHardLimitMB | int | 512 | `sidecar_hard_limit_mb` | V8 `--max-old-space-size`. Hitting this OOM-aborts the sidecar (no graceful soft stop). Must be comfortably above SidecarSoftLimitMB. 0 uses V8's default (~512–1500 MB depending on host). **Restart-required** (the sidecar is started with it once); both settings UIs mark it so. |

#### [bgutils]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| UseSidecar | bool | true | `use_sidecar` | Node + JSDOM + bgutils-js sidecar for real PO tokens. **Restart-required**; both UIs mark it so. |

#### [twitch]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| ClientID | string | `""` | `client_id` | Overrides the Client-ID every Twitch GQL call and Usher playlist fetch sends (`twitch.SetClientID`, installed by `initServices` before the Twitch service is built); empty keeps `constants.TwitchGQLClientID`. yt-dlp's `twitch:client_id` extractor argument is the same override (G70). 1-64 letters and digits (`config.ValidTwitchClientID`, the one check `config.Validate`, `PUT /api/config` and the TUI's save share); a malformed one is put back to empty on load. Both settings UIs show it in their Twitch section. **Restart-required**; both UIs mark it so. |

#### [connectivity]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| ProbeTargets | []string | `1.1.1.1:443, 8.8.8.8:443, 9.9.9.9:443` | `probe_targets` | host:port TCP targets raced to detect internet reachability. |

##### ProbeTargets

host:port TCP targets raced to detect internet reachability. Exposed in both settings UIs' Network section. Every entry must pass `config.ValidateProbeTarget`, the one check `config.Validate`, `PUT /api/config` and the TUI's save share: it must split as host:port, its port must be 1-65535 or a TCP service name the system knows (`1.1.1.1:`, `1.1.1.1:99999` and `1.1.1.1:https443` can never connect, and a list of only those would keep the monitor offline for good), and its host must not be this machine — empty (`:443` dials the local host), `localhost` or a name under `.localhost`, a loopback or an unspecified address — where a local service would answer through any outage. A named host is accepted as written; nothing is resolved. The refusal names the entry. `PUT /api/config` also rejects an empty list; on load an empty list or any refused entry falls back to the defaults. **Restart-required** (the monitor takes its targets at construction).

#### [[channels]] (array of tables)

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| ID | string | "" | `id` | YouTube channel ID or Twitch username. Every writer stores it through `utils.NormalizeChannelID` — trimmed, a URL or bare `@handle` resolved — and `Validate` refuses an ID with surrounding whitespace or one another entry has, compared case-insensitively, so `Save` refuses either; `Normalize` (Load) trims it and drops the later duplicate. |
| Name | string | "" | `name` | Display name |
| Platform | string | "youtube" | `platform` | "youtube" or "twitch" |
| Enabled | *bool | nil (true) | `enabled` | nil defaults to true |
| Terms | ChannelTerms | empty | `terms` | Regex filter (string or map of named patterns) |
| NumDescLookbehind | *int | nil | `num_desc_lookbehind` | Retired: terms match titles only, so nothing reads it. Not on either channel editor; kept so an existing config loads and saves unchanged. |
| OutputDirectory | string | "" | `output_directory` | Per-channel override. Editable in both channel editors (Web dialog, TUI form); blank = the global value. |
| IncludeNonLiveContent | bool | false | `include_non_live_content` | |
| ArchiveWindowDays | *int | nil | `archive_window_days` | Per-channel override (1-3650). Editable in both channel editors (Web dialog, TUI form); blank = the global value. |
| ArchiveSlots | *int | nil | `archive_slots` | Per-channel override (1-100). Editable in both channel editors (Web dialog, TUI form); blank = the global value. |
| QualityPreference | string | "" | `quality_preference` | e.g. "1080p60", "best", "audio_only" |
| TwitchGapFillAdBreaks | bool | false | `twitch_gap_fill_ad_breaks` | Twitch only (owner decision D17 follow-up 3). Also fills the stitched-ad breaks a live capture skipped from the broadcast's archive VOD; a capture of such a channel ends a part at each break so the hole sits between two parts. Lost-data gaps are filled whatever it says ([architecture.md](architecture.md) § Twitch Archive-VOD Gap-Fill). Editable in both channel editors. Refused on a non-Twitch channel — Validate names it, Normalize clears it, the API answers `400` (owner decision PR-I3). |
| TwitchVodBacklog | bool | false | `twitch_vod_backlog` | Twitch only (owner decision D18). The Twitch monitor lists the channel's videos hourly and queues each one no job has had as a Queued backlog VOD (`monitor.TwitchBacklog`), less the archive of a broadcast a live job of the channel holds — one with footage, Cancelled, with a gap-fill plan, still capturing, or an Error row the monitor will still re-initialise (owner decision PR-M4). Editable in both channel editors. Refused on a non-Twitch channel — Validate names it, Normalize clears it, the API answers `400` (owner decision PR-I3). |
| TwitchArchiveClips | bool | false | `twitch_archive_clips` | Twitch only (owner decision D26 follow-up 2; named with the `twitch_` prefix by FE-34, unreleased as `archive_clips`, so no migration). The Twitch monitor lists the channel's clips hourly and queues each one it has not seen as a Queued clip job, paced by archive_slots (`internal/monitor/twitch_clips.go`). Normalized off on a non-Twitch channel; editable in both channel editors. |

#### [[notifications]] (array of tables)

| Field | Type | TOML Key | Notes |
|-------|------|----------|-------|
| URL | string | `url` | Discord webhook URL, full or `discord://ID/TOKEN` shorthand |
| Enabled | *bool | `enabled` | nil defaults to true (`IsEnabled`), mirroring `ChannelConfig.Enabled`. `false` mutes the target without deleting its filter. |
| Events | []string | `events` | Event types to notify on. Empty/absent means all. Vocabulary is `notifications.EventGroups`, not enforced here (would be an import cycle) — checked at `buildTargets` and the web save path. |
| Mention | string | `mention` | Content line pinging a role (`<@&ID>`), a user (`<@ID>`, also accepts the legacy `<@!ID>` nickname spelling), `@everyone`, or `@here`. Empty means no ping. Validated and canonicalised by `ParseMention`. |
| MentionEvents | *[]string | `mention_events` | Which events `Mention` rides along with. Three states: absent → `DefaultMentionEvents()` (`error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered`, `sidecar_down`); pointer to an empty list (`mention_events = []`) → never; pointer to a list → exactly those events. See `ResolveMentionEvents`. |
| Mode | string | `mode` | `"separate"` (default) or `"edit"` — one message per event, or one message per job rewritten in place. See [operations.md](operations.md) § Delivery Modes |

### FlexDuration

`FlexDuration` is a custom type that stores a `float64` value whose unit is determined by the field (`flexDurationFields` in `internal/config/flex_duration.go`):

- **seconds**: `probe_cooldown`
- **minutes**: `feed_check_interval`, `interruption_timeout`, `refresh_interval`
- **days**: `hide_finished_age_days`, `incomplete_staging_expiry_days`

**Parsing rules:**

| Input | Type | Result |
|-------|------|--------|
| `10` | int/float | Stored as-is (10.0) |
| `"10"` | string (plain number) | Stored as 10.0 |
| `"30m"` | string (duration) | Parsed as 30 minutes; stored as 1800.0 when unit is "seconds", 30.0 when "minutes", or 0.0208... when "days" |
| `"7d"` | string (duration) | Parsed as 7 days; stored as 10080.0 when unit is "minutes", or 7.0 when unit is "days" |

**Supported duration suffixes:** `ms`, `s`, `m`, `h`, `d`, `w`.

**Serialization:** `MarshalTOML()` writes a plain number. `MarshalJSON()` writes a plain number. This prevents the encoder from producing a nested `{Value = 5.0}` table.

**TOML deserialization:** `UnmarshalTOML()` handles int64, float64, string (plain number or duration string), and map (legacy `{Value = 5.0}` format from earlier serialization). It cannot see which field it is decoding, so a duration string's value there is provisional; `loadFromFile` then re-reads every duration string in its field's unit (`resolveFlexDurationStrings`). Before that step the guess (days for a `d`/`w` suffix, else minutes) was final, and `probe_cooldown = "2m"` loaded as 2 seconds.

### ChannelTerms

`ChannelTerms` supports two TOML representations:

1. **Simple string:** `terms = "regex_pattern"` -- stored in `Simple` field.
2. **Named map:** `[channels.terms]\nstream = "pattern1"\nvod = "pattern2"` -- stored in `Named` map.

`Patterns()` returns all patterns as a `[]string` regardless of representation.

### Config Migrations (migrateOldFormat)

Handles backward compatibility with older flat config formats. All migrations are non-destructive: they only apply when the target section does not already exist.

| Legacy Format | Current Format | Condition |
|---------------|----------------|-----------|
| `allow_lan` / `allow_external` (top-level booleans) | `network.network_access` (string) | Only if `network_access` is empty or "localhost" |
| `[tasklist].hide_finished_age_days` | `monitors.hide_finished_age_days` | Only if `[monitors]` section doesn't exist |
| Top-level `port`, `network_access`, `https_enabled`, `tls_cert_path`, `tls_key_path`, `password_hash` | `[network]` section | Always migrated (top-level takes precedence) |
| Top-level `log_level`, `log_file_path`, `log_max_file_size`, `log_max_files` | `[logs]` / `[paths]` sections | Always migrated |
| Top-level `database_path` | `paths.database_path` | Always migrated |
| Top-level `feed_check_interval`, `decapi_check_interval`, `twitch_check_interval`, `hide_finished_age_days` | `[monitors]` section | Always migrated. (The deleted per-cycle candidate-cap key that the archive window/slots settings replaced is silently ignored in old configs, not migrated.) |
| `[downloader].output_directory`, `staging_directory`, `ffmpeg_path` | `[paths]` section | Only if `[paths]` doesn't exist |
| `[downloader].cookie_file` | `[cookies]` section | Only if `[cookies]` doesn't exist |
| `[auto_cookies]` section | `[cookies]` section | Only if `[cookies]` doesn't exist |

### Validation

`validate(cfg)` enforces constraints after loading and migration:

- Port: 1-65535 (default: 774)
- NetworkAccess: must be "localhost", "lan", or "external"
- LogLevel: must be DEBUG/INFO/WARN/ERROR (default: INFO)
- ArchiveWindowDays: 1-3650 days (also enforced per-channel; an invalid override is cleared to fall back to the global)
- ArchiveSlots: 1-100 (same per-channel treatment)
- FeedCheckInterval: min 1 minute
- HideFinishedAgeDays: min 0
- DecapiCheckInterval: 15-3600 seconds (or nil)
- TwitchCheckInterval: 5-3600 seconds (or nil)
- NumParallelDownloads: min 1
- SegmentWorkers: min 1, no max (values above `SegmentWorkersWarnThreshold`, 16, log a warning at startup and on a save that changes it, instead of failing validation)
- ReorderBufferMB: min 0, no max (`0` = unbounded)
- ReorderBudgetMB: min 0, no max (`0` = unbounded). The cross-check is NOT made by `validate` — a per-job ceiling above the process budget is legal config that `DownloaderConfig.ReorderLimitBytes` clamps at read time, with one warning, rather than a value rewritten on disk
- ProgressIntervalMS: min 1, no max. The one downloader key where `0` is an error rather than a documented "disabled" value, because an ungated progress tracker writes the database once per segment callback
- MaxVideoResolution: min 0 (`0` = unbounded; only a negative value resets to the default)
- MaximumTimeout: min 30 seconds (no maximum)
- OutputTemplate: non-empty, at most `OutputTemplateMaxLen` (500) bytes — one rule for every writer; the web PUT and the TUI also refuse a longer one up front, naming the field
- DiskWarnPercent: 1-99, DiskCriticalPercent: must be > WarnPercent (auto-adjusted if not)
- CookieRefreshInterval: min 10 minutes
- QualityPreference: validated against a fixed set of allowed values (best, 2160p60, 2160p, 1440p60, 1440p, 1080p60, 1080p, 900p60, 900p, 720p60, 720p, 480p, 360p, 160p, audio_only)

### Config Saving

`Save(cfg, path)` writes configuration atomically:

1. Validate the config.
2. Write to `<path>.tmp`.
3. Encode TOML.
4. Rename `<path>.tmp` -> `<path>`.

File permissions: `0o600` (owner read/write only).

### Output Template Resolution

`ResolveTemplate(template, vars)` expands the following variables:

| Variable | Source | Sanitization |
|----------|--------|--------------|
| `${title}` | Video/stream title | Filesystem-unsafe characters removed; Unicode preserved; cut to 180 bytes on a rune boundary (`templateTitleMaxBytes`) |
| `${id}` | Video/stream ID | No sanitization (IDs are alphanumeric) |
| `${channel}` | Channel name | Filesystem-unsafe characters removed; Unicode preserved; cut to 200 bytes (`templateChannelMaxBytes`) |
| `${start_date}` | Stream start time (or now) | Formatted as `YYYYMMDD` in the host's local zone (`time.Local`: `TZ` in Docker, the system zone on Windows and Linux) |
| `${start_time}` | Stream start time (or now) | Formatted as `HHMM` in the host's local zone, as `${start_date}` |

Every stored time is UTC — `created_at`, YouTube's `startTimestamp` (`+00:00`), Twitch's `createdAt` (`Z`), the workers' "now" fallbacks — so the resolver converts the parsed time to `time.Local` before formatting it. Formatting it in the string's own offset would name every install's files in UTC: `TZ` in `docker-compose.yml` would have no effect, and an evening stream in the Americas or a morning one in Japan would be filed under the wrong day. Files a release named in the string's own (UTC) offset keep their names; nothing renames them. The previews show the same zone: the TUI's calls the resolver, and the dashboard's two — Settings' and the first-run wizard's — render their sample date at the server's offset, `utcOffsetMinutes`, rather than in the browser's zone (`renderTemplatePreview`, `web/public/modules/settings.js`). `/api/status` carries it for Settings and `/api/setup/status` for the wizard, which `app.init` reads before the wizard draws: a first run never reads `/api/status`, and its wizard — loopback-only, so on a headless host a browser through an SSH tunnel, in another zone — showed the browser's date.

Sanitization preserves CJK, Japanese kana, and full-width characters via a regex allowlist. Those keep three bytes a character, and Linux caps a name at 255 BYTES, so the byte caps are what keep a long Japanese title from failing the finalize with ENAMETOOLONG: with the default template a capped title leaves room for the id and the longest suffix a job writes beside its archive. No ASCII title reaches the cap (YouTube allows 100 characters, Twitch 140). Those caps fit the default layout only, so the expansion is also fitted per path component (`fitTemplateComponents`, `internal/config/config.go`): a directory to 255 bytes and the archive's own name to `templateStemMaxBytes` (210 — the default template at the caps with an 18-byte Twitch id, leaving room for the 42-byte longest suffix). A component over its budget shrinks `${title}` and `${channel}` where they appear, levelling the two rather than cutting one away, down to 30 bytes each; literal text and `${id}` never shrink, so a name keeps the id that tells two archives apart, and only a component still over once both are at that floor (long literal text) is cut on a rune boundary. Without that bound a custom `${channel} - ${title} [${id}]` could resolve to about 400 bytes and fail the finalize.

After expansion every path component that names a Windows device (`CON`, `NUL`, `COM1`, … with or without an extension — `utils.IsWindowsReservedName`) gets a leading underscore, on every platform, so a channel called `CON` does not make the `${channel}/…` directory impossible to create on Windows.

### Auto-Hash

On startup, if `network.password_hash` contains a plaintext password (detected by checking if it starts with the scrypt prefix), it is automatically hashed with scrypt and the config file is re-saved. This is a one-time migration.

---

## Cookies (internal/cookies/)

ONE `cookies.txt` on disk, three pieces above it. `CookieJar` (`internal/cookies/jar.go`) parses the file and answers typed questions about it. `RefreshService` (`internal/cookies/refresh.go`) validates the credentials in-process and rotates YouTube's session cookies out of `Set-Cookie` headers. `AutoCookieService` (`internal/cookies/autocookies.go` and its `autocookies_*.go` siblings) acquires credentials three ways — through a browser, browser-free out of a browser profile, or from the operator's own file pasted or uploaded through `POST /api/cookies/import` (`AutoCookieService.ImportCookies`, `internal/cookies/cookie_import.go`).

### Cookie Jar

`CookieJar` parses Netscape-format cookie files and provides typed access to authentication cookies for YouTube and Twitch.

**One file, two in-memory jars.** `CookieJar` holds two maps — `youtube` (youtube.com *and* google.com rows) and `twitch` (twitch.tv rows) — and `loadFrom` (`internal/cookies/jar.go`) routes each row to one of them by domain before any name test runs. `cookies.txt` itself stays a single store holding every platform's rows, and every writer keeps updating it in place; the split is purely how the parsed state is represented. It is not cosmetic: one map keyed by bare cookie NAME cannot hold a `.twitch.tv` `SID` and a `.google.com` `SID` at once, so one silently evicted the other and the winner was whichever row the file listed last.

**Loading behavior** (`Load` → `loadFrom`, `internal/cookies/jar.go`):

1. Read the whole file, parse into fresh maps, then swap. A transient read error (EIO, a permission flip) leaves the previous state intact rather than wiping valid authentication.
2. Skip comments, except `#HttpOnly_` lines (those are data).
3. Parse tab-separated fields: domain, include_subdomains, path, secure, expiry, name, value. Fewer than 7 fields is a malformed row — logged at Debug, skipped. Fields 6.. are re-joined, so a value that legitimately contains a tab is preserved rather than truncated.
4. **Admission is DOMAIN-FIRST**, and that ordering is load-bearing. `isYouTubeDomain`/`isGoogleDomain` select the `youtube` jar, `isTwitchDomain` the `twitch` jar, and any other domain is dropped; only then is the name tested — `essentialYouTubeCookies` (or, on google.com only, the `SID`/`HSID`/`SSID`/`APISID`/`SAPISID`/`__Secure-1P*`/`__Secure-3P*` auth names) for the first, `essentialTwitchCookies` for the second. Under a name-first rule a `.twitch.tv` row named `SID` would be admitted, because that name is in `essentialYouTubeCookies`. Domain matchers are suffix-anchored (`domainMatches`, `internal/cookies/autocookies_merge.go`), so `.fakegoogle.com.evil.tld` is not google.com.
5. **Within one jar** a name can still arrive from several domains. `compareCookieDomains` (`internal/cookies/jar.go`) is a total order that settles it: youtube.com beats google.com, then fewer labels wins, then dot-prefixed wins, then lexically smaller on the stored domain string. A row is skipped only when the incumbent ranks strictly better, so any permutation of the same set of rows loads to the same jar. Rule 3 (dot-prefixed) is subsumed by rule 4 today and is kept deliberately — the agreement is an accident of ASCII, not of intent.
6. Both maps are swapped under ONE `Lock`, so no reader can observe the new YouTube rows beside the old Twitch ones. Accessors that need two values to agree take a single `RLock` to match (`GetTwitchCredentials`, `YouTubeIdentity`).

**Expiry is CAPTURED, never filtered.** `cookieEntry` (`internal/cookies/jar.go`) is `{value, domain, expiry}`; `loadFrom` parses Netscape field 5 with exactly `rowExpired`'s semantics (TrimSpace, ParseInt, 0 on a parse error), so "expired" means the same thing to the jar and to the merge. Nothing in the jar acts on it, and that disagreement is load-bearing: `mergeCookieFiles`/`rowExpired` (`internal/cookies/autocookies_merge.go`) remain the only pruner, and `RefreshCookiesDetailed` detects credential loss by comparing what the jar holds against what the merge produced. Make the two agree here and the signal vanishes; drop rows here and `GetCookieHeader` silently sends less. Expiry surfaces as a diagnostic only, and the two accessors are not equally shipped:

- `ExpiredAuthCookiesFor(platform, now)` has one production caller — the startup `Cookies loaded` line (`cookiesLoadedFields`, `cmd/moombox/services.go`), which emits it per platform as `expiredYouTubeAuth` / `expiredTwitchAuth`.
- `AuthCookieHorizonFor(platform)` and `TwitchLoginExpiry()` reach production through `CookieJar.HorizonLogFields()`, the single producer of `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` — ISO-8601 UTC, or `none` when there is no expiry to run out. Those three fields ride exactly TWO log lines: the startup `Cookies loaded` line and `refreshCookiesDetailed`'s `cookie refresh succeeded` line — the refresh's SUCCESS arm, and the only one of its three outcome-switch arms that both stands downstream of the write and the jar reload and credits this pass for it. The other two arms are downstream too but carry no horizon: `"cookie refresh verified one platform and lost another"` (a partial credential loss) and `"cookies still verify, but this pass could not confirm the browser refreshed the profile"` (the historically common Firefox outcome, where crediting the pass would be the unearned claim that branch exists to stop making) — so a refresh that lands in either of those logs no horizon at all, and the settling observation below cannot be made from it.

  Read against each other they are the settling observation for whether a browser refresh renews the Twitch `auth-token`. **No UI carries a horizon**: there is no badge, payload key or API field, and none is planned. `TwitchLoginExpiry` is its own accessor because `login` is deliberately outside `twitchAuthCookieNames` and stays outside it — that list drives an alarm, this is a diagnostic. Timestamps only, never a value.

Per-platform rather than YouTube-only because `RefreshService` has no Twitch refresh at all: `checkTwitchAuth` validates and never rotates, and nothing else writes a Twitch cookie back — `processYouTubeSetCookies` is YouTube-only and `trackedCookieName` refuses any origin off the Google platform. So the expired-count is the earliest warning a Twitch credential is running out.

Whether the browser path's twitch.tv navigation renews `auth-token` is **UNMEASURED** — see `platform-services.md §Twitch Authentication` for why there is no in-process keepalive and for the settling observation that would answer it.

**ENOENT-as-empty is a ruling, not an oversight.** A missing file loads as an EMPTY jar, both maps cleared (`Load`, `internal/cookies/jar.go`). Deleting `cookies.txt` is how an operator logs Moombox out, and keeping the last good session in memory until restart would make a deliberate delete do nothing observable. The race objection does not apply, and `Load`'s doc comment carries the derivation: every writer goes through `writeFileAtomic` (`internal/cookies/cookie_files.go`), which writes a temp file and renames without unlinking the destination; on Windows `os.Rename` is one `MoveFileEx(..., MOVEFILE_REPLACE_EXISTING)` with no `DeleteFile` ahead of it, and `os.ReadFile`'s open asks for `FILE_SHARE_READ|FILE_SHARE_WRITE` and *not* `FILE_SHARE_DELETE` — so a `Load` in flight makes the RENAME fail loudly instead of being made to read a missing file. On Linux `rename(2)` replaces the name atomically. Re-derive that paragraph if a writer ever stops going through `writeFileAtomic` or starts removing the target first.

**Any OTHER read failure records the path and the reason.** A `Load` that fails with anything but ENOENT — the compose `user:` uid mismatch is the likeliest container shape — leaves both maps ALONE (a failed read is no evidence that the credentials in memory are wrong) but still records `filePath`, because `Reload()` short-circuits on an empty one: left empty after a failed boot load, the 30-minute pass, every `SyncCookies` before an extraction and `twitch.Auth.Reload` would all return nil having read nothing for the life of the process, and repairing the permission on the host would do nothing until a restart. It also records WHY, readable through `LastLoadError` (`internal/cookies/jar.go`): a bounded (200 bytes, cut on a rune boundary) sentence built from the path the caller passed in plus a `syscall.Errno`, in which a path too long for the bound loses its front, so the file name and the cause are always there (`cookieLoadErrorSentence`) — never copied out of an error’s own message, so no producer can put a line of the file in a badge title. The narrowing to an errno is what makes that structural rather than incidental: `os.ReadFile` always fails with an `*fs.PathError` whose `Err` is an errno, so the realistic case keeps the platform’s own words (“permission denied”, “Access is denied.”), but `*fs.PathError.Err` is an `error` and `cookieJarReadFile` is a SEAM — an `os.Root`-rooted open, a decrypting reader or a network-mount shim would each bring their own error type with them, and any cause that is not an errno renders the fixed phrase “the file could not be read” beside the path.

Empty for an ABSENT file, which is never-configured rather than unreadable. The refresh pass projects it onto `AuthStatus.CookieFileError` and from there onto both dashboards; before it existed, a `cookies.txt` sitting unreadable on the volume rendered on both of them as “you have not set cookies up”.

**Essential YouTube cookies (21):** SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, __Secure-1PSIDTS, __Secure-3PSIDTS, __Secure-1PSIDCC, __Secure-3PSIDCC, LOGIN_INFO, VISITOR_INFO1_LIVE, VISITOR_PRIVACY_METADATA, YSC, __Secure-ROLLOUT_TOKEN, CONSENT, PREF, SOCS. SOCS is the consent YouTube reads in CONSENT's place (yt-dlp `378ae9f9f`); it is kept so a browser's own consent survives Load, extraction and Set-Cookie admission and the page cookies forward it rather than `SOCS=CAI` (platform-services.md § Cookie-Based OAuth). Every other essential name is admitted on a YouTube or a Google domain, one credential platform; SOCS only from a domain `www.youtube.com` is sent (`.youtube.com`, `youtube.com`, `www.youtube.com` — `admitsEssentialYouTubeName`, `youtubeOnlyEssentialCookies`, in `Load`, browser extraction's `isEssentialCookie` and an unscoped Set-Cookie's `trackedCookieName` alike). google.com keeps a consent of its own, and admitted into the jar's one name-keyed map it stood in YouTube's slot and rode every Innertube call (`GetCookieHeader`) as a consent YouTube was never given; yt-dlp's YouTube requests carry `_get_cookies('https://www.youtube.com')` (`_youtube_cookies`, `_base.py:777-779`), which a google.com cookie does not domain-match.

An expired SOCS is held, as `Load` holds every row, but `GetCookieHeader` leaves it out, as the page header does (`youtubeSees`) and as yt-dlp's `add_cookie_header` does (`return_ok_expires`, behind `get_cookie_header`, `cookies.py:1405-1409`); every other expired name is still sent, by `Load`'s contract. A Set-Cookie value refresh may cross YouTube and Google rows, but never carries a SOCS across `www.youtube.com`'s read (`crossesConsentSides`, `resolveRowUpdate`'s rule 3), and the deletion of a SOCS `Load` would not keep logs at Debug, not as an essential cookie's. The two headers still differ where the jar holds no SOCS upstream would read: the page header sends `SOCS=CAI` in its place, the Innertube header none.

**Essential Twitch cookies (4):** auth-token, twilight-user, login, name.

**Two predicate tiers per platform.** The STRICT pair answers "is a complete working set present right now"; the LOOSE pair answers "was this install ever configured for the platform", which is the question the auth-loss gate and the status badges actually ask.

| Platform | Strict | Loose | Loose name set |
|----------|--------|-------|----------------|
| YouTube | `HasYouTubeAuthCookies()` — SAPISID, __Secure-3PAPISID or __Secure-1PAPISID AND LOGIN_INFO (yt-dlp's `_has_auth_cookies`, `_base.py:828-833`; `TestSecure1PAPISIDAloneIsSignedIn`) | `HasAnyYouTubeAuthCookie()` | `youtubeAuthCookieNames` (10) — SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, LOGIN_INFO |
| Twitch | `HasTwitchAuthCookies()` — auth-token present | `HasAnyTwitchAuthCookie()` | `twitchAuthCookieNames` — auth-token, twilight-user |

A file holding SAPISID with LOGIN_INFO cleared is a CONFIGURED platform with BROKEN credentials — exactly the state worth reporting — and the strict predicate reads it as never-configured. Every name in a loose set must also be in the corresponding essential set or `loadFrom` drops it before the predicate can see it.

**`login` is deliberately NOT in `twitchAuthCookieNames`**, and the reason is the alarm the list drives, traced end to end in that variable's doc comment: `HasAnyTwitchAuthCookie` → `refresh`'s `hasTWCookies` → `shouldFireRecovery` → `OnRecoveryNeeded("twitch")` → `runState.handleRecoveryNeeded` (`cmd/moombox/monitor_callbacks.go`). The alarm does not require a failed validate — `checkTwitchAuth` (`internal/cookies/refresh_twitch.go`) returns a conclusive `(false, nil)` **without issuing any request** when auth-token is absent, and `shouldFireRecovery`'s first-conclusive-check arm then returns `cookiesPresent` verbatim. A file holding `login` and no auth-token would therefore fire "twitch auth lost" on the first check of every start. `twilight-user` earns its place instead: it is Twitch's own record of the signed-in user, and `mergeCookieFiles` can prune a lapsed auth-token out from under it. That variable's comment also ENUMERATES the four silent Twitch states rather than claiming a superlative, and names which of them `ChatDownloader.noteMissingLogin` (`internal/twitch`) reports.

**Key methods:**

- `GetCookieHeaderFor(platform)`: builds a `Cookie:` header from one platform's rows and no other's, pairs sorted by name for determinism. `GetCookieHeader()` is the YOUTUBE one — every production caller is a YouTube request path — so Twitch rows do not ride along on authenticated youtube.com requests.
- `YouTubePageCookieHeader()` / `YouTubeConsentCookieHeader()` (`internal/cookies/page_cookies.go`): the header a YouTube HTML page is fetched with — the jar's YouTube cookies, or only SOCS and PREF, with `SOCS=CAI` unless signed in and PREF carrying `hl=en&tz=UTC` — built under one read lock and never written back (platform-services.md § Cookie-Based OAuth). The jar's own SOCS and PREF count only when `www.youtube.com` would be sent them and they have not expired (`youtubeSees`): `Load` keeps an expired row, and a google.com PREF, and neither is YouTube's (a google.com SOCS `Load` does not admit at all, above).
- `GetCookieFor(platform, name)`: one platform's value by name. `GetCookie(name)` reads the **Twitch** jar; the generic name is a deliberate mismatch, since its sole in-tree consumer is `internal/twitch/auth.go` fetching `auth-token`, and routing it to the YouTube jar would de-authenticate Twitch silently (IRC treats an empty token as "connect anonymously").
- `GetTwitchCredentials()`: returns `auth-token` and `login` as a pair under ONE `RLock`, because `internal/twitch/chat_irc.go` builds one IRC handshake out of both and a pair read under two locks is not a pair.
- `GenerateAuthorizationHeader(origin)` / `GenerateAuthorizationHeaderForUser(origin, userSessionID)`: SAPISIDHASH + SAPISID1PHASH + SAPISID3PHASH, each `SHA1(timestamp + " " + sid + " " + origin)` with one timestamp shared across all three (`makeSidAuthorization`); bound to a user session id, each hash input is prefixed `userSessionID + " "` and each part gains a `_u` suffix, as yt-dlp's `_make_sid_authorization` builds it (`_base.py:762-775`). An empty id is the unbound form exactly. Returns "" for any origin outside `allowedSAPISIDHASHOrigins`, which is defence in depth — Google's auth uses the origin as a shared secret, so a caller must never be handed a valid hash bound to an attacker-supplied one.
- `YouTubeIdentity()`: SHA-256 over `SAPISID + NUL + LOGIN_INFO`, "" when either is missing — SAPISID falling back to `__Secure-3PAPISID`, then `__Secure-1PAPISID` (so it is "" exactly when `HasYouTubeAuthCookies` is false), with that fallback inlined rather than delegated to `GetSapisid` so both reads happen under ONE `RLock` (the two must be kept in sync by hand). An opaque equality token for "which Google account is this" — never a credential, never displayed. LOGIN_INFO is the load-bearing half: SAPISID identifies a SESSION, not an account, so a fingerprint over it alone would be blind to an account switch. The rotating `__Secure-*PSIDTS`/`SIDCC` names are excluded because they would fire on every refresh cycle.
- `TwitchIdentity()`: SHA-256 over `auth-token + NUL + login`, both read under ONE `RLock` for the reason `GetTwitchCredentials` documents. `""` means **no Twitch credentials at all** — deliberately NOT `YouTubeIdentity`'s "either half missing" rule. The question here is "is this the same credential PAIR a downgrade was observed under", and a token with no `login` beside it is one of the four downgrade routes rather than an unanswerable state: folding it to `""` would make the operator's fix, adding the `login` row, compare equal to the breakage it replaced. A token rotation therefore reads as a change, which is the cheap direction — one re-check and one IRC reconnect that the credentials pass.
- `Reload()`: re-reads from the same file path; a no-op when the jar came from no file — which, since the failed-read arm records the path, does not happen after a boot read that failed. `Load` (`internal/cookies/jar.go`) memoises the file's `(size, mtime)` pair only when the stats before and after the read agree, the file is ≥ 2 s old and no concurrent install landed after it; a later `Load` whose stat matches the recorded pair skips the re-parse.

**Thread safety:** All methods are protected by `sync.RWMutex`. Nil-receiver-safe where a caller may legitimately hold none (`HasAnyYouTubeAuthCookie`, `HasAnyTwitchAuthCookie`, `ExpiredAuthCookiesFor`, `AuthCookieHorizonFor`, `TwitchLoginExpiry`, `YouTubeIdentity`, `TwitchIdentity`).

### Refresh Service

`RefreshService` periodically validates cookies against the actual platform APIs and refreshes YouTube session cookies from `Set-Cookie` headers.

**`accounts.google.com/RotateCookies` is not a YouTube keepalive, and is not to be re-proposed** (verified and rejected 2026-08-25). The endpoint exists and answers, but it rotates only `__Secure-1PSIDTS` / `__Secure-3PSIDTS`, which YouTube's Innertube auth never reads: yt-dlp's auth predicate is `LOGIN_INFO` plus one of the `*APISID` cookies, `SAPISIDHASH` self-refreshes from a static cookie and a timestamp, and the jar's own identity fingerprint excludes exactly those two names because they rotate constantly. Ordinary page traffic already rotates whatever rotates, so a scripted rotation would add a second writer to a session that dies precisely because a browser elsewhere rotated it first — the remedy is isolation (the managed setup profile as the sole rotator), not more rotation. `/RotateBoundCookies` (DBSC) is the TPM-signed successor and unreachable from Go regardless.

**Lifecycle and the single-flight.** Three entry points share one body, `refresh(ctx, allowFallback)` (`internal/cookies/refresh_pass.go`); `allowFallback` is the only thing that separates them.

- `Start(ctx)` runs the initial pass **synchronously on the caller's goroutine** with `allowFallback=false`, wrapped in its own inline `recover`, and returns when the first check of the process has finished. The caller is the cookie startup goroutine (`startCookieSequence`, `cmd/moombox/cookie_startup.go`), not `run()` itself: by owner ruling R-H2 and its amendment neither UI waits for the check. `run()` starts the web server and waits for its bind between the refresh service's seeds and that goroutine, and starts the TUI right after it; each of the pass's two round trips can take 15 s on a bad link, and both UIs used to stay down for all of it. Until the pass publishes its status, `AuthStatus.Checked` is false: `Unchecked()` is the one predicate for that, both UIs show "checking cookies" instead of the no-cookies and re-login indicators, and nothing decides anything on such a status. A finished pass sets `Checked` (a pass that panicked sets it too, in the single-flight's release), it is part of `authStatusChanged` so the first pass always fires `OnAuthChange`, and an install with no cookie file, whose `Start` is never called, is marked by `SkipStartupCheck` before the bind. Downloads and monitors start before the bind, as they did. The goroutine keeps the order `run()` had in line: the first check, then the profile seed (`StartProfileSeed`), then the browser refresh timer (`StartPeriodicRefresh`), one after the other, under its own inline `recover` and one more around each step. The seed and the timer follow a finished check in every case: when a dashboard recheck (`CheckNow`) lands between the bind and `Start` and wins the single-flight, `Start`'s own pass is turned away and `Start` waits for the pass in flight (`AwaitPass`, under `ctx`) before it returns. After the first check `Start` launches the ticker goroutine, which carries the same recover. Shutdown: `Stop` latches the service stopped, so a `Start` that comes after it starts nothing, and a `Stop` during the first check cancels it and `Start` returns without a ticker; the goroutine skips the steps it has not reached once the run context has ended, and the shutdown waits for it for at most 2 seconds (`cookieShutdownWait`) after stopping both cookie services.
- `CheckNow(ctx)` is `POST /api/cookies/recheck` and the TUI's `R C`, also `allowFallback=false`: it runs on a handler goroutine and must not buy a full page fetch.
- `doRefresh(ctx)` is the ticker, and the only path allowed `allowFallback=true`.

All three single-flight on `RefreshService.refreshInFlight` (guarded by `rs.mu`). A second caller is a **no-op** that returns `started=false` and logs at Debug — it does not queue and does not wait. It is **never** a `RefreshDeclinedCauses` member: that vocabulary belongs to `AutoCookieService`'s browser refresh and is pinned across three consumers. A dropped ticker tick waits a full interval rather than doubling up. A caller that has just rewritten `cookies.txt` and wants *that file* re-verified cannot be given that guarantee — the in-flight pass may have read the old file — so every caller in that position logs the skip at **Info**, through one helper — `recheckAfterCookieWrite` (`cmd/moombox/monitor_callbacks.go`), which says "status may lag until the next refresh" and names the gesture.

Its callers are the post-recovery re-check (hoisted above `runCookieRecovery`'s verdict switch and gated on `RefreshResult.Ran`, so a pass that ran and FAILED is re-read too — it moved the credential fingerprint just as a working one would), the post-`R F` re-check, the worker's job-triggered refresh (`OnCookieRefreshNeeded`), the TUI setup wizard's finish (gated on `SetupResult.Wrote`, the setup path's counterpart to `Ran`, since a wizard finish reports `SetupResult{}` on every error path and there is otherwise nothing to gate on), and the TUI's `E I` cookie-file import (gated on `ImportResult.Wrote`, the same flag the Web import route gates its own re-check on — `ImportCookies` is the fifth writer of `cookies.txt` and this is the `cmd/moombox` half of its "the caller runs the re-check" contract). Two credential writers share one further injected seam — `AutoCookieService.OnPassCompleted`, fired by both `StartPeriodicRefresh`'s tick and `StartProfileSeed`'s boot import, each gated on `Ran` — and that seam's body is wrapped in `postRefreshRecheckHook` (`cmd/moombox/services.go`) for its own recover, so a panic there costs only the re-check.

It is also the one site that re-checks under its caller's context rather than `context.Background()`: that context is the auto-cookie goroutine's own, which only the shutdown ends, and `AutoCookieService.Stop`'s join can only cover a re-check whose round trips that cancel reaches (see "The timer and the seed are the service's own goroutines" below). Both cookie timers (this one and `RefreshService`'s ticker) also run each tick under its own recover (`runPeriodicTick`; the ticker's inline wrap), because their goroutines recover outside the loop and an escaping panic would otherwise end the timer for the life of the process rather than costing one tick. The three `internal/web/routes` callers — the dashboard/Settings browser refresh, the setup wizard's finish and `POST /api/cookies/import` — call `CheckNow` bare, because that package has no operational logger. The wizard finish and the import are additionally DETACHED onto their own 45-second timeout rather than the request's context, and both answer with an explicit `Content-Length` (`jsonResponseSized` / `jsonErrorSized`, `internal/web/routes/cookies.go`) before flushing, so a client that navigates away can neither cancel the fingerprint comparison its own write caused nor be made to wait out a re-check it has already been answered for.

The length is the load-bearing half (owner decision O-L): with no `Content-Length` net/http falls back to chunked encoding and writes the terminating chunk only when the HANDLER returns, so the flush would release the headers and `fetch().json()` — which awaits the body — would sit through the entire re-check, inside a 60-second dialog budget that also has to cover `FinishSetup`. Every exit written inline in either handler literal goes through those two writers, success and refusal alike, and each keeps the trailing newline `json.Encoder.Encode` wrote so no body's bytes moved. `writeBrowserReadError` and `readCookieImportBody`'s refusals stay unsized, deliberately: both answer before the deferred re-check exists, leaving `Wrote` false, so no client is waiting on a pass behind them. They are safe under `CompressionMiddleware` only below its 1024-byte `gzipMinSize`, where `commitPlain` sends the identity body and leaves the header alone; above it `startGzip` deletes `Content-Length` and re-chunks, and the gzip trailer is written after the handler returns — accepted rather than exempted because the widest body either handler produces is a ~282-byte import success (three maps: the status snapshot, the relogin map and `activePlatforms`).

The re-check stays BLOCKING on the handler goroutine rather than being detached: that is the property the AST call-site test protects, and a goroutine would satisfy the test while deleting it. Each is gated on the pass having written (`SetupResult.Wrote` / `ImportResult.Wrote`), which is true on the jar-reload error exit as well as on success — the one error path that runs over a file already replaced, and the one where the re-check is worth most. **Every gesture that can write `cookies.txt` ends in one of these**, and that is a requirement rather than an observation: `refresh`'s status block is the only place the Twitch credential fingerprint is compared and the auth mark cleared, so a writer that reaches no pass is invisible until the ticker. The `POST /api/cookies/recheck` handler ignores the bool on purpose — its payload is a status snapshot, not a claim that this request produced it. Before the guard existed, a manual recheck landing during a ticker pass produced two guide fetches, two `Set-Cookie` merges and two interleaved `updateCookieFile` rewrites of the same file. An operator counting passes from clicks will therefore occasionally see a recheck produce no new pass in the log; that is the guard, not a broken button.

**Every `rs.mu` section inside `refresh` releases through `defer`.** This is a standing rule, not a style preference. The guard-release defer takes `rs.mu`, and `rs.mu` is a plain non-reentrant `RWMutex`: a panic unwinding with the write lock held would block that defer forever, park the goroutine holding `rs.mu`, and turn a loud crash into a silent hang in which every later `GetStatus()` blocks. The status update is scoped into a func literal for exactly that reason. Two unexported test seams, `refreshPassHook` (outside the lock) and `refreshLockedHook` (inside it), exist because the two windows need opposite things.

**Validation endpoints:**

| Platform | Method | Endpoint | Auth Check |
|----------|--------|----------|------------|
| YouTube | POST | `youtubeGuideURL` = `www.youtube.com/youtubei/v1/guide?prettyPrint=false` | Explicit login marker in the response, or inconclusive — see below |
| Twitch | GET | `twitchValidateURL` = `id.twitch.tv/oauth2/validate` | HTTP 200 = valid, 401 = conclusively invalid, anything else = inconclusive |

There is **one** guide URL. One package var names it: the guide functions share one exchange, so there is no second, byte-identical URL kept apart as if it were a different endpoint. Both vars (rather than consts) exist solely so tests can point them at an `httptest` server.

Provenance is asserted before status and before body, on both platforms: `authResponseIsOurs` (`internal/cookies/refresh_youtube.go`) requires the answering request's scheme and raw `host:port` to match what was sent AND the credential header (`Cookie` for YouTube, `Authorization` for Twitch) to still be present. Go strips manually-set credential headers on a cross-hostname redirect and the decision is sticky, so `origin → wall → origin` lands back on the right host carrying an uncredentialed body. The check is non-vacuous only because both callers refuse the empty-credential case before fetching, and it means anything only because `cookiesHTTPClient` carries no `http.CookieJar`.

**Liveness over presence.** Both checks distinguish three outcomes, not two. A
conclusive "not authenticated" is the only thing that fires credential
recovery, so it is claimed only from evidence — never from the absence of
evidence.

For YouTube that means an explicit marker in the guide reply:

- **Authenticated:** `logged_in: "1"` in `serviceTrackingParams`, or
  `loggedIn: true` in `mainAppWebResponseContext`.
- **Conclusively not authenticated:** `logged_in: "0"` in
  `serviceTrackingParams`, or `loggedOut: true` / `loggedIn: false` in
  `mainAppWebResponseContext`. (Measured 2026-08-27: an anonymous reply sends
  `logged_in` = the *string* `"0"` and `loggedOut: true`; it carries no
  `loggedIn` key at all.)
- **Inconclusive (an error, not a verdict):** anything else — a non-200, an
  answer that came back from a different host or without our credential
  header, or a 200 whose body carries no marker we recognise. A transparent
  intermediary such as a captive portal or corporate proxy answering 200 with
  HTML lands here; reading it as a verdict would tell an operator their
  working cookies were dead.

Positive markers are checked before negative ones, so a reply that
authenticated before always still does.

`youtubeGuideAuthVerdict` (`internal/cookies/refresh_youtube.go`) reads the body with `encoding/json` and falls back to literal substring needles (`guideLoginMarkersIn`/`guideLoginMarkersOut`) only when the body is not valid JSON at all. `loggedIn`/`loggedOut` are `*bool` because a real anonymous reply omits `loggedIn` entirely, and a plain `bool` cannot tell "the flag said false" from "the flag was absent". The `logged_in` param's `value` is `json.RawMessage`, decoded only for params whose key already matched, so one unrelated param gaining a non-string type cannot collapse the whole body to the fallback. The fallback caps the promoted string at `authBodyFallbackLimit` (16 KB) so session material deeper in the payload cannot reach a log line. An unreadable marker resolves to `errGuideLoginMarkerUnreadable`, which deliberately does **not** wrap `ErrAuthCheckNotAttempted`: a request did leave the process here, and `autocookies_profile.go`'s `attempted` flag turns on that distinction.

**One guide exchange, one writer.** `youtubeGuideExchange` (`internal/cookies/refresh_youtube.go`) makes the POST, reads the body to a verdict, closes it, and hands the verdict *and* the response back. It never writes anything. Two callers:

- `checkYouTubeAuth` — the VERIFY path, exported as `CheckYouTubeAuth` and wired into `AutoCookieService.VerifyYouTubeAuth` (`cmd/moombox/services.go`), where `checkPlatformAuth` runs it on the **rollback** decision on all three writing paths — `platformsToRestore` (the mounted-profile import, both arms) and `platformsToRestoreOnRegression` (the browser refresh and the operator's pasted import, regression arm only). It discards the response. A shared exchange that merged `Set-Cookie` headers itself would write the jar from the very response being used to judge the write.
- `checkAndRefreshYouTube` — the sole writer. Only on an authenticated, readable reply does it call `processYouTubeSetCookies`. Every error path and the never-configured gate return a nil response, so "a reply we could not read is not a reply anyone may write the jar from" is a fact about the return values rather than a rule to remember. It also records whose session the request carried (`youTubeSessionKey`, taken before the request — `YouTubeIdentity`'s hash, but present whenever SAPISID is, because `YouTubeIdentity` is "" without LOGIN_INFO and an empty key would switch the check off for such a session) and `updateCookieFile` writes the rotations only into a file that still holds that session: the file is re-read at write time, so an import that replaced it while the request was in flight does not receive the old session's rotated `__Secure-1PSIDTS` on top of the new session's rows (`errCookieSessionReplaced`, skipped at Debug).

`youtubeGuideExchange`'s three entry gates encode one rule, and only the FIRST may answer `(false, nil)`: nothing configured at all is a silent negative; configured-but-no-request-could-be-built errors with `ErrAuthCheckNotAttempted`, because a check that did not happen is not dead credentials. A jar with SAPISID and a cleared LOGIN_INFO is configured with broken credentials and its verdict has to come from YouTube.

**Set-Cookie ADMISSION, by parsed attributes.** `admitSetCookie(sc, origin)` (`internal/cookies/refresh_cookiefile.go`) is the outer layer: a header it turns down never reaches the write path in any form. No substring pre-filter opens this loop: one run before `Domain=` is parsed would drop every legitimate unscoped first-party rotation (RFC 6265 §4.1.2.3 host-scopes a Domain-less `Set-Cookie` to the responding host), and would not be the guard it looks like, since `x=youtube.com; Domain=evil.tld` passes a substring test on its value. Admission happens after the whole attribute list is read:

1. **Row-breaking characters** in the name, the value or the domain are refused outright (`hasRowBreakingChar`, `rowBreakingChars` = tab, CR, LF, NUL). Only the TAB is a live vector — `net/textproto` cannot deliver CR, LF or NUL inside a header value — and the other three are defence in depth for a write into a line-oriented file. This governs only what *this writer* may add; `CookieJar.Load`'s tolerance of tab-carrying rows is unchanged. (Pre-existing interop, out of scope and named: a row that already carries a tab inside its VALUE loads here but is rejected by yt-dlp's `MozillaCookieJar`, which requires exactly seven fields — a shared `cookies.txt` silently loses that row on the yt-dlp side.)
2. **SCOPED** (`Domain=` present): admitted only when the domain lies on the declared origin's credential platform (`cookiePlatformOf`). `accounts.google.com.evil.tld`, `evil.tld` and a bare `.` are all refused; the emptiness test is load-bearing, since an undeclared origin has no platform and bare equality would read `"" == ""` as a match.
3. **UNSCOPED** (no `Domain=`): the key keeps `Domain: ""` and stays host-scoped to the declared origin. Admitted only under a name the jar actually tracks — `trackedCookieName`, the union of `essentialYouTubeCookies` and `isGoogleOnlyAuthName`, neither of which contains the other — so an unscoped `foo=bar` never enters the file. `trackedCookieName` also refuses outright when the declared origin is not on the Google platform (`origin.platform() != originYouTube.platform()`), so only a Google-platform caller can admit an unscoped header at all today; adding `essentialTwitchCookies` there is a one-line change the day a Twitch caller arrives, and it needs that caller's tests rather than a guess.

Parsing details that are decisions rather than incidentals: leading and trailing SP/HTAB are trimmed from the name and the value **separately**, per RFC 6265 §5.2 step 3 (`strings.Trim`, not `TrimSpace`, so a stray CR cannot be quietly rescued past step 1). Quoted values are **not** de-quoted — §5.2 never strips DQUOTEs and no browser does; CPython's `SimpleCookie` strips because it implements the older RFC 2109. `Domain=` is lowercased at parse, not at comparison, because it becomes a map key. `Max-Age` takes precedence over `Expires` and is clamped to `maxCookieLifetime` (400 days, RFC 6265bis §5.5), which also makes the `now + maxAge` int64 overflow unreachable — a wrapped negative expiry reads as "not expired" to every `exp > 0 && exp < now` guard in the package. `Expires` is deliberately *not* clamped. Every attribute is read to the end of the header; breaking out early on `Max-Age<=0` would throw away the `Domain=` that usually follows it.

**Deletion semantics are CPython's**, from `http.cookiejar._cookie_from_cookie_tuple`: `Max-Age<=0` or an `Expires` at or before now deletes the row, keyed by domain+path+name, rather than storing it with an empty value. A bare `NAME=` with no expiry attribute is **REFUSED**, not treated as a third deletion form — this package cannot represent an empty-valued row at all (`CookieJar.Load` TrimSpaces the line, the trailing tab disappears, and the row reads as 6 fields and is skipped as malformed), and a reply that asserts "you are signed in" while blanking the credential that proves it is self-contradictory. `updateCookieFile` logs that refusal at Warn when the row carries an essential cookie.

**What an admitted header may do, by verb.** `admitSetCookie`'s doc comment is the authority here and states the rule in full; the summary below must agree with it. The design comes first, because the enforcement rules read wrongly in isolation — the owner's rule, verbatim: *"youtube cookies should allow Google cookies as well."* YouTube and Google are ONE credential platform (`cookiePlatformOf`), `.google.com` and `.youtube.com` rows live in one jar keyed by bare name, so a youtube.com reply is ENTITLED to move Google rows and does. The one thing declined is MISATTRIBUTION: a host-only cookie from www.youtube.com carrying no `Domain=` is a youtube.com cookie, so a NEW row for it goes on `.youtube.com` and is not invented on `.google.com`. As *observed*, Google's own cookies carry an explicit `Domain=` — that is a fact about Google's servers, not an invariant this code enforces — so nothing real is caught by that rule; the retired branch that guessed `.google.com` from the cookie NAME was minting a different cookie under a real one's name.

The three verbs have three different scopes, each enforced somewhere else:

| Verb | Unscoped header | Scoped header | Enforced by |
|------|-----------------|---------------|-------------|
| **REFRESH** | May rewrite an existing same-name row anywhere inside the declared origin's PLATFORM — an unscoped `SID=fresh` from a youtube.com reply *does* rewrite an existing `.google.com SID` row | Same fan-out, on the same rule: a `Domain=.google.com` rotation also repairs a stale `.youtube.com` twin, **when it is the only candidate** | `resolveRowUpdate` rule 2 (`origin.covers`) for the origin's own site, rule 3 + `sameCookiePlatform` for the rest of the platform. |
| **CREATE** | Only on the declared origin's own SITE — the insertion loop derives `domain = "." + string(origin)` and nothing else contributes | On the domain it declared, whenever that domain is inside the origin's platform. An admitted `SID=x; Domain=.google.com` from a youtube.com reply DOES create a `.google.com` row against a file holding none, and that is correct | `updateCookieFile`'s insertion loop, plus its platform guard (`cookiePlatformOf(domain) == origin.platform()`) |
| **DELETE** | Only inside the declared origin's own SITE, through `resolveRowUpdate` rule 2 alone — rule 1 needs a `Domain=`, rule 3 skips deletions, and the insertion loop skips them too | Rule 1's, reaching only rows whose domain it exactly scope-matches (`sameCookieScope`) | `resolveRowUpdate` |

#### **REFRESH**

`resolveRowUpdate` rule 2 (`origin.covers`) for the origin's own site, rule 3 + `sameCookiePlatform` for the rest of the platform. Rule 3 DISAMBIGUATES rather than guessing: it fires only when exactly one non-deleting candidate qualifies (`refreshes == 1`), so two same-name updates decline rather than let map order pick

Unscoped CREATE is narrower than unscoped REFRESH because a rewrite repairs a row the FILE has already asserted belongs to this platform, while a creation has no such prior assertion to lean on. Narrower still for one batch shape: an unscoped key is not INSERTED beside a scoped NON-deleting sibling of the same name (`hasScopedSibling`), because the scoped header has already claimed a row and the unscoped twin would override it by name in the jar. A scoped DELETION is not such a sibling — delete-plus-insert is "replace", and counting it would eat the replacement. The wider rule ("once any key of this name matched, treat every key of that name as handled") is wrong: a response rotating `SID` on both domains against a file holding only the `.google.com` row must still insert the `.youtube.com` one.

**The write path.** `processYouTubeSetCookies` declares `originYouTube` twice — once to `admitSetCookie`, once to `updateCookieFile` — because a Domain-less `Set-Cookie` is host-scoped to a response that no longer exists by the time the updates reach the writer. `cookieOrigin` is a SITE (`youtube.com` / `google.com` / `twitch.tv`), and it feeds three decisions: `resolveRowUpdate`'s rule 2, `sameCookiePlatform`'s Domain-less default, and the insertion loop's platform guard. The zero value is deliberately inert — `covers` reports false for every row and the insertion loop refuses everything — because declining to MATCH is not declining to WRITE: every update the matching rules turn down arrives at the insertion loop, which without an origin check appended new rows under a domain nobody declared.

Other write-path rules (`updateCookieFile`, `internal/cookies/refresh_cookiefile.go`):

- **Every** matching row is rewritten, not just the first, so multi-domain duplicates do not drift out of sync.
- Rows are rebuilt from exactly the first seven fields of the row being replaced, with the new expiry and value substituted. A live row can arrive split into 8+ parts (a value containing a tab), and assigning `parts[6]` alone would leave the rest of the value dangling.
- `#HttpOnly_` is **preserved** on rewrite (`parts[0]` is emitted verbatim — the file's own row is the authority) and **added** only on insertion, from `cu.HTTPOnly`. Nothing in the package treats the flag as a control, so a server that starts or stops sending `HttpOnly` costs a stale annotation, not a downgraded cookie.
- The include-subdomains flag is derived from a leading dot rather than hardcoded TRUE; `secure` follows a `__Secure-` name prefix.
- Deletions remove the row. Grow broadly, destroy narrowly: a deletion scoped to `.youtube.com` does **not** remove a host-only `www.youtube.com` row, even though RFC 6265 domain-matching covers it — browser extraction really does write host-only rows, and a stale row that keeps being sent is recoverable where a deleted credential is not.
- A read failure aborts (`ErrCookieFileUnreadable`, `internal/cookies/errors.go`): an unreadable `cookies.txt` is not an absent one, and consumers MUST discriminate it from every other failure, because the correct instruction is "fix the permission or mount problem", never "replace cookies.txt".
- The write ends in `writeFileAtomic`. A failure Warns and names the deployment mistake that causes it: a rename cannot replace a single-file bind mount, so `cookies.txt` belongs *inside* a mounted directory, not mounted as an individual file.
- Refused headers are counted, never logged. A `Set-Cookie` is the credential itself.

**Auth loss detection.** The service tracks previous auth state per platform. `shouldFireRecovery(everConcluded, prevAuth, nowAuth, checkErr, cookiesPresent)` is a pure function (table-testable without a network seam) and fires in two cases, both requiring `checkErr == nil && !nowAuth`: a **witnessed transition** (this platform concluded before and was authenticated then), and **startup dead-auth** (this platform's first conclusive check ever), the latter gated on `cookiesPresent`. `everConcluded` is per-platform (`ytEverConcluded`/`twEverConcluded`) and not the service-wide `hasCheckedOnce`, because `SetExpectedPlatforms` seeds `hasCheckedOnce=true` as soon as ANY platform is in the persisted list — using the shared flag would let one platform's presence mask a sibling that was never checked. `cookiesPresent` is the LOOSE predicate: a half-cleared session is a configured platform with broken credentials.

"Conclusive" is the three-outcome rule above, not merely "no network error": a non-200, an answer from the wrong host, and a 200 whose body carries no recognisable marker are all inconclusive too, and none of them fires recovery, moves the previous-auth baseline, or marks the platform concluded.

`SetExpectedPlatforms(platforms)` seeds the previous auth state from persisted config platforms so auth loss is detectable after a restart. `OnAuthRecovered` fires on the inverse transition — and, for a platform named by `SetUnrecoveredPlatforms(platforms)` (an auth failure a previous process announced and never closed, read from the open-alert state `cmd/moombox` persists), on that platform's first conclusive authenticated check, once, whatever the seeded baseline or `hasCheckedOnce` says: the fall happened before this process could witness it, and without this its `auth_recovered` close never came. The recovery-needed side is untouched. `OnCredentialsChanged` is separate and not a weaker `OnAuthRecovered`: a job parked because the signed-in account lacks a membership parked while auth was HEALTHY, so swapping accounts produces no auth transition to ride. It fires for BOTH platforms, against per-platform baselines (`prevYouTubeIdentity` / `prevTwitchIdentity`), and the two mean different things: a YouTube fire is "the signed-in ACCOUNT may have changed", a Twitch fire is "the credential PAIR changed".

The Twitch fire has a second subscriber — `cmd/moombox` broadcasts it to every live Twitch IRC chat downloader through `DownloadWorker.ReauthenticateTwitchChats`, which tells each one to re-read its credentials and reconnect (the count is downloaders TOLD, never "authenticated"). `OnAuthRecovered` broadcasts the same thing on its own edge, which is what a capture already in flight needs for the case this fire cannot see: a transient refusal that heals with the fingerprint UNCHANGED fires no `OnCredentialsChanged` at all. `shouldObserveCredentials` and `advanceIdentityBaseline` (both pure, both platform-agnostic) govern both — the baseline advances only on a check that was conclusive **and** authenticated, so a stale intermediate export cannot consume the edge, and the `baseline == ""` case fires once per process on purpose so an offline cookie swap is noticed at all.

**Two-tier liveness, and its pilot is ARMED.** Tier 1 is the auth check above. Tier 2 is `ObserveLiveness(platform, loggedIn)`, fed by three producers — two YouTube: the per-channel membership probe (every un-memoized channel each feed cycle, plus ONE nominated memoized non-member — the non-member memo's TTL is `membershipMemoTTL` in `internal/monitor/feed.go`, 6 h, and `armMembershipLiveness` nominates the memoized channel with the earliest horizon **that has not recently errored** (`membershipFetchErrored`, `internal/monitor/feed.go`) so the signal keeps firing on an install where every channel is memoized — when every candidate has errored the earliest errored one is tried anyway, bounded by `membershipLivenessMaxTries`) and the channel-independent `FallbackLiveness` probe injected by `cmd/moombox` (this package cannot import `internal/youtube`) — and one Twitch, below. Callers must filter their own inconclusive results out; reaching the method means "the platform told us", not "we asked".

Twitch's producer is the channel-independent `TwitchFallbackLiveness` probe, injected by `cmd/moombox` for the same reason and one more — `internal/twitch` imports `internal/cookies`, so the call is an import cycle in either direction. It asks `internal/twitch.Service.ProbeSessionLiveness` for a playback access token on one enabled configured Twitch channel (the first in config order) and reads `user_id` out of the token document (`PlaybackTokenSession`) — the question `checkTwitchAuth` cannot answer, because `oauth2/validate` returns 200 for a token that is valid but no longer entitled to authenticated playback. It runs under the YouTube twin's three conditions plus one: the jar must hold an `auth-token` right now (`HasTwitchAuthCookies`, the narrow predicate), because the probe SENDS that token and an install without it would get an anonymous playback token by design. A 401/403 is INCONCLUSIVE, not signed-out — `gqlRequest` raises `ErrTwitchAuthExpired` for both statuses and 403 is an edge block as often as a credential verdict. Nothing on this path writes `AuthStatus`; the capture-time mark (`NoteTwitchAuthLoss`) remains the only direct writer outside `doRefresh`'s own status block.

`const livenessRecoveryArmed = true` (`internal/cookies/refresh_liveness.go`) gates whether a tier-2 verdict may invoke `OnRecoveryNeeded`. It is true, armed by owner ruling with the five-day pre-arming soak skipped by that same ruling. A signed-out verdict that clears the per-platform dedupe logs `a liveness observation reports this platform is signed out, triggering recovery` at Warn and calls `OnRecoveryNeeded(platform)`; `ObserveLiveness`'s single log line carries `wouldFireRecovery` and `armed` (`armed=true`). The risk it accepts is not scoped to `auto_enabled` installs: on `auto_enabled = true` a spurious verdict drives a headless browser and notifies unless it races the auto-cookie single-flight; on `auto_enabled = false` there is no quiet case at all — `handleRecoveryNeeded` returns without calling the refresher, so a synchronous "Cookie Re-Authentication Required" (TypeError) fires every time, at a 30-minute per-platform cooldown, to the population least able to reach the remedy it names. It is a source constant, not a config flag: the only way back from a wrong verdict is to set it to `false` and rebuild.

Arming was an owner decision, taken 2026-09-03 and not a change made in passing. One question it had to answer first — what a genuinely dead session should do once the gate is true — was decided on 2026-08-29 and was BUILT before the flip. Tier 1 alone notifies once per process — `shouldFireRecovery`'s witnessed-transition arm needs `prevAuth` to have been true, and the first conclusive negative clears it — whereas an armed tier 2 would otherwise have re-fired every flat `livenessRefireWindow` for as long as the session stayed dead: 48 notifications a day for one loss. The owner ruled a back-off instead, and `recordLiveness` (`internal/cookies/refresh_liveness.go`) is where it runs: the first re-alarm lands one `livenessRefireWindow` (30 min) after the first alarm, `escalateLivenessRefire` doubles the window (`livenessRefireFactor`) on every alarm after that up to a `livenessRefireCap` of 24 hours, and `livenessRefireWindowFor` is what `recordLiveness` consults instead of the flat constant.

A conclusive signed-in observation calls `resetLivenessRefire`, which puts the platform back on the base — the escalation only: `lastRecoveryDecided` is left standing, because clearing it would let a healthy verdict from one channel swallow a dead one from the next channel in the same cycle. A tier-1 fire (`noteRecoveryDecided`) stamps the dedupe without escalating, so a platform is never pushed onto a longer schedule by the check that predates this signal. The schedule governs a real re-alarm: `recordLiveness` computes it, `ObserveLiveness` logs the answer, and the same answer decides whether `OnRecoveryNeeded` is called.

Four maps, deliberately separate (`internal/cookies/refresh.go`):

| Map | Written by | Read by |
|-----|-----------|---------|
| `lastLivenessObserved` | conclusive verdicts, **both** directions | `livenessObservedRecently` — the sole gate on paying for the fallback probe |
| `lastRecoveryDecided` | a signed-out verdict that cleared the dedupe (`recordLiveness`), and `noteRecoveryDecided` from a tier-1 fire | `recordLiveness`'s back-off check (`livenessRefireWindowFor`). A `LoggedIn` observation must never write here, or a healthy verdict could swallow a dead one from another channel in the same cycle |
| `lastLivenessKnown` | every outcome including `livenessInconclusive` | log-level selection only (`notable`) |
| `livenessRefireBackoff` | `escalateLivenessRefire` after every verdict that clears the dedupe (the first sets the base, each later one doubles, clamped at `livenessRefireCap`); `resetLivenessRefire` from a conclusive `LoggedIn` verdict — the ONLY writer that ever shrinks it | `livenessRefireWindowFor`, which `recordLiveness` consults instead of the flat constant. A missing entry reads as the base window. A tier-1 fire (`noteRecoveryDecided`) stamps the dedupe without touching this map |

`noteRecoveryDecided` is one-directional: the tier-1 fire stamps the map but never consults it, because suppressing the tier-1 fire would change behaviour that predates this signal entirely. `recordInconclusiveLiveness` touches none of the other three maps — recording an observation would make the next cycle skip the probe, silencing the signal for as long as it keeps failing; consuming the dedupe would swallow the next real logged-out verdict; and resetting `livenessRefireBackoff` would treat "inconclusive" as the reset condition when only a conclusive signed-in verdict is, letting an install stuck behind a captive portal or rate limit — inconclusive on every cycle — clear its own escalation every cycle too.

`livenessFreshWindow` (25 min) bounds how old the last conclusive observation may be before a periodic pass pays for the fallback probe. It **must** be strictly shorter than the refresh interval, because the fallback records its own answer through the same method — at one full cadence the probe would suppress itself on alternate cycles, halving coverage with no symptom. `NewRefreshService` enforces that against the interval it is actually handed, replacing anything at or below the window with the default and warning with both numbers. The lower bound is an *assumption about configuration*, not an invariant: the skip only works while membership observations arrive more often than the window expires, so an install with `monitors.feed_check_interval` above ~25 minutes pays for the fallback on roughly every other cycle. That degradation is bounded and one-directional.

**`authStatusChanged` is a CONTRACT, not an observation.** It is the `OnAuthChange` gate (`internal/cookies/refresh.go`) and compares six fields: the two auth booleans, the two cookies-present flags, and the two `RefreshVerdict`s. It deliberately excludes `YouTubeError`/`TwitchError`, whose text can vary between two occurrences of the same outcome, so comparing them would fire the callback on churn no verdict transition accompanies. The rule is stated forwards: **no `OnAuthChange`-driven surface may render the two strings; per-request surfaces may.** Both of today's readers are per-request — each pulls a `GetStatus()` snapshot it asked for — so neither depends on this callback and neither can go stale on screen. Widening the gate is the *precondition* for a push-driven surface that renders them, and is a deliberate change with its own cost. The verdicts and the cookies-present flags have to be in the gate: a platform going from conclusively-rejected to could-not-check leaves both booleans false, and on a boolean-only gate that badge transition was silent.

All six are surface inputs in their own right and each is pinned on its own — including the two `Authenticated` booleans, which every other row of the gate's table moves together with a verdict. Whether today's producers can move one without the other is a fact about the producers, not about the gate. **This paragraph is the contract restated, not widened: the exclusion list is unchanged and no `OnAuthChange`-driven surface may render `YouTubeError`/`TwitchError`.**

`AuthStatus` (`internal/cookies/refresh_auth_status.go`) is never marshalled — every consumer hand-projects it — and **every field has a reader**, which is a property to keep. There is no `LastCheck` string: no projection would carry it, and the obvious misreading ("the credentials were valid as of this time") is not what the timestamp of a pass that may have concluded nothing says. Anything re-added needs a reader in the same change, and if it moves on every tick it also needs a line in `authStatusChanged`'s exclusion list.

**`rs.status` has TWO writers, both under `rs.mu`, and the mark wins.** `refresh`'s status block is not the only writer of `rs.status`; `RefreshService.NoteTwitchAuthLoss(reason)` is a second. It writes the Twitch triple — `TwitchAuthenticated=false`, `TwitchVerification=RefreshFailed`, `TwitchError` = a sentence from a fixed `switch` of string literals — and records a `twitchAuthMark` carrying the reason and the `CookieJar.TwitchIdentity()` it was taken under. `refresh`'s block then CONSULTS that mark: while it stands, one `twEffective` value drives the status, the previous-auth baseline, `shouldFireRecovery`, the `OnAuthRecovered` transition and the identity baseline, so validate's 200 cannot flip any of them. It has to work this way — `oauth2/validate` answers 200 for a valid `auth-token` whether or not a usable `login` sits beside it, so two of the four chat-downgrade routes would otherwise be erased within one tick with nothing repaired. The mark clears on a **changed fingerprint alone**, with no authenticated gate: a swap to a REVOKED token must report the 401, not the stale reason from the pair it replaced.

Recovery rides the existing `shouldFireRecovery` dedupe and advances the same two baselines, so one loss raises one alarm. The reason never leaves the vocabulary: `twitchAuthLossMessage`'s arms are all string literals, so the set of strings `TwitchError` can hold is fixed at compile time, which is what makes it safe on the two per-request surfaces that render it. The mark is **process-local**: `twitchAuthMark` is a plain `RefreshService` field with nothing behind it, so a restart drops it and validate reports Twitch green again for the two missing-`login` routes until the next chat handshake re-takes it. And because `NoteTwitchAuthLoss`'s write lock plus its `OnAuthChange`/`OnRecoveryNeeded` fan-out are synchronous, its caller cannot be the IRC session goroutine that reports the downgrade — `twitchAuthLossHook` (`cmd/moombox/services.go`) delivers it through its own recover-guarded goroutine instead, because `chat.go`'s `OnAuthDowngrade` contract parks the read loop behind the call and must not block. The same hook value is handed to `twitch.Service.SetOnAuthLoss`, whose stream-status checks report `status-check-token-refused` from the Twitch monitor's poll goroutine on every check whose token Twitch refused and then answered signed out, while the jar still holds that token (G21) — the mark's idempotence is what makes a report per 15-second poll harmless.

**Timing:**

- `RefreshService` interval: `defaultRefreshInterval` = 30 minutes. `NewRefreshService(jar, 0, log)` is how `initServices` constructs it, so nothing in production feeds the interval parameter; `cookies.refresh_interval` drives the *auto-cookie* periodic refresh instead.
- `authCheckTimeout` = 15 seconds per platform.
- `livenessRefireWindow` = 30 minutes — the BASE of the per-platform tier-2 back-off, doubling per alarm (`livenessRefireFactor` = 2) to a `livenessRefireCap` of 24 hours. Its own constant on purpose: it is neither the notification cooldown in `wireMonitorCallbacks` nor `defaultRefreshInterval`, however the three numbers line up today.
- `livenessFreshWindow` = 25 minutes.
- Initial check runs synchronously inside `Start()` (on `cmd/moombox`'s cookie startup goroutine) with `allowFallback=false`; tier-2 coverage therefore begins one cadence in.

### Auto-Cookie Service

`AutoCookieService` acquires credentials into `cookies.txt` four ways — an interactive browser login, a headless browser refresh, a browser-free import of a mounted browser profile (which `cookies.acquisition = "profile"` also selects on a desktop that HAS a browser), and `ImportCookies`, the operator-supplied Netscape file that `POST /api/cookies/import` and the TUI's `E I` chord both deliver. That last one is the FIFTH writer of `cookies.txt` (with the refresh service's rotation write) and inherits the same catalogue as the rest: read through the `readCookieFile` seam and abort on anything but ENOENT, merge through `mergeCookieFiles` keyed by name+domain+path (RFC 6265 identity — Secure and HttpOnly are attributes, not identity, and stay out; the jar itself remains path-blind, so two paths still load as one entry and the file merely stops losing the other row), write through `writeFileAtomic`, and never emit an empty-valued row — it filters those out of the paste before merging AND out of the merged text before writing, which also repairs any an older writer left behind.

The browser refresh and the setup finish merge through `mergeBrowserCookies`, which leaves the browser's empty-valued rows out the same way: a Firefox NULL or an unencrypted Chromium row read as empty would otherwise replace a working `LOGIN_INFO` by name+domain+path and sign the jar out. The five share no lock, and they never will: the containment is a SLOT, not a mutex over `cookies.txt` (owner decision O-D, 2026-09-17). `ImportCookies` claims the same refresh slot `RefreshCookiesDetailed` and `StartSetup` gate on (`refreshSlot`, `internal/cookies/refresh_slot.go`, which records its holder: a refresh pass or an import), and answers "please try again shortly" — `ErrRefreshInProgress` while a pass holds it, `ErrImportInProgress` (which matches `ErrRefreshInProgress` under `errors.Is`) while another import does; HTTP 409 on the Web route, the identical sentence `/api/cookies/auto-setup/start` gives for the identical holder. An import never launches a browser, so `Stop` does not wait out the launch window on one. What decides it is the DIRECTION of the loss, not the width of the window: an import racing a pass does not lose a rotation the next 30-minute pass repairs, it loses the OPERATOR'S PASTE, which the pass replaces on the recovery path with the dead rows that raised the alarm, while the import reports `Wrote=true` with both platforms `ok`.

The window it closes is the read → verify → write gap, which also contains the pre-write snapshot's two verification round trips — `checkPlatformAuth` bounds those with one `authVerifyTimeout` (12 s) PER PLATFORM, run concurrently, so the gap is bounded by ~12 s rather than by the merge alone. A re-read-and-re-merge containment is not taken: it would keep the import always-accepted at the cost of an extra read and merge in every pass, and a refusal the operator retries within two minutes is the cheaper honest answer. One residual is deliberate and stated: the setup wizard's finish is another writer, a third party to this exclusion, with its own read → write gap, gated by the SETUP slot rather than this one, and widening the import's gate to cover it would refuse a container operator's only re-authentication route for the 60 s grace a stale setup slot lingers.

**A paste is verified before it is committed, and reversible per platform.** `ImportCookies` runs the same protection the two refresh paths do, on the same machinery: `snapshotPlatformAuth` (`internal/cookies/autocookies_profile.go`) verifies both platforms BEFORE the write — skipped when there is no `cookies.txt`, so a first acquisition costs no round trips — the post-write check goes through `platformsToRestoreOnRegression`, and a platform it names gets its previous rows back through `restorePlatformRows`. Until 2.8.7 the import had none of this: a paste whose rows for one platform were dead REPLACED that platform's working rows (the merge lets the pasted value win by name+domain+path), the operator was told the credentials failed rather than the paste, and the sibling platform — carried verbatim by the merge, and verifying — made the whole import look partly successful. The **regression arm only**, deliberately: `credentialAccepted` already accepts an inconclusive check over a credential a human just supplied, so rolling that same credential off the disk would report it accepted and gone.

`ImportResult` carries a per-platform outcome. Four of them reach the wire — `ImportInstalled`, `ImportRolledBack`, `ImportRejected` (rejected with nothing established to give back) and `ImportUnchanged` (the paste carried no row for that platform) — and a fifth, `ImportUnknown`, never does: it is the zero value, the guard for the exits that fail with `cookies.txt` already replaced, and every one of those returns an error the route answers with a `jsonErrorSized` before the payload is built. It also carries `RollbackProtected`, which is false both when there was nothing to protect and when the pre-write load failed ("rollback protection is off", the refresh path's own sentence; the import proceeds either way, because refusing it would throw away credentials the operator supplied by hand). The two per-platform outcomes reach the dashboard as `youtubeImport` / `twitchImport`; a rollback leaves `authenticated` **true** and the verification `"ok"` — truthfully, about the restored rows — so those keys are the only thing that stops the toast reporting a discarded paste as a success. A rollback that cannot be written or reloaded FAILS the call rather than being reported as one.

**What `cookies.auto_enabled` means.** Two independent liveness mechanisms on two independent timers, **not** a primary and a fallback. The in-process Go refresh (`RefreshService`) always runs, on its own timer and on demand from either UI (`R C` / `POST /api/cookies/recheck`); the monitors reach it only through `ObserveLiveness`, never `CheckNow`. The headless-browser refresh is a **much slower** second timer that exists only when the flag is on. The flag owns that timer, the one automatic recovery attempt, and — the exception this table has to name — the `SetExpectedPlatforms` read at `cmd/moombox/main.go:276-278`. Nothing else.

| Surface | Mechanism | Gated on `auto_enabled`? |
|---------|-----------|--------------------------|
| `RefreshService` (monitors + own timer) | in-process | never |
| `StartPeriodicRefresh` | headless browser | yes — it *is* that timer |
| automatic recovery (`OnRecoveryNeeded` → `handleRecoveryNeeded`) | headless browser, one attempt | yes |
| `SetExpectedPlatforms` seeding (`cmd/moombox/main.go:276-278`) | — | yes |
| `R F` / Web shift+click / the Settings-page twin | best-available ladder | **no** — the flag only picks the rung, and never causes a decline |
| `R C` / `POST /api/cookies/recheck` | in-process | never |
| `StartSetup` (interactive login — the TUI's `E L` and first-run wizard, the dashboard's `Re-login` click and `/auto-setup/*`) | browser | never — acquisition, and an explicit gesture |

**`cookies.acquisition` picks the path; `auto_enabled` picks whether a browser may run.** They compose and neither replaces the other. `acquisition` is read LIVE through `AutoCookieService.AcquisitionMode` (`internal/cookies/autocookies.go`), the same injected-predicate shape as `BrowserLaunchAllowed` and `ConfiguredBrowserOverride`. In a refresh it changes one thing, `importedFromProfile`: `"profile"` forces the browser-free import branch regardless of `resolvedBrowser()`, which is the only route to reading a REAL signed-in profile on a Windows desktop; `"auto"` leaves the rule as it was. It has two values by ruling — a `"browser"` value would mean "launch when a browser resolves", which is what `"auto"` already means, so the two could not be told apart at any site. Under `"profile"` the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` (below). `resolvedBrowser` is untouched, and `StartSetup` never consults the mode: the interactive login is acquisition, and gating it would make a fresh install in `"profile"` mode unable to create the profile it is told to read. The no-source outcomes are unchanged, because the missing-directory block runs BEFORE the decision: `"profile"` with no profile directory still returns `ErrProfileNotFound`, and `"auto"` with no browser and no profile still returns `ErrNoBrowserFound` — both still rung 3.

**The periodic timer is `gateExempt`.** `browserGatePolicy` (`internal/cookies/autocookies.go`) has two values: `gateApplies` (the zero value, so anything that forgets to say gets the safe answer) for every caller acting on a live operator intention, and `gateExempt` for `StartPeriodicRefresh`'s goroutine and nothing else. `main.go` starts that loop only when the flag was true at boot, so the flag has already been consulted; re-asking it per tick would leave an operator who switched it off without restarting with the timer still running *and* silently switched to browser-free imports of a profile nothing changes between ticks. Flipping the flag off at runtime therefore leaves the timer launching browsers until restart, **by ruling** — the restart-required label both UIs carry is the honest cover. Do not "fix" it.

**`R F` is a three-rung ladder and never dead-ends.** `R F` (TUI), the dashboard header's shift+click, and the Settings page's "Refresh cookies from browser profile" button are one gesture: refresh by the strongest means available. The Settings twin exists because a modifier key does not exist on a phone or tablet, which left a mobile-only operator with dead cookies, an updated profile and no trigger at all on exactly the workflow designated for Docker; it calls `app.autoCookieRefresh()` directly rather than adding a second implementation.

1. Browser launching allowed AND a browser available → launch the headless browser, refresh the profile, import.
2. No browser launch (flag off, or no browser present) but a profile IS present → import from the profile immediately.
3. No browser profile at all → run what `R C` runs, and say so.

`cookies.acquisition` moves the gesture down the ladder without changing it: in `"profile"` mode rung 1 is never taken, rung 2 always is, and rung 3 is untouched. The TUI's pre-flight line and the dashboard's toast both name the mechanism that will actually run — `Importing cookies from the browser profile...` instead of `Running browser cookie refresh...` — because the browser sentence is a claim only one of the two modes can support. The two surfaces render the SAME sentence (`cookieRefreshPreflightToast` in `web/public/modules/utils.js`, `cookieRefreshFeedback` in `internal/tui/app_actions.go`); unlike the rung-3 pair they name no per-surface affordance, so they do not diverge. Afterwards the mechanism is not a guess: `RefreshResult.Mechanism` records which source the pass actually used (`"browser"`, `"profile-import"`, or empty when it declined before choosing), rides the wire as `mechanism` on `cookieRefreshOutcome`, and feeds ONE subject-producer per surface — `cookieRefreshMechanismLabel` in `internal/tui/app_actions.go` and in `web/public/modules/utils.js`.

The RESULT outranks the mode there, and the mode is only the fallback, because the two disagree wherever the host decides rather than the setting: a machine with no browser installed imports in `"auto"` mode, so a sentence keyed on the mode would open `Browser cookie refresh ...` after a pass that launched nothing. Only the SUBJECT is shared; each surface keeps its own predicates. The `renewed === false` arm keeps its browser wording on both, and is allowed to: `renewed := importedFromProfile || browserActed`, so an import that reaches a verdict always renewed and that arm is unreachable for one.

Rung 3 is one exported predicate, `cookies.IsNoBrowserProfile` (`internal/cookies/errors.go`), so the two surfaces cannot diverge: it is exactly `ErrProfileNotFound` or `ErrNoBrowserFound`, both from the same pre-work missing-directory check. The TUI branches on it in `internal/tui/app_update.go` and returns `recheckCookiesCmd()` — R C's own command, not a second implementation — so the sentence leads a real refresh. The Web half (`autoCookieRefresh`, `web/public/app.js`) branches on the STATUS (404 for `ErrProfileNotFound`, 424 for `ErrNoBrowserFound`), never on the prose, and then awaits `recheckCookies()`. The two sentences differ **on purpose**, each naming its own surface's affordance: the TUI's is the owner's copy verbatim, `No browser profile found, running R C instead...` (ellipsis included), and the Web's is `No browser profile found, running a normal cookie refresh instead...`. The ladder still declines nil-error on the running-service causes in `RefreshDeclinedCauses` (a setup or another refresh already in flight, or no platform with cookies worth refreshing) — unchanged and correct. `R C` is never gated.

Rung 3 deliberately EXCLUDES every profile-import failure — `ErrProfileDirUnreadable`, `ErrProfileNotADirectory`, `ErrCookieDBNotFound`, `ErrCookieDBLocked`, `ErrCookieDBUnreadable`, `ErrNoCookiesInProfile` — and `ErrProfileDirNotOptedIn`, the config refusal beside them (the profile IS there; the remedy is one setting). Those mean the profile IS there and is wrong in a diagnosable way, each from a pass that RAN, and each carries the only guidance the operator has. Folding them into the fallback would replace real diagnosis with a recheck that cannot fix any of them. `ErrNoCookiesInProfile` is likewise never to be redefined as the auth-cookie predicate: it means the profile held nothing Moombox would keep, and narrowing it to "no auth cookie" would discard `CONSENT`, which the essential list deliberately keeps.

**Every AUTOMATIC browser-free import runs only when there is no `cookies.txt` to lose.** `automaticImportGuard` (`internal/cookies/autocookies_profile.go`) is that rule, and it is ONE rule with exactly two automatic callers: `decideStartupSeed` (the boot import) and `StartPeriodicRefresh`'s tick when that tick would be browser-free — browser-free because no browser resolves, or because `cookies.acquisition = "profile"` makes the pass an import regardless of the host. That second case is a desktop reading the operator's REAL profile, and it is exactly the scheduled re-read over live credentials this rule refuses. "Nothing to lose" means **absent, or present with zero cookie rows**; an **unreadable** file ABORTS (`autoImportCookieFileUnreadable`) rather than counting as absent, and `StartProfileSeed` Warns on that one stand-down because it is the operator-actionable case.

The asymmetry is the whole argument: a false "nothing to lose" imports over credentials and the operator may not find out until a recording fails, while a false "something to lose" costs one keypress. So the predicate does not need to be accurate — it needs zero false "nothing to lose" answers. `countNetscapeCookieRows` (`internal/cookies/autocookies_profile.go`) therefore **must over-count**: it counts lines that are neither blank nor plain comments, so a malformed row, an unrelated domain and an expired row all count. Over-counting can only produce the cheap error. Any replacement must keep that direction, which is also why "present but holding no auth cookies for either platform" is ruled out as a definition — deciding it needs a per-platform predicate, and a wrong one fails in the expensive direction. One shape is reachable only by misconfiguration and is named here so it is not rediscovered: `auto_enabled` on, a browser-free tick (no browser resolves, or `"profile"` mode), a profile directory whose import fails permanently, and no `cookies.txt` to protect — every tick imports, fails and Warns `periodic auto-cookie refresh failed`, with no back-off. The remedy is the configuration; a back-off there would be a mechanism to contain a mechanism.

The guard is **not for the manual triggers**: `R F`, shift+click and the Settings twin must keep importing over whatever `cookies.txt` holds, because replacing a live cookie file out of a profile the operator just updated by hand *is* the gesture, and it is the only path a container has. And it is **not for the recovery path**: `OnRecoveryNeeded` and the worker's `OnCookieRefreshNeeded` reach `RefreshCookiesDetailed` without passing through it, because recovery fires only on a conclusive not-authenticated — refusing the one automatic import most likely to fix the problem, on the grounds that a file exists which has just been proven not to work, would be backwards. That exemption stops being safe if a recovery producer is ever added that can fire on an INCONCLUSIVE verdict. The two-platform case (only one platform died) is covered not by the guard but by `RefreshCookiesDetailed`'s own abort/merge/rollback, which re-checks at write time: it verifies each platform BEFORE the write — **on both paths**, whenever a `cookies.txt` already exists — and then applies one of two policies.

The MOUNTED-PROFILE import path uses `platformsToRestore` (`internal/cookies/autocookies_profile.go`), which hands back the rows of any platform that either verified before and failed after (a regression) or had credentials before and could not be checked after (inconclusive — committing a set nobody could evaluate over one that may be fine is a bet with no upside). The BROWSER path uses `platformsToRestoreOnRegression`, the **regression arm only**: a browser refresh has just re-fetched from the live site, so a check that then could not reach the network is evidence about the network, and restoring on it would discard a fresher set on every DNS blip — on the path a desktop install runs every thirty minutes. The operator's pasted import shares that second policy for the same reason plus one of its own (see § Auto-Cookie Service). Both share `regressedAfterWrite`, so the arm they agree on cannot drift. A platform that was already dead is deliberately not restored on any path. The regression arm applies to the browser path too: that its cookies "cannot be staler than what was on disk" is true of their age and says nothing about whether they authenticate.

**`StartProfileSeed(ctx)`** (`internal/cookies/autocookies_periodic.go`) runs AT MOST ONE browser-free import shortly after start, and `startCookieSequence` (`cmd/moombox/cookie_startup.go`) calls it **unconditionally**, after the first auth check has finished — it is not under the flag. The flag owns a *repeating* read of a profile nothing changes between ticks; a boot is the one moment a mounted profile plausibly did change, because something replaced it while the process was down. Its safety condition is the cookie file, not the flag, and lives in `decideStartupSeed`. It returns immediately; the import runs on its own goroutine after `profileImportStartupDelay` (15 s) and **re-asks** `decideStartupSeed` after the wait, because an interactive setup finishing or a hand-dropped `cookies.txt` both write the file the first decision was made about. `shouldSeedFromProfileAtStartup` is deleted.

**The timer and the seed are the service's own goroutines, and `Stop` joins them.** `StartPeriodicRefresh` and `StartProfileSeed` both register through `startBackground` (`internal/cookies/autocookies_periodic.go`): each goroutine runs under its caller's context narrowed to the service's lifetime, and `Stop` ends that lifetime — cancelling a pass in flight — and then waits up to `AutoCookieStopWait` (2 s) for both to return, logging `auto-cookie refresh pass still running after stop` if one does not. Before, `Stop` latched and killed browsers but waited for neither, so a pass in flight went on writing `cookies.txt` and `cookies.txt.meta.json` after shutdown — which stops the refresh service just ahead of this one — had moved on to the PO token provider, the web server and the database; a test that stopped at the same point had testing remove its TempDir under the meta write, which Windows refuses (`The directory is not empty`). The pass includes its post-pass re-check: `OnPassCompleted` takes the goroutine's context (`notePassCompleted` hands it on) and `cmd/moombox` runs `recheckAfterCookieWrite` under it — the one re-check site that does not use `context.Background()` — so the cancel that ends the loop ends the re-check's guide and validate round trips too, and the join covers it.

Under `context.Background()` a re-check in flight when the shutdown began outlived the 2-second wait and rewrote `cookies.txt` afterwards. A check cut short by the cancel is inconclusive (`RefreshUnknown`), so a shutdown fires no recovery and moves no baseline. A pass that returns after its goroutine's context ended fires no `OnPassCompleted` at all: a re-check under an ended context could only fail both round trips, and the next boot's first check reads the file the pass wrote. The 2-second bound still holds against a step that ignores the cancel: `Stop` logs and returns, and that step can go on writing. Once the service is stopped neither entry point starts anything. Tests start the loop through `startJoinedPeriodicRefresh`, which joins it before the test's TempDirs are removed.

**Docker guidance.** Two browser-free paths, and they answer different questions. To REFRESH from a mounted profile: leave `auto_enabled` off, update the profile on the host, then press `R F` (or shift+click the header button, or the Settings-page twin). That is the designated workflow on a headless host: no browser is launched, the profile is imported directly, and the manual triggers are exempt from `automaticImportGuard` precisely so it works over an existing file. `cookies.txt` must live *inside* a mounted directory rather than being bind-mounted as an individual file — `writeFileAtomic`'s rename cannot replace a single-file mount. To RE-AUTHENTICATE when the profile itself is stale or there is none: paste or upload a fresh Netscape export in Settings → Cookies, which needs no volume access at all and is the only path that works from a phone against a tunnelled instance.

**The `lastError` write policy.** `lastError` (`AutoCookieService`, `internal/cookies/autocookies.go`) is the last thing a cookie pass concluded that the OPERATOR has to act on, published as `AutoCookieStatus.LastError`. One rule: **a write is allowed only where THIS pass established the thing it is asserting.** Setting asserts a problem; CLEARING asserts that whatever was recorded is not wrong any more, and that is the half that keeps being written by paths with no basis for it.

- `setError` is the single SET funnel and the only place a message enters the field. Every exit that returns an error from a cookie pass sets — `FinishSetup`'s empty-profile, read-failure, merge-abort, mkdir, write and jar-load exits, and the refresh's import-failure, merge-abort, credential-loss and verification-failure exits.
- **Two exits deliberately do NOT set**: the guard clauses at the top of `FinishSetupDetailed` (`ErrNoSetupInProgress`, `ErrSetupCancelled`). No pass has run when they fire, so there is no failure to see afterwards, and the caller gets the answer synchronously in the same dialog. "A pass" is the boundary; a guard that refuses to start one is not an exit from one.
- Three CLEARS, each earned: `StartSetup`'s slot claim (a new attempt is starting, so the recorded message belongs to an attempt that is over — the one clear about intent rather than evidence); `RefreshCookiesDetailed`'s `case renewed` in the any-platform-verified arm (the pass actually produced the credentials it verified); and that switch's "nothing to verify" branch, kept with a note because no route to it has been found and "I could not find a route" is not "there is none". The `default` beside `renewed` deliberately does not clear — a pass whose browser did nothing has established that the credentials on disk work, not that the refresh mechanism does.
- The loss branch of the same arm writes `s.lastError` directly, because a partial success still has to report the platform that was lost.
- **`cleanup()` / `cleanupLocked()` MUST NOT clear it.** `cleanup` runs on every setup exit path including the failed ones — `FinishSetup` calls `setError` and then `cleanup` on each failure exit — so clearing there would erase the message microseconds after it was written.

It has two readers: the Web settings panel's `lastError` line and the TUI's `R C` result line (`Last cookie error:`).

**`fetchedNoCredential`** (`RefreshCookiesDetailed`, `internal/cookies/autocookies_refresh.go`) is a NEW flag, never a redefinition of `fetchedRows`: rows came back and **not one of them is a session credential** — a signed-out browser profile, or one set to clear cookies on exit and re-seeded with `YSC`/`VISITOR_INFO1_LIVE` by the navigation this pass just made. Read as either neighbouring case it would be wrong: "the browser profile contained no cookies" is false, and "auth verification failed — manual re-login required" says nothing an operator can act on. It is measured on what THIS PASS fetched, before the merge folds the previous `cookies.txt` in, and it is computed with `netscapeCookiesHoldACredential` — which loads the text into a THROWAWAY jar and asks the jar's own loose predicates, so "is this a credential" has one answer across the package. Overloading `fetchedRows == 0` was rejected because that counter's deliberate over-counting is load-bearing for the import guard.

**`GetStatus()` versus `ReloginStatus()`.** Both take `s.mu` and both call `reapAbandonedSetupLocked`; `ReloginStatus` returns only the cloned `needsRelogin` map and skips `GetStatus`'s browser/registry detection (~155 ms measured 2026-08-25). Four of `GetStatus`'s five production callers read nothing else, so they were paying that on every poll. Status polling is the most frequent visitor to this lock, which makes it the reap that actually fires in practice — so `ReloginStatus` **must** keep calling the reap. A "simplification" that drops it because the method "only reads `needsRelogin`" would silently stop the abandoned-setup reap from firing in production, with no test noticing.

**Browser detection is cached, both halves.** `DetectBrowser()` (the single best pick) and `DetectBrowsers()` (the full list) share one mutex and one 60-second TTL in `browserDetectCache` (`internal/cookies/autocookies_detect.go`). `DetectBrowsers` is cached, so a status poll does not rebuild the list from a filesystem+registry scan (a `reg.exe` spawn on Windows) it would almost always throw away. Both return the cache's own backing value — callers must treat them as read-only. `InvalidateBrowserDetection()` clears both, and is called whenever the configured browser changes, since that is exactly when an operator has most likely just installed the browser they are pointing Moombox at.

**Supported browsers** — `knownBrowsers` (`internal/cookies/autocookies_detect.go`), ten entries in search order. Firefox-family first, so the `cookies.sqlite` path is preferred when both kinds are installed; within each family, less-common forks come ahead of mainline so a LibreWolf user is not auto-detected as Firefox.

| Browser | Engine | Type key |
|---------|--------|----------|
| LibreWolf | Gecko | `librewolf` |
| Zen Browser | Gecko | `zen` |
| Waterfox | Gecko | `waterfox` |
| Firefox | Gecko | `firefox` |
| Vivaldi | Chromium | `vivaldi` |
| Thorium | Chromium | `thorium` |
| Brave | Chromium | `brave` |
| Google Chrome | Chromium | `chrome` |
| Opera GX | Chromium | `opera` |
| Microsoft Edge | Chromium | `edge` |

Detection is `exec.LookPath` over each entry's candidate names plus, on Windows, `PROGRAMFILES` / `PROGRAMFILES(X86)` / `LOCALAPPDATA` install paths, deduped by absolute path. The system default browser is promoted to the front of the order when it can be read, **except Edge**, which frequently hijacks the Windows registry default.

**Launch args by engine:**

| Engine | Interactive setup | Headless refresh |
|--------|-------------------|------------------|
| Firefox/Gecko | `--new-instance --profile <DIR> <LOGIN_URL>`; a `user.js` written first suppresses first-run dialogs and explicitly disables telemetry upload | `--new-instance --screenshot <tmp> --profile <DIR> <URL>`, one launch per platform, `firefoxLaunchSpacing` (5 s) apart |
| Chromium | `--user-data-dir=<DIR> --no-first-run --no-default-browser-check --disable-blink-features=AutomationControlled --remote-debugging-port=<port> <LOGIN_URL>` | the same plus `--headless=new`, `--disable-gpu`, `--disable-session-crashed-bubble`, `--disable-features=InfiniteSessionRestore`, `--window-size=1280,720`, and **no URL** — navigation happens over CDP |

The anti-automation flags are mirrored on the headless launch deliberately: YouTube raises fraud scores for a browser that advertises itself as automated, which would invalidate the very cookies the pass is refreshing. `dangerousProfilePathSubstrings` (`internal/cookies/autocookies_browser_resolve.go`) refuses a configured profile directory that belongs to a real installed browser, so a hostile config cannot launch Chrome against the user's actual signed-in profile and exfiltrate it through the `cookies.txt` export. The check is `validateBrowserProfileDirForLaunch`, computed once at construction, and its name carries its scope: it holds on all four subprocess sites (`startFirefoxSetup`, `refreshFirefox`, `startChromiumSetup`, `refreshChromium`) in every acquisition mode. The two READ-ONLY sites — `importProfileCookies` and `decideStartupSeed` — go through `readOnlyProfileDirErr` instead, which honours it by default and lifts it when `cookies.acquisition = "profile"`, and refuses with `ErrProfileDirNotOptedIn` rather than a sentence about a launch that cannot happen on that path.

The list covers Windows, Linux (the dotfile, snap and flatpak trees) and macOS: 18 Windows shapes, 16 Linux `~/.mozilla` / `~/.config` dotfile shapes (snap's `~/snap/firefox/common` tree matches through the same `~/.mozilla` path, while snap Chromium — Ubuntu's default since 20.04 — keeps its profile at `~/snap/chromium/common/chromium` with no `.config` anywhere and carries its own entry), five flatpak `~/.var/app` sandboxes and seven macOS `~/Library/Application Support` shapes — 46 entries, every one written with `/` separators and matched case-insensitively against the absolute path with `\` normalised to `/`. The Linux desktop case it worried about (audit G3) is answered by the read boundary rather than by a narrow list: `cookies.acquisition = "profile"` lifts the guard for the two READ-ONLY sites, while launching a browser against a real profile stays refused on every OS.

**Cookie extraction:**

| Engine | Source | Method |
|--------|--------|--------|
| Firefox | `cookies.sqlite` in the profile dir | `readFirefoxCookies` (`internal/cookies/autocookies_firefox.go`) SNAPSHOTS the DB **together with its `-wal` sidecar** into a temp dir and queries the copy … |
| Chromium | the live browser over CDP | `cdpGetCookiesAsNetscape` (`internal/cookies/autocookies_chromium.go`) runs a three-tier ladder: browser-level `Storage.getCookies`, then per-page `Network.getAllCookies`, then per-page `Network.getCookies`. The gate between tiers is the RELEVANT row count, not the raw one, so a tier-1 answer full of other sites' cookies does not stop the ladder |
| Chromium (opt-in fallback) | the user's REAL profile under `%LOCALAPPDATA%`, decrypted with `CryptUnprotectData` | `dpapiExtractAsNetscape` (`internal/cookies/autocookies_dpapi.go`). |

#### Firefox

`readFirefoxCookies` (`internal/cookies/autocookies_firefox.go`) SNAPSHOTS the DB **together with its `-wal` sidecar** into a temp dir and queries the copy; copying without the sidecar silently returns rows that are missing every uncommitted write. Falls back to querying in place if the snapshot itself fails, and retries up to 5 times at 500 ms for WAL lock contention and torn snapshots, breaking early on anything non-retryable. NULL columns are defaulted and unusable rows counted, both reported by the caller. Only the default browsing context is read (`firefoxCookieContext`, from `originAttributes`): a Multi-Account Container's rows (`userContextId`) are another account's session, and a Total Cookie Protection partition's (`partitionKey`) are cookies an embedded player set inside another site; read with the rest, the last row of a name won and mixed them into the default session. First-party isolation's `firstPartyDomain` rows are kept

#### Chromium (opt-in fallback)

`dpapiExtractAsNetscape` (`internal/cookies/autocookies_dpapi.go`). A row Chrome stored unencrypted is read from its plain `value` column first (as yt-dlp does); reading only `encrypted_value` turned it into an empty cookie. Reached only when the browser refresh already returned an error, `cookies.dpapi_fallback` is on, and the resolved browser is non-Firefox — DPAPI launches nothing, so it sidesteps "Chromium is already running our profile" entirely. Off by default: it reads the user's actual signed-in cookies, so it is a privacy surface they have to enable consciously

An empty result and an unanswered read are different facts. `cdpCookieReadOutcome` distinguishes them: `ErrBrowserLadderBlocked` when a structural failure (the `/json` target listing) stopped the fallbacks from running at all, `ErrBrowserReadUnanswered` when no query answered. Neither is `IsNoBrowserProfile`, so neither triggers rung 3 — both come from a pass that RAN, and a plain recheck cannot fix either. `writeBrowserReadError` (`internal/web/routes/cookies.go`) maps the first to **409** and the second to **502**, each with a machine-readable `cause`, and passes the composed message through verbatim.

**The DPAPI fallback reads exactly ONE profile.** It does not merge every profile `dpapi.FindBrowserProfiles` returns before dedup: with two signed-in Chromium profiles `deduplicateAndFormat`'s bare-name dedup would let whichever profile was listed LAST silently win each cookie name — an order-dependent coin flip that could pair a SAPISID from profile A with a LOGIN_INFO from profile B. Selection: filter by the configured browser type (empty **and** `"chrome"` both mean "every profile is a candidate" — the Web UI's only Chromium option is literally the whole family; a type with no layout in `dpapi.KnownBrowserFamilies()` also falls back to every profile, logged at Debug, because that is a coverage gap rather than a missing browser), score each candidate with `dpapiProfileScore` using the same loose/strict predicates `jar.go` keeps, take the highest, break ties by scan order with an Info line naming every tied profile, and discard the rest whole. Known limitation, deliberately not built and ruled NEVER by the owner (2026-08-29: one profile is the design — the managed setup profile signs into both platforms — so a two-pass per-platform selection is not to be re-proposed): the score sums both platforms into one number, so YouTube-on-profile-A / Twitch-on-profile-B still loses one platform — deterministically and logged, instead of silently.

**One question about this path is unmeasured**, and it is written out in `internal/cookies/dpapi/dpapi_windows.go`: whether a `mode=ro` read of a live WAL-mode Chromium `Cookies` DB returns stale rows — that is, whether `modernc.org/sqlite`'s pure-Go reader merges committed `-wal` frames the way the C library's WAL reader protocol does. Lock conflicts are already handled and degrade to a clean error; staleness would not, because a read that misses the `-wal` returns whatever was last checkpointed into the main file — well-formed rows that nothing errors on. Settling it needs a signed-in browser writing to the DB on the same machine. Until that runs, the Firefox path's copy-the-sidecar shape is deliberately not applied here.

**CDP readiness** (`waitForCDP`, `internal/cookies/autocookies_chromium.go`) polls `http://127.0.0.1:<port>/json/version` with exponential backoff — 200 ms doubling to a 2 s cap — until `cdpPollTimeout` (15 s). It waits for the debugging endpoint to come up; it does **not** poll for login completion. Interactive login completion is the operator pressing "I'm Logged In", which calls `FinishSetup`. Other CDP budgets: `cdpExtractTimeout` 30 s, `cdpRefreshTimeout` 60 s (cold start plus sequential per-platform navigations plus extraction, all sharing it), `cdpNavigateTimeout` 30 s per `Page.navigate` + `loadEventFired` wait. A navigation whose read loop hits its deadline without ever seeing `Page.loadEventFired` returns `errNavigateBudgetExhausted`. `navigateAllPlatforms` treats that per platform as NOT OBSERVED rather than as a failure — it never joins `navFailures` — and lets it flip the pass's "navigated" answer only when it is **every** platform's outcome. One platform timing out beside a sibling that fired its load event is a slow page on a browser that demonstrably works; do not loosen "every" to "any".

**Lock file cleanup.** Before launching, the service removes stale lock files that would otherwise prevent a launch:

- Chromium (`cleanChromiumLockFiles`): `SingletonLock` FIRST, then `lockfile`, `SingletonSocket`, `SingletonCookie`, plus the `Singleton*` and `*lockfile*` globs for variants newer builds leave behind. It stops at the first lock a live browser may hold and returns that lock's `ErrProfileInUse`, and both Chromium launch sites — the headless refresh and the interactive setup — start nothing on that answer.
- Firefox (`cleanFirefoxLockFiles`): `parent.lock`, `.parentlock`.

Both go through `removeStaleLock` (`internal/cookies/autocookies_chromium.go`), which applies one of two rules by the lock's shape:

- **A symlink whose target reads `<hostname>-<pid>`** is Chromium's POSIX `SingletonLock`, judged by the holder it names (`singletonLockHolder`). The target is a name, never a file, so the age rule cannot read it — `os.Stat` follows the link and always fails — and before this rule that failure read as "not present" and the lock was deleted **under a live browser every time**. Exactly one answer deletes: this machine's hostname (`os.Hostname`) AND a pid that no longer answers (`pidRunning`: signal 0 on POSIX — only `ESRCH` means gone, `EPERM` is someone else's live process — and `OpenProcess` through `os.FindProcess` on Windows). Another host's lock is **never** deleted, whatever its pid: that pid cannot be signalled from here, so a live browser and a dead one look the same — the host's browser seen from a container through a mounted profile is the case — and the launch is skipped instead. A hostname this machine cannot read is treated the same way. A pid that answers is kept even though it may have been reused: a reused pid costs a skipped pass whose sentence names it, never a lock broken under a live browser. The target is split at its LAST hyphen (hostnames carry them) and the pid must be a positive 32-bit decimal; a target that does not parse falls to the age rule, as Chromium itself treats an unparseable lock as invalid.
- **Anything else keeps the age rule** and **skips any file touched within `lockFileFreshThreshold` (5 s)** — a truly stale lock from a crashed run is older than that, while one held by a live browser is not. That covers every Windows lock (`lockfile` is a plain file there and Windows has no symlink lock, so its behaviour is unchanged), both Firefox locks, and the two symlinks whose targets name no holder — `SingletonSocket` (the live socket's path) and `SingletonCookie` (a random number). Those two are why `SingletonLock` is judged first: the age rule would unlink them under the very browser the lock has just said is running.

**A held profile is a skip that says why, not a failure.** `RefreshCookiesDetailed` answers `ErrProfileInUse` with a DECLINED result — `Ran` false, so no auth re-check is owed for a file nobody wrote, and `Mechanism` `"browser"` because that branch was chosen — and records the sentence with `setError`, so `AutoCookieStatus.LastError`, which both UIs render as the last cookie error (the TUI's `R C` line wraps so the path is drawn whole, and so does `R F`'s line, which is the same sentence), says `browser profile in use by <host> …` and names the lock by its full path (the one Moombox sees — inside a container, the mounted profile's) with the way out only the operator can check: another machine's lock reads `close it there, or delete "<path>" if no browser on <host> is using that profile`; a live local pid's reads `or delete "<path>" if that pid is no longer one`; an unreadable hostname's ends `delete "<path>" if no browser on <host> is using that profile`. A lock whose browser crashed on another machine is never cleared by any pass, so without the path the profile would stay skipped with nothing on screen saying which file was in the way.

Moombox itself still never deletes it on any of these answers. It is checked ahead of the DPAPI fallback, which answers a launch that failed; this one was never attempted. `StartSetup` returns it unchanged and records it the same way: its slot claim clears `LastError` before the launcher has judged the lock, so a refusal that only returned the sentence would leave the status blank while the profile was still held and every later refresh still skipped — and with no active jobs no refresh runs to put it back. Both routes map it to 409 with the sentence verbatim (see [user-interfaces.md](user-interfaces.md)). Every surface that reports the pass calls it a skip: `R F` and the dashboard's refresh toast show the sentence alone in the warning colour (the refresh route's `cause` `profile-in-use` is what lets the toast tell it from a locked cookie DB), the periodic tick and a job-triggered refresh log `… skipped — a browser holds the profile` at Warn with the sentence as the reason — the job-triggered one answers the worker `CookieRefreshSkipped`, so the worker's next line says the job stays parked for a refresh that can run or a replaced cookie file, not `auto cookie refresh failed — the cookie file has to be replaced by hand` — and a recovery sends `Cookie Auto-Refresh Skipped` (see Credential Notifications in [operations.md](operations.md)). None of them calls it a failure — red, `failed`, `error`, or a notification telling the operator to replace cookies nothing had rejected.

**Orphaned temp files and directory permissions.** `writeFileAtomic` writes `<base>.<random>.tmp` beside the target and renames; a crash, a kill or a panic in between leaves a full copy of `cookies.txt` — the highest-value secret in the app — under a name nothing reads. `sweepStaleCookieTempFiles` (`internal/cookies/cookie_files.go`) removes those, ONCE per process (`cookieTempFileSweepOnce`, wired at `NewAutoCookieService` because that is the one place holding the real cookie path up front), matching only `<base>.*.tmp` in the cookie file's own directory and only when older than `cookieTempFileMaxAge` (1 hour). Age is the only guard against sweeping a write in progress. Every failure is Debug-logged with the path only and left for the next start.

`tightenCookieDirOnce` applies `utils.ApplyUserOnlyDACL` to the cookie file's parent — `icacls` on Windows, a real `chmod` to 0700/0600 on Linux (not a no-op there). It is **memoised on SUCCESS, not on attempt**, with three states per directory (absent = not started, `dirTighteningInFlight`, `dirTighteningDone`): a transient failure or a panic mid-apply deletes the entry so the NEXT cookie write retries, rather than disabling hardening for the rest of the process because the failure was demoted to a Debug line. Cost if it fails permanently on a host: one extra ~30-80 ms shell-out per cookie write instead of one per process, bounded by the write cadence. No failure cap and no backoff — either would be a mechanism to contain a mechanism.

**On POSIX that directory chmod applies only to a DEDICATED directory** (owner decision O-K, 2026-09-17). Both writers — `tightenCookieDirOnce` and the twin block in `config.Save` — ask `utils.DirTighteningAllowed` (`internal/utils/dedicateddir.go`) first. A refusal is **silent on the cookie side** — `tightenCookieDirOnce` has no logger and is not being given one for this — and leaves a Debug line in `config.Save`, which does. The answer is always yes on Windows, where `icacls` writes inheritable ACEs onto the directory and its children rather than removing traversal from everyone else's world: O-K leaves the Windows path byte-identical. On POSIX it is a CONTENT test over the directory's own entries (`DirHoldsSharedData`) — an entry named `output`, `staging` or `logs` — tested by NAME before its type, so a dangling symlink at an unmounted bind target counts too — or a file whose extension is `.db`, `.sqlite`, `.sqlite3` or `.log` after SQLite's `-wal`/`-shm` sidecar suffix is stripped, marks the directory as Moombox's DATA directory rather than a dedicated secrets one.

A listing that reaches 512 entries disqualifies the directory outright rather than being truncated. Content rather than config because the rule has to answer the same way in `internal/config` (which cannot import `internal/cookies`), in `internal/cookies` (which does not import `internal/config`) and in the `moombox add` side process, which wires no config store at all. Three boundary answers are deliberate: an empty directory is dedicated (that is the first-write case), a directory holding only `cookies.txt` and its own family is dedicated, and a directory that cannot be listed is treated as shared — a hardening that cannot see what it is tightening declines. `config.toml` is deliberately NOT a shared-data marker: it is a secret of the same family as `cookies.txt`, so counting it would mean no config directory anywhere was ever tightened.

The motivating case is the image: `cookies.txt` and `config.toml` both live in `/data`, the bind mount that also holds `output/`, `staging/`, the database and the log, so a first cookie write or settings save that chmodded it 0700 would undo the Dockerfile's deliberate `chmod 777 /data` and take the operator's archives away from their own host user (a compose-default `./data` auto-created as root stays host-readable). Files are unaffected on every path: `writeFileAtomic` chmods its temp file to 0600 before the rename and `SaveMeta` writes 0600, so what a shared directory gives up is only the untraversable parent.

**First-run platform detection is sidecar-first.** `detectCookiePlatforms(meta, jar)` (`cmd/moombox/services.go`) decides `cfg.Cookies.Platforms` when the config has none. `cookies.meta.json`'s `Platforms` — the on-disk record of what an import ACTUALLY verified — wins outright whenever non-empty, and is never unioned with a guess. Only when the sidecar is absent, unreadable or empty does it fall through to the jar, and then to the **loose** predicates (`HasAnyYouTubeAuthCookie` / `HasAnyTwitchAuthCookie`), not the strict pair: a `cookies.txt` holding SAPISID with LOGIN_INFO cleared is a configured platform with broken credentials, and persisting "unconfigured" for it made every downstream gate that reads `Platforms` — the auth-loss notification, the `SetExpectedPlatforms` seeding, the recovery path — treat it as never having existed, permanently, since nothing automatic re-runs this once `Platforms` is non-empty.

**Platform URLs:**

| Platform | Login URL (`StartSetup`) | Refresh URL (`platformRefreshURLs`) |
|----------|--------------------------|-------------------------------------|
| YouTube | `accounts.google.com/ServiceLogin?service=youtube` | `www.youtube.com` |
| Twitch | `www.twitch.tv/login` | `www.twitch.tv` |

**Budgets:** `processTimeout` 30 s per browser launch, `authVerifyTimeout` 12 s for ONE verification window PER PLATFORM, the platforms verified concurrently so a whole `checkPlatformAuth` call still costs ~12 s (it was 15 s shared by both; the split was paid for out of the window and by the concurrency, so no outer budget had to move), `refreshOverallBudget` 2 minutes end to end — which a refresh pass spends against **two** `checkPlatformAuth` calls, the pre-write snapshot and the post-write verify, and three when it rolls back (≈101 s and ≈113 s). These are coupled — raising `processTimeout` without raising `refreshOverallBudget` makes the outer context cancel the second platform's launch mid-flight instead of granting it the budget it was just given.

---

## File Output

### Staging Directory

In-progress downloads write to the staging directory (default: `./staging`). Nothing is written to the output directory until the mux: FFmpeg muxes from staging straight into it, and staging is cleaned up only after that succeeds. This prevents incomplete files from appearing in the final output location.

**Display aspect.** A YouTube video whose `videoDetails.keywords` carry `yt:stretch=W:H` — old anamorphic uploads, 4:3 frames meant for 16:9 display — has the ratio recorded in its staging root as `display-aspect.txt` (`recordDisplayAspect`, `internal/worker/display_aspect.go`) when its download is set up, and every stream copy of a root recording reads it (`stagedDisplayAspect`, from `copyStaged`) and passes it to FFmpeg as `-aspect` (`Muxer.MuxCopyArchive`, the one archive copy that also takes the chapter list — see Chapters; `Muxer.MuxCopyAspect` is the aspect alone): the frames are copied untouched and the archive displays at the asked aspect, as yt-dlp's default fixup leaves it (`FFmpegFixupStretchedPP`, `postprocessor/ffmpeg.py`, the ratio as `%f`). A file, not a column: the boot re-mux and the Mux action read staging, and no migration is spent on a keyword only pre-2012 uploads carry. It is written only for such a video — a ratio of 1, or none, clears it — so every other job's staging is unchanged. Because it is written before the first request, it is not a recording: `HasStagingFiles` (`internal/worker/staging.go`) ignores it, so a stretched video whose run failed or was cancelled before any media byte does not read as preserved staging — no "Resume available", no Resume or delete-staging offer, no footage counted at channel removal.

**Damaged fragments.** A mux that leaves part of a staged recording out of its output — a damaged fragment's unreadable bytes, which it muxes around from a repaired copy — or that nothing independent of FFmpeg could check (a fragmented MP4 the walk cannot read, or whose decode times run backwards) writes `<recording>.mux-gaps.json` beside the recording before FFmpeg runs (`muxGapRecord`, `internal/worker/mux_repair.go`): the recording's `size` as the mux read it, the `output` it wrote, each `dropped` stretch as its byte range and per-track time range, `droppedBytes`, `gapSeconds` (a drop ahead of a track's first readable fragment counts as the lead a two-input copy restores for it, 0 where there is none to restore), an `unmeasured` reason, `heldWith` for the whole half of a set-aside restart kept beside a damaged one, and `at`. The record is the staging shield (`stagedMuxGaps`, in the root and every `seg_N` dir, tombstoned ones included) for as long as its recording is there at that size: one whose recording a resume has since appended to, or that went, shields nothing, and a clean mux of the same file removes it.

It moves with its recording when Moombox moves one — part 0's capture into `seg_0` for a split job's from-the-start download (`claimStagingRootForVod`, the record first, so a crash between the two leaves the recording in the root for the next claim to set aside, never a moved recording without its record), a recording set aside (`setAsideStagedMedia`); the engine's own set-aside at a restart leaves it behind, beside a fresh file that will not have its size, and the aside's mux walks it and records it again. The shield has no age rule, a part merge does not delete a superseded part's dir that holds one, and the Download Finished notification carries it as a Gaps field. The repaired copy the mux reads is `<recording>.moombox-repair`, beside it; it is removed when the copy is done. One left behind — a mux killed by a restart, an update or a crash, or a removal that failed — is removed by the next repair of the same file before it counts the free space (credited, when it will not go, as the bytes that repair truncates), by the job's staging cleanup after its copies, and by the boot sweep. Neither is a name any staging scan takes for a recording.

Nothing about a mux gap is stored in the database: the job's own log carries the Warn lines, and the record carries the rest — `GET /api/jobs/{id}` names each record that holds as `muxGaps` (`worker.ScanMuxGaps`), which both UIs' job details show, so the shield still says why once the in-memory job log is gone after a restart.

One job's staging dir holds ONE current recording in its root: the whole-file VOD download sets an earlier live-shape capture aside before it writes (`setAsideLiveShapesForVod`, `internal/worker/staging_shapes.go`), a capture whose setup changed shape inside its part — DASH to HLS or back, which the live HLS fallback does when the player's JS challenges cannot be solved or a GVS token cannot be minted — sets the other shape's staged media aside the same way (`setAsideOtherLiveShape`), and a VOD run on a job that already split into parts moves part 0's capture into `seg_0` and marks the root as the from-the-start recording's (`vodRootMarkerFile`, `internal/worker/vod_supersede.go`) — `downloading` while it runs, `complete` once nothing is missing from it, which is when it replaces the parts as the archive.

**Boot leftovers.** Each start, the worker deletes the staging leftovers a previous run left behind that are PROVABLY redundant, on its own goroutine so the queue never waits on it (`reclaimBootLeftovers`, `internal/worker/boot_leftovers.go`), and logs each deletion with its path and reason. Three kinds qualify. A repaired copy of a staged recording (`<recording>.moombox-repair`) a mux did not remove — the process stopped mid-mux, or the removal failed — goes, in the root and every `seg_N` dir of a job whose row is not active, under the job's claim; it is derived from the recording beside it, which a repair never touches (`removeStaleRepairedCopies`). A set-aside recording carrying a recovered marker (`asideRecoveredMarker`) whose muxed sibling — the path the marker holds — is on disk as a non-empty file goes, with its resume twin and the marker, in the root and every `seg_N` dir, whatever the job's row says (`removeVerifiedRecoveredAsides`); a marker whose sibling has since been moved or deleted keeps its aside, which is then the only copy, and so does one whose removal fails.

And the whole staging dir of a `Finished` job goes when the row records a download of its own (`download_started_at`, stamped as a run starts and cleared only by a Reinitialize that deletes staging with it — an imported row has none, and the staging under its video ID can be an unmuxed capture a deleted job of the same ID left behind), every archive file its row names is on disk (`output_file` and each part's file, `missingArchiveFile`), no aside marked recovered is left in it — the post-mux cleanup does not count a marked aside, since straight after a finalize its sibling has just been written, so one the sweep kept would otherwise go with the dir — and the post-mux cleanup's own decision says remove: `decideStagingCleanup` (`internal/worker/worker.go`) is the decision `cleanupStagingAfterMux` acts on, so the boot sweep applies exactly its shields — set-aside recordings, unmuxed parts, an unused root recording, a recording a mux could not carry whole, an incomplete tail, an incomplete chat capture — and never a looser copy; a chat-incomplete dir is not even pruned. That covers a finalize whose cleanup never ran or did not finish: the process died between the Finished write and the delete, or Windows held a handle.

An active job — skipped BEFORE the per-job claim is taken, and checked again under it: the sweep runs beside `enqueueExistingJobs`, whose restart mux of a `Muxing` row takes the same claim and, refused, falls back to resetting the row to `Downloading` — a job whose staging an off-queue operation holds (the per-job claim, which the sweep takes as `opBootCleanup`, and the output claims), and anything not provably redundant — no row, an unreadable row, a row that never downloaded, a missing archive, a kept shield — are left exactly as they are for the orphan sweep and its confirm-before-delete.

**Tail flags from before the end-of-stream margin.** Ahead of that sweep, on the same goroutine, `clearToleratedTails` (`internal/worker/tail_tolerance.go`; owner ruling R-T1) walks the job rows — not the staging directory, since a flagged row whose staging is already gone still shows the badge — for `Finished` YouTube live captures flagged `incomplete_tail` whose saved progress line `(V: a/b A: c/d …)` shows both streams at the same segment under the same head, exactly 2 short of it (`toleratedTailRow`: `a == c`, `b == d`, `b-a == engine.EndOfStreamHeadMargin`). The ruling's text is `1 <= b-a <= 2`; a row 1 short is left flagged, because `a` is the last segment written, so that downloader stood at its head, which the old rule never flagged — interruption evidence flagged it, and its staging was kept for a resume. An `is_vod` row is not read this way: a VOD download's incomplete line counts the next segment, not the last written.

The pass runs once. A saved row cannot say why it was flagged, and the rule itself still flags a capture 2 short of its head when an interruption, a wait for a resume or an unconfirmed end says so, leaving the same progress line; a pass that re-read the rows on every start would clear exactly those and delete the staging and resume state kept for them. So the first start of a version that has the rule lists the matching rows into `end-of-stream-tails.json` beside the database (`tailPassDoc`, written atomically: job ID to the row's `updated_at`) before it touches anything, and removes each as it is reached; a start that finds the file works only on what is still listed there, which is non-empty only after a process that died mid-pass, and clears such a row only while its `updated_at` is unchanged. A file that cannot be read or written leaves every row as it is. Deleting the file makes the next start list again.

For each listed row it takes the job's staging claim (`opTailClear`), reads the row again under it, and writes `incomplete_tail` false, `progress` empty and `percent` 100 with `UpdateJobFieldsIf(…, Finished, …)`. Only then is the staging put through `cleanupStagingAfterMux`, the cleanup a complete job gets after its mux: removed unless a set-aside recording, an unmuxed part, an unused root recording or a mux-gap record keeps it, and pruned to the chat capture when chat ended incomplete; one of the boot sweep's guards comes first, a set-aside recording marked recovered that cannot be proved redundant, which leaves the directory for the orphan sweep. A row is left exactly as it was — flag, progress line, staging and Resume — and logged with the reason, when another operation or a finalize holds its staging, when it changed since it was listed, or when its archive is not where the row says (the staging may then be the only copy; clearing the flag alone would leave it an orphan beside a row that no longer offers Resume). Such a row is not tried again. A Resume accepted while the pass holds the claim waits for it and then reads its gate again (`ResumeJob`), so it does not re-run the job the pass has just made complete. One Info line per cleared job, in the job's own log, names the segment, the head and what became of the staging (`TestBootClearsToleratedTails`, `TestBootTailPassIsOneShot`, `TestBootTailPassFinishesAnInterruptedPass`).

### Output Template

The output template (default: `${channel}/${start_date} ${title} [${id}]`) is resolved at download time using `config.ResolveTemplate()`. The file extension (`.mkv`, `.mp4`, etc.) is appended by the muxer.

Per-channel output directories override the global `paths.output_directory` when configured.

**Which directory a job writes to.** A change of `paths.output_directory`, or of a channel's override, reaches every job that has not started, and no job that has (owner ruling R2-X1). The monitors store a directory on each row they create, and a scheduled stream or a backlog VOD can wait a long time after that; so when the worker builds the run's context at the start of the download (`buildJobContext`, `internal/worker/worker.go`), a monitor-detected row that is still `Upcoming`, `Queued` or `Live`, with no archive and no part, takes the override of its channel as configured now (matched by `channel_id`, a Twitch live row by the login in its URL), else the global directory, and the row is rewritten with it, only while it is still waiting (`settleOutputDirectory`, `internal/worker/output_dir.go`). A channel that is no longer configured has no override, so its kept backlog goes to the global directory. From then on the row's `output_directory` is where its files are, which the mux, a Resume, the file routes and the orphan sweep all read; a Reinitialize returns the row to `Upcoming`, so the new recording follows the settings again, and the archive the reset kept stays where it is. A manually added job is not touched: one whose request named a directory keeps it, and one that named none stores none and reads the global setting at every run, as before. The disk gauge and the backlog's disk gate read the volume of the global directory only.

### Resume State Files

Each active download (video stream and audio stream separately) maintains a resume state sidecar file at `<output_file>.resume.json`.

**ResumeState structure:**

```go
type ResumeState struct {
    LastSeq      int    `json:"lastSeq"`      // Last successfully written segment number
    BytesWritten int64  `json:"bytesWritten"`  // Bytes written to output file
    Timestamp    int64  `json:"timestamp"`     // Unix timestamp of last save
    BaseURL      string `json:"baseUrl"`       // Base URL for segment downloads
    StreamID     string `json:"streamId,omitempty"`    // Broadcast identity (Twitch)
    InitWritten  bool   `json:"initWritten,omitempty"` // fMP4 HLS: init segment at file head
    InitURI      string `json:"initUri,omitempty"`     // fMP4 HLS: #EXT-X-MAP URI the init was adopted under
    InitHash     string `json:"initHash,omitempty"`    // fMP4 HLS: SHA-256 of the written init bytes
}
```

The `Init*` fields let a successor downloader appending to the same staged
file know the file already begins with an fMP4 init segment: an unchanged
`#EXT-X-MAP` URI needs no re-fetch, a rotated URI is re-fetched and compared
by content hash (Twitch token rotation is not a transcode restart), and a
genuinely different init part-splits under `StopOnGap` instead of corrupting
the file with fragments that reference a different `moov`.

**Save frequency:**

- Sequential downloads: every 50 segments (`ResumeSeqInterval`)
- Catch-up downloads: every 10 segments (`ResumeCatchupInterval`)
- Always saved on exit (clean or interrupted)

**Resume validation on load** (`resumeIdentityMismatch` in `engine/downloader_resume.go`):

1. Identity, in precedence order: (a) when both the saved state and the
   current options carry an explicit `StreamID` (Twitch broadcast/VOD id), a
   mismatch discards the state — the YouTube whole-file VOD path sets one
   per stream (video, itag, `clen`; `vodStreamID`,
   `internal/worker/strategy_youtube_vod_wiring.go`), since every rendition
   stages to the same `video.mp4` / `audio.m4a`; (b) otherwise YouTube URL
   fingerprinting (`videoID/itag` from the path or query shape, or a finished
   VOD's `id=o-…` URL read as its itag plus `clen`) — differing or mixed
   fingerprints discard; (c) when NEITHER URL carries an extractable
   identity (Twitch weaver URLs), the state is TRUSTED — raw URL equality
   has no signal there and rejecting on it used to truncate hours of
   recording on every restart.
2. Output file must exist and be at least as large as `BytesWritten`; states
   older than 7 days (`maxResumeStateAge`) are discarded. A whole-file
   download is then held to its file once the probe answers: a total other
   than the recorded `TotalSize`, or a partial longer than the file, means a
   different file, and `runDirectDownload` starts over
   (`internal/engine/downloader_direct.go`). When the probe fails, the
   streaming fallback holds it to the total its resume Range's answer states
   instead, and a 416 short of the recorded `TotalSize` is an error that
   keeps the sidecar, never a completed file; a 206 whose body ends short
   of its stated or the recorded total is asked for the rest.
3. If validation fails, the resume file is discarded — but the staged
   bytes are not. NO caller truncates them. The shared no-truncate guard in
   `Start` (`internal/engine/downloader.go`) runs whenever the engine could
   not resume and the output file is non-empty (whole-file `IsDirectURL`
   downloads excepted — their partial is re-fetchable from the same static
   URL): Twitch live (`StopOnGap`) gets `ErrGapDetected`, so the orchestrator
   muxes the staged data as a finished part and continues fresh; every other
   caller gets `ErrStagedMediaPresent` and the orchestrator decides; and a
   caller that REQUIRES a file beginning at the start of the stream
   (`DiscardStaged`, the manifest-free restart) has the recording renamed to
   `<OutputFile>.restart-<unix ts>` beside the fresh one by
   `preserveStagedRecording` instead — only bytes the engine positively read
   and found unrecognisable are discarded.
4. If the resume file is missing but `StartSeq > 0` in the database, the
   database state is used as a fallback (less precise but allows recovery).

**Lifecycle:**

- Created during download.
- Updated periodically during download.
- Cleared when the stream ends naturally and cleanly — and, on a
  `DiscardStaged` restart, sent after the recording it describes:
  `moveResumeStateAside` (`internal/engine/downloader.go`) renames it beside
  the set-aside file and, only when that rename fails, deletes it outright, so
  a later Start cannot resume the regrown file against the old offsets.
- Preserved on shutdown/cancel so downloads can be resumed on restart.

### Archive Chapters

A single-file archive carries chapters (owner decision D23 and its follow-ups), embedded at mux as an FFMETADATA1 input (`-f ffmetadata -i chapters.ffmeta -map_chapters N`, `Muxer.MuxCopyArchive` from `copyStaged`, which also carries a recorded yt:stretch aspect — `Muxer.MuxCopyAtChapters` is the chapters alone — `internal/engine/muxer.go` — yt-dlp's `--embed-chapters` input, `FFmpegMetadataPP`). `prepareArchiveChapters` (`internal/worker/chapters.go`) chooses the list and fits it to the staged media's span (`utils.SanitizeChapters`: yt-dlp's fill-in — an untitled chapter in front of a late first one, ends from the next start, `<Untitled Chapter N>` — clamped so none runs past the last frame):

- **Twitch VOD**: its moments (`GetVodInfo`), staged as `chapters.json` (`utils.StagedChaptersFile`) in the staging root before the finalize — for a single-part archive and a split one alike (a split job's finalize reads it for its `<name>.chapters.json`, below).
- **Twitch live capture**: its archive VOD's moments when `ArchiveVodFor` finds the VOD at finalize; otherwise the categories the capture observed — the one it started in, then each change a once-a-minute `StreamMetadata` poll saw (`pollTwitchCategory`) — timed from the stream start. Either list is shifted by where the capture began in the broadcast (the root part's first segment's program date-time). The raw material is `twitch_timeline.json` in staging, so the Mux action over a crashed capture still builds the observed list. A split capture is looked up and staged the same way, so its `<name>.chapters.json` has the archive VOD's moments too (`ExecuteTwitch` stages for every finalize, not only a single part's).
- **YouTube VOD** (including a finished stream added as one): the watch page's chapter markers read at the mux, in yt-dlp's order — `chapteredPlayerBarRenderer`, then the engagement panel's `macroMarkersListRenderer` — falling back to the description's timestamps (`ChaptersFromDescription`, yt-dlp's `_extract_chapters_from_description`).
- **YouTube live capture recorded from sq=0** (owner decision FE-18): the same list, asked for at the mux like a VOD's. A capture that began at the broadcast's first segment shares the VOD's timeline, so the chapters line up. Where a recording began is what its downloader reported for the first segment it wrote into the file it opened empty (`OnFirstMedia`), kept in staging beside the file in `recording-start.json` (`recordingStartFile`, `internal/worker/recording_start.go`; resumes and appends leave the entry alone; a downloader that opens the file empty drops the entry an earlier, set-aside recording of that name left, and its own first write records its own), and every input of the single-file mux must have an entry of 0 (`recordedFromStart`); a split capture's `<name>.chapters.json` never has them. A capture that joined late — the live edge, a restart past the head — carries none, and so does one whose start was never recorded (one begun before the record existed): its start is not guessed.

A split job's parts carry none: they are muxed while the broadcast runs, before any list is known.

**Shown in the Web UI player** (owner decision CH). Nothing stores the list: `GET /api/jobs/{id}/chapters` (`internal/web/routes/chapter_routes.go`) reads it back from the archive with ffprobe when the player opens the job — a single-file job's archive, or each part of a multi-part one, a part's chapters shifted by its start offset on the player's global timeline, which is how a W14 gap-filled job shows its original file's chapters among the recovered parts — through a per-file cache checked against size and mtime, so a re-mux is seen on the next open. ffprobe refusing a file is cached with its version and logged once; a probe that never started, timed out (the 10 s bound, for a stalled share) or was abandoned with its request is retried on the next open. A part's chapter ends at its part's end, and one that starts at or past it (a gap-fill row is the VOD's whole-second length) is dropped rather than shown inside the next part. A part that carries chapters in its own file shows them; nothing new is muxed for split parts. When no part carries any, the route answers the split archive's own list beside its parts (below). The player draws them as a marker strip under the video and a collapsible list beside the chat (user-interfaces.md, the `player.js` row).

**A split archive** (owner decision FE-15). Its parts are muxed one by one as they finish, before a list for the broadcast exists, and are never muxed again, so the list a single file would embed is written beside them at finalize instead, as `<name>.chapters.json` (`utils.ChaptersSidecarSuffix`, `writeSplitArchiveChapters`, `internal/worker/split_chapters.go`) — `<name>` the base the parts share, the first part's `" - partN"` off (`worker.ArchiveSidecarPath`), and no column naming it. The sources and the fitting are the single file's (`archiveChapters`, `internal/worker/chapters.go`, which `prepareArchiveChapters` calls too), measured against the parts' timeline: a VOD's parts laid end to end, a live capture's wall-clock span from part 1's start, gaps included. The list is then moved onto the player's global timeline (`splitArchiveChapters`): a VOD's stands as it is; a live capture's points are placed part by part by each part's `unix_start` and `duration_seconds` — inside a part at its offset plus the distance into it, in a gap at the start of the part after it, past the end at the global end — and a chapter wholly in a gap is dropped.

The file is `{"chapters": [{"start", "end", "title"}]}` in seconds, the route's own shape, and `GET /api/jobs/{id}/chapters` answers it, under the parts' containment guard, when the parts carry no chapters of their own. A gap-fill re-finalize (W14) that inserts recovered parts shifts each point past every recovered part inserted at or before it, so the chapter running into a filled gap covers it and the list's start stays 0, and writes the list under the new first part's base (a recovered opening becomes part 1), removing the one under the old base (`refitSplitArchiveChapters`): the list's wall-clock source went with the staging, and the rows' `unix_start` is the session's clock, so it is shifted rather than rebuilt. A finalize with no list removes it, and so do a Reinitialize that deletes the parts and a complete VOD download that supersedes them (`removeSplitArchiveChapters`); the orphan sweep counts it the archive's (see Chat Files, FE-16).

**Imported from a pasted comment** (owner decisions CI-1 to CI-5). Many videos have no description chapters, but a viewer posts a timestamp comment; the Web player's Import chapters dialog and the TUI's `A C` read one (user-interfaces.md). The one parser is `utils.ParseChapterComment` (`internal/utils/chapter_import.go`), flat: every line that carries a timestamp becomes one chapter in time order, its leading whitespace (U+3000 included), Box Drawing tree marks (├ ┣ ┗ ┃ │ └ ╰ ┠ ┖ ─…) and bullets (- – — • ・ * ▶ ►) stripped; the timestamp is H:MM:SS, HH:MM:SS, M:SS or MM:SS in ASCII or full-width digits and colons, first after the decoration — or last, when it is the line's only one; the title is the rest with the separators next to the timestamp (spaces, - – — | : ： ・) trimmed and everything else, emote shortcodes and emoji included, kept as pasted; an untimed line (a heading, a separator, a decoration) and a run the shapes do not cover ("1:2:3") are ignored; a start pasted twice keeps its first line; an empty title is "Chapter N". It is not yt-dlp's description reader (`ChaptersFromDescription`), which keeps lines only while they run in order.

`utils.FitImportedChapters` then applies the offset (the operator's, ±H:MM:SS with an optional fraction or seconds, `utils.ParseChapterOffset`; the field starts at the capture's known start negated — a late-joined live YouTube capture's `join_offset_seconds` (owner decision CJ-3: a viewer's "1:30:00" lands at 1:30:00 minus the join point; a split archive's is part 1's, CK-3, and gaps between parts are not compensated), 0 for a VOD and a capture recorded from sq=0, and left empty for a row whose join point is unknown, editable either way; pre-filled as a signed clock, "-1:23:45" or "-1:23:45.033" with a fraction (`utils.FormatChapterOffset`, owner decision CK-2), with a note that always says where it came from — "joined 1:23:45 in", "recorded from the start", "VOD" or "start unknown — enter it by hand"; a trim's start would combine with it, but a trim is a file of its own and never the archive an import rewrites, so that term is always 0 — see `defaultChapterOffset`), drops what lands before 0 or at or past the end, puts a chapter named "Intro" (`utils.ImportedIntroTitle`, owner decision CJ-1) in front of a list whose first chapter starts after 0 — where yt-dlp's fill-in, which every automatic list keeps, would put `<Untitled Chapter 1>` — and fits the rest with `SanitizeChapters` against the archive's length (a single file's as ffprobe reads it, a split archive's parts laid end to end).

The preview, Web and TUI alike, shows the Intro row the save would write. Nothing timed is "No timestamps found".

A save writes the list to the archive's `<name>.chapters.json` (`utils.WriteImportedChaptersSidecar`), the FE-15 file, on the player's global timeline, and the first import of a job also writes there, as `"original"`, the chapters the player showed before it — embedded in the file, a finalize's list beside the parts, or none (`[]`). That first read is strict (`jobChaptersStrict`): every file is probed afresh, past the cache, and not under the request's context, since the re-mux outlives the request — a probe that fails refuses the save (503) with nothing written, rather than keeping "none" and re-muxing the archive's own chapters away. The `"original"` key is what marks the file as an import's, and `GET /api/jobs/{id}/chapters` answers an import's list ahead of the file and its parts, so the player shows it at once; Restore original saves the original by the same path, keeping it. The import's lifecycle across a re-finalize (owner decision CJ-2) turns on whether the archive written is the same recording.

A **re-finalize of the same recording keeps it**. Two finalizes are of the same recording:

- MuxJob's finalize (`JobContext.Refinalize`): the Mux action over an Error or Cancelled row that carried an import from before that row was re-run, the automatic mux of an ended Twitch broadcast, the boot re-mux of an interrupted Muxing row.
- The finalize of a download run that continues the recording (owner decision CK-1): `runJobContext` marks a run whose row, read at its start, already names an archive or has recorded parts (`continuesRecording`) — a Resume, the monitor's auto-resume, a parked or requeued row's retry, a boot resume, an auto-reinitialize that continues the same broadcast's parts. A fresh row's first download has neither, and a Reinitialize clears both.

Such a finalize reads the import beside the archive the row names, and only there (`keptImportFor`; a row that names none never had an archive to import into, so a list at the name it writes is another recording's left at a reused name, and is dropped). It refits the list and its original to the new timeline (`utils.RefitImportedChapters`: a chapter at or past the new end dropped, one running past it cut, the last running to the new end; starts and titles kept), embeds the refitted list in a single-file archive in place of the automatic one (`prepareArchiveChapters`), and saves both beside it (`keptImport.write`). When the template moved the archive, the old name's file is removed — and only when it is another file: `sameFilePath` compares the two names normalised and with `os.SameFile`, since on NTFS a name differing only in case is the file just written, and removing it would lose the import and its original on Windows (owner decision PR-M8; the gap-fill's `refitSplitArchiveChapters` asks the same).

A split archive's re-finalize keeps it beside the parts the same way, read where the import route saved it as the row stood at the run's start — beside the first part the row had recorded by then (`JobContext.RecordedFirstPart`, the route's `splitSidecarPath`), else beside the archive the row names — and refitted to the parts' durations laid end to end (`writeSplitArchiveChapters`), so a resumed single file that splits under a name the template resolves afresh carries its import beside the parts and a list another recording left there is written over. A Twitch gap-fill shifts the list and its original past each recovered part exactly as it shifts an automatic list, writing them back as an import's (`refitSplitArchiveChapters`).

A **new recording drops it**: a Reinitialize removes the single-file archive's import when it resets the row (`reinitializeNow`; a split archive's list goes with its parts, `clearJobParts`), so a re-download Cancelled and then muxed by the Mux action cannot carry the old list onto it; a complete VOD that supersedes the parts removes the parts' list (`supersedePartsWithVod`); and a fresh row's first download still removes an import beside the name it writes (`dropImportedArchiveChapters`) or writes its own list over a split archive's, since an archive name can be reused and a list beside it cannot be told from the job's own there. (`POST /api/jobs/{id}/retry` is an alias of `/resume`, so it keeps it; only `/reinitialize` starts over.) A list fitted to a different recording would otherwise go on overriding the new archive's chapters, with Restore offering the old file's original.

A single-file archive is then re-muxed with the list embedded, so VLC, mpv and Plex see it: `engine.Muxer.RemuxChapters`, the archive copy's own `buildArgs` and `writeOutput`, with `-map 0 -map -0:d -map_metadata 0 -c copy` (every stream, the global metadata, the yt:stretch aspect; the old chapter track, which ffprobe lists as a data stream, left out so imports do not stack them) and the FFMETADATA input mapped with `-map_chapters`, or `-map_chapters -1` to restore an archive that had none. FFmpeg writes a temporary file beside the archive, which replaces it only once `verifyArchiveRemux` (`internal/worker/chapter_import.go`) has held it to the original — the same media streams, none of them, and not the file, short of its original by more than the re-mux's own tolerance (`remuxIsShort`: the larger of 1 s and 0.5% of the original, measured however short the archive is — owner decision PR-M7; the live capture's `muxedOutputIsShort`, which allows 20 s and judges nothing under a minute, is for raw fragments, not a stream copy that replaces the only copy), and exactly the chapters asked for.

Before it writes anything the re-mux checks the archive's volume (owner decision PR-I4, `remuxRoom`): it needs the archive's size plus `remuxHeadroom` (256 MiB) free, and the copy must not take the volume to `disk_critical`; a volume short of either is not re-muxed (`ErrArchiveRemuxNoSpace`), the list is saved beside the archive, and the save's view says so — `noSpace`, and an `embedError` with the sizes, which both UIs already show as "saved beside the file, which could not be rewritten". Any failure leaves the archive untouched and the sidecar saved; the row's `file_size` and `length_seconds` follow a replaced file through `UpdateJobFieldsIf` on the status read under the claim. A split archive gets the sidecar only: recorded parts are never rewritten.

Only a complete Finished row with an archive is imported (`importableJob`: Finished and not `incomplete_tail`; owner decision, 2026-11-07, narrowing CI-1) — an Error or Cancelled row, or a Finished one missing its tail, can still be resumed by the operator or the monitors, and a resumed run rewrites the archive, so import is not offered there — under the per-job claim the Mux action and Recover Set-aside take (`ClaimChapterImport`), so neither runs while an import does; a trim and an import refuse each other, a run still stopping refuses it, the re-mux runs under the orchestrator's mux root (Stop cancels it, a closed browser tab does not), and the row's deletion stops it before the copy takes the archive's name.

Those, and a Twitch gap-fill, are the only verbs that reach a complete Finished row; a gap-fill leaves its plan due for the next sweep while an import holds the claim (`twitchGapFiller.archiveRewriting`). Resume, Retry and Reinitialize refuse a complete Finished row by its status (400) before the claim is ever asked, so what follows is a safety net (owner decisions PR-M5, kept by PR-S1), not the import's exclusion: those three REST verbs answer 409 while an import holds the claim (`refuseWhileStagingClaimed`, which since owner ruling R-A2 refuses for any holder of the claim, naming it, and keeps the import's own message); a Resume, Reinitialize or auto-reinitialize that reaches the worker another way (the TUI's, the monitors') waits for the import — for any holder, with no cut-off (R-A1) — and then runs (`afterJobExit`); a run's own finalize waits before it writes anything (`muxAndFinalize`, through `waitArchiveRewrite` = `waitChapterImport`); and the re-mux re-reads the row immediately before its rename, leaving the archive as it was when the row is no longer in the status the import read or a run of the job is in flight (`archiveStillAsRead`).

### Chat Files

Chat is stored as JSON with the following top-level structure:

```go
type ChatData struct {
    VideoID          string        `json:"videoId"`
    VideoTitle       string        `json:"videoTitle"`
    ChannelName      string        `json:"channelName"`
    StreamStartTime  string        `json:"streamStartTime,omitempty"`
    DownloadedAt     string        `json:"downloadedAt"`
    MessageCount     int           `json:"messageCount"`
    Messages         []ChatMessage `json:"messages"`
}
```

`StreamStartTime` is the epoch every message's `OffsetMs` is computed against. It is written with `time.RFC3339Nano` (`epochRFC3339`, `internal/chat/downloader.go`) so millisecond-precision offsets never lose up to 999ms of the header's own fractional second, and read back with `time.Parse(time.RFC3339, ...)`, which accepts the fractional-second suffix `RFC3339Nano` produces. Each `ChatMessage.OffsetMs` is SIGNED on both platforms — a message that arrived before the video's own start position is negative rather than clamped to 0 (YouTube pre-stream/waiting-room chat; Twitch messages timestamped before a part's recording base) — see `platform-services.md` for how each platform computes it.

**The Twitch chat file** (`TwitchChatData`, `internal/twitch/types.go`) has its own header: `platform`, `channelLogin`, `channelDisplayName`, `streamId`, `streamStartTime`, `recordingStartTime`, `downloadedAt`, `messageCount`, `emoteOffsets`, then the optional `emotes` object and the `messages` array. Field ORDER is load-bearing on both ends: every scalar precedes `emotes`/`messages` because `chatFileRecordingBaseMs` (`internal/twitch/chat.go`) stops its bounded header scan at the first composite value, and `messages` is last because the append path splices at the file's final `]`.

- `emoteOffsets` names the index space each message's `emotes[].start`/`end` count in. `"utf16"` is the only value any writer produces (`chatEmoteOffsetsUTF16`, `internal/twitch/chat.go`), and its ABSENCE is the era marker: a file written before 2026-09-15 holds the live IRC path's raw code-point offsets, because that path mistook Twitch's code-point offsets for UTF-16 units. The player repairs such a file once at load — `correctLegacyTwitchEmotes` (`web/public/modules/chat-timeline.js`) maps the spans and unwraps `/me` text, per part, before `mergePartChats` (which keeps no header scalars). Only messages carrying `raw` are touched: VOD comments have none and were UTF-16 all along. **Known window:** only the two full-file writers set the scalar, so a part file created before the change and appended to after it keeps an unmarked header while its newest messages already carry UTF-16 offsets; those few are over-shifted at replay, and the window closes at the next part roll or at job end.
- `isAction` marks a `/me` message. The CTCP wrapper `\x01ACTION …\x01` is stripped at parse time so `message` holds only the text and the emote offsets line up; `raw` keeps the verbatim IRC line. `raw` stays in the file; the Web player reads it only for the legacy repair above and drops it from the rows it keeps in memory (`player.js`), since it is usually larger than the rest of the message.
- **Moderation rows** (owner ruling R-G1). A live capture stores each Twitch `CLEARCHAT` (a timeout, a ban, a full chat clear) and `CLEARMSG` (one deleted message) as a row of `messages` in arrival order: `messageType` `"moderation"`, `moderation` one of `"deleted"` (with `targetMsgId`), `"author_deleted"` (with `targetUserId`, and `banDurationSec` for a timeout; absent for a ban) or `"chat_cleared"`, `targetLogin` when the wire named the user, the event as a sentence in `systemMsg`, an empty `message` and `authorId`, and the verbatim line in `raw`. The five fields are omitempty and appear on no other row. A row is counted in `messageCount` like any other, so the header count, the resume sidecar and the salvage reconciliation are unchanged; the messages it removed stay in the file exactly as sent, with no `deleted` flag written — the file is append-only and is not rewritten for a deletion. See platform-services.md § Message Processing.
  The Web player applies them once at load (`applyTwitchModeration`, `web/public/modules/chat-timeline.js`), on the list in file order and, for a multi-part job, on the merged list: a `deleted` row marks the message with that `id`, an `author_deleted` row every EARLIER message with that `authorId` (the user may post again after a timeout; a row with an empty `targetUserId`, which Twitch does not send today, is matched by its `targetLogin` against the author's `authorName` without regard to case, so it cannot reach a user whose display name is a localized one, and a row naming neither marks nothing). Each marked message gets `deleted: true` and the first such event's sentence as `deletedState`, in memory only, and is then drawn as a YouTube deletion is (below); these rows themselves leave the list, so the sidebar, the overlay and the sidebar's counts never hold one. A `chat_cleared` row (a moderator's `/clear`) marks nothing (owner ruling "show a marker only"): every earlier message stays as it was, unstruck in the sidebar and still flying on the overlay, across parts. The row stays in the list at its own place and offset as an in-memory copy retyped `messageType` `"system"`, with `systemMsg` "Chat was cleared by a moderator" (`TWITCH_CHAT_CLEARED_TEXT`, whatever sentence the row carries), an empty `message` and `sidebarOnly: true`. The sidebar draws it as it draws any `system` notice, one dim notice line at that moment, and counts it in its header as it counts those; `sidebarOnly` keeps it off the video (`_buildNicoEl`) and out of the "+N not shown" count (`NicoScheduler.countDrop`). A file with no such row — every file written before them, every VOD chat — comes back as the same array, untouched.
  A player from before the rows existed (v2.9.8 and earlier) does not crash on them: it draws each as a flat sidebar row reading "Moderation" with no text, flies nothing on the video for it (its overlay skips a message with no content), counts it in the sidebar header, and leaves the removed messages unmarked; a full clear is such a row too, with no line of its own. Player and recorder shipped in the same release.
  The row's keys and the three `moderation` words are held to the player by a shared fixture, `web/tests/fixtures/twitch-moderation-parity.json`: `internal/twitch/chat_moderation_parity_test.go` fails when `parseLine` no longer marshals the fixture's IRC lines to exactly its rows, and `web/tests/twitch-moderation.test.mjs` runs `applyTwitchModeration` over those rows.

**Markers and the raw sidecar (YouTube, owner decision D16).** A message YouTube deleted, retracted or swept with a banned author's carries `deleted: true` and, when YouTube sent one, `deletedState` ("[message retracted]"); a replaced one `replaced: true` and its new parts in `replacement` — the original text always kept. All four are omitempty. Every action that did not become a message is in `chat.raw.jsonl` beside the chat file (`<name>.chat.raw.jsonl` beside the archive), one JSON per line. It is the only record of what the chat file leaves out, so a failed copy of it, at finalize or beside a recovered set-aside recording, keeps the chat capture in staging exactly as a failed `chat.json` copy does: `chat_status` reads `incomplete`, the staging cleanup prunes the dir down to the capture, and the failure is logged at Error (owner decision PR-M6). See platform-services.md § Message Types.

A `replaceChatItemAction` whose target was never stored — a `liveChatPlaceholderItemRenderer`'s, the item YouTube shows while a message is held for review — becomes a message of its own at its own timestamp (owner decision FE-14), and is still copied to the raw sidecar. The marker carries the replacement item parsed whole (`ChatMarker.Message`, at its replay wrapper's offset); `applyMarkers` hands it back for `commitMessages` — the batch's own commit: the offset, the dedup, the count, past the replay high-water mark its batch just advanced — when nothing is on disk or the dedup still holds every stored ID and not the target's. Otherwise it is held, and the end-of-run pass (`applyPendingMarkers`) reads the whole file and appends it only if no message on disk is its target. The player sorts a chat by offset, so its place at the end of the array is no matter.

The Web UI player's chat replay draws the markers (owner decision FE-13): a deleted message dimmed and struck through with a "deleted" tag whose tooltip, when the message carries `deletedState`, is YouTube's own wording for it (owner decision FQ-5), a replaced one with its new text and an "edited" tag whose tooltip holds the original, and found by the chat search by either text. The nico-style video overlay flies no deleted message and flies a replaced one as its replacement (FQ-5). Exports read the fields unchanged.

**The derived chat and chapter files** (owner decision FE-16). `<name>.chat.raw.jsonl` and a split archive's `<name>.chapters.json` are named after the archive and on no column, and are treated as its chat archive is: the orphan sweep (`GET /api/files/orphaned`, `archiveDerivedStem` in `internal/worker/orphans.go`) counts each the job's while a row names the archive, its parts' base or its chat, and lists it once none does; an aside recovery carries the kept capture's raw sidecar beside the recovered recording with its chat (`copyKeptChatSidecar`); and the parts' chapter list goes with the parts on a Reinitialize and a VOD supersede. Moombox has no job-delete, output-folder-move or chat-carrying export path that removes or moves a job's files (`DELETE /api/jobs/{id}` removes the row; its files become the sweep's), so the sweep is where the two files leave.

**Incremental append pattern:**

To avoid O(file_size) rewrites as chat grows, the downloader uses an incremental append strategy after the first flush to disk:

1. Open the file in read-write mode.
2. Seek backward from the end to find the closing `]` of the messages array.
3. Truncate the file at that position.
4. Append: comma-separated new messages + `\n  ]\n}`.
5. Write at the truncation point.

If the incremental append fails for any reason, it falls back to a full rewrite.

**Fixed-width messageCount:**

The `messageCount` field in the JSON header is padded to 20 characters with trailing whitespace. This allows the header to be updated in-place (overwriting the count value at a known byte offset) without shifting the rest of the file. The `updateChatFileHeader()` method reads the first portion of the file, replaces the messageCount and downloadedAt values, and writes them back.

**Chat resume state:**

```go
type ChatResumeState struct {
    MessageCount  int      `json:"messageCount"`
    Continuation  string   `json:"continuation"`
    Timestamp     int64    `json:"timestamp"`
    VideoID       string   `json:"videoId"`
    RecentIDs     []string `json:"recentIds"`
    StreamStartMs int64    `json:"streamStartMs,omitempty"`
    Mode          string   `json:"mode,omitempty"`
}
```

Saved as `<chat_file>.resume.json`. Updated on every disk flush (at most once per `writeInterval`, 1 s) — `maybeFlush` writes the chat file and the sidecar together, so the two never describe different positions. `streamStartMs` is the epoch every `offsetMs` already written to the chat file was computed against; a restarted run reads it back and keeps it even when its own `StreamStartTime` option carries a newer (actual, vs. scheduled) start — one chat file, one epoch, never two. `mode` is `"live"` or `"replay"` — which kind of run wrote the sidecar (the mode rule: a replay run refuses a live run's sidecar and starts from scratch, since its count, continuation, dedup IDs and epoch all describe the live half of the file; an empty `mode`, written before the field existed, is adopted as before). (An older `lastTimestampUsec` field may still appear in sidecars written before this field existed — it is ignored on load and no longer written.) A replay that gives up keeps its sidecar, like a cancelled one; only a run that reached the end of the archive clears it. A replay run that finds an archive and no usable sidecar writes `<chat_file>.rerun` beside it and swaps it in only on a completion or at least as many messages (`finishReplayRerun`, `internal/chat/downloader.go`) — `keepOnlyChatCapture` keeps a `.rerun` left behind, as it keeps every `chat.json.*` file.

**Batching:**

Chat messages are buffered in memory and flushed to disk at 1-second intervals (`writeIntervalMs = 1000`). After flushing, the in-memory buffer is released to minimize memory usage during long streams.

---

## Logger (internal/logger/)

### Overview

The `Logger` wraps Go's `slog` package with file rotation, a ring buffer for recent entries, per-job log buffers, and a pub/sub system for real-time delivery.

### Multi-Writer Output

Log output is sent to both, in this order:

1. **Log file** via the Logger itself (which implements `io.Writer` with rotation).
2. **Stdout** via a `switchableWriter` that can be toggled off when the TUI is running (BubbleTea owns the alternate screen; raw writes would corrupt the display). `SuppressStdout()` / `RestoreStdout()` control this.

**Stdout is best effort and can never cost the file a line.** The `lineSinks` fan-out writes the file first, so the line is on disk before stdout is touched, and ignores each sink's error, so neither keeps the line from the other. `io.MultiWriter(stdout, file)` would not do: it returns at the first writer's error, so a stdout that fails every write for the rest of the run — the hung-up tty of an SSH session that started `moombox --headless` and logged out (EIO), a process started with fd 1 closed (EBADF), a console-less Windows child's invalid handle — would keep every line out of `moombox.log`, the only persistent log, while the ring buffer and the dashboard looked normal. The same writer with the file first would have handed the bug to the console: a full disk would keep every line off it.

**A broken pipe is an error, not a death sentence (Unix).** When stdout or stderr is a pipe whose reader went away — an `ssh host moombox --headless` session without a pty that dropped, a `| tee` that exited, and the launcher's child, which inherits the launcher's fd 1 — the Go runtime kills a process that writes to it, from inside the write, unless the process asked for SIGPIPE; a parent that started it with SIGPIPE ignored does not change that. No error would ever reach the sinks, and the process — every later line, every recording in progress — would be gone. `SurviveBrokenPipes` asks for SIGPIPE (`signal.Notify` on a channel nobody reads; not `signal.Ignore`, which FFmpeg and the Node sidecar would inherit across exec), so the write fails with EPIPE like any other and the sinks swallow it. `New` calls it, and `main` calls it before anything else, for the launcher's crash-supervision and rollback notices and the child's startup banner, which are written without a Logger. Windows has no SIGPIPE: a broken pipe there is an ordinary write error.

### File Rotation

| Setting | Default | Config Key |
|---------|---------|------------|
| Max file size | 10MB | `logs.log_max_file_size` |
| Max files | 5 | `logs.log_max_files` |

**Rotation algorithm:**

1. Close current log file.
2. Shift existing numbered files: `.5` -> `.6`, `.4` -> `.5`, ..., `.1` -> `.2`.
3. Rename current file to `.1`.
4. Delete excess file (`.max_files+1`).
5. Open a fresh file.

Rotation is checked after every write. The `currentSize` counter tracks bytes written since the last rotation.

**A rotation whose renames fail backs off for `rotateBackoff`** (`internal/logger/logger.go`, 60 s) and reports the error once per failure STREAK rather than once per attempt. On Windows any process holding `<log>.1` open — a tail, an editor, an antivirus scanner — makes both renames fail, and re-attempting after every write past the cap turned that into a WARN diagnostic per log line, into the ring buffer, every WebSocket subscriber and the TUI log panel, while the live file kept growing (reproduced at 7.4× the cap). While the back-off runs the live file is allowed to grow past `log_max_file_size`; no log line is ever dropped for it. A rotation that succeeds clears both the window and the streak, so the next failure reports again.

**While the streak lasts the logger repeats itself once per `rotateReportInterval`** (1 h), naming how long the rotation has been blocked and how large the un-rotated file has become. That reminder exists because the one error line is easy to lose: rotation diagnostics go through `diagf`, which writes to **stderr** when the TUI is not holding the terminal — so they never reach `moombox.log` itself — and into the 200-entry ring buffer when it is, where a busy instance recycles them within minutes. Measured on a rename failing for 48 h: one error line and 2,879 silent attempts before, 48 lines after, with the file at 2,135× the cap either way.

### Ring Buffer

A fixed-size ring buffer (200 entries, `defaultRingSize`) holds the most recent log lines in memory. Used to populate the TUI log panel and Web UI log view on initial load (before real-time subscription kicks in).

`GetRecentLines()` returns lines in chronological order regardless of current buffer position.

**Every line the ring takes is numbered** (`ringSeq`, 1 for the first line of the process), and `RecentLines()` returns the lines together with the number of the newest, in one read. A reader that pairs a snapshot with a live feed subscribes FIRST (`SubscribeLines`, whose lines carry their number) and reads the snapshot second, so no line falls between the two; a line logged in between is then in both, and the reader skips every fed line numbered at or below the snapshot's. Both such readers do: the dashboard, through `initial_state`'s `logSeq` and each `log` frame's `seq`, and the TUI's log forwarder (`forwardTUILogs`, `cmd/moombox/tui_wiring.go`) against its backfill. Without the number the line showed twice — at DEBUG the hub's own "websocket connected" line, on every dashboard connect.

### Per-Job Log Buffers

The LIVE per-job log pipeline is the database's: the logger's line router (`SetLineRouter`, called inside every log call, before `Debug`/`Info`/`Warn`/`Error` return) feeds `db.RouteLogToJobs()`, served by `db.GetJobLogs` (capped at 200 lines, trimmed to 100; `db.ClearJobLogs` drops a deleted job's buffer). Only NON-TERMINAL jobs are scanned for — see § Per-Job Log Buffers above for the routed set.

The Logger type once carried a parallel `LogForJob`/`GetJobLogs`/`PruneJobLogs` buffer API; nothing in production ever wired it (the buffers stayed permanently empty at runtime), and it was removed in 2026-07. Per-job log consumers use the database pipeline above.

### Pub/Sub

`Subscribe()` returns a buffered channel (`cap 100`) that receives formatted log lines in real-time.

`Unsubscribe(ch)` removes the channel from the subscriber list. The channel is not closed (to avoid a race with concurrent `broadcast()` calls); it is left for GC.

`SubscribeLines()` / `UnsubscribeLines(ch)` are the same with each line delivered as a `Line{Seq, Text}` — its number in the ring beside the text (see § Ring Buffer).

`broadcast(line, seq)` sends to all subscribers of both kinds. If a subscriber's channel is full, the line is dropped (non-blocking send).

### Log Levels

| Level | slog Equivalent |
|-------|-----------------|
| DEBUG | slog.LevelDebug |
| INFO | slog.LevelInfo |
| WARN | slog.LevelWarn |
| ERROR | slog.LevelError |

`SetLevel(level)` changes the level dynamically at runtime. Level check is performed before any formatting work.

### Timestamp Format

All log lines use `2006-01-02 15:04:05` format (Go reference time). This applies to both the slog handler output and the formatted lines in the ring buffer / pub/sub.

### Crash Output File

Beside the log sits `<log name>-crash.log` (`moombox-crash.log` for the default `./moombox.log`; `crashOutputPath`, `cmd/moombox/crash_output.go`), the runtime's second destination for a fatal error's output — an unrecovered panic, a fatal runtime error, a fatal signal's trace — which otherwise reaches stderr only and never the logger. Each child creates it empty as soon as the logger is open; the next start logs the beginning of whatever a crash left there and moves it to `<crash file>.1` (the previous crash's, at most 1 MiB) — or copies it there when it cannot be moved. See operations.md §Launcher/Supervisor Pattern.

---

## BotGuard Sidecar Cache (`%LOCALAPPDATA%/Moombox/sidecar/`)

Moombox extracts the embedded Node.js binary and BotGuard sidecar payload to a per-user cache directory on first launch. This is the only on-disk artifact Moombox produces outside its working directory.

### Path

`os.UserCacheDir() + "/Moombox/sidecar"`. On Windows: `%LOCALAPPDATA%/Moombox/sidecar`. Chosen over `%TEMP%` because Defender heuristics treat `%TEMP%` extractions of executables as more suspicious than `%LOCALAPPDATA%`.

### Contents

```
%LOCALAPPDATA%/Moombox/sidecar/
├── moombox-sidecar[.exe]            (the Node binary, ~83 MB extracted from the platform's gzipped ~33-44 MB node-<goos>-<goarch>.gz)
├── package.json
├── package-lock.json
├── version.txt                      (the embedded Node manifest + "sidecar@<sha256 of sidecar.tar.gz>" — cache-invalidation key)
├── src/
│   ├── server.js                    (~900 lines, JSON-RPC server)
│   ├── cipher.js                    (ejs signature/n solving)
│   └── homepage.js                  (homepage challenge extraction)
└── node_modules/                    (production deps: bgutils-js, jsdom, transitives — ~17 MB)
```

### Lifecycle

- **First launch:** `extractIfNeeded(cacheDir)` creates the dir, applies `utils.ApplyUserOnlyDACL` to tighten permissions to current-user-only (matches the config-dir hardening), gunzips the platform's `node-<goos>-<goarch>.gz` into `moombox-sidecar[.exe]`, gunzip+tar-extracts `sidecar.tar.gz` using stdlib `archive/tar` + `compress/gzip` (no system tar required), writes `version.txt` last.
- **Subsequent launches:** Compares the on-disk `version.txt` against the stamp `buildCacheStamp` computes: the embedded `bgembed.Version` (the Node pin) plus `sidecar@<sha256>` of the embedded tarball. The tarball hash is what makes a sidecar JS change a mismatch; `Version` alone does not move with it. On match AND key files present, skips extraction. On mismatch (Node version bump, sidecar JS update), deletes the old `version.txt`, then re-extracts the whole payload, removing what the dir held under each top-level name the tarball writes (`src`, `node_modules`, `vendor`, the manifests) so nothing from the previous payload is left behind. Files the tarball does not name are left alone.
- **Tar-slip defense:** Rejects any tar entry whose target path escapes `cacheDir`.
- **DACL hoist:** Runs even on cache-hit so users upgrading from v2.5.x (whose pre-existing dir was created with the looser inherited ACL) get the tightened DACL on the next launch.

### Cleanup

Not currently auto-cleaned. The cache survives Moombox uninstall — operators wanting to reclaim the ~100 MB can manually delete the dir. A `moombox uninstall-data` CLI subcommand is planned but not in v2.6.0.

---

## Cross-References

- **[architecture.md](architecture.md)** -- The synchronous write path and where write amplification is actually bounded; pub/sub as an inter-component communication mechanism; service initialization order (Config -> Logger -> Database -> ...).
- **[security.md](security.md)** -- Password auto-hashing in config; client_tokens table and scrypt token hashing; cookie file permissions.
- **[platform-services.md](platform-services.md)** -- How YouTube and Twitch services consume cookies from the jar; SAPISIDHASH generation; PO token dependency on cookies.
- **[operations.md](operations.md)** -- Config file search paths; database file location; log file paths; staging vs output directories.

### Source Files

- `internal/database/database.go` -- Database struct, Open(), UpdateJobFields, CRUD operations
- `internal/database/database_subscribers.go` -- The six subscriber kinds, safeCall* wrappers, dispatchJobsChange
- `internal/database/types.go` -- Job, Gap, Segment, TrimRecord, ClientToken, JobStatus, JobStats type definitions
- `internal/database/migrations.go` -- Schema DDL, versioned migrations
- `internal/config/config.go` -- Load(), Save(), migrateOldFormat(), validate(), ResolveTemplate()
- `internal/config/types.go` -- MoomboxConfig and all section structs
- `internal/config/flex_duration.go` -- FlexDuration type with TOML/JSON marshaling
- `internal/config/channel_terms.go` -- ChannelTerms with dual string/map representation
- `internal/cookies/jar.go` -- CookieJar, Netscape parsing, auth cookie detection, SAPISIDHASH
- `internal/cookies/refresh.go` -- RefreshService struct, constructor, lifecycle (Start/Stop/GetStatus/CheckNow)
- `internal/cookies/refresh_update_types.go` -- cookieUpdateKey/cookieUpdate/cookieOrigin, the pending Set-Cookie update vocabulary
- `internal/cookies/refresh_auth_status.go` -- AuthStatus, verdictFromCheck, the Twitch credential-loss vocabulary, authStatusChanged
- `internal/cookies/refresh_liveness.go` -- the liveness recovery pilot: ObserveLiveness, recordLiveness, the per-platform re-fire back-off
- `internal/cookies/refresh_pass.go` -- the refresh pass: CheckYouTubeAuth/CheckTwitchAuth/NoteTwitchAuthLoss, doRefresh/refresh
- `internal/cookies/refresh_youtube.go` -- the YouTube guide-endpoint exchange, its auth verdict, and processing its Set-Cookie reply
- `internal/cookies/refresh_cookiefile.go` -- the Netscape cookie-file rewrite rules: admitSetCookie, updateCookieFile
- `internal/cookies/refresh_twitch.go` -- checkTwitchAuth, the Twitch oauth2/validate check
- `internal/cookies/autocookies.go` -- AutoCookieService struct, constructor, refreshBrowser, FlagManualRelogin, Stop
- `internal/cookies/autocookies_setup_slot.go` -- the setup-slot lifecycle: browser-gone tracking, the abandoned-setup reap
- `internal/cookies/autocookies_status.go` -- AutoCookieStatus, GetStatus, ReloginStatus, LogProfileDirVerdict
- `internal/cookies/autocookies_browser_resolve.go` -- validateBrowserProfileDirForLaunch and browser/profile-path resolution
- `internal/cookies/autocookies_setup.go` -- StartSetup, FinishSetup(Detailed), CancelSetup, AbandonSetup
- `internal/cookies/autocookies_verdict.go` -- RefreshVerdict, RecheckReport, RefreshResult and the verdict algebra over them
- `internal/cookies/autocookies_refresh.go` -- RefreshCookies/RefreshCookiesDetailed, the headless-browser refresh pass
- `internal/cookies/autocookies_messages.go` -- operator-facing message rendering: platformDisplayName, cookiesLostMessage
- `internal/cookies/autocookies_periodic.go` -- StartProfileSeed/StartPeriodicRefresh, the periodic-refresh skip rules
- `internal/cookies/autocookies_process.go` -- process lifecycle helpers: killing a setup/refresh browser, Job Object tracking
- `internal/cookies/cookie_files.go` -- cookie file hygiene: writeFileAtomic, orphaned temp-file sweep, directory ACL tightening, and CarryCookieFileTo (the cookies a first-run setup carries into the cookie file it saves)
- `internal/logger/logger.go` -- Logger, file rotation, ring buffer, pub/sub, per-job buffers
- `internal/engine/downloader.go` -- ResumeState, resume file management
- `internal/chat/downloader.go` -- Chat file writing, incremental append, messageCount padding
- `internal/chat/types.go` -- ChatData, ChatMessage, ChatResumeState

## Tests

The tests that pin the behaviour above, by the section they pin. Each bullet names the paragraph it belongs to when the paragraph has a bold lead.

**Tables**

- twitch_gap_fills: pinned by `TestTwitchCaptureThatLacksNothingDropsAStaleGapFillRow`, `internal/worker/twitch_gapfill_capture_test.go`, and `TestMuxedTwitchCaptureThatLacksNothingDropsAStaleGapFillRow`, `internal/worker/twitch_gapfill_guards_test.go`

**[cookies]**

- `TestRestartRequiredListsAgree` pins the two restart-required lists against each other.

**Cookie Jar**

- Essential YouTube cookies (21): `TestSOCSIsAdmittedFromYouTubeDomainsAlone`, `internal/cookies/socs_scope_test.go`
- Essential YouTube cookies (21): `TestAnExpiredSOCSIsNotSentOnInnertube`
- Essential YouTube cookies (21): `TestASOCSRefreshStaysOnItsSideOfWWWYouTube`; `TestADeletedGoogleSOCSIsNotAnEssentialCookie`
- “Every name in a loose set must also be…”: `TestAuthCookieNameListsDoNotDrift`
- “`GenerateAuthorizationHeader(origin)` / `GenerateAuthorizationHeaderForUser(origin, userSessionID)`: SAPISIDHASH + SAPISID1PHASH + SAPISID3PHASH…”: `TestMakeSidAuthorizationUserSessionVector`, `TestGenerateAuthorizationHeaderForUserBindsEveryScheme`

**Refresh Service**

- `TestCookiesHTTPClientCarriesNoCookieJar` pins that `cookiesHTTPClient` carries no `http.CookieJar`.
- `TestLivenessRecoveryPilotIsArmed` (`internal/cookies/refresh_liveness_test.go`) pins the fire, the Warn sentence and the in-window dedupe; `TestTwitchMarkAndTierTwoFireRecoveryOnce` (`internal/cookies/refresh_twitch_mark_test.go`) pins that a chat-marked Twitch loss plus the tier-2 verdict that follows it reach `OnRecoveryNeeded` once, and that the re-alarm lands only once the window has passed.
- `authStatusChanged` is a CONTRACT, not an observation: `TestAuthStatusChangedGateCoversEverySurfaceInput` has a row for each of the six inputs, including an "alone" row for each `Authenticated` boolean.

**Auto-Cookie Service**

- `TestRefreshPreflightSentenceAgreesAcrossSurfaces` pins the pre-flight sentence by exact equality across the TUI and the Web.
- `TestRefreshPostflightMechanismAgreesAcrossSurfaces` pins `cookieRefreshMechanismLabel` by exact equality across the TUI and the Web.
- `TestRungThreeSentencesDivergeByDesign` asserts that the rung-3 sentences diverge.
- `cleanup()` / `cleanupLocked()` MUST NOT clear it: Pinned by `TestCleanupAfterAFailedSetupKeepsLastError` and `TestFinishSetupRecordsTheFailureItReturns`.

**Staging Directory**

- Display aspect: Pinned by `TestHasStagingFiles` (`internal/worker/staging_test.go`), `TestRecordDisplayAspect`, `TestTheArchiveCopyAppliesTheRecordedAspect` and `TestExecuteWithChatRecordsTheStretchBeforeDownloading` (`internal/worker/display_aspect_test.go`), `TestWithDisplayAspect` and `TestMuxCopyAspectSetsTheDisplayAspect` (`internal/engine/muxer_aspect_test.go`), `TestMuxCopyArchiveCarriesAspectAndChapters` (`internal/engine/muxer_archive_test.go`), and `TestStretchedRatioFromKeywords` (`internal/youtube/rank_test.go`).

**Archive Chapters**

- Shown in the Web UI player: Pinned by `TestChaptersSingleFileArchive`, `TestChaptersMultiPartShiftedByPartOffset`, `TestChaptersEmptyAnswers`, `TestChaptersCacheFollowsTheFileVersion`, `TestChaptersAnsweredInTimeOrder`, `TestChaptersPartsFollowThePlayersTimeline`, `TestParseFFprobeChaptersReadsTheTitleTag`, `TestChapterCacheIsBounded` and `TestChapterCacheRetriesWhatSaysNothingAboutTheFile` (`internal/web/routes/chapter_routes_test.go`), and `web/tests/player-chapters.test.mjs`.
- A split archive: Pinned by `TestSplitArchiveChaptersOnTheGlobalTimeline` and `TestSplitArchiveFinalizeWritesItsChapterList` (`internal/worker/split_chapters_test.go`), `TestSplitLiveCaptureGetsItsArchiveVodChapters` (`internal/worker/split_chapters_source_test.go`), `TestGapFillRefitsTheSplitArchiveChapters` (`internal/worker/twitch_gapfill_chapters_test.go`), `TestChaptersSplitArchiveListBesideTheParts` (`internal/web/routes/chapter_routes_split_test.go`) and `TestDerivedSidecarsFollowTheParts` (`internal/worker/derived_sidecars_test.go`).
- “A re-finalize of the same recording keeps it: MuxJob's…”: `TestKeptImportWriteNeverRemovesTheFileItWrote`, `TestRefitSplitArchiveChaptersNeverRemovesTheFileItWrote` and `TestSameFilePath` in `internal/worker/path_compare_test.go`
- “FFmpeg writes a temporary file beside the archive, which…”: `TestVerifyArchiveRemuxTolerance`
- “Before it writes anything the re-mux checks the archive's…”: `TestRemuxRoom`, `TestRemuxArchiveChaptersChecksTheVolumeFirst` in `internal/worker/chapter_import_claim_test.go`; `TestChapterImportNoSpaceSavesTheSidecarOnly` in `internal/web/routes/chapter_import_test.go`
- “Only a complete Finished row with an archive is…”: `TestChapterImportOnlyIntoACompleteFinishedJob`, `TestChapterImportableOnlyACompleteFinishedJob`, `web/tests/chapter-import-scope.test.mjs`
- Pinned by `TestResumeAndReinitializeWaitForAChapterImport`, `TestFinalizeWaitsForAChapterImport`, `TestWaitChapterImportReturnsOnRelease`, `TestRemuxArchiveChaptersRefusesARowThatMovedOn`, `TestGapFillSkipsWhileAChapterImportRuns` and `TestWorkerGapFillerAsksTheImportClaim` (`internal/worker/chapter_import_claim_test.go`) and `TestVerbsRefusedWhileAChapterImportRuns` (`internal/web/routes/jobs_retry_worker_test.go`).
- Pinned by `TestParseChapterCommentGoldens`, `TestParseChapterCommentIrys`, `TestParseChapterCommentEdgeRows`, `TestFitImportedChapters`, `TestImportedListIsLedByIntro`, `TestParseChapterOffset`, `TestFormatChapterOffset` and `TestImportedChaptersSidecar` (`internal/utils/chapter_import_test.go`, over the owner's three example comments in `internal/utils/testdata/chapter_import/`), `TestRemuxChaptersArgs` and `TestRemuxChaptersKeepsTheArchive` (`internal/engine/muxer_remux_chapters_test.go`), `TestVerifyArchiveRemux`, `TestRemuxArchiveChaptersEmbedsAndRecordsTheFile`, `TestRemuxArchiveChaptersRefusedLeavesTheArchive`, `TestRemuxArchiveChaptersStopsWhenTheRowIsDeleted`, `TestChapterImportClaimExcludesTheOtherVerbs`, `TestSingleFileFinalizeDropsAnImportedChapterList` and `TestRefinalizeOfANeverMuxedRowDropsAForeignImport` (`internal/worker/chapter_import_test.go`), `TestMuxActionKeepsAnImportedChapterList`, `TestReinitializeDropsAnImportedChapterList`, `TestSplitRefinalizeKeepsAnImportedChapterList`, `TestProcessJobBuildsTheRunContext`, `TestRunContextContinuesTheRecording`, `TestResumeKeepsAnImportedChapterList` `TestSplitResumeKeepsTheImportBesideTheRowsArchive` and `TestSplitRefinalizeReadsTheImportWhereTheRouteSavedIt` (`internal/worker/chapter_import_refinalize_test.go`), `TestGapFillRefitsTheSplitArchiveChapters` (`internal/worker/twitch_gapfill_chapters_test.go`), `TestRefitImportedChapters` (`internal/utils/chapter_import_test.go`), and `TestChapterImportPreviewDoesNotWrite`, `TestChapterImportSaveRemuxesAndRestores`, `TestChapterImportFailedRemuxKeepsTheSidecar`, `TestChapterImportFirstSaveKeepsTheArchivesChapters`, `TestChapterImportSplitArchiveSidecarOnly`, `TestChapterImportRefusals`, `TestChapterImportPrefillsTheJoinPoint`, `TestChapterImportSplitArchivePrefillsPartOnesJoinPoint` and `TestDefaultChapterOffset` (`internal/web/routes/chapter_import_test.go`), `TestFinalizeRecordsTheJoinOffset`, `TestJoinOffsetAsksEveryLateInput` and `TestReinitializeClearsTheJoinOffset` (`internal/worker/join_offset_test.go`), `TestSplitFinalizeRecordsPartOnesJoinOffset` (`internal/worker/split_join_offset_test.go`), `TestDownloadHlsCarriesTheSegmentDuration` (`internal/worker/strategy_youtube_hls_test.go`).
- The embedding is pinned by `TestMuxCopyAtChaptersEmbedsThem` (`internal/engine/muxer_chapters_test.go`), `TestMuxAndFinalizeEmbedsTheChapters` (`internal/worker/metadata_chapters_test.go`), `TestRecordedFromStart` and `TestRunDownloadersRecordsWhereEachRecordingBegins` (`internal/worker/recording_start_test.go`), `TestDashLiveReportsTheFirstWrittenSequence` (`internal/engine/downloader_first_media_test.go`), `TestLiveTwitchChaptersVodFirstObservedFallback`, `TestStageTwitchChapters` and `TestPollTwitchCategoryNotesEachMinute` (`internal/worker/chapters_test.go`), `TestChaptersFromWatchPage` and `TestFetchChaptersFallsBackToTheDescription` (`internal/youtube/chapters_test.go`), and `TestChaptersFromDescription` and `TestSanitizeChapters` (`internal/utils/chapters_test.go`).

**Chat Files**

- Markers and the raw sidecar (YouTube, owner decision D16): `TestRawChatCopyFailureKeepsTheCapture`, `TestRecoveredRawChatCopyFailureKeepsTheCapture` in `internal/worker/chat_copy_failure_test.go`
- Pinned by `TestOrphanReplacementBecomesAMessage` and `TestHeldOrphanReplacementIsStoredAtTheEnd` (`internal/chat/orphan_replacement_test.go`).
- The derived chat and chapter files: Pinned by `TestOrphanSweepTreatsDerivedSidecarsLikeTheChat` and `TestDerivedSidecarsFollowTheParts` (`internal/worker/derived_sidecars_test.go`).

**v21**

- `TestMigrationV21UpgradesAV20Database`, `TestMigrationV21DevelopmentDatabaseKeepsWorking` (`internal/database/migrations_v21_test.go`).

**v23**

- `TestMigrationV23UpgradesAV22Database`, `TestMigrationV23Idempotent` (`internal/database/migrations_v23_test.go`).

**v24**

- `TestMigrationV24OwesTheResetToChannelsWithNotAStreamRows`, `TestMigrationV24Idempotent`, `TestResetMisfiledFeedItemsResetsTheWalksScopeOnce`, `TestResetMisfiledFeedItemsTakesTheUnresolvedArm`, `TestResetMisfiledFeedItemsReadsMembershipRowsWhileDiscoveryIsOn` and `TestDeleteChannelFeedDataTakesTheResetDebt` (`internal/database/migrations_v24_test.go`); through a feed cycle, `TestFeed_TheUpgradeProbesEachMisfiledRowOnce` and `TestFeed_TheUpgradesResetFollowsTheMembershipToggle` (`internal/monitor/misfiled_reset_test.go`).

**DpapiProfileDir**

- The value is only ever read, never launched against: `TestDpapiProfileDirNeverReachesALaunch`.
