# Feed History 1/5 — Store & Config Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Schema v16 (`feed_items`, `channel_state`, new `jobs` columns), the store API every later plan consumes, the two new config settings replacing `max_feed_items`, and the `last_videos` removal.

**Architecture:** All new persistence lives in `internal/database` as methods on the existing `*Database` (mutex + `getCtx()` pattern, `SetMaxOpenConns(1)` aware). Config follows the existing two-level pattern (`Monitors.<X>` global + `ChannelConfig.<X> *int` override). The precision ladder is defined ONCE in Go and rendered into both SQL statements from it.

**Tech Stack:** Go 1.25, modernc.org/sqlite (no CGo), BurntSushi/toml.

**Spec:** `docs/superpowers/specs/2026-07-15-feed-history.md` (§5 config, §6 schema/upsert/probe-write, §7 Q1/Q2, §16 migration). The spec is authoritative; on any conflict, the spec wins and the discrepancy gets reported.

## Global Constraints (all 5 plans)

- Go 1.25, module `github.com/vampiricwulf/Moombox`; pure Go, **no CGo**.
- Every goroutine gets an inline `defer func(){ if r := recover(); ... }()` (CLAUDE.md).
- DB migrations: sequential `if version < 16 { ... return db.writeUserVersion(16) }` in `internal/database/migrations.go` (current `schemaVersion = 15` at `:26`); new tables ALSO added to `createSchema`; `ALTER TABLE` guarded by the `isDuplicateColumnErr` pattern (`migrations.go:234-238`); never UPDATE while a SELECT cursor is open (`SetMaxOpenConns(1)`, hazard documented at `migrations.go:242-244`).
- Test fixtures stay **inline** — no `testdata/` in `internal/monitor/` or `internal/database/`.
- After every task: `go build ./... && go vet ./... && go test ./...` must pass.
- Date/precision ladder: `assumed(1) < coarse(2) < day(3) < exact(4) < started(5)`; unrecognised strings rank 1 (fail-closed).
- Job statuses (`internal/database/types.go:7-16`): `Upcoming, Live, Downloading, Muxing, Finished, Error, Cancelled, COOKIES?` — plan 4 adds `Queued`. `IsTerminal()` = Finished/Error/Cancelled (`types.go:92-94`).

---

### Task 1: Schema v16 migration

**Files:**
- Modify: `internal/database/migrations.go` (`schemaVersion` at `:26`; append v16 block after the v15 block; extend `createSchema`)
- Test: `internal/database/migrations_v16_test.go` (create)

**Interfaces:**
- Consumes: existing `writeUserVersion`, `isDuplicateColumnErr`, `createSchema`.
- Produces: tables `feed_items`, `channel_state`; indexes `idx_feed_items_window`, `idx_feed_items_status`; columns `jobs.channel_id TEXT`, `jobs.queue_priority INTEGER NOT NULL DEFAULT 1`; `last_videos` dropped.

- [ ] **Step 1: Find the test-DB helper used by existing tests**

Run: `grep -n "func newTest\|func openTest\|func setupTest" internal/database/database_test.go | head -3`
Expected: one helper that returns a ready `*Database` against a temp file. Use that helper name wherever this plan writes `newTestDB(t)`.

- [ ] **Step 2: Write the failing migration test**

```go
package database

import "testing"

func TestMigrationV16(t *testing.T) {
	db := newTestDB(t)

	// Tables and indexes exist.
	for _, name := range []string{"feed_items", "channel_state"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", name, n, err)
		}
	}
	for _, name := range []string{"idx_feed_items_window", "idx_feed_items_status"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing", name)
		}
	}

	// last_videos is gone.
	var n int
	db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='last_videos'`).Scan(&n)
	if n != 0 {
		t.Fatal("last_videos still exists")
	}

	// jobs gained channel_id (NULL) and queue_priority (DEFAULT 1) — insert a
	// legacy-shaped row without either column and read the defaults back.
	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy1','legacy1','u','t','Finished','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var qp int
	var chID any
	if err := db.db.QueryRow(`SELECT queue_priority, channel_id FROM jobs WHERE id='legacy1'`).Scan(&qp, &chID); err != nil {
		t.Fatal(err)
	}
	if qp != 1 {
		t.Fatalf("queue_priority default = %d, want 1 (spec §6: DEFAULT 1 is fail-closed for the M count)", qp)
	}
	if chID != nil {
		t.Fatalf("channel_id = %v, want NULL on legacy rows", chID)
	}
}

func TestMigrationV16Idempotent(t *testing.T) {
	db := newTestDB(t)
	// Re-running the block must be a no-op, not an error (crash-mid-block re-runs it).
	if err := db.migrateV16(); err != nil {
		t.Fatalf("second run: %v", err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test -run TestMigrationV16 ./internal/database/ -v`
Expected: FAIL — `feed_items` missing / `migrateV16` undefined.

- [ ] **Step 4: Implement the migration**

In `migrations.go`: bump `const schemaVersion = 16`. Append after the v15 block:

```go
if version < 16 {
	if err := db.migrateV16(); err != nil {
		return err
	}
	return db.writeUserVersion(16)
}
```

Add (SQL verbatim from spec §6 — comments included, they are the design record):

```go
const createFeedItemsSQL = `
CREATE TABLE IF NOT EXISTS feed_items (
    channel_id     TEXT NOT NULL,
    video_id       TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    published      TEXT NOT NULL,   -- RFC3339 UTC. Frozen at insert; only upgraded
    date_precision TEXT NOT NULL,   -- assumed|coarse|day|exact|started
    catalog_pos    INTEGER NOT NULL DEFAULT 0,
    source         TEXT NOT NULL,   -- rss|membership|videos|streams
    status         TEXT NOT NULL,   -- unknown|upcoming|live|vod|not_a_stream
    first_seen     TEXT NOT NULL,
    PRIMARY KEY (channel_id, video_id)
)`

const createFeedItemsWindowIdxSQL = `
CREATE INDEX IF NOT EXISTS idx_feed_items_window
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC)`

const createFeedItemsStatusIdxSQL = `
CREATE INDEX IF NOT EXISTS idx_feed_items_status
    ON feed_items(channel_id, status)`

const createChannelStateSQL = `
CREATE TABLE IF NOT EXISTS channel_state (
    channel_id                 TEXT PRIMARY KEY,
    backfilled_at              TEXT,
    backfilled_window_days     INTEGER,
    backfilled_with_membership INTEGER,
    backfill_state             TEXT,
    last_rss_ok_at             TEXT
)`

func (db *Database) migrateV16() error {
	ctx := db.getCtx()
	for _, q := range []string{
		createFeedItemsSQL, createFeedItemsWindowIdxSQL, createFeedItemsStatusIdxSQL,
		createChannelStateSQL,
		`DROP TABLE IF EXISTS last_videos`,
	} {
		if _, err := db.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("v16: %w", err)
		}
	}
	// Guarded ALTERs: a crash mid-block re-runs the whole block (user_version is
	// written last), so duplicate-column errors are expected and benign.
	for _, q := range []string{
		`ALTER TABLE jobs ADD COLUMN channel_id TEXT`,
		`ALTER TABLE jobs ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 1`,
	} {
		if _, err := db.db.ExecContext(ctx, q); err != nil && !isDuplicateColumnErr(err) {
			return fmt.Errorf("v16 alter: %w", err)
		}
	}
	return nil
}
```

Add the four `CREATE` statements to `createSchema` too (fresh installs), and the two job columns to the fresh-install `jobs` DDL there.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -run TestMigrationV16 ./internal/database/ -v` — Expected: PASS. Then `go test ./internal/database/` — expected: FAIL only in `TestLastVideos` (removed in Task 5; if anything else fails, stop and fix).

- [ ] **Step 6: Commit** — `git commit -m "feat(db): schema v16 — feed_items, channel_state, jobs.channel_id/queue_priority"`

---

### Task 2: The precision ladder — one definition

**Files:**
- Create: `internal/database/feed_precision.go`
- Test: `internal/database/feed_precision_test.go`

**Interfaces:**
- Produces: `database.PrecisionRank(p string) int`; `precisionRankCaseSQL(expr string) string` (package-private, used by Task 3's two statements).

- [ ] **Step 1: Write the failing test**

```go
package database

import "testing"

func TestPrecisionRank(t *testing.T) {
	// Spec §6/§12: assumed < coarse < day < exact < started; unknown strings rank
	// as assumed (fail-closed: a typo loses to everything rather than winning).
	want := map[string]int{"assumed": 1, "coarse": 2, "day": 3, "exact": 4, "started": 5, "": 1, "exactt": 1}
	for p, w := range want {
		if got := PrecisionRank(p); got != w {
			t.Errorf("PrecisionRank(%q) = %d, want %d", p, got, w)
		}
	}
}

func TestPrecisionRankCaseSQLMatchesGo(t *testing.T) {
	db := newTestDB(t)
	for _, p := range []string{"assumed", "coarse", "day", "exact", "started", "bogus"} {
		var sqlRank int
		q := `SELECT ` + precisionRankCaseSQL("?")
		if err := db.db.QueryRow(q, p).Scan(&sqlRank); err != nil {
			t.Fatal(err)
		}
		if sqlRank != PrecisionRank(p) {
			t.Errorf("SQL rank(%q)=%d, Go rank=%d — the ladder MUST have one definition", p, sqlRank, PrecisionRank(p))
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test -run TestPrecisionRank ./internal/database/ -v` → FAIL (undefined).

- [ ] **Step 3: Implement**

```go
package database

// PrecisionRank orders the date-precision ladder (spec §12). One definition:
// both SQL statements render their CASE from precisionRankCaseSQL below.
// Unrecognised strings rank as 'assumed' — fail-closed (spec §6).
func PrecisionRank(p string) int {
	switch p {
	case "started":
		return 5
	case "exact":
		return 4
	case "day":
		return 3
	case "coarse":
		return 2
	default:
		return 1
	}
}

// precisionRankCaseSQL renders the ladder as a SQL CASE over expr.
func precisionRankCaseSQL(expr string) string {
	return "CASE " + expr +
		" WHEN 'started' THEN 5 WHEN 'exact' THEN 4 WHEN 'day' THEN 3" +
		" WHEN 'coarse' THEN 2 ELSE 1 END"
}
```

- [ ] **Step 4: Verify pass** — `go test -run TestPrecisionRank ./internal/database/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(db): precision ladder, defined once"`

---

### Task 3: The store API

**Files:**
- Create: `internal/database/database_feed_items.go`
- Test: `internal/database/database_feed_items_test.go`

**Interfaces (every later plan consumes these — exact signatures):**

```go
type FeedItem struct {
	ChannelID, VideoID, Title           string
	Published, DatePrecision            string // RFC3339 UTC / ladder value
	CatalogPos                          int
	Source, Status                      string
	FirstSeen                           string // RFC3339; caller passes the cycle instant
}
func (db *Database) UpsertFeedItem(it FeedItem) (insertedNow bool, err error)
func (db *Database) ApplyProbeToFeedItem(channelID, videoID, status, title, published, precision string) error
func (db *Database) SetFeedItemSource(channelID, videoID, source string) error   // the refusal's write (§9)
func (db *Database) FeedScope(channelID, cutoff string, includeMembership bool) ([]FeedItem, error) // Q1 ∪ Q2, Q1 order, deduped
func (db *Database) HasAnyJob(videoID string) (bool, error)                      // jobs row exists, ANY status (§8 restart)
func (db *Database) SetChannelRSSOK(channelID, ts string) error                  // upsert (§6: neither writer depends on the other)
func (db *Database) DeleteChannelFeedData(channelID string) error                 // feed_items + channel_state
```

- [ ] **Step 1: Write the failing tests**

```go
package database

import (
	"strings"
	"testing"
	"time"
)

func fi(ch, vid, pub, prec, src, status string, pos int) FeedItem {
	return FeedItem{ChannelID: ch, VideoID: vid, Published: pub, DatePrecision: prec,
		CatalogPos: pos, Source: src, Status: status, FirstSeen: "2026-07-16T00:00:00Z"}
}

func TestUpsertFeedItem_GuardAndAlwaysArms(t *testing.T) {
	db := newTestDB(t)
	// Insert coarse; insertedNow true; status forced 'unknown' whatever caller sets.
	it := fi("UC1", "v1", "2026-07-10T00:00:00Z", "coarse", "membership", "vod", 3)
	ins, err := db.UpsertFeedItem(it)
	if err != nil || !ins {
		t.Fatalf("insert: ins=%v err=%v", ins, err)
	}
	got := mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "unknown" {
		t.Fatalf("INSERT wrote status %q; a listing supplies a DATE, never a classification (§6)", got.Status)
	}
	// Re-sight with a BETTER date (exact) and a new source: date upgrades, source
	// and catalog_pos always update, insertedNow false, first_seen unchanged.
	it2 := fi("UC1", "v1", "2026-07-12T09:00:00Z", "exact", "rss", "unknown", 0)
	it2.FirstSeen = "2026-07-16T01:00:00Z"
	ins, err = db.UpsertFeedItem(it2)
	if err != nil || ins {
		t.Fatalf("update: ins=%v err=%v (RETURNING first_seen must reveal update-vs-insert)", ins, err)
	}
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.DatePrecision != "exact" || got.Source != "rss" || got.CatalogPos != 0 || got.FirstSeen != "2026-07-16T00:00:00Z" {
		t.Fatalf("after upgrade: %+v", got)
	}
	// Re-sight with a WORSE date (coarse): date arms untouched, source still moves.
	it3 := fi("UC1", "v1", "2026-07-01T00:00:00Z", "coarse", "membership", "unknown", 7)
	db.UpsertFeedItem(it3)
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.Published != "2026-07-12T09:00:00Z" || got.DatePrecision != "exact" {
		t.Fatal("a worse estimate overwrote a better one — the guard is broken")
	}
	if got.Source != "membership" || got.CatalogPos != 7 {
		t.Fatal("source/catalog_pos are ALWAYS arms and must move on every sighting")
	}
}

func TestApplyProbeToFeedItem(t *testing.T) {
	db := newTestDB(t)
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-16T00:00:00Z", "assumed", "membership", "unknown", 0))
	// Probe: vod with a started date → status flips, date upgrades, source untouched.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Real Title", "2026-07-14T20:00:00Z", "started"); err != nil {
		t.Fatal(err)
	}
	got := mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "vod" || got.DatePrecision != "started" || got.Title != "Real Title" || got.Source != "membership" {
		t.Fatalf("probe write: %+v", got)
	}
	// A later probe with a WORSE date must not downgrade; a stale-listing-shaped
	// status write is the caller's responsibility to prevent, but 'Unknown Title'
	// must never overwrite a real title.
	db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Unknown Title", "2026-07-13T00:00:00Z", "day")
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.DatePrecision != "started" || got.Title != "Real Title" {
		t.Fatalf("probe downgrade leaked: %+v", got)
	}
	// upcoming supplies no date: rank 0 no-op, status still lands.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "upcoming", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "upcoming" || got.Published != "2026-07-14T20:00:00Z" {
		t.Fatalf("dateless probe: %+v (must never write published='')", got)
	}
}

func TestFeedScope_Q1UnionQ2(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -3).Format(time.RFC3339)
	old := now.AddDate(0, 0, -30).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)

	db.UpsertFeedItem(fi("UC1", "in", recent, "exact", "rss", "unknown", 0))       // Q1
	db.UpsertFeedItem(fi("UC1", "out", old, "coarse", "videos", "unknown", 1))     // neither
	db.UpsertFeedItem(fi("UC1", "up", old, "assumed", "rss", "unknown", 2))        // Q2 via status
	db.ApplyProbeToFeedItem("UC1", "up", "upcoming", "", "", "")
	db.UpsertFeedItem(fi("UC1", "unres", old, "assumed", "membership", "unknown", 3)) // Q2 unresolved arm (§7)
	db.UpsertFeedItem(fi("UC1", "memb", recent, "coarse", "membership", "unknown", 4))

	got, err := db.FeedScope("UC1", cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(got)
	for _, want := range []string{"in", "up", "unres", "memb"} {
		if !ids[want] {
			t.Errorf("scope missing %q", want)
		}
	}
	if ids["out"] {
		t.Error("out-of-window coarse row leaked into scope")
	}
	// The read arm is the OPERATOR'S toggle: membership rows drop from BOTH arms.
	got, _ = db.FeedScope("UC1", cutoff, false)
	ids = idsOf(got)
	if ids["memb"] || ids["unres"] {
		t.Error("membership rows visible with the toggle off")
	}
	if !ids["in"] || !ids["up"] {
		t.Error("read arm removed non-membership rows")
	}
}

func TestFeedScope_QueryPlan(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 1500; i++ { // enough rows that a bad plan is visible
		db.UpsertFeedItem(fi("UC1", "v"+strings.Repeat("x", 2)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26)),
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour).Format(time.RFC3339),
			"coarse", "videos", "unknown", i))
	}
	db.db.Exec(`ANALYZE`)
	assertPlan := func(q string, args []any, mustContain string) {
		rows, err := db.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all string
		for rows.Next() {
			var a, b, c int
			var d string
			rows.Scan(&a, &b, &c, &d)
			all += d + "\n"
		}
		if !strings.Contains(all, mustContain) || strings.Contains(all, "TEMP B-TREE") {
			t.Fatalf("plan for %q:\n%s\nwant %q, no TEMP B-TREE", q, all, mustContain)
		}
	}
	assertPlan(feedScopeQ1SQL(false), []any{"UC1", "2026-01-01T00:00:00Z"}, "idx_feed_items_window")
	assertPlan(feedScopeQ2SQL(false), []any{"UC1"}, "idx_feed_items_status")
}
```

Include the small helpers `mustGetFeedItem` (SELECT one row into `FeedItem`, `t.Fatal` on error) and `idsOf` (`[]FeedItem → map[string]bool` by VideoID) at the bottom of the test file — 10 lines each, plain SQL.

- [ ] **Step 2: Verify failure** — `go test -run 'TestUpsertFeedItem|TestApplyProbe|TestFeedScope' ./internal/database/ -v` → FAIL (undefined).

- [ ] **Step 3: Implement `database_feed_items.go`**

```go
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
// precision="" — rank 1 vs stored ≥1 is never strictly greater... rank("") is 1,
// so pass precision "" and the guard blocks it UNLESS stored is also rank 1 —
// therefore dateless probes MUST bind rank 0: handled by the CASE on :prec below.
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

func (db *Database) SetChannelRSSOK(channelID, ts string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.db.ExecContext(db.getCtx(), `INSERT INTO channel_state (channel_id, last_rss_ok_at)
VALUES (?, ?) ON CONFLICT(channel_id) DO UPDATE SET last_rss_ok_at = excluded.last_rss_ok_at`, channelID, ts)
	return err
}

func (db *Database) DeleteChannelFeedData(channelID string) error {
	// One transaction (BatchSetWatched pattern, database_jobs.go:191): a crash
	// between the two deletes would orphan a channel_state row with a stale
	// backfilled_at, and a re-added channel would silently skip its backfill.
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
```

Note on `ApplyProbeToFeedItem`'s comment vs code: the `rank` int is computed in Go and bound — delete the stale half of the comment if it confuses; the binding is the behavior.

- [ ] **Step 4: Verify pass** — `go test -run 'TestUpsert|TestApplyProbe|TestFeedScope|TestPrecision' ./internal/database/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(db): feed_items store API — upsert, probe write, FeedScope Q1∪Q2"`

---

### Task 4: Config — `archive_window_days`, `archive_slots`, deletions

**Files:**
- Modify: `internal/config/types.go` (Monitors struct ~`:83`; ChannelConfig ~`:263-265`)
- Modify: `internal/config/config.go` (Defaults ~`:64-71`; validator ~`:490-495`; `migrateOldFormat` ~`:303`; the `O(N)` comment ~`:485`)
- Modify: `internal/worker/queue.go:56-58` (fallback 2 → 10)
- Modify: `internal/monitor/feed.go` (`maxFeedItems` `:553-565` → two new resolvers; `defaultMaxFeedItems` `:23`)
- Modify (mechanical UI sweep): `internal/web/routes/config_routes.go:169`, `internal/tui/settings.go:544` + help text `:90`, `internal/tui/setup_wizard.go:1066` + `:113`, `web/public/index.html:795-800` + `:1682`, `web/public/modules/setup.js:682`, `config.example.toml:69,208` + `:103-104`
- Test: `internal/config/config_test.go` (extend)

**Interfaces:**
- Produces: `Monitors.ArchiveWindowDays int`, `Monitors.ArchiveSlots int`; `ChannelConfig.ArchiveWindowDays *int`, `ChannelConfig.ArchiveSlots *int`; monitor resolvers `fm.archiveWindowDays(ch) int`, `fm.archiveSlots(ch) int` (override → global → constant 3, `0`-or-nil means unset, mirroring `fm.maxFeedItems` at `feed.go:553-565`).
- Removes: `Monitors.MaxFeedItems`, `ChannelConfig.MaxFeedItems`, `fm.maxFeedItems`, the `migrateOldFormat` parse at `config.go:303`, all six UI validation/labels sites.

- [ ] **Step 1: Write the failing tests**

```go
func TestArchiveSettingsDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Monitors.ArchiveWindowDays != 3 || cfg.Monitors.ArchiveSlots != 3 {
		t.Fatalf("defaults %d/%d, want 3/3", cfg.Monitors.ArchiveWindowDays, cfg.Monitors.ArchiveSlots)
	}
	if cfg.Downloader.NumParallelDownloads != 10 {
		t.Fatalf("num_parallel_downloads default %d, want 10", cfg.Downloader.NumParallelDownloads)
	}
}

func TestArchiveSettingsValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Monitors.ArchiveWindowDays = 5000 // > 3650
	bad := 200                            // > 100
	cfg.Channels = []ChannelConfig{{Name: "c", ArchiveSlots: &bad}}
	Normalize(cfg)
	if cfg.Monitors.ArchiveWindowDays != 3 {
		t.Fatal("global out-of-range must warn-and-reset to default (config.go:490 contract)")
	}
	if cfg.Channels[0].ArchiveSlots != nil {
		t.Fatal("out-of-range per-channel override must clear to nil (fall back to global) — spec §5: ONE validator covers overrides too")
	}
}

func TestMaxFeedItemsIgnored(t *testing.T) {
	// An existing TOML carrying max_feed_items must load without error and
	// without effect (spec §5: dropped, not migrated).
	toml := "[monitors]\nmax_feed_items = 500\n"
	cfg, err := loadFromString(toml) // use the existing test-load helper; grep: grep -n "func load\|toml.Decode" internal/config/config_test.go | head -3
	if err != nil {
		t.Fatalf("legacy key must not error: %v", err)
	}
	if cfg.Monitors.ArchiveWindowDays != 3 {
		t.Fatal("defaults must apply")
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/config/ -run 'TestArchive|TestMaxFeedItems' -v` → FAIL.

- [ ] **Step 3: Implement**

`types.go`: in `MonitorsConfig` replace `MaxFeedItems int` with:

```go
ArchiveWindowDays int `toml:"archive_window_days" json:"archive_window_days"`
ArchiveSlots      int `toml:"archive_slots" json:"archive_slots"`
```

In `ChannelConfig` replace `MaxFeedItems *int` (`:265`) with:

```go
ArchiveWindowDays *int `toml:"archive_window_days,omitempty" json:"archive_window_days,omitempty"`
ArchiveSlots      *int `toml:"archive_slots,omitempty" json:"archive_slots,omitempty"`
```

`config.go` Defaults: `ArchiveWindowDays: 3, ArchiveSlots: 3`, `NumParallelDownloads: 10`. Validator (replacing the MaxFeedItems block at `:490-495`, same `fail`/reportOnly contract):

```go
if cfg.Monitors.ArchiveWindowDays < 1 || cfg.Monitors.ArchiveWindowDays > 3650 {
	fail("monitors.archive_window_days %d out of range 1..3650", cfg.Monitors.ArchiveWindowDays)
	if !reportOnly {
		cfg.Monitors.ArchiveWindowDays = defaults.Monitors.ArchiveWindowDays
	}
}
if cfg.Monitors.ArchiveSlots < 1 || cfg.Monitors.ArchiveSlots > 100 {
	fail("monitors.archive_slots %d out of range 1..100", cfg.Monitors.ArchiveSlots)
	if !reportOnly {
		cfg.Monitors.ArchiveSlots = defaults.Monitors.ArchiveSlots
	}
}
for i := range cfg.Channels {
	ch := &cfg.Channels[i]
	if ch.ArchiveWindowDays != nil && (*ch.ArchiveWindowDays < 1 || *ch.ArchiveWindowDays > 3650) {
		fail("channel %q archive_window_days %d out of range 1..3650", ch.Name, *ch.ArchiveWindowDays)
		if !reportOnly {
			ch.ArchiveWindowDays = nil // clear override → falls back to global
		}
	}
	if ch.ArchiveSlots != nil && (*ch.ArchiveSlots < 1 || *ch.ArchiveSlots > 100) {
		fail("channel %q archive_slots %d out of range 1..100", ch.Name, *ch.ArchiveSlots)
		if !reportOnly {
			ch.ArchiveSlots = nil
		}
	}
}
```

`migrateOldFormat` (`:303` area): delete the `max_feed_items` parse; TOML decoding of unknown keys is already tolerant — verify with the Step 1 test.

`feed.go`: replace `maxFeedItems` (`:553-565`) with two resolvers of identical shape (`*ch.X > 0` means set — replicating today's `0`-falls-through semantics), backed by `const defaultArchiveWindowDays = 3` / `defaultArchiveSlots = 3` replacing `defaultMaxFeedItems` (`:23`):

```go
func (fm *FeedMonitor) archiveWindowDays(ch *config.ChannelConfig) int {
	if ch.ArchiveWindowDays != nil && *ch.ArchiveWindowDays > 0 {
		return *ch.ArchiveWindowDays
	}
	var g int
	fm.configStore.Read(func(c *config.MoomboxConfig) { g = c.Monitors.ArchiveWindowDays })
	if g > 0 {
		return g
	}
	return defaultArchiveWindowDays
}
```

(`archiveSlots` identical with its own fields/constant.)

`queue.go:56-58`: `maxDownloads = 10` in the `<= 0` fallback — the second default site (spec §10: miss it and every test disagrees with production).

UI sweep — delete or replace each `max_feed_items` site; the new fields get inputs with `min=1 max=3650` / `min=1 max=100` and help text stating: *window = how many days back to archive (upcoming/live always covered); slots = how many backlog downloads per channel at once (new content never waits)*. Sites: `config_routes.go:169`, `tui/settings.go:544,:90`, `tui/setup_wizard.go:1066,:113`, `web/public/index.html:795-800,:1682`, `web/public/modules/setup.js:682`, `config.example.toml:69,208`. Also `config.example.toml:103-104`: `# num_parallel_downloads = 10` with help text stating the peak is `(live streams) + N` (spec §10).

Run: `grep -rn "max_feed_items\|MaxFeedItems" --include="*.go" --include="*.html" --include="*.js" --include="*.toml" . | grep -v docs/` — Expected: no output when done.

- [ ] **Step 4: Verify pass** — `go build ./... && go test ./internal/config/ ./internal/monitor/ -v` → PASS (monitor may not compile until `fm.maxFeedItems` callers in `checkChannel` are stubbed — if `mergeCandidates` still calls it, substitute `fm.archiveSlots(ch)` TEMPORARILY with a `// PLAN3 replaces this call site` comment; plan 3 deletes the whole call).
- [ ] **Step 5: Commit** — `git commit -m "feat(config): archive_window_days + archive_slots replace max_feed_items; parallel downloads default 10"`

---

### Task 5: Remove `last_videos` API

**Files:**
- Modify: `internal/database/database_extras.go:126-148` (delete `GetLastVideo`/`SetLastVideo`)
- Modify: `internal/database/database_test.go:119` (delete `TestLastVideos`)
- Modify: `internal/database/database_jobs.go:723` (legacy JSON importer: skip `lastVideos` instead of writing the dropped table)

- [ ] **Step 1: Delete the two functions and the test.** They have zero non-test callers (verified in the spec §16); if `go build` finds one, stop — that is a spec error to report, not code to keep.
- [ ] **Step 2: In the importer, replace the `lastVideos` write with a no-op** that logs at Debug: `"legacy lastVideos ignored (dropped in v16)"` — keep parsing the JSON field so old export files still load.
- [ ] **Step 3: Verify** — `go build ./... && go test ./internal/database/` → PASS, and `grep -rn "last_videos\|LastVideos" --include="*.go" internal/ | grep -v "LastVideoSeq"` → only the importer's ignore path. (`LastVideoSeq` is the live download-resume counter — MUST remain untouched.)
- [ ] **Step 4: Commit** — `git commit -m "feat(db): remove last_videos and its API"`
