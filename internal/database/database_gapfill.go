package database

import (
	"database/sql"
	"errors"
)

// GapFill is one twitch_gap_fills row: a finished live Twitch job owed a
// gap-fill from its archive VOD (owner decision D17). Plan is the worker's
// JSON, opaque here; NextCheckAt is RFC3339 UTC.
type GapFill struct {
	JobID       string
	Plan        string
	NextCheckAt string
}

// SaveGapFill inserts or replaces jobID's row.
func (db *Database) SaveGapFill(g GapFill) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(),
		`INSERT INTO twitch_gap_fills (job_id, plan, next_check_at) VALUES (?, ?, ?)
		 ON CONFLICT(job_id) DO UPDATE SET plan = excluded.plan, next_check_at = excluded.next_check_at`,
		g.JobID, g.Plan, g.NextCheckAt)
	return err
}

// DeleteGapFill removes jobID's row; a row that is not there is no error.
func (db *Database) DeleteGapFill(jobID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(), `DELETE FROM twitch_gap_fills WHERE job_id = ?`, jobID)
	return err
}

// GetGapFill returns jobID's row, nil when it has none.
func (db *Database) GetGapFill(jobID string) (*GapFill, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT job_id, plan, next_check_at FROM twitch_gap_fills WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var g GapFill
	if err := rows.Scan(&g.JobID, &g.Plan, &g.NextCheckAt); err != nil {
		return nil, err
	}
	return &g, nil
}

// DueGapFills returns the rows whose next_check_at is at or before now
// (RFC3339 UTC, which orders as text), soonest first.
func (db *Database) DueGapFills(now string) ([]GapFill, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT job_id, plan, next_check_at FROM twitch_gap_fills WHERE next_check_at <= ? ORDER BY next_check_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GapFill
	for rows.Next() {
		var g GapFill
		if err := rows.Scan(&g.JobID, &g.Plan, &g.NextCheckAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ReplaceJobSegmentsIf is ReplaceJobSegments applied only while jobID's status
// is still expected, checked inside the same transaction that replaces the
// rows: the gap-fill's commit (owner decision D17), decided on a Finished row
// it read minutes earlier, must not write its parts over a row an operator has
// since re-run, reset or deleted. applied is false, and nothing is written,
// when the row is gone or its status is another.
func (db *Database) ReplaceJobSegmentsIf(jobID string, expected JobStatus, segs []Segment) (applied bool, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status string
	switch err := tx.QueryRowContext(ctx, "SELECT status FROM jobs WHERE id = ?", jobID).Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	if JobStatus(status) != expected {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM segments WHERE job_id = ?", jobID); err != nil {
		return false, err
	}
	for i := range segs {
		if err := insertSegmentExec(ctx, tx, jobID, &segs[i]); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
