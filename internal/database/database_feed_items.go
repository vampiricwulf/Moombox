package database

import (
	"database/sql"
	"fmt"
)

type FeedItem struct {
	ChannelID, VideoID, Title string
	Published, DatePrecision  string
	CatalogPos                int
	Source, Status            string
	FirstSeen                 string
}

// UpsertFeedItem is the §6 upsert. status is ALWAYS written 'unknown' on insert
// and never touched on conflict — a listing supplies a date, never a
// classification. source/catalog_pos are ALWAYS arms; published/date_precision
// move only upward via the ladder. insertedNow is derived from RETURNING
// first_seen: equal to it.FirstSeen ⇒ this call inserted the row.
func (db *Database) UpsertFeedItem(it FeedItem) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	newRank, oldRank := precisionRankCaseSQL("excluded.date_precision"), precisionRankCaseSQL("feed_items.date_precision")
	q := fmt.Sprintf(`INSERT INTO feed_items
  (channel_id, video_id, title, published, date_precision, catalog_pos, source, status, first_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, 'unknown', ?)
ON CONFLICT(channel_id, video_id) DO UPDATE SET
  source = excluded.source,
  catalog_pos = excluded.catalog_pos,
  published = CASE WHEN %[1]s > %[2]s THEN excluded.published ELSE feed_items.published END,
  date_precision = CASE WHEN %[1]s > %[2]s THEN excluded.date_precision ELSE feed_items.date_precision END,
  title = CASE WHEN excluded.title <> '' AND excluded.title <> 'Unknown Title' THEN excluded.title ELSE feed_items.title END
RETURNING first_seen`, newRank, oldRank)
	var firstSeen string
	err := db.db.QueryRowContext(db.getCtx(), q,
		it.ChannelID, it.VideoID, it.Title, it.Published, it.DatePrecision,
		it.CatalogPos, it.Source, it.FirstSeen).Scan(&firstSeen)
	if err != nil {
		return false, fmt.Errorf("upsert feed_item: %w", err)
	}
	return firstSeen == it.FirstSeen, nil
}

// ApplyProbeToFeedItem is the §6 probe write — a SEPARATE statement sharing the
// one ladder. It sets status (caller has already normalized post_live→vod and
// enforced the terminal invariant), refreshes title, and upgrades the date.
// It never touches source or catalog_pos. Dateless probes pass published=""
// precision="": rank is forced to 0 in Go so it can never win the guard, even
// against 'assumed' (which ranks 1).
func (db *Database) ApplyProbeToFeedItem(channelID, videoID, status, title, published, precision string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// rank 0 for "no date supplied" so it can never win, even against 'assumed'.
	rank := 0
	if published != "" {
		rank = PrecisionRank(precision)
	}
	q := fmt.Sprintf(`UPDATE feed_items SET
  status = ?,
  title = CASE WHEN ? <> '' AND ? <> 'Unknown Title' THEN ? ELSE title END,
  published = CASE WHEN ? > %[1]s THEN ? ELSE published END,
  date_precision = CASE WHEN ? > %[1]s THEN ? ELSE date_precision END
WHERE channel_id = ? AND video_id = ?`, precisionRankCaseSQL("date_precision"))
	_, err := db.db.ExecContext(db.getCtx(), q,
		status, title, title, title,
		rank, published, rank, precision,
		channelID, videoID)
	return err
}

// GetFeedItem returns the feed_items row for (channelID, videoID), or nil (no
// error) if no such row exists. A small exported read path alongside the
// upsert/probe writers — used by tests and (Plan 5) the history API.
func (db *Database) GetFeedItem(channelID, videoID string) (*FeedItem, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	it := &FeedItem{ChannelID: channelID, VideoID: videoID}
	err := db.db.QueryRowContext(db.getCtx(), `SELECT title, published, date_precision, catalog_pos, source, status, first_seen
  FROM feed_items WHERE channel_id = ? AND video_id = ?`, channelID, videoID).Scan(
		&it.Title, &it.Published, &it.DatePrecision, &it.CatalogPos, &it.Source, &it.Status, &it.FirstSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return it, nil
}

// SetFeedItemSource is the refusal's write (§9).
func (db *Database) SetFeedItemSource(channelID, videoID, source string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(),
		`UPDATE feed_items SET source = ? WHERE channel_id = ? AND video_id = ?`,
		source, channelID, videoID)
	return err
}

func feedScopeQ1SQL(includeMembership bool) string {
	q := `SELECT video_id, title, published, date_precision, catalog_pos, source, status, first_seen
  FROM feed_items WHERE channel_id = ? AND published >= ?`
	if !includeMembership {
		q += ` AND source <> 'membership'`
	}
	return q + ` ORDER BY published DESC, catalog_pos ASC, video_id ASC`
}

func feedScopeQ2SQL(includeMembership bool) string {
	// Fetch three statuses via the index; the unresolved-arm precision filter is
	// applied in Go (spec §7: post-filter unknown rows to assumed-only).
	q := `SELECT video_id, title, published, date_precision, catalog_pos, source, status, first_seen
  FROM feed_items WHERE channel_id = ? AND status IN ('upcoming','live','unknown')`
	if !includeMembership {
		q += ` AND source <> 'membership'`
	}
	return q
}

// FeedScope returns Q1 ∪ Q2 (spec §7), deduped by video_id, in Q1's order with
// Q2-only rows appended. includeMembership is MembershipDiscoveryEnabled() —
// the operator's toggle, NEVER membershipActive() (cookie state must not move scope).
func (db *Database) FeedScope(channelID, cutoff string, includeMembership bool) ([]FeedItem, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	scan := func(q string, args ...any) ([]FeedItem, error) {
		rows, err := db.db.QueryContext(db.getCtx(), q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []FeedItem
		for rows.Next() {
			it := FeedItem{ChannelID: channelID}
			if err := rows.Scan(&it.VideoID, &it.Title, &it.Published, &it.DatePrecision,
				&it.CatalogPos, &it.Source, &it.Status, &it.FirstSeen); err != nil {
				return nil, err
			}
			out = append(out, it)
		}
		return out, rows.Err()
	}
	q1, err := scan(feedScopeQ1SQL(includeMembership), channelID, cutoff)
	if err != nil {
		return nil, err
	}
	q2, err := scan(feedScopeQ2SQL(includeMembership), channelID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(q1))
	for _, it := range q1 {
		seen[it.VideoID] = true
	}
	for _, it := range q2 {
		// unresolved arm: unknown rows qualify only at 'assumed' precision (§7).
		if it.Status == "unknown" && it.DatePrecision != "assumed" {
			continue
		}
		if !seen[it.VideoID] {
			q1 = append(q1, it)
			seen[it.VideoID] = true
		}
	}
	return q1, nil
}

// HasAnyJob reports whether a jobs row exists for videoID, in ANY status (§8 restart).
// Relies on jobs.id == jobs.video_id — true at every production creation site today,
// but not schema-enforced; the restart carve-out depends on it.
func (db *Database) HasAnyJob(videoID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var one int
	err := db.db.QueryRowContext(db.getCtx(), `SELECT 1 FROM jobs WHERE id = ? LIMIT 1`, videoID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// SetChannelRSSOK upserts last_rss_ok_at (§6: neither writer depends on the other).
func (db *Database) SetChannelRSSOK(channelID, ts string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO channel_state (channel_id, last_rss_ok_at)
VALUES (?, ?) ON CONFLICT(channel_id) DO UPDATE SET last_rss_ok_at = excluded.last_rss_ok_at`, channelID, ts)
	return err
}

// GetChannelRSSOK returns channel_state.last_rss_ok_at for channelID, or "" if
// unset or the channel_state row doesn't exist yet — mirrors SetChannelRSSOK.
func (db *Database) GetChannelRSSOK(channelID string) (string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var ts sql.NullString
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT last_rss_ok_at FROM channel_state WHERE channel_id = ?`, channelID).Scan(&ts)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ts.String, nil
}

// SaveBackfillCursor upserts channel_state.backfill_state — the backfill's
// per-tab resume cursor JSON (spec §11), saved by the scanner after every
// good page. Mirrors SetChannelRSSOK's upsert shape (§6: neither
// channel_state writer depends on the other having run, and nothing creates
// rows up front).
func (db *Database) SaveBackfillCursor(channelID, stateJSON string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO channel_state (channel_id, backfill_state)
VALUES (?, ?) ON CONFLICT(channel_id) DO UPDATE SET backfill_state = excluded.backfill_state`, channelID, stateJSON)
	return err
}

// LoadBackfillCursor returns channel_state.backfill_state for channelID, or
// "" when unset or the channel_state row doesn't exist yet — either way a
// fresh scan starts from page 1 of every tab. Mirrors GetChannelRSSOK.
func (db *Database) LoadBackfillCursor(channelID string) (string, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var state sql.NullString
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT backfill_state FROM channel_state WHERE channel_id = ?`, channelID).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return state.String, nil
}

// FeedOrderRow is the §11 ordering pass's read shape: the sort-key
// coordinates of one feed_items row — published, the provisional (per-tab)
// catalog_pos the scan wrote, and video_id as the final tie-break.
type FeedOrderRow struct {
	VideoID    string
	Published  string
	CatalogPos int
}

// ListFeedOrderRows returns the sort-key coordinates of ALL of channelID's
// feed_items rows, fully collected before returning. The §11 ordering pass
// must collect-then-update: with SetMaxOpenConns(1), issuing UPDATEs under an
// open SELECT cursor deadlocks on the single connection. FeedScope is not a
// substitute reader — it is window-bounded, and the renumber must cover every
// row the channel has.
func (db *Database) ListFeedOrderRows(channelID string) ([]FeedOrderRow, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	rows, err := db.db.QueryContext(db.getCtx(),
		`SELECT video_id, published, catalog_pos FROM feed_items WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedOrderRow
	for rows.Next() {
		var r FeedOrderRow
		if err := rows.Scan(&r.VideoID, &r.Published, &r.CatalogPos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RenumberCatalog writes catalog_pos = 0..n-1 over orderedVideoIDs, in order,
// in ONE transaction (§11 ordering pass; BeginTx idiom per
// DeleteChannelFeedData). The CALLER — scanChannel's completion path — has
// already collected the rows, closed its cursor, and sorted (collect-then-
// update; see ListFeedOrderRows). The UPDATE is deliberately unguarded: the
// precision ladder governs the upsert only, and would reject writing a coarse
// row's new position onto an exact RSS row.
func (db *Database) RenumberCatalog(chID string, orderedVideoIDs []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE feed_items SET catalog_pos = ? WHERE channel_id = ? AND video_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range orderedVideoIDs {
		if _, err := stmt.ExecContext(ctx, i, chID, id); err != nil {
			return fmt.Errorf("renumber %s pos %d: %w", id, i, err)
		}
	}
	return tx.Commit()
}

// SetChannelBackfilled writes the §11 completion record — backfilled_at,
// backfilled_window_days (the resolved archive_window_days the scan ran at)
// and backfilled_with_membership (whether /membership was eligible for the
// scan) — and clears backfill_state in the same statement: the cursor's
// lifecycle ends at completion (§11 — a completed scan's stale continuation
// token must not leak into the next one). Upsert shape per SetChannelRSSOK;
// last_rss_ok_at is untouched (§6: neither writer depends on the other).
func (db *Database) SetChannelBackfilled(chID string, windowDays int, withMembership bool, ts string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	m := 0
	if withMembership {
		m = 1
	}
	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO channel_state
  (channel_id, backfilled_at, backfilled_window_days, backfilled_with_membership, backfill_state)
VALUES (?, ?, ?, ?, NULL)
ON CONFLICT(channel_id) DO UPDATE SET
  backfilled_at = excluded.backfilled_at,
  backfilled_window_days = excluded.backfilled_window_days,
  backfilled_with_membership = excluded.backfilled_with_membership,
  backfill_state = NULL`, chID, ts, windowDays, m)
	return err
}

// ChannelBackfill is the completion record's read shape — the three
// backfilled_* columns of channel_state. Zero values mean NULL (or a missing
// row): At "" ⇒ the channel never completed a backfill. Read by tests and by
// the sweep's arms (Task 4).
type ChannelBackfill struct {
	At             string // backfilled_at, "" when NULL
	WindowDays     *int   // backfilled_window_days, nil when NULL
	WithMembership *bool  // backfilled_with_membership, nil when NULL
}

// GetChannelBackfill returns channelID's completion record. A missing
// channel_state row reads the same as an all-NULL one — never backfilled.
func (db *Database) GetChannelBackfill(channelID string) (ChannelBackfill, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var cb ChannelBackfill
	var at sql.NullString
	var wd, wm sql.NullInt64
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT backfilled_at, backfilled_window_days, backfilled_with_membership
		   FROM channel_state WHERE channel_id = ?`, channelID).Scan(&at, &wd, &wm)
	if err == sql.ErrNoRows {
		return cb, nil
	}
	if err != nil {
		return cb, err
	}
	cb.At = at.String
	if wd.Valid {
		v := int(wd.Int64)
		cb.WindowDays = &v
	}
	if wm.Valid {
		v := wm.Int64 != 0
		cb.WithMembership = &v
	}
	return cb, nil
}

// GetChannelEstablished reads the §11 established gate for channelID:
// last_rss_ok_at IS NOT NULL OR backfilled_at IS NOT NULL. A missing
// channel_state row is NOT established — a fresh install whose first RSS
// cycle 404s must not treat its membership items as the whole channel.
func (db *Database) GetChannelEstablished(channelID string) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var one int
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT 1 FROM channel_state
		  WHERE channel_id = ? AND (last_rss_ok_at IS NOT NULL OR backfilled_at IS NOT NULL)`,
		channelID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// DeleteChannelFeedData deletes feed_items + channel_state rows for channelID.
// Both DELETEs run in one transaction: a crash between independent commits
// would leave an orphaned channel_state row (stale backfilled_at) for a
// channel with no feed_items — a re-added channel would then be treated as
// already-backfilled and skip its rescan.
func (db *Database) DeleteChannelFeedData(channelID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	ctx := db.getCtx()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM feed_items WHERE channel_id = ?`, channelID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_state WHERE channel_id = ?`, channelID); err != nil {
		return err
	}
	return tx.Commit()
}
