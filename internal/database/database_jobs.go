package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

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
		selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at,
		quality_preference)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?)`,
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
		job.QualityPreference)
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
	if trims, err := db.getTrimsUnlocked(id); err != nil {
		if db.logger != nil {
			db.logger.Warn("failed to load trims for job", "jobID", id, "err", err)
		}
	} else {
		job.Trims = trims
	}
	// Load segments (non-fatal — segments may simply not exist)
	if segments, err := db.getSegments(id); err != nil {
		if db.logger != nil {
			db.logger.Warn("failed to load segments for job", "jobID", id, "err", err)
		}
	} else {
		job.Segments = segments
	}

	return job, nil
}

// GetAllJobs returns all jobs from the database. Age-based filtering of
// finished jobs is handled by filterJobsByAge in the route/presentation layer.
func (db *Database) GetAllJobs() ([]*Job, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	return db.getAllJobsUnlocked()
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
		last_recheck_at, quality_preference, watched, resume_position, chat_offset
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
			if db.logger != nil {
				db.logger.Debug("getAllJobsUnlocked: scan error", "err", err)
			}
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
		err := updateJobExec(db.getCtx(), db.db, job)
		db.mu.Unlock()

		if err != nil {
			if db.logger != nil {
				db.logger.Error("database sync update fallback failed", "jobID", job.ID, "err", err)
			}
		} else {
			// Notify subscribers (batch path does this in flushUpdates)
			db.notifyJobUpdate(job)
		}
	}
}

// UpdateJobSync synchronously updates a job in the database.
func (db *Database) UpdateJobSync(job *Job) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return updateJobExec(db.getCtx(), db.db, job)
}

// BatchSetWatched marks multiple jobs as watched or unwatched and clears
// their resume_position. Only affects Finished jobs. Triggers OnJobsChange
// for a full list refresh.
func (db *Database) BatchSetWatched(jobIDs []string, watched bool) error {
	if len(jobIDs) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	placeholders := make([]string, len(jobIDs))
	now := time.Now().UTC().Format(time.RFC3339)
	args := make([]any, 0, len(jobIDs)+3)
	args = append(args, boolToInt(watched), now)
	for i, id := range jobIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, string(StatusFinished))

	query := fmt.Sprintf(
		"UPDATE jobs SET watched = ?, resume_position = NULL, updated_at = ? WHERE id IN (%s) AND status = ?",
		strings.Join(placeholders, ","),
	)
	_, err := db.db.ExecContext(db.getCtx(), query, args...)
	if err != nil {
		if db.logger != nil {
			db.logger.Error("BatchSetWatched failed", "err", err)
		}
		return err
	}

	db.notifyJobsChange()
	return nil
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

// HasActiveJob checks if there's an active (non-terminal) job for the given video ID.
func (db *Database) HasActiveJob(videoID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var one int
	err := db.db.QueryRowContext(db.getCtx(), `SELECT 1 FROM jobs WHERE video_id = ? AND status NOT IN (?, ?, ?) LIMIT 1`,
		videoID, StatusFinished, StatusError, StatusCancelled).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
			if db.logger != nil {
				db.logger.Debug("getGaps: scan error", "jobID", jobID, "err", err)
			}
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
	if err == nil {
		db.notifyJobsChange()
	}
	return err
}

// DeleteTrim removes a trim record.
func (db *Database) DeleteTrim(trimID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), "DELETE FROM trims WHERE id = ?", trimID)
	if err == nil {
		db.notifyJobsChange()
	}
	return err
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
			if db.logger != nil {
				db.logger.Debug("getTrimsUnlocked: scan error", "jobID", jobID, "err", err)
			}
			continue
		}
		trims = append(trims, tr)
	}
	if err := rows.Err(); err != nil {
		return trims, err
	}
	return trims, nil
}

// AddSegment adds a segment record for a multi-segment quality-split job.
func (db *Database) AddSegment(seg *Segment) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	result, err := db.db.ExecContext(db.getCtx(), `INSERT INTO segments (job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seg.JobID, seg.SegmentIndex, seg.UnixStart, seg.UnixEnd, seg.Quality, seg.Filename,
		seg.FilePath, seg.FileSize, seg.VideoWidth, seg.VideoHeight, seg.VideoFps, seg.DurationSeconds)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	seg.ID = int(id)
	return nil
}

// GetSegments returns all segments for a given job, ordered by segment_index.
func (db *Database) GetSegments(jobID string) ([]Segment, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.getSegments(jobID)
}

func (db *Database) getSegments(jobID string) ([]Segment, error) {
	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds
		FROM segments WHERE job_id = ? ORDER BY segment_index`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var segments []Segment
	for rows.Next() {
		var s Segment
		if err := rows.Scan(&s.ID, &s.JobID, &s.SegmentIndex, &s.UnixStart, &s.UnixEnd,
			&s.Quality, &s.Filename, &s.FilePath, &s.FileSize,
			&s.VideoWidth, &s.VideoHeight, &s.VideoFps, &s.DurationSeconds); err != nil {
			if db.logger != nil {
				db.logger.Debug("getSegments: scan error", "jobID", jobID, "err", err)
			}
			continue
		}
		segments = append(segments, s)
	}
	if err := rows.Err(); err != nil {
		return segments, err
	}
	return segments, nil
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
	if err != nil {
		if db.logger != nil {
			db.logger.Warn("attachTrimsAndGaps: failed to query trims", "err", err)
		}
	} else {
		trimMap := make(map[string][]TrimRecord, len(jobs))
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
	if err != nil {
		if db.logger != nil {
			db.logger.Warn("attachTrimsAndGaps: failed to query gaps", "err", err)
		}
	} else {
		gapMap := make(map[string][]Gap, len(jobs))
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

	// Batch-load all segments in one query.
	segRows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds
		FROM segments ORDER BY segment_index`)
	if err != nil {
		if db.logger != nil {
			db.logger.Warn("attachTrimsAndGaps: failed to query segments", "err", err)
		}
	} else {
		segMap := make(map[string][]Segment, len(jobs))
		for segRows.Next() {
			var s Segment
			if err := segRows.Scan(&s.ID, &s.JobID, &s.SegmentIndex, &s.UnixStart, &s.UnixEnd,
				&s.Quality, &s.Filename, &s.FilePath, &s.FileSize,
				&s.VideoWidth, &s.VideoHeight, &s.VideoFps, &s.DurationSeconds); err == nil {
				segMap[s.JobID] = append(segMap[s.JobID], s)
			}
		}
		segRows.Close()
		for _, job := range jobs {
			if segs, ok := segMap[job.ID]; ok {
				job.Segments = segs
			}
		}
	}
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
			selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at,
			quality_preference)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?)`,
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
			job.LastRecheckAt,
			job.QualityPreference)
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

// --- Job logs ---

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

// GetJobLogs returns a copy of the in-memory log lines for a job.
func (db *Database) GetJobLogs(jobID string) []string {
	db.jobLogsMu.RLock()
	defer db.jobLogsMu.RUnlock()
	src := db.jobLogs[jobID]
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
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
