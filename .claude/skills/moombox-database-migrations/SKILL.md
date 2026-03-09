---
name: moombox-database-migrations
description: Use when adding database tables, columns, or indexes — covers schema versioning, idempotent migrations, data backfill, and field map updates
---

# Database Migrations

Schema changes use incremental version-based migrations in `internal/database/migrations.go`. Current schema version: **v6**.

## Checklist

### 1. Bump Schema Version
`internal/database/migrations.go` — Increment `schemaVersion` constant.

### 2. Add Migration Function
Write an idempotent migration that only runs when `version < N`:
```go
if version < 7 {
    // Use IF NOT EXISTS for tables
    _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS foo (...)`)
    // For new columns, handle duplicate gracefully
    _, err = tx.Exec(`ALTER TABLE jobs ADD COLUMN bar TEXT DEFAULT ''`)
    if err != nil && strings.Contains(err.Error(), "duplicate column") {
        err = nil // Column already exists, skip
    }
    // Bump version
    _, err = tx.Exec(`UPDATE schema_version SET version = 7`)
}
```

**Idempotency patterns used in codebase:**
- `CREATE TABLE IF NOT EXISTS` for new tables
- `CREATE INDEX IF NOT EXISTS` for indexes
- Error string matching `"duplicate column"` for `ALTER TABLE ADD COLUMN` (SQLite has no `IF NOT EXISTS` for columns)

### 3. Data Backfill (if needed)
Derive new column values from existing data within the same migration. Examples from codebase:
- v2: `chat_file` derived from `output_file` base path + `.chat.json`
- v3: `thumbnail_file`/`description_file` backfilled by checking files on disk

### 4. Update Field Maps (if adding updatable columns)
`internal/database/database.go` — Add entry to `fieldToColumn` map so `UpdateJobFields()` can write to the new column. This map is intentionally a subset — only columns that change during the job lifecycle (status, progress, percent, speed, etc.) are included. Columns set once at creation (platform, manually_added, itags) are not in the map.

### 5. Update Queries
Add the new column to relevant `SELECT` (prepared statements), `INSERT` (`AddJob`), and struct scan targets. The column order must match between INSERT and SELECT statements.

## Key Constraints

- Migrations use `if version < N` pattern (not switch/case)
- SQLite limitations: no `DROP COLUMN` (before 3.35), no `ALTER COLUMN`, limited `ALTER TABLE`
- Non-destructive: never delete data that might be needed
- Migration code stays forever (users may upgrade from any version)
- `UpdateJobFields()` auto-appends `updated_at` with ISO 8601 timestamp on every call

## Common Mistakes

- Forgetting `fieldToColumn` entry for a frequently-updated field — `UpdateJobFields` silently ignores it
- Non-idempotent migration — crashes on re-run if partially applied (always handle `duplicate column` error)
- Adding column without default — existing rows get NULL, may break queries
- Column order mismatch between INSERT and SELECT/scan — causes wrong data in fields
