package database

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isDuplicateColumnErr reports whether err indicates an ALTER TABLE ADD
// COLUMN failed because the column already exists. Used to make migrations
// idempotent across partial-apply retries.
//
// modernc.org/sqlite does expose an integer result code via *sqlite.Error,
// but the driver returns a wrapped error whose type isn't importable without
// pulling the driver into this package's dependency graph. Stick with string
// matching for now — if modernc changes the error text, migrations start
// failing loudly rather than silently, which is the preferable fail mode.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column")
}

const schemaVersion = 15

// CurrentSchemaVersion returns the schema version this binary creates and
// migrates to. Exposed for side processes (`moombox add`) that must refuse
// to touch a database from a different binary version.
func CurrentSchemaVersion() int {
	return schemaVersion
}

// createSchema defines the full schema for new databases. It includes all tables
// and indexes from the start. The incremental migrations below handle upgrading
// existing databases from older schema versions. Tables like "segments" and
// "client_tokens" appear in both places by design: createSchema for new databases,
// migrations for upgrades from older versions.
//
// Schema versioning uses SQLite's built-in PRAGMA user_version (since v11). Older
// databases created with the legacy schema_version table are auto-migrated by
// migrate() on first open.
const createSchema = `
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
    quality_preference TEXT DEFAULT '',
    watched INTEGER DEFAULT 0,
    resume_position REAL,
    chat_offset REAL NOT NULL DEFAULT 0,
    auto_retry_count INTEGER NOT NULL DEFAULT 0
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
CREATE INDEX IF NOT EXISTS idx_jobs_video_id ON jobs(video_id);
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
    duration_seconds REAL,
    chat_file TEXT NOT NULL DEFAULT ''
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
CREATE INDEX IF NOT EXISTS idx_history_added_at ON history(added_at);
`

// readUserVersion reads SQLite's PRAGMA user_version. Returns 0 on a fresh DB.
func (db *Database) readUserVersion() (int, error) {
	var v int
	err := db.db.QueryRowContext(db.getCtx(), "PRAGMA user_version").Scan(&v)
	return v, err
}

// writeUserVersion sets SQLite's PRAGMA user_version. PRAGMA statements do not
// accept bind parameters, so the value is interpolated via %d (always a Go int
// literal in-package, never user input — injection-safe).
func (db *Database) writeUserVersion(v int) error {
	_, err := db.db.ExecContext(db.getCtx(), fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}

// readLegacyVersion returns the value from the old schema_version table, or
// (0, false) if the table is missing, empty, or can't be read. Used once during
// the v11 cutover to carry the version forward to PRAGMA user_version.
func (db *Database) readLegacyVersion() (int, bool) {
	var v int
	err := db.db.QueryRowContext(db.getCtx(), "SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (db *Database) migrate() error {
	// Read current schema version from PRAGMA user_version (authoritative since v11).
	version, err := db.readUserVersion()
	if err != nil {
		return err
	}

	// If PRAGMA is 0 the DB is either brand-new or still on the pre-v11 schema_version
	// table. Check the legacy table; if it exists, carry the value forward.
	if version == 0 {
		if legacy, hasLegacy := db.readLegacyVersion(); hasLegacy {
			version = legacy
			if err := db.writeUserVersion(version); err != nil {
				return err
			}
		} else {
			// Fresh install — create schema, seed PRAGMA at current version.
			if _, err := db.db.ExecContext(db.getCtx(), createSchema); err != nil {
				return err
			}
			return db.writeUserVersion(schemaVersion)
		}
	}

	// Downgrade guard: a database whose schema is NEWER than this binary
	// means the user rolled back after a newer version migrated it (the
	// exact scenario the update-rollback path enables). Every `version < N`
	// block below would be false and the old binary would silently write
	// against tables and columns it has never seen. Refuse loudly instead —
	// the `moombox add` side process already has this guard (addvideo.go
	// via FileSchemaVersion); this closes the same hole for the daemon.
	if version > schemaVersion {
		return fmt.Errorf(
			"database schema v%d is newer than this binary supports (v%d) — you appear to have downgraded after an update migrated the database; restore the newer binary (moombox.exe.old from the update swap, if still present) or upgrade again",
			version, schemaVersion)
	}

	// Run incremental migrations if needed
	if version < 2 {
		// Add chat_file column to store absolute chat file path
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN chat_file TEXT`); err != nil {
			// Column may already exist if migration was partially applied
			if !isDuplicateColumnErr(err) {
				return err
			}
		}

		// Backfill: derive chat_file from output_file for existing jobs.
		// Collect first, THEN update — with MaxOpenConns(1) an UPDATE issued
		// while the SELECT cursor is still open waits forever for the pool's
		// only connection.
		type chatBackfill struct{ id, chatFile string }
		var chatBackfills []chatBackfill
		rows, err := db.db.QueryContext(db.getCtx(),
			`SELECT id, output_file FROM jobs WHERE output_file != '' AND chat_filename != '' AND (chat_file IS NULL OR chat_file = '')`)
		if err == nil {
			for rows.Next() {
				var id, outputFile string
				if err := rows.Scan(&id, &outputFile); err != nil {
					continue
				}
				// Chat file lives alongside the output file — replace video extension with .chat.json
				ext := filepath.Ext(outputFile)
				chatBackfills = append(chatBackfills, chatBackfill{
					id:       id,
					chatFile: strings.TrimSuffix(outputFile, ext) + ".chat.json",
				})
			}
			rows.Close()
			if err := rows.Err(); err != nil && db.logger != nil {
				db.logger.Warn("migration v2: row iteration error during backfill", "err", err)
			}
			for _, b := range chatBackfills {
				if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET chat_file = ? WHERE id = ?`, b.chatFile, b.id); err != nil && db.logger != nil {
					db.logger.Warn("migration v2: failed to backfill chat_file", "jobID", b.id, "err", err)
				}
			}
		}

		err = db.writeUserVersion(2)
		if err != nil {
			return err
		}
	}

	if version < 3 {
		// Add thumbnail_file and description_file columns.
		//
		// WARNING: The column names are concatenated into the ALTER TABLE
		// statement below because SQLite does not allow bind parameters for
		// identifiers. This is safe ONLY because the slice is a hardcoded
		// compile-time literal — do not copy this pattern with any value
		// that could come from user input, configuration, or another query.
		// Use a strict allowlist check instead.
		for _, col := range []string{"thumbnail_file", "description_file"} {
			if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN `+col+` TEXT`); err != nil {
				if !isDuplicateColumnErr(err) {
					return err
				}
			}
		}

		// Backfill: derive thumbnail_file and description_file from
		// output_file. Collect first, THEN update — same MaxOpenConns(1)
		// constraint as the v2 backfill above.
		type assetBackfill struct{ id, outputFile string }
		var assetBackfills []assetBackfill
		rows, err := db.db.QueryContext(db.getCtx(),
			`SELECT id, output_file FROM jobs WHERE output_file != '' AND (thumbnail_file IS NULL OR thumbnail_file = '')`)
		if err == nil {
			for rows.Next() {
				var id, outputFile string
				if err := rows.Scan(&id, &outputFile); err != nil {
					continue
				}
				assetBackfills = append(assetBackfills, assetBackfill{id: id, outputFile: outputFile})
			}
			rows.Close()
			if err := rows.Err(); err != nil && db.logger != nil {
				db.logger.Warn("migration v3: row iteration error during backfill", "err", err)
			}

			for _, b := range assetBackfills {
				ext := filepath.Ext(b.outputFile)
				base := strings.TrimSuffix(b.outputFile, ext)

				// Check which thumbnail extension exists on disk
				for _, thumbExt := range []string{".jpg", ".webp", ".png"} {
					thumbPath := base + thumbExt
					if fileExists(thumbPath) {
						if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET thumbnail_file = ? WHERE id = ?`, thumbPath, b.id); err != nil && db.logger != nil {
							db.logger.Warn("migration v3: failed to backfill thumbnail_file", "jobID", b.id, "err", err)
						}
						break
					}
				}

				// Check if description file exists
				descPath := base + ".description"
				if fileExists(descPath) {
					if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET description_file = ? WHERE id = ?`, descPath, b.id); err != nil && db.logger != nil {
						db.logger.Warn("migration v3: failed to backfill description_file", "jobID", b.id, "err", err)
					}
				}
			}
		}

		err = db.writeUserVersion(3)
		if err != nil {
			return err
		}
	}

	if version < 4 {
		// Add index for video_id (used by HasActiveJob, AddToHistory)
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE INDEX IF NOT EXISTS idx_jobs_video_id ON jobs(video_id)`); err != nil {
			return err
		}

		err := db.writeUserVersion(4)
		if err != nil {
			return err
		}
	}

	if version < 5 {
		// Add quality_preference column to jobs
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN quality_preference TEXT DEFAULT ''`); err != nil {
			if !isDuplicateColumnErr(err) {
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

		err := db.writeUserVersion(5)
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

		err := db.writeUserVersion(6)
		if err != nil {
			return err
		}
	}

	if version < 7 {
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE TABLE IF NOT EXISTS player_prefs (
			video_id TEXT PRIMARY KEY,
			chat_offset REAL NOT NULL DEFAULT 0
		)`); err != nil {
			return err
		}

		err := db.writeUserVersion(7)
		if err != nil {
			return err
		}
	}

	if version < 8 {
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN watched INTEGER DEFAULT 0`); err != nil {
			if !isDuplicateColumnErr(err) {
				return err
			}
		}
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN resume_position REAL`); err != nil {
			if !isDuplicateColumnErr(err) {
				return err
			}
		}

		err := db.writeUserVersion(8)
		if err != nil {
			return err
		}
	}

	if version < 9 {
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN chat_offset REAL NOT NULL DEFAULT 0`); err != nil {
			if !isDuplicateColumnErr(err) {
				return err
			}
		}

		// Count how many rows we *expect* to backfill so we can detect partial copies.
		var expected int64
		if err := db.db.QueryRowContext(db.getCtx(), `SELECT COUNT(*)
			FROM jobs WHERE EXISTS (
				SELECT 1 FROM player_prefs pp WHERE pp.video_id = jobs.video_id
			)`).Scan(&expected); err != nil && db.logger != nil {
			// Not fatal: just means we can't sanity-check the UPDATE.
			db.logger.Warn("v9 migration: could not count rows eligible for chat_offset backfill", "err", err)
		}

		// Backfill from player_prefs: copy chat_offset to jobs using video_id match.
		res, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET chat_offset = (
			SELECT pp.chat_offset FROM player_prefs pp WHERE pp.video_id = jobs.video_id
		) WHERE EXISTS (
			SELECT 1 FROM player_prefs pp WHERE pp.video_id = jobs.video_id
		)`)
		if err != nil {
			if db.logger != nil {
				db.logger.Warn("v9 migration: player_prefs backfill failed (non-fatal)", "err", err)
			}
		} else if db.logger != nil {
			// A row-count mismatch almost always indicates data corruption or a
			// migration bug. Log at ERROR (keeping the migration non-fatal so
			// operators can still boot and investigate).
			if affected, aerr := res.RowsAffected(); aerr == nil && affected != expected {
				db.logger.Error("v9 migration: chat_offset backfill row count mismatch",
					"expected", expected, "affected", affected)
			}
		}

		if err := db.writeUserVersion(9); err != nil {
			return err
		}
	}

	if version < 10 {
		// player_prefs was superseded by jobs.chat_offset in v9. Drop the now-dead table.
		if _, err := db.db.ExecContext(db.getCtx(), `DROP TABLE IF EXISTS player_prefs`); err != nil {
			return err
		}

		err := db.writeUserVersion(10)
		if err != nil {
			return err
		}
	}

	if version < 11 {
		// Schema versioning moved to PRAGMA user_version. Drop the legacy
		// schema_version table now that readUserVersion is authoritative.
		if _, err := db.db.ExecContext(db.getCtx(), `DROP TABLE IF EXISTS schema_version`); err != nil {
			return err
		}

		if err := db.writeUserVersion(11); err != nil {
			return err
		}
	}

	if version < 12 {
		// pruneHistory ORDERs by added_at and DELETEs via a subquery on every
		// AddToHistory. Without an index on added_at the subquery is a full
		// table scan. CREATE INDEX IF NOT EXISTS is idempotent on retry.
		if _, err := db.db.ExecContext(db.getCtx(), `CREATE INDEX IF NOT EXISTS idx_history_added_at ON history(added_at)`); err != nil {
			return err
		}

		if err := db.writeUserVersion(12); err != nil {
			return err
		}
	}

	if version < 13 {
		// auto_retry_count tracks how many times the monitor's auto-recovery
		// has re-enqueued this job after a transient "twitch channel is offline"
		// flap. Capped at 2 (see worker.MaxTwitchAutoRetries) so persistent
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

	if version < 14 {
		// (a) The TEXT columns added by v2/v3 carry no DEFAULT, so rows that
		// predate those migrations (and weren't backfilled) hold NULL — which
		// scanJob reads into plain strings, erroring the row out of every
		// query and making the job invisible. Normalize to ''.
		//
		// Identifier concatenation is safe here for the same reason as the
		// v3 ALTER above: hardcoded compile-time literals only.
		for _, col := range []string{"chat_file", "thumbnail_file", "description_file"} {
			if _, err := db.db.ExecContext(db.getCtx(), `UPDATE jobs SET `+col+` = '' WHERE `+col+` IS NULL`); err != nil {
				return err
			}
		}

		// (b) Foreign-key enforcement is now actually enabled (the old DSN's
		// mattn-style parameters were silently ignored by modernc/sqlite),
		// which makes the child tables' ON DELETE CASCADE live going
		// forward. Sweep the orphans accumulated while enforcement was off —
		// job IDs are video IDs, so re-adding a previously-deleted video
		// would otherwise resurrect the old job's gaps/trims/segments onto
		// the new job.
		for _, table := range []string{"gaps", "trims", "segments"} {
			if _, err := db.db.ExecContext(db.getCtx(), `DELETE FROM `+table+` WHERE job_id NOT IN (SELECT id FROM jobs)`); err != nil {
				return err
			}
		}

		if err := db.writeUserVersion(14); err != nil {
			return err
		}
	}

	if version < 15 {
		// Per-part chat files: Twitch live jobs split into multiple output
		// parts (gap/quality splits), each with its own chat file recorded
		// on the segment row. Empty string for segments muxed before this
		// feature or without chat.
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE segments ADD COLUMN chat_file TEXT NOT NULL DEFAULT ''`); err != nil {
			if !isDuplicateColumnErr(err) {
				return err
			}
		}

		if err := db.writeUserVersion(15); err != nil {
			return err
		}
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
