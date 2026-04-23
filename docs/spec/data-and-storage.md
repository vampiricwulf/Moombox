# Data and Storage

## Scope

This document specifies every data persistence layer in Moombox: the SQLite database (schema, connection tuning, batch coalescing, pub/sub), the TOML configuration system (sections, types, migrations, validation), the cookie management subsystem (jar, refresh, auto-cookie), the logger (file rotation, ring buffer, pub/sub), and the on-disk file output conventions (staging, output templates, resume state, chat files). It is the authoritative reference for how Moombox reads, writes, and organizes persistent and transient data.

## Rules and Constraints

These are hard rules. An AI assisting with Moombox development must follow them without exception:

- **SQLite with WAL mode, 1 connection, 5s busy timeout, foreign keys on.** The DSN is `file:<path>?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on`. Connection pool is `SetMaxOpenConns(1)` and `SetMaxIdleConns(1)`. SQLite is single-writer; do not change the pool size.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** The method accepts `map[string]any`, maps keys through `fieldToColumn` (40 entries), dynamically builds a `SET` clause, and auto-appends `updated_at` with the current UTC RFC3339 timestamp. After writing, it re-reads the full job row to notify subscribers with a complete `*Job` object. Returns the updated `*Job`.
- **`fieldToColumn` defines the allowed keys for `UpdateJobFields`.** Any key not present in this map is silently ignored. The map currently has 40 entries mapping Go field names to SQLite column names (identity mapping in all cases). Adding a new column to the jobs table requires adding a corresponding entry here.
- **`JobStatus` is `type JobStatus string`.** Status values are string constants, not integers or enums. Timestamps are ISO 8601 / RFC3339 strings. Optional numeric fields (sequence counters, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`).
- **Batch update coalescing: 100ms signal-driven window, zero IO when idle.** The `batchUpdateLoop` goroutine sleeps on a channel until the first update arrives, then waits 100ms to accumulate more updates, then flushes all pending updates in a single transaction. When no updates are pending, the goroutine consumes zero CPU and performs zero IO.
- **Config migrations are non-destructive.** `migrateOldFormat()` only applies a migration when the target section does not already exist in the TOML file. It never overwrites user-configured values in existing sections.
- **FlexDuration parses config values as minutes or days, context-dependent.** A bare integer in `feed_check_interval` means minutes; in `hide_finished_age_days` it means days. Duration strings like `"10m"`, `"7d"` are parsed via regex and converted to the context-appropriate unit.
- **Schema migrations are versioned, idempotent, and forward-only.** Currently at v9. Each migration checks the current version before applying. Migrations run at startup in `Database.Init()`. There is no rollback mechanism.
- **Cookie file format is Netscape.** The jar only loads cookies matching YouTube/Google domains or Twitch domains. Cookies are filtered to essential authentication cookies only.
- **Log file rotation uses numbered suffixes.** The current file is renamed to `.1`, existing `.N` files shift to `.N+1`, and excess files beyond `max_files` are deleted.
- **Resume state files are JSON sidecars.** Named `<output_file>.resume.json`, they store the last successful segment sequence number, bytes written, timestamp, and base URL. They are validated on load (URL match + file size check) and cleared only on clean stream completion.
- **Chat files use incremental append, not full rewrite.** After the first flush, new messages are appended by seeking to the closing `]` bracket, truncating there, and writing new messages plus the closing structure. The `messageCount` field in the JSON header is padded to 20 characters so it can be updated in-place without shifting the rest of the file.

---

## Database (internal/database/)

### Connection Configuration

The database is opened via `sql.Open("sqlite", dsn)` using the `modernc.org/sqlite` pure-Go driver (no CGo).

**DSN parameters:**

| Parameter | Value | Purpose |
|-----------|-------|---------|
| `_journal_mode` | `WAL` | Write-ahead logging for concurrent reads during writes |
| `_busy_timeout` | `5000` | Wait up to 5 seconds for a locked database before returning SQLITE_BUSY |
| `_foreign_keys` | `on` | Enforce foreign key constraints (gaps, trims, segments reference jobs) |

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

**Current version: 9**

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

**segments** (added v5):

| Column | Type | Notes |
|--------|------|-------|
| id | INTEGER | PRIMARY KEY AUTOINCREMENT |
| job_id | TEXT | NOT NULL, FK -> jobs(id) ON DELETE CASCADE |
| segment_index | INTEGER | 0-based ordering within a job |
| unix_start | INTEGER | Unix timestamp |
| unix_end | INTEGER | Unix timestamp |
| quality | TEXT | e.g. "1080p60" |
| filename | TEXT | Segment filename |
| file_path | TEXT | Absolute path, nullable |
| file_size | INTEGER | Bytes, nullable |
| video_width | INTEGER | Nullable |
| video_height | INTEGER | Nullable |
| video_fps | INTEGER | Nullable |
| duration_seconds | REAL | Nullable |

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

**last_videos:**

| Column | Type | Notes |
|--------|------|-------|
| channel_id | TEXT | PRIMARY KEY |
| video_id | TEXT | NOT NULL |

**Schema version:**

Tracked via SQLite's built-in `PRAGMA user_version` (since v11). Older databases created with the legacy `schema_version` table are auto-migrated on first open.

### Schema Migrations

Migrations are forward-only and run at startup in `Database.migrate()`. `PRAGMA user_version` is authoritative. On a fresh DB the PRAGMA reads 0 and `createSchema` is executed followed by a PRAGMA set to the current version. On a pre-v11 DB the PRAGMA still reads 0, so migrate() falls back to reading the legacy `schema_version` table, carries the value forward into PRAGMA, runs any pending migrations, and the v11 block drops the legacy table.

| Version | Changes |
|---------|---------|
| v1 | Initial schema (jobs, gaps, trims, history, last_videos) |
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

Each migration uses `ALTER TABLE ADD COLUMN` with duplicate-column error suppression (columns may already exist from partial migrations). Backfill queries run against existing data where applicable.

### Job Status Lifecycle

`JobStatus` is `type JobStatus string` with the following constants:

| Constant | Value | Meaning |
|----------|-------|---------|
| `StatusUpcoming` | `"Upcoming"` | Stream is scheduled but not yet live |
| `StatusLive` | `"Live"` | Stream detected as live, waiting to start download |
| `StatusDownloading` | `"Downloading"` | Actively downloading segments |
| `StatusMuxing` | `"Muxing"` | FFmpeg muxing video + audio + chat |
| `StatusFinished` | `"Finished"` | Download and mux completed successfully |
| `StatusError` | `"Error"` | Failed with error message in `error` field |
| `StatusCancelled` | `"Cancelled"` | User-cancelled |
| `StatusCookies` | `"COOKIES?"` | Needs cookie refresh to continue (special auth-failure state) |

**Normal flow:** `Upcoming` -> `Live` -> `Downloading` -> `Muxing` -> `Finished`

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
- **Last videos:** `GetLastVideo(channelID)` / `SetLastVideo(channelID, videoID)` tracks the most recent video per channel for deduplication.
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
| NetworkAccess | string | "localhost" | `network_access` | "localhost", "lan", or "external" |
| HTTPSEnabled | bool | false | `https_enabled` | |
| TLSCertPath | string | "" | `tls_cert_path` | |
| TLSKeyPath | string | "" | `tls_key_path` | |
| PasswordHash | string | "" | `password_hash` | scrypt hash, omitted from JSON |

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
| MaxFeedItems | int | 15 | `max_feed_items` | Min: 1 |
| FeedCheckInterval | FlexDuration | 10 (minutes) | `feed_check_interval` | |
| DecapiCheckInterval | *int | nil | `decapi_check_interval` | Seconds, valid: 15-3600 |
| TwitchCheckInterval | *int | nil | `twitch_check_interval` | Seconds, valid: 1-3600 |
| HideFinishedAgeDays | FlexDuration | 30 (days) | `hide_finished_age_days` | |

#### [downloader]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| OutputTemplate | string | `${channel}/${start_date} ${title} [${id}]` | `output_template` |
| MaxVideoResolution | int | 2160 | `max_video_resolution` | |
| NumParallelDownloads | int | 2 | `num_parallel_downloads` | Min: 1 |
| DownloadChat | bool | true | `download_chat` | |
| Prefer60fps | bool | true | `prefer_60fps` | |
| SegmentRetryDelayCap | int | 60 | `segment_retry_delay_cap` | Seconds |
| SegmentLiveCheckRetries | int | 16 | `segment_live_check_retries` | |
| PoToken | string | "" | `po_token` | Manual PO token override |
| VisitorData | string | "" | `visitor_data` | Manual visitor data override |
| PotProviderURL | string | "" | `pot_provider_url` | External PO token provider |

#### [cookies]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| CookieFile | string | "./cookies.txt" | `cookie_file` | |
| AutoEnabled | bool | false | `auto_enabled` | |
| BrowserProfileDir | string | "./browser-profile" | `browser_profile_dir` | |
| Platforms | []string | [] | `platforms` | Platforms with verified cookies |
| ActivePlatforms | []string | [] | `active_platforms` | Explicit override for UI display |
| RefreshInterval | FlexDuration | 360 (minutes = 6h) | `refresh_interval` | Min: 10 |

#### [disk]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| WarnPercent | int | 90 | `disk_warn_percent` | Valid: 1-99 |
| CriticalPercent | int | 95 | `disk_critical_percent` | Must be > WarnPercent |

#### [updates]

| Field | Type | Default | TOML Key |
|-------|------|---------|----------|
| AutoCheckUpdates | bool | true | `auto_check_updates` |

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
| MaxFeedItems | *int | nil | `max_feed_items` | Per-channel override |
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
| Top-level `max_feed_items`, `feed_check_interval`, `decapi_check_interval`, `twitch_check_interval`, `hide_finished_age_days` | `[monitors]` section | Always migrated |
| `[downloader].output_directory`, `staging_directory`, `ffmpeg_path` | `[paths]` section | Only if `[paths]` doesn't exist |
| `[downloader].cookie_file` | `[cookies]` section | Only if `[cookies]` doesn't exist |
| `[auto_cookies]` section | `[cookies]` section | Only if `[cookies]` doesn't exist |

### Validation

`validate(cfg)` enforces constraints after loading and migration:

- Port: 1-65535 (default: 774)
- NetworkAccess: must be "localhost", "lan", or "external"
- LogLevel: must be DEBUG/INFO/WARN/ERROR (default: INFO)
- MaxFeedItems: min 1
- FeedCheckInterval: min 1 minute
- HideFinishedAgeDays: min 0
- DecapiCheckInterval: 15-3600 seconds (or nil)
- TwitchCheckInterval: 1-3600 seconds (or nil)
- NumParallelDownloads: min 1
- MaxVideoResolution: min 1
- SegmentRetryDelayCap: min 1
- SegmentLiveCheckRetries: min 1
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

### Cookie Jar

`CookieJar` parses Netscape-format cookie files and provides typed access to authentication cookies for YouTube and Twitch.

**Loading behavior:**

1. Read the file line by line.
2. Skip comments (lines starting with `#`), except `#HttpOnly_` lines (those are data).
3. Parse tab-separated fields: domain, include_subdomains, path, secure, expiry, name, value.
4. Filter by domain: only YouTube (`youtube.com`), Google (`google.com`), and Twitch (`twitch.tv`) domains.
5. Filter by name: only essential authentication cookies (defined in `essentialYouTubeCookies` and `essentialTwitchCookies` maps).
6. Deduplication: prefer `youtube.com` cookies over `google.com` when both have the same cookie name.

**Essential YouTube cookies (20):** SAPISID, __Secure-1PAPISID, __Secure-3PAPISID, SID, HSID, SSID, APISID, __Secure-1PSID, __Secure-3PSID, __Secure-1PSIDTS, __Secure-3PSIDTS, __Secure-1PSIDCC, __Secure-3PSIDCC, LOGIN_INFO, VISITOR_INFO1_LIVE, VISITOR_PRIVACY_METADATA, YSC, __Secure-ROLLOUT_TOKEN, CONSENT, PREF.

**Essential Twitch cookies (4):** auth-token, twilight-user, login, name.

**Key methods:**

- `HasYouTubeAuthCookies()`: true if SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO are present.
- `HasTwitchAuthCookies()`: true if auth-token is present.
- `GetCookieHeader()`: builds a `Cookie:` header string from all loaded cookies.
- `GenerateAuthorizationHeader(origin)`: generates SAPISIDHASH + SAPISID1PHASH + SAPISID3PHASH header using SHA1(timestamp + SAPISID + origin).
- `Reload()`: re-reads from the same file path.

**Thread safety:** All methods are protected by `sync.RWMutex`.

### Refresh Service

`RefreshService` periodically validates cookies against the actual platform APIs and refreshes session cookies from YouTube Set-Cookie headers.

**Validation endpoints:**

| Platform | Method | Endpoint | Auth Check |
|----------|--------|----------|------------|
| YouTube | POST | `youtube.com/youtubei/v1/guide` | Parse response for `logged_in: "1"` in serviceTrackingParams or `loggedIn: true` in mainAppWebResponseContext |
| Twitch | GET | `id.twitch.tv/oauth2/validate` | HTTP 200 = valid |

**YouTube session refresh:**

During the YouTube auth check, Set-Cookie headers from the response are processed:

1. Parse each Set-Cookie header for YouTube/Google domains.
2. Extract name, value, and expiry (from `expires` or `max-age` directives).
3. Update the cookie file: replace existing entries, append new ones.
4. Reload the jar.

**Auth loss detection:**

The service tracks previous auth state per platform. When a platform transitions from authenticated to not-authenticated (and the check was conclusive -- no network error), it fires `OnRecoveryNeeded(platform)`. This triggers the auto-cookie service to attempt re-authentication.

`SetExpectedPlatforms(platforms)` seeds the previous auth state from persisted config platforms so auth loss is detectable even after an app restart.

**Timing:**

- Default refresh interval: 30 minutes (hardcoded; the config `cookies.refresh_interval` controls the auto-cookie periodic refresh, not the auth-check refresh service).
- Auth check timeout: 15 seconds per platform.
- Initial check runs immediately on `Start()`.

### Auto-Cookie Service

`AutoCookieService` launches a headless browser to capture authentication cookies via an interactive login flow.

**Supported browsers:**

| Browser | Engine | Detection |
|---------|--------|-----------|
| Firefox | Gecko | Windows registry + common install paths |
| Waterfox | Gecko | Windows registry + common install paths |
| Chrome | Chromium | Windows registry + common install paths |
| Brave | Chromium | Windows registry + common install paths |
| Edge | Chromium | Windows registry + common install paths |
| Opera | Chromium | Windows registry + common install paths |

Browser detection results are cached for 60 seconds.

**Login flow by engine:**

| Engine | Launch Args | Profile |
|--------|-------------|---------|
| Firefox/Gecko | `-profile <DIR> -new-instance <LOGIN_URL>` | User.js suppresses first-run dialogs |
| Chromium | `--user-data-dir=<DIR> <LOGIN_URL>` | Clean data dir |

**Cookie extraction:**

| Engine | Source | Method |
|--------|--------|--------|
| Firefox | `cookies.sqlite` in profile dir | SQL query against `moz_cookies` table |
| Chromium | `Default/Cookies` in user-data-dir | SQL query against `cookies` table; values decrypted via DPAPI on Windows |

**CDP (Chrome DevTools Protocol) polling:**

For Chromium browsers, the auto-cookie service connects via CDP WebSocket to poll for login completion:

- Poll interval: 500ms
- Poll timeout: 15 seconds
- Detects login by checking for authentication cookies in the browser's cookie store

**Lock file cleanup:**

Before launching, the service removes stale lock files that could prevent browser launch:

- Chromium: `lockfile`, `SingletonLock`, `SingletonSocket`, `SingletonCookie`
- Firefox: `parent.lock`, `.parentlock`

**Platform URLs:**

| Platform | Login URL | Refresh URL |
|----------|-----------|-------------|
| YouTube | `accounts.google.com/ServiceLogin?service=youtube` | `www.youtube.com` |
| Twitch | `www.twitch.tv/login` | `www.twitch.tv` |

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
}
```

**Save frequency:**

- Sequential downloads: every 50 segments (`ResumeSeqInterval`)
- Catch-up downloads: every 10 segments (`ResumeCatchupInterval`)
- Always saved on exit (clean or interrupted)

**Resume validation on load:**

1. BaseURL must match the current download's base URL (stream URL may have changed).
2. Output file must exist and be at least as large as `BytesWritten`.
3. If validation fails, the resume file is discarded and download starts fresh.
4. If the resume file is missing but `StartSeq > 0` in the database, the database state is used as a fallback (less precise but allows recovery).

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

Separate from the database's `jobLogs`, the Logger also maintains per-job buffers:

- `LogForJob(jobID, level, msg, args...)`: logs normally AND appends to the job-specific buffer.
- Per-job buffers are capped at 200 lines. When exceeded, trimmed to the last 100 entries.
- `PruneJobLogs(activeIDs)` removes buffers for jobs no longer active.

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
