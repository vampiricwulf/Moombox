# Data and Storage

## Scope

This document specifies every data persistence layer in Moombox: the SQLite database (schema, connection tuning, batch coalescing, pub/sub), the TOML configuration system (sections, types, migrations, validation), the cookie management subsystem (jar, refresh, auto-cookie), the logger (file rotation, ring buffer, pub/sub), and the on-disk file output conventions (staging, output templates, resume state, chat files). It is the authoritative reference for how Moombox reads, writes, and organizes persistent and transient data.

## Rules and Constraints

These are hard rules. An AI assisting with Moombox development must follow them without exception:

- **SQLite with WAL mode, 1 connection, 5s busy timeout, foreign keys on.** The DSN is `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)` — modernc.org/sqlite only honors `_pragma=...` parameters (the mattn-style `_journal_mode=...` form is silently ignored). Connection pool is `SetMaxOpenConns(1)` and `SetMaxIdleConns(1)`. SQLite is single-writer; do not change the pool size.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** The method accepts `map[string]any`, maps keys through `fieldToColumn` (40 entries), dynamically builds a `SET` clause, and auto-appends `updated_at` with the current UTC RFC3339 timestamp. After writing, it re-reads the full job row to notify subscribers with a complete `*Job` object. Returns the updated `*Job`.
- **`fieldToColumn` defines the allowed keys for `UpdateJobFields`.** Any key not present in this map is silently ignored. The map currently has 40 entries mapping Go field names to SQLite column names (identity mapping in all cases). Adding a new column to the jobs table requires adding a corresponding entry here.
- **`JobStatus` is `type JobStatus string`.** Status values are string constants, not integers or enums. Timestamps are ISO 8601 / RFC3339 strings. Optional numeric fields (sequence counters, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`).
- **Batch update coalescing: 100ms signal-driven window, zero IO when idle.** The `batchUpdateLoop` goroutine sleeps on a channel until the first update arrives, then waits 100ms to accumulate more updates, then flushes all pending updates in a single transaction. When no updates are pending, the goroutine consumes zero CPU and performs zero IO.
- **Config migrations are non-destructive.** `migrateOldFormat()` only applies a migration when the target section does not already exist in the TOML file. It never overwrites user-configured values in existing sections.
- **FlexDuration parses config values as minutes or days, context-dependent.** A bare integer in `feed_check_interval` means minutes; in `hide_finished_age_days` it means days. Duration strings like `"10m"`, `"7d"` are parsed via regex and converted to the context-appropriate unit.
- **Schema migrations are versioned, idempotent, and forward-only.** Currently at v15. Each migration checks the current version before applying. Migrations run at startup in `Database.Init()`. There is no rollback mechanism.
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
    ctx       context.Context
    mu        sync.RWMutex
    closeOnce sync.Once
    logger    dbLogger

    // Batch update coalescing
    updateCh  chan *Job     // buffer 100
    batchDone chan struct{} // closed when batchUpdateLoop exits

    // Pub/sub
    onJobUpdate  []func(*Job)
    onJobsChange []func([]*Job)
    subMu        sync.RWMutex

    // Prepared statements
    stmtGetJob *sql.Stmt

    // Per-job in-memory log buffers
    jobLogsMu sync.RWMutex
    jobLogs   map[string][]string
}
```

Key details:

- `mu` (sync.RWMutex) guards all database operations. Read operations acquire `RLock`; write operations acquire `Lock`.
- `updateCh` is a buffered channel (capacity 100) that feeds the batch update goroutine.
- `batchDone` is closed when the batch loop exits, allowing `Close()` to wait for pending flushes.
- `stmtGetJob` is the only prepared statement (hot-path SELECT for `GetJob`).
- `jobLogs` is an in-memory map of per-job log buffers (not persisted to SQLite). Capped at 200 lines per job; when exceeded, trimmed to the last 100.

### Batch Update Coalescing

The `batchUpdateLoop()` goroutine implements signal-driven coalescing to reduce write amplification during rapid job progress updates.

**Algorithm:**

1. The goroutine blocks on `updateCh` until the first `*Job` arrives.
2. It starts a 100ms coalesce timer.
3. Any additional jobs arriving during the 100ms window are accumulated in a `map[string]*Job` (keyed by job ID, last-write-wins).
4. When the timer fires, all pending jobs are flushed in a single SQL transaction via `flushUpdates()`.
5. The goroutine returns to step 1.

**Flush process (`flushUpdates`):**

1. Begin transaction.
2. For each pending job, execute a full-row UPDATE (all columns). Failures are logged but do not abort the transaction for other jobs.
3. Commit transaction.
4. Snapshot subscribers under `subMu.RLock`.
5. For each successfully persisted job, call `safeCallJobUpdate(fn, job)` for all OnJobUpdate subscribers.

**Edge cases:**

- When `updateCh` is full (100 pending), `UpdateJob()` falls back to a synchronous direct write under `db.mu.Lock`.
- When `Close()` is called, `updateCh` is closed. The batch loop drains remaining items, flushes them, then closes `batchDone`.
- If the transaction commit fails, no subscribers are notified.

**Performance characteristics:**

- Zero IO when idle (goroutine blocks on empty channel).
- During active downloads, typically 1 transaction per 100ms covering all active jobs.
- Non-blocking send to `updateCh` means callers (download workers) never block on database writes.

### Partial Updates (UpdateJobFields)

`UpdateJobFields(id string, fields map[string]any)` is the primary mechanism for updating specific job fields without loading and re-writing the entire job row.

**Process:**

1. Iterate `fields` map; for each key, look up the column name in `fieldToColumn`. Unknown keys are silently skipped.
2. Build dynamic `SET col1=?, col2=?, ..., updated_at=?` clause.
3. Execute the UPDATE under `db.mu.Lock`.
4. Re-read the full job row via SELECT (subscribers need all fields, not just the changed ones).
5. Notify all `onJobUpdate` subscribers with the complete `*Job`.

**fieldToColumn map (40 entries):**

```
status, progress, percent, eta, speed, error, title, channel_name,
thumbnail_url, description, output_file, filename, output_directory,
download_started_at, stream_start_time, stream_end_time, length_seconds,
last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
total_chat_messages, chat_status, chat_filename, chat_file, thumbnail_file,
description_file, is_vod, video_width, video_height, video_fps, file_size,
last_recheck_at, twitch_quality, twitch_category, channel_avatar_url,
quality_preference, watched, resume_position, chat_offset
```

All entries use identity mapping (Go key name == SQLite column name).

**Usage example:**

```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "V:1234 A:5678 C:900",
    "percent":  42.5,
})
```

This differs from `UpdateJob()` which queues a full-row write through the batch coalescer. `UpdateJobFields` is synchronous, immediate, and triggers subscribers directly.

### Pub/Sub System

Two callback types:

| Callback | Signature | Trigger |
|----------|-----------|---------|
| `OnJobUpdate` | `func(*Job)` | After each job is written (both batch flush and `UpdateJobFields`) |
| `OnJobsChange` | `func([]*Job)` | After `AddJob`, `DeleteJob`, `AddTrim`, `DeleteTrim` (structural changes) |

Both registration methods return an unsubscribe function. Unsubscription nils out the callback slot (avoids slice reallocation).

**Panic safety:**

- `safeCallJobUpdate(fn, job)` wraps each callback in `defer func() { if r := recover(); ... }()`.
- `safeCallJobsChange(fn, jobs)` does the same.
- A panicking subscriber cannot prevent other subscribers from being notified.

**Notification flow for `notifyJobsChange()`:**

1. Called while `db.mu` is already held.
2. Uses `getAllJobsUnlocked()` (skips acquiring `db.mu`) to get the full job list.
3. Fires callbacks in a separate goroutine to avoid blocking the caller.

### Schema

**Current version: 17**

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
| chat_status | TEXT | NULL | |
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

- `AddJobLog(jobID, line)` appends a line. Capped at 200 lines; when exceeded, trimmed to last 100.
- `RouteLogToJobs(line)` scans all tracked job IDs and routes the line to the first matching buffer (substring match on job ID in log line).
- `TrackJobForLogs(jobID)` initializes the buffer for a job (nil slice).
- `PruneJobLogs(activeIDs)` removes buffers for jobs no longer in the database.
- `ClearJobLogs(jobID)` removes a specific buffer.

### Auxiliary Data Operations

- **History:** `HasProcessed(videoID)` / `AddToHistory(videoID)` tracks previously seen video IDs (10,000 cap with LRU pruning).
- **Feed-history store** (`database_feed_items.go`): `UpsertFeedItem` (insert-or-update, reports whether the row is new), `ApplyProbeToFeedItem` (probe writes status/title/date back), `FeedScope` (the window + always-covered upcoming/live read), `SetFeedItemSource`, `RenumberCatalog`/`ListFeedOrderRows` (backfill ordering pass), `SaveBackfillCursor`/`LoadBackfillCursor`, `SetChannelBackfilled`/`GetChannelBackfill`, `SetChannelRSSOK`/`GetChannelRSSOK`, `GetChannelEstablished`, `ListFeedChannelIDs`, `DeleteChannelFeedData` (channel-removal prune).
- **Client tokens:** Full CRUD operations (`AddClientToken`, `GetClientTokenByPrefix`, `ListClientTokens`, `UpdateClientTokenUsage`, `DeleteClientToken`, `DeleteAllClientTokens`).
- **Job stats:** `GetJobStats()` returns aggregate counts and sizes via a single SQL query with CASE expressions.
- **JSON import:** `ImportFromJSON(path)` imports data from the TypeScript-era `moombox.json` format in a single transaction.

---

## Configuration (internal/config/)

### Format and Parsing

Configuration is TOML, parsed via `BurntSushi/toml`. The full config type is `MoomboxConfig`, a struct with nested section structs and TOML struct tags.

### File Search Order

When loading configuration (via `Load(customPath)`), files are checked in order:

1. `--config` flag path (if provided)
2. `<cwd>/config.toml`
3. `<cwd>/config/config.toml`
4. `~/.config/moombox/config.toml`
5. If none found: use `Defaults()` with no file loaded (`ConfigLoaded = false`)

### Configuration Sections

#### [network]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| Port | int | 774 | `port` | Valid range: 1-65535 |
| NetworkAccess | string | "localhost" | `network_access` | "localhost", "lan", "external", or "public" — "public" is a config-file-only synonym for "external" (rejected as an API input, absent from both UIs) |
| HTTPSEnabled | bool | false | `https_enabled` | |
| TLSCertPath | string | "" | `tls_cert_path` | |
| TLSKeyPath | string | "" | `tls_key_path` | |
| PasswordHash | string | "" | `password_hash` | scrypt hash, omitted from JSON; a plaintext value is auto-converted on the next start |
| ClientTokenTTLDays | int | 365 | `client_token_ttl_days` | Valid range: 1-3650 |
| TrustForwardedProto | bool | false | `trust_forwarded_proto` | Only behind a TLS-terminating proxy that strips the client's own header |
| TrustedProxies | []string | `[]` | `trusted_proxies` | Reverse-proxy IPs/CIDRs whose `X-Forwarded-For` is honored. Entries must parse as an IP or CIDR (invalid ones are reported and dropped). Hot-reloadable — no restart. See [security.md](security.md) |

#### [paths]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| DatabasePath | string | "./moombox.db" | `database_path` |
| LogFilePath | string | "./moombox.log" | `log_file_path` |
| OutputDirectory | string | "./output" | `output_directory` |
| StagingDirectory | string | "./staging" | `staging_directory` |
| FfmpegPath | string | "" | `ffmpeg_path` |

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
| TwitchCheckInterval | *int | nil | `twitch_check_interval` | Seconds, valid: 1-3600 |
| HideFinishedAgeDays | FlexDuration | 30 (days) | `hide_finished_age_days` | |
| ProbeCooldown | FlexDuration | 0 (seconds, disabled) | `probe_cooldown` | Min seconds between re-probing the same video's metadata. 0 = every cycle re-probes; no max. |
| MembershipDiscovery | *bool | nil (→ true) | `membership_discovery` | Members-only `/membership`-tab discovery. Absent/nil = enabled; needs YouTube auth cookies to do anything. |

#### [downloader]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| OutputTemplate | string | `${channel}/${start_date} ${title} [${id}]` | `output_template` |
| MaxVideoResolution | int | 2160 | `max_video_resolution` | |
| NumParallelDownloads | int | 10 | `num_parallel_downloads` | Min: 1. Peak concurrent VOD **jobs** across all channels — broadcasts never wait on the pool, so total concurrent downloads can reach (live streams) + this. Not to be confused with SegmentWorkers below, which gates concurrency *within* one download; a live broadcast's catch-up speed is governed entirely by SegmentWorkers, since this setting never applies to it. |
| SegmentWorkers | int | 12 | `segment_workers` | Min: 1, **no max**. Concurrent segment fetches within a single download (catch-up on a live DASH stream, parallel VOD HLS). Values above `config.SegmentWorkersWarnThreshold` (16) log a startup warning and are flagged in both UIs' help text: a wide simultaneous fan-out to YouTube is a traffic shape that attracts bot detection. Measured 2026-08-15 on a live stream mid-archive, at the pre-rewrite fixed 6-worker baseline: Moombox sustained 5.96 MB/s, against 2.86 MB/s for one `curl` connection and 11.28 MB/s for six parallel `curl` connections — the headroom the new configurable default (12 workers) and the rolling-window catch-up rewrite exist to close (a harness test of the same rewrite dropped 4.888s of batched catch-up to 0.84s). Not restart-required. |
| DownloadChat | bool | true | `download_chat` | |
| Prefer60fps | bool | true | `prefer_60fps` | |
| MaximumTimeout | int | 600 | `maximum_timeout` | Seconds; YouTube livestreams. Min: 30 |
| InterruptionTimeout | FlexDuration | 120 (minutes) | `interruption_timeout` | Min: 0, no max. How long a live YouTube download's MaxTimeout-backstop finalize may keep deferring while `engine.SegmentDownloader.MayResume` reports the broadcast may still resume (`stallForPossibleResume`, `internal/engine/downloader.go`) — the interruption-resume design's Tier 1 stall. `0` disables the STALL only, not Tier 2 preservation: `attachMayResume` (`internal/worker/interruption.go`) installs `MayResume` unconditionally, and every live strategy site maps the config value through `engineInterruptionTimeout` before it reaches `engine.DownloaderOptions.InterruptionTimeout` — a positive value passes through as the ceiling, `0` (or a defensive negative) maps onto the sentinel `engine.InterruptionNoStall` (`-1`). `stallForPossibleResume`'s `InterruptionNoStall` branch still consults `MayResume` once per call and still latches `finalizedDuringInterruption` when it reports true, but always returns `false` — no stall, no clock. A parallel worker-side latch, `resumeWaitLatch` (fed by `noteRefreshFailure`/`resumeEvidence` in `internal/worker/interruption.go`), gives the same treatment to the `ErrQualityLost` refresh-failure path: evidence latches `incomplete_tail` even when `shouldWaitForResume` itself never permits an actual wait. So a `0` job never blocks finalize, but a genuinely-interrupted `0` job still finalizes with staging + resume data preserved exactly like an enabled one that gave up. Snapshotted per job start (`buildJobContext`), like `MaximumTimeout`/`SegmentWorkers` above; not restart-required. |
| IncompleteStagingExpiryDays | FlexDuration | 7 (days) | `incomplete_staging_expiry_days` | Min: 0, no max. How long a Finished job flagged `incomplete_tail` keeps its staging directory shielded from orphan cleanup (`jobNeedsStaging`/`incompleteStagingExpired`, `internal/worker/orphans.go`). Only the disk-heavy staging shield expires — the flag (the "may be missing its tail" badge) never does: YouTube cannot resume a broadcast days later, so aged interruption staging has no resume value, while the badge stays honest indefinitely. Age is measured from the job's `updated_at`, so any activity restarts the window; unparseable timestamps preserve. After expiry the staging becomes an ordinary orphan-scanner candidate; auto-resume's staging-existence gate then falls to the silent drop and manual Reinitialize remains the recovery. `0` = preserve forever. Read live per scan; not restart-required. |
| PoToken | string | "" | `po_token` | Manual PO token override |
| VisitorData | string | "" | `visitor_data` | Manual visitor data override |
| PotProviderURL | string | "" | `pot_provider_url` | External PO token provider |

#### [cookies]

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| CookieFile | string | "./cookies.txt" | `cookie_file` | **Restart-required.** `AutoCookieService` is constructed from this once, at startup (`initServices`, `cmd/moombox/services.go`). |
| AutoEnabled | bool | false | `auto_enabled` | **Restart-required.** Owns exactly three things: the headless-browser periodic timer, the one automatic recovery attempt, and the `SetExpectedPlatforms` seeding at `cmd/moombox/main.go:276-278`. See §Auto-Cookie Service for the full settled meaning. |
| BrowserProfileDir | string | "./browser-profile" | `browser_profile_dir` | **Restart-required.** The directory's *existence* is not part of the start condition — `periodicRefreshHasSource` (`internal/cookies/autocookies.go`) asks per tick. |
| BrowserPath | string | "" | `browser_path` | Explicit browser override. Only a real override when paired with `browser_type` (`browserOverrideConfigured`, `internal/cookies/autocookies.go`). |
| BrowserType | string | "" | `browser_type` | Which extraction backend applies to `browser_path` — Firefox `cookies.sqlite` vs Chromium CDP. Validated against `knownBrowserTypes` (`internal/cookies/browser_validate.go`). |
| Platforms | []string | [] | `platforms` | Platforms with verified cookies. Seeded on first run by `detectCookiePlatforms` (`cmd/moombox/services.go`) — sidecar first, loose cookie-name predicates second. Nothing automatic ever prunes it; the sole removal path is an operator replacing the list through `PATCH /api/config`. |
| ActivePlatforms | []string | [] | `active_platforms` | Explicit override for UI display; consumed by `config.GetActivePlatforms`. |
| RefreshInterval | FlexDuration | 360 (minutes = 6h) | `refresh_interval` | Valid: 10-10080 minutes. Drives `AutoCookieService.StartPeriodicRefresh` (the browser timer) only — **not** `RefreshService`, whose interval is the hardcoded 30-minute default. |
| DpapiFallback | bool | false | `dpapi_fallback` | Windows-only. Opt-in: reads the user's REAL Chromium-family profile via `CryptUnprotectData` when the CDP refresh cannot acquire the managed profile. |

`cookie_file`, `auto_enabled` and `browser_profile_dir` are the three cookie keys in `restartRequiredKeys` (`internal/tui/settings.go`) and in `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`); `TestRestartRequiredListsAgree` pins the two lists against each other. What `auto_enabled` does *not* need a restart for is the manual triggers — they read it live.

#### [disk]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| WarnPercent | int | 90 | `disk_warn_percent` | Valid: 1-99 |
| CriticalPercent | int | 95 | `disk_critical_percent` | Must be > WarnPercent |

#### [updates]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| AutoCheckUpdates | bool | true | `auto_check_updates` |

#### [memory]

Bounds steady-state memory for the Go process and the embedded BotGuard sidecar. See `docs/spec/operations.md` "Memory Limits" for the full design rationale and tuning guide.

| Field | Type | Default | TOML Key | Notes |
|-------|------|---------|----------|-------|
| GoSoftLimitMB | int | 256 | `go_soft_limit_mb` | `debug.SetMemoryLimit`. Soft cap — Go GC ramps up near the limit but allocations succeed beyond it. 0 disables. |
| SidecarSoftLimitMB | int | 200 | `sidecar_soft_limit_mb` | RSS threshold. When sidecar RSS crosses, Moombox calls `triggerGC` JSON-RPC. 0 disables. |
| SidecarHardLimitMB | int | 512 | `sidecar_hard_limit_mb` | V8 `--max-old-space-size`. Hitting this OOM-aborts the sidecar (no graceful soft stop). Must be comfortably above SidecarSoftLimitMB. 0 uses V8's default (~512–1500 MB depending on host). |

#### [[channels]] (array of tables)

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| ID | string | "" | `id` | YouTube channel ID or Twitch username |
| Name | string | "" | `name` | Display name |
| Platform | string | "youtube" | `platform` | "youtube" or "twitch" |
| Enabled | *bool | nil (true) | `enabled` | nil defaults to true |
| Terms | ChannelTerms | empty | `terms` | Regex filter (string or map of named patterns) |
| NumDescLookbehind | *int | nil | `num_desc_lookbehind` | |
| OutputDirectory | string | "" | `output_directory` | Per-channel override |
| IncludeNonLiveContent | bool | false | `include_non_live_content` | |
| ArchiveWindowDays | *int | nil | `archive_window_days` | Per-channel override (1-3650) |
| ArchiveSlots | *int | nil | `archive_slots` | Per-channel override (1-100) |
| QualityPreference | string | "" | `quality_preference` | e.g. "1080p60", "best", "audio_only" |

#### [[notifications]] (array of tables)

| Field | Type | TOML Key |
|-------|------|----------|
| URL | string | `url` | Webhook URL |
| Tags | []string | `tags` | Filter tags |
| Events | []string | `events` | Event types to notify on |

### FlexDuration

`FlexDuration` is a custom type that stores a `float64` value whose unit is determined by context:

- When used as `feed_check_interval`: the value represents **minutes**.
- When used as `hide_finished_age_days`: the value represents **days**.

**Parsing rules:**

| Input | Type | Result |
|-------|------|--------|
| `10` | int/float | Stored as-is (10.0) |
| `"10"` | string (plain number) | Stored as 10.0 |
| `"30m"` | string (duration) | Parsed as 30 minutes; stored as 30.0 when unit is "minutes", or 0.0208... when unit is "days" |
| `"7d"` | string (duration) | Parsed as 7 days; stored as 10080.0 when unit is "minutes", or 7.0 when unit is "days" |

**Supported duration suffixes:** `ms`, `s`, `m`, `h`, `d`, `w`.

**Serialization:** `MarshalTOML()` writes a plain number. `MarshalJSON()` writes a plain number. This prevents the encoder from producing a nested `{Value = 5.0}` table.

**TOML deserialization:** `UnmarshalTOML()` handles int64, float64, string (plain number or duration string), and map (legacy `{Value = 5.0}` format from earlier serialization).

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
- TwitchCheckInterval: 1-3600 seconds (or nil)
- NumParallelDownloads: min 1
- SegmentWorkers: min 1, no max (values above `SegmentWorkersWarnThreshold`, 16, log a startup warning instead of failing validation)
- MaxVideoResolution: min 1
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
| `${title}` | Video/stream title | Filesystem-unsafe characters removed; Unicode preserved |
| `${id}` | Video/stream ID | No sanitization (IDs are alphanumeric) |
| `${channel}` | Channel name | Filesystem-unsafe characters removed; Unicode preserved |
| `${start_date}` | Stream start time (or now) | Formatted as `YYYYMMDD` |
| `${start_time}` | Stream start time (or now) | Formatted as `HHMM` |

Sanitization preserves CJK, Japanese kana, and full-width characters via a regex allowlist.

### Auto-Hash

On startup, if `network.password_hash` contains a plaintext password (detected by checking if it starts with the scrypt prefix), it is automatically hashed with scrypt and the config file is re-saved. This is a one-time migration.

---

## Cookies (internal/cookies/)

ONE `cookies.txt` on disk, three pieces above it. `CookieJar` (`internal/cookies/jar.go`) parses the file and answers typed questions about it. `RefreshService` (`internal/cookies/refresh.go`) validates the credentials in-process and rotates YouTube's session cookies out of `Set-Cookie` headers. `AutoCookieService` (`internal/cookies/autocookies.go` and its `autocookies_*.go` siblings) acquires credentials — through a browser, or browser-free out of a browser profile.

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

- `ExpiredAuthCookiesFor(platform, now)` has one production caller — the startup `Cookies loaded` line (`initServices`, `cmd/moombox/services.go:375-376`), which emits it per platform as `expiredYouTubeAuth` / `expiredTwitchAuth`. That log line is the only horizon-shaped output an operator ever sees.
- `AuthCookieHorizonFor(platform)` is an exported accessor with **test callers only**. Nothing in production calls it, so no UI and no log carries a horizon timestamp today.

Per-platform rather than YouTube-only because `RefreshService` has no Twitch refresh at all: `checkTwitchAuth` validates and never rotates, and nothing else writes a Twitch cookie back — `processYouTubeSetCookies` is YouTube-only and `trackedCookieName` refuses any origin off the Google platform. So the expired-count is the earliest warning a Twitch credential is running out.

Whether the browser path's twitch.tv navigation renews `auth-token` is **UNMEASURED**. The settling observation costs one run and has no surface yet: read the horizon through a test or a one-off call to `AuthCookieHorizonFor(PlatformTwitch)` before and after a browser refresh, and compare. A timestamp, never a value.

**ENOENT-as-empty is a ruling, not an oversight.** A missing file loads as an EMPTY jar, both maps cleared (`Load`, `internal/cookies/jar.go`). Deleting `cookies.txt` is how an operator logs Moombox out, and keeping the last good session in memory until restart would make a deliberate delete do nothing observable. The race objection does not apply, and `Load`'s doc comment carries the derivation: every writer goes through `writeFileAtomic` (`internal/cookies/autocookies.go`), which writes a temp file and renames without unlinking the destination; on Windows `os.Rename` is one `MoveFileEx(..., MOVEFILE_REPLACE_EXISTING)` with no `DeleteFile` ahead of it, and `os.ReadFile`'s open asks for `FILE_SHARE_READ|FILE_SHARE_WRITE` and *not* `FILE_SHARE_DELETE` — so a `Load` in flight makes the RENAME fail loudly instead of being made to read a missing file. On Linux `rename(2)` replaces the name atomically. Re-derive that paragraph if a writer ever stops going through `writeFileAtomic` or starts removing the target first.

**Essential YouTube cookies (20):** SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, __Secure-1PSIDTS, __Secure-3PSIDTS, __Secure-1PSIDCC, __Secure-3PSIDCC, LOGIN_INFO, VISITOR_INFO1_LIVE, VISITOR_PRIVACY_METADATA, YSC, __Secure-ROLLOUT_TOKEN, CONSENT, PREF.

**Essential Twitch cookies (4):** auth-token, twilight-user, login, name.

**Two predicate tiers per platform.** The STRICT pair answers "is a complete working set present right now"; the LOOSE pair answers "was this install ever configured for the platform", which is the question the auth-loss gate and the status badges actually ask.

| Platform | Strict | Loose | Loose name set |
|----------|--------|-------|----------------|
| YouTube | `HasYouTubeAuthCookies()` — SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO | `HasAnyYouTubeAuthCookie()` | `youtubeAuthCookieNames` (10) — SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, LOGIN_INFO |
| Twitch | `HasTwitchAuthCookies()` — auth-token present | `HasAnyTwitchAuthCookie()` | `twitchAuthCookieNames` — auth-token, twilight-user |

A file holding SAPISID with LOGIN_INFO cleared is a CONFIGURED platform with BROKEN credentials — exactly the state worth reporting — and the strict predicate reads it as never-configured. Every name in a loose set must also be in the corresponding essential set or `loadFrom` drops it before the predicate can see it; `TestAuthCookieNameListsDoNotDrift` pins that.

**`login` is deliberately NOT in `twitchAuthCookieNames`**, and the reason is the alarm the list drives, traced end to end in that variable's doc comment: `HasAnyTwitchAuthCookie` → `refresh`'s `hasTWCookies` → `shouldFireRecovery` → `OnRecoveryNeeded("twitch")` → `runState.handleRecoveryNeeded` (`cmd/moombox/monitor_callbacks.go`). The alarm does not require a failed validate — `checkTwitchAuth` (`internal/cookies/refresh.go`) returns a conclusive `(false, nil)` **without issuing any request** when auth-token is absent, and `shouldFireRecovery`'s first-conclusive-check arm then returns `cookiesPresent` verbatim. A file holding `login` and no auth-token would therefore fire "twitch auth lost" on the first check of every start. `twilight-user` earns its place instead: it is Twitch's own record of the signed-in user, and `mergeCookieFiles` can prune a lapsed auth-token out from under it. That variable's comment also ENUMERATES the four silent Twitch states rather than claiming a superlative — the list has been wrong twice — and names which of them `ChatDownloader.noteMissingLogin` (`internal/twitch`) now reports.

**Key methods:**

- `GetCookieHeaderFor(platform)`: builds a `Cookie:` header from one platform's rows and no other's, pairs sorted by name for determinism. `GetCookieHeader()` is the YOUTUBE one — every production caller is a YouTube request path — so Twitch rows no longer ride along on authenticated youtube.com requests.
- `GetCookieFor(platform, name)`: one platform's value by name. `GetCookie(name)` reads the **Twitch** jar; the generic name is a deliberate mismatch, since its sole in-tree consumer is `internal/twitch/auth.go` fetching `auth-token`, and routing it to the YouTube jar would de-authenticate Twitch silently (IRC treats an empty token as "connect anonymously").
- `GetTwitchCredentials()`: returns `auth-token` and `login` as a pair under ONE `RLock`, because `internal/twitch/chat_irc.go` builds one IRC handshake out of both and a pair read under two locks is not a pair.
- `GenerateAuthorizationHeader(origin)`: SAPISIDHASH + SAPISID1PHASH + SAPISID3PHASH, each `SHA1(timestamp + " " + sid + " " + origin)` with one timestamp shared across all three (`makeSidAuthorization`). Returns "" for any origin outside `allowedSAPISIDHASHOrigins`, which is defence in depth — Google's auth uses the origin as a shared secret, so a caller must never be handed a valid hash bound to an attacker-supplied one.
- `YouTubeIdentity()`: SHA-256 over `SAPISID + NUL + LOGIN_INFO`, "" when either is missing — SAPISID falling back to `__Secure-3PAPISID`, with that fallback inlined rather than delegated to `GetSapisid` so both reads happen under ONE `RLock` (the two must be kept in sync by hand). An opaque equality token for "which Google account is this" — never a credential, never displayed. LOGIN_INFO is the load-bearing half: SAPISID identifies a SESSION, not an account, so a fingerprint over it alone would be blind to an account switch. The rotating `__Secure-*PSIDTS`/`SIDCC` names are excluded because they would fire on every refresh cycle.
- `Reload()`: re-reads from the same file path; a no-op when the jar came from no file.

**Thread safety:** All methods are protected by `sync.RWMutex`. Nil-receiver-safe where a caller may legitimately hold none (`HasAnyYouTubeAuthCookie`, `HasAnyTwitchAuthCookie`, `ExpiredAuthCookiesFor`, `AuthCookieHorizonFor`, `YouTubeIdentity`).

### Refresh Service

`RefreshService` periodically validates cookies against the actual platform APIs and refreshes YouTube session cookies from `Set-Cookie` headers.

**Lifecycle and the single-flight.** Three entry points share one body, `refresh(ctx, allowFallback)` (`internal/cookies/refresh.go`); `allowFallback` is the only thing that separates them.

- `Start(ctx)` runs the initial pass **synchronously on the caller's goroutine** with `allowFallback=false`, wrapped in its own inline `recover` — `cmd/moombox`'s `run()` blocks on it before the web server binds, so an unrecovered panic there takes the process down at boot with no dashboard, no TUI and no log surface. It then launches the ticker goroutine, which carries the same recover.
- `CheckNow(ctx)` is `POST /api/cookies/recheck` and the TUI's `R C`, also `allowFallback=false`: it runs on a handler goroutine and must not buy a full page fetch.
- `doRefresh(ctx)` is the ticker, and the only path allowed `allowFallback=true`.

All three single-flight on `RefreshService.refreshInFlight` (guarded by `rs.mu`). A second caller is a **no-op** that returns `started=false` and logs at Debug — it does not queue and does not wait. It is **never** a `RefreshDeclinedCauses` member: that vocabulary belongs to `AutoCookieService`'s browser refresh and is pinned across three consumers. A dropped ticker tick waits a full interval rather than doubling up. A caller that has just rewritten `cookies.txt` and wants *that file* re-verified cannot be given that guarantee — the in-flight pass may have read the old file — so the two callers in that position log the skip at **Info**: the post-recovery re-check (`handleRecoveryNeeded`, `cmd/moombox/monitor_callbacks.go`) and the post-`R F` re-check (`cmd/moombox/tui_wiring.go`), both saying "status may lag until the next refresh". The `POST /api/cookies/recheck` handler ignores the bool on purpose — its payload is a status snapshot, not a claim that this request produced it. Before the guard existed, a manual recheck landing during a ticker pass produced two guide fetches, two `Set-Cookie` merges and two interleaved `updateCookieFile` rewrites of the same file. An operator counting passes from clicks will therefore occasionally see a recheck produce no new pass in the log; that is the guard, not a broken button.

**Every `rs.mu` section inside `refresh` releases through `defer`.** This is a standing rule, not a style preference. The guard-release defer takes `rs.mu`, and `rs.mu` is a plain non-reentrant `RWMutex`: a panic unwinding with the write lock held would block that defer forever, park the goroutine holding `rs.mu`, and turn a loud crash into a silent hang in which every later `GetStatus()` blocks. The status update is scoped into a func literal for exactly that reason. Two unexported test seams, `refreshPassHook` (outside the lock) and `refreshLockedHook` (inside it), exist because the two windows need opposite things.

**Validation endpoints:**

| Platform | Method | Endpoint | Auth Check |
|----------|--------|----------|------------|
| YouTube | POST | `youtubeGuideURL` = `www.youtube.com/youtubei/v1/guide?prettyPrint=false` | Explicit login marker in the response, or inconclusive — see below |
| Twitch | GET | `twitchValidateURL` = `id.twitch.tv/oauth2/validate` | HTTP 200 = valid, 401 = conclusively invalid, anything else = inconclusive |

There is **one** guide URL. Two package vars used to name it — `youtubeGuideURL` and `youtubeGuideRefreshURL`, described as different endpoints kept apart on purpose — and they were byte-identical; folding the two guide functions into one exchange made the duplicate visible. Both vars (rather than consts) exist solely so tests can point them at an `httptest` server.

Provenance is asserted before status and before body, on both platforms: `authResponseIsOurs` (`internal/cookies/refresh.go`) requires the answering request's scheme and raw `host:port` to match what was sent AND the credential header (`Cookie` for YouTube, `Authorization` for Twitch) to still be present. Go strips manually-set credential headers on a cross-hostname redirect and the decision is sticky, so `origin → wall → origin` lands back on the right host carrying an uncredentialed body. The check is non-vacuous only because both callers refuse the empty-credential case before fetching, and it means anything only because `cookiesHTTPClient` carries no `http.CookieJar` — `TestCookiesHTTPClientCarriesNoCookieJar` pins that.

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

`youtubeGuideAuthVerdict` (`internal/cookies/refresh.go`) reads the body with `encoding/json` and falls back to literal substring needles (`guideLoginMarkersIn`/`guideLoginMarkersOut`) only when the body is not valid JSON at all. `loggedIn`/`loggedOut` are `*bool` because a real anonymous reply omits `loggedIn` entirely, and a plain `bool` cannot tell "the flag said false" from "the flag was absent". The `logged_in` param's `value` is `json.RawMessage`, decoded only for params whose key already matched, so one unrelated param gaining a non-string type cannot collapse the whole body to the fallback. The fallback caps the promoted string at `authBodyFallbackLimit` (16 KB) so session material deeper in the payload cannot reach a log line. An unreadable marker resolves to `errGuideLoginMarkerUnreadable`, which deliberately does **not** wrap `ErrAuthCheckNotAttempted`: a request did leave the process here, and `autocookies_profile.go`'s `attempted` flag turns on that distinction.

**One guide exchange, one writer.** `youtubeGuideExchange` (`internal/cookies/refresh.go`) makes the POST, reads the body to a verdict, closes it, and hands the verdict *and* the response back. It never writes anything. Two callers:

- `checkYouTubeAuth` — the VERIFY path, exported as `CheckYouTubeAuth` and wired into `AutoCookieService.VerifyYouTubeAuth` (`cmd/moombox/services.go`), where `checkPlatformAuth` runs it on the **rollback** decision of a profile import. It discards the response. A shared exchange that merged `Set-Cookie` headers itself would write the jar from the very response being used to judge the import.
- `checkAndRefreshYouTube` — the sole writer. Only on an authenticated, readable reply does it call `processYouTubeSetCookies`. Every error path and the never-configured gate return a nil response, so "a reply we could not read is not a reply anyone may write the jar from" is a fact about the return values rather than a rule to remember.

`youtubeGuideExchange`'s three entry gates encode one rule, and only the FIRST may answer `(false, nil)`: nothing configured at all is a silent negative; configured-but-no-request-could-be-built errors with `ErrAuthCheckNotAttempted`, because a check that did not happen is not dead credentials. A jar with SAPISID and a cleared LOGIN_INFO is configured with broken credentials and its verdict has to come from YouTube.

**Set-Cookie ADMISSION, by parsed attributes.** `admitSetCookie(sc, origin)` (`internal/cookies/refresh.go`) is the outer layer: a header it turns down never reaches the write path in any form. The substring pre-filter that used to open this loop is **gone** — it ran before `Domain=` was parsed, dropped every legitimate unscoped first-party rotation (RFC 6265 §4.1.2.3 host-scopes a Domain-less `Set-Cookie` to the responding host), and was never the guard it looked like, since `x=youtube.com; Domain=evil.tld` passes a substring test on its value. Admission now happens after the whole attribute list is read:

1. **Row-breaking characters** in the name, the value or the domain are refused outright (`hasRowBreakingChar`, `rowBreakingChars` = tab, CR, LF, NUL). Only the TAB is a live vector — `net/textproto` cannot deliver CR, LF or NUL inside a header value — and the other three are defence in depth for a write into a line-oriented file. This governs only what *this writer* may add; `CookieJar.Load`'s tolerance of tab-carrying rows is unchanged.
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

Other write-path rules (`updateCookieFile`, `internal/cookies/refresh.go`):

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

`SetExpectedPlatforms(platforms)` seeds the previous auth state from persisted config platforms so auth loss is detectable after a restart. `OnAuthRecovered` fires on the inverse transition. `OnCredentialsChanged` is separate and not a weaker `OnAuthRecovered`: a job parked because the signed-in account lacks a membership parked while auth was HEALTHY, so swapping accounts produces no auth transition to ride. `shouldObserveCredentials` and `advanceIdentityBaseline` (both pure) govern it — the baseline advances only on a check that was conclusive **and** authenticated, so a stale intermediate export cannot consume the edge, and the `baseline == ""` case fires once per process on purpose so an offline cookie swap is noticed at all.

**Two-tier liveness, and its pilot is DISARMED.** Tier 1 is the auth check above. Tier 2 is `ObserveLiveness(platform, loggedIn)`, fed by two YouTube producers — the per-channel membership probe (once per configured channel per feed cycle) and the channel-independent `FallbackLiveness` probe injected by `cmd/moombox` (this package cannot import `internal/youtube`). Callers must filter their own inconclusive results out; reaching the method means "YouTube told us", not "we asked". Twitch has no tier-2 producer.

`const livenessRecoveryArmed = false` (`internal/cookies/refresh.go:632`) gates whether a tier-2 verdict may invoke `OnRecoveryNeeded`. It is false today. The observation, the dedupe and the freshness accounting all happen and are logged; only the last step is withheld, and `ObserveLiveness`'s single log line carries `wouldFireRecovery` and `armed` so the pilot can be read as evidence. Arming is a deliberate, separate change — not a side effect of wiring something else — and its risk is not scoped to `auto_enabled` installs: on `auto_enabled = true` a spurious verdict drives a headless browser and notifies unless it races the auto-cookie single-flight; on `auto_enabled = false` there is no quiet case at all — `handleRecoveryNeeded` returns without calling the refresher, so a synchronous "Cookie Re-Authentication Required" (TypeError) fires every time, at a 30-minute per-platform cooldown, to the population least able to reach the remedy it names.

Arming is an owner decision taken after a soak of the log-only pilot, not a change made in passing. One question it has to answer first is still **open**: what a genuinely dead session should do once the gate is true. Tier 1 alone notifies once per process — `shouldFireRecovery`'s witnessed-transition arm needs `prevAuth` to have been true, and the first conclusive negative clears it — whereas an armed tier 2 re-fires every `livenessRefireWindow` for as long as the session stays dead. Whether that periodic re-alarm, a once-per-process one, or a back-off is wanted has not been decided.

Three maps, deliberately separate (`internal/cookies/refresh.go`):

| Map | Written by | Read by |
|-----|-----------|---------|
| `lastLivenessObserved` | conclusive verdicts, **both** directions | `livenessObservedRecently` — the sole gate on paying for the fallback probe |
| `lastRecoveryDecided` | a signed-out verdict that cleared the dedupe (`recordLiveness`), and `noteRecoveryDecided` from a tier-1 fire | `recordLiveness`'s `livenessRefireWindow` check. A `LoggedIn` observation must never write here, or a healthy verdict could swallow a dead one from another channel in the same cycle |
| `lastLivenessKnown` | every outcome including `livenessInconclusive` | log-level selection only (`notable`) |

`noteRecoveryDecided` is one-directional: the tier-1 fire stamps the map but never consults it, because suppressing the tier-1 fire would change behaviour that predates this signal entirely. `recordInconclusiveLiveness` touches neither of the other two maps — recording an observation would make the next cycle skip the probe, silencing the signal for as long as it keeps failing.

`livenessFreshWindow` (25 min) bounds how old the last conclusive observation may be before a periodic pass pays for the fallback probe. It **must** be strictly shorter than the refresh interval, because the fallback records its own answer through the same method — at one full cadence the probe would suppress itself on alternate cycles, halving coverage with no symptom. `NewRefreshService` enforces that against the interval it is actually handed, replacing anything at or below the window with the default and warning with both numbers. The lower bound is an *assumption about configuration*, not an invariant: the skip only works while membership observations arrive more often than the window expires, so an install with `monitors.feed_check_interval` above ~25 minutes pays for the fallback on roughly every other cycle. That degradation is bounded and one-directional.

**`authStatusChanged` is a CONTRACT, not an observation.** It is the `OnAuthChange` gate (`internal/cookies/refresh.go`) and compares six fields: the two auth booleans, the two cookies-present flags, and the two `RefreshVerdict`s. It deliberately excludes `YouTubeError`/`TwitchError`, whose text can vary between two occurrences of the same outcome, so comparing them would fire the callback on churn no verdict transition accompanies. The rule is stated forwards: **no `OnAuthChange`-driven surface may render the two strings; per-request surfaces may.** Both of today's readers are per-request — each pulls a `GetStatus()` snapshot it asked for — so neither depends on this callback and neither can go stale on screen. Widening the gate is the *precondition* for a push-driven surface that renders them, and is a deliberate change with its own cost. The verdicts and the cookies-present flags have to be in the gate: a platform going from conclusively-rejected to could-not-check leaves both booleans false, and on a boolean-only gate that badge transition was silent.

`AuthStatus` (`internal/cookies/refresh.go`) is never marshalled — every consumer hand-projects it — and **every field has a reader**, which is a property to keep. A `LastCheck` string was removed rather than wired: no projection carried it, and the obvious misreading ("the credentials were valid as of this time") is not what the timestamp of a pass that may have concluded nothing says. Anything re-added needs a reader in the same change, and if it moves on every tick it also needs a line in `authStatusChanged`'s exclusion list.

**Timing:**

- `RefreshService` interval: `defaultRefreshInterval` = 30 minutes. `NewRefreshService(jar, 0, log)` is how `initServices` constructs it, so nothing in production feeds the interval parameter; `cookies.refresh_interval` drives the *auto-cookie* periodic refresh instead.
- `authCheckTimeout` = 15 seconds per platform.
- `livenessRefireWindow` = 30 minutes (its own constant on purpose — it is neither the notification cooldown in `wireMonitorCallbacks` nor `defaultRefreshInterval`, however the three numbers line up today).
- `livenessFreshWindow` = 25 minutes.
- Initial check runs synchronously on `Start()`; tier-2 coverage therefore begins one cadence in.

### Auto-Cookie Service

`AutoCookieService` acquires credentials into `cookies.txt` — through an interactive browser login, through a headless browser refresh, or browser-free by importing a browser profile directly.

**What `cookies.auto_enabled` means.** Two independent liveness mechanisms on two independent timers, **not** a primary and a fallback. The in-process Go refresh (`RefreshService`) always runs, with the monitors and its own timer. The headless-browser refresh is a **much slower** second timer that exists only when the flag is on. The flag owns that timer, the one automatic recovery attempt, and — the exception this table has to name — the `SetExpectedPlatforms` read at `cmd/moombox/main.go:276-278`. Nothing else.

| Surface | Mechanism | Gated on `auto_enabled`? |
|---------|-----------|--------------------------|
| `RefreshService` (monitors + own timer) | in-process | never |
| `StartPeriodicRefresh` | headless browser | yes — it *is* that timer |
| automatic recovery (`OnRecoveryNeeded` → `handleRecoveryNeeded`) | headless browser, one attempt | yes |
| `SetExpectedPlatforms` seeding (`cmd/moombox/main.go:276-278`) | — | yes |
| `R F` / Web shift+click / the Settings-page twin | best-available ladder | **no** — the flag only picks the rung, and never causes a decline |
| `R C` / `POST /api/cookies/recheck` | in-process | never |
| `StartSetup` (interactive login) | browser | never — acquisition, and an explicit gesture |

**The periodic timer is `gateExempt`.** `browserGatePolicy` (`internal/cookies/autocookies.go`) has two values: `gateApplies` (the zero value, so anything that forgets to say gets the safe answer) for every caller acting on a live operator intention, and `gateExempt` for `StartPeriodicRefresh`'s goroutine and nothing else. `main.go` starts that loop only when the flag was true at boot, so the flag has already been consulted; re-asking it per tick would leave an operator who switched it off without restarting with the timer still running *and* silently switched to browser-free imports of a profile nothing changes between ticks. Flipping the flag off at runtime therefore leaves the timer launching browsers until restart, **by ruling** — the restart-required label both UIs carry is the honest cover. Do not "fix" it.

**`R F` is a three-rung ladder and never dead-ends.** `R F` (TUI), the dashboard header's shift+click, and the Settings page's "Refresh cookies from browser profile" button are one gesture: refresh by the strongest means available. The Settings twin exists because a modifier key does not exist on a phone or tablet, which left a mobile-only operator with dead cookies, an updated profile and no trigger at all on exactly the workflow designated for Docker; it calls `app.autoCookieRefresh()` directly rather than adding a second implementation.

1. Browser launching allowed AND a browser available → launch the headless browser, refresh the profile, import.
2. No browser launch (flag off, or no browser present) but a profile IS present → import from the profile immediately.
3. No browser profile at all → run what `R C` runs, and say so.

Rung 3 is one exported predicate, `cookies.IsNoBrowserProfile` (`internal/cookies/errors.go`), so the two surfaces cannot diverge: it is exactly `ErrProfileNotFound` or `ErrNoBrowserFound`, both from the same pre-work missing-directory check. The TUI branches on it in `internal/tui/app_update.go` and returns `recheckCookiesCmd()` — R C's own command, not a second implementation — so the sentence leads a real refresh. The Web half (`autoCookieRefresh`, `web/public/app.js`) branches on the STATUS (404 for `ErrProfileNotFound`, 424 for `ErrNoBrowserFound`), never on the prose, and then awaits `recheckCookies()`. The two sentences differ **on purpose**, each naming its own surface's affordance: the TUI's is the owner's copy verbatim, `No browser profile found, running R C instead...` (ellipsis included), and the Web's is `No browser profile found, running a normal cookie refresh instead...`; `TestRungThreeSentencesDivergeByDesign` asserts the divergence. The ladder still declines nil-error on the running-service causes in `RefreshDeclinedCauses` (a setup or another refresh already in flight, or no platform with cookies worth refreshing) — unchanged and correct. `R C` is never gated.

Rung 3 deliberately EXCLUDES every profile-import failure — `ErrProfileDirUnreadable`, `ErrProfileNotADirectory`, `ErrCookieDBNotFound`, `ErrCookieDBLocked`, `ErrCookieDBUnreadable`, `ErrNoCookiesInProfile`. Those mean the profile IS there and is wrong in a diagnosable way, each from a pass that RAN, and each carries the only guidance the operator has. Folding them into the fallback would replace real diagnosis with a recheck that cannot fix any of them.

**Every AUTOMATIC browser-free import runs only when there is no `cookies.txt` to lose.** `automaticImportGuard` (`internal/cookies/autocookies_profile.go`) is that rule, and it is ONE rule with exactly two automatic callers: `decideStartupSeed` (the boot import) and `StartPeriodicRefresh`'s tick when that tick would be browser-free. "Nothing to lose" means **absent, or present with zero cookie rows**; an **unreadable** file ABORTS (`autoImportCookieFileUnreadable`) rather than counting as absent, and `StartProfileSeed` Warns on that one stand-down because it is the operator-actionable case.

The asymmetry is the whole argument: a false "nothing to lose" imports over credentials and the operator may not find out until a recording fails, while a false "something to lose" costs one keypress. So the predicate does not need to be accurate — it needs zero false "nothing to lose" answers. `countNetscapeCookieRows` (`internal/cookies/autocookies_profile.go`) therefore **must over-count**: it counts lines that are neither blank nor plain comments, so a malformed row, an unrelated domain and an expired row all count. Over-counting can only produce the cheap error. Any replacement must keep that direction, which is also why "present but holding no auth cookies for either platform" is ruled out as a definition — deciding it needs a per-platform predicate, and a wrong one fails in the expensive direction.

The guard is **not for the manual triggers**: `R F`, shift+click and the Settings twin must keep importing over whatever `cookies.txt` holds, because replacing a live cookie file out of a profile the operator just updated by hand *is* the gesture, and it is the only path a container has. And it is **not for the recovery path**: `OnRecoveryNeeded` and the worker's `OnCookieRefreshNeeded` reach `RefreshCookiesDetailed` without passing through it, because recovery fires only on a conclusive not-authenticated — refusing the one automatic import most likely to fix the problem, on the grounds that a file exists which has just been proven not to work, would be backwards. That exemption stops being safe if a recovery producer is ever added that can fire on an INCONCLUSIVE verdict. The two-platform case (only one platform died) is covered not by the guard but by `RefreshCookiesDetailed`'s own abort/merge/rollback, which re-checks at write time: it verifies each platform BEFORE the import, and `platformsToRestore` (`internal/cookies/autocookies_profile.go`) hands back the rows of any platform that either verified before and failed after (a regression) or had credentials before and could not be checked after (inconclusive — committing a set nobody could evaluate over one that may be fine is a bet with no upside). A platform that was already dead is deliberately not restored.

**`StartProfileSeed(ctx)`** (`internal/cookies/autocookies.go`) runs AT MOST ONE browser-free import shortly after start, and `main.go` calls it **unconditionally** — it is not under the flag. The flag owns a *repeating* read of a profile nothing changes between ticks; a boot is the one moment a mounted profile plausibly did change, because something replaced it while the process was down. Its safety condition is the cookie file, not the flag, and lives in `decideStartupSeed`. It returns immediately; the import runs on its own goroutine after `profileImportStartupDelay` (15 s) and **re-asks** `decideStartupSeed` after the wait, because an interactive setup finishing or a hand-dropped `cookies.txt` both write the file the first decision was made about. `shouldSeedFromProfileAtStartup` is deleted.

**Docker guidance.** Leave `auto_enabled` off, update the mounted browser profile, then press `R F` (or shift+click the header button, or the Settings-page twin). That is the designated workflow on a headless host: no browser is launched, the profile is imported directly, and the manual triggers are exempt from `automaticImportGuard` precisely so it works over an existing file. `cookies.txt` must live *inside* a mounted directory rather than being bind-mounted as an individual file — `writeFileAtomic`'s rename cannot replace a single-file mount.

**The `lastError` write policy.** `lastError` (`AutoCookieService`, `internal/cookies/autocookies.go`) is the last thing a cookie pass concluded that the OPERATOR has to act on, published as `AutoCookieStatus.LastError`. One rule: **a write is allowed only where THIS pass established the thing it is asserting.** Setting asserts a problem; CLEARING asserts that whatever was recorded is not wrong any more, and that is the half that keeps being written by paths with no basis for it.

- `setError` is the single SET funnel and the only place a message enters the field. Every exit that returns an error from a cookie pass sets — `FinishSetup`'s empty-profile, read-failure, merge-abort, mkdir, write and jar-load exits, and the refresh's import-failure, merge-abort, credential-loss and verification-failure exits.
- **Two exits deliberately do NOT set**: the guard clauses at the top of `FinishSetupDetailed` (`ErrNoSetupInProgress`, `ErrSetupCancelled`). No pass has run when they fire, so there is no failure to see afterwards, and the caller gets the answer synchronously in the same dialog. "A pass" is the boundary; a guard that refuses to start one is not an exit from one.
- Three CLEARS, each earned: `StartSetup`'s slot claim (a new attempt is starting, so the recorded message belongs to an attempt that is over — the one clear about intent rather than evidence); `RefreshCookiesDetailed`'s `case renewed` in the any-platform-verified arm (the pass actually produced the credentials it verified); and that switch's "nothing to verify" branch, kept with a note because no route to it has been found and "I could not find a route" is not "there is none". The `default` beside `renewed` deliberately does not clear — a pass whose browser did nothing has established that the credentials on disk work, not that the refresh mechanism does.
- The loss branch of the same arm writes `s.lastError` directly, because a partial success still has to report the platform that was lost.
- **`cleanup()` / `cleanupLocked()` MUST NOT clear it.** `cleanup` runs on every setup exit path including the failed ones — `FinishSetup` calls `setError` and then `cleanup` on each failure exit — so clearing there would erase the message microseconds after it was written. Pinned by `TestCleanupAfterAFailedSetupKeepsLastError` and `TestFinishSetupRecordsTheFailureItReturns`.

It has two readers: the Web settings panel's `lastError` line and the TUI's `R C` result line (`Last cookie error:`).

**`fetchedNoCredential`** (`RefreshCookiesDetailed`, `internal/cookies/autocookies.go`) is a NEW flag, never a redefinition of `fetchedRows`: rows came back and **not one of them is a session credential** — a signed-out browser profile, or one set to clear cookies on exit and re-seeded with `YSC`/`VISITOR_INFO1_LIVE` by the navigation this pass just made. Read as either neighbouring case it was wrong: "the browser profile contained no cookies" is false, and "auth verification failed — manual re-login required" says nothing an operator can act on. It is measured on what THIS PASS fetched, before the merge folds the previous `cookies.txt` in, and it is computed with `netscapeCookiesHoldACredential` — which loads the text into a THROWAWAY jar and asks the jar's own loose predicates, so "is this a credential" has one answer across the package. Overloading `fetchedRows == 0` was rejected because that counter's deliberate over-counting is load-bearing for the import guard.

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

The anti-automation flags are mirrored on the headless launch deliberately: YouTube raises fraud scores for a browser that advertises itself as automated, which would invalidate the very cookies the pass is refreshing. `dangerousProfilePathSubstrings` (`internal/cookies/autocookies.go`) refuses a configured profile directory that belongs to a real installed browser, so a hostile config cannot launch Chrome against the user's actual signed-in profile and exfiltrate it through the `cookies.txt` export.

**Cookie extraction:**

| Engine | Source | Method |
|--------|--------|--------|
| Firefox | `cookies.sqlite` in the profile dir | `readFirefoxCookies` (`internal/cookies/autocookies_firefox.go`) SNAPSHOTS the DB **together with its `-wal` sidecar** into a temp dir and queries the copy; copying without the sidecar silently returns rows that are missing every uncommitted write. Falls back to querying in place if the snapshot itself fails, and retries up to 5 times at 500 ms for WAL lock contention and torn snapshots, breaking early on anything non-retryable. NULL columns are defaulted and unusable rows counted, both reported by the caller |
| Chromium | the live browser over CDP | `cdpGetCookiesAsNetscape` (`internal/cookies/autocookies_chromium.go`) runs a three-tier ladder: browser-level `Storage.getCookies`, then per-page `Network.getAllCookies`, then per-page `Network.getCookies`. The gate between tiers is the RELEVANT row count, not the raw one, so a tier-1 answer full of other sites' cookies does not stop the ladder |
| Chromium (opt-in fallback) | the user's REAL profile under `%LOCALAPPDATA%`, decrypted with `CryptUnprotectData` | `dpapiExtractAsNetscape` (`internal/cookies/autocookies_dpapi.go`). Reached only when the browser refresh already returned an error, `cookies.dpapi_fallback` is on, and the resolved browser is non-Firefox — DPAPI launches nothing, so it sidesteps "Chromium is already running our profile" entirely. Off by default: it reads the user's actual signed-in cookies, so it is a privacy surface they have to enable consciously |

An empty result and an unanswered read are different facts. `cdpCookieReadOutcome` distinguishes them: `ErrBrowserLadderBlocked` when a structural failure (the `/json` target listing) stopped the fallbacks from running at all, `ErrBrowserReadUnanswered` when no query answered. Neither is `IsNoBrowserProfile`, so neither triggers rung 3 — both come from a pass that RAN, and a plain recheck cannot fix either. `writeBrowserReadError` (`internal/web/routes/cookies.go`) maps the first to **409** and the second to **502**, each with a machine-readable `cause`, and passes the composed message through verbatim.

**The DPAPI fallback reads exactly ONE profile.** It used to walk every profile `dpapi.FindBrowserProfiles` returned and merge them before dedup, so with two signed-in Chromium profiles `deduplicateAndFormat`'s bare-name dedup let whichever profile was listed LAST silently win each cookie name — an order-dependent coin flip that could pair a SAPISID from profile A with a LOGIN_INFO from profile B. Selection now: filter by the configured browser type (empty **and** `"chrome"` both mean "every profile is a candidate" — the Web UI's only Chromium option is literally the whole family; a type with no layout in `dpapi.KnownBrowserFamilies()` also falls back to every profile, logged at Debug, because that is a coverage gap rather than a missing browser), score each candidate with `dpapiProfileScore` using the same loose/strict predicates `jar.go` keeps, take the highest, break ties by scan order with an Info line naming every tied profile, and discard the rest whole. Known limitation, deliberately not built here: the score sums both platforms into one number, so YouTube-on-profile-A / Twitch-on-profile-B still loses one platform — deterministically and logged, instead of silently. **One question about this path is unmeasured**, and it is written out in `internal/cookies/dpapi/dpapi_windows.go`: whether a `mode=ro` read of a live WAL-mode Chromium `Cookies` DB returns stale rows — that is, whether `modernc.org/sqlite`'s pure-Go reader merges committed `-wal` frames the way the C library's WAL reader protocol does. Lock conflicts are already handled and degrade to a clean error; staleness would not, because a read that misses the `-wal` returns whatever was last checkpointed into the main file — well-formed rows that nothing errors on. Settling it needs a signed-in browser writing to the DB on the same machine. Until that runs, the Firefox path's copy-the-sidecar shape is deliberately not applied here.

**CDP readiness** (`waitForCDP`, `internal/cookies/autocookies_chromium.go`) polls `http://127.0.0.1:<port>/json/version` with exponential backoff — 200 ms doubling to a 2 s cap — until `cdpPollTimeout` (15 s). It waits for the debugging endpoint to come up; it does **not** poll for login completion. Interactive login completion is the operator pressing "I'm Logged In", which calls `FinishSetup`. Other CDP budgets: `cdpExtractTimeout` 30 s, `cdpRefreshTimeout` 60 s (cold start plus sequential per-platform navigations plus extraction, all sharing it), `cdpNavigateTimeout` 30 s per `Page.navigate` + `loadEventFired` wait. A navigation whose read loop hits its deadline without ever seeing `Page.loadEventFired` returns `errNavigateBudgetExhausted`. `navigateAllPlatforms` treats that per platform as NOT OBSERVED rather than as a failure — it never joins `navFailures` — and lets it flip the pass's "navigated" answer only when it is **every** platform's outcome. One platform timing out beside a sibling that fired its load event is a slow page on a browser that demonstrably works; do not loosen "every" to "any".

**Lock file cleanup.** Before launching, the service removes stale lock files that would otherwise prevent a launch:

- Chromium (`cleanChromiumLockFiles`): `lockfile`, `SingletonLock`, `SingletonSocket`, `SingletonCookie`, plus the `Singleton*` and `*lockfile*` globs for variants newer builds leave behind.
- Firefox (`cleanFirefoxLockFiles`): `parent.lock`, `.parentlock`.

Both go through `removeStaleLock`, which **skips any file touched within `lockFileFreshThreshold` (5 s)** — a truly stale lock from a crashed run is older than that, while one held by a live browser is not, so this cannot yank the lock out from under a running instance.

**Orphaned temp files and directory permissions.** `writeFileAtomic` writes `<base>.<random>.tmp` beside the target and renames; a crash, a kill or a panic in between leaves a full copy of `cookies.txt` — the highest-value secret in the app — under a name nothing reads. `sweepStaleCookieTempFiles` (`internal/cookies/autocookies.go`) removes those, ONCE per process (`cookieTempFileSweepOnce`, wired at `NewAutoCookieService` because that is the one place holding the real cookie path up front), matching only `<base>.*.tmp` in the cookie file's own directory and only when older than `cookieTempFileMaxAge` (1 hour). Age is the only guard against sweeping a write in progress. Every failure is Debug-logged with the path only and left for the next start.

`tightenCookieDirOnce` applies `utils.ApplyUserOnlyDACL` to the cookie file's parent — `icacls` on Windows, a real `chmod` to 0700/0600 on Linux (not a no-op there). It is **memoised on SUCCESS, not on attempt**, with three states per directory (absent = not started, `dirTighteningInFlight`, `dirTighteningDone`): a transient failure or a panic mid-apply deletes the entry so the NEXT cookie write retries, rather than disabling hardening for the rest of the process because the failure was demoted to a Debug line. Cost if it fails permanently on a host: one extra ~30-80 ms shell-out per cookie write instead of one per process, bounded by the write cadence. No failure cap and no backoff — either would be a mechanism to contain a mechanism.

**First-run platform detection is sidecar-first.** `detectCookiePlatforms(meta, jar)` (`cmd/moombox/services.go`) decides `cfg.Cookies.Platforms` when the config has none. `cookies.meta.json`'s `Platforms` — the on-disk record of what an import ACTUALLY verified — wins outright whenever non-empty, and is never unioned with a guess. Only when the sidecar is absent, unreadable or empty does it fall through to the jar, and then to the **loose** predicates (`HasAnyYouTubeAuthCookie` / `HasAnyTwitchAuthCookie`), not the strict pair: a `cookies.txt` holding SAPISID with LOGIN_INFO cleared is a configured platform with broken credentials, and persisting "unconfigured" for it made every downstream gate that reads `Platforms` — the auth-loss notification, the `SetExpectedPlatforms` seeding, the recovery path — treat it as never having existed, permanently, since nothing automatic re-runs this once `Platforms` is non-empty.

**Platform URLs:**

| Platform | Login URL (`StartSetup`) | Refresh URL (`platformRefreshURLs`) |
|----------|--------------------------|-------------------------------------|
| YouTube | `accounts.google.com/ServiceLogin?service=youtube` | `www.youtube.com` |
| Twitch | `www.twitch.tv/login` | `www.twitch.tv` |

**Budgets:** `processTimeout` 30 s per browser launch, `authVerifyTimeout` 15 s for ONE verification window covering BOTH platforms, `refreshOverallBudget` 2 minutes end to end. These are coupled — raising `processTimeout` without raising `refreshOverallBudget` makes the outer context cancel the second platform's launch mid-flight instead of granting it the budget it was just given.

---

## File Output

### Staging Directory

In-progress downloads write to the staging directory (default: `./staging`). Files are moved to the output directory only after successful muxing. This prevents incomplete files from appearing in the final output location.

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
3. If validation fails, the resume file is discarded. For Twitch live
   (`StopOnGap`), a discarded/corrupt state with staged data present does
   NOT truncate — the engine returns `ErrGapDetected` so the orchestrator
   muxes the staged data as a finished part and continues fresh.
4. If the resume file is missing but `StartSeq > 0` in the database, the
   database state is used as a fallback (less precise but allows recovery).

**Lifecycle:**

- Created during download.
- Updated periodically during download.
- Cleared (deleted) only when the stream ends naturally and cleanly.
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
    LastTimestampUsec string   `json:"lastTimestampUsec"`
    MessageCount      int      `json:"messageCount"`
    Continuation      string   `json:"continuation"`
    Timestamp         int64    `json:"timestamp"`
    VideoID           string   `json:"videoId"`
    RecentIDs         []string `json:"recentIds"`
}
```

Saved as `<chat_file>.resume.json`. Updated every 10 seconds during chat download.

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

### Ring Buffer

A fixed-size ring buffer (200 entries, `defaultRingSize`) holds the most recent log lines in memory. Used to populate the TUI log panel and Web UI log view on initial load (before real-time subscription kicks in).

`GetRecentLines()` returns lines in chronological order regardless of current buffer position.

### Per-Job Log Buffers

The LIVE per-job log pipeline is the database's: `Logger.Subscribe()` feeds `db.RouteLogToJobs()`, served by `db.GetJobLogs` (capped at 200 lines, trimmed to 100; `db.PruneJobLogs(activeIDs)` drops buffers for inactive jobs).

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
├── version.txt                      ("node@vX.Y.Z sha256@<sha>" — cache-invalidation key)
├── src/
│   └── server.js                    (~250 lines, JSON-RPC server)
└── node_modules/                    (production deps: bgutils-js, jsdom, transitives — ~17 MB)
```

### Lifecycle

- **First launch:** `extractIfNeeded(cacheDir)` creates the dir, applies `utils.ApplyUserOnlyDACL` to tighten permissions to current-user-only (matches the config-dir hardening), gunzips `node.exe.gz`, gunzip+tar-extracts `sidecar.tar.gz` using stdlib `archive/tar` + `compress/gzip` (no system tar required), writes `version.txt` last.
- **Subsequent launches:** Compares the on-disk `version.txt` against the embedded `bgembed.Version`. On match AND key files present, skips extraction. On mismatch (Node version bump, sidecar JS update), re-extracts the whole payload.
- **Tar-slip defense:** Rejects any tar entry whose target path escapes `cacheDir`.
- **DACL hoist:** Runs even on cache-hit so users upgrading from v2.5.x (whose pre-existing dir was created with the looser inherited ACL) get the tightened DACL on the next launch.

### Cleanup

Not currently auto-cleaned. The cache survives Moombox uninstall — operators wanting to reclaim the ~100 MB can manually delete the dir. A `moombox uninstall-data` CLI subcommand is planned but not in v2.6.0.

---

## Cross-References

- **[architecture.md](architecture.md)** -- Batch coalescing as a concurrency pattern; pub/sub as an inter-component communication mechanism; service initialization order (Config -> Logger -> Database -> ...).
- **[security.md](security.md)** -- Password auto-hashing in config; client_tokens table and scrypt token hashing; cookie file permissions.
- **[platform-services.md](platform-services.md)** -- How YouTube and Twitch services consume cookies from the jar; SAPISIDHASH generation; PO token dependency on cookies.
- **[operations.md](operations.md)** -- Config file search paths; database file location; log file paths; staging vs output directories.

### Source Files

- `internal/database/database.go` -- Database struct, Open(), batch coalescing, UpdateJobFields, pub/sub, CRUD operations
- `internal/database/types.go` -- Job, Gap, Segment, TrimRecord, ClientToken, JobStatus, JobStats type definitions
- `internal/database/migrations.go` -- Schema DDL, versioned migrations
- `internal/config/config.go` -- Load(), Save(), migrateOldFormat(), validate(), ResolveTemplate()
- `internal/config/types.go` -- MoomboxConfig and all section structs
- `internal/config/flex_duration.go` -- FlexDuration type with TOML/JSON marshaling
- `internal/config/channel_terms.go` -- ChannelTerms with dual string/map representation
- `internal/cookies/jar.go` -- CookieJar, Netscape parsing, auth cookie detection, SAPISIDHASH
- `internal/cookies/refresh.go` -- RefreshService, YouTube/Twitch auth validation, session cookie refresh
- `internal/cookies/autocookies.go` -- AutoCookieService, headless browser launch, CDP polling, cookie extraction
- `internal/logger/logger.go` -- Logger, file rotation, ring buffer, pub/sub, per-job buffers
- `internal/engine/downloader.go` -- ResumeState, resume file management
- `internal/chat/downloader.go` -- Chat file writing, incremental append, messageCount padding
- `internal/chat/types.go` -- ChatData, ChatMessage, ChatResumeState
