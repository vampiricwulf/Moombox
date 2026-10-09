// Package database provides SQLite-based persistence for Moombox.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
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
	"park_reason":         "park_reason",
	"park_identity":       "park_identity",
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
	// AddJob writer path; onJobsChange is left to the two bulk writers
	// (BatchSetWatched, DeleteJobsAndHistoryForChannel).
	onJobUpdate    []jobUpdateSub
	onJobChange    []jobChangeSub
	onJobAdded     []jobAddedSub
	onJobDeleted   []jobDeletedSub
	onTrimsChanged []trimsChangedSub
	onJobsChange   []jobsChangeSub
	nextSubID      uint64
	subMu          sync.RWMutex

	// jobsChangeSeq numbers each OnJobsChange snapshot as it is TAKEN (under
	// db.mu, so in write order); jobsChangeDelivered is the newest number
	// handed to subscribers, guarded by jobsChangeMu. dispatchJobsChange runs
	// asynchronously, so two bulk writes in quick succession raced to the
	// subscribers, and an older full list landing last brought pruned jobs
	// back onto every dashboard. A snapshot older than one already delivered
	// is dropped instead.
	jobsChangeSeq       uint64
	jobsChangeMu        sync.Mutex
	jobsChangeDelivered uint64

	// jobWriteVersion is the last Job.Version handed out; incremented under
	// db.mu by every UpdateJobFields and AddJob read-back, so versions follow
	// write order.
	jobWriteVersion uint64

	// Prepared statements
	stmtGetJob *sql.Stmt
	// preparedStmts tracks every *sql.Stmt prepared via prepareStmt so Close
	// can release them without each addition risking a leak.
	preparedStmts []*sql.Stmt

	// Per-job in-memory log buffers
	jobLogsMu sync.RWMutex
	jobLogs   map[string][]string
	// logRouted is the SET of job IDs RouteLogToJobs scans. It is deliberately
	// not the same map as jobLogs: tracking follows only non-terminal jobs
	// (a years-old Finished row cost 47 µs of substring scanning per log
	// line at 5,000 jobs, under the write lock — CORE-12), while the BUFFER
	// must outlive the terminal transition because the operator reads a
	// failed job's log right after it fails. Guarded by jobLogsMu.
	logRouted map[string]struct{}

	// GetJobStats cache — the aggregate is a full-table scan, so results are
	// cached for jobStatsCacheTTL. Not invalidated on writes: stats drive UI
	// widgets where a few-second staleness is acceptable.
	statsMu       sync.Mutex
	statsCached   *JobStats
	statsCachedAt time.Time
}

// getCtx is the context every statement runs under: context.Background().
// Open takes no context and nothing cancels a statement mid-flight — a
// "query cancellation" field this used to read was never assigned. This is
// the one place to change if shutdown should ever cancel in-flight queries.
func (db *Database) getCtx() context.Context {
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
	sqlDB, err := sql.Open("sqlite", sqliteFileURI(dbPath)+"?mode=ro&_pragma=busy_timeout(5000)")
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

// sqliteURIPathEscaper escapes the three characters SQLite's URI parser reads
// in a "file:" path: '%' (a %HH escape), '?' (the query) and '#' (a fragment).
var sqliteURIPathEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// sqliteFileURI is the "file:" URI that names dbPath literally. modernc opens
// every "file:" DSN with SQLITE_OPEN_URI, so SQLite parses the path as a URI:
// pasted in raw, a database_path of "/srv/Moombox #2/moombox.db" opened
// whatever came before the '#' as the database, one with a '?' was cut there
// and lost the busy_timeout pragma into the query, and "%41" was decoded to
// "A". Only those three characters are escaped, so a path without them —
// relative, with spaces, or a Windows one with a drive letter and backslashes
// — reads exactly as before. A path that starts with "//" is given an empty
// authority in front of it, or SQLite would read its first segment as a host
// name and refuse it.
func sqliteFileURI(dbPath string) string {
	p := sqliteURIPathEscaper.Replace(dbPath)
	if strings.HasPrefix(p, "//") {
		p = "//" + p
	}
	return "file:" + p
}

// openDSN builds the SQLite connection string. Production keeps SQLite's
// default synchronous level (FULL in WAL mode: an fsync per commit; the
// durability ruling of 2026-07-03 stands). Under `go test` — and only there,
// testing.Testing() is the runtime's own answer — synchronous is OFF: every
// test opens its own database and migrates it from scratch, and those fsyncs
// were most of a 205-second database package on the Windows CI runner
// (5.6 s on Linux, where fsync is cheap). Nothing else about the test
// database differs.
func openDSN(dbPath string, underTest bool) string {
	dsn := sqliteFileURI(dbPath) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if underTest {
		dsn += "&_pragma=synchronous(OFF)"
	}
	return dsn
}

// Open creates or opens a SQLite database at the given path.
// The logger parameter is optional; if nil, database errors will be silently dropped.
func Open(dbPath string, logger ...dbLogger) (*Database, error) {
	// modernc.org/sqlite only honors `_pragma=...` query parameters — the
	// mattn-style `_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on`
	// form was silently ignored, leaving foreign keys OFF (the child tables'
	// ON DELETE CASCADE never fired), journal mode DELETE, and busy timeout
	// 0 (the `moombox add` second process got immediate SQLITE_BUSY).
	sqlDB, err := sql.Open("sqlite", openDSN(dbPath, testing.Testing()))
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
		logRouted:     make(map[string]struct{}),
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
		auto_retry_count, channel_id, queue_priority, incomplete_tail, park_reason, park_identity,
		notification_msgs
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

// insertJobExec performs an INSERT OR IGNORE INTO jobs using the provided
// executor (either *sql.DB or *sql.Tx), so the 44-column INSERT exists
// once.
//
// channel_id and queue_priority are written on EVERY insert (spec §10):
// a nil ChannelID stores NULL — never "" — and the Go zero QueuePriority
// stores an explicit 0, so no creator ever inherits the schema's
// fail-closed DEFAULT 1 (which exists only for pre-v16 legacy rows).
//
// A Job field this list leaves out takes the schema default whatever the
// caller set, silently. TestAddJobStoresEveryCreationField pins the split:
// every field is either written here or on its runtime-only list.
func insertJobExec(ctx context.Context, exec executor, job *Job) (sql.Result, error) {
	return exec.ExecContext(ctx, `INSERT OR IGNORE INTO jobs (id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, chat_status, total_chat_messages, chat_filename, chat_file,
		thumbnail_file, description_file,
		twitch_quality, twitch_category, channel_avatar_url,
		selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at,
		quality_preference, channel_id, queue_priority, file_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?)`,
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
		job.QualityPreference, job.ChannelID, job.QueuePriority, job.FileSize)
}

// UpdateJobFields performs a partial update of a job using a map of field names to values.
// This is useful when only a few fields need to change without loading the full job.
// Returns the updated job after notifying subscribers, or nil on error. The
// job is the row GetJob would return, child rows included, unless the write
// was a progress tick (IsProgressOnlyChange; see JobChange).
//
// Note: updated_at is bumped and OnJobUpdate fires on every call, even when the
// supplied values match what's already on disk (no dirty check). Callers that
// would otherwise emit duplicate writes — e.g. a status set to its current
// value — should dedupe at the call site (audit reports/database.md U6).
func (db *Database) UpdateJobFields(id string, fields map[string]any) *Job {
	job, _ := db.updateJobFieldsWhere(id, fields, "", nil)
	return job
}

// UpdateJobFieldsIf is UpdateJobFields as a compare-and-set on the row's
// status: the write applies only while the row's status is still expected,
// and reports whether it did. A transition decided on a status read earlier
// — the backlog scheduler's Queued → Upcoming admission — would otherwise
// write over whatever landed between the read and the write: an operator's
// Cancel, which the admission then turned back into a download.
//
// The check and the write are one UPDATE statement, so nothing can land
// between them. updated_at and the OnJobUpdate / OnJobChange subscribers move
// only when the write applied; a row whose status had moved on is left
// exactly as it is, and so is a row that no longer exists (no
// notifyJobDeleted either — the delete fired its own).
func (db *Database) UpdateJobFieldsIf(id string, expected JobStatus, fields map[string]any) bool {
	_, applied := db.updateJobFieldsWhere(id, fields, "status=?", []any{expected})
	return applied
}

// UpdateJobFieldsUnless is UpdateJobFieldsIf's complement: the write applies
// only while the row's status is NOT unwanted. For a write whose caller knows
// the one status it must not overwrite rather than the one it expects — the
// worker recording a job's failure must not turn an operator's Cancelled
// into Error, whatever status the run itself had reached.
func (db *Database) UpdateJobFieldsUnless(id string, unwanted JobStatus, fields map[string]any) bool {
	_, applied := db.updateJobFieldsWhere(id, fields, "status<>?", []any{unwanted})
	return applied
}

// UpdateJobFieldsUnlessTerminal is UpdateJobFieldsUnless over every terminal
// status at once (Job.IsTerminal: Finished, Error, Cancelled): the write
// applies only while the row has not reached an outcome. For a write that
// must not undo whichever outcome landed since its caller read the row — the
// operator's Cancel, decided on the status a UI showed, turned a job that had
// finished or failed meanwhile into a Cancelled one.
func (db *Database) UpdateJobFieldsUnlessTerminal(id string, fields map[string]any) bool {
	args := make([]any, len(terminalStatuses))
	for i, st := range terminalStatuses {
		args[i] = st
	}
	cond := "status NOT IN (?" + strings.Repeat(", ?", len(args)-1) + ")"
	_, applied := db.updateJobFieldsWhere(id, fields, cond, args)
	return applied
}

// updateJobFieldsWhere is the dynamic SET machinery behind UpdateJobFields
// and its two conditional forms. cond, when not empty, is ANDed to the
// statement's WHERE id=? with condArgs as its arguments, and a statement it
// matched no row for reports false and touches nothing else. Unconditional,
// it behaves exactly as UpdateJobFields always has, and reports whether the
// statement ran.
func (db *Database) updateJobFieldsWhere(id string, fields map[string]any, cond string, condArgs []any) (*Job, bool) {
	if len(fields) == 0 {
		return nil, false
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
		return nil, false
	}

	// Always update updated_at
	setClauses = append(setClauses, "updated_at=?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)

	query := "UPDATE jobs SET " + strings.Join(setClauses, ", ") + " WHERE id=?"
	if cond != "" {
		query += " AND " + cond
		args = append(args, condArgs...)
	}
	res, err := db.db.ExecContext(db.getCtx(), query, args...)
	if err != nil {
		db.mu.Unlock()
		if db.logger != nil {
			db.logger.Error("UpdateJobFields failed", "jobID", id, "err", err)
		}
		return nil, false
	}
	if cond != "" {
		// A driver that cannot say how many rows changed is read as a
		// match: the read-back below then publishes the row as it really
		// is, the same assumption updateSingleColumnSilent makes.
		if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
			db.mu.Unlock()
			return nil, false
		}
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
	// only wrote a subset, so a SELECT is required. The whole row, child
	// rows included, as GetJob reads it — except for a progress tick, which
	// moves none of them and no subscriber replaces a row with (JobChange).
	job, scanErr := scanJob(db.stmtGetJob.QueryRowContext(db.getCtx(), id))
	if scanErr == nil {
		if !IsProgressOnlyChange(changes) {
			db.loadChildRows(job)
		}
		db.jobWriteVersion++
		job.Version = db.jobWriteVersion
	}
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
		return nil, true
	}

	db.notifyJobUpdate(job, changes)
	return job, true
}

// silentColumns is the whitelist of columns that may be updated via
// updateSingleColumnSilent. Restricting to this set prevents accidental misuse
// for fields that should trigger subscriber notifications.
var silentColumns = map[string]struct{}{
	"resume_position":   {},
	"chat_offset":       {},
	"notification_msgs": {},
}

// updateSingleColumnSilent sets a single column on jobs WITHOUT bumping
// updated_at or notifying subscribers. Designed for high-frequency player
// state saves (every ~10s). The column name must be present in silentColumns
// so this helper can't be misused for subscribed fields. Returns true when a
// row was actually updated, so callers can report "job not found" for an
// unknown jobID instead of silently answering success (U-M9).
func (db *Database) updateSingleColumnSilent(jobID, column string, value any, opName string) bool {
	if _, ok := silentColumns[column]; !ok {
		if db.logger != nil {
			db.logger.Error(opName+": disallowed column", "column", column)
		}
		return false
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.db.ExecContext(db.getCtx(),
		"UPDATE jobs SET "+column+" = ? WHERE id = ?", value, jobID)
	if err != nil {
		if db.logger != nil {
			db.logger.Error(opName+" failed", "jobID", jobID, "err", err)
		}
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return true // the driver cannot say; assume the row existed
	}
	return n > 0
}

// UpdateResumePosition saves the playback position without bumping updated_at
// or triggering subscriber notifications. Designed for frequent periodic saves
// during video playback (every ~10 seconds). Returns true when a row was
// updated (false for an unknown jobID).
func (db *Database) UpdateResumePosition(jobID string, seconds float64) bool {
	return db.updateSingleColumnSilent(jobID, "resume_position", seconds, "UpdateResumePosition")
}

// UpdateChatOffset saves the chat timing offset without bumping updated_at
// or triggering subscriber notifications. Returns true when a row was
// updated (false for an unknown jobID).
func (db *Database) UpdateChatOffset(jobID string, offset float64) bool {
	return db.updateSingleColumnSilent(jobID, "chat_offset", offset, "UpdateChatOffset")
}

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
	var notifMsgs sql.NullString
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

// scanJob is a thin wrapper for single-row scans. Retained for readability at
// call sites that expect an *sql.Row.
func scanJob(row *sql.Row) (*Job, error) { return scanJobRow(row) }

// scanJobRows is a thin wrapper for multi-row iteration scans.
func scanJobRows(rows *sql.Rows) (*Job, error) { return scanJobRow(rows) }
