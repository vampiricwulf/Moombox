---
name: moombox-database-migrations
description: Use when adding database tables, columns, or indexes — covers schema versioning, idempotent migrations, data backfill, and field map updates
---

# Database Migrations

Schema changes use incremental version-based migrations in `internal/database/migrations.go`. Current schema version: **v16**. Versioning uses SQLite's built-in `PRAGMA user_version` (since v11), read via `readUserVersion()` and written via `writeUserVersion()` — the value is `%d`-interpolated because PRAGMA statements accept no bind parameters. Pre-v11 databases with the legacy `schema_version` table are carried forward automatically; a database NEWER than the binary is refused loudly (downgrade guard).

## Checklist

### 1. Bump Schema Version
`internal/database/migrations.go` — Increment the `schemaVersion` constant (exposed as `CurrentSchemaVersion()` for the `moombox add` side process).

### 2. Add Migration Block
Append an idempotent `if version < N` block to `migrate()`. There is **no transaction** — statements run one at a time through `db.db.ExecContext(db.getCtx(), ...)`, and `writeUserVersion(N)` runs LAST. A crash mid-block therefore re-runs the whole block on next startup, so **every statement must be individually idempotent**:

```go
if version < 17 {
    // Tables and indexes: IF NOT EXISTS
    if _, err := db.db.ExecContext(db.getCtx(), `CREATE TABLE IF NOT EXISTS foo (...)`); err != nil {
        return err
    }
    // New columns: suppress duplicate-column (SQLite has no IF NOT EXISTS for columns)
    if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN bar TEXT NOT NULL DEFAULT ''`); err != nil {
        if !isDuplicateColumnErr(err) {
            return err
        }
    }
    if err := db.writeUserVersion(17); err != nil {
        return err
    }
}
```

Larger migrations get their own method (see `migrateV16()`), called from the version block.

### 3. Update createSchema Too
Fresh databases never run migrations — they execute the full `createSchema` constant and seed `PRAGMA user_version` at the current version. New tables, columns, and indexes must appear in **both** places (e.g. `segments`, `feed_items` are in `createSchema` AND in their migration blocks, by design).

### 4. Data Backfill (if needed)
Derive new column values from existing data within the same block. **Collect-then-update is mandatory**: the pool runs `SetMaxOpenConns(1)`, so an `UPDATE` issued while a `SELECT` cursor is still open waits forever for the pool's only connection. Read all rows into a slice, `rows.Close()`, THEN loop the updates (see the v2 `chat_file` backfill). Log-and-continue on per-row failures; don't fail the migration.

### 5. Update Field Maps (jobs columns only)
`internal/database/database.go` — `fieldToColumn` maps `UpdateJobFields()` keys to **`jobs` table** columns; it does not cover other tables (they have their own dedicated query functions, e.g. `database_feed_items.go`). `TestFieldToColumnCoverage` (database_test.go) enforces that every JSON-tagged `Job` field has an entry — when a new jobs column legitimately shouldn't go through `UpdateJobFields` (set once at insert, like `channel_id`), add it to the test's `excluded` set with a comment instead of skipping the map silently.

### 6. Update Queries
Add the new column to the relevant `SELECT` statements, `INSERT` (`AddJob`), and struct scan targets. Column order must match between INSERT and SELECT/scan. New tables get their own query file (pattern: `database_feed_items.go`).

## Key Constraints

- Migrations use the `if version < N` pattern (not switch/case); `writeUserVersion(N)` is the last statement of each block
- No transactions — idempotency per statement is the crash-safety mechanism
- SQLite limitations: no `ALTER COLUMN`, limited `ALTER TABLE`; `DROP TABLE IF EXISTS` is fine (v16 dropped `last_videos`)
- Non-destructive: never delete data that might be needed
- Migration code stays forever (users may upgrade from any version)
- `UpdateJobFields()` auto-appends `updated_at` on every call

## Common Mistakes

- Forgetting the `createSchema` half — fresh installs silently lack the new table/column
- Forgetting `fieldToColumn` (or the coverage-test excluded set) for a new jobs column — `TestFieldToColumnCoverage` fails, or `UpdateJobFields` silently ignores the field
- Non-idempotent statement — crashes on the re-run of a partially applied block (always `IF NOT EXISTS` / `isDuplicateColumnErr`)
- Interleaving a backfill UPDATE inside an open SELECT cursor — deadlocks on the single connection
- Adding a column without a default — existing rows get NULL, which may break scans expecting a value
- Column order mismatch between INSERT and SELECT/scan — wrong data in fields
