package database

import "database/sql"

// backfillTwitchOfflineParkReasons gives the Error rows an earlier release
// wrote with one of the two Twitch-offline error texts, and no park_reason,
// the reason a current writer records beside that text (owner ruling R-I1:
// ParkReasonTwitchOffline, ParkReasonTwitchEndedOffline). One statement, run
// by migrate on every Open of an existing database; a fresh one has no rows.
//
// It is a data backfill with NO schema-version bump, deliberately:
//
//   - Nothing about the schema changes — park_reason has existed since v18 —
//     and a bump would raise migrate's downgrade guard for a change every
//     earlier binary reads correctly: the last release keys the offline flap
//     on the error text, which the backfill leaves as it is (and reads the
//     ended-offline text nowhere), and compares park_reason only for
//     equality with the values it knows, so a value it has never seen reads
//     as "none of mine".
//   - A version block runs once. A row written afterwards by an older binary
//     on this database (a rollback that ran for a while, then the update
//     again) would keep the bare text, and once the readers' one-release
//     text fallback is gone nothing would route it. Run on every Open, the
//     backfill converts it at the next boot.
//
// Exact: status Error, an empty park_reason, error equal to one of the frozen
// texts. A row another reason claimed, a row no longer in Error (a Cancel and
// the Mux's own Muxing write leave the text on it) and any other wording are
// left alone. updated_at is not touched and no subscriber is told: at boot
// nothing is subscribed, and the one other caller of Open, the `moombox add`
// side process, converts under a running daemon only rows whose two shapes
// that daemon reads alike. Idempotent, and a failure is logged, not fatal —
// the row is read by its text this release and converted on a later Open
// (`moombox add` opens with no logger, so there it is silent).
//
// The UPDATE is sent only when a read finds a row to convert. An UPDATE that
// matches nothing still opens a write transaction, and every Open but the
// first after the update has nothing to convert: `moombox add` would wait on
// the running daemon's writer, and take the write lock from it, each time it
// ran. A row written after a read that found none is converted by the next
// Open.
func (db *Database) backfillTwitchOfflineParkReasons() {
	var pending bool
	err := db.db.QueryRowContext(db.getCtx(),
		`SELECT EXISTS(SELECT 1 FROM jobs WHERE status = ? AND park_reason = '' AND error IN (?, ?))`,
		StatusError, LegacyTwitchOfflineError, LegacyTwitchEndedOfflineError).Scan(&pending)
	if err == nil && !pending {
		return
	}
	var res sql.Result
	if err == nil {
		res, err = db.db.ExecContext(db.getCtx(),
			`UPDATE jobs SET park_reason = CASE error WHEN ? THEN ? ELSE ? END
			 WHERE status = ? AND park_reason = '' AND error IN (?, ?)`,
			LegacyTwitchOfflineError, ParkReasonTwitchOffline, ParkReasonTwitchEndedOffline,
			StatusError, LegacyTwitchOfflineError, LegacyTwitchEndedOfflineError)
	}
	if err != nil {
		if db.logger == nil {
			return
		}
		db.logger.Warn("twitch offline park_reason backfill failed; the rows are read by their error text until a later start converts them", "err", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 && db.logger != nil {
		db.logger.Info("recorded the park reason on Twitch offline Error rows written by an earlier release", "rows", n)
	}
}
