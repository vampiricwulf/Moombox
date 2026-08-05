// Package database provides SQLite-based persistence for Moombox.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// fieldToColumn maps UpdateJobFields key names to database column names.
// Every column on `jobs` that callers should be able to write partially should
// have an entry here. When adding a new column to the schema, add a matching
// entry here (TestFieldToColumnCoverage keeps this honest).
var fieldToColumn = map[string]string{
	"status":              "status",
	"progress":            "progress",
	"percent":             "percent",
	"eta":                 "eta",
	"speed":               "speed",
	"error":               "error",
	"title":               "title",
	"channel_name":        "channel_name",
	"thumbnail_url":       "thumbnail_url",
	"description":         "description",
	"output_file":         "output_file",
	"filename":            "filename",
	"output_directory":    "output_directory",
	"download_started_at": "download_started_at",
	"stream_start_time":   "stream_start_time",
	"stream_end_time":     "stream_end_time",
	"length_seconds":      "length_seconds",
	"last_video_seq":      "last_video_seq",
	"last_audio_seq":      "last_audio_seq",
	"total_video_seq":     "total_video_seq",
	"total_audio_seq":     "total_audio_seq",
	"total_chat_messages": "total_chat_messages",
	"chat_status":         "chat_status",
	"chat_filename":       "chat_filename",
	"chat_file":           "chat_file",
	"thumbnail_file":      "thumbnail_file",
	"description_file":    "description_file",
	"is_vod":              "is_vod",
	"manually_added":      "manually_added",
	"allow_non_stream":    "allow_non_stream",
	"video_width":         "video_width",
	"video_height":        "video_height",
	"video_fps":           "video_fps",
	"file_size":           "file_size",
	"last_recheck_at":     "last_recheck_at",
	"twitch_quality":      "twitch_quality",
	"twitch_category":     "twitch_category",
	"channel_avatar_url":  "channel_avatar_url",
	"selected_video_itag": "selected_video_itag",
	"selected_audio_itag": "selected_audio_itag",
	"start_time":          "start_time",
	"end_time":            "end_time",
	"quality_preference":  "quality_preference",
	"watched":             "watched",
	"resume_position":     "resume_position",
	"chat_offset":         "chat_offset",
	"auto_retry_count":    "auto_retry_count",
	"queue_priority":      "queue_priority",
	"incomplete_tail":     "incomplete_tail",
}

// dbLogger is the interface for database error logging.
type dbLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// executor abstracts *sql.DB and *sql.Tx for shared query execution.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Database provides SQLite-backed persistence for Moombox.
type Database struct {
	db        *sql.DB
	ctx       context.Context // Optional context for query cancellation
	mu        sync.RWMutex
	closeOnce sync.Once
	logger    dbLogger

	// fieldToColumn is a per-instance copy of the package-level map. Snapshotting
	// into the struct at Open() keeps the map read-only at runtime — callers
	// writing to a shared mutable package-level map would otherwise risk races.
	fieldToColumn map[string]string

	// Pub/sub. onJobChange coexists with onJobUpdate during the
	// DECISIONS #21 migration — both fire on every UpdateJobFields,
	// so callers can opt into the richer JobChange shape (full Job +
	// changed columns) without disturbing legacy OnJobUpdate
	// subscribers. onJobAdded is the lifecycle counterpart on the
	// AddJob writer path; like onJobChange it coexists with the
	// legacy onJobsChange dispatch until consumers migrate.
	onJobUpdate    []jobUpdateSub
	onJobChange    []jobChangeSub
	onJobAdded     []jobAddedSub
	onJobDeleted   []jobDeletedSub
	onTrimsChanged []trimsChangedSub
	onJobsChange   []jobsChangeSub
	nextSubID      uint64
	subMu          sync.RWMutex

	// Prepared statements
	stmtGetJob *sql.Stmt
	// preparedStmts tracks every *sql.Stmt prepared via prepareStmt so Close
	// can release them without each addition risking a leak.
	preparedStmts []*sql.Stmt

	// Per-job in-memory log buffers
	jobLogsMu sync.RWMutex
	jobLogs   map[string][]string

	// GetJobStats cache — the aggregate is a full-table scan, so results are
	// cached for jobStatsCacheTTL. Not invalidated on writes: stats drive UI
	// widgets where a few-second staleness is acceptable.
	statsMu       sync.Mutex
	statsCached   *JobStats
	statsCachedAt time.Time
}

// getCtx returns the stored context or context.Background().
func (db *Database) getCtx() context.Context {
	if db.ctx != nil {
		return db.ctx
	}
	return context.Background()
}

// jobStatsCacheTTL is how long GetJobStats' result stays fresh in the in-memory
// cache before another full-table scan is required.
const jobStatsCacheTTL = 5 * time.Second

// FileSchemaVersion reads PRAGMA user_version from a database file WITHOUT
// running migrations (read-only open). Side processes use it to refuse a
// database whose schema doesn't match their binary — Open migrates
// unconditionally, and migrating the daemon's live DB from a second process
// (e.g. a newer on-disk binary during the staged-update window) would leave
// the running daemon's old code writing against a new schema.
func FileSchemaVersion(dbPath string) (int, error) {
	sqlDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		return 0, err
	}
	defer sqlDB.Close()
	var v int
	if err := sqlDB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// Open creates or opens a SQLite database at the given path.
// The logger parameter is optional; if nil, database errors will be silently dropped.
func Open(dbPath string, logger ...dbLogger) (*Database, error) {
	// modernc.org/sqlite only honors `_pragma=...` query parameters — the
	// mattn-style `_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on`
	// form was silently ignored, leaving foreign keys OFF (the child tables'
	// ON DELETE CASCADE never fired), journal mode DELETE, and busy timeout
	// 0 (the `moombox add` second process got immediate SQLITE_BUSY).
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool
	sqlDB.SetMaxOpenConns(1) // SQLite is single-writer
	sqlDB.SetMaxIdleConns(1)

	// Snapshot the package-level fieldToColumn map into the instance so that
	// the package-level map is effectively immutable once any Database is open.
	ftc := make(map[string]string, len(fieldToColumn))
	maps.Copy(ftc, fieldToColumn)

	db := &Database{
		db:            sqlDB,
		fieldToColumn: ftc,
		jobLogs:       make(map[string][]string),
	}
	if len(logger) > 0 && logger[0] != nil {
		db.logger = logger[0]
	}

	// Run migrations
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	// Prepare hot-path statements
	if err := db.prepareStatements(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("failed to prepare statements: %w", err)
	}

	return db, nil
}

// prepareStmt prepares a statement and records it in preparedStmts so Close
// can release it. All prepared statements should flow through this helper.
func (db *Database) prepareStmt(query string) (*sql.Stmt, error) {
	stmt, err := db.db.PrepareContext(context.Background(), query)
	if err != nil {
		return nil, err
	}
	db.preparedStmts = append(db.preparedStmts, stmt)
	return stmt, nil
}

func (db *Database) prepareStatements() error {
	var err error

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
		auto_retry_count, channel_id, queue_priority, incomplete_tail
		FROM jobs WHERE id = ?`)
	if err != nil {
		return err
	}

	return nil
}

// Close closes the database.
// Safe to call concurrently — only the first call performs cleanup.
func (db *Database) Close() error {
	var closeErr error
	db.closeOnce.Do(func() {
		// Release all prepared statements tracked by prepareStmt.
		for _, stmt := range db.preparedStmts {
			if stmt != nil {
				stmt.Close()
			}
		}

		closeErr = db.db.Close()
	})
	return closeErr
}

// Helper functions

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intToBool(i int) bool {
	return i != 0
}

// updateJobExec performs a full job UPDATE using the provided executor (either *sql.DB or *sql.Tx).
// This is the single implementation shared by both direct and transactional update paths.
func updateJobExec(ctx context.Context, exec executor, job *Job) error {
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
}

// insertJobExec performs an INSERT OR IGNORE INTO jobs using the provided
// executor (either *sql.DB or *sql.Tx). Single implementation shared by
// AddJob and ImportFromJSON so the 43-column INSERT only exists once.
//
// channel_id and queue_priority are written on EVERY insert (spec §10):
// a nil ChannelID stores NULL — never "" — and the Go zero QueuePriority
// stores an explicit 0, so no creator ever inherits the schema's
// fail-closed DEFAULT 1 (which exists only for pre-v16 legacy rows).
func insertJobExec(ctx context.Context, exec executor, job *Job) (sql.Result, error) {
	return exec.ExecContext(ctx, `INSERT OR IGNORE INTO jobs (id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, chat_status, total_chat_messages, chat_filename, chat_file,
		thumbnail_file, description_file,
		twitch_quality, twitch_category, channel_avatar_url,
		selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at,
		quality_preference, channel_id, queue_priority)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?)`,
		job.ID, job.VideoID, job.URL, job.Title, job.ChannelName, job.Platform,
		job.Status, job.Progress, job.Percent, job.ETA, job.Speed, job.Error,
		job.CreatedAt, job.UpdatedAt,
		boolToInt(job.IsVod), boolToInt(job.ManuallyAdded), boolToInt(job.AllowNonStream),
		job.StreamStartTime, job.StreamEndTime,
		job.LengthSeconds, job.DownloadStartedAt, job.ThumbnailURL, job.Description,
		job.OutputFile, job.Filename, job.OutputDirectory,
		job.ChatStatus, job.TotalChatMessages, job.ChatFilename, job.ChatFile,
		job.ThumbnailFile, job.DescriptionFile,
		job.TwitchQuality, job.TwitchCategory, job.ChannelAvatarURL,
		job.SelectedVideoItag, job.SelectedAudioItag, job.StartTime, job.EndTime,
		job.LastRecheckAt,
		job.QualityPreference, job.ChannelID, job.QueuePriority)
}

// UpdateJobFields performs a partial update of a job using a map of field names to values.
// This is useful when only a few fields need to change without loading the full job.
// Returns the updated job after notifying subscribers, or nil on error.
//
// Note: updated_at is bumped and OnJobUpdate fires on every call, even when the
// supplied values match what's already on disk (no dirty check). Callers that
// would otherwise emit duplicate writes — e.g. a status set to its current
// value — should dedupe at the call site (audit reports/database.md U6).
func (db *Database) UpdateJobFields(id string, fields map[string]any) *Job {
	if len(fields) == 0 {
		return nil
	}

	db.mu.Lock()

	setClauses := make([]string, 0, len(fields)+1)
	args := make([]any, 0, len(fields)+2)

	for key, val := range fields {
		col, ok := db.fieldToColumn[key]
		if !ok {
			if db.logger != nil {
				db.logger.Debug("UpdateJobFields: ignoring unknown field", "jobID", id, "field", key)
			}
			continue
		}
		setClauses = append(setClauses, col+"=?")
		args = append(args, val)
	}

	if len(setClauses) == 0 {
		db.mu.Unlock()
		return nil
	}

	// Always update updated_at
	setClauses = append(setClauses, "updated_at=?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)

	query := "UPDATE jobs SET " + strings.Join(setClauses, ", ") + " WHERE id=?"
	_, err := db.db.ExecContext(db.getCtx(), query, args...)
	if err != nil {
		db.mu.Unlock()
		if db.logger != nil {
			db.logger.Error("UpdateJobFields failed", "jobID", id, "err", err)
		}
		return nil
	}

	// Capture the schema column names that were actually written so
	// OnJobChange subscribers can drive fine-grained updates. Don't
	// include "updated_at" — every UpdateJobFields call bumps it, so
	// signalling it would defeat the consumer's "skip updated_at-only
	// updates" optimisation.
	changes := make([]string, 0, len(setClauses))
	for _, clause := range setClauses {
		col := clause[:len(clause)-2] // strip "=?"
		if col == "updated_at" {
			continue
		}
		changes = append(changes, col)
	}

	// Read back the full job under the same critical section so subscribers
	// see consistent state. TUI + WebSocket need all fields; UpdateJobFields
	// only wrote a subset, so a SELECT is required.
	job, scanErr := scanJob(db.stmtGetJob.QueryRowContext(db.getCtx(), id))
	db.mu.Unlock() // Release BEFORE notify so subscribers can call back into Database without deadlocking. Audit C1.

	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			// Row was deleted between the UPDATE and the read-back.
			// Fire notifyJobDeleted so the orchestrator's OnJobDeleted
			// listener can cancel the per-job context — same signal the
			// explicit DeleteJob path emits. Duplicate-fire is benign:
			// subscribers (WS hub, orchestrator) are idempotent on
			// "already gone." Debug-level since this is an expected
			// outcome of delete-while-active, not an error.
			if db.logger != nil {
				db.logger.Debug("UpdateJobFields: row gone after update", "jobID", id, "err", scanErr)
			}
			db.notifyJobDeleted(id)
		} else if db.logger != nil {
			// Anything else (DB corruption, schema drift, transient I/O)
			// is a real failure operators need to see.
			db.logger.Error("UpdateJobFields: failed to read back job", "jobID", id, "err", scanErr)
		}
		return nil
	}

	db.notifyJobUpdate(job, changes)
	return job
}

// silentColumns is the whitelist of columns that may be updated via
// updateSingleColumnSilent. Restricting to this set prevents accidental misuse
// for fields that should trigger subscriber notifications.
var silentColumns = map[string]struct{}{
	"resume_position": {},
	"chat_offset":     {},
}

// updateSingleColumnSilent sets a single column on jobs WITHOUT bumping
// updated_at or notifying subscribers. Designed for high-frequency player
// state saves (every ~10s). The column name must be present in silentColumns
// so this helper can't be misused for subscribed fields.
func (db *Database) updateSingleColumnSilent(jobID, column string, value any, opName string) {
	if _, ok := silentColumns[column]; !ok {
		if db.logger != nil {
			db.logger.Error(opName+": disallowed column", "column", column)
		}
		return
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(),
		"UPDATE jobs SET "+column+" = ? WHERE id = ?", value, jobID)
	if err != nil && db.logger != nil {
		db.logger.Error(opName+" failed", "jobID", jobID, "err", err)
	}
}

// UpdateResumePosition saves the playback position without bumping updated_at
// or triggering subscriber notifications. Designed for frequent periodic saves
// during video playback (every ~10 seconds).
func (db *Database) UpdateResumePosition(jobID string, seconds float64) {
	db.updateSingleColumnSilent(jobID, "resume_position", seconds, "UpdateResumePosition")
}

// UpdateChatOffset saves the chat timing offset without bumping updated_at
// or triggering subscriber notifications.
func (db *Database) UpdateChatOffset(jobID string, offset float64) {
	db.updateSingleColumnSilent(jobID, "chat_offset", offset, "UpdateChatOffset")
}

// rowScanner is implemented by both *sql.Row and *sql.Rows, allowing a single
// scanJobRow function to serve both the single-row and iteration paths.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJobRow reads a Job from either an *sql.Row (single-row query) or an
// *sql.Rows iterator (multi-row query). Column order must match the SELECT
// list used by prepareStatements() and getAllJobsUnlocked().
func scanJobRow(r rowScanner) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream, watched, incompleteTail int
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
		&j.AutoRetryCount, &j.ChannelID, &j.QueuePriority, &incompleteTail,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = intToBool(isVod)
	j.ManuallyAdded = intToBool(manuallyAdded)
	j.AllowNonStream = intToBool(allowNonStream)
	j.Watched = intToBool(watched)
	j.IncompleteTail = intToBool(incompleteTail)
	return &j, nil
}

// scanJob is a thin wrapper for single-row scans. Retained for readability at
// call sites that expect an *sql.Row.
func scanJob(row *sql.Row) (*Job, error) { return scanJobRow(row) }

// scanJobRows is a thin wrapper for multi-row iteration scans.
func scanJobRows(rows *sql.Rows) (*Job, error) { return scanJobRow(rows) }
