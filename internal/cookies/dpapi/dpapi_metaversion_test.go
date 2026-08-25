package dpapi

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openMetaFixture builds a throwaway SQLite database and applies the given
// statements, standing in for a Chrome "Cookies" file's `meta` table.
func openMetaFixture(t *testing.T, stmts ...string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "Cookies")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return db
}

// chromeMetaTable is the real shape Chrome uses — a LONGVARCHAR key/value
// table, so `version` can land as either a TEXT or an INTEGER storage class
// depending on how it was written.
const chromeMetaTable = `CREATE TABLE meta (key LONGVARCHAR NOT NULL UNIQUE PRIMARY KEY, value LONGVARCHAR)`

// TestReadChromeMetaVersion covers both the happy path and every way the
// probe can fail. A failed probe MUST read as 0 so the hash-prefix strip
// stays off: stripping 32 bytes that aren't there mangles every cookie
// irreversibly, while not stripping leaves values that fail the UTF-8 check
// loudly.
func TestReadChromeMetaVersion(t *testing.T) {
	tests := []struct {
		name  string
		stmts []string
		want  int64
	}{
		{
			name:  "integer-typed version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', 24)`},
			want:  24,
		},
		{
			name:  "text-typed version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', '24')`},
			want:  24,
		},
		{
			name:  "pre-hash-prefix version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', '23')`},
			want:  23,
		},
		{
			name:  "no meta table at all",
			stmts: nil,
			want:  0,
		},
		{
			name:  "meta table without a version row",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('last_compatible_version', '1')`},
			want:  0,
		},
		{
			name:  "non-numeric version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', 'twenty-four')`},
			want:  0,
		},
		{
			name:  "null version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', NULL)`},
			want:  0,
		},
		{
			name:  "negative version",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', '-5')`},
			want:  0,
		},
		{
			name:  "version with surrounding whitespace",
			stmts: []string{chromeMetaTable, `INSERT INTO meta VALUES ('version', ' 24 ')`},
			want:  24,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openMetaFixture(t, tt.stmts...)
			if got := readChromeMetaVersion(db); got != tt.want {
				t.Errorf("readChromeMetaVersion = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestChromeUsesHashPrefix pins the version gate itself. 24 is the first
// meta.version that carries the 32-byte domain hash (Chrome ~130).
func TestChromeUsesHashPrefix(t *testing.T) {
	tests := []struct {
		version int64
		want    bool
	}{
		{0, false},  // probe failed / unknown
		{1, false},  // ancient
		{23, false}, // last version without the prefix
		{24, true},  // Chrome ~130
		{25, true},
		{99, true}, // future schemas keep the prefix
	}
	for _, tt := range tests {
		if got := chromeUsesHashPrefix(tt.version); got != tt.want {
			t.Errorf("chromeUsesHashPrefix(%d) = %v, want %v", tt.version, got, tt.want)
		}
	}
}
