package cookies

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestFirefoxReadsOnlyTheDefaultContext: moz_cookies also holds rows from
// Multi-Account Containers (userContextId) and from Total Cookie Protection
// partitions (partitionKey — an embedded player on some other site). They
// were read with the default context's, and the last row of a name won: a
// container's SAPISID paired with the default context's LOGIN_INFO, and an
// embed's VISITOR_INFO1_LIVE replaced the real one. First-party isolation's
// firstPartyDomain rows are the default context with FPI on, and stay.
//
// Mutant: make firefoxCookieContext accept every row — sapisid-B and the
// embed's visitor value come back.
func TestFirefoxReadsOnlyTheDefaultContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY, originAttributes TEXT NOT NULL DEFAULT '',
		name TEXT, value TEXT, host TEXT, path TEXT,
		expiry INTEGER, isHttpOnly INTEGER, isSecure INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ins := func(oa, name, value, host string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO moz_cookies (originAttributes,name,value,host,path,expiry,isHttpOnly,isSecure) VALUES (?,?,?,?,'/',0,0,1)`,
			oa, name, value, host); err != nil {
			t.Fatal(err)
		}
	}
	ins("", "SAPISID", "sapisid-A", ".youtube.com")
	ins("", "LOGIN_INFO", "login-A", ".youtube.com")
	ins("", "VISITOR_INFO1_LIVE", "visitor-first-party", ".youtube.com")
	ins("^userContextId=2", "SAPISID", "sapisid-B", ".youtube.com")
	ins("^partitionKey=%28https%2Cexample.com%29", "VISITOR_INFO1_LIVE", "visitor-partitioned-embed", ".youtube.com")
	ins("^firstPartyDomain=twitch.tv", "auth-token", "tw-token", ".twitch.tv")
	db.Close()

	lines, stats, err := queryFirefoxCookieDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{"sapisid-A", "login-A", "visitor-first-party", "tw-token"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s from the default context", want)
		}
	}
	for _, unwanted := range []string{"sapisid-B", "visitor-partitioned-embed"} {
		if strings.Contains(all, unwanted) {
			t.Errorf("read %s from another context", unwanted)
		}
	}
	if stats.otherContext != 2 {
		t.Errorf("otherContext = %d, want the 2 skipped rows", stats.otherContext)
	}
}
