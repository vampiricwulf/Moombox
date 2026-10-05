# Data and Storage

## Scope

This document specifies every data persistence layer in Moombox: the SQLite database (schema, connection tuning, the synchronous write path, pub/sub), the TOML configuration system (sections, types, migrations, validation), the cookie management subsystem (jar, refresh, auto-cookie), the logger (file rotation, ring buffer, pub/sub), and the on-disk file output conventions (staging, output templates, resume state, chat files). It is the authoritative reference for how Moombox reads, writes, and organizes persistent and transient data.

## Rules and Constraints

These are hard rules. An AI assisting with Moombox development must follow them without exception:

- **SQLite with WAL mode, 1 connection, 5s busy timeout, foreign keys on.** The DSN is `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)` — modernc.org/sqlite only honors `_pragma=...` parameters (the mattn-style `_journal_mode=...` form is silently ignored). Connection pool is `SetMaxOpenConns(1)` and `SetMaxIdleConns(1)`. SQLite is single-writer; do not change the pool size.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** The method accepts `map[string]any`, maps keys through `fieldToColumn` (51 entries), dynamically builds a `SET` clause, and auto-appends `updated_at` with the current UTC RFC3339 timestamp. After writing, it re-reads the full job row to notify subscribers with a complete `*Job` object. Returns the updated `*Job`.
- **`fieldToColumn` defines the allowed keys for `UpdateJobFields`.** Any key not present in this map is silently ignored. The map currently has 51 entries mapping Go field names to SQLite column names (identity mapping in all cases). Deliberately left out: `notification_msgs` (`UpdateNotificationMsgs` is its only writer), `channel_id` (set at insert — feed affiliation never changes, and a partial write of `""` would fake-empty a NULL), and the identity and bookkeeping columns `id`, `video_id`, `url`, `platform`, `created_at`, `updated_at`. Adding a new column that `UpdateJobFields` should be able to write requires adding a corresponding entry here.
- **`JobStatus` is `type JobStatus string`.** Status values are string constants, not integers or enums. Timestamps are ISO 8601 / RFC3339 strings. Optional numeric fields (sequence counters, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`).
- **Job writes are synchronous; there is no batching.** `UpdateJobFields` executes its `UPDATE` immediately under `db.mu`, re-reads the row in the same critical section, releases the lock and then notifies subscribers. There is no update channel, writer goroutine or coalescing window — the only goroutine the package starts is the `OnJobsChange` fan-out — so when nothing is being written, nothing runs and the database performs zero IO.
- **Config migrations are non-destructive.** `migrateOldFormat()` only applies a migration when the target section does not already exist in the TOML file. It never overwrites user-configured values in existing sections.
- **FlexDuration parses config values in each field's own unit.** A bare integer in `feed_check_interval` means minutes; in `hide_finished_age_days` days; in `probe_cooldown` seconds. Duration strings like `"10m"`, `"7d"` are parsed via regex and converted to the field's unit.
- **Schema migrations are versioned, idempotent, and forward-only.** Currently at v20 (`schemaVersion` in `internal/database/migrations.go`; `appendix-metrics.md` mirrors it). Each migration checks the current version before applying. Migrations run at startup in `Database.migrate()`, called from `Open()`. There is no rollback mechanism.
- **Cookie file format is Netscape.** The jar only loads cookies matching YouTube/Google domains or Twitch domains. Cookies are filtered to essential authentication cookies only.
- **Log file rotation uses numbered suffixes.** The current file is renamed to `.1`, existing `.N` files shift to `.N+1`, and excess files beyond `max_files` are deleted.
- **Resume state files are JSON sidecars.** Named `<output_file>.resume.json`, they store the last successful segment sequence number, bytes written, timestamp, base URL, and stream ID. Validated on load by IDENTITY (`resumeIdentityMismatch`: explicit StreamID first, then YouTube URL fingerprinting; opaque URLs with no identity — Twitch weaver — are deliberately TRUSTED) plus a file-size check, and cleared only on clean stream completion. Raw URL equality must NOT be used as the identity check: Twitch weaver URLs rotate every fetch, and URL-equality validation is what used to truncate hours of recording on every daemon restart.
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
4. Re-read the full job row via the prepared `stmtGetJob` in the same critical section (subscribers need all fields, not just the changed ones), then release `db.mu` — BEFORE notifying, so a subscriber can call back into the database.
5. Notify all `onJobUpdate` subscribers with the complete `*Job`, and all `onJobChange` subscribers with the job plus the list of columns written (`updated_at` excluded). If the read-back finds no row — deleted between the UPDATE and the SELECT — `notifyJobDeleted` fires instead.

**fieldToColumn map (51 entries):**

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
auto_retry_count, queue_priority, incomplete_tail, park_reason, park_identity
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

### Pub/Sub System

Six callback types (`internal/database/database_subscribers.go`):

| Callback | Signature | Trigger |
|----------|-----------|---------|
| `OnJobUpdate` | `func(*Job)` | After every `UpdateJobFields` write |
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

**Current version: 20** (`schemaVersion`, `internal/database/migrations.go`)

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
| description | TEXT | NULL | |
| output_file | TEXT | NULL | Absolute path to final output file |
| filename | TEXT | NULL | Basename only |
| output_directory | TEXT | NULL | Directory path |
| video_width | INTEGER | NULL | Pixels |
| video_height | INTEGER | NULL | Pixels |
| video_fps | INTEGER | NULL | |
| file_size | INTEGER | NULL | Bytes (int64 in Go) |
| chat_status | TEXT | NULL | `pending` / `downloading` / `finished` / `unavailable` / `incomplete`; the terminal value comes from the downloader's OUTCOME (`chatStatusForOutcome`, `internal/worker/orchestrator_chat.go`), not its message count — `incomplete` means the capture stopped short, or that finalize could not copy it beside the archive (`copyAssets`, `internal/worker/orchestrator_mux.go`): either way the archive lacks a whole chat, and the value is what makes the staging cleanup keep the capture rather than delete it. A part whose chat cannot be copied is not recorded at all, so its `seg_N` stays unmuxed and shielded until a retry. `pending` until the capture actually starts (both platforms; a Twitch VOD's download-slot wait included), then `downloading`. A user cancel settles a still-running value (`cancelledChatStatus`, `internal/worker/worker.go`): `downloading` becomes `incomplete` and `pending` is cleared; a shutdown leaves it for the capture to resume |
| total_chat_messages | INTEGER | NULL | |
| chat_filename | TEXT | NULL | Basename |
| chat_file | TEXT | NULL | Absolute path (added v2) |
| thumbnail_file | TEXT | NULL | Absolute path (added v3) |
| description_file | TEXT | NULL | Absolute path (added v3) |
| twitch_quality | TEXT | NULL | e.g. "1080p60" |
| twitch_category | TEXT | NULL | |
| channel_avatar_url | TEXT | NULL | |
| selected_video_itag | INTEGER | NULL | YouTube itag, -1 = audio-only |
| selected_audio_itag | INTEGER | NULL | YouTube itag |
| start_time | REAL | NULL | Trim start (seconds, float64) |
| end_time | REAL | NULL | Trim end (seconds, float64) |
| last_recheck_at | TEXT | NULL | RFC3339 |
| quality_preference | TEXT | '' | e.g. "1080p60", "best" (added v5) |
| watched | INTEGER | 0 | Boolean (0/1), watched status (added v8) |
| resume_position | REAL | NULL | Playback resume position in seconds (added v8) |
| chat_offset | REAL | 0 | Chat timing offset in seconds, can be negative (added v9, migrated from player_prefs) |
| auto_retry_count | INTEGER | NOT NULL, 0 | Monitor-driven Twitch flap auto-recovery attempts (added v13); reset by user-driven reinit/resume |
| channel_id | TEXT | NULL | Config channel ID of the monitor channel that created the job (added v16). NULL for manually added and pre-v16 jobs. Set at insert, never updated. |
| queue_priority | INTEGER | NOT NULL, 1 | Backlog marker (added v16): 0 = broadcast or newly discovered VOD (admitted immediately), 1 = backlog VOD (paced by the archive-slots scheduler) |
| incomplete_tail | INTEGER | NOT NULL, 0 | Boolean (0/1), added v17. Marks a `Finished` job whose recording is known to be missing tail segments (finalized behind head after the VOD-branch refresh loop's retries). Staging dir + resume sidecar are preserved instead of cleaned up; Resume is allowed on the flagged job (Retry is NOT — it deletes staging via ReinitializeJob, which would destroy exactly what the flag protects) and unconditionally rewrites this column, so a clean re-run clears it. |
| park_reason | TEXT | NOT NULL, `''` | Why the job stopped at `COOKIES?` (added v18). `'auth'` = the request was not signed in (cookies missing or dead); `'membership'` = the request WAS signed in and the platform still refused, so the account simply lacks the channel's membership; `''` = not parked, or parked before this column existed. Meaningful only while status is `COOKIES?`; every park path rewrites it and every un-park path clears it. Drives which recovery sweep may resume the job — see the `COOKIES?` -> `Upcoming` transition rules in `architecture.md`. |
| park_identity | TEXT | NOT NULL, `''` | Opaque fingerprint of WHICH account refused the job (added v19), recorded only for a `'membership'` park. A membership park resumes when the current account differs from this one — a durable comparison, so it survives restarts and cannot be consumed by a missed in-process transition. `''` means "parked under an unknown account" and resolves permissively (one retry, not a strand). Credential-derived: `json:"-"`, never serialized to clients. |
| notification_msgs | TEXT | NULL | JSON object added v20, mapping a notification target's key (first 16 hex digits of SHA-256 over the RESOLVED webhook URL) to the id of the one Discord message that target rewrites in place for this job (`mode = "edit"`). NULL for every job on a separate-mode install, which is the default. Written once per (job, target) on the first successful POST through `UpdateNotificationMsgs` — a silent single-column write that bumps no `updated_at` and wakes no subscriber. A corrupt or half-written value decodes to nil and the job simply posts a new message. Credential-adjacent: `json:"-"`, never serialized to clients. |

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
| id | TEXT | PRIMARY KEY (UUID) |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| start_time | REAL | Seconds |
| end_time | REAL | Seconds |
| filename | TEXT | Output filename |
| created_at | TEXT | RFC3339 |
| duration | REAL | Seconds |
| file_size | INTEGER | Bytes, nullable |

Index: `idx_trims_job_id(job_id)`.

**segments** (added v5) — one row per output *part* of a multi-part job.
Parts are produced by quality splits (resolution changed mid-stream, both
platforms) and by Twitch live gap splits (segments expired unrecoverably from
the CDN; each part file is internally gapless):

| Column | Type | Notes |
|--------|------|-------|
| id | INTEGER | PRIMARY KEY AUTOINCREMENT |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| segment_index | INTEGER | 0-based ordering within a job; stable across restarts (maps to staging dirs: root = 0, `seg_N` = N). Filenames use `segment_index + 1` as the part number |
| unix_start | INTEGER | Unix timestamp |
| unix_end | INTEGER | Unix timestamp |
| quality | TEXT | e.g. "1080p60" |
| filename | TEXT | Part filename: `{resolved template} - partN.mp4` (a job finalizing with exactly one part is renamed back to the plain `{resolved template}.mp4`) |
| file_path | TEXT | Absolute path, nullable |
| file_size | INTEGER | Bytes, nullable |
| video_width | INTEGER | Nullable |
| video_height | INTEGER | Nullable |
| video_fps | INTEGER | Nullable |
| duration_seconds | REAL | Nullable |
| chat_file | TEXT | (v15) Absolute path of this part's chat JSON, `''` when the part has no chat (pre-v15 rows, chat disabled, YouTube — only Twitch live IRC chat rolls per part) |

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
| status | TEXT | `unknown` / `upcoming` / `live` / `vod` / `not_a_stream` — last probe classification |
| first_seen | TEXT | RFC3339 — the cycle that first inserted the row (new-vs-backlog discriminator) |

**Indexes on feed_items:** `idx_feed_items_window(channel_id, published DESC, catalog_pos ASC, video_id ASC)` (the archive-scope read), `idx_feed_items_status(channel_id, status)`.

**channel_state** (added v16) — per-channel bookkeeping for the feed-history feature:

| Column | Type | Notes |
|--------|------|-------|
| channel_id | TEXT | PRIMARY KEY |
| backfilled_at | TEXT | RFC3339 — when the full-catalog backfill last completed; NULL = never backfilled (sweep re-queues) |
| backfilled_window_days | INTEGER | Window depth that backfill covered — a later, wider `archive_window_days` triggers a deeper rescan |
| backfilled_with_membership | INTEGER | Boolean — whether the membership tab was included; enabling membership later triggers a rescan |
| backfill_state | TEXT | Resumable scan cursor (JSON); cleared on completion or deliberate restart |
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

**Backlog flow:** backlog VODs only enter as `Queued` and are admitted to `Upcoming` by the archive-slots scheduler; broadcasts and newly discovered content never wait in `Queued`.

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

The database maintains in-memory per-job log buffers (`jobLogs map[string][]string`) for real-time log viewing in the Web UI and TUI. These are not persisted to SQLite.

- `RouteLogToJobs(line)` is the only writer: it scans the ROUTED SET of job IDs (`logRouted`, a second map beside `jobLogs`) and appends the line to the first matching buffer (substring match on job ID in log line). Each buffer is capped at 200 lines; when exceeded, it is trimmed to the last 100.
- `TrackJobForLogs(jobID)` starts routing to a job and initializes its buffer (nil slice).
- `UntrackJobForLogs(jobID)` stops routing to a job and KEEPS its buffer — the job that just failed is the one whose log an operator opens next.
- `SyncJobLogTracking(jobs)` applies both rules to a whole list: non-terminal jobs tracked, terminal ones untracked. The boot seed and the `OnJobsChange` fan-out both call it (`cmd/moombox/monitor_callbacks.go`), while single-job transitions go through that file's `syncJobLogRouting` — from `OnJobAdded` (the ZIP import really does add a `Finished` job) and from `OnJobChange` whenever the `status` column was written, which is what re-routes a job that LEAVES a terminal state: `/retry`, `/resume` and auto-retry each resurrect a job with a plain `UpdateJobFields(status=…)`.
- The per-line cost is a substring scan per TRACKED id — proportional to the number of LIVE jobs, not constant, and not to the size of the `jobs` table: ~99 ns at 5 live, ~76 µs at 5,000 live (the pre-fix cost, when every row the database had ever held was tracked). Live jobs are bounded by the archive slots and `num_parallel_downloads`; a long-lived tracked set is what must never come back.
- `PruneJobLogs(activeIDs)` removes buffers — and routing — for jobs no longer in the database.
- `ClearJobLogs(jobID)` removes a specific buffer and its routing.

### Auxiliary Data Operations

- **History:** `HasProcessed(videoID)` / `AddToHistory(videoID)` tracks previously seen video IDs (10,000 cap with LRU pruning).
- **Feed-history store** (`database_feed_items.go`): `UpsertFeedItem` (insert-or-update, reports whether the row is new), `ApplyProbeToFeedItem` (probe writes status/title/date back), `FeedScope` (the window + always-covered upcoming/live read), `SetFeedItemSource`, `RenumberCatalog`/`ListFeedOrderRows` (backfill ordering pass), `SaveBackfillCursor`/`LoadBackfillCursor`, `SetChannelBackfilled`/`GetChannelBackfill`, `SetChannelRSSOK`, `GetChannelEstablished`, `ListFeedChannelIDs`, `DeleteChannelFeedData` (channel-removal prune).
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

**Saves go back to the file that answered.** The location that was read is recorded in `LoadedFrom`
(`internal/config/types.go`), and `cmd/moombox/services.go` points the run's config path at it before
building the store — so a config found in `./config/` is written back to `./config/` rather than forked
into a fresh `./config.toml` that would shadow it on the next boot (the first write is often the
boot-time `NeedsAutoPersist` flush, so the fork used to happen without anyone touching a setting). When
NOTHING is found, the path that was asked for stays the target and the file is created there —
`storePathFor` in `cmd/moombox/helpers.go` is that rule (with no `-config`, `<cwd>/config.toml`).

**Out-of-range values are replaced, and said so.** `loadFromFile` runs `Normalize`, which puts each
value `Validate` rejects back to its default; the issues `Validate` found first are kept in
`NormalizedOnLoad` (`internal/config/types.go`) and boot logs one Warn per issue. The default is what
the next save writes, so without the line a hand-edited value disappeared from the file unexplained.

**Retired keys are ignored, and said so.** `retiredKeys` (`internal/config/config.go`) lists keys an
older file may still hold that nothing reads — today `downloader.po_token` and `downloader.visitor_data`,
which were saved as a "manual PO token override" no code path ever consulted (PO tokens are minted per
session through `[bgutils]`). `loadFromFile` records the ones present in `IgnoredOnLoad`, boot logs one
Warn per key, and the next save leaves them out of the file.

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
| PublicURL | string | "" | `public_url` | Externally reachable dashboard base URL. Empty means unset. Consumed only by the notification manager, which links a job embed's title to `{public_url}/#job=<id>`. Must be an absolute http(s) URL with a host and no query, fragment, or userinfo — validated by `ValidatePublicURL`, which also trims a trailing slash. An unusable value is reported and cleared, never substituted. Hot-reloadable — read at send time. |

#### [paths]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| DatabasePath | string | "./moombox.db" | `database_path` |
| LogFilePath | string | "./moombox.log" | `log_file_path` |
| OutputDirectory | string | "./output" | `output_directory` |
| StagingDirectory | string | "./staging" | `staging_directory` |
| FfmpegPath | string | "" | `ffmpeg_path` |

`ffmpeg_path` is hot-reloadable: a save from either UI calls `TrimService.SetFfmpegPath` and `DownloadWorker.SetFfmpegPath` (the orchestrator's muxer), so new trims, muxes, probes and part merges use the new binary; operations already running keep the muxer they started with.

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
| MaxVideoResolution | int | 2160 | `max_video_resolution` | Min 0; **`0` = unbounded**. Compares the SHORTER frame dimension, so `2160` recognises both 3840x2160 and 2160x3840. Resolves to the largest rendition at or below the cap, or the closest one ABOVE it when a stream offers nothing that small — it never leaves a job with nothing to download. One rule for all four selection sites (`CapDimension`/`SelectByCap`, `internal/utils/resolution.go`). Both UIs offer it as a preset picker (Unbounded/480p/720p/1080p/1440p/4K/8K plus Custom); the stored value is still the integer and there is no migration. |
| NumParallelDownloads | int | 10 | `num_parallel_downloads` | Min: 1. Peak concurrent VOD **jobs** across all channels — broadcasts never wait on the pool, so total concurrent downloads can reach (live streams) + this. Not to be confused with SegmentWorkers below, which gates concurrency *within* one download; a live broadcast's catch-up speed is governed entirely by SegmentWorkers, since this setting never applies to it. |
| SegmentWorkers | int | 12 | `segment_workers` | Min: 1, **no max**. Concurrent segment fetches within a single download (catch-up on a live DASH stream, parallel VOD HLS). Values above `config.SegmentWorkersWarnThreshold` (16) log a startup warning and are flagged in both UIs' help text: a wide simultaneous fan-out to YouTube is a traffic shape that attracts bot detection. Measured 2026-08-15 on a live stream mid-archive, at the pre-rewrite fixed 6-worker baseline: Moombox sustained 5.96 MB/s, against 2.86 MB/s for one `curl` connection and 11.28 MB/s for six parallel `curl` connections — the headroom the new configurable default (12 workers) and the rolling-window catch-up rewrite exist to close (a harness test of the same rewrite dropped 4.888s of batched catch-up to 0.84s). Not restart-required. |
| ReorderBufferMB | int | 1024 (256 on arm64) | `reorder_buffer_mb` | Min: 0, **no max**; `0` = unbounded. Megabytes of out-of-order segment data ONE download may hold in RAM while its head-of-order segment works through its retry ladder — the ceiling on `engine.reorderBuffer`, fed by both parallel paths (`runParallelCatchUp` on live DASH catch-up, `runHlsVodParallel` on VOD HLS). The head segment is admitted regardless, so a stalled head can put one segment over. This is the first config default that differs by platform: the value was originally chosen for an arm-class box, and owner ruling R3 (2026-09-24) scoped every arm-motivated cap to arm64 only — see `platformDefaults` (`internal/config/config.go`). Applied through `engine.ConfigureReorder` at boot and on every save; not restart-required. |
| ReorderBudgetMB | int | 4096 (1024 on arm64) | `reorder_budget_mb` | Min: 0, **no max**; `0` = unbounded. The same megabytes, summed across EVERY live reorder buffer in the process. Nothing bounded that sum before: at the per-job ceiling alone, ten concurrent VOD jobs could admit ten ceilings' worth of segments. A non-head segment waits on this ceiling as well as its own buffer's; the head is exempt from both, because nothing frees either without a flush and nothing flushes without its head. A `reorder_buffer_mb` above this budget is incoherent as written and is clamped to it at read time (`DownloaderConfig.ReorderLimitBytes`), with one warning naming both keys. Applied through `engine.ConfigureReorder` at boot and on every save; not restart-required. |
| ProgressIntervalMS | int | 16 | `progress_interval_ms` | Min: 1, **no max**. Milliseconds between one job's progress reports — `ProgressTracker.maybeUpdate`'s gate (`internal/worker/progress.go`), and therefore the upstream rate limit on both the WebSocket hub (which throttles nothing of its own) and the TUI's rows. Unlike the two reorder ceilings above, `0` is NOT a documented "unbounded" value: an ungated tracker would write the database once per arriving segment callback on every download at once, so anything below 1 resets to the default. The knob exists so 8 ms — about 120 reports a second, matching the TUI's 120 fps renderer — can be tried on a fast machine without a rebuild (owner ruling F1, 2026-09-25); each report costs one `UpdateJobFields` write and one fan-out, so lower values buy smoothness with CPU and disk. Snapshotted per job start into `JobConfig.ProgressInterval` by `buildJobContext` (`internal/worker/worker.go`), like MaximumTimeout and SegmentWorkers, so a save applies to the NEXT job and a running tracker keeps the interval it started with. Not restart-required, and absent from both restart-required lists because it has no Settings row: a config-file-only key by the same ruling, like DpapiProfileDir below — no control in either UI, no API setter, so in practice it is edit-and-restart with the edit made while Moombox is stopped. |
| DownloadChat | bool | true | `download_chat` | |
| Prefer60fps | bool | true | `prefer_60fps` | |
| MaximumTimeout | int | 600 | `maximum_timeout` | Seconds; YouTube livestreams. Min: 30 |
| InterruptionTimeout | FlexDuration | 120 (minutes) | `interruption_timeout` | Min: 0, no max. How long a live YouTube download's MaxTimeout-backstop finalize may keep deferring while `engine.SegmentDownloader.MayResume` reports the broadcast may still resume (`stallForPossibleResume`, `internal/engine/downloader.go`) — the interruption-resume design's Tier 1 stall. `0` disables the STALL only, not Tier 2 preservation: `attachMayResume` (`internal/worker/interruption.go`) installs `MayResume` unconditionally, and every live strategy site maps the config value through `engineInterruptionTimeout` before it reaches `engine.DownloaderOptions.InterruptionTimeout` — a positive value passes through as the ceiling, `0` (or a defensive negative) maps onto the sentinel `engine.InterruptionNoStall` (`-1`). `stallForPossibleResume`'s `InterruptionNoStall` branch still consults `MayResume` once per call and still latches `finalizedDuringInterruption` when it reports true, but always returns `false` — no stall, no clock. A parallel worker-side latch, `resumeWaitLatch` (fed by `noteRefreshFailure`/`resumeEvidence` in `internal/worker/interruption.go`), gives the same treatment to the `ErrQualityLost` refresh-failure path: evidence latches `incomplete_tail` even when `shouldWaitForResume` itself never permits an actual wait. So a `0` job never blocks finalize, but a genuinely-interrupted `0` job still finalizes with staging + resume data preserved exactly like an enabled one that gave up. Snapshotted per job start (`buildJobContext`), like `MaximumTimeout`/`SegmentWorkers` above; not restart-required. |
| IncompleteStagingExpiryDays | FlexDuration | 7 (days) | `incomplete_staging_expiry_days` | Min: 0, no max. How long the two EXPIRING staging shields hold. `jobNeedsStaging` (`internal/worker/orphans.go`) keeps a Finished job's staging out of orphan cleanup for FOUR reasons, and this window governs two of them: the job is flagged `incomplete_tail` (the tail is Resume-able), or its chat capture ended incomplete (`chatStatusIncomplete` — no verb re-pages from the capture kept in staging, since `/retry` refuses a Finished job and Reinitialize starts over, but it can be the only copy of those comments when the archive's chat copy failed, an aside recovery carries it beside the recovered recording, and the operator can take it by hand). Both lapse on the one age rule, `incompleteStagingExpired` (`internal/worker/orphans.go`). The other two shields carry NO age rule at all, because each holds captured media that exists nowhere else: staging still holding a recording the engine set aside rather than truncated (`stagedAsideRecordings`, `internal/worker/orchestrator_mux.go`) and staging still holding an unmuxed captured part (`hasUnmuxedSegmentParts`, `internal/worker/worker.go`, recoverable via the Mux action) are shielded until they are muxed or the job is deleted. Only the disk-heavy staging shield expires — the flag (the "may be missing its tail" badge) never does: YouTube cannot resume a broadcast days later, so aged interruption staging has no resume value, while the badge stays honest indefinitely. Age is measured from the job's `updated_at`, so any activity restarts the window; unparseable timestamps preserve. After expiry the staging becomes an ordinary orphan-scanner candidate; auto-resume's staging-existence gate then falls to the silent drop and manual Reinitialize remains the recovery. `0` = preserve forever. Read live per scan; not restart-required. |

#### [cookies]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| CookieFile | string | "./cookies.txt" | `cookie_file` | **Restart-required.** `AutoCookieService` is constructed from this once, at startup (`initServices`, `cmd/moombox/services.go`). Advice that names the file to replace — the auth alerts (`cookieFilePath`, `cmd/moombox/helpers.go`) and the worker's failed-refresh line (`CookieFileInUse`) — names the file the running services use (the jar's path), so a save without the restart cannot send the operator to a file nothing reads. |
| AutoEnabled | bool | false | `auto_enabled` | **Restart-required.** Owns exactly three things: the headless-browser periodic timer, the one automatic recovery attempt, and the `SetExpectedPlatforms` seeding at `cmd/moombox/main.go:276-278`. See §Auto-Cookie Service for the full settled meaning. |
| BrowserProfileDir | string | "./browser-profile" | `browser_profile_dir` | **Restart-required.** The directory's *existence* is not part of the start condition — `periodicRefreshHasSource` (`internal/cookies/autocookies_periodic.go`) asks per tick. |
| BrowserPath | string | "" | `browser_path` | Explicit browser override. Only a real override when paired with `browser_type` (`browserOverrideConfigured`, `internal/cookies/autocookies_browser_resolve.go`). |
| BrowserType | string | "" | `browser_type` | Which extraction backend applies to `browser_path` — Firefox `cookies.sqlite` vs Chromium CDP. Validated against `knownBrowserTypes` (`internal/cookies/browser_validate.go`). |
| Platforms | []string | [] | `platforms` | Platforms with verified cookies. Seeded by `detectCookiePlatforms` (`cmd/moombox/services.go`) — sidecar first, loose cookie-name predicates second — on the first boot that has a config file: during a first run the save would create config.toml and mark it loaded, skipping both setup wizards, so that write and `PersistPlatforms`' go through `Store.UpdateIfLoaded` (`internal/config/store.go`), which does nothing until the operator's first save. Nothing automatic ever prunes it; the sole removal path is an operator replacing the list through `PUT /api/config`. Its auth-loss consumer reads it once, at boot (`RefreshService.SetExpectedPlatforms` seeds the "previously authenticated" state before the first check), so a runtime removal changes that seed from the next boot; the running service already tracks each platform's real verdict from its own checks, and the status-bar fallback (`GetActivePlatforms`) follows the list at once. |
| ActivePlatforms | []string | unset | `active_platforms` | Explicit override for UI display; consumed by `config.GetActivePlatforms`. Unset (nil) means no override, and the display falls back to `platforms`, then to the enabled channels; an EMPTY list is an override too — both indicators off — so the field carries no `omitempty` (the TOML encoder skips nil on its own). The settings forms write it only once a toggle has been changed (or an override already exists): the toggles otherwise show the inferred answer, and saving that back would freeze it. Two persistence policies, both deliberate and pre-existing: both first-run wizards (`web/public/modules/setup.js`, `internal/tui/setup_wizard.go`) list a platform here on an ACCEPTED verdict — hedged verdicts included — while the `platforms` row above is fed through the `PersistPlatforms` callback (`internal/cookies/autocookies.go`) only on a VERIFIED one. The two lists answer different questions (what to display; what was proven); unifying them is an owner call nobody has asked for. |
| RefreshInterval | FlexDuration | 360 (minutes = 6h) | `refresh_interval` | Valid: 10-10080 minutes. Drives `AutoCookieService.StartPeriodicRefresh` (the browser timer) only — **not** `RefreshService`, whose interval is the hardcoded 30-minute default. **Restart-required**, in both lists: the ticker is built from this value once when the loop starts (`internal/cookies/autocookies_periodic.go`) and is never `Reset`, and `cmd/moombox/main.go` reads the same value to decide whether to start the loop at all — so a save changes nothing until the next start, and up to a week (10080 minutes) can pass before that is visible. |
| DpapiFallback | bool | false | `dpapi_fallback` | Windows-only. Opt-in: reads the user's REAL Chromium-family profile via `CryptUnprotectData` when the CDP refresh cannot acquire the managed profile. Not restart-required: `AutoCookieService.DpapiFallback` reads it live on every failed refresh. |
| DpapiProfileDir | string | "" | `dpapi_profile_dir` | Windows-only, and empty by default. Names a Chromium-family PROFILE directory for the DPAPI fallback to read, REPLACING the discovery walk rather than joining it — discovery knows eleven fixed `%LOCALAPPDATA%` `User Data` layouts, so a portable Chromium, a `--user-data-dir` profile and Opera are invisible to it. `dpapi.ValidateProfileDir` (`internal/cookies/dpapi/profiles.go`) checks the three structural facts the reader needs — an existing directory that is not a symlink or junction; a `Local State` that `ChromeLocalStatePath` can find *beside* it (Chromium's `User Data` root) or *inside* it (Opera's self-contained layout, the parent preferred so a stray copy cannot displace the real key); and `Cookies` or `Network/Cookies` inside — and a directory that fails them is an error rather than a fall-back to discovery, because falling back would answer "no profiles found under LOCALAPPDATA" about a setting the operator had just written. The same helper resolves the master key for `loadChromeMasterKey` (`internal/cookies/dpapi/dpapi_windows.go`), so validation and the reader cannot disagree. The launch-boundary deny-list (`dangerousProfilePathSubstrings`) is deliberately **not** applied here: it exists to stop a headless browser being *launched* against a real user profile, this value is only ever *read* (a `mode=ro` SQLite open plus a `Local State` read — pinned by `TestDpapiProfileDirNeverReachesALaunch`), and applying it refused a portable Chromium and every Opera while adding nothing, since the discovery walk reads the same real profiles with no deny-list at all. Relative values resolve against Moombox's working directory, like `paths.*`. **Not** restart-required by design: `AutoCookieService.DpapiProfileDir` reads it live at the start of every DPAPI pass, so a change applies to the next pass — though today nothing but a `config.toml` edit can change it (no UI control, no API setter), so in practice it is edit-and-restart, with the edit made while Moombox is stopped. `Validate` checks the path's shape only (no `..` traversal), never its existence — a container writes its config before the volume is mounted — and the boot-time verdict is a Warn from `LogDpapiProfileDirVerdict` (`internal/cookies/autocookies_dpapi.go`), never a boot failure; it fires off Windows, when `dpapi_fallback` is off (the default, so the likeliest case), and when the directory is unusable. |
| Acquisition | string | "auto" | `acquisition` | `auto` \| `profile`. Decides how a REFRESH acquires credentials: `auto` (a resolvable browser launches, a host with none imports the profile — the pre-existing rule), `profile` (never launch for a refresh; read `browser_profile_dir` read-only even on a desktop with a browser). Two values by ruling — the audit's `browser` behaved exactly like `auto` and was dropped. **Not** restart-required — `AutoCookieService.AcquisitionMode` reads it live. Composes with `auto_enabled`, which still owns whether a pass may launch at all. `StartSetup` never consults it. Absent or empty means `auto` and needs no migration: `Load` decodes over `Defaults()`. |

`cookie_file`, `refresh_interval`, `auto_enabled` and `browser_profile_dir` are the four cookie keys in `restartRequiredKeys` (`internal/tui/settings.go`) and in `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`); `TestRestartRequiredListsAgree` pins the two lists against each other. What `auto_enabled` does *not* need a restart for is the manual triggers — they read it live.

#### [disk]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| WarnPercent | int | 90 | `disk_warn_percent` | Valid: 1-99 |
| CriticalPercent | int | 95 | `disk_critical_percent` | Must be > WarnPercent |

#### [updates]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| AutoCheckUpdates | bool | true | `auto_check_updates` |

`runUpdateCheckLoop` (`cmd/moombox/helpers.go`) checks GitHub shortly after boot and then daily, each time
only while the flag is on — re-read on every check, so turning it off stops the schedule without a
restart. It also re-reads the flag every minute and checks at once when it has turned ON, so enabling it
at runtime no longer waits up to a day; reading the store covers every writer (both settings UIs, the
setup wizard, the update banner's dismiss).

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

#### [connectivity]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| ProbeTargets | []string | `1.1.1.1:443, 8.8.8.8:443, 9.9.9.9:443` | `probe_targets` | host:port TCP targets raced to detect internet reachability. Exposed in both settings UIs' Network section; `PUT /api/config` validates each entry with `net.SplitHostPort` and rejects an empty list. **Restart-required** (the monitor takes its targets at construction). |

#### [[channels]] (array of tables)

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| ID | string | "" | `id` | YouTube channel ID or Twitch username |
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
- SegmentWorkers: min 1, no max (values above `SegmentWorkersWarnThreshold`, 16, log a startup warning instead of failing validation)
- ReorderBufferMB: min 0, no max (`0` = unbounded)
- ReorderBudgetMB: min 0, no max (`0` = unbounded). The cross-check is NOT made by `validate` — a per-job ceiling above the process budget is legal config that `DownloaderConfig.ReorderLimitBytes` clamps at read time, with one warning, rather than a value rewritten on disk
- ProgressIntervalMS: min 1, no max. The one downloader key where `0` is an error rather than a documented "disabled" value, because an ungated progress tracker writes the database once per segment callback
- MaxVideoResolution: min 0 (`0` = unbounded; only a negative value resets to the default)
- MaximumTimeout: min 30 seconds (no maximum)
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
| `${start_date}` | Stream start time (or now) | Formatted as `YYYYMMDD` |
| `${start_time}` | Stream start time (or now) | Formatted as `HHMM` |

Sanitization preserves CJK, Japanese kana, and full-width characters via a regex allowlist. Those keep
three bytes a character, and Linux caps a name at 255 BYTES, so the byte caps are what keep a long
Japanese title from failing the finalize with ENAMETOOLONG: with the default template a capped title
leaves room for the id and the longest suffix a job writes beside its archive. No ASCII title reaches
the cap (YouTube allows 100 characters, Twitch 140). After expansion every path component that names a
Windows device (`CON`, `NUL`, `COM1`, … with or without an extension — `utils.IsWindowsReservedName`)
gets a leading underscore, on every platform, so a channel called `CON` no longer makes the
`${channel}/…` directory impossible to create on Windows.

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
4. **Admission is DOMAIN-FIRST**, and that ordering is the fix rather than a tidy-up. `isYouTubeDomain`/`isGoogleDomain` select the `youtube` jar, `isTwitchDomain` the `twitch` jar, and any other domain is dropped; only then is the name tested — `essentialYouTubeCookies` (or, on google.com only, the `SID`/`HSID`/`SSID`/`APISID`/`SAPISID`/`__Secure-1P*`/`__Secure-3P*` auth names) for the first, `essentialTwitchCookies` for the second. Under the old name-first rule a `.twitch.tv` row named `SID` was admitted, because that name is in `essentialYouTubeCookies`. Domain matchers are suffix-anchored (`domainMatches`, `internal/cookies/autocookies_merge.go`), so `.fakegoogle.com.evil.tld` is not google.com.
5. **Within one jar** a name can still arrive from several domains. `compareCookieDomains` (`internal/cookies/jar.go`) is a total order that settles it: youtube.com beats google.com, then fewer labels wins, then dot-prefixed wins, then lexically smaller on the stored domain string. A row is skipped only when the incumbent ranks strictly better, so any permutation of the same set of rows loads to the same jar. Rule 3 (dot-prefixed) is subsumed by rule 4 today and is kept deliberately — the agreement is an accident of ASCII, not of intent.
6. Both maps are swapped under ONE `Lock`, so no reader can observe the new YouTube rows beside the old Twitch ones. Accessors that need two values to agree take a single `RLock` to match (`GetTwitchCredentials`, `YouTubeIdentity`).

**Expiry is CAPTURED, never filtered.** `cookieEntry` (`internal/cookies/jar.go`) is `{value, domain, expiry}`; `loadFrom` parses Netscape field 5 with exactly `rowExpired`'s semantics (TrimSpace, ParseInt, 0 on a parse error), so "expired" means the same thing to the jar and to the merge. Nothing in the jar acts on it, and that disagreement is load-bearing: `mergeCookieFiles`/`rowExpired` (`internal/cookies/autocookies_merge.go`) remain the only pruner, and `RefreshCookiesDetailed` detects credential loss by comparing what the jar holds against what the merge produced. Make the two agree here and the signal vanishes; drop rows here and `GetCookieHeader` silently sends less. Expiry surfaces as a diagnostic only, and the two accessors are not equally shipped:

- `ExpiredAuthCookiesFor(platform, now)` has one production caller — the startup `Cookies loaded` line (`cookiesLoadedFields`, `cmd/moombox/services.go`), which emits it per platform as `expiredYouTubeAuth` / `expiredTwitchAuth`.
- `AuthCookieHorizonFor(platform)` and `TwitchLoginExpiry()` reach production through `CookieJar.HorizonLogFields()`, the single producer of `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` — ISO-8601 UTC, or `none` when there is no expiry to run out. Those three fields ride exactly TWO log lines: the startup `Cookies loaded` line and `refreshCookiesDetailed`'s `cookie refresh succeeded` line — the refresh's SUCCESS arm, and the only one of its three outcome-switch arms that both stands downstream of the write and the jar reload and credits this pass for it. The other two arms are downstream too but carry no horizon: `"cookie refresh verified one platform and lost another"` (a partial credential loss) and `"cookies still verify, but this pass could not confirm the browser refreshed the profile"` (the historically common Firefox outcome, where crediting the pass would be the unearned claim that branch exists to stop making) — so a refresh that lands in either of those logs no horizon at all, and the settling observation below cannot be made from it. Read against each other they are the settling observation for whether a browser refresh renews the Twitch `auth-token`. **No UI carries a horizon**: there is no badge, payload key or API field, and none is planned. `TwitchLoginExpiry` is its own accessor because `login` is deliberately outside `twitchAuthCookieNames` and stays outside it — that list drives an alarm, this is a diagnostic. Timestamps only, never a value.

Per-platform rather than YouTube-only because `RefreshService` has no Twitch refresh at all: `checkTwitchAuth` validates and never rotates, and nothing else writes a Twitch cookie back — `processYouTubeSetCookies` is YouTube-only and `trackedCookieName` refuses any origin off the Google platform. So the expired-count is the earliest warning a Twitch credential is running out.

Whether the browser path's twitch.tv navigation renews `auth-token` is **UNMEASURED** — see `platform-services.md §Twitch Authentication` for why there is no in-process keepalive and for the settling observation that would answer it.

**ENOENT-as-empty is a ruling, not an oversight.** A missing file loads as an EMPTY jar, both maps cleared (`Load`, `internal/cookies/jar.go`). Deleting `cookies.txt` is how an operator logs Moombox out, and keeping the last good session in memory until restart would make a deliberate delete do nothing observable. The race objection does not apply, and `Load`'s doc comment carries the derivation: every writer goes through `writeFileAtomic` (`internal/cookies/cookie_files.go`), which writes a temp file and renames without unlinking the destination; on Windows `os.Rename` is one `MoveFileEx(..., MOVEFILE_REPLACE_EXISTING)` with no `DeleteFile` ahead of it, and `os.ReadFile`'s open asks for `FILE_SHARE_READ|FILE_SHARE_WRITE` and *not* `FILE_SHARE_DELETE` — so a `Load` in flight makes the RENAME fail loudly instead of being made to read a missing file. On Linux `rename(2)` replaces the name atomically. Re-derive that paragraph if a writer ever stops going through `writeFileAtomic` or starts removing the target first.

**Any OTHER read failure records the path and the reason.** A `Load` that fails with anything but ENOENT — the compose `user:` uid mismatch is the likeliest container shape — leaves both maps ALONE (a failed read is no evidence that the credentials in memory are wrong) but still records `filePath`, because `Reload()` short-circuits on an empty one: until the 2026-09-17 sweep a failed boot load left it empty, so the 30-minute pass, every `SyncCookies` before an extraction and `twitch.Auth.Reload` all returned nil having read nothing for the life of the process, and repairing the permission on the host did nothing until a restart. It also records WHY, readable through `LastLoadError` (`internal/cookies/jar.go`): a bounded (200 bytes, cut on a rune boundary) sentence built from the path the caller passed in plus a `syscall.Errno` — never copied out of an error’s own message, so no producer can put a line of the file in a badge title. The narrowing to an errno is what makes that structural rather than incidental: `os.ReadFile` always fails with an `*fs.PathError` whose `Err` is an errno, so the realistic case keeps the platform’s own words (“permission denied”, “Access is denied.”), but `*fs.PathError.Err` is an `error` and `cookieJarReadFile` is a SEAM — an `os.Root`-rooted open, a decrypting reader or a network-mount shim would each bring their own error type with them, and any cause that is not an errno renders the fixed phrase “the file could not be read” beside the path. Empty for an ABSENT file, which is never-configured rather than unreadable. The refresh pass projects it onto `AuthStatus.CookieFileError` and from there onto both dashboards; before it existed, a `cookies.txt` sitting unreadable on the volume rendered on both of them as “you have not set cookies up”.

**Essential YouTube cookies (20):** SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, __Secure-1PSIDTS, __Secure-3PSIDTS, __Secure-1PSIDCC, __Secure-3PSIDCC, LOGIN_INFO, VISITOR_INFO1_LIVE, VISITOR_PRIVACY_METADATA, YSC, __Secure-ROLLOUT_TOKEN, CONSENT, PREF.

**Essential Twitch cookies (4):** auth-token, twilight-user, login, name.

**Two predicate tiers per platform.** The STRICT pair answers "is a complete working set present right now"; the LOOSE pair answers "was this install ever configured for the platform", which is the question the auth-loss gate and the status badges actually ask.

| Platform | Strict | Loose | Loose name set |
|----------|--------|-------|----------------|
| YouTube | `HasYouTubeAuthCookies()` — SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO | `HasAnyYouTubeAuthCookie()` | `youtubeAuthCookieNames` (10) — SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, LOGIN_INFO |
| Twitch | `HasTwitchAuthCookies()` — auth-token present | `HasAnyTwitchAuthCookie()` | `twitchAuthCookieNames` — auth-token, twilight-user |

A file holding SAPISID with LOGIN_INFO cleared is a CONFIGURED platform with BROKEN credentials — exactly the state worth reporting — and the strict predicate reads it as never-configured. Every name in a loose set must also be in the corresponding essential set or `loadFrom` drops it before the predicate can see it; `TestAuthCookieNameListsDoNotDrift` pins that.

**`login` is deliberately NOT in `twitchAuthCookieNames`**, and the reason is the alarm the list drives, traced end to end in that variable's doc comment: `HasAnyTwitchAuthCookie` → `refresh`'s `hasTWCookies` → `shouldFireRecovery` → `OnRecoveryNeeded("twitch")` → `runState.handleRecoveryNeeded` (`cmd/moombox/monitor_callbacks.go`). The alarm does not require a failed validate — `checkTwitchAuth` (`internal/cookies/refresh_twitch.go`) returns a conclusive `(false, nil)` **without issuing any request** when auth-token is absent, and `shouldFireRecovery`'s first-conclusive-check arm then returns `cookiesPresent` verbatim. A file holding `login` and no auth-token would therefore fire "twitch auth lost" on the first check of every start. `twilight-user` earns its place instead: it is Twitch's own record of the signed-in user, and `mergeCookieFiles` can prune a lapsed auth-token out from under it. That variable's comment also ENUMERATES the four silent Twitch states rather than claiming a superlative — the list has been wrong twice — and names which of them `ChatDownloader.noteMissingLogin` (`internal/twitch`) now reports.

**Key methods:**

- `GetCookieHeaderFor(platform)`: builds a `Cookie:` header from one platform's rows and no other's, pairs sorted by name for determinism. `GetCookieHeader()` is the YOUTUBE one — every production caller is a YouTube request path — so Twitch rows no longer ride along on authenticated youtube.com requests.
- `GetCookieFor(platform, name)`: one platform's value by name. `GetCookie(name)` reads the **Twitch** jar; the generic name is a deliberate mismatch, since its sole in-tree consumer is `internal/twitch/auth.go` fetching `auth-token`, and routing it to the YouTube jar would de-authenticate Twitch silently (IRC treats an empty token as "connect anonymously").
- `GetTwitchCredentials()`: returns `auth-token` and `login` as a pair under ONE `RLock`, because `internal/twitch/chat_irc.go` builds one IRC handshake out of both and a pair read under two locks is not a pair.
- `GenerateAuthorizationHeader(origin)`: SAPISIDHASH + SAPISID1PHASH + SAPISID3PHASH, each `SHA1(timestamp + " " + sid + " " + origin)` with one timestamp shared across all three (`makeSidAuthorization`). Returns "" for any origin outside `allowedSAPISIDHASHOrigins`, which is defence in depth — Google's auth uses the origin as a shared secret, so a caller must never be handed a valid hash bound to an attacker-supplied one.
- `YouTubeIdentity()`: SHA-256 over `SAPISID + NUL + LOGIN_INFO`, "" when either is missing — SAPISID falling back to `__Secure-3PAPISID`, with that fallback inlined rather than delegated to `GetSapisid` so both reads happen under ONE `RLock` (the two must be kept in sync by hand). An opaque equality token for "which Google account is this" — never a credential, never displayed. LOGIN_INFO is the load-bearing half: SAPISID identifies a SESSION, not an account, so a fingerprint over it alone would be blind to an account switch. The rotating `__Secure-*PSIDTS`/`SIDCC` names are excluded because they would fire on every refresh cycle.
- `TwitchIdentity()`: SHA-256 over `auth-token + NUL + login`, both read under ONE `RLock` for the reason `GetTwitchCredentials` documents. `""` means **no Twitch credentials at all** — deliberately NOT `YouTubeIdentity`'s "either half missing" rule. The question here is "is this the same credential PAIR a downgrade was observed under", and a token with no `login` beside it is one of the four downgrade routes rather than an unanswerable state: folding it to `""` would make the operator's fix, adding the `login` row, compare equal to the breakage it replaced. A token rotation therefore reads as a change, which is the cheap direction — one re-check and one IRC reconnect that the credentials pass.
- `Reload()`: re-reads from the same file path; a no-op when the jar came from no file — which, since the failed-read arm records the path, no longer happens after a boot read that failed. Since the
  2026-09-15 sweep (Arc 3), `Load` (`internal/cookies/jar.go`) memoises the file's `(size, mtime)` pair
  only when the stats before and after the read agree, the file is ≥ 2 s old and no concurrent install
  landed after it; a later `Load` whose stat matches the recorded pair skips the re-parse.

**Thread safety:** All methods are protected by `sync.RWMutex`. Nil-receiver-safe where a caller may legitimately hold none (`HasAnyYouTubeAuthCookie`, `HasAnyTwitchAuthCookie`, `ExpiredAuthCookiesFor`, `AuthCookieHorizonFor`, `TwitchLoginExpiry`, `YouTubeIdentity`, `TwitchIdentity`).

### Refresh Service

`RefreshService` periodically validates cookies against the actual platform APIs and refreshes YouTube session cookies from `Set-Cookie` headers.

**`accounts.google.com/RotateCookies` is not a YouTube keepalive, and is not to be re-proposed** (verified and rejected 2026-08-25). The endpoint exists and answers, but it rotates only `__Secure-1PSIDTS` / `__Secure-3PSIDTS`, which YouTube's Innertube auth never reads: yt-dlp's auth predicate is `LOGIN_INFO` plus one of the `*APISID` cookies, `SAPISIDHASH` self-refreshes from a static cookie and a timestamp, and the jar's own identity fingerprint excludes exactly those two names because they rotate constantly. Ordinary page traffic already rotates whatever rotates, so a scripted rotation would add a second writer to a session that dies precisely because a browser elsewhere rotated it first — the remedy is isolation (the managed setup profile as the sole rotator), not more rotation. `/RotateBoundCookies` (DBSC) is the TPM-signed successor and unreachable from Go regardless.

**Lifecycle and the single-flight.** Three entry points share one body, `refresh(ctx, allowFallback)` (`internal/cookies/refresh_pass.go`); `allowFallback` is the only thing that separates them.

- `Start(ctx)` runs the initial pass **synchronously on the caller's goroutine** with `allowFallback=false`, wrapped in its own inline `recover` — `cmd/moombox`'s `run()` blocks on it before the web server binds, so an unrecovered panic there takes the process down at boot with no dashboard, no TUI and no log surface. It then launches the ticker goroutine, which carries the same recover.
- `CheckNow(ctx)` is `POST /api/cookies/recheck` and the TUI's `R C`, also `allowFallback=false`: it runs on a handler goroutine and must not buy a full page fetch.
- `doRefresh(ctx)` is the ticker, and the only path allowed `allowFallback=true`.

All three single-flight on `RefreshService.refreshInFlight` (guarded by `rs.mu`). A second caller is a **no-op** that returns `started=false` and logs at Debug — it does not queue and does not wait. It is **never** a `RefreshDeclinedCauses` member: that vocabulary belongs to `AutoCookieService`'s browser refresh and is pinned across three consumers. A dropped ticker tick waits a full interval rather than doubling up. A caller that has just rewritten `cookies.txt` and wants *that file* re-verified cannot be given that guarantee — the in-flight pass may have read the old file — so every caller in that position logs the skip at **Info**, through one helper — `recheckAfterCookieWrite` (`cmd/moombox/monitor_callbacks.go`), which says "status may lag until the next refresh" and names the gesture. Its callers are the post-recovery re-check (hoisted above `runCookieRecovery`'s verdict switch and gated on `RefreshResult.Ran`, so a pass that ran and FAILED is re-read too — it moved the credential fingerprint just as a working one would), the post-`R F` re-check, the worker's job-triggered refresh (`OnCookieRefreshNeeded`), the TUI setup wizard's finish (gated on `SetupResult.Wrote`, the setup path's counterpart to `Ran`, since a wizard finish reports `SetupResult{}` on every error path and there is otherwise nothing to gate on), and the TUI's `E I` cookie-file import (gated on `ImportResult.Wrote`, the same flag the Web import route gates its own re-check on — `ImportCookies` is the fifth writer of `cookies.txt` and this is the `cmd/moombox` half of its "the caller runs the re-check" contract). Two credential writers share one further injected seam — `AutoCookieService.OnPassCompleted`, fired by both `StartPeriodicRefresh`'s tick and `StartProfileSeed`'s boot import, each gated on `Ran` — and that seam's body is wrapped in `postRefreshRecheckHook` (`cmd/moombox/services.go`) for its own recover, so a panic there costs only the re-check. Both cookie timers (this one and `RefreshService`'s ticker) also run each tick under its own recover (`runPeriodicTick`; the ticker's inline wrap), because their goroutines recover outside the loop and an escaping panic would otherwise end the timer for the life of the process rather than costing one tick. The three `internal/web/routes` callers — the dashboard/Settings browser refresh, the setup wizard's finish and `POST /api/cookies/import` — call `CheckNow` bare, because that package has no operational logger. The wizard finish and the import are additionally DETACHED onto their own 45-second timeout rather than the request's context, and both answer with an explicit `Content-Length` (`jsonResponseSized` / `jsonErrorSized`, `internal/web/routes/cookies.go`) before flushing, so a client that navigates away can neither cancel the fingerprint comparison its own write caused nor be made to wait out a re-check it has already been answered for. The length is the load-bearing half and was missing until 2026-09-17 (owner decision O-L): with no `Content-Length` net/http falls back to chunked encoding and writes the terminating chunk only when the HANDLER returns, so the flush released the headers and `fetch().json()` — which awaits the body — sat through the entire re-check, inside a 60-second dialog budget that also has to cover `FinishSetup`. Every exit written inline in either handler literal goes through those two writers, success and refusal alike, and each keeps the trailing newline `json.Encoder.Encode` wrote so no body's bytes moved. `writeBrowserReadError` and `readCookieImportBody`'s refusals stay unsized, deliberately: both answer before the deferred re-check exists, leaving `Wrote` false, so no client is waiting on a pass behind them. They are safe under `CompressionMiddleware` only below its 1024-byte `gzipMinSize`, where `commitPlain` sends the identity body and leaves the header alone; above it `startGzip` deletes `Content-Length` and re-chunks, and the gzip trailer is written after the handler returns — accepted rather than exempted because the widest body either handler produces is a ~282-byte import success (three maps: the status snapshot, the relogin map and `activePlatforms`). The re-check stays BLOCKING on the handler goroutine rather than being detached: that is the property the AST call-site test protects, and a goroutine would satisfy the test while deleting it. Each is gated on the pass having written (`SetupResult.Wrote` / `ImportResult.Wrote`), which is true on the jar-reload error exit as well as on success — the one error path that runs over a file already replaced, and the one where the re-check is worth most. **Every gesture that can write `cookies.txt` ends in one of these**, and that is a requirement rather than an observation: `refresh`'s status block is the only place the Twitch credential fingerprint is compared and the auth mark cleared, so a writer that reaches no pass is invisible until the ticker. The `POST /api/cookies/recheck` handler ignores the bool on purpose — its payload is a status snapshot, not a claim that this request produced it. Before the guard existed, a manual recheck landing during a ticker pass produced two guide fetches, two `Set-Cookie` merges and two interleaved `updateCookieFile` rewrites of the same file. An operator counting passes from clicks will therefore occasionally see a recheck produce no new pass in the log; that is the guard, not a broken button.

**Every `rs.mu` section inside `refresh` releases through `defer`.** This is a standing rule, not a style preference. The guard-release defer takes `rs.mu`, and `rs.mu` is a plain non-reentrant `RWMutex`: a panic unwinding with the write lock held would block that defer forever, park the goroutine holding `rs.mu`, and turn a loud crash into a silent hang in which every later `GetStatus()` blocks. The status update is scoped into a func literal for exactly that reason. Two unexported test seams, `refreshPassHook` (outside the lock) and `refreshLockedHook` (inside it), exist because the two windows need opposite things.

**Validation endpoints:**

| Platform | Method | Endpoint | Auth Check |
|----------|--------|----------|------------|
| YouTube | POST | `youtubeGuideURL` = `www.youtube.com/youtubei/v1/guide?prettyPrint=false` | Explicit login marker in the response, or inconclusive — see below |
| Twitch | GET | `twitchValidateURL` = `id.twitch.tv/oauth2/validate` | HTTP 200 = valid, 401 = conclusively invalid, anything else = inconclusive |

There is **one** guide URL. Two package vars used to name it — `youtubeGuideURL` and `youtubeGuideRefreshURL`, described as different endpoints kept apart on purpose — and they were byte-identical; folding the two guide functions into one exchange made the duplicate visible. Both vars (rather than consts) exist solely so tests can point them at an `httptest` server.

Provenance is asserted before status and before body, on both platforms: `authResponseIsOurs` (`internal/cookies/refresh_youtube.go`) requires the answering request's scheme and raw `host:port` to match what was sent AND the credential header (`Cookie` for YouTube, `Authorization` for Twitch) to still be present. Go strips manually-set credential headers on a cross-hostname redirect and the decision is sticky, so `origin → wall → origin` lands back on the right host carrying an uncredentialed body. The check is non-vacuous only because both callers refuse the empty-credential case before fetching, and it means anything only because `cookiesHTTPClient` carries no `http.CookieJar` — `TestCookiesHTTPClientCarriesNoCookieJar` pins that.

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
- `checkAndRefreshYouTube` — the sole writer. Only on an authenticated, readable reply does it call `processYouTubeSetCookies`. Every error path and the never-configured gate return a nil response, so "a reply we could not read is not a reply anyone may write the jar from" is a fact about the return values rather than a rule to remember.

`youtubeGuideExchange`'s three entry gates encode one rule, and only the FIRST may answer `(false, nil)`: nothing configured at all is a silent negative; configured-but-no-request-could-be-built errors with `ErrAuthCheckNotAttempted`, because a check that did not happen is not dead credentials. A jar with SAPISID and a cleared LOGIN_INFO is configured with broken credentials and its verdict has to come from YouTube.

**Set-Cookie ADMISSION, by parsed attributes.** `admitSetCookie(sc, origin)` (`internal/cookies/refresh_cookiefile.go`) is the outer layer: a header it turns down never reaches the write path in any form. The substring pre-filter that used to open this loop is **gone** — it ran before `Domain=` was parsed, dropped every legitimate unscoped first-party rotation (RFC 6265 §4.1.2.3 host-scopes a Domain-less `Set-Cookie` to the responding host), and was never the guard it looked like, since `x=youtube.com; Domain=evil.tld` passes a substring test on its value. Admission now happens after the whole attribute list is read:

1. **Row-breaking characters** in the name, the value or the domain are refused outright (`hasRowBreakingChar`, `rowBreakingChars` = tab, CR, LF, NUL). Only the TAB is a live vector — `net/textproto` cannot deliver CR, LF or NUL inside a header value — and the other three are defence in depth for a write into a line-oriented file. This governs only what *this writer* may add; `CookieJar.Load`'s tolerance of tab-carrying rows is unchanged. (Pre-existing interop, out of scope and named: a row that already carries a tab inside its VALUE loads here but is rejected by yt-dlp's `MozillaCookieJar`, which requires exactly seven fields — a shared `cookies.txt` silently loses that row on the yt-dlp side.)
2. **SCOPED** (`Domain=` present): admitted only when the domain lies on the declared origin's credential platform (`cookiePlatformOf`). `accounts.google.com.evil.tld`, `evil.tld` and a bare `.` are all refused; the emptiness test is load-bearing, since an undeclared origin has no platform and bare equality would read `"" == ""` as a match.
3. **UNSCOPED** (no `Domain=`): the key keeps `Domain: ""` and stays host-scoped to the declared origin. Admitted only under a name the jar actually tracks — `trackedCookieName`, the union of `essentialYouTubeCookies` and `isGoogleOnlyAuthName`, neither of which contains the other — so an unscoped `foo=bar` never enters the file. `trackedCookieName` also refuses outright when the declared origin is not on the Google platform (`origin.platform() != originYouTube.platform()`), so only a Google-platform caller can admit an unscoped header at all today; adding `essentialTwitchCookies` there is a one-line change the day a Twitch caller arrives, and it needs that caller's tests rather than a guess.

Parsing details that are decisions rather than incidentals: leading and trailing SP/HTAB are trimmed from the name and the value **separately**, per RFC 6265 §5.2 step 3 (`strings.Trim`, not `TrimSpace`, so a stray CR cannot be quietly rescued past step 1). Quoted values are **not** de-quoted — §5.2 never strips DQUOTEs and no browser does; CPython's `SimpleCookie` strips because it implements the older RFC 2109. `Domain=` is lowercased at parse, not at comparison, because it becomes a map key. `Max-Age` takes precedence over `Expires` and is clamped to `maxCookieLifetime` (400 days, RFC 6265bis §5.5), which also makes the `now + maxAge` int64 overflow unreachable — a wrapped negative expiry reads as "not expired" to every `exp > 0 && exp < now` guard in the package. `Expires` is deliberately *not* clamped. Every attribute is read to the end of the header; the old loop broke out early on `Max-Age<=0` and threw away the `Domain=` that usually follows it.

**Deletion semantics are CPython's**, from `http.cookiejar._cookie_from_cookie_tuple`: `Max-Age<=0` or an `Expires` at or before now deletes the row, keyed by domain+path+name, rather than storing it with an empty value. A bare `NAME=` with no expiry attribute is **REFUSED**, not treated as a third deletion form — this package cannot represent an empty-valued row at all (`CookieJar.Load` TrimSpaces the line, the trailing tab disappears, and the row reads as 6 fields and is skipped as malformed), and a reply that asserts "you are signed in" while blanking the credential that proves it is self-contradictory. `updateCookieFile` logs that refusal at Warn when the row carries an essential cookie.

**What an admitted header may do, by verb.** `admitSetCookie`'s doc comment is the authority here and states the rule in full; the summary below must agree with it. The design comes first, because the enforcement rules read wrongly in isolation — the owner's rule, verbatim: *"youtube cookies should allow Google cookies as well."* YouTube and Google are ONE credential platform (`cookiePlatformOf`), `.google.com` and `.youtube.com` rows live in one jar keyed by bare name, so a youtube.com reply is ENTITLED to move Google rows and does. The one thing declined is MISATTRIBUTION: a host-only cookie from www.youtube.com carrying no `Domain=` is a youtube.com cookie, so a NEW row for it goes on `.youtube.com` and is not invented on `.google.com`. As *observed*, Google's own cookies carry an explicit `Domain=` — that is a fact about Google's servers, not an invariant this code enforces — so nothing real is caught by that rule; the retired branch that guessed `.google.com` from the cookie NAME was minting a different cookie under a real one's name.

The three verbs have three different scopes, each enforced somewhere else:

| Verb | Unscoped header | Scoped header | Enforced by |
|------|-----------------|---------------|-------------|
| **REFRESH** | May rewrite an existing same-name row anywhere inside the declared origin's PLATFORM — an unscoped `SID=fresh` from a youtube.com reply *does* rewrite an existing `.google.com SID` row | Same fan-out, on the same rule: a `Domain=.google.com` rotation also repairs a stale `.youtube.com` twin, **when it is the only candidate** | `resolveRowUpdate` rule 2 (`origin.covers`) for the origin's own site, rule 3 + `sameCookiePlatform` for the rest of the platform. Rule 3 DISAMBIGUATES rather than guessing: it fires only when exactly one non-deleting candidate qualifies (`refreshes == 1`), so two same-name updates decline rather than let map order pick |
| **CREATE** | Only on the declared origin's own SITE — the insertion loop derives `domain = "." + string(origin)` and nothing else contributes | On the domain it declared, whenever that domain is inside the origin's platform. An admitted `SID=x; Domain=.google.com` from a youtube.com reply DOES create a `.google.com` row against a file holding none, and that is correct | `updateCookieFile`'s insertion loop, plus its platform guard (`cookiePlatformOf(domain) == origin.platform()`) |
| **DELETE** | Only inside the declared origin's own SITE, through `resolveRowUpdate` rule 2 alone — rule 1 needs a `Domain=`, rule 3 skips deletions, and the insertion loop skips them too | Rule 1's, reaching only rows whose domain it exactly scope-matches (`sameCookieScope`) | `resolveRowUpdate` |

Unscoped CREATE is narrower than unscoped REFRESH because a rewrite repairs a row the FILE has already asserted belongs to this platform, while a creation has no such prior assertion to lean on. Narrower still for one batch shape: an unscoped key is not INSERTED beside a scoped NON-deleting sibling of the same name (`hasScopedSibling`), because the scoped header has already claimed a row and the unscoped twin would override it by name in the jar. A scoped DELETION is not such a sibling — delete-plus-insert is "replace", and counting it would eat the replacement. The wider rule ("once any key of this name matched, treat every key of that name as handled") is wrong: a response rotating `SID` on both domains against a file holding only the `.google.com` row must still insert the `.youtube.com` one.

**The write path.** `processYouTubeSetCookies` declares `originYouTube` twice — once to `admitSetCookie`, once to `updateCookieFile` — because a Domain-less `Set-Cookie` is host-scoped to a response that no longer exists by the time the updates reach the writer. `cookieOrigin` is a SITE (`youtube.com` / `google.com` / `twitch.tv`), and it feeds three decisions: `resolveRowUpdate`'s rule 2, `sameCookiePlatform`'s Domain-less default, and the insertion loop's platform guard. The zero value is deliberately inert — `covers` reports false for every row and the insertion loop refuses everything — because declining to MATCH is not declining to WRITE: every update the matching rules turn down arrives at the insertion loop, which without an origin check appended new rows under a domain nobody declared.

Other write-path rules (`updateCookieFile`, `internal/cookies/refresh_cookiefile.go`):

- **Every** matching row is rewritten, not just the first, so multi-domain duplicates do not drift out of sync.
- Rows are rebuilt from exactly the first seven fields of the row being replaced, with the new expiry and value substituted. A live row can arrive split into 8+ parts (a value containing a tab), and assigning `parts[6]` left the old tail dangling.
- `#HttpOnly_` is **preserved** on rewrite (`parts[0]` is emitted verbatim — the file's own row is the authority) and **added** only on insertion, from `cu.HTTPOnly`. Nothing in the package treats the flag as a control, so a server that starts or stops sending `HttpOnly` costs a stale annotation, not a downgraded cookie.
- The include-subdomains flag is derived from a leading dot rather than hardcoded TRUE; `secure` follows a `__Secure-` name prefix.
- Deletions remove the row. Grow broadly, destroy narrowly: a deletion scoped to `.youtube.com` does **not** remove a host-only `www.youtube.com` row, even though RFC 6265 domain-matching covers it — browser extraction really does write host-only rows, and a stale row that keeps being sent is recoverable where a deleted credential is not.
- A read failure aborts (`ErrCookieFileUnreadable`, `internal/cookies/errors.go`): an unreadable `cookies.txt` is not an absent one, and consumers MUST discriminate it from every other failure, because the correct instruction is "fix the permission or mount problem", never "replace cookies.txt".
- The write ends in `writeFileAtomic`. A failure Warns and names the deployment mistake that causes it: a rename cannot replace a single-file bind mount, so `cookies.txt` belongs *inside* a mounted directory, not mounted as an individual file.
- Refused headers are counted, never logged. A `Set-Cookie` is the credential itself.

**Auth loss detection.** The service tracks previous auth state per platform. `shouldFireRecovery(everConcluded, prevAuth, nowAuth, checkErr, cookiesPresent)` is a pure function (table-testable without a network seam) and fires in two cases, both requiring `checkErr == nil && !nowAuth`: a **witnessed transition** (this platform concluded before and was authenticated then), and **startup dead-auth** (this platform's first conclusive check ever), the latter gated on `cookiesPresent`. `everConcluded` is per-platform (`ytEverConcluded`/`twEverConcluded`) and not the service-wide `hasCheckedOnce`, because `SetExpectedPlatforms` seeds `hasCheckedOnce=true` as soon as ANY platform is in the persisted list — using the shared flag would let one platform's presence mask a sibling that was never checked. `cookiesPresent` is the LOOSE predicate: a half-cleared session is a configured platform with broken credentials.

"Conclusive" is the three-outcome rule above, not merely "no network error": a non-200, an answer from the wrong host, and a 200 whose body carries no recognisable marker are all inconclusive too, and none of them fires recovery, moves the previous-auth baseline, or marks the platform concluded.

`SetExpectedPlatforms(platforms)` seeds the previous auth state from persisted config platforms so auth loss is detectable after a restart. `OnAuthRecovered` fires on the inverse transition. `OnCredentialsChanged` is separate and not a weaker `OnAuthRecovered`: a job parked because the signed-in account lacks a membership parked while auth was HEALTHY, so swapping accounts produces no auth transition to ride. It fires for BOTH platforms, against per-platform baselines (`prevYouTubeIdentity` / `prevTwitchIdentity`), and the two mean different things: a YouTube fire is "the signed-in ACCOUNT may have changed", a Twitch fire is "the credential PAIR changed". The Twitch fire has a second subscriber — `cmd/moombox` broadcasts it to every live Twitch IRC chat downloader through `DownloadWorker.ReauthenticateTwitchChats`, which tells each one to re-read its credentials and reconnect (the count is downloaders TOLD, never "authenticated"). `OnAuthRecovered` broadcasts the same thing on its own edge, which is what a capture already in flight needs for the case this fire cannot see: a transient refusal that heals with the fingerprint UNCHANGED fires no `OnCredentialsChanged` at all. `shouldObserveCredentials` and `advanceIdentityBaseline` (both pure, both platform-agnostic) govern both — the baseline advances only on a check that was conclusive **and** authenticated, so a stale intermediate export cannot consume the edge, and the `baseline == ""` case fires once per process on purpose so an offline cookie swap is noticed at all.

**Two-tier liveness, and its pilot is ARMED.** Tier 1 is the auth check above. Tier 2 is `ObserveLiveness(platform, loggedIn)`, fed by three producers — two YouTube: the per-channel membership probe (every un-memoized channel each feed cycle, plus ONE nominated memoized non-member — the non-member memo's TTL is `membershipMemoTTL` in `internal/monitor/feed.go`, 6 h, and `armMembershipLiveness` nominates the memoized channel with the earliest horizon **that has not recently errored** (`membershipFetchErrored`, `internal/monitor/feed.go`) so the signal keeps firing on an install where every channel is memoized — when every candidate has errored the earliest errored one is tried anyway, bounded by `membershipLivenessMaxTries`) and the channel-independent `FallbackLiveness` probe injected by `cmd/moombox` (this package cannot import `internal/youtube`) — and one Twitch, below. Callers must filter their own inconclusive results out; reaching the method means "the platform told us", not "we asked". Twitch's producer is the channel-independent `TwitchFallbackLiveness` probe, injected by `cmd/moombox` for the same reason and one more — `internal/twitch` imports `internal/cookies`, so the call is an import cycle in either direction. It asks `internal/twitch.Service.ProbeSessionLiveness` for a playback access token on one enabled configured Twitch channel (the first in config order) and reads `user_id` out of the token document (`PlaybackTokenSession`) — the question `checkTwitchAuth` cannot answer, because `oauth2/validate` returns 200 for a token that is valid but no longer entitled to authenticated playback. It runs under the YouTube twin's three conditions plus one: the jar must hold an `auth-token` right now (`HasTwitchAuthCookies`, the narrow predicate), because the probe SENDS that token and an install without it would get an anonymous playback token by design. A 401/403 is INCONCLUSIVE, not signed-out — `gqlRequest` raises `ErrTwitchAuthExpired` for both statuses and 403 is an edge block as often as a credential verdict. Nothing on this path writes `AuthStatus`; Arc 10's capture-time mark (`NoteTwitchAuthLoss`) remains the only direct writer outside `doRefresh`'s own status block.

`const livenessRecoveryArmed = true` (`internal/cookies/refresh_liveness.go`) gates whether a tier-2 verdict may invoke `OnRecoveryNeeded`. It is true since 2026-09-03, armed by owner ruling with the five-day pre-arming soak skipped by that same ruling. A signed-out verdict that clears the per-platform dedupe now logs `a liveness observation reports this platform is signed out, triggering recovery` at Warn and calls `OnRecoveryNeeded(platform)`; the observation, the dedupe and the freshness accounting happen exactly as before, and `ObserveLiveness`'s single log line still carries `wouldFireRecovery` and `armed` (now `armed=true`), so the same reading rules apply to a run that acts. The risk it accepts is not scoped to `auto_enabled` installs: on `auto_enabled = true` a spurious verdict drives a headless browser and notifies unless it races the auto-cookie single-flight; on `auto_enabled = false` there is no quiet case at all — `handleRecoveryNeeded` returns without calling the refresher, so a synchronous "Cookie Re-Authentication Required" (TypeError) fires every time, at a 30-minute per-platform cooldown, to the population least able to reach the remedy it names. It is a source constant, not a config flag: the only way back from a wrong verdict is to set it to `false` and rebuild.

Arming was an owner decision, taken 2026-09-03 and not a change made in passing. One question it had to answer first — what a genuinely dead session should do once the gate is true — was decided on 2026-08-29 and was BUILT before the flip. Tier 1 alone notifies once per process — `shouldFireRecovery`'s witnessed-transition arm needs `prevAuth` to have been true, and the first conclusive negative clears it — whereas an armed tier 2 would otherwise have re-fired every flat `livenessRefireWindow` for as long as the session stayed dead: 48 notifications a day for one loss. The owner ruled a back-off instead, and `recordLiveness` (`internal/cookies/refresh_liveness.go`) is where it runs: the first re-alarm lands one `livenessRefireWindow` (30 min) after the first alarm, `escalateLivenessRefire` doubles the window (`livenessRefireFactor`) on every alarm after that up to a `livenessRefireCap` of 24 hours, and `livenessRefireWindowFor` is what `recordLiveness` consults instead of the flat constant. A conclusive signed-in observation calls `resetLivenessRefire`, which puts the platform back on the base — the escalation only: `lastRecoveryDecided` is left standing, because clearing it would let a healthy verdict from one channel swallow a dead one from the next channel in the same cycle. A tier-1 fire (`noteRecoveryDecided`) stamps the dedupe without escalating, so a platform is never pushed onto a longer schedule by the check that predates this signal. The schedule was mutation-tested before the flip and is now what governs a real re-alarm: `recordLiveness` computes it, `ObserveLiveness` logs the answer, and the same answer decides whether `OnRecoveryNeeded` is called. `TestLivenessRecoveryPilotIsArmed` (`internal/cookies/refresh_liveness_test.go`) pins the fire, the Warn sentence and the in-window dedupe; `TestTwitchMarkAndTierTwoFireRecoveryOnce` (`internal/cookies/refresh_twitch_mark_test.go`) pins that a chat-marked Twitch loss plus the tier-2 verdict that follows it reach `OnRecoveryNeeded` once, and that the re-alarm lands only once the window has passed.

Four maps, deliberately separate (`internal/cookies/refresh.go`):

| Map | Written by | Read by |
|-----|-----------|---------|
| `lastLivenessObserved` | conclusive verdicts, **both** directions | `livenessObservedRecently` — the sole gate on paying for the fallback probe |
| `lastRecoveryDecided` | a signed-out verdict that cleared the dedupe (`recordLiveness`), and `noteRecoveryDecided` from a tier-1 fire | `recordLiveness`'s back-off check (`livenessRefireWindowFor`). A `LoggedIn` observation must never write here, or a healthy verdict could swallow a dead one from another channel in the same cycle |
| `lastLivenessKnown` | every outcome including `livenessInconclusive` | log-level selection only (`notable`) |
| `livenessRefireBackoff` | `escalateLivenessRefire` after every verdict that clears the dedupe (the first sets the base, each later one doubles, clamped at `livenessRefireCap`); `resetLivenessRefire` from a conclusive `LoggedIn` verdict — the ONLY writer that ever shrinks it | `livenessRefireWindowFor`, which `recordLiveness` consults instead of the flat constant. A missing entry reads as the base window. A tier-1 fire (`noteRecoveryDecided`) stamps the dedupe without touching this map |

`noteRecoveryDecided` is one-directional: the tier-1 fire stamps the map but never consults it, because suppressing the tier-1 fire would change behaviour that predates this signal entirely. `recordInconclusiveLiveness` touches none of the other three maps — recording an observation would make the next cycle skip the probe, silencing the signal for as long as it keeps failing; consuming the dedupe would swallow the next real logged-out verdict; and resetting `livenessRefireBackoff` would treat "inconclusive" as the reset condition when only a conclusive signed-in verdict is, letting an install stuck behind a captive portal or rate limit — inconclusive on every cycle — clear its own escalation every cycle too.

`livenessFreshWindow` (25 min) bounds how old the last conclusive observation may be before a periodic pass pays for the fallback probe. It **must** be strictly shorter than the refresh interval, because the fallback records its own answer through the same method — at one full cadence the probe would suppress itself on alternate cycles, halving coverage with no symptom. `NewRefreshService` enforces that against the interval it is actually handed, replacing anything at or below the window with the default and warning with both numbers. The lower bound is an *assumption about configuration*, not an invariant: the skip only works while membership observations arrive more often than the window expires, so an install with `monitors.feed_check_interval` above ~25 minutes pays for the fallback on roughly every other cycle. That degradation is bounded and one-directional.

**`authStatusChanged` is a CONTRACT, not an observation.** It is the `OnAuthChange` gate (`internal/cookies/refresh.go`) and compares six fields: the two auth booleans, the two cookies-present flags, and the two `RefreshVerdict`s. It deliberately excludes `YouTubeError`/`TwitchError`, whose text can vary between two occurrences of the same outcome, so comparing them would fire the callback on churn no verdict transition accompanies. The rule is stated forwards: **no `OnAuthChange`-driven surface may render the two strings; per-request surfaces may.** Both of today's readers are per-request — each pulls a `GetStatus()` snapshot it asked for — so neither depends on this callback and neither can go stale on screen. Widening the gate is the *precondition* for a push-driven surface that renders them, and is a deliberate change with its own cost. The verdicts and the cookies-present flags have to be in the gate: a platform going from conclusively-rejected to could-not-check leaves both booleans false, and on a boolean-only gate that badge transition was silent. All six are surface inputs in their own right and each is pinned on its own — including the two `Authenticated` booleans, which every other row of the gate's table moves together with a verdict, so a gate missing either comparison passed the whole tree until `TestAuthStatusChangedGateCoversEverySurfaceInput` gained its two "alone" rows. Whether today's producers can move one without the other is a fact about the producers, not about the gate. **This paragraph is the contract restated, not widened: the exclusion list is unchanged and no `OnAuthChange`-driven surface may render `YouTubeError`/`TwitchError`.**

`AuthStatus` (`internal/cookies/refresh_auth_status.go`) is never marshalled — every consumer hand-projects it — and **every field has a reader**, which is a property to keep. A `LastCheck` string was removed rather than wired: no projection carried it, and the obvious misreading ("the credentials were valid as of this time") is not what the timestamp of a pass that may have concluded nothing says. Anything re-added needs a reader in the same change, and if it moves on every tick it also needs a line in `authStatusChanged`'s exclusion list.

**`rs.status` has TWO writers, both under `rs.mu`, and the mark wins.** `refresh`'s status block used to be the only writer of `rs.status`; `RefreshService.NoteTwitchAuthLoss(reason)` is a second. It writes the Twitch triple — `TwitchAuthenticated=false`, `TwitchVerification=RefreshFailed`, `TwitchError` = a sentence from a fixed `switch` of string literals — and records a `twitchAuthMark` carrying the reason and the `CookieJar.TwitchIdentity()` it was taken under. `refresh`'s block then CONSULTS that mark: while it stands, one `twEffective` value drives the status, the previous-auth baseline, `shouldFireRecovery`, the `OnAuthRecovered` transition and the identity baseline, so validate's 200 cannot flip any of them. It has to work this way — `oauth2/validate` answers 200 for a valid `auth-token` whether or not a usable `login` sits beside it, so two of the four chat-downgrade routes would otherwise be erased within one tick with nothing repaired. The mark clears on a **changed fingerprint alone**, with no authenticated gate: a swap to a REVOKED token must report the 401, not the stale reason from the pair it replaced. Recovery rides the existing `shouldFireRecovery` dedupe and advances the same two baselines, so one loss raises one alarm. The reason never leaves the vocabulary: `twitchAuthLossMessage`'s arms are all string literals, so the set of strings `TwitchError` can hold is fixed at compile time, which is what makes it safe on the two per-request surfaces that render it. The mark is **process-local**: `twitchAuthMark` is a plain `RefreshService` field with nothing behind it, so a restart drops it and validate reports Twitch green again for the two missing-`login` routes until the next chat handshake re-takes it. And because `NoteTwitchAuthLoss`'s write lock plus its `OnAuthChange`/`OnRecoveryNeeded` fan-out are synchronous, its caller cannot be the IRC session goroutine that reports the downgrade — `twitchAuthLossHook` (`cmd/moombox/services.go`) delivers it through its own recover-guarded goroutine instead, because `chat.go`'s `OnAuthDowngrade` contract parks the read loop behind the call and must not block.

**Timing:**

- `RefreshService` interval: `defaultRefreshInterval` = 30 minutes. `NewRefreshService(jar, 0, log)` is how `initServices` constructs it, so nothing in production feeds the interval parameter; `cookies.refresh_interval` drives the *auto-cookie* periodic refresh instead.
- `authCheckTimeout` = 15 seconds per platform.
- `livenessRefireWindow` = 30 minutes — the BASE of the per-platform tier-2 back-off, doubling per alarm (`livenessRefireFactor` = 2) to a `livenessRefireCap` of 24 hours. Its own constant on purpose: it is neither the notification cooldown in `wireMonitorCallbacks` nor `defaultRefreshInterval`, however the three numbers line up today.
- `livenessFreshWindow` = 25 minutes.
- Initial check runs synchronously on `Start()`; tier-2 coverage therefore begins one cadence in.

### Auto-Cookie Service

`AutoCookieService` acquires credentials into `cookies.txt` four ways — an interactive browser login, a headless browser refresh, a browser-free import of a mounted browser profile (which `cookies.acquisition = "profile"` also selects on a desktop that HAS a browser), and `ImportCookies`, the operator-supplied Netscape file that `POST /api/cookies/import` and the TUI's `E I` chord both deliver. That last one is the FIFTH writer of `cookies.txt` (with the refresh service's rotation write) and inherits the same catalogue as the rest: read through the `readCookieFile` seam and abort on anything but ENOENT, merge through `mergeCookieFiles` keyed by name+domain+path (RFC 6265 identity — Secure and HttpOnly are attributes, not identity, and stay out; the jar itself remains path-blind, so two paths still load as one entry and the file merely stops losing the other row), write through `writeFileAtomic`, and never emit an empty-valued row — it filters those out of the paste before merging AND out of the merged text before writing, which also repairs any an older writer left behind. The five share no lock, and they never will: the containment is a SLOT, not a mutex over `cookies.txt` (owner decision O-D, 2026-09-17, superseding the Arc 8 ruling of 2026-08-29). `ImportCookies` now claims the same `refreshCmd` sentinel `RefreshCookiesDetailed` and `StartSetup` already gate on, and answers `ErrRefreshInProgress` — "please try again shortly", HTTP 409 on the Web route, the identical sentence `/api/cookies/auto-setup/start` gives for the identical sentinel — while a pass holds it. What reopened the Arc 8 ruling was not the width of the window but the DIRECTION of the loss: the ruling priced it as "loses at most a rotation the next 30-minute pass repairs", and the reproduction showed the pass destroying the OPERATOR'S PASTE, replacing it on the recovery path with the dead rows that raised the alarm, while the import reported `Wrote=true` with both platforms `ok`. The window it closes is the read → verify → write gap, which since 2.8.7 also contains the pre-write snapshot's two verification round trips — `checkPlatformAuth` bounds those with one `authVerifyTimeout` (12 s) PER PLATFORM, run concurrently, so the gap is bounded by ~12 s rather than by the merge alone. The re-read-and-re-merge containment the Arc 8 text named as its preferred fix was NOT taken: it keeps the import always-accepted at the cost of an extra read and merge in every pass, and a refusal the operator retries within two minutes is the cheaper honest answer. One residual is deliberate and stated: the setup wizard's finish is another writer, a third party to this exclusion, with its own read → write gap, gated by the SETUP slot rather than this one, and widening the import's gate to cover it would refuse a container operator's only re-authentication route for the 60 s grace a stale setup slot lingers.

**A paste is verified before it is committed, and reversible per platform.** `ImportCookies` runs the same protection the two refresh paths do, on the same machinery: `snapshotPlatformAuth` (`internal/cookies/autocookies_profile.go`) verifies both platforms BEFORE the write — skipped when there is no `cookies.txt`, so a first acquisition costs no round trips — the post-write check goes through `platformsToRestoreOnRegression`, and a platform it names gets its previous rows back through `restorePlatformRows`. Until 2.8.7 the import had none of this: a paste whose rows for one platform were dead REPLACED that platform's working rows (the merge lets the pasted value win by name+domain+path), the operator was told the credentials failed rather than the paste, and the sibling platform — carried verbatim by the merge, and verifying — made the whole import look partly successful. The **regression arm only**, deliberately: `credentialAccepted` already accepts an inconclusive check over a credential a human just supplied, so rolling that same credential off the disk would report it accepted and gone. `ImportResult` carries a per-platform outcome. Four of them reach the wire — `ImportInstalled`, `ImportRolledBack`, `ImportRejected` (rejected with nothing established to give back) and `ImportUnchanged` (the paste carried no row for that platform) — and a fifth, `ImportUnknown`, never does: it is the zero value, the guard for the exits that fail with `cookies.txt` already replaced, and every one of those returns an error the route answers with a `jsonErrorSized` before the payload is built. It also carries `RollbackProtected`, which is false both when there was nothing to protect and when the pre-write load failed ("rollback protection is off", the refresh path's own sentence; the import proceeds either way, because refusing it would throw away credentials the operator supplied by hand). The two per-platform outcomes reach the dashboard as `youtubeImport` / `twitchImport`; a rollback leaves `authenticated` **true** and the verification `"ok"` — truthfully, about the restored rows — so those keys are the only thing that stops the toast reporting a discarded paste as a success. A rollback that cannot be written or reloaded FAILS the call rather than being reported as one.

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

**`cookies.acquisition` picks the path; `auto_enabled` picks whether a browser may run.** They compose and neither replaces the other. `acquisition` is read LIVE through `AutoCookieService.AcquisitionMode` (`internal/cookies/autocookies.go`), the same injected-predicate shape as `BrowserLaunchAllowed` and `ConfiguredBrowserOverride`. In a refresh it changes one thing, `importedFromProfile`: `"profile"` forces the browser-free import branch regardless of `resolvedBrowser()`, which is the only route to reading a REAL signed-in profile on a Windows desktop; `"auto"` leaves the rule as it was. It has two values by ruling — the audit's `"browser"` meant "launch when a browser resolves", which is what `"auto"` already means, so the two could not be told apart at any site and the value was dropped. Under `"profile"` the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` (below). `resolvedBrowser` is untouched, and `StartSetup` never consults the mode: the interactive login is acquisition, and gating it would make a fresh install in `"profile"` mode unable to create the profile it is told to read. The no-source outcomes are unchanged, because the missing-directory block runs BEFORE the decision: `"profile"` with no profile directory still returns `ErrProfileNotFound`, and `"auto"` with no browser and no profile still returns `ErrNoBrowserFound` — both still rung 3.

**The periodic timer is `gateExempt`.** `browserGatePolicy` (`internal/cookies/autocookies.go`) has two values: `gateApplies` (the zero value, so anything that forgets to say gets the safe answer) for every caller acting on a live operator intention, and `gateExempt` for `StartPeriodicRefresh`'s goroutine and nothing else. `main.go` starts that loop only when the flag was true at boot, so the flag has already been consulted; re-asking it per tick would leave an operator who switched it off without restarting with the timer still running *and* silently switched to browser-free imports of a profile nothing changes between ticks. Flipping the flag off at runtime therefore leaves the timer launching browsers until restart, **by ruling** — the restart-required label both UIs carry is the honest cover. Do not "fix" it.

**`R F` is a three-rung ladder and never dead-ends.** `R F` (TUI), the dashboard header's shift+click, and the Settings page's "Refresh cookies from browser profile" button are one gesture: refresh by the strongest means available. The Settings twin exists because a modifier key does not exist on a phone or tablet, which left a mobile-only operator with dead cookies, an updated profile and no trigger at all on exactly the workflow designated for Docker; it calls `app.autoCookieRefresh()` directly rather than adding a second implementation.

1. Browser launching allowed AND a browser available → launch the headless browser, refresh the profile, import.
2. No browser launch (flag off, or no browser present) but a profile IS present → import from the profile immediately.
3. No browser profile at all → run what `R C` runs, and say so.

`cookies.acquisition` moves the gesture down the ladder without changing it: in `"profile"` mode rung 1 is never taken, rung 2 always is, and rung 3 is untouched. The TUI's pre-flight line and the dashboard's toast both name the mechanism that will actually run — `Importing cookies from the browser profile...` instead of `Running browser cookie refresh...` — because the browser sentence is a claim only one of the two modes can support. The two surfaces render the SAME sentence (`cookieRefreshPreflightToast` in `web/public/modules/utils.js`, `cookieRefreshFeedback` in `internal/tui/app_actions.go`), pinned by exact equality in `TestRefreshPreflightSentenceAgreesAcrossSurfaces`; unlike the rung-3 pair they name no per-surface affordance, so they do not diverge. Afterwards the mechanism is no longer a guess: `RefreshResult.Mechanism` records which source the pass actually used (`"browser"`, `"profile-import"`, or empty when it declined before choosing), rides the wire as `mechanism` on `cookieRefreshOutcome`, and feeds ONE subject-producer per surface — `cookieRefreshMechanismLabel` in `internal/tui/app_actions.go` and in `web/public/modules/utils.js`, pinned by exact equality in `TestRefreshPostflightMechanismAgreesAcrossSurfaces`. The RESULT outranks the mode there, and the mode is only the fallback, because the two disagree wherever the host decides rather than the setting: a machine with no browser installed imports in `"auto"` mode and always has, which is why every post-flight sentence used to open `Browser cookie refresh ...` after a pass that launched nothing. Only the SUBJECT is shared; each surface keeps its own predicates. The `renewed === false` arm keeps its browser wording on both, and is allowed to: `renewed := importedFromProfile || browserActed`, so an import that reaches a verdict always renewed and that arm is unreachable for one.

Rung 3 is one exported predicate, `cookies.IsNoBrowserProfile` (`internal/cookies/errors.go`), so the two surfaces cannot diverge: it is exactly `ErrProfileNotFound` or `ErrNoBrowserFound`, both from the same pre-work missing-directory check. The TUI branches on it in `internal/tui/app_update.go` and returns `recheckCookiesCmd()` — R C's own command, not a second implementation — so the sentence leads a real refresh. The Web half (`autoCookieRefresh`, `web/public/app.js`) branches on the STATUS (404 for `ErrProfileNotFound`, 424 for `ErrNoBrowserFound`), never on the prose, and then awaits `recheckCookies()`. The two sentences differ **on purpose**, each naming its own surface's affordance: the TUI's is the owner's copy verbatim, `No browser profile found, running R C instead...` (ellipsis included), and the Web's is `No browser profile found, running a normal cookie refresh instead...`; `TestRungThreeSentencesDivergeByDesign` asserts the divergence. The ladder still declines nil-error on the running-service causes in `RefreshDeclinedCauses` (a setup or another refresh already in flight, or no platform with cookies worth refreshing) — unchanged and correct. `R C` is never gated.

Rung 3 deliberately EXCLUDES every profile-import failure — `ErrProfileDirUnreadable`, `ErrProfileNotADirectory`, `ErrCookieDBNotFound`, `ErrCookieDBLocked`, `ErrCookieDBUnreadable`, `ErrNoCookiesInProfile` — and `ErrProfileDirNotOptedIn`, the config refusal beside them (the profile IS there; the remedy is one setting). Those mean the profile IS there and is wrong in a diagnosable way, each from a pass that RAN, and each carries the only guidance the operator has. Folding them into the fallback would replace real diagnosis with a recheck that cannot fix any of them. `ErrNoCookiesInProfile` is likewise never to be redefined as the auth-cookie predicate: it means the profile held nothing Moombox would keep, and narrowing it to "no auth cookie" would discard `CONSENT`, which the essential list deliberately keeps.

**Every AUTOMATIC browser-free import runs only when there is no `cookies.txt` to lose.** `automaticImportGuard` (`internal/cookies/autocookies_profile.go`) is that rule, and it is ONE rule with exactly two automatic callers: `decideStartupSeed` (the boot import) and `StartPeriodicRefresh`'s tick when that tick would be browser-free — browser-free because no browser resolves, or because `cookies.acquisition = "profile"` makes the pass an import regardless of the host. That second case is a desktop reading the operator's REAL profile, and it is exactly the scheduled re-read over live credentials this rule refuses. "Nothing to lose" means **absent, or present with zero cookie rows**; an **unreadable** file ABORTS (`autoImportCookieFileUnreadable`) rather than counting as absent, and `StartProfileSeed` Warns on that one stand-down because it is the operator-actionable case.

The asymmetry is the whole argument: a false "nothing to lose" imports over credentials and the operator may not find out until a recording fails, while a false "something to lose" costs one keypress. So the predicate does not need to be accurate — it needs zero false "nothing to lose" answers. `countNetscapeCookieRows` (`internal/cookies/autocookies_profile.go`) therefore **must over-count**: it counts lines that are neither blank nor plain comments, so a malformed row, an unrelated domain and an expired row all count. Over-counting can only produce the cheap error. Any replacement must keep that direction, which is also why "present but holding no auth cookies for either platform" is ruled out as a definition — deciding it needs a per-platform predicate, and a wrong one fails in the expensive direction. One shape is reachable only by misconfiguration and is named here so it is not rediscovered: `auto_enabled` on, a browser-free tick (no browser resolves, or `"profile"` mode), a profile directory whose import fails permanently, and no `cookies.txt` to protect — every tick imports, fails and Warns `periodic auto-cookie refresh failed`, with no back-off. The remedy is the configuration; a back-off there would be a mechanism to contain a mechanism.

The guard is **not for the manual triggers**: `R F`, shift+click and the Settings twin must keep importing over whatever `cookies.txt` holds, because replacing a live cookie file out of a profile the operator just updated by hand *is* the gesture, and it is the only path a container has. And it is **not for the recovery path**: `OnRecoveryNeeded` and the worker's `OnCookieRefreshNeeded` reach `RefreshCookiesDetailed` without passing through it, because recovery fires only on a conclusive not-authenticated — refusing the one automatic import most likely to fix the problem, on the grounds that a file exists which has just been proven not to work, would be backwards. That exemption stops being safe if a recovery producer is ever added that can fire on an INCONCLUSIVE verdict. The two-platform case (only one platform died) is covered not by the guard but by `RefreshCookiesDetailed`'s own abort/merge/rollback, which re-checks at write time: it verifies each platform BEFORE the write — **on both paths**, whenever a `cookies.txt` already exists — and then applies one of two policies. The MOUNTED-PROFILE import path uses `platformsToRestore` (`internal/cookies/autocookies_profile.go`), which hands back the rows of any platform that either verified before and failed after (a regression) or had credentials before and could not be checked after (inconclusive — committing a set nobody could evaluate over one that may be fine is a bet with no upside). The BROWSER path uses `platformsToRestoreOnRegression`, the **regression arm only**: a browser refresh has just re-fetched from the live site, so a check that then could not reach the network is evidence about the network, and restoring on it would discard a fresher set on every DNS blip — on the path a desktop install runs every thirty minutes. The operator's pasted import shares that second policy for the same reason plus one of its own (see § Auto-Cookie Service). Both share `regressedAfterWrite`, so the arm they agree on cannot drift. A platform that was already dead is deliberately not restored on any path. The browser path's exclusion used to be total, on the grounds that its cookies "cannot be staler than what was on disk"; that is true of their age and says nothing about whether they authenticate, which is why the regression arm now applies there too.

**`StartProfileSeed(ctx)`** (`internal/cookies/autocookies_periodic.go`) runs AT MOST ONE browser-free import shortly after start, and `main.go` calls it **unconditionally** — it is not under the flag. The flag owns a *repeating* read of a profile nothing changes between ticks; a boot is the one moment a mounted profile plausibly did change, because something replaced it while the process was down. Its safety condition is the cookie file, not the flag, and lives in `decideStartupSeed`. It returns immediately; the import runs on its own goroutine after `profileImportStartupDelay` (15 s) and **re-asks** `decideStartupSeed` after the wait, because an interactive setup finishing or a hand-dropped `cookies.txt` both write the file the first decision was made about. `shouldSeedFromProfileAtStartup` is deleted.

**Docker guidance.** Two browser-free paths, and they answer different questions. To REFRESH from a mounted profile: leave `auto_enabled` off, update the profile on the host, then press `R F` (or shift+click the header button, or the Settings-page twin). That is the designated workflow on a headless host: no browser is launched, the profile is imported directly, and the manual triggers are exempt from `automaticImportGuard` precisely so it works over an existing file. `cookies.txt` must live *inside* a mounted directory rather than being bind-mounted as an individual file — `writeFileAtomic`'s rename cannot replace a single-file mount. To RE-AUTHENTICATE when the profile itself is stale or there is none: paste or upload a fresh Netscape export in Settings → Cookies, which needs no volume access at all and is the only path that works from a phone against a tunnelled instance.

**The `lastError` write policy.** `lastError` (`AutoCookieService`, `internal/cookies/autocookies.go`) is the last thing a cookie pass concluded that the OPERATOR has to act on, published as `AutoCookieStatus.LastError`. One rule: **a write is allowed only where THIS pass established the thing it is asserting.** Setting asserts a problem; CLEARING asserts that whatever was recorded is not wrong any more, and that is the half that keeps being written by paths with no basis for it.

- `setError` is the single SET funnel and the only place a message enters the field. Every exit that returns an error from a cookie pass sets — `FinishSetup`'s empty-profile, read-failure, merge-abort, mkdir, write and jar-load exits, and the refresh's import-failure, merge-abort, credential-loss and verification-failure exits.
- **Two exits deliberately do NOT set**: the guard clauses at the top of `FinishSetupDetailed` (`ErrNoSetupInProgress`, `ErrSetupCancelled`). No pass has run when they fire, so there is no failure to see afterwards, and the caller gets the answer synchronously in the same dialog. "A pass" is the boundary; a guard that refuses to start one is not an exit from one.
- Three CLEARS, each earned: `StartSetup`'s slot claim (a new attempt is starting, so the recorded message belongs to an attempt that is over — the one clear about intent rather than evidence); `RefreshCookiesDetailed`'s `case renewed` in the any-platform-verified arm (the pass actually produced the credentials it verified); and that switch's "nothing to verify" branch, kept with a note because no route to it has been found and "I could not find a route" is not "there is none". The `default` beside `renewed` deliberately does not clear — a pass whose browser did nothing has established that the credentials on disk work, not that the refresh mechanism does.
- The loss branch of the same arm writes `s.lastError` directly, because a partial success still has to report the platform that was lost.
- **`cleanup()` / `cleanupLocked()` MUST NOT clear it.** `cleanup` runs on every setup exit path including the failed ones — `FinishSetup` calls `setError` and then `cleanup` on each failure exit — so clearing there would erase the message microseconds after it was written. Pinned by `TestCleanupAfterAFailedSetupKeepsLastError` and `TestFinishSetupRecordsTheFailureItReturns`.

It has two readers: the Web settings panel's `lastError` line and the TUI's `R C` result line (`Last cookie error:`).

**`fetchedNoCredential`** (`RefreshCookiesDetailed`, `internal/cookies/autocookies_refresh.go`) is a NEW flag, never a redefinition of `fetchedRows`: rows came back and **not one of them is a session credential** — a signed-out browser profile, or one set to clear cookies on exit and re-seeded with `YSC`/`VISITOR_INFO1_LIVE` by the navigation this pass just made. Read as either neighbouring case it was wrong: "the browser profile contained no cookies" is false, and "auth verification failed — manual re-login required" says nothing an operator can act on. It is measured on what THIS PASS fetched, before the merge folds the previous `cookies.txt` in, and it is computed with `netscapeCookiesHoldACredential` — which loads the text into a THROWAWAY jar and asks the jar's own loose predicates, so "is this a credential" has one answer across the package. Overloading `fetchedRows == 0` was rejected because that counter's deliberate over-counting is load-bearing for the import guard.

**`GetStatus()` versus `ReloginStatus()`.** Both take `s.mu` and both call `reapAbandonedSetupLocked`; `ReloginStatus` returns only the cloned `needsRelogin` map and skips `GetStatus`'s browser/registry detection (~155 ms measured 2026-08-25). Four of `GetStatus`'s five production callers read nothing else, so they were paying that on every poll. Status polling is the most frequent visitor to this lock, which makes it the reap that actually fires in practice — so `ReloginStatus` **must** keep calling the reap. A "simplification" that drops it because the method "only reads `needsRelogin`" would silently stop the abandoned-setup reap from firing in production, with no test noticing.

**Browser detection is cached, both halves.** `DetectBrowser()` (the single best pick) and `DetectBrowsers()` (the full list) share one mutex and one 60-second TTL in `browserDetectCache` (`internal/cookies/autocookies_detect.go`). `DetectBrowsers` used to be uncached, so every status poll rebuilt the list from a filesystem+registry scan (a `reg.exe` spawn on Windows) it almost always threw away. Both return the cache's own backing value — callers must treat them as read-only. `InvalidateBrowserDetection()` clears both, and is called whenever the configured browser changes, since that is exactly when an operator has most likely just installed the browser they are pointing Moombox at.

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

The anti-automation flags are mirrored on the headless launch deliberately: YouTube raises fraud scores for a browser that advertises itself as automated, which would invalidate the very cookies the pass is refreshing. `dangerousProfilePathSubstrings` (`internal/cookies/autocookies_browser_resolve.go`) refuses a configured profile directory that belongs to a real installed browser, so a hostile config cannot launch Chrome against the user's actual signed-in profile and exfiltrate it through the `cookies.txt` export. The check is `validateBrowserProfileDirForLaunch`, computed once at construction, and its name carries its scope: it holds on all four subprocess sites (`startFirefoxSetup`, `refreshFirefox`, `startChromiumSetup`, `refreshChromium`) in every acquisition mode. The two READ-ONLY sites — `importProfileCookies` and `decideStartupSeed` — go through `readOnlyProfileDirErr` instead, which honours it by default and lifts it when `cookies.acquisition = "profile"`, and refuses with `ErrProfileDirNotOptedIn` rather than a sentence about a launch that cannot happen on that path. The list covers Windows, Linux (the dotfile, snap and flatpak trees) and macOS, and has since 2026-09-08: 18 Windows shapes, 16 Linux `~/.mozilla` / `~/.config` dotfile shapes (snap's `~/snap/firefox/common` tree matches through the same `~/.mozilla` path, while snap Chromium — Ubuntu's default since 20.04 — keeps its profile at `~/snap/chromium/common/chromium` with no `.config` anywhere and carries its own entry), five flatpak `~/.var/app` sandboxes and seven macOS `~/Library/Application Support` shapes — 46 entries, every one written with `/` separators and matched case-insensitively against the absolute path with `\` normalised to `/`. The sentence that stood here until 2026-09-17 called the list Windows-only and deliberately un-widened; it described the pre-2026-09-08 list and had been false for over a week. The Linux desktop case it worried about (audit G3) is answered by the read boundary rather than by a narrow list: `cookies.acquisition = "profile"` lifts the guard for the two READ-ONLY sites, while launching a browser against a real profile stays refused on every OS.

**Cookie extraction:**

| Engine | Source | Method |
|--------|--------|--------|
| Firefox | `cookies.sqlite` in the profile dir | `readFirefoxCookies` (`internal/cookies/autocookies_firefox.go`) SNAPSHOTS the DB **together with its `-wal` sidecar** into a temp dir and queries the copy; copying without the sidecar silently returns rows that are missing every uncommitted write. Falls back to querying in place if the snapshot itself fails, and retries up to 5 times at 500 ms for WAL lock contention and torn snapshots, breaking early on anything non-retryable. NULL columns are defaulted and unusable rows counted, both reported by the caller |
| Chromium | the live browser over CDP | `cdpGetCookiesAsNetscape` (`internal/cookies/autocookies_chromium.go`) runs a three-tier ladder: browser-level `Storage.getCookies`, then per-page `Network.getAllCookies`, then per-page `Network.getCookies`. The gate between tiers is the RELEVANT row count, not the raw one, so a tier-1 answer full of other sites' cookies does not stop the ladder |
| Chromium (opt-in fallback) | the user's REAL profile under `%LOCALAPPDATA%`, decrypted with `CryptUnprotectData` | `dpapiExtractAsNetscape` (`internal/cookies/autocookies_dpapi.go`). Reached only when the browser refresh already returned an error, `cookies.dpapi_fallback` is on, and the resolved browser is non-Firefox — DPAPI launches nothing, so it sidesteps "Chromium is already running our profile" entirely. Off by default: it reads the user's actual signed-in cookies, so it is a privacy surface they have to enable consciously |

An empty result and an unanswered read are different facts. `cdpCookieReadOutcome` distinguishes them: `ErrBrowserLadderBlocked` when a structural failure (the `/json` target listing) stopped the fallbacks from running at all, `ErrBrowserReadUnanswered` when no query answered. Neither is `IsNoBrowserProfile`, so neither triggers rung 3 — both come from a pass that RAN, and a plain recheck cannot fix either. `writeBrowserReadError` (`internal/web/routes/cookies.go`) maps the first to **409** and the second to **502**, each with a machine-readable `cause`, and passes the composed message through verbatim.

**The DPAPI fallback reads exactly ONE profile.** It used to walk every profile `dpapi.FindBrowserProfiles` returned and merge them before dedup, so with two signed-in Chromium profiles `deduplicateAndFormat`'s bare-name dedup let whichever profile was listed LAST silently win each cookie name — an order-dependent coin flip that could pair a SAPISID from profile A with a LOGIN_INFO from profile B. Selection now: filter by the configured browser type (empty **and** `"chrome"` both mean "every profile is a candidate" — the Web UI's only Chromium option is literally the whole family; a type with no layout in `dpapi.KnownBrowserFamilies()` also falls back to every profile, logged at Debug, because that is a coverage gap rather than a missing browser), score each candidate with `dpapiProfileScore` using the same loose/strict predicates `jar.go` keeps, take the highest, break ties by scan order with an Info line naming every tied profile, and discard the rest whole. Known limitation, deliberately not built and ruled NEVER by the owner (2026-08-29: one profile is the design — the managed setup profile signs into both platforms — so a two-pass per-platform selection is not to be re-proposed): the score sums both platforms into one number, so YouTube-on-profile-A / Twitch-on-profile-B still loses one platform — deterministically and logged, instead of silently. **One question about this path is unmeasured**, and it is written out in `internal/cookies/dpapi/dpapi_windows.go`: whether a `mode=ro` read of a live WAL-mode Chromium `Cookies` DB returns stale rows — that is, whether `modernc.org/sqlite`'s pure-Go reader merges committed `-wal` frames the way the C library's WAL reader protocol does. Lock conflicts are already handled and degrade to a clean error; staleness would not, because a read that misses the `-wal` returns whatever was last checkpointed into the main file — well-formed rows that nothing errors on. Settling it needs a signed-in browser writing to the DB on the same machine. Until that runs, the Firefox path's copy-the-sidecar shape is deliberately not applied here.

**CDP readiness** (`waitForCDP`, `internal/cookies/autocookies_chromium.go`) polls `http://127.0.0.1:<port>/json/version` with exponential backoff — 200 ms doubling to a 2 s cap — until `cdpPollTimeout` (15 s). It waits for the debugging endpoint to come up; it does **not** poll for login completion. Interactive login completion is the operator pressing "I'm Logged In", which calls `FinishSetup`. Other CDP budgets: `cdpExtractTimeout` 30 s, `cdpRefreshTimeout` 60 s (cold start plus sequential per-platform navigations plus extraction, all sharing it), `cdpNavigateTimeout` 30 s per `Page.navigate` + `loadEventFired` wait. A navigation whose read loop hits its deadline without ever seeing `Page.loadEventFired` returns `errNavigateBudgetExhausted`. `navigateAllPlatforms` treats that per platform as NOT OBSERVED rather than as a failure — it never joins `navFailures` — and lets it flip the pass's "navigated" answer only when it is **every** platform's outcome. One platform timing out beside a sibling that fired its load event is a slow page on a browser that demonstrably works; do not loosen "every" to "any".

**Lock file cleanup.** Before launching, the service removes stale lock files that would otherwise prevent a launch:

- Chromium (`cleanChromiumLockFiles`): `lockfile`, `SingletonLock`, `SingletonSocket`, `SingletonCookie`, plus the `Singleton*` and `*lockfile*` globs for variants newer builds leave behind.
- Firefox (`cleanFirefoxLockFiles`): `parent.lock`, `.parentlock`.

Both go through `removeStaleLock`, which **skips any file touched within `lockFileFreshThreshold` (5 s)** — a truly stale lock from a crashed run is older than that, while one held by a live browser is not, so this cannot yank the lock out from under a running instance.

**Orphaned temp files and directory permissions.** `writeFileAtomic` writes `<base>.<random>.tmp` beside the target and renames; a crash, a kill or a panic in between leaves a full copy of `cookies.txt` — the highest-value secret in the app — under a name nothing reads. `sweepStaleCookieTempFiles` (`internal/cookies/cookie_files.go`) removes those, ONCE per process (`cookieTempFileSweepOnce`, wired at `NewAutoCookieService` because that is the one place holding the real cookie path up front), matching only `<base>.*.tmp` in the cookie file's own directory and only when older than `cookieTempFileMaxAge` (1 hour). Age is the only guard against sweeping a write in progress. Every failure is Debug-logged with the path only and left for the next start.

`tightenCookieDirOnce` applies `utils.ApplyUserOnlyDACL` to the cookie file's parent — `icacls` on Windows, a real `chmod` to 0700/0600 on Linux (not a no-op there). It is **memoised on SUCCESS, not on attempt**, with three states per directory (absent = not started, `dirTighteningInFlight`, `dirTighteningDone`): a transient failure or a panic mid-apply deletes the entry so the NEXT cookie write retries, rather than disabling hardening for the rest of the process because the failure was demoted to a Debug line. Cost if it fails permanently on a host: one extra ~30-80 ms shell-out per cookie write instead of one per process, bounded by the write cadence. No failure cap and no backoff — either would be a mechanism to contain a mechanism.

**On POSIX that directory chmod applies only to a DEDICATED directory** (owner decision O-K, 2026-09-17). Both writers — `tightenCookieDirOnce` and the twin block in `config.Save` — ask `utils.DirTighteningAllowed` (`internal/utils/dedicateddir.go`) first. A refusal is **silent on the cookie side** — `tightenCookieDirOnce` has no logger and is not being given one for this — and leaves a Debug line in `config.Save`, which does. The answer is always yes on Windows, where `icacls` writes inheritable ACEs onto the directory and its children rather than removing traversal from everyone else's world: O-K leaves the Windows path byte-identical. On POSIX it is a CONTENT test over the directory's own entries (`DirHoldsSharedData`) — an entry named `output`, `staging` or `logs` — tested by NAME before its type, so a dangling symlink at an unmounted bind target counts too — or a file whose extension is `.db`, `.sqlite`, `.sqlite3` or `.log` after SQLite's `-wal`/`-shm` sidecar suffix is stripped, marks the directory as Moombox's DATA directory rather than a dedicated secrets one. A listing that reaches 512 entries disqualifies the directory outright rather than being truncated. Content rather than config because the rule has to answer the same way in `internal/config` (which cannot import `internal/cookies`), in `internal/cookies` (which does not import `internal/config`) and in the `moombox add` side process, which wires no config store at all. Three boundary answers are deliberate: an empty directory is dedicated (that is the first-write case), a directory holding only `cookies.txt` and its own family is dedicated, and a directory that cannot be listed is treated as shared — a hardening that cannot see what it is tightening declines. `config.toml` is deliberately NOT a shared-data marker: it is a secret of the same family as `cookies.txt`, so counting it would mean no config directory anywhere was ever tightened.

The motivating case is the image: `cookies.txt` and `config.toml` both live in `/data`, the bind mount that also holds `output/`, `staging/`, the database and the log, so the first cookie write or settings save used to chmod it 0700 — undoing the Dockerfile's deliberate `chmod 777 /data` and taking the operator's archives away from their own host user (a compose-default `./data` auto-created as root stays host-readable now). Files are unaffected on every path: `writeFileAtomic` chmods its temp file to 0600 before the rename and `SaveMeta` writes 0600, so what a shared directory gives up is only the untraversable parent.

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

### Output Template

The output template (default: `${channel}/${start_date} ${title} [${id}]`) is resolved at download time using `config.ResolveTemplate()`. The file extension (`.mkv`, `.mp4`, etc.) is appended by the muxer.

Per-channel output directories override the global `paths.output_directory` when configured.

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
   mismatch discards the state; (b) otherwise YouTube URL fingerprinting
   (`videoID/itag` extracted from either URL shape) — differing or mixed
   fingerprints discard; (c) when NEITHER URL carries an extractable
   identity (Twitch weaver URLs), the state is TRUSTED — raw URL equality
   has no signal there and rejecting on it used to truncate hours of
   recording on every restart.
2. Output file must exist and be at least as large as `BytesWritten`; states
   older than 7 days (`maxResumeStateAge`) are discarded.
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
- `isAction` marks a `/me` message. The CTCP wrapper `\x01ACTION …\x01` is stripped at parse time so `message` holds only the text and the emote offsets line up; `raw` keeps the verbatim IRC line.

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

Saved as `<chat_file>.resume.json`. Updated on every disk flush (at most once per `writeInterval`, 1 s) — `maybeFlush` writes the chat file and the sidecar together, so the two never describe different positions. `streamStartMs` is the epoch every `offsetMs` already written to the chat file was computed against; a restarted run reads it back and keeps it even when its own `StreamStartTime` option carries a newer (actual, vs. scheduled) start — one chat file, one epoch, never two. `mode` is `"live"` or `"replay"` — which kind of run wrote the sidecar (the mode rule: a replay run refuses a live run's sidecar and starts from scratch, since its count, continuation, dedup IDs and epoch all describe the live half of the file; an empty `mode`, written before the field existed, is adopted as before). (An older `lastTimestampUsec` field may still appear in sidecars written before this field existed — it is ignored on load and no longer written.)

**Batching:**

Chat messages are buffered in memory and flushed to disk at 1-second intervals (`writeIntervalMs = 1000`). After flushing, the in-memory buffer is released to minimize memory usage during long streams.

---

## Logger (internal/logger/)

### Overview

The `Logger` wraps Go's `slog` package with file rotation, a ring buffer for recent entries, per-job log buffers, and a pub/sub system for real-time delivery.

### Multi-Writer Output

Log output is sent to both:

1. **Stdout** via a `switchableWriter` that can be toggled off when the TUI is running (BubbleTea owns the alternate screen; raw writes would corrupt the display). `SuppressStdout()` / `RestoreStdout()` control this.
2. **Log file** via the Logger itself (which implements `io.Writer` with rotation).

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

### Per-Job Log Buffers

The LIVE per-job log pipeline is the database's: `Logger.Subscribe()` feeds `db.RouteLogToJobs()`, served by `db.GetJobLogs` (capped at 200 lines, trimmed to 100; `db.PruneJobLogs(activeIDs)` drops buffers for inactive jobs). Only NON-TERMINAL jobs are scanned for — see § Per-Job Log Buffers above for the routed set.

The Logger type once carried a parallel `LogForJob`/`GetJobLogs`/`PruneJobLogs` buffer API; nothing in production ever wired it (the buffers stayed permanently empty at runtime), and it was removed in 2026-07. Per-job log consumers use the database pipeline above.

### Pub/Sub

`Subscribe()` returns a buffered channel (`cap 100`) that receives formatted log lines in real-time.

`Unsubscribe(ch)` removes the channel from the subscriber list. The channel is not closed (to avoid a race with concurrent `broadcast()` calls); it is left for GC.

`broadcast(line)` sends to all subscribers. If a subscriber's channel is full, the line is dropped (non-blocking send).

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

---

## BotGuard Sidecar Cache (`%LOCALAPPDATA%/Moombox/sidecar/`)

Moombox extracts the embedded Node.js binary and BotGuard sidecar payload to a per-user cache directory on first launch. This is the only on-disk artifact Moombox produces outside its working directory.

### Path

`os.UserCacheDir() + "/Moombox/sidecar"`. On Windows: `%LOCALAPPDATA%/Moombox/sidecar`. Chosen over `%TEMP%` because Defender heuristics treat `%TEMP%` extractions of executables as more suspicious than `%LOCALAPPDATA%`.

### Contents

```
%LOCALAPPDATA%/Moombox/sidecar/
├── node.exe                         (~83 MB extracted from embed's gzipped 33 MB)
├── package.json
├── package-lock.json
├── version.txt                      (the embedded Node manifest + "sidecar@<sha256 of sidecar.tar.gz>" — cache-invalidation key)
├── src/
│   └── server.js                    (~250 lines, JSON-RPC server)
└── node_modules/                    (production deps: bgutils-js, jsdom, transitives — ~17 MB)
```

### Lifecycle

- **First launch:** `extractIfNeeded(cacheDir)` creates the dir, applies `utils.ApplyUserOnlyDACL` to tighten permissions to current-user-only (matches the config-dir hardening), gunzips `node.exe.gz`, gunzip+tar-extracts `sidecar.tar.gz` using stdlib `archive/tar` + `compress/gzip` (no system tar required), writes `version.txt` last.
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
- `internal/cookies/cookie_files.go` -- cookie file hygiene: writeFileAtomic, orphaned temp-file sweep, directory ACL tightening
- `internal/logger/logger.go` -- Logger, file rotation, ring buffer, pub/sub, per-job buffers
- `internal/engine/downloader.go` -- ResumeState, resume file management
- `internal/chat/downloader.go` -- Chat file writing, incremental append, messageCount padding
- `internal/chat/types.go` -- ChatData, ChatMessage, ChatResumeState
