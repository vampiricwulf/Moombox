package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// dbLogger is the interface for database error logging.
type dbLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Database provides SQLite-backed persistence for Moombox.
type Database struct {
	db     *sql.DB
	ctx    context.Context // Optional context for query cancellation
	mu     sync.RWMutex
	closed bool
	logger dbLogger

	// Batch update coalescing
	updateCh   chan *Job
	batchDone  chan struct{}

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

// getCtx returns the stored context or context.Background().
func (db *Database) getCtx() context.Context {
	if db.ctx != nil {
		return db.ctx
	}
	return context.Background()
}

// Open creates or opens a SQLite database at the given path.
// The logger parameter is optional; if nil, database errors will be silently dropped.
func Open(dbPath string, logger ...dbLogger) (*Database, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool
	sqlDB.SetMaxOpenConns(1) // SQLite is single-writer
	sqlDB.SetMaxIdleConns(1)

	db := &Database{
		db:        sqlDB,
		updateCh:  make(chan *Job, 100),
		batchDone: make(chan struct{}),
		jobLogs:   make(map[string][]string),
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

	// Start batch update goroutine
	go db.batchUpdateLoop()

	return db, nil
}

func (db *Database) prepareStatements() error {
	var err error

	db.stmtGetJob, err = db.db.PrepareContext(db.getCtx(), `SELECT id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, video_width, video_height, video_fps, file_size,
		chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file,
		twitch_quality, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at
		FROM jobs WHERE id = ?`)
	if err != nil {
		return err
	}

	return nil
}

// batchUpdateLoop coalesces rapid job updates into batched writes.
// Signal-driven: sleeps until work arrives, then coalesces for 100ms before flushing.
// Zero IO when idle.
func (db *Database) batchUpdateLoop() {
	const coalesceDelay = 100 * time.Millisecond
	pending := make(map[string]*Job)
	var coalesceTimer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case job, ok := <-db.updateCh:
			if !ok {
				// Channel closed, flush remaining
				if coalesceTimer != nil {
					coalesceTimer.Stop()
				}
				db.flushUpdates(pending)
				close(db.batchDone)
				return
			}
			pending[job.ID] = job
			// Start coalesce timer on first item; subsequent items just accumulate
			if coalesceTimer == nil {
				coalesceTimer = time.NewTimer(coalesceDelay)
				timerC = coalesceTimer.C
			}

		case <-timerC:
			db.flushUpdates(pending)
			pending = make(map[string]*Job)
			coalesceTimer = nil
			timerC = nil
		}
	}
}

func (db *Database) flushUpdates(pending map[string]*Job) {
	if len(pending) == 0 {
		return
	}

	tx, err := db.db.BeginTx(db.getCtx(), nil)
	if err != nil {
		if db.logger != nil {
			db.logger.Error("database batch flush: failed to begin transaction", "err", err)
		}
		return
	}

	// Track which jobs were successfully persisted
	persisted := make(map[string]*Job, len(pending))
	for id, job := range pending {
		if err := updateJobInTx(db.getCtx(), tx, job); err != nil {
			if db.logger != nil {
				db.logger.Error("database batch flush: failed to update job", "jobID", id, "err", err)
			}
		} else {
			persisted[id] = job
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		if db.logger != nil {
			db.logger.Error("database batch flush: commit failed", "err", err, "jobs", len(pending))
		}
		return
	}

	// Snapshot subscribers, then notify outside lock to avoid blocking
	db.subMu.RLock()
	subs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, fn := range db.onJobUpdate {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	for _, job := range persisted {
		for _, fn := range subs {
			db.safeCallJobUpdate(fn, job)
		}
	}
}

// AddJob inserts a new job into the database.
func (db *Database) AddJob(job *Job) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	if job.CreatedAt == "" {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	result, err := db.db.ExecContext(db.getCtx(), `INSERT OR IGNORE INTO jobs (id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, chat_status, total_chat_messages, chat_filename, chat_file,
		thumbnail_file, description_file,
		twitch_quality, twitch_category, channel_avatar_url,
		selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		job.LastRecheckAt)
	if err != nil {
		return false, fmt.Errorf("failed to insert job: %w", err)
	}

	// INSERT OR IGNORE returns RowsAffected=0 when the row already exists
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return false, nil // Duplicate — job already exists
	}

	// Insert gaps
	for _, gap := range job.Gaps {
		_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO gaps (job_id, gap_from, gap_to, stream) VALUES (?, ?, ?, ?)`,
			job.ID, gap.From, gap.To, gap.Stream)
		if err != nil {
			return false, fmt.Errorf("failed to insert gap: %w", err)
		}
	}

	db.notifyJobsChange()
	return true, nil
}

// JobExists checks if a job with the given ID exists.
func (db *Database) JobExists(id string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var count int
	err := db.db.QueryRowContext(db.getCtx(), `SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}

// GetJob retrieves a job by ID.
func (db *Database) GetJob(id string) (*Job, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	job, err := scanJob(db.stmtGetJob.QueryRowContext(db.getCtx(), id))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	// Load gaps (non-fatal — gaps may simply not exist)
	if gaps, err := db.getGaps(id); err != nil {
		if db.logger != nil {
			db.logger.Warn("failed to load gaps for job", "jobID", id, "err", err)
		}
	} else {
		job.Gaps = gaps
	}
	// Load trims (non-fatal — trims may simply not exist)
	if trims, err := db.getTrims(id); err != nil {
		if db.logger != nil {
			db.logger.Warn("failed to load trims for job", "jobID", id, "err", err)
		}
	} else {
		job.Trims = trims
	}

	return job, nil
}

// GetAllJobs returns all jobs, optionally excluding archived ones.
func (db *Database) GetAllJobs(includeArchived bool) ([]*Job, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// includeArchived controls whether the caller wants all jobs (true) or just
	// non-archived ones (false). The actual age-based filtering of finished jobs
	// is handled by filterJobsByAge in the route layer — the database returns all
	// jobs and lets the caller decide what to show.
	query := `SELECT id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, video_width, video_height, video_fps, file_size,
		chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file,
		twitch_quality, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at
		FROM jobs ORDER BY updated_at DESC`

	rows, err := db.db.QueryContext(db.getCtx(), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJobRows(rows)
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	db.attachTrimsAndGaps(jobs)
	return jobs, nil
}

// UpdateJob queues a job update for batch writing.
func (db *Database) UpdateJob(job *Job) {
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	select {
	case db.updateCh <- job:
	default:
		// Channel full, do synchronous update
		db.mu.Lock()
		if err := updateJobDirect(db.getCtx(), db.db, job); err != nil && db.logger != nil {
			db.logger.Error("database sync update fallback failed", "jobID", job.ID, "err", err)
		}
		db.mu.Unlock()
	}
}

// UpdateJobSync synchronously updates a job in the database.
func (db *Database) UpdateJobSync(job *Job) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return updateJobDirect(db.getCtx(), db.db, job)
}

func updateJobDirect(ctx context.Context, db *sql.DB, job *Job) error {
	_, err := db.ExecContext(ctx, `UPDATE jobs SET
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
		last_recheck_at=?
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
		job.LastRecheckAt,
		job.ID)
	return err
}

func updateJobInTx(ctx context.Context, tx *sql.Tx, job *Job) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET
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
		last_recheck_at=?
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
		job.LastRecheckAt,
		job.ID)
	return err
}

// DeleteJob removes a job and its associated data.
func (db *Database) DeleteJob(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), "DELETE FROM jobs WHERE id = ?", id)
	if err != nil {
		return err
	}

	db.notifyJobsChange()
	return nil
}

// AddGap adds a gap record for a job.
func (db *Database) AddGap(jobID string, from, to int, stream string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO gaps (job_id, gap_from, gap_to, stream) VALUES (?, ?, ?, ?)`,
		jobID, from, to, stream)
	return err
}

func (db *Database) getGaps(jobID string) ([]Gap, error) {
	rows, err := db.db.QueryContext(db.getCtx(), "SELECT id, job_id, gap_from, gap_to, stream FROM gaps WHERE job_id = ?", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var gaps []Gap
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.ID, &g.JobID, &g.From, &g.To, &g.Stream); err != nil {
			continue
		}
		gaps = append(gaps, g)
	}
	if err := rows.Err(); err != nil {
		return gaps, err
	}
	return gaps, nil
}

// AddTrim adds a trim record for a job.
func (db *Database) AddTrim(trim *TrimRecord) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO trims (id, job_id, start_time, end_time, filename, created_at, duration, file_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		trim.ID, trim.JobID, trim.StartTime, trim.EndTime, trim.Filename,
		trim.CreatedAt, trim.Duration, trim.FileSize)
	return err
}

// DeleteTrim removes a trim record.
func (db *Database) DeleteTrim(trimID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), "DELETE FROM trims WHERE id = ?", trimID)
	return err
}

func (db *Database) getTrims(jobID string) ([]TrimRecord, error) {
	return db.getTrimsUnlocked(jobID)
}

// HasProcessed checks if a video ID has been previously processed.
func (db *Database) HasProcessed(videoID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var count int
	err := db.db.QueryRowContext(db.getCtx(), "SELECT COUNT(*) FROM history WHERE video_id = ?", videoID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// AddToHistory adds a video ID to the processing history.
func (db *Database) AddToHistory(videoID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.db.ExecContext(db.getCtx(), "INSERT OR IGNORE INTO history (video_id, added_at) VALUES (?, ?)", videoID, now)
	if err != nil {
		return err
	}

	// Prune if over limit
	return db.pruneHistory()
}

func (db *Database) pruneHistory() error {
	var count int
	if err := db.db.QueryRowContext(db.getCtx(), "SELECT COUNT(*) FROM history").Scan(&count); err != nil {
		return fmt.Errorf("failed to count history: %w", err)
	}
	if count <= 10000 {
		return nil
	}

	_, err := db.db.ExecContext(db.getCtx(), `DELETE FROM history WHERE video_id IN (
		SELECT video_id FROM history ORDER BY added_at ASC LIMIT ?
	)`, count-10000)
	return err
}

// GetLastVideo returns the last known video ID for a channel.
func (db *Database) GetLastVideo(channelID string) (string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var videoID string
	err := db.db.QueryRowContext(db.getCtx(), "SELECT video_id FROM last_videos WHERE channel_id = ?", channelID).Scan(&videoID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return videoID, err
}

// SetLastVideo updates the last known video ID for a channel.
func (db *Database) SetLastVideo(channelID, videoID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO last_videos (channel_id, video_id) VALUES (?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET video_id = excluded.video_id`,
		channelID, videoID)
	return err
}

// HasActiveJob checks if there's an active (non-terminal) job for the given video ID.
func (db *Database) HasActiveJob(videoID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var count int
	err := db.db.QueryRowContext(db.getCtx(), `SELECT COUNT(*) FROM jobs WHERE video_id = ? AND status NOT IN (?, ?, ?)`,
		videoID, StatusFinished, StatusError, StatusCancelled).Scan(&count)
	return count > 0, err
}

// ImportFromJSON imports data from a TypeScript-version moombox.json file.
func (db *Database) ImportFromJSON(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read JSON: %w", err)
	}

	var jsonDB struct {
		Jobs       []Job             `json:"jobs"`
		History    []string          `json:"history"`
		LastVideos map[string]string `json:"lastVideos"`
	}

	if err := json.Unmarshal(data, &jsonDB); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	tx, err := db.db.BeginTx(db.getCtx(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Import jobs
	for _, job := range jsonDB.Jobs {
		if job.Platform == "" {
			job.Platform = "youtube"
		}
		// Use AddJob logic but in transaction
		_, err := tx.ExecContext(db.getCtx(), `INSERT OR IGNORE INTO jobs (id, video_id, url, title, channel_name, platform,
			status, progress, percent, eta, speed, error, created_at, updated_at,
			is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
			length_seconds, download_started_at, thumbnail_url, description, output_file,
			filename, output_directory, chat_status, total_chat_messages, chat_filename, chat_file,
			thumbnail_file, description_file,
			twitch_quality, twitch_category, channel_avatar_url,
			selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			job.ID, job.VideoID, job.URL, job.Title, job.ChannelName, job.Platform,
			job.Status, job.Progress, job.Percent, job.ETA, job.Speed, job.Error,
			job.CreatedAt, job.UpdatedAt,
			boolToInt(job.IsVod), boolToInt(job.ManuallyAdded), boolToInt(job.AllowNonStream),
			job.StreamStartTime, job.StreamEndTime, job.LengthSeconds, job.DownloadStartedAt,
			job.ThumbnailURL, job.Description, job.OutputFile, job.Filename, job.OutputDirectory,
			job.ChatStatus, job.TotalChatMessages, job.ChatFilename, job.ChatFile,
			job.ThumbnailFile, job.DescriptionFile,
			job.TwitchQuality, job.TwitchCategory, job.ChannelAvatarURL,
			job.SelectedVideoItag, job.SelectedAudioItag, job.StartTime, job.EndTime,
			job.LastRecheckAt)
		if err != nil {
			if db.logger != nil {
				db.logger.Warn("import: failed to insert job", "jobID", job.ID, "err", err)
			}
			continue
		}

		for _, gap := range job.Gaps {
			if _, err := tx.ExecContext(db.getCtx(), "INSERT INTO gaps (job_id, gap_from, gap_to, stream) VALUES (?, ?, ?, ?)",
				job.ID, gap.From, gap.To, gap.Stream); err != nil && db.logger != nil {
				db.logger.Warn("import: failed to insert gap", "jobID", job.ID, "err", err)
			}
		}
	}

	// Import history
	now := time.Now().UTC().Format(time.RFC3339)
	for _, videoID := range jsonDB.History {
		if _, err := tx.ExecContext(db.getCtx(), "INSERT OR IGNORE INTO history (video_id, added_at) VALUES (?, ?)", videoID, now); err != nil && db.logger != nil {
			db.logger.Warn("import: failed to insert history", "videoID", videoID, "err", err)
		}
	}

	// Import last videos
	for channelID, videoID := range jsonDB.LastVideos {
		if _, err := tx.ExecContext(db.getCtx(), `INSERT INTO last_videos (channel_id, video_id) VALUES (?, ?)
			ON CONFLICT(channel_id) DO UPDATE SET video_id = excluded.video_id`,
			channelID, videoID); err != nil && db.logger != nil {
			db.logger.Warn("import: failed to insert last_video", "channelID", channelID, "err", err)
		}
	}

	return tx.Commit()
}

// OnJobUpdate registers a callback for job update events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobUpdate(fn func(*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := len(db.onJobUpdate)
	db.onJobUpdate = append(db.onJobUpdate, fn)
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		if id < len(db.onJobUpdate) {
			// Nil out the entry to avoid slice realloc
			db.onJobUpdate[id] = nil
		}
	}
}

// OnJobsChange registers a callback for job add/delete events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobsChange(fn func([]*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := len(db.onJobsChange)
	db.onJobsChange = append(db.onJobsChange, fn)
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		if id < len(db.onJobsChange) {
			db.onJobsChange[id] = nil
		}
	}
}

// notifyJobsChange must be called while db.mu is already held (Lock or RLock).
// It uses getAllJobsUnlocked to avoid deadlock.
func (db *Database) notifyJobsChange() {
	db.subMu.RLock()
	subs := make([]func([]*Job), 0, len(db.onJobsChange))
	for _, fn := range db.onJobsChange {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	// Use unlocked version since caller already holds db.mu
	jobs, _ := db.getAllJobsUnlocked()
	for _, fn := range subs {
		db.safeCallJobsChange(fn, jobs)
	}
}

// getAllJobsUnlocked queries all jobs without acquiring db.mu.
// Caller must already hold db.mu.
func (db *Database) getAllJobsUnlocked() ([]*Job, error) {
	query := `SELECT id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, video_width, video_height, video_fps, file_size,
		chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file,
		twitch_quality, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at
		FROM jobs ORDER BY updated_at DESC`

	rows, err := db.db.QueryContext(db.getCtx(), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJobRows(rows)
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	db.attachTrimsAndGaps(jobs)
	return jobs, nil
}

// UpdateJobFields performs a partial update of a job using a map of field names to values.
// This is useful when only a few fields need to change without loading the full job.
func (db *Database) UpdateJobFields(id string, fields map[string]any) {
	if len(fields) == 0 {
		return
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	var setClauses []string
	var args []any

	fieldToColumn := map[string]string{
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
		"video_width":         "video_width",
		"video_height":        "video_height",
		"video_fps":           "video_fps",
		"file_size":           "file_size",
		"last_recheck_at":     "last_recheck_at",
		"twitch_quality":      "twitch_quality",
		"twitch_category":     "twitch_category",
		"channel_avatar_url":  "channel_avatar_url",
	}

	for key, val := range fields {
		col, ok := fieldToColumn[key]
		if !ok {
			continue
		}
		setClauses = append(setClauses, col+"=?")
		args = append(args, val)
	}

	if len(setClauses) == 0 {
		return
	}

	// Always update updated_at
	setClauses = append(setClauses, "updated_at=?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)

	query := "UPDATE jobs SET " + strings.Join(setClauses, ", ") + " WHERE id=?"
	_, err := db.db.ExecContext(db.getCtx(), query, args...)
	if err != nil {
		if db.logger != nil {
			db.logger.Error("UpdateJobFields failed", "jobID", id, "err", err)
		}
		return
	}

	// Notify subscribers with the full job object (TUI + WebSocket need all fields).
	// A full SELECT is required here because UpdateJobFields only writes a subset.
	row := db.db.QueryRowContext(db.getCtx(), `SELECT id, video_id, url, title, channel_name, platform,
		status, progress, percent, eta, speed, error, created_at, updated_at,
		last_video_seq, last_audio_seq, total_video_seq, total_audio_seq,
		is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time,
		length_seconds, download_started_at, thumbnail_url, description, output_file,
		filename, output_directory, video_width, video_height, video_fps, file_size,
		chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file,
		twitch_quality, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at
		FROM jobs WHERE id = ?`, id)
	job, scanErr := scanJob(row)
	if scanErr != nil {
		if db.logger != nil {
			db.logger.Error("UpdateJobFields: failed to read back job", "jobID", id, "err", scanErr)
		}
		return
	}

	db.subMu.RLock()
	subs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, fn := range db.onJobUpdate {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	for _, fn := range subs {
		db.safeCallJobUpdate(fn, job)
	}
}

// attachTrimsAndGaps batch-loads all trims and gaps and attaches them to jobs.
// Caller must already hold db.mu (read or write).
func (db *Database) attachTrimsAndGaps(jobs []*Job) {
	if len(jobs) == 0 {
		return
	}

	// Batch-load all trims in one query.
	trimRows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, job_id, start_time, end_time, filename, created_at, duration, file_size FROM trims`)
	if err == nil {
		trimMap := make(map[string][]TrimRecord)
		for trimRows.Next() {
			var tr TrimRecord
			if err := trimRows.Scan(&tr.ID, &tr.JobID, &tr.StartTime, &tr.EndTime,
				&tr.Filename, &tr.CreatedAt, &tr.Duration, &tr.FileSize); err == nil {
				trimMap[tr.JobID] = append(trimMap[tr.JobID], tr)
			}
		}
		trimRows.Close()
		for _, job := range jobs {
			if trims, ok := trimMap[job.ID]; ok {
				job.Trims = trims
			}
		}
	}

	// Batch-load all gaps in one query.
	gapRows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, job_id, gap_from, gap_to, stream FROM gaps`)
	if err == nil {
		gapMap := make(map[string][]Gap)
		for gapRows.Next() {
			var g Gap
			if err := gapRows.Scan(&g.ID, &g.JobID, &g.From, &g.To, &g.Stream); err == nil {
				gapMap[g.JobID] = append(gapMap[g.JobID], g)
			}
		}
		gapRows.Close()
		for _, job := range jobs {
			if gaps, ok := gapMap[job.ID]; ok {
				job.Gaps = gaps
			}
		}
	}
}

// GetTrimsForJob returns all trim records for a given job.
func (db *Database) GetTrimsForJob(jobID string) ([]TrimRecord, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.getTrimsUnlocked(jobID)
}

func (db *Database) getTrimsUnlocked(jobID string) ([]TrimRecord, error) {
	rows, err := db.db.QueryContext(db.getCtx(), `SELECT id, job_id, start_time, end_time, filename, created_at, duration, file_size
		FROM trims WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trims []TrimRecord
	for rows.Next() {
		var tr TrimRecord
		if err := rows.Scan(&tr.ID, &tr.JobID, &tr.StartTime, &tr.EndTime,
			&tr.Filename, &tr.CreatedAt, &tr.Duration, &tr.FileSize); err != nil {
			continue
		}
		trims = append(trims, tr)
	}
	if err := rows.Err(); err != nil {
		return trims, err
	}
	return trims, nil
}

// GetJobStats returns aggregate statistics across all jobs.
func (db *Database) GetJobStats() (*JobStats, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var s JobStats
	err := db.db.QueryRowContext(db.getCtx(), `SELECT
		COALESCE(SUM(CASE WHEN status = 'Finished' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status IN ('Downloading', 'Live') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Muxing' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Error' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Cancelled' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform IN ('youtube', '') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform = 'twitch' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Finished' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Error' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Cancelled' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform IN ('youtube', '') THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform = 'twitch' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Finished' THEN length_seconds ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'Finished' THEN total_chat_messages ELSE 0 END), 0)
		FROM jobs`).Scan(
		&s.FinishedCount, &s.ActiveCount, &s.MuxingCount,
		&s.ErrorCount, &s.CancelledCount,
		&s.YouTubeCount, &s.TwitchCount,
		&s.FinishedSize, &s.ErrorSize, &s.CancelledSize,
		&s.YouTubeSize, &s.TwitchSize,
		&s.TotalDuration, &s.TotalChatMessages,
	)
	if err != nil {
		return nil, fmt.Errorf("GetJobStats: %w", err)
	}
	return &s, nil
}

// AddJobLog adds a log line to the per-job in-memory buffer.
func (db *Database) AddJobLog(jobID, line string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	logs := db.jobLogs[jobID]
	logs = append(logs, line)
	// Match TS: cap at 200 lines, trim to last 100
	if len(logs) > 200 {
		logs = logs[len(logs)-100:]
	}
	db.jobLogs[jobID] = logs
}

// GetJobLogs returns the in-memory log lines for a job.
func (db *Database) GetJobLogs(jobID string) []string {
	db.jobLogsMu.RLock()
	defer db.jobLogsMu.RUnlock()
	return db.jobLogs[jobID]
}

// ClearJobLogs removes the per-job log buffer.
func (db *Database) ClearJobLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	delete(db.jobLogs, jobID)
}

// RouteLogToJobs checks if a log line contains any known job ID and routes
// it to the corresponding per-job log buffer. Matches the TypeScript server's
// knownJobIds-based log routing behavior. Each log line belongs to at most one job.
func (db *Database) RouteLogToJobs(line string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()

	for jobID := range db.jobLogs {
		if strings.Contains(line, jobID) {
			logs := db.jobLogs[jobID]
			logs = append(logs, line)
			if len(logs) > 200 {
				logs = logs[len(logs)-100:]
			}
			db.jobLogs[jobID] = logs
			return // Each log line belongs to at most one job
		}
	}
}

// TrackJobForLogs ensures a job ID is tracked for log routing.
// Called when jobs are created or loaded, matching TS knownJobIds behavior.
func (db *Database) TrackJobForLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	if _, ok := db.jobLogs[jobID]; !ok {
		db.jobLogs[jobID] = nil
	}
}

// PruneJobLogs removes log entries for job IDs not in the provided set.
// Called on jobsChange to keep the log map in sync with the database.
func (db *Database) PruneJobLogs(activeIDs map[string]struct{}) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	for id := range db.jobLogs {
		if _, ok := activeIDs[id]; !ok {
			delete(db.jobLogs, id)
		}
	}
}

// Close flushes pending updates and closes the database.
func (db *Database) Close() error {
	if db.closed {
		return nil
	}
	db.closed = true

	// Close update channel and wait for batch loop to finish
	close(db.updateCh)
	<-db.batchDone

	if db.stmtGetJob != nil {
		db.stmtGetJob.Close()
	}

	return db.db.Close()
}

// safeCallJobUpdate calls a subscriber callback, recovering from panics so
// one misbehaving subscriber can't prevent others from being notified.
func (db *Database) safeCallJobUpdate(fn func(*Job), job *Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on job update", "jobID", job.ID, "panic", r)
		}
	}()
	fn(job)
}

// safeCallJobsChange calls a jobs-change subscriber callback with panic recovery.
func (db *Database) safeCallJobsChange(fn func([]*Job), jobs []*Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on jobs change", "panic", r)
		}
	}()
	fn(jobs)
}

// Helper functions

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream int
	err := row.Scan(
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
		&j.LastRecheckAt,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = isVod != 0
	j.ManuallyAdded = manuallyAdded != 0
	j.AllowNonStream = allowNonStream != 0
	return &j, nil
}

func scanJobRows(rows *sql.Rows) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream int
	err := rows.Scan(
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
		&j.LastRecheckAt,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = isVod != 0
	j.ManuallyAdded = manuallyAdded != 0
	j.AllowNonStream = allowNonStream != 0
	return &j, nil
}
