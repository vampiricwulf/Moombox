# Twitch Flap Auto-Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the `"twitch channel is offline"` false-error caused by Twitch GQL StreamMetadata briefly returning `Stream=nil` immediately after the monitor confirms a stream is live, AND auto-recover errored jobs whose underlying stream is still live on the next monitor tick.

**Architecture:** Two layered fixes. **Layer 1 (Option A — pass-through):** `OnStreamFound` stashes its freshly-fetched `*twitch.TwitchStreamInfo` into a take-once hint cache on `StreamProcessor`, keyed by jobID. `processTwitchLive` consumes the hint instead of re-calling `GetStreamInfo` — eliminating the duplicate API call that caused the flap. **Layer 2 (auto-recovery):** monitor's `checkChannel` detects errored jobs whose recorded error matches a recoverable shape (offline-flap signature) and the same broadcast is still live (matching streamID); fires a new `OnStreamRecover` callback that re-stashes the hint, increments an `auto_retry_count` column, and re-enqueues via a new `AutoReinitializeJob`. Capped at 2 auto-retries; user-driven `ReinitializeJob` resets the counter; subsequent retry-failures suppress duplicate error notifications.

**Tech Stack:** Go 1.25, modernc/sqlite, existing internal packages — `internal/twitch`, `internal/monitor`, `internal/worker`, `internal/database`, `cmd/moombox`. No new external dependencies.

**Setup before starting:** Recommended to create a dedicated git worktree:
```bash
git worktree add ../moombox-twitch-recovery feat/twitch-flap-recovery
cd ../moombox-twitch-recovery
```

**Sequencing:** Phases run sequentially — each builds on the previous. Phase 1 (database) must land first because every later phase reads/writes the new column. Phases 2-4 ship Option A (the pass-through fix); the codebase is in a working, shippable state at the end of Phase 4. Phases 5-9 add auto-recovery on top. Phase 10 verifies end-to-end.

---

## Background

See conversation log: 2026-05-03 investigation into job `tw_316500481527` (channel `shachimu`). Monitor at 22:05:56 saw the stream live with the streamID; 0.x seconds later, `processTwitchLive` re-queried Twitch GQL and got `user.Stream == nil`, treated as offline, errored. Five minutes later the user reinit'd; same job ID, same streamID, immediately succeeded. Root cause: redundant `GetStreamInfo` call hit a transient sparse-then-nil Twitch GQL response. The monitor's call had succeeded; the second call landed in a different point of the API's flap. Re-using the monitor's already-good result eliminates the failure mode.

---

## Phase 1: Database column for auto-retry counter

Adds `auto_retry_count INTEGER NOT NULL DEFAULT 0` to the `jobs` table. Plumbs it through every read/write path so later phases can update and read it.

### Task 1.1: Bump schema version + add migration block

**Files:**
- Modify: `internal/database/migrations.go`

- [ ] **Step 1: Bump `schemaVersion` constant**

In `internal/database/migrations.go`, change line 26 from:
```go
const schemaVersion = 12
```
to:
```go
const schemaVersion = 13
```

- [ ] **Step 2: Add the new column to `createSchema`**

In `internal/database/migrations.go`, modify the `CREATE TABLE jobs` block. Find the line ending with `chat_offset REAL NOT NULL DEFAULT 0` (line ~90) and add `auto_retry_count INTEGER NOT NULL DEFAULT 0,` immediately above it. Final block tail looks like:
```sql
    chat_offset REAL NOT NULL DEFAULT 0,
    auto_retry_count INTEGER NOT NULL DEFAULT 0
);
```

(Note: trailing comma moved from `chat_offset` to the new line — `chat_offset` was the last column before, now `auto_retry_count` is.)

- [ ] **Step 3: Add the migration block for v13**

Append after the v12 block (just before `return nil`) in `migrate()`:
```go
	if version < 13 {
		// auto_retry_count tracks how many times the monitor's auto-recovery
		// has re-enqueued this job after a transient "twitch channel is offline"
		// flap. Capped at 2 (see worker.maxTwitchAutoRetries) so persistent
		// issues don't loop forever. Reset to 0 by user-driven ReinitializeJob.
		if _, err := db.db.ExecContext(db.getCtx(),
			`ALTER TABLE jobs ADD COLUMN auto_retry_count INTEGER NOT NULL DEFAULT 0`); err != nil {
			if !isDuplicateColumnErr(err) {
				return err
			}
		}

		if err := db.writeUserVersion(13); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Verify schema compiles + migration is idempotent**

```bash
go build ./internal/database/...
go test ./internal/database/... -run TestOpenAndMigrate -v
```
Expected: PASS, no errors.

- [ ] **Step 5: Commit**

```bash
git add internal/database/migrations.go
git commit -m "db: add auto_retry_count column (schema v13)"
```

### Task 1.2: Wire the column into Job struct + scan/update paths

**Files:**
- Modify: `internal/database/types.go`
- Modify: `internal/database/database.go`

- [ ] **Step 1: Add field to Job struct**

In `internal/database/types.go`, find the `Job` struct. Add `AutoRetryCount` near the bottom of the struct, after `ChatOffset` (around line 82). Insert:
```go
	// Auto-recovery
	AutoRetryCount    int        `json:"autoRetryCount,omitempty"`
```

- [ ] **Step 2: Add the field to `fieldToColumn` map**

In `internal/database/database.go`, find `fieldToColumn` (line ~20). Add a new entry after `"chat_offset": "chat_offset",`:
```go
	"chat_offset":         "chat_offset",
	"auto_retry_count":    "auto_retry_count",
}
```

- [ ] **Step 3: Add the column to `prepareStatements` SELECT**

In `internal/database/database.go`, find `prepareStatements()` (line ~194). The `SELECT ... FROM jobs WHERE id = ?` query lists every column in order. Append `, auto_retry_count` to the end of the column list, just before `FROM jobs WHERE id = ?`:
```go
	db.stmtGetJob, err = db.prepareStmt(`SELECT id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, video_width, video_height, video_fps, file_size,
		chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file,
		twitch_quality, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at, quality_preference, watched, resume_position, chat_offset,
		auto_retry_count
		FROM jobs WHERE id = ?`)
```

- [ ] **Step 4: Add the column to `scanJobRow` Scan call**

In `internal/database/database.go`, find `scanJobRow` (line ~442). Append `&j.AutoRetryCount,` to the `Scan(...)` arg list as the last entry (after `&j.ChatOffset`):
```go
	err := r.Scan(
		&j.ID, &j.VideoID, &j.URL, &j.Title, &j.ChannelName, &j.Platform,
		&j.Status, &j.Progress, &j.Percent, &j.ETA, &j.Speed, &j.Error,
		&j.CreatedAt, &j.UpdatedAt,
		&j.LastVideoSeq, &j.LastAudioSeq, &j.TotalVideoSeq, &j.TotalAudioSeq,
		&isVod, &manuallyAdded, &allowNonStream, &j.StreamStartTime, &j.StreamEndTime,
		&j.LengthSeconds, &j.DownloadStartedAt, &j.ThumbnailURL, &j.Description,
		&j.OutputFile, &j.Filename, &j.OutputDirectory,
		&j.VideoWidth, &j.VideoHeight, &j.VideoFps, &j.FileSize,
		&j.ChatStatus, &j.TotalChatMessages, &j.ChatFilename, &j.ChatFile,
		&j.ThumbnailFile, &j.DescriptionFile,
		&j.TwitchQuality, &j.TwitchCategory, &j.ChannelAvatarURL,
		&j.SelectedVideoItag, &j.SelectedAudioItag, &j.StartTime, &j.EndTime,
		&j.LastRecheckAt, &j.QualityPreference, &watched, &j.ResumePosition, &j.ChatOffset,
		&j.AutoRetryCount,
	)
```

- [ ] **Step 5: Add the column to `updateJobExec` SET clause**

In `internal/database/database.go`, find `updateJobExec` (line ~247). Add `auto_retry_count=?` to the SET clause (after `chat_offset=?`) and append `job.AutoRetryCount,` to the `ExecContext` arg list (between `job.ChatOffset` and `job.ID`):
```go
	_, err := exec.ExecContext(ctx, `UPDATE jobs SET
		video_id=?, url=?, title=?, channel_name=?, platform=?, status=?,
		progress=?, percent=?, eta=?, speed=?, error=?, updated_at=?,
		last_video_seq=?, last_audio_seq=?, total_video_seq=?, total_audio_seq=?,
		is_vod=?, manually_added=?, allow_non_stream=?, stream_start_time=?,
		stream_end_time=?, length_seconds=?, download_started_at=?,
		thumbnail_url=?, description=?, output_file=?, filename=?,
		output_directory=?, video_width=?, video_height=?, video_fps=?, file_size=?,
		chat_status=?, total_chat_messages=?, chat_filename=?, chat_file=?,
		thumbnail_file=?, description_file=?,
		twitch_quality=?, twitch_category=?, channel_avatar_url=?,
		selected_video_itag=?, selected_audio_itag=?, start_time=?, end_time=?,
		last_recheck_at=?, quality_preference=?, watched=?, resume_position=?, chat_offset=?,
		auto_retry_count=?
		WHERE id=?`,
		job.VideoID, job.URL, job.Title, job.ChannelName, job.Platform, job.Status,
		job.Progress, job.Percent, job.ETA, job.Speed, job.Error, job.UpdatedAt,
		job.LastVideoSeq, job.LastAudioSeq, job.TotalVideoSeq, job.TotalAudioSeq,
		boolToInt(job.IsVod), boolToInt(job.ManuallyAdded), boolToInt(job.AllowNonStream),
		job.StreamStartTime, job.StreamEndTime, job.LengthSeconds, job.DownloadStartedAt,
		job.ThumbnailURL, job.Description, job.OutputFile, job.Filename,
		job.OutputDirectory, job.VideoWidth, job.VideoHeight, job.VideoFps, job.FileSize,
		job.ChatStatus, job.TotalChatMessages, job.ChatFilename, job.ChatFile,
		job.ThumbnailFile, job.DescriptionFile,
		job.TwitchQuality, job.TwitchCategory, job.ChannelAvatarURL,
		job.SelectedVideoItag, job.SelectedAudioItag, job.StartTime, job.EndTime,
		job.LastRecheckAt, job.QualityPreference, boolToInt(job.Watched), job.ResumePosition,
		job.ChatOffset, job.AutoRetryCount, job.ID)
	return err
```

- [ ] **Step 6: Run TestFieldToColumnCoverage to confirm coverage**

```bash
go test ./internal/database/ -run TestFieldToColumnCoverage -v
```
Expected: PASS. If it fails with `Job field "autoRetryCount" (column "auto_retry_count") has no fieldToColumn entry`, re-check Step 2.

- [ ] **Step 7: Run full database test suite**

```bash
go test ./internal/database/... -v
```
Expected: PASS, no errors.

- [ ] **Step 8: Commit**

```bash
git add internal/database/types.go internal/database/database.go
git commit -m "db: plumb auto_retry_count through Job struct, scan, and update paths"
```

### Task 1.3: Test that UpdateJobFields can read/write the counter

**Files:**
- Modify: `internal/database/database_test.go`

- [ ] **Step 1: Write a failing test**

Append to `internal/database/database_test.go` (after `TestUpdateJobFieldsEmpty` at line 341):
```go
// TestUpdateJobFieldsAutoRetryCount verifies the new auto_retry_count column
// can be read and written via UpdateJobFields. Locks down the column-plumbing
// done in Phase 1 of the Twitch flap auto-recovery feature.
func TestUpdateJobFieldsAutoRetryCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "tw_retry1",
		VideoID: "retry1",
		URL:     "https://twitch.tv/somestreamer",
		Status:  StatusError,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	// Default value on insert
	got, err := db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 0 {
		t.Errorf("default AutoRetryCount: want 0, got %d", got.AutoRetryCount)
	}

	// Update via UpdateJobFields
	db.UpdateJobFields("tw_retry1", map[string]any{
		"auto_retry_count": 1,
	})
	got, err = db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 1 {
		t.Errorf("after update: want 1, got %d", got.AutoRetryCount)
	}

	// Update again
	db.UpdateJobFields("tw_retry1", map[string]any{
		"auto_retry_count": 2,
	})
	got, err = db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 2 {
		t.Errorf("after second update: want 2, got %d", got.AutoRetryCount)
	}
}
```

- [ ] **Step 2: Run the test**

```bash
go test ./internal/database/ -run TestUpdateJobFieldsAutoRetryCount -v
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/database/database_test.go
git commit -m "db: test auto_retry_count read/write via UpdateJobFields"
```

---

## Phase 2: Hint cache on StreamProcessor

Adds the take-once hint store. Independent of Phase 1; could be done in parallel, but kept sequential here.

### Task 2.1: Hint type, stash, take, TTL

**Files:**
- Create: `internal/worker/twitch_hint.go`
- Test: `internal/worker/twitch_hint_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/worker/twitch_hint_test.go`:
```go
package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

func TestTwitchHintCacheStashAndTake(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "111", IsLive: true}

	c.stash("tw_111", info)

	got := c.take("tw_111")
	if got == nil {
		t.Fatal("take after stash: want non-nil")
	}
	if got.StreamID != "111" {
		t.Errorf("StreamID: want 111, got %q", got.StreamID)
	}

	// take again — should be empty (take-once)
	if got := c.take("tw_111"); got != nil {
		t.Errorf("second take: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTakeMissing(t *testing.T) {
	c := newTwitchHintCache()
	if got := c.take("tw_does_not_exist"); got != nil {
		t.Errorf("take of unknown jobID: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTTLEviction(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "222", IsLive: true}

	// Stash with a manual past timestamp older than the TTL
	c.mu.Lock()
	c.entries["tw_222"] = twitchHintEntry{
		info:     info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()

	if got := c.take("tw_222"); got != nil {
		t.Errorf("take of expired entry: want nil, got %+v", got)
	}

	// Verify the expired entry was removed (no zombie)
	c.mu.Lock()
	_, exists := c.entries["tw_222"]
	c.mu.Unlock()
	if exists {
		t.Error("expired entry should be removed from cache after take")
	}
}

func TestTwitchHintCacheNilSafe(t *testing.T) {
	var c *twitchHintCache
	// nil receiver should be safe (no-op)
	c.stash("tw_nil", &twitch.TwitchStreamInfo{StreamID: "nil"})
	if got := c.take("tw_nil"); got != nil {
		t.Errorf("take on nil cache: want nil, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/worker/ -run TestTwitchHintCache -v
```
Expected: FAIL — `undefined: newTwitchHintCache`, `undefined: twitchHintEntry`, etc.

- [ ] **Step 3: Implement the cache**

Create `internal/worker/twitch_hint.go`:
```go
package worker

import (
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// twitchHintTTL bounds how long a fresh-from-monitor TwitchStreamInfo stays
// usable. EnqueueJob fires the worker within milliseconds, so 60s is
// generous; the upper bound only matters as a leak guard if AddJob succeeded
// but EnqueueJob's signal got lost (which would be a separate bug anyway).
const twitchHintTTL = 60 * time.Second

type twitchHintEntry struct {
	info      *twitch.TwitchStreamInfo
	stashedAt time.Time
}

// twitchHintCache is a take-once map keyed by jobID, used by the monitor's
// OnStreamFound (and OnStreamRecover) callback to forward its already-fetched
// stream info to the worker's processTwitchLive. Eliminates a redundant
// GetStreamInfo call that exposed the worker to transient Twitch GQL flaps
// where StreamMetadata briefly returned Stream=nil between two consecutive
// requests for the same channel.
//
// Take-once semantics ensure the same hint can't accidentally be consumed by
// multiple processing attempts; user-driven Reinit always falls back to a
// fresh GetStreamInfo, which is the right behaviour at reinit time.
type twitchHintCache struct {
	mu      sync.Mutex
	entries map[string]twitchHintEntry
}

func newTwitchHintCache() *twitchHintCache {
	return &twitchHintCache{entries: map[string]twitchHintEntry{}}
}

// stash records a fresh TwitchStreamInfo for the given jobID. Safe to call
// on a nil receiver (test harnesses may not wire one up).
func (c *twitchHintCache) stash(jobID string, info *twitch.TwitchStreamInfo) {
	if c == nil || info == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]twitchHintEntry{}
	}
	c.entries[jobID] = twitchHintEntry{info: info, stashedAt: time.Now()}
}

// take consumes and returns the hint for jobID, or nil if absent or expired.
// Always removes the entry whether expired or fresh — take-once.
func (c *twitchHintCache) take(jobID string) *twitch.TwitchStreamInfo {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[jobID]
	if !ok {
		return nil
	}
	delete(c.entries, jobID)
	if time.Since(entry.stashedAt) > twitchHintTTL {
		return nil
	}
	return entry.info
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/worker/ -run TestTwitchHintCache -v
```
Expected: PASS, all four sub-tests green.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/twitch_hint.go internal/worker/twitch_hint_test.go
git commit -m "worker: add take-once Twitch stream-info hint cache"
```

### Task 2.2: Wire the cache onto StreamProcessor

**Files:**
- Modify: `internal/worker/stream_processor.go`
- Modify: `internal/worker/worker.go`

- [ ] **Step 1: Add `twitchHints` field to StreamProcessor and accessor methods**

In `internal/worker/stream_processor.go`, modify the `StreamProcessor` struct (line ~68) to add a hint cache. Also add a method to stash hints from outside the package. Insert after the existing fields:

Replace the struct + constructor:
```go
// StreamProcessor handles stream status probing and waiting.
type StreamProcessor struct {
	yt          *youtube.Service
	tw          *twitch.Service
	cfg         *config.MoomboxConfig // captured for early-init reads before SetConfigStore
	configStore *config.Store         // shared config store (set via SetConfigStore)
	db          *database.Database
	notifier    *notifications.Manager
	logger      logger
	isOnline    func() bool

	twitchHints *twitchHintCache // populated by OnStreamFound; consumed by processTwitchLive

	mu          sync.Mutex
	activeChats []*chat.ChatDownloader // Track active chat downloaders for cleanup
}

// NewStreamProcessor creates a new stream processor.
func NewStreamProcessor(yt *youtube.Service, tw *twitch.Service, cfg *config.MoomboxConfig, db *database.Database, logger logger) *StreamProcessor {
	return &StreamProcessor{
		yt:          yt,
		tw:          tw,
		cfg:         cfg,
		db:          db,
		logger:      logger,
		twitchHints: newTwitchHintCache(),
	}
}
```

Then add a public stash method at the bottom of the file:
```go
// StashTwitchStreamInfo records a freshly-fetched Twitch stream info under
// the given jobID. The next processTwitchLive call for that jobID will use
// this info instead of re-querying Twitch — eliminating the duplicate GQL
// call that exposed the worker to transient StreamMetadata flaps.
//
// Take-once: the hint is removed on consumption. If never consumed, it
// expires after twitchHintTTL.
func (sp *StreamProcessor) StashTwitchStreamInfo(jobID string, info *twitch.TwitchStreamInfo) {
	sp.twitchHints.stash(jobID, info)
}
```

- [ ] **Step 2: Add a delegation method on DownloadWorker**

In `internal/worker/worker.go`, add after the `EnqueueJob` method (line ~226):
```go
// StashTwitchStreamInfo forwards a fresh Twitch stream info hint to the
// underlying StreamProcessor. Called by cmd/moombox's OnStreamFound /
// OnStreamRecover monitor callbacks so the processor doesn't re-fetch what
// the monitor just successfully fetched.
func (w *DownloadWorker) StashTwitchStreamInfo(jobID string, info *twitch.TwitchStreamInfo) {
	if w.streamProc != nil {
		w.streamProc.StashTwitchStreamInfo(jobID, info)
	}
}
```

- [ ] **Step 3: Verify it compiles + tests still pass**

```bash
go build ./internal/worker/...
go test ./internal/worker/ -v -run TestTwitchHintCache
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/worker/stream_processor.go internal/worker/worker.go
git commit -m "worker: expose StashTwitchStreamInfo via StreamProcessor + DownloadWorker"
```

---

## Phase 3: Consume the hint in processTwitchLive

### Task 3.1: Use stashed info; fall back to GetStreamInfo

**Files:**
- Modify: `internal/worker/stream_processor_twitch.go`

- [ ] **Step 1: Extract the offline error message to an exported worker constant**

In `internal/worker/stream_processor_twitch.go`, near the top of the file (after the imports block at line 16, before the `twitchAuthSentinel` function), add:
```go
// TwitchOfflineErrMsg is the literal error string emitted when a non-manual
// Twitch job's GetStreamInfo returns nil/!IsLive. Exported so the monitor's
// auto-recovery predicate can match on it without drifting from the producer.
// Keep these two sites aligned — a regression test in internal/monitor checks
// the predicate accepts exactly this string.
const TwitchOfflineErrMsg = "twitch channel is offline"
```

- [ ] **Step 2: Modify processTwitchLive to consume the hint and use the constant**

In `internal/worker/stream_processor_twitch.go`, replace the body of `processTwitchLive` (line 155-176) up through the offline-check block. Before the change:
```go
func (sp *StreamProcessor) processTwitchLive(ctx context.Context, job *database.Job, login string) (*StreamProcessResult, error) {
	streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("twitch stream info: %w", err)
	}

	if streamInfo == nil || !streamInfo.IsLive {
		if job.ManuallyAdded {
			sp.logger.Info("twitch channel is offline, waiting for stream", "channel", login)
			streamInfo, err = sp.waitForTwitchLive(ctx, job, login)
			if err != nil {
				return nil, err
			}
			if streamInfo == nil {
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			// Fall through to existing live handling below
		} else {
			sp.logger.Info("twitch channel is offline", "channel", login)
			return &StreamProcessResult{ShouldDownload: false, Error: "twitch channel is offline"}, nil
		}
	}
```
After the change:
```go
func (sp *StreamProcessor) processTwitchLive(ctx context.Context, job *database.Job, login string) (*StreamProcessResult, error) {
	// Consume any monitor-stashed hint first — avoids a redundant GetStreamInfo
	// call that exposed us to transient Twitch StreamMetadata flaps where the
	// API briefly returned Stream=nil between two consecutive requests for the
	// same channel. take() is a no-op if no hint exists (manual add, user
	// reinit, app restart, etc.) and we fall back to a fresh fetch.
	streamInfo := sp.twitchHints.take(job.ID)
	if streamInfo != nil {
		sp.logger.Debug("twitch using stashed monitor hint",
			"jobID", job.ID, "streamID", streamInfo.StreamID, "channel", login)
	}

	if streamInfo == nil {
		fetched, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			return nil, fmt.Errorf("twitch stream info: %w", err)
		}
		streamInfo = fetched
	}

	if streamInfo == nil || !streamInfo.IsLive {
		if job.ManuallyAdded {
			sp.logger.Info("twitch channel is offline, waiting for stream", "channel", login)
			waitInfo, err := sp.waitForTwitchLive(ctx, job, login)
			if err != nil {
				return nil, err
			}
			if waitInfo == nil {
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			streamInfo = waitInfo
			// Fall through to existing live handling below
		} else {
			sp.logger.Info(TwitchOfflineErrMsg, "channel", login)
			return &StreamProcessResult{ShouldDownload: false, Error: TwitchOfflineErrMsg}, nil
		}
	}
```

Note the `streamInfo, err = ...` becoming `waitInfo, err := ...` (and the assignment after) — required because `streamInfo` and `err` were originally declared together in the outer `:=`; with the hint-consumption refactor, `streamInfo` is now declared earlier and `err` was never declared in this scope. The intermediate `waitInfo` keeps the change diff-minimal and correct.

(The rest of `processTwitchLive` from the metadata-update block onward stays unchanged.)

- [ ] **Step 3: Verify it compiles**

```bash
go build ./internal/worker/...
go vet ./internal/worker/...
```
Expected: no errors.

- [ ] **Step 4: Run worker tests**

```bash
go test ./internal/worker/... -v
```
Expected: PASS. Existing tests don't exercise this path directly, but they confirm we haven't broken sibling code.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/stream_processor_twitch.go
git commit -m "worker: consume monitor-stashed Twitch hint + extract TwitchOfflineErrMsg"
```

### Task 3.2: Test that processTwitchLive uses a stashed hint

**Files:**
- Modify: `internal/worker/twitch_hint_test.go`

- [ ] **Step 1: Add an integration-style test**

Append to `internal/worker/twitch_hint_test.go`:
```go
// TestProcessorConsumesHint verifies a stashed hint short-circuits the
// GetStreamInfo call. We can't easily inject a fake Twitch service without
// substantially refactoring StreamProcessor, so this test focuses on the
// observable side-effect: after stashing and a single take, the cache is
// empty (take-once). The end-to-end path is covered by the integration
// scenario in Phase 10.
func TestProcessorStashAndTake(t *testing.T) {
	sp := &StreamProcessor{twitchHints: newTwitchHintCache()}
	info := &twitch.TwitchStreamInfo{StreamID: "abc", IsLive: true}

	sp.StashTwitchStreamInfo("tw_abc", info)

	got := sp.twitchHints.take("tw_abc")
	if got == nil || got.StreamID != "abc" {
		t.Fatalf("expected stashed info to be retrievable, got %+v", got)
	}

	// take-once: second take is empty
	if got := sp.twitchHints.take("tw_abc"); got != nil {
		t.Errorf("second take after stash: want nil, got %+v", got)
	}
}
```

- [ ] **Step 2: Run the test**

```bash
go test ./internal/worker/ -run TestProcessorStashAndTake -v
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/worker/twitch_hint_test.go
git commit -m "worker: test StreamProcessor stash/take API"
```

---

## Phase 4: Wire OnStreamFound to stash the hint

This completes Option A. After Phase 4, the original flap is fixed; auto-recovery (Phases 5-9) is independent additional value.

### Task 4.1: Stash hint in monitor_callbacks.go OnStreamFound

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go`

- [ ] **Step 1: Stash the hint after AddJob succeeds**

In `cmd/moombox/monitor_callbacks.go`, find the OnStreamFound callback for Twitch (line ~185). Modify the section right after `AddJob` succeeds. Before:
```go
		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
```
After:
```go
		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		// Stash the monitor's fresh streamInfo for processTwitchLive to consume,
		// so it doesn't immediately re-query Twitch GQL (which has been observed
		// to return Stream=nil for ~1s after StreamMetadata reports a stream as
		// live, manifesting as a false "twitch channel is offline" error).
		s.dlWorker.StashTwitchStreamInfo(jobID, info)
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./cmd/moombox/...
go vet ./cmd/moombox/...
```
Expected: no errors.

- [ ] **Step 3: Run all tests**

```bash
go test ./...
```
Expected: PASS. Nothing should regress.

- [ ] **Step 4: Commit**

```bash
git add cmd/moombox/monitor_callbacks.go
git commit -m "main: stash Twitch monitor streamInfo for processor consumption"
```

**Checkpoint: Option A complete.** The codebase is in a working, shippable state. The `tw_316500481527` flap from 2026-05-03 22:05:56 cannot reoccur — the processor uses the monitor's fresh `info` instead of re-querying. Phases 5-9 add auto-recovery for any remaining error paths (e.g. monitor's first call fails, then later succeeds).

---

## Phase 5: AutoReinitializeJob + counter reset on user reinit

Adds a sibling to `ReinitializeJob` that increments the counter instead of resetting it; user-driven `ReinitializeJob` is updated to reset the counter to 0.

### Task 5.1: Counter reset behaviour for ReinitializeJob

**Files:**
- Modify: `internal/worker/worker.go`

- [ ] **Step 1: Add `auto_retry_count: 0` to ReinitializeJob's update map**

In `internal/worker/worker.go`, find `ReinitializeJob` (line 710). Modify the `UpdateJobFields` map to also reset the counter. Before (showing only the relevant block at line 723-752):
```go
	// Clear all non-input fields
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
	})
	w.EnqueueJob(jobID)
}
```
After (add `"auto_retry_count": 0,` to the map):
```go
	// Clear all non-input fields. auto_retry_count resets here because
	// user-driven reinit grants the job a fresh budget; auto-recovery
	// uses AutoReinitializeJob (sibling method) which increments instead.
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
		"auto_retry_count":    0,
	})
	w.EnqueueJob(jobID)
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./internal/worker/...
```
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add internal/worker/worker.go
git commit -m "worker: ReinitializeJob resets auto_retry_count to 0"
```

### Task 5.2: AutoReinitializeJob sibling method

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/worker/worker_test.go` (or create the file if it doesn't include a test for ReinitializeJob; check first with `grep -n "TestReinitialize" internal/worker/worker_test.go`):
```go
func TestAutoReinitializeJobIncrementsCounter(t *testing.T) {
	// Reuses the standard newTestWorker pattern in worker_test.go; if the
	// helper has a different name in this codebase, adapt accordingly.
	w, db := newTestWorker(t)

	job := &database.Job{
		ID:             "tw_autoreinit",
		VideoID:        "autoreinit",
		URL:            "https://twitch.tv/x",
		Platform:       "twitch",
		Status:         database.StatusError,
		Error:          "twitch channel is offline",
		AutoRetryCount: 0,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	w.AutoReinitializeJob("tw_autoreinit")

	got, err := db.GetJob("tw_autoreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 1 {
		t.Errorf("AutoRetryCount: want 1, got %d", got.AutoRetryCount)
	}
	if got.Status != database.StatusUpcoming {
		t.Errorf("Status: want Upcoming, got %s", got.Status)
	}
	if got.Error != "" {
		t.Errorf("Error: want cleared, got %q", got.Error)
	}

	// Second call: counter goes to 2
	w.AutoReinitializeJob("tw_autoreinit")
	got, err = db.GetJob("tw_autoreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 2 {
		t.Errorf("after second call, AutoRetryCount: want 2, got %d", got.AutoRetryCount)
	}
}

func TestReinitializeJobResetsCounter(t *testing.T) {
	w, db := newTestWorker(t)

	job := &database.Job{
		ID:             "tw_userreinit",
		VideoID:        "userreinit",
		URL:            "https://twitch.tv/x",
		Platform:       "twitch",
		Status:         database.StatusError,
		Error:          "twitch channel is offline",
		AutoRetryCount: 2,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	w.ReinitializeJob("tw_userreinit")

	got, err := db.GetJob("tw_userreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 0 {
		t.Errorf("after user reinit, AutoRetryCount: want 0, got %d", got.AutoRetryCount)
	}
}
```

If `newTestWorker` doesn't exist in `worker_test.go`, look at the existing test patterns there and adapt — most worker tests construct via `NewDownloadWorker` directly. Check with:
```bash
grep -n "NewDownloadWorker\|newTestWorker" internal/worker/worker_test.go | head -5
```

If neither helper exists, replace the `newTestWorker(t)` lines with the direct construction pattern used by the existing tests (typically `db := openTestDB(t); w := NewDownloadWorker(db, nil, &config.MoomboxConfig{}, log, nil)` or similar — match what the file already does).

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/worker/ -run "TestAutoReinitializeJob|TestReinitializeJob" -v
```
Expected: FAIL — `undefined: AutoReinitializeJob` (and possibly the reset test fails if counter writes don't go through).

- [ ] **Step 3: Implement AutoReinitializeJob**

In `internal/worker/worker.go`, append after `ReinitializeJob` (around line 754):
```go
// AutoReinitializeJob is the auto-recovery sibling of ReinitializeJob: same
// state reset (clears progress fields, deletes staging dir, sets status to
// Upcoming, re-enqueues), but increments auto_retry_count instead of
// resetting it. Called by the Twitch monitor's OnStreamRecover callback
// when an errored job's underlying broadcast is still live and the error
// matches a recoverable shape.
//
// Capped at maxTwitchAutoRetries (the caller is expected to pre-check the
// budget; this method blindly increments).
func (w *DownloadWorker) AutoReinitializeJob(jobID string) {
	prev, err := w.db.GetJob(jobID)
	if err != nil || prev == nil {
		w.logger.Warn("AutoReinitializeJob: job not found", "jobID", jobID, "err", err)
		return
	}
	newCount := prev.AutoRetryCount + 1

	// Read config for staging path
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})

	// Delete staging directory
	stagingDir := filepath.Join(stagingBase, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("AutoReinitializeJob: staging cleanup failed", "path", stagingDir, "err", err)
	}

	// Same field reset as ReinitializeJob, but auto_retry_count INCREMENTS.
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
		"auto_retry_count":    newCount,
	})
	w.EnqueueJob(jobID)
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/worker/ -run "TestAutoReinitializeJob|TestReinitializeJob" -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/worker.go internal/worker/worker_test.go
git commit -m "worker: AutoReinitializeJob increments auto_retry_count"
```

### Task 5.3: Define maxTwitchAutoRetries constant

**Files:**
- Modify: `internal/worker/worker.go`

- [ ] **Step 1: Add the constant near other worker package constants**

In `internal/worker/worker.go`, find the `const` block near line 47-49 (`heartbeatInterval`). Add:
```go
// heartbeatInterval is the safety-net poll interval for catching missed jobs.
// Normal job discovery is signal-driven via NotifyNewJob.
const heartbeatInterval = 60 * time.Second

// maxTwitchAutoRetries caps how many times the Twitch monitor's auto-recovery
// will re-enqueue an errored "twitch channel is offline" job before giving up.
// 2 retries (3 total attempts including the original) handles transient GQL
// flaps measured in seconds without looping indefinitely on a persistent
// issue. User-driven Reinit always resets this counter.
const maxTwitchAutoRetries = 2
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./internal/worker/...
```
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add internal/worker/worker.go
git commit -m "worker: define maxTwitchAutoRetries cap for monitor-driven recovery"
```

---

## Phase 6: Recoverable-error predicate

### Task 6.1: isRecoverableTwitchError helper

**Files:**
- Create: `internal/monitor/twitch_recover.go`
- Test: `internal/monitor/twitch_recover_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/monitor/twitch_recover_test.go`:
```go
package monitor

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestIsRecoverableTwitchError(t *testing.T) {
	intPtr := func(n int) *int { i := n; return &i }

	tests := []struct {
		name string
		job  *database.Job
		want bool
	}{
		{
			name: "exact flap signature is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch channel is offline",
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: true,
		},
		{
			name: "different error string is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch HLS error: bad request",
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "non-error status is not recoverable",
			job: &database.Job{
				Status:         database.StatusFinished,
				Error:          "twitch channel is offline",
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "started downloading is not recoverable (download-time failure)",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch channel is offline",
				LastVideoSeq:   intPtr(42),
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "retry budget exhausted is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch channel is offline",
				LastVideoSeq:   nil,
				AutoRetryCount: 2,
			},
			want: false,
		},
		{
			name: "retry budget at 1 of 2 is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch channel is offline",
				LastVideoSeq:   nil,
				AutoRetryCount: 1,
			},
			want: true,
		},
		{
			name: "nil job is not recoverable",
			job:  nil,
			want: false,
		},
	}

	const maxRetries = 2
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRecoverableTwitchError(tt.job, maxRetries)
			if got != tt.want {
				t.Errorf("isRecoverableTwitchError(%+v, %d) = %v, want %v", tt.job, maxRetries, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/monitor/ -run TestIsRecoverableTwitchError -v
```
Expected: FAIL — `undefined: isRecoverableTwitchError`.

- [ ] **Step 3: Verify no monitor → worker import cycle**

```bash
grep -rn "internal/monitor" internal/worker/*.go || echo "no cycle"
```
Expected: `no cycle`. If the grep returns matches, stop and use the local-constant fallback (see "Phase 7 cycle risk" in the self-review section). On a clean codebase the import is safe.

- [ ] **Step 4: Implement the predicate**

Create `internal/monitor/twitch_recover.go`:
```go
package monitor

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// isRecoverableTwitchError reports whether an errored Twitch job is in the
// narrow shape that auto-recovery is designed for: a transient flap during
// the initial probe (no segments downloaded yet), captured in the database
// with the exact worker.TwitchOfflineErrMsg message, and still within the
// retry budget. Any deviation — different error string, partial download
// already on disk, exhausted budget — defers to user action.
//
// Imports worker.TwitchOfflineErrMsg so producer (stream_processor_twitch.go)
// and consumer (this predicate) can never drift on the literal.
func isRecoverableTwitchError(job *database.Job, maxRetries int) bool {
	if job == nil {
		return false
	}
	if job.Status != database.StatusError {
		return false
	}
	if job.Error != worker.TwitchOfflineErrMsg {
		return false
	}
	if job.LastVideoSeq != nil {
		return false
	}
	if job.AutoRetryCount >= maxRetries {
		return false
	}
	return true
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/monitor/ -run TestIsRecoverableTwitchError -v
```
Expected: PASS, all seven sub-tests green.

- [ ] **Step 6: Update the predicate test to use the worker constant**

In `internal/monitor/twitch_recover_test.go`, replace the literal `"twitch channel is offline"` in the test cases with `worker.TwitchOfflineErrMsg`. Add the import:
```go
import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)
```
And in each test case `Error:` field, replace the literal with `worker.TwitchOfflineErrMsg`. This locks producer/consumer alignment at the test level too. Re-run:
```bash
go test ./internal/monitor/ -run TestIsRecoverableTwitchError -v
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/twitch_recover.go internal/monitor/twitch_recover_test.go
git commit -m "monitor: predicate for recoverable Twitch flap errors"
```

---

## Phase 7: Hook OnStreamRecover into the monitor's check loop

### Task 7.1: Add OnStreamRecover callback to TwitchMonitor

**Files:**
- Modify: `internal/monitor/twitch.go`

- [ ] **Step 1: Add the callback field**

In `internal/monitor/twitch.go`, modify the `TwitchMonitor` struct (line 21-42). Find the existing callback fields:
```go
	OnSchedule    func(nextCheckAt int64)
	OnStreamFound func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig)
	IsOnline      func() bool // nil = always online
}
```
Replace with:
```go
	OnSchedule      func(nextCheckAt int64)
	OnStreamFound   func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig)
	OnStreamRecover func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig, jobID string)
	IsOnline        func() bool // nil = always online
}
```

- [ ] **Step 2: Modify checkChannel to detect recoverable errors**

In `internal/monitor/twitch.go`, modify `checkChannel` (line 237). The existing `HasProcessed` short-circuit becomes a branch that may dispatch to recovery. Before (line 256-262):
```go
	jobID := twitch.BuildJobID(info.StreamID, false)
	processed, hpErr := tm.db.HasProcessed(jobID)
	if hpErr != nil {
		tm.logger.Debug("HasProcessed query failed", "jobID", jobID, "err", hpErr)
	}
	if processed {
		return nil
	}
```
After:
```go
	jobID := twitch.BuildJobID(info.StreamID, false)
	processed, hpErr := tm.db.HasProcessed(jobID)
	if hpErr != nil {
		tm.logger.Debug("HasProcessed query failed", "jobID", jobID, "err", hpErr)
	}
	if processed {
		// Check whether the existing job is in a recoverable error state —
		// i.e. the SAME broadcast is still live (we got a hit on the same
		// streamID that produced this jobID) and the prior error matches
		// a transient Twitch GQL flap. If so, dispatch to OnStreamRecover.
		if tm.OnStreamRecover != nil {
			existing, getErr := tm.db.GetJob(jobID)
			if getErr != nil {
				tm.logger.Debug("recover check: GetJob failed", "jobID", jobID, "err", getErr)
			} else if isRecoverableTwitchError(existing, maxTwitchAutoRetries) {
				tm.logger.Info("twitch recoverable error — re-enqueueing job",
					"jobID", jobID,
					"channel", info.ChannelDisplayName,
					"streamID", info.StreamID,
					"prevRetries", existing.AutoRetryCount)
				tm.OnStreamRecover(info, ch, jobID)
				return nil
			}
		}
		return nil
	}
```

- [ ] **Step 3: Reference the cap constant from the worker package**

The `worker` import is already in `internal/monitor/twitch_recover.go` from Phase 6. We need the same import on `internal/monitor/twitch.go` for the dispatch site.

First, the constant must be exported. In `internal/worker/worker.go`, rename `maxTwitchAutoRetries` (added in Phase 5.3) to `MaxTwitchAutoRetries`. Update its single use in `internal/worker/worker.go` if any (Phase 5.3 only defined it, didn't reference it — references appear from Phase 6 onward via the import). Also update the comment if it references the old name.

Then, in `internal/monitor/twitch.go`, ensure the import block includes `worker`:
```go
import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/worker"
)
```
And in the Step 2 `checkChannel` change above, replace `maxTwitchAutoRetries` with `worker.MaxTwitchAutoRetries`.

⚠️ If the `grep` from Phase 6.1 step 3 had returned a cycle, fall back: keep the constant unexported, define a duplicate `monitorMaxTwitchAutoRetries` in `internal/monitor/twitch_recover.go`, and add a regression test in `internal/worker/worker_test.go` that imports `monitor` and asserts the two values match. This keeps the dependency direction monitor → worker only.

- [ ] **Step 4: Verify everything compiles**

```bash
go build ./...
go vet ./...
```
Expected: no errors. If a cycle is detected (`worker` already imports `monitor`), revert to the local-copy pattern.

- [ ] **Step 5: Run all tests**

```bash
go test ./internal/monitor/... ./internal/worker/... -v
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/monitor/twitch.go internal/worker/worker.go
git commit -m "monitor: dispatch OnStreamRecover for recoverable Twitch flap errors"
```

### Task 7.2: Test the monitor's recovery dispatch

**Files:**
- Modify: `internal/monitor/twitch_recover_test.go`

The predicate is already covered by Phase 6.1's seven-case `TestIsRecoverableTwitchError`. The end-to-end dispatch path through `checkChannel` requires a Twitch GQL fake (heavy refactor) and is covered by Phase 10's manual smoke test. No additional test is added in this task.

- [ ] **Step 1: Confirm Phase 6.1 test still covers the predicate fully**

```bash
go test ./internal/monitor/ -run TestIsRecoverableTwitchError -v
```
Expected: PASS, all cases green. If any new edge case has been discovered during Phases 7.1, add a sub-test here before moving on.

---

## Phase 8: Wire OnStreamRecover in cmd/moombox

### Task 8.1: Implement OnStreamRecover callback

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go`

- [ ] **Step 1: Add the callback after OnStreamFound**

In `cmd/moombox/monitor_callbacks.go`, find the end of the OnStreamFound twitch callback (line ~253). Append a new callback definition immediately after, and before the `broadcastAllTimers` definition:
```go
	s.twitchMon.OnStreamRecover = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig, jobID string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamRecover (twitch)", slog.Any("panic", r))
			}
		}()

		// Stash the fresh streamInfo so processTwitchLive consumes it instead
		// of re-querying GQL — same flap-prevention as the OnStreamFound path.
		s.dlWorker.StashTwitchStreamInfo(jobID, info)

		// AutoReinitializeJob increments auto_retry_count, clears state, and
		// re-enqueues. The cap (worker.MaxTwitchAutoRetries) is enforced by
		// the monitor's predicate before we even get here.
		s.dlWorker.AutoReinitializeJob(jobID)

		s.log.Info("auto-recovered twitch job",
			slog.String("jobID", jobID),
			slog.String("channel", info.ChannelDisplayName),
			slog.String("streamID", info.StreamID))
	}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./cmd/moombox/...
```
Expected: no errors.

- [ ] **Step 3: Run all tests**

```bash
go test ./...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/moombox/monitor_callbacks.go
git commit -m "main: wire OnStreamRecover (stash hint + AutoReinitializeJob)"
```

---

## Phase 9: Notification suppression for retry-driven errors

### Task 9.1: Suppress notifications when auto_retry_count > 0

**Files:**
- Modify: `internal/worker/worker.go`

- [ ] **Step 1: Update setJobError's suppression logic**

In `internal/worker/worker.go`, find `setJobError` (line 551). Modify the `suppressNotification` calculation. Before (line 570-573):
```go
	// Suppress notifications for non-actionable errors (matches TS behavior):
	// - Age-restricted content: nothing user can do
	// - Probe timeout: transient, stream may have ended naturally
	suppressNotification := errors.Is(err, ErrNonActionable)
```
After:
```go
	// Suppress notifications for non-actionable errors (matches TS behavior):
	// - Age-restricted content: nothing user can do
	// - Probe timeout: transient, stream may have ended naturally
	// - Twitch monitor-driven retries (auto_retry_count > 0): user already
	//   got the original error notification; subsequent retry-failure
	//   notifications would be noise on the same job.
	suppressNotification := errors.Is(err, ErrNonActionable) || job.AutoRetryCount > 0
```

- [ ] **Step 2: Verify it compiles + tests still pass**

```bash
go build ./internal/worker/...
go test ./internal/worker/... -v
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/worker/worker.go
git commit -m "worker: suppress error notifications during Twitch auto-retry"
```

### Task 9.2: Notification-suppression coverage

The suppression logic is two lines (`|| job.AutoRetryCount > 0` appended to an existing predicate). The notification path in `setJobError` uses a concrete `*notifications.Manager` rather than an interface, so a unit test would require refactoring the dependency injection — out of scope for this plan.

Coverage strategy: rely on Phase 10's smoke test (specifically Step 5's auto-recovery trigger). With a Discord/ntfy webhook configured, the user should observe **exactly one** error notification for the original `auto_retry_count=0` failure, and **no further notifications** for the auto-retries themselves. If the webhook receives a second "Job Failed" while `auto_retry_count > 0`, the suppression check is wrong; investigate.

No code-level test is added here.

---

## Phase 10: End-to-end manual verification

This phase has no code changes — it's a checklist for confirming the feature works against real Twitch.

### Task 10.1: Smoke test against a real Twitch channel

**Files:** none.

- [ ] **Step 1: Build a fresh binary**

```bash
go build -o moombox.exe ./cmd/moombox
```
Expected: builds clean.

- [ ] **Step 2: Migrate a test database**

Use `D:\Moombox` or a fresh test data directory. On first run after this build, the v13 migration should add `auto_retry_count`. Verify:
```bash
# After running moombox once and stopping it:
sqlite3 "D:/Moombox/moombox.db" "PRAGMA user_version;"
# Expected: 13
sqlite3 "D:/Moombox/moombox.db" ".schema jobs" | grep auto_retry_count
# Expected: auto_retry_count INTEGER NOT NULL DEFAULT 0
```

- [ ] **Step 3: Configure a live Twitch channel for monitoring**

Add a Twitch channel that's currently live (or set up `shachimu`-like scenario where a stream is actively going). Wait for the monitor to detect it — DEBUG-level logs should show:
```
twitch checking channels=N
twitch stream found channel=... streamID=...
Stream found by Twitch monitor jobID=tw_...
```

- [ ] **Step 4: Verify Phase 4 took effect**

The processor's GetStreamInfo call should be skipped. Look for:
```
twitch using stashed monitor hint jobID=tw_... streamID=... channel=...
```
in the log immediately after `processing stream platform=twitch`. If you don't see this line, the hint wasn't consumed — debug.

- [ ] **Step 5: Force the auto-recovery path (synthetic)**

The natural flap is rare and hard to reproduce. To validate the recovery path, manually flip an existing live job into a recoverable error state:
```bash
sqlite3 "D:/Moombox/moombox.db" "
UPDATE jobs
SET status='Error',
    error='twitch channel is offline',
    last_video_seq=NULL,
    auto_retry_count=0
WHERE id='tw_<your_test_streamID>';"
```
Within ~15s (next monitor tick), the log should show:
```
twitch recoverable error — re-enqueueing job jobID=tw_... prevRetries=0
auto-recovered twitch job jobID=tw_... channel=... streamID=...
processing job jobID=tw_...
twitch using stashed monitor hint jobID=tw_... streamID=...
twitch live stream ready channel=... quality=...
starting Twitch download jobID=tw_...
```

- [ ] **Step 6: Verify retry budget caps**

Repeat Step 5 but set `auto_retry_count=2` first:
```bash
sqlite3 "D:/Moombox/moombox.db" "
UPDATE jobs
SET status='Error',
    error='twitch channel is offline',
    last_video_seq=NULL,
    auto_retry_count=2
WHERE id='tw_<your_test_streamID>';"
```
Within ~15s, the log should NOT contain `recoverable error — re-enqueueing`. The job stays errored. Manual reinit via the UI should reset the counter and succeed.

- [ ] **Step 7: Verify user reinit resets the counter**

With the job in `auto_retry_count=2` Error state, click "Reinit" in the web UI. After reinit, query the counter:
```bash
sqlite3 "D:/Moombox/moombox.db" "SELECT auto_retry_count FROM jobs WHERE id='tw_<id>';"
# Expected: 0
```

- [ ] **Step 8: Document any deviations**

If any step produced unexpected output, capture the relevant log excerpts (timestamp + ~10 surrounding lines) and add them as a comment in the related commit, OR file a follow-up issue. Don't ship a partial fix.

- [ ] **Step 9: Final commit (if any tweaks needed)**

If Steps 1-8 surfaced bugs that needed fixes:
```bash
git add <files>
git commit -m "fix: <specific tweak from Phase 10 smoke>"
```

If everything passed cleanly, no final commit is needed.

---

## Multi-Angle Self-Review

This section reviews the plan against the angles that came up during design, plus standard rigor checks. Run through this checklist before declaring the plan done.

### Spec coverage
- [x] **Eliminate the flap false-error** → Phases 2-4 (hint cache + processor consumption + monitor stash).
- [x] **Auto-recover errored jobs whose stream is still live** → Phases 5-8 (counter, AutoReinitializeJob, predicate, monitor dispatch, OnStreamRecover wiring).
- [x] **Bounded retries** → Phase 5 task 5.3 (`MaxTwitchAutoRetries=2`); enforced at the predicate level in Phase 6.
- [x] **No notification noise during retries** → Phase 9.
- [x] **User reinit resets budget** → Phase 5 task 5.1.
- [x] **End-to-end verification** → Phase 10.

### Concurrency
- Hint cache uses `sync.Mutex`; stash and take are serialized. ✓
- `take` always deletes (even on TTL miss) — no lock contention from zombie entries. ✓
- DB writes go through `UpdateJobFields` which already serializes via `db.mu`. ✓
- Monitor's `checkChannel` runs under `tm.mu` already (existing code); no new contention added. ✓
- Worker queue's `IsProcessing` dedup handles the case where a manual reinit and an auto-recover land at the same time. ✓

### Memory leaks
- Hints expire after 60s; `take` is the only access path and always removes. ✓
- No periodic sweeper needed because there's no path that strands an entry indefinitely (every stash has a corresponding processing attempt within milliseconds; if that attempt is somehow lost, the next take call within 60s drops it). The cache could grow unboundedly if a flood of monitor calls stash without consumption — but the monitor's HasProcessed dedup means at most one stash per stream per broadcast.
- ⚠️ **Hardening**: a defensive periodic sweep (e.g. every 5min, drop entries older than TTL) would zero the worst case. Not added because the worst-case bound is small (one entry per active broadcast); revisit if profiling shows growth.

### Database migration safety
- `ALTER TABLE ADD COLUMN` with `DEFAULT 0` is fast and non-blocking on SQLite (no row rewrite). ✓
- Idempotent via `isDuplicateColumnErr` check (matches existing migration patterns). ✓
- `PRAGMA user_version` advance is atomic with the schema change in the same transaction-less migrate path used by all prior migrations. ✓
- Existing jobs default to `auto_retry_count=0` — they're treated as fresh-budget on next reinit, which is correct.

### Job ID collision
- Twitch streamIDs are per-broadcast unique (Helix/GQL contract). Same channel goes live twice → two different streamIDs → two different jobIDs. The recovery predicate matches on jobID, so cross-broadcast contamination is impossible. ✓

### Frequent rechecks reviving stale errored jobs (user's concern)
This was the explicit risk the user raised: "Twitch does the rechecks very frequently — could it kick to life jobs that errored if they match the ID?". The design has three layers preventing unwanted revival:

1. **HasProcessed dedup remains the gate.** After the first `OnStreamFound`, `AddToHistory(jobID)` is called. Every subsequent `checkChannel` returns at the `if processed` short-circuit *before* the dispatch decision. The new `OnStreamRecover` branch lives INSIDE that short-circuit and only fires when the predicate matches a *narrow* error shape. Mismatching errors (cookie expired, HLS 404, finished/cancelled jobs, …) hit the `return nil` line below and are silently skipped.
2. **Retry budget caps revival.** Once `auto_retry_count >= MaxTwitchAutoRetries` (currently 2), the predicate returns false and the job stays errored. The user has full control via Reinit to grant a fresh budget.
3. **`LastVideoSeq` guard prevents re-killing partial downloads.** If the job ever made it past the probe phase (any `last_video_seq` value), the predicate refuses to recover — that error belongs to the download/orchestrator path, not the probe path.

Concretely, in the `tw_316500481527` flap from the original bug report: the predicate matches (StatusError + offline string + LastVideoSeq=nil + AutoRetryCount<2), so it'd get one auto-retry. If that retry fails, a second auto-retry. If both fail (now AutoRetryCount=2), the predicate stops matching and the job sits in Error until the user acts. Maximum three total processing attempts in ~30s, then user-driven from there.

### Race: user reinit during auto-recovery
- Both paths terminate in `EnqueueJob`. Worker queue's `IsProcessing` map dedups. Worst case: redundant enqueue, processed once. ✓
- Counter semantics: if user reinit lands first, counter goes to 0; subsequent auto-recover would increment from 0 (correct — fresh budget). If auto-recover lands first, counter goes to N+1; subsequent user reinit resets to 0 (correct — user always gets a fresh budget).

### Monitor restart between OnStreamFound and worker pickup
- Hint map is in-memory; lost on restart. ✓ (acknowledged degradation, not a regression — restart-time recovery falls through to fresh GetStreamInfo, identical to today)
- `enqueueExistingJobs` on startup picks up jobs in `Live`/`Upcoming`/`Downloading`. Errored jobs stay errored — correct, because we don't want monitor restart to silently resurrect errors.

### Errors that aren't transient
- Cookie required (`StatusCookies`) → predicate matches `StatusError` only, so `StatusCookies` jobs are skipped. ✓
- HLS playlist failures (`twitch HLS error: ...`) → predicate matches the literal `twitchOfflineErrMsg` only. ✓
- VOD errors (`twitch VOD error: ...`) → same string-mismatch protection. ✓
- Manual jobs (`tw_manual_*`) take the `waitForTwitchLive` path which never produces the offline-flap error → predicate naturally excludes them. ✓

### Retry budget exhaustion
- Predicate's `AutoRetryCount >= maxRetries` guard fires before dispatch. ✓
- After the third total attempt fails (original + 2 retries), the job sticks at `Error`. ✓
- Notification suppression keeps the user from being spammed during retries; the original error notification stands as the single user-facing signal.

### Frontend display
- The new column is exported via JSON tag `autoRetryCount,omitempty`. Web UI doesn't read it today; nothing breaks.
- Future enhancement: surface "Auto-recovered N times" in the job details panel. Out of scope for this plan.

### Phase 7 cycle risk
- Plan checks for the cycle in Step 3 of Task 7.1. If a cycle exists (`worker` imports `monitor`), the fallback is a duplicated constant + regression test. The likely outcome on this codebase is no cycle (monitor depends on database/twitch/config, worker depends on monitor's siblings via... actually let me re-read)— double-check by running the grep in step 3 before committing the import.

### Sentinel approach revisited
- We string-match against `worker.TwitchOfflineErrMsg` — a shared exported constant. Producer (`stream_processor_twitch.go`) and consumer (`monitor/twitch_recover.go`) both reference it, so drift is structurally impossible: rename one site, the other won't compile.
- A further refactor could promote this to a real `ErrTwitchOffline` sentinel (analogous to existing `ErrCookiesRequired`/`ErrNonActionable`). Defer because:
  1. The error round-trips through `database.Job.Error` as plain text — `errors.Is` wrapping wouldn't survive the DB layer anyway.
  2. The string-via-constant approach already has compile-time drift protection.
  3. Test coverage in Phase 6 explicitly references the constant via `worker.TwitchOfflineErrMsg`.

### Feature-flag / kill-switch
- Auto-recovery activates whenever `OnStreamRecover` is wired (Phase 8). To temporarily disable without a code change, the user can set the callback to nil (would require a config flag). **Not added to this plan** — the cap (2 retries) plus the narrow predicate means the worst-case behaviour is identical to today's "user reinits the job 3 times" workflow. If real-world traffic shows pathologies, add a `[twitch] auto_recover = true` config flag in a follow-up.

### Test pyramid
- **Unit tests** (Phases 1.3, 2.1, 3.2, 5.2, 6.1, 7.2): cover hint cache, predicate, AutoReinitializeJob counter behaviour, ReinitializeJob counter reset, DB column read/write.
- **Integration tests**: minimal — Phase 7.2 stays at predicate level because constructing a real TwitchMonitor with a mock GQL service is heavy and out of scope. The full path is exercised in Phase 10.
- **Manual smoke** (Phase 10): the only way to confirm the live Twitch GQL flap behaviour. Documented step-by-step.

### Files touched (final inventory)
- `internal/database/migrations.go` (Phase 1.1)
- `internal/database/types.go` (Phase 1.2)
- `internal/database/database.go` (Phase 1.2)
- `internal/database/database_test.go` (Phase 1.3)
- `internal/worker/twitch_hint.go` (NEW, Phase 2.1)
- `internal/worker/twitch_hint_test.go` (NEW, Phase 2.1, 3.2)
- `internal/worker/stream_processor.go` (Phase 2.2)
- `internal/worker/worker.go` (Phase 2.2, 5.1, 5.2, 5.3, 9.1)
- `internal/worker/stream_processor_twitch.go` (Phase 3.1)
- `internal/worker/worker_test.go` (Phase 5.2, 9.2)
- `cmd/moombox/monitor_callbacks.go` (Phase 4.1, 8.1)
- `internal/monitor/twitch_recover.go` (NEW, Phase 6.1)
- `internal/monitor/twitch_recover_test.go` (NEW, Phase 6.1, 7.2)
- `internal/monitor/twitch.go` (Phase 7.1)
- `docs/superpowers/plans/2026-05-03-twitch-flap-auto-recovery.md` (this plan)

15 files, 4 new. Approximately 250 lines of production code, 200 lines of tests.

### What this plan does NOT do
- Surface auto-recovery state in the Web UI / TUI (out of scope; users notice via faster recovery, not a UI badge).
- Generalize to YouTube (different failure modes; no analogous flap pattern observed).
- Convert the `"twitch channel is offline"` string to a typed sentinel (deferred refactor; see "Sentinel approach revisited" above).
- Add a config flag to disable auto-recovery (deferred; bounded blast radius makes the kill-switch unnecessary today).
- Backfill `auto_retry_count` for existing errored jobs (`DEFAULT 0` handles this implicitly; a backfill would be cosmetic).
- **Refresh sparse metadata captured during a flap.** When Twitch GQL StreamMetadata returns a sparse `Stream` object (StreamID populated but Title/DisplayName empty — the exact shape we observed in the 2026-05-03 incident), the monitor's hint carries that sparse data forward. The processor's existing metadata fix-up block (`stream_processor_twitch.go:179-200`) is a no-op against sparse input, so the job persists with a placeholder title like `"shachimu — 2026-05-03T22:05:56Z"` and lowercase channel name. The download itself succeeds (HLS access tokens use a different endpoint that wasn't sparse). This is a deliberate tradeoff: an ugly title plus a successful download beats a failed job that needs manual reinit. A late metadata-refresh pass (one extra `GetStreamInfo` call ~30s into the download to backfill) is a clean follow-up if real users complain.
- Add hint-cache observability (hit/miss counters). If a future refactor silently breaks the stash path, the only signal is "no flap errors" — easy to mistake for "no flaps occurring". A small atomic counter exposed via the existing stats endpoint is a reasonable follow-up.

---

## Notes for the executing engineer

- **Critical patterns from CLAUDE.md** that this plan respects: anonymous logger interface (don't extract), `UpdateJobFields` for partial updates, panic recovery on goroutines (the new `OnStreamRecover` callback in Phase 8 includes the inline `defer recover()` like its siblings).
- **Don't skip Phase 1's tests**: `TestFieldToColumnCoverage` is a tripwire that catches every column drift. If you add a new column to the schema and forget to update `fieldToColumn`, this test fails.
- **Phase 10's smoke test is mandatory.** The unit tests can't see real Twitch flaps; they only validate the plumbing. Skipping Phase 10 leaves the production behaviour unverified.
- **Frequent commits**: the plan structure intentionally commits per-task so a failed phase can be partially salvaged. Don't bundle.
