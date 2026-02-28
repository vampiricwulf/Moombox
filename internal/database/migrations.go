package database

const schemaVersion = 1

const createSchema = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    video_id TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    channel_name TEXT NOT NULL DEFAULT '',
    platform TEXT DEFAULT 'youtube',
    status TEXT NOT NULL DEFAULT 'Upcoming',
    progress TEXT DEFAULT '',
    percent REAL DEFAULT 0,
    eta TEXT DEFAULT '',
    speed TEXT DEFAULT '',
    error TEXT DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_video_seq INTEGER,
    last_audio_seq INTEGER,
    total_video_seq INTEGER,
    total_audio_seq INTEGER,
    is_vod INTEGER DEFAULT 0,
    manually_added INTEGER DEFAULT 0,
    allow_non_stream INTEGER DEFAULT 0,
    stream_start_time TEXT,
    stream_end_time TEXT,
    length_seconds INTEGER,
    download_started_at TEXT,
    thumbnail_url TEXT,
    description TEXT,
    output_file TEXT,
    filename TEXT,
    output_directory TEXT,
    video_width INTEGER,
    video_height INTEGER,
    video_fps INTEGER,
    file_size INTEGER,
    chat_status TEXT,
    total_chat_messages INTEGER,
    chat_filename TEXT,
    twitch_quality TEXT,
    twitch_category TEXT,
    channel_avatar_url TEXT,
    selected_video_itag INTEGER,
    selected_audio_itag INTEGER,
    start_time REAL,
    end_time REAL,
    last_recheck_at TEXT
);

CREATE TABLE IF NOT EXISTS gaps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    gap_from INTEGER NOT NULL,
    gap_to INTEGER NOT NULL,
    stream TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS trims (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    start_time REAL NOT NULL,
    end_time REAL NOT NULL,
    filename TEXT NOT NULL,
    created_at TEXT NOT NULL,
    duration REAL NOT NULL,
    file_size INTEGER
);

CREATE TABLE IF NOT EXISTS history (
    video_id TEXT PRIMARY KEY,
    added_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS last_videos (
    channel_id TEXT PRIMARY KEY,
    video_id TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_updated_at ON jobs(updated_at);
CREATE INDEX IF NOT EXISTS idx_gaps_job_id ON gaps(job_id);
CREATE INDEX IF NOT EXISTS idx_trims_job_id ON trims(job_id);
`

func (db *Database) migrate() error {
	// Check current schema version
	var version int
	row := db.db.QueryRowContext(db.getCtx(), "SELECT version FROM schema_version LIMIT 1")
	err := row.Scan(&version)
	if err != nil {
		// Table doesn't exist or is empty — create everything
		if _, err := db.db.ExecContext(db.getCtx(), createSchema); err != nil {
			return err
		}
		_, err = db.db.ExecContext(db.getCtx(), "INSERT INTO schema_version (version) VALUES (?)", schemaVersion)
		return err
	}

	// Run incremental migrations if needed
	if version < schemaVersion {
		// Future migrations go here
		_, err = db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", schemaVersion)
		return err
	}

	return nil
}
