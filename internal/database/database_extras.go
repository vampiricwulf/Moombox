package database

import (
	"database/sql"
	"strings"
	"time"
)

// HasProcessed checks if a video ID has been previously processed.
func (db *Database) HasProcessed(videoID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var one int
	err := db.db.QueryRowContext(db.getCtx(), "SELECT 1 FROM history WHERE video_id = ? LIMIT 1", videoID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
	_, err := db.db.ExecContext(db.getCtx(), `DELETE FROM history WHERE video_id IN (
		SELECT video_id FROM history ORDER BY added_at ASC
		LIMIT MAX(0, (SELECT COUNT(*) FROM history) - 10000)
	)`)
	return err
}

// OrphanedHistoryEntry is a processing-history row whose job no longer exists —
// the job was deleted, or the video was skipped and never jobbed. While the row
// remains, the monitor treats the video as already-processed (HasProcessed) and
// won't re-discover it, even though nothing is tracking it anymore. Removing the
// row lets the video be picked up again.
type OrphanedHistoryEntry struct {
	VideoID string `json:"videoId"`
	AddedAt string `json:"addedAt"`
}

// ListOrphanedHistory returns processing-history rows that have no matching job,
// newest first. The NOT-NULL / non-empty guard on the subquery avoids the
// NULL-in-NOT-IN pitfall (a single NULL in the list would otherwise make NOT IN
// match nothing).
func (db *Database) ListOrphanedHistory() ([]OrphanedHistoryEntry, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT video_id, added_at FROM history
		 WHERE video_id NOT IN (SELECT video_id FROM jobs WHERE video_id IS NOT NULL AND video_id != '')
		 ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OrphanedHistoryEntry
	for rows.Next() {
		var e OrphanedHistoryEntry
		if err := rows.Scan(&e.VideoID, &e.AddedAt); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteHistoryEntries removes the given video IDs from the processing history
// and returns the number of rows deleted. Batched to stay under SQLite's bound
// on parameters per statement.
func (db *Database) DeleteHistoryEntries(videoIDs []string) (int, error) {
	if len(videoIDs) == 0 {
		return 0, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	total := 0
	const chunk = 500
	for start := 0; start < len(videoIDs); start += chunk {
		end := min(start+chunk, len(videoIDs))
		batch := videoIDs[start:end]

		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		res, err := db.db.ExecContext(db.getCtx(),
			"DELETE FROM history WHERE video_id IN ("+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
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

// --- Client Token CRUD ---

// AddClientToken inserts a new client token.
func (db *Database) AddClientToken(ct *ClientToken) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(),
		`INSERT INTO client_tokens (id, token_prefix, token_hash, label, created_at, last_used_at, last_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ct.ID, ct.TokenPrefix, ct.TokenHash, ct.Label, ct.CreatedAt, ct.LastUsedAt, ct.LastIP)
	return err
}

// GetClientTokenByPrefix returns the client token matching the given prefix, or nil.
func (db *Database) GetClientTokenByPrefix(prefix string) (*ClientToken, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var ct ClientToken
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT id, token_prefix, token_hash, label, created_at, last_used_at, last_ip
		FROM client_tokens WHERE token_prefix = ?`, prefix).Scan(
		&ct.ID, &ct.TokenPrefix, &ct.TokenHash, &ct.Label, &ct.CreatedAt, &ct.LastUsedAt, &ct.LastIP)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &ct, nil
}

// ListClientTokens returns all client tokens, newest first.
func (db *Database) ListClientTokens() ([]*ClientToken, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT id, token_prefix, token_hash, label, created_at, last_used_at, last_ip
		FROM client_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []*ClientToken
	for rows.Next() {
		var ct ClientToken
		if err := rows.Scan(&ct.ID, &ct.TokenPrefix, &ct.TokenHash, &ct.Label,
			&ct.CreatedAt, &ct.LastUsedAt, &ct.LastIP); err != nil {
			continue
		}
		tokens = append(tokens, &ct)
	}
	return tokens, rows.Err()
}

// UpdateClientTokenUsage updates the last-used timestamp and IP for a token.
func (db *Database) UpdateClientTokenUsage(id, ip string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.db.ExecContext(db.getCtx(),
		`UPDATE client_tokens SET last_used_at = ?, last_ip = ? WHERE id = ?`,
		now, ip, id)
	return err
}

// DeleteClientToken removes a single client token.
func (db *Database) DeleteClientToken(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), `DELETE FROM client_tokens WHERE id = ?`, id)
	return err
}

// DeleteAllClientTokens removes all client tokens.
func (db *Database) DeleteAllClientTokens() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(), `DELETE FROM client_tokens`)
	return err
}
