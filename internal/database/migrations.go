package database

import (
	"os"
	"path/filepath"
	"strings"
)

const schemaVersion = 6

const createSchema = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL UNIQUE
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
    chat_file TEXT,
    thumbnail_file TEXT,
    description_file TEXT,
    twitch_quality TEXT,
    twitch_category TEXT,
    channel_avatar_url TEXT,
    selected_video_itag INTEGER,
    selected_audio_itag INTEGER,
    start_time REAL,
    end_time REAL,
    last_recheck_at TEXT,
    quality_preference TEXT DEFAULT ''
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
CREATE TABLE IF NOT EXISTS segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    segment_index INTEGER NOT NULL,
    unix_start INTEGER NOT NULL,
    unix_end INTEGER NOT NULL,
    quality TEXT NOT NULL,
    filename TEXT NOT NULL,
    file_path TEXT,
    file_size INTEGER,
    video_width INTEGER,
    video_height INTEGER,
    video_fps INTEGER,
    duration_seconds REAL
);

CREATE TABLE IF NOT EXISTS client_tokens (
    id TEXT PRIMARY KEY,
    token_prefix TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL DEFAULT '',
    label TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT '',
    last_used_at TEXT NOT NULL DEFAULT '',
    last_ip TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_gaps_job_id ON gaps(job_id);
CREATE INDEX IF NOT EXISTS idx_trims_job_id ON trims(job_id);
CREATE INDEX IF NOT EXISTS idx_segments_job_id ON segments(job_id);
CREATE INDEX IF NOT EXISTS idx_client_tokens_prefix ON client_tokens(token_prefix);
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
	if version < 2 {
		// Add chat_file column to store absolute chat file path
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN chat_file TEXT`); err != nil {
			// Column may already exist if migration was partially applied
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}

		// Backfill: derive chat_file from output_file for existing jobs
		rows, err := db.db.QueryContext(db.getCtx(),
			`SELECT id, output_file, chat_filename FROM jobs WHERE output_file != '' AND chat_filename != '' AND (chat_file IS NULL OR chat_file = '')`)
		if err == nil {
			for rows.Next() {
				var id, outputFile, chatFilename string
				if err := rows.Scan(&id, &outputFile, &chatFilename); err != nil {
					continue
				}
				// Chat file lives alongside the output file — replace video extension with .chat.json
				ext := filepath.Ext(outputFile)
				chatFile := strings.TrimSuffix(outputFile, ext) + ".chat.json"
				if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET chat_file = ? WHERE id = ?`, chatFile, id); err != nil && db.logger != nil {
					db.logger.Warn("migration v2: failed to backfill chat_file", "jobID", id, "err", err)
				}
			}
			rows.Close()
		}

		_, err = db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 2)
		if err != nil {
			return err
		}
	}

	if version < 3 {
		// Add thumbnail_file and description_file columns
		for _, col := range []string{"thumbnail_file", "description_file"} {
			if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN `+col+` TEXT`); err != nil {
				if !strings.Contains(err.Error(), "duplicate column") {
					return err
				}
			}
		}

		// Backfill: derive thumbnail_file and description_file from output_file
		rows, err := db.db.QueryContext(db.getCtx(),
			`SELECT id, output_file FROM jobs WHERE output_file != '' AND (thumbnail_file IS NULL OR thumbnail_file = '')`)
		if err == nil {
			for rows.Next() {
				var id, outputFile string
				if err := rows.Scan(&id, &outputFile); err != nil {
					continue
				}
				ext := filepath.Ext(outputFile)
				base := strings.TrimSuffix(outputFile, ext)

				// Check which thumbnail extension exists on disk
				for _, thumbExt := range []string{".jpg", ".webp", ".png"} {
					thumbPath := base + thumbExt
					if fileExists(thumbPath) {
						if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET thumbnail_file = ? WHERE id = ?`, thumbPath, id); err != nil && db.logger != nil {
							db.logger.Warn("migration v3: failed to backfill thumbnail_file", "jobID", id, "err", err)
						}
						break
					}
				}

				// Check if description file exists
				descPath := base + ".description"
				if fileExists(descPath) {
					if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET description_file = ? WHERE id = ?`, descPath, id); err != nil && db.logger != nil {
						db.logger.Warn("migration v3: failed to backfill description_file", "jobID", id, "err", err)
					}
				}
			}
			rows.Close()
		}

		_, err = db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 3)
		if err != nil {
			return err
		}
	}

	if version < 4 {
		// Add index for video_id (used by HasActiveJob, AddToHistory)
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE INDEX IF NOT EXISTS idx_jobs_video_id ON jobs(video_id)`); err != nil {
			return err
		}

		_, err := db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 4)
		if err != nil {
			return err
		}
	}

	if version < 5 {
		// Add quality_preference column to jobs
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN quality_preference TEXT DEFAULT ''`); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}

		// Create segments table for multi-segment quality-split jobs
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE TABLE IF NOT EXISTS segments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			segment_index INTEGER NOT NULL,
			unix_start INTEGER NOT NULL,
			unix_end INTEGER NOT NULL,
			quality TEXT NOT NULL,
			filename TEXT NOT NULL,
			file_path TEXT,
			file_size INTEGER,
			video_width INTEGER,
			video_height INTEGER,
			video_fps INTEGER,
			duration_seconds REAL
		)`); err != nil {
			return err
		}
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE INDEX IF NOT EXISTS idx_segments_job_id ON segments(job_id)`); err != nil {
			return err
		}

		_, err := db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 5)
		if err != nil {
			return err
		}
	}

	if version < 6 {
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE TABLE IF NOT EXISTS client_tokens (
			id TEXT PRIMARY KEY,
			token_prefix TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL DEFAULT '',
			label TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			last_used_at TEXT NOT NULL DEFAULT '',
			last_ip TEXT NOT NULL DEFAULT ''
		)`); err != nil {
			return err
		}
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE INDEX IF NOT EXISTS idx_client_tokens_prefix ON client_tokens(token_prefix)`); err != nil {
			return err
		}

		_, err := db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 6)
		if err != nil {
			return err
		}
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
