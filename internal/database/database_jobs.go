package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AddJob inserts a new job into the database.
//
// The row and its gap rows go in one transaction: a gap insert that failed
// used to leave the job row behind while AddJob reported an error and fired no
// JobAdded — a job every list showed only after a restart, that its creator
// believed was never made.
//
// JobAdded carries the row as STORED, read back inside the same lock: the
// INSERT names a fixed column list, so a field it does not cover (watched,
// incomplete_tail, park_reason, auto_retry_count — written later through
// UpdateJobFields) takes the schema default no matter what the caller's struct
// held, and subscribers must see what a GetJob would return, not the struct.
func (db *Database) AddJob(job *Job) (bool, error) {
	db.mu.Lock()

	now := time.Now().UTC().Format(time.RFC3339)
	if job.CreatedAt == "" {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		db.mu.Unlock()
		return false, fmt.Errorf("failed to begin job insert: %w", err)
	}
	defer tx.Rollback() // a no-op once committed

	result, err := insertJobExec(ctx, tx, job)
	if err != nil {
		db.mu.Unlock()
		return false, fmt.Errorf("failed to insert job: %w", err)
	}

	// INSERT OR IGNORE returns RowsAffected=0 when the row already exists
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		db.mu.Unlock()
		return false, nil // Duplicate — job already exists
	}

	// Insert gaps
	for _, gap := range job.Gaps {
		_, err := tx.ExecContext(ctx, `INSERT INTO gaps (job_id, gap_from, gap_to, stream) VALUES (?, ?, ?, ?)`,
			job.ID, gap.From, gap.To, gap.Stream)
		if err != nil {
			db.mu.Unlock()
			return false, fmt.Errorf("failed to insert gap: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		db.mu.Unlock()
		return false, fmt.Errorf("failed to commit job insert: %w", err)
	}

	added := job
	if stored, err := scanJob(db.stmtGetJob.QueryRowContext(ctx, job.ID)); err == nil {
		if gaps, gErr := db.getGaps(job.ID); gErr == nil {
			stored.Gaps = gaps
		}
		db.jobWriteVersion++
		stored.Version = db.jobWriteVersion
		added = stored
	}

	db.mu.Unlock()
	// AddJob fires ONLY OnJobAdded — the legacy OnJobsChange dispatch
	// was dropped now that the WS broadcaster + TUI both consume the
	// targeted lifecycle event (DECISIONS #21 consumer migration). Only
	// the bulk writers (BatchSetWatched, DeleteJobsAndHistoryForChannel)
	// still fire OnJobsChange.
	db.notifyJobAdded(added)
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
		twitch_quality, twitch_quality_preference, twitch_category,
		channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time,
		last_recheck_at, quality_preference, watched, resume_position, chat_offset,
		auto_retry_count, channel_id, queue_priority, incomplete_tail, park_reason, park_identity,
		notification_msgs
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
				db.logger.Warn("getAllJobsUnlocked: scan error", "err", err)
			}
			continue
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := db.attachTrimsAndGaps(jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// BatchSetWatched marks multiple jobs as watched or unwatched and clears
// their resume_position. Only affects Finished jobs. Triggers OnJobsChange
// for a full list refresh.
//
// jobIDs is chunked to stay under SQLITE_MAX_VARIABLE_NUMBER and all chunks
// run inside a single outer transaction so the update is atomic.
//
// BatchSetWatched is one of the two bulk writers still firing OnJobsChange
// (a full jobs+trims+gaps re-scan) rather than per-event notifications; the
// other is DeleteJobsAndHistoryForChannel, for the same reason. Migration
// rationale: a batch can flip 100+ jobs at once and per-event dispatch
// would amplify into 100+ subscriber callbacks, each running their own
// re-render. The single full re-scan is cheaper for the consumer side
// even though it's heavier in the DB. AddJob/DeleteJob/AddTrim/DeleteTrim
// all migrated to per-event notifications (OnJobAdded / OnJobDeleted /
// OnTrimsChanged) which don't have this fan-out concern.
func (db *Database) BatchSetWatched(jobIDs []string, watched bool) error {
	if len(jobIDs) == 0 {
		return nil
	}

	db.mu.Lock()

	tx, err := db.db.BeginTx(db.getCtx(), nil)
	if err != nil {
		db.mu.Unlock()
		if db.logger != nil {
			db.logger.Error("BatchSetWatched: BeginTx failed", "err", err)
		}
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for start := 0; start < len(jobIDs); start += idChunkSize {
		end := min(start+idChunkSize, len(jobIDs))
		chunk := jobIDs[start:end]

		placeholders := make([]string, len(chunk))
		// args: watched, updated_at, ...ids, status  → len(chunk) + 3 entries
		args := make([]any, 0, len(chunk)+3)
		args = append(args, boolToInt(watched), now)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, string(StatusFinished))

		query := fmt.Sprintf(
			"UPDATE jobs SET watched = ?, resume_position = NULL, updated_at = ? WHERE id IN (%s) AND status = ?",
			strings.Join(placeholders, ","),
		)
		if _, err := tx.ExecContext(db.getCtx(), query, args...); err != nil {
			tx.Rollback()
			db.mu.Unlock()
			if db.logger != nil {
				db.logger.Error("BatchSetWatched failed", "err", err)
			}
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		db.mu.Unlock()
		if db.logger != nil {
			db.logger.Error("BatchSetWatched: commit failed", "err", err)
		}
		return err
	}

	jobs := db.snapshotJobsChange()
	db.mu.Unlock()
	db.dispatchJobsChange(jobs)
	return nil
}

// DeleteJob removes a job and its associated data.
func (db *Database) DeleteJob(id string) error {
	db.mu.Lock()

	result, err := db.db.ExecContext(db.getCtx(), "DELETE FROM jobs WHERE id = ?", id)
	if err != nil {
		db.mu.Unlock()
		return err
	}
	rowsAffected, _ := result.RowsAffected()

	db.mu.Unlock()
	// Fires ONLY OnJobDeleted — the legacy OnJobsChange dispatch was
	// dropped now that the WS broadcaster + TUI consume the targeted
	// lifecycle event (DECISIONS #21 consumer migration). The DELETE
	// on a missing ID stays silent (rowsAffected == 0): no event, no
	// broadcast, no work.
	if rowsAffected > 0 {
		db.notifyJobDeleted(id)
	}
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

// ManualTwitchJobs returns the non-terminal manually added Twitch jobs for
// login's channel — the `tw_manual_<login>_<ns>` rows the Web add creates when
// the channel is offline — with ID, Status and StreamStartTime set. Such a row
// carries no stream ID, so the Twitch monitor's HasActiveJob(streamID) dedupe
// never matches it; the monitor decides from these whether one has claimed a
// broadcast (manualJobClaims). The login is everything between the prefix and
// the last underscore (the add's UnixNano suffix has none), matched in Go
// because a login's own underscores are LIKE wildcards.
func (db *Database) ManualTwitchJobs(login string) ([]*Job, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, status, stream_start_time FROM jobs WHERE id LIKE 'tw\_manual\_%' ESCAPE '\' AND status NOT IN (?, ?, ?)`,
		StatusFinished, StatusError, StatusCancelled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var id, status string
		var start sql.NullString
		if err := rows.Scan(&id, &status, &start); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(id, "tw_manual_")
		if i := strings.LastIndex(rest, "_"); i > 0 && strings.EqualFold(rest[:i], login) {
			out = append(out, &Job{ID: id, VideoID: id, Status: JobStatus(status), StreamStartTime: start.String})
		}
	}
	return out, rows.Err()
}

// TwitchEndUnconfirmedJobs returns the Twitch jobs sitting in Error with
// ParkReasonTwitchEndUnconfirmed — a live capture that failed while its
// broadcast's end was unconfirmed, staging kept — with the fields the Twitch
// monitor needs to tell whether that broadcast is over: ID, VideoID, URL,
// ChannelName, StreamStartTime and ManuallyAdded.
func (db *Database) TwitchEndUnconfirmedJobs() ([]*Job, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, video_id, url, channel_name, stream_start_time, manually_added FROM jobs
		 WHERE platform = 'twitch' AND status = ? AND park_reason = ?`,
		StatusError, ParkReasonTwitchEndUnconfirmed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var id string
		var videoID, url, channelName, start sql.NullString
		var manual sql.NullInt64
		if err := rows.Scan(&id, &videoID, &url, &channelName, &start, &manual); err != nil {
			return nil, err
		}
		out = append(out, &Job{ID: id, VideoID: videoID.String, URL: url.String, ChannelName: channelName.String,
			StreamStartTime: start.String, ManuallyAdded: manual.Int64 != 0, Platform: "twitch",
			Status: StatusError, ParkReason: ParkReasonTwitchEndUnconfirmed})
	}
	return out, rows.Err()
}

// BackfillTwitchQualityPreference records a twitch_quality_preference on every
// Twitch row that has none — the rows that predate schema v21 — and reports
// how many it wrote. prefFor decides each row's value from ID, VideoID, URL,
// ChannelName and QualityPreference (the rule is the caller's, because it
// needs the configured channels; see migrateV21); an empty answer is stored
// as "best".
//
// Silent like the other single-column maintenance writes: no updated_at bump
// and no subscriber, because it runs at startup before anything subscribes
// and a backfill is not an event in any job's life. Collect-then-update, as
// every backfill must be on a one-connection pool, and each UPDATE re-checks
// the column is still empty so it can never overwrite a value written since.
func (db *Database) BackfillTwitchQualityPreference(prefFor func(*Job) string) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	ctx := db.getCtx()

	rows, err := db.db.QueryContext(ctx,
		`SELECT id, video_id, url, channel_name, quality_preference FROM jobs
		 WHERE platform = 'twitch' AND twitch_quality_preference = ''`)
	if err != nil {
		return 0, err
	}
	var pending []*Job
	for rows.Next() {
		var id string
		var videoID, url, channelName, pref sql.NullString
		if err := rows.Scan(&id, &videoID, &url, &channelName, &pref); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, &Job{ID: id, VideoID: videoID.String, URL: url.String,
			ChannelName: channelName.String, Platform: "twitch", QualityPreference: pref.String})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	written := 0
	for _, j := range pending {
		pref := prefFor(j)
		if pref == "" {
			pref = "best"
		}
		res, err := db.db.ExecContext(ctx,
			`UPDATE jobs SET twitch_quality_preference = ? WHERE id = ? AND twitch_quality_preference = ''`,
			pref, j.ID)
		if err != nil {
			if db.logger != nil {
				db.logger.Warn("BackfillTwitchQualityPreference: update failed", "jobID", j.ID, "err", err)
			}
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	return written, nil
}

// QueuedChannels returns the distinct channel IDs that currently have Queued
// (un-admitted backlog) jobs. NULL channel_id rows are excluded: Twitch and
// manual adds have no channel affiliation and are never scheduler-paced, and
// a defensive NULL-channel Queued row must not wedge the admission sweep.
func (db *Database) QueuedChannels() ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT DISTINCT channel_id FROM jobs WHERE status = ? AND channel_id IS NOT NULL`,
		StatusQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

// CountBacklogInFlight returns the channel's admitted-backlog count — the M
// count of spec §10.
//
// M count: ALLOW-list. NOT IN would leak COOKIES? — neither Queued nor terminal —
// and a channel whose cookies lapse would silently lose its throughput forever.
func (db *Database) CountBacklogInFlight(channelID string) (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var n int
	// Bound from the JobStatus constants, as HasActiveJob does, so a status
	// rename cannot silently zero the scheduler's in-flight count.
	err := db.db.QueryRowContext(db.getCtx(), `SELECT COUNT(*) FROM jobs
 WHERE channel_id = ? AND queue_priority = 1
   AND status IN (?, ?, ?, ?);`, channelID,
		StatusUpcoming, StatusLive, StatusDownloading, StatusMuxing).Scan(&n)
	return n, err
}

// NextQueuedJobs returns up to limit Queued job IDs for the channel, in the
// order the scheduler admits them.
//
// Admission order: published DESC — no priority term (only backlog is ever Queued).
// INNER JOIN is guaranteed to hit: only the archival pass creates Queued rows.
// A cookie repair also returns priority-1 rows to Queued, and those were created
// by the archival pass too — the cookie sweep checks for the partner
// (GetFeedItem) before choosing Queued, so no Queued row lacks one. It has to:
// the prune deletes {Queued, Upcoming, COOKIES?} jobs before it deletes
// feed_items (backfill.go) but leaves a RUNNING download alone, and that is the
// row that parks in COOKIES? afterwards with no partner left.
func (db *Database) NextQueuedJobs(channelID string, limit int) ([]string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(), `SELECT j.id FROM jobs j
  JOIN feed_items f ON f.channel_id = j.channel_id AND f.video_id = j.video_id
 WHERE j.channel_id = ? AND j.status = 'Queued'
 ORDER BY f.published DESC
 LIMIT ?;`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteJobsAndHistoryForChannel deletes the channel's jobs in the given
// statuses AND their processing-history rows, returning the number of jobs
// deleted. Plan 5's backfill prune calls it with {Queued, Upcoming, COOKIES?}
// (spec §11): pre-download states with nothing on disk. AddToHistory fires at
// job CREATION, so deleting the job while its history row survives
// manufactures an orphan — HasProcessed keeps answering true and the re-added
// channel can never re-archive the video (the exact class that re-armed
// gr-ZTohjwnQ).
//
// Statement order is load-bearing: the history delete's subquery reads the
// jobs table, so it must run BEFORE the jobs delete. Both run in one
// transaction so a crash between them can't strand a half-pruned channel.
// The subquery keys on jobs.id: history rows are keyed by JOB ID, and the
// package rule (see ListOrphanedHistory) is that history-vs-jobs matching
// MUST be against jobs.id, never jobs.video_id. The plan text originally
// wrote the subquery over video_id — equivalent for every job this helper
// can currently match (channel-affiliated jobs are YouTube-only, where
// ID == VideoID), but jobs.id stays correct even if a channel-affiliated
// job class with a prefixed ID ever appears.
//
// jobs.channel_id is nullable; SQL `channel_id = ?` never matches NULL, so
// Twitch/manual jobs (no channel affiliation) are untouchable here.
//
// Dispatch: one OnJobsChange full-list refresh after commit — the
// BatchSetWatched bulk pattern, and for the same reason: a deep prune can
// drop 100+ jobs at once, and per-job OnJobDeleted would amplify into N
// full jobs fetches in the WS subscriber and can overflow the TUI's bounded
// drop-on-full channel. A prune that deleted nothing dispatches nothing
// (DeleteJob's rowsAffected guard).
func (db *Database) DeleteJobsAndHistoryForChannel(channelID string, statuses []JobStatus) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}

	deleted, jobs, err := db.deleteJobsAndHistoryForChannelTx(channelID, statuses)
	if err != nil {
		return 0, err
	}
	db.dispatchJobsChange(jobs) // no-op on nil (nothing deleted, or no subscribers)
	return deleted, nil
}

// deleteJobsAndHistoryForChannelTx runs the two-statement prune transaction
// under db.mu and, when rows were deleted, snapshots the post-delete jobs
// list for the caller's OnJobsChange dispatch (the snapshot must be taken
// while the lock is still held).
func (db *Database) deleteJobsAndHistoryForChannelTx(channelID string, statuses []JobStatus) (deleted int, snapshot jobsSnapshot, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	match := "channel_id = ? AND status IN (" + placeholders + ")"
	args := make([]any, 0, len(statuses)+1)
	args = append(args, channelID)
	for _, s := range statuses {
		args = append(args, string(s))
	}

	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, jobsSnapshot{}, err
	}
	defer tx.Rollback()

	// History FIRST — the subquery reads jobs.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM history WHERE video_id IN (SELECT id FROM jobs WHERE "+match+")",
		args...); err != nil {
		return 0, jobsSnapshot{}, err
	}

	res, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE "+match, args...)
	if err != nil {
		return 0, jobsSnapshot{}, err
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, jobsSnapshot{}, err
	}
	if n == 0 {
		return 0, jobsSnapshot{}, nil
	}
	return int(n), db.snapshotJobsChange(), nil
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
				db.logger.Warn("getGaps: scan error", "jobID", jobID, "err", err)
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

	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO trims (id, job_id, start_time, end_time, filename, created_at, duration, file_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		trim.ID, trim.JobID, trim.StartTime, trim.EndTime, trim.Filename,
		trim.CreatedAt, trim.Duration, trim.FileSize)
	if err != nil {
		db.mu.Unlock()
		return err
	}

	db.mu.Unlock()
	// Fires ONLY OnTrimsChanged — the legacy OnJobsChange dispatch was
	// dropped now that the WS broadcaster + TUI consume the targeted
	// lifecycle event (DECISIONS #21 consumer migration). Only the bulk
	// writers (BatchSetWatched, DeleteJobsAndHistoryForChannel) still fire
	// OnJobsChange.
	db.notifyTrimsChanged(trim.JobID)
	return nil
}

// DeleteTrim removes a trim record.
func (db *Database) DeleteTrim(trimID string) error {
	db.mu.Lock()

	// Look up the parent job_id BEFORE the DELETE so notifyTrimsChanged
	// can carry it. ErrNoRows means the trim never existed — silent
	// success: no event fired, no broadcast, the caller's "delete
	// this trim" intent is satisfied by the SQL no-op.
	var jobID string
	err := db.db.QueryRowContext(db.getCtx(), "SELECT job_id FROM trims WHERE id = ?", trimID).Scan(&jobID)
	if err == sql.ErrNoRows {
		db.mu.Unlock()
		return nil
	}
	if err != nil {
		db.mu.Unlock()
		return err
	}

	if _, err := db.db.ExecContext(db.getCtx(), "DELETE FROM trims WHERE id = ?", trimID); err != nil {
		db.mu.Unlock()
		return err
	}

	db.mu.Unlock()
	// Same as AddTrim — fires ONLY OnTrimsChanged; the legacy
	// OnJobsChange dispatch was dropped per DECISIONS #21.
	db.notifyTrimsChanged(jobID)
	return nil
}

// GetTrimsForJob returns all trim records for a given job.
func (db *Database) GetTrimsForJob(jobID string) ([]TrimRecord, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.getTrimsUnlocked(jobID)
}

func (db *Database) getTrimsUnlocked(jobID string) ([]TrimRecord, error) {
	rows, err := db.db.QueryContext(db.getCtx(), `SELECT `+trimColumns+` FROM trims WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	// Projection and scan are shared with GetAllTrims (database_extras.go):
	// same columns, same order, one place to change them.
	return db.scanTrims(rows, "getTrimsUnlocked", "jobID", jobID)
}

// AddSegment adds a part record for a multi-part (quality/gap split) job.
func (db *Database) AddSegment(seg *Segment) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	result, err := db.db.ExecContext(db.getCtx(), `INSERT INTO segments (job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds, chat_file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seg.JobID, seg.SegmentIndex, seg.UnixStart, seg.UnixEnd, seg.Quality, seg.Filename,
		seg.FilePath, seg.FileSize, seg.VideoWidth, seg.VideoHeight, seg.VideoFps, seg.DurationSeconds,
		seg.ChatFile)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	seg.ID = int(id)
	return nil
}

// UpdateSegmentFile updates a segment row's file identity (filename,
// file_path, chat_file). Used by the single-part rename at finalize, where
// "{name} - part1" collapses to the plain "{name}" when a job ends with
// exactly one part.
func (db *Database) UpdateSegmentFile(id int, filename, filePath, chatFile string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(),
		`UPDATE segments SET filename = ?, file_path = ?, chat_file = ? WHERE id = ?`,
		filename, filePath, chatFile, id)
	return err
}

// ClearJobSegmentsAndGaps deletes all segment and gap rows for a job. Used by
// a user-initiated Reinitialize ("fresh start"): without it, stale part rows
// from a prior quality/gap-split attempt survive the reset, and the clean
// re-download is then finalized as multi-part from the OLD part files while the
// freshly-downloaded media is discarded. (The job-delete cascade is the only
// other place these rows are removed.)
//
// One transaction, like ReplaceJobSegments: as two autocommit DELETEs a crash
// between them left the previous attempt's gap rows on the fresh run.
func (db *Database) ClearJobSegmentsAndGaps(jobID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM segments WHERE job_id = ?", jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM gaps WHERE job_id = ?", jobID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceJobSegments atomically replaces all segment rows for a job with segs:
// existing rows are deleted, then segs are re-inserted using the same column
// list as AddSegment, all inside one transaction. Used by the part-merge row
// collapse (quality-split part rows folded together on resume/finalize).
// Unlike ClearJobSegmentsAndGaps, gap rows are left untouched — this replaces
// segments only. Fires no job-update notification (AddSegment fires none).
//
// The jobID parameter — not each row's own JobID field — is the single
// source of truth for both the DELETE scope and the inserted job_id: a
// mismatched or stale seg.JobID is corrected to jobID (and written back onto
// the caller's row) rather than trusted, so a row that happens to name a
// different existing job can never delete one job's rows while silently
// inserting them under another. On a non-nil error the transaction is rolled
// back, but seg.ID values already written onto rows preceding the failure
// are stale — output state (including seg.ID/seg.JobID) is unspecified on
// error.
func (db *Database) ReplaceJobSegments(jobID string, segs []Segment) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM segments WHERE job_id = ?", jobID); err != nil {
		return err
	}

	for i := range segs {
		seg := &segs[i]
		result, err := tx.ExecContext(ctx, `INSERT INTO segments (job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds, chat_file)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			jobID, seg.SegmentIndex, seg.UnixStart, seg.UnixEnd, seg.Quality, seg.Filename,
			seg.FilePath, seg.FileSize, seg.VideoWidth, seg.VideoHeight, seg.VideoFps, seg.DurationSeconds,
			seg.ChatFile)
		if err != nil {
			return err
		}
		id, _ := result.LastInsertId()
		seg.ID = int(id)
		seg.JobID = jobID
	}

	return tx.Commit()
}

// GetSegments returns all segments for a given job, ordered by segment_index.
func (db *Database) GetSegments(jobID string) ([]Segment, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.getSegments(jobID)
}

func (db *Database) getSegments(jobID string) ([]Segment, error) {
	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds, chat_file
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
			&s.VideoWidth, &s.VideoHeight, &s.VideoFps, &s.DurationSeconds, &s.ChatFile); err != nil {
			if db.logger != nil {
				db.logger.Warn("getSegments: scan error", "jobID", jobID, "err", err)
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

// idChunkSize bounds the number of bind parameters per query to stay well
// under SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766 in modern builds, 999 in
// older defaults). 500 leaves plenty of headroom and keeps plans compact.
const idChunkSize = 500

// attachTrimsAndGaps batch-loads trims, gaps, and segments for the given jobs
// and attaches them to the caller-provided slice. Queries are narrowed to the
// requested job IDs via WHERE job_id IN (...) and chunked to respect
// SQLITE_MAX_VARIABLE_NUMBER.
// Caller must already hold db.mu (read or write).
//
// A failed query or an iteration that ends in error fails the whole load
// rather than returning the jobs without their child rows: the orphan scanner
// reads segment chat files through GetAllJobs, so a job silently missing its
// segments makes those files look like orphans. A single row that fails to
// scan is logged and skipped, as getGaps/getSegments and the jobs loop do.
func (db *Database) attachTrimsAndGaps(jobs []*Job) error {
	if len(jobs) == 0 {
		return nil
	}

	// Collect the job IDs we actually care about so each sub-query is
	// parametrized (WHERE job_id IN (?, ?, ...)) instead of scanning the
	// whole trims/gaps/segments table.
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}

	trimMap := make(map[string][]TrimRecord, len(jobs))
	gapMap := make(map[string][]Gap, len(jobs))
	segMap := make(map[string][]Segment, len(jobs))

	// each runs one child query and hands every row to scan, which reports
	// whether the row scanned. It owns the Close and the rows.Err check, so
	// the three loads below cannot drift apart on either.
	each := func(table, query string, args []any, scan func(*sql.Rows) error) error {
		rows, err := db.db.QueryContext(db.getCtx(), query, args...)
		if err != nil {
			return fmt.Errorf("attachTrimsAndGaps: query %s: %w", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil && db.logger != nil {
				db.logger.Warn("attachTrimsAndGaps: scan error", "table", table, "err", err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("attachTrimsAndGaps: read %s: %w", table, err)
		}
		return nil
	}

	for start := 0; start < len(ids); start += idChunkSize {
		end := min(start+idChunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1] // drop trailing ","
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		// Trims
		if err := each("trims",
			`SELECT id, job_id, start_time, end_time, filename, created_at, duration, file_size
			FROM trims WHERE job_id IN (`+placeholders+`)`, args,
			func(rows *sql.Rows) error {
				var tr TrimRecord
				if err := rows.Scan(&tr.ID, &tr.JobID, &tr.StartTime, &tr.EndTime,
					&tr.Filename, &tr.CreatedAt, &tr.Duration, &tr.FileSize); err != nil {
					return err
				}
				trimMap[tr.JobID] = append(trimMap[tr.JobID], tr)
				return nil
			}); err != nil {
			return err
		}

		// Gaps
		if err := each("gaps",
			`SELECT id, job_id, gap_from, gap_to, stream
			FROM gaps WHERE job_id IN (`+placeholders+`)`, args,
			func(rows *sql.Rows) error {
				var g Gap
				if err := rows.Scan(&g.ID, &g.JobID, &g.From, &g.To, &g.Stream); err != nil {
					return err
				}
				gapMap[g.JobID] = append(gapMap[g.JobID], g)
				return nil
			}); err != nil {
			return err
		}

		// Segments — keep this column list in lockstep with getSegments:
		// the orphan scanner protects part chat files through THIS loader
		// (GetAllJobs), so a column missed here reads as "no chat file" and
		// the file becomes deletable as an orphan.
		if err := each("segments",
			`SELECT id, job_id, segment_index, unix_start, unix_end, quality, filename, file_path, file_size, video_width, video_height, video_fps, duration_seconds, chat_file
			FROM segments WHERE job_id IN (`+placeholders+`) ORDER BY segment_index`, args,
			func(rows *sql.Rows) error {
				var s Segment
				if err := rows.Scan(&s.ID, &s.JobID, &s.SegmentIndex, &s.UnixStart, &s.UnixEnd,
					&s.Quality, &s.Filename, &s.FilePath, &s.FileSize,
					&s.VideoWidth, &s.VideoHeight, &s.VideoFps, &s.DurationSeconds, &s.ChatFile); err != nil {
					return err
				}
				segMap[s.JobID] = append(segMap[s.JobID], s)
				return nil
			}); err != nil {
			return err
		}
	}

	for _, job := range jobs {
		if trims, ok := trimMap[job.ID]; ok {
			job.Trims = trims
		}
		if gaps, ok := gapMap[job.ID]; ok {
			job.Gaps = gaps
		}
		if segs, ok := segMap[job.ID]; ok {
			job.Segments = segs
		}
	}
	return nil
}

// GetJobStats returns aggregate statistics across all jobs. The result is a
// full-table scan, so values are cached for jobStatsCacheTTL (~5s); callers
// that need exact numbers should query directly.
func (db *Database) GetJobStats() (*JobStats, error) {
	// Fast path: serve from cache if fresh. Returning the cached pointer is
	// safe because callers only read the value; no mutations are expected.
	db.statsMu.Lock()
	if db.statsCached != nil && time.Since(db.statsCachedAt) < jobStatsCacheTTL {
		cached := db.statsCached
		db.statsMu.Unlock()
		return cached, nil
	}
	db.statsMu.Unlock()

	db.mu.RLock()
	defer db.mu.RUnlock()

	// Status literals here are interpolated from the JobStatus constants in
	// types.go so a status rename can't silently desync the stats query
	// (audit reports/database.md Q6).
	statsQuery := fmt.Sprintf(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status = '%s' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status IN ('%s', '%s') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform IN ('youtube', '') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform = 'twitch' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform IN ('youtube', '') AND status IN ('%s', '%s', '%s') THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN platform = 'twitch' AND status IN ('%s', '%s', '%s') THEN file_size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN length_seconds ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = '%s' THEN total_chat_messages ELSE 0 END), 0)
		FROM jobs`,
		StatusFinished, StatusDownloading, StatusLive, StatusMuxing, StatusError, StatusCancelled, StatusQueued,
		StatusFinished, StatusError, StatusCancelled,
		// The per-platform sizes count the same three statuses the per-status
		// ones do, so the two cards always add up to Total Recorded. They
		// summed every row, and a Finished incomplete-tail job being Resumed
		// keeps its file_size while Downloading — the platforms then
		// outweighed the total for the length of the resume.
		StatusFinished, StatusError, StatusCancelled,
		StatusFinished, StatusError, StatusCancelled,
		StatusFinished, StatusFinished,
	)

	var s JobStats
	err := db.db.QueryRowContext(db.getCtx(), statsQuery).Scan(
		&s.TotalCount,
		&s.FinishedCount, &s.ActiveCount, &s.MuxingCount,
		&s.ErrorCount, &s.CancelledCount, &s.QueuedCount,
		&s.YouTubeCount, &s.TwitchCount,
		&s.FinishedSize, &s.ErrorSize, &s.CancelledSize,
		&s.YouTubeSize, &s.TwitchSize,
		&s.TotalDuration, &s.TotalChatMessages,
	)
	if err != nil {
		return nil, fmt.Errorf("GetJobStats: %w", err)
	}

	// Cache the fresh result for the next ~5s.
	db.statsMu.Lock()
	db.statsCached = &s
	db.statsCachedAt = time.Now()
	db.statsMu.Unlock()

	return &s, nil
}

// --- Job logs ---

// capLogLines enforces the 200-line cap on a per-job log buffer. When
// exceeded, the slice is trimmed to the most recent 100 lines. Shared by
// AddJobLog and RouteLogToJobs so the policy lives in one place.
func capLogLines(logs []string) []string {
	if len(logs) > 200 {
		return logs[len(logs)-100:]
	}
	return logs
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

// ClearJobLogs removes the per-job log buffer and stops routing to it.
func (db *Database) ClearJobLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	delete(db.jobLogs, jobID)
	delete(db.logRouted, jobID)
}

// RouteLogToJobs checks if a log line contains any TRACKED job ID and routes
// it to the corresponding per-job log buffer. Only non-terminal jobs are
// tracked (see SyncJobLogTracking / UntrackJobForLogs), so the scan is over
// the jobs that are actually producing log lines rather than over every row
// the database has ever held — years of Finished jobs used to be scanned per
// line, under this lock (CORE-12). Each log line belongs to at most one job.
//
// The cost per line is still a substring scan per tracked ID — proportional
// to the number of LIVE jobs, not constant: 5 live measures ~99 ns, 5,000
// live measures ~76 µs, which is the pre-CORE-12 cost. Live jobs are bounded
// by the archive slots and num_parallel_downloads, so that ceiling is not
// reachable in practice; keeping a terminal job tracked is what used to make
// it so.
func (db *Database) RouteLogToJobs(line string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()

	for jobID := range db.logRouted {
		if strings.Contains(line, jobID) {
			db.jobLogs[jobID] = capLogLines(append(db.jobLogs[jobID], line))
			return // Each log line belongs to at most one job
		}
	}
}

// TrackJobForLogs starts routing log lines to a job's buffer. Callers gate
// this on the job being non-terminal; SyncJobLogTracking does it for a whole
// list.
func (db *Database) TrackJobForLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	db.trackForLogsLocked(jobID)
}

// UntrackJobForLogs stops routing to a job while KEEPING its buffer: a job
// that just failed is exactly the one whose log the operator opens next
// (CORE-12). ClearJobLogs is what drops the buffer.
func (db *Database) UntrackJobForLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	delete(db.logRouted, jobID)
}

// SyncJobLogTracking brings the routed set in line with a job list: every
// non-terminal job is tracked, every terminal one untracked (buffer kept).
// Both callers in cmd/moombox — the boot seed over GetAllJobs and the
// OnJobsChange fan-out — used to track EVERY row, which is what made
// RouteLogToJobs scan the whole history per log line (CORE-12). One lock
// acquisition for the list, not one per job.
func (db *Database) SyncJobLogTracking(jobs []*Job) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if j.IsTerminal() {
			delete(db.logRouted, j.ID)
			continue
		}
		db.trackForLogsLocked(j.ID)
	}
}

// trackForLogsLocked is TrackJobForLogs' body; callers hold jobLogsMu.
func (db *Database) trackForLogsLocked(jobID string) {
	db.logRouted[jobID] = struct{}{}
	if _, ok := db.jobLogs[jobID]; !ok {
		db.jobLogs[jobID] = nil
	}
}

// PruneJobLogs removes log entries — and routing — for job IDs not in the
// provided set. Called on jobsChange to keep the log maps in sync with the
// database.
func (db *Database) PruneJobLogs(activeIDs map[string]struct{}) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	for id := range db.jobLogs {
		if _, ok := activeIDs[id]; !ok {
			delete(db.jobLogs, id)
		}
	}
	for id := range db.logRouted {
		if _, ok := activeIDs[id]; !ok {
			delete(db.logRouted, id)
		}
	}
}
