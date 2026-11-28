package database

// The rows the worker's latency-mode backfill walks (owner ruling RV-19,
// "Backfill everything in the background"): archived YouTube recordings
// whose broadcast's latency mode was never asked, because they finished
// before v30 stored it, or are VOD downloads, for which the worker stores a
// mode only when YouTube names one.

// LatencyBackfillRow is one recording the backfill may ask YouTube about.
type LatencyBackfillRow struct {
	JobID   string
	VideoID string
}

// latencyModeUnset is the SQL for "never asked": NULL, or an empty string,
// which nothing writes today but which Job.LatencyMode reads the same way.
const latencyModeUnset = "(latency_mode IS NULL OR latency_mode = '')"

// importPlaceholderGlob matches the video ID an archive import gives a row
// that carries none of its own ("imp_" and randomHex(4)'s eight lowercase
// hex digits, importPlaceholderPattern in internal/web/routes): twelve
// characters, so a YouTube video ID (eleven) never matches it.
const importPlaceholderGlob = "imp_[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]"

// LatencyBackfillRows returns, in job ID order, up to limit Finished
// non-Twitch rows with a video ID and no latency mode whose ID sorts after
// after ("" for the start). A Finished row is the archive the backfill is
// for; a row still on its way is the worker's to fill at its own
// extraction. Twitch has no latency mode. The walk keeps its place by the
// last ID it was handed, so a row it cannot settle does not stand at the
// head of every pass. An archive import's row is left out: its video ID is
// the placeholder "imp_" and eight hex digits (importPlaceholderGlob), not
// a video YouTube could answer about.
func (db *Database) LatencyBackfillRows(after string, limit int) ([]LatencyBackfillRow, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, video_id FROM jobs
		 WHERE status = ? AND (platform IS NULL OR platform <> 'twitch') AND video_id <> ''
		   AND video_id NOT GLOB '`+importPlaceholderGlob+`'
		   AND `+latencyModeUnset+` AND id > ?
		 ORDER BY id LIMIT ?`,
		StatusFinished, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatencyBackfillRow
	for rows.Next() {
		var r LatencyBackfillRow
		if err := rows.Scan(&r.JobID, &r.VideoID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetLatencyModeIfUnset stores mode as jobID's latency mode only while the
// row has none, in one statement, and reports whether it did: a mode the
// worker stored meanwhile (a Reinitialize that re-ran the extraction) is
// never replaced. The subscribers are told, so both UIs show the new Latency
// row, but updated_at is left alone: learning a fact about an old archive is
// not a change that should move it to the top of the player's recording
// picker, which sorts by it.
func (db *Database) SetLatencyModeIfUnset(jobID string, mode LatencyMode) bool {
	if mode == LatencyModeNone {
		return false
	}
	_, applied := db.updateJobFieldsWhereStamped(jobID, map[string]any{"latency_mode": mode}, latencyModeUnset, nil, false)
	return applied
}

// HasJobWithStatus reports whether any row has status: the backfill holds
// its next request while a capture is starting (StatusLive, the step between
// the go-live confirmation and the first segment).
func (db *Database) HasJobWithStatus(status JobStatus) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var one int
	err := db.db.QueryRowContext(db.getCtx(), `SELECT EXISTS(SELECT 1 FROM jobs WHERE status = ?)`, status).Scan(&one)
	return one == 1, err
}
