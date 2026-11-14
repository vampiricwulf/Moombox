package cookies

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// censusSentinel is in the value of every row the census fixtures write. No
// log line, status field or stats struct may ever carry it.
const censusSentinel = "CENSUS-SENTINEL-VALUE"

// censusRow is one moz_cookies row of a census fixture. updated is the row's
// updateTime; zero writes NULL.
type censusRow struct {
	name, host string
	updated    time.Time
	expires    time.Time
}

// writeCensusProfile builds a profile directory whose cookies.sqlite holds
// rows, each with the sentinel in its value. withStamp chooses a schema with
// or without the updateTime column.
func writeCensusProfile(t *testing.T, withStamp bool, rows []censusRow) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, firefoxCookieDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stampCol := ""
	if withStamp {
		stampCol = ", updateTime INTEGER"
	}
	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY, name TEXT, value TEXT, host TEXT, path TEXT,
		expiry INTEGER, isHttpOnly INTEGER, isSecure INTEGER, originAttributes TEXT` + stampCol + `)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		value := fmt.Sprintf("%s-%d", censusSentinel, i)
		var expiry int64
		if !r.expires.IsZero() {
			expiry = r.expires.Unix()
		}
		if withStamp {
			var stamp any
			if !r.updated.IsZero() {
				stamp = r.updated.UnixMicro()
			}
			_, err = db.Exec(`INSERT INTO moz_cookies (name, value, host, path, expiry, isHttpOnly, isSecure, originAttributes, updateTime)
				VALUES (?,?,?,'/',?,1,1,'',?)`, r.name, value, r.host, expiry, stamp)
		} else {
			_, err = db.Exec(`INSERT INTO moz_cookies (name, value, host, path, expiry, isHttpOnly, isSecure, originAttributes)
				VALUES (?,?,?,'/',?,1,1,'')`, r.name, value, r.host, expiry)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// censusFixtureRows is a signed-in profile: the pass (at `pass`) renewed the
// SID family's SID and SIDCC, left the PSIDTS pair and LOGIN_INFO alone, and
// the Google sign-in rows were last written long before.
func censusFixtureRows(pass time.Time) []censusRow {
	renewed := pass.Add(3 * time.Second)
	old := pass.Add(-72 * time.Hour)
	older := pass.Add(-30 * 24 * time.Hour)
	farExpiry := pass.Add(400 * 24 * time.Hour)
	return []censusRow{
		{name: "SAPISID", host: ".youtube.com", updated: old, expires: farExpiry},
		{name: "SID", host: ".youtube.com", updated: renewed, expires: farExpiry},
		{name: "SIDCC", host: ".youtube.com", updated: renewed, expires: farExpiry},
		{name: "__Secure-1PSIDTS", host: ".youtube.com", updated: old, expires: farExpiry},
		{name: "__Secure-3PSIDTS", host: ".youtube.com", updated: old, expires: farExpiry},
		{name: "LOGIN_INFO", host: ".youtube.com", updated: old, expires: farExpiry},
		{name: "LSID", host: "accounts.google.com", updated: older, expires: pass.Add(100 * 24 * time.Hour)},
		{name: "SID", host: ".google.com", updated: older, expires: farExpiry},
		{name: "auth-token", host: ".twitch.tv", updated: time.Time{}, expires: farExpiry},
		// Not a census row: a name no group reports, and a tracker on another site.
		{name: "PREF", host: ".youtube.com", updated: renewed},
		{name: "SID", host: ".example.com", updated: renewed},
	}
}

func censusGroup(t *testing.T, c RenewalCensus, group CensusGroup) CensusGroupResult {
	t.Helper()
	for _, g := range c.Groups {
		if g.Group == group {
			return g
		}
	}
	t.Fatalf("census has no %q group: %+v", group, c.Groups)
	return CensusGroupResult{}
}

// TestFirefoxReadTakesTheWriteStampWhereTheSchemaHasOne: a store with the
// updateTime column sorts each name by its stamp against the pass start; one
// without it reports every name as unknown and none as unchanged. Either way
// the cookies themselves are read as before.
//
// Mutants: the fallback query dropped (the old schema then fails the read);
// a missing stamp read as time zero (the old schema then lists everything
// under unchanged).
func TestFirefoxReadTakesTheWriteStampWhereTheSchemaHasOne(t *testing.T) {
	pass := time.Now().Truncate(time.Second)
	rows := censusFixtureRows(pass)

	t.Run("with the stamp column", func(t *testing.T) {
		netscape, stats, err := readFirefoxCookies(writeCensusProfile(t, true, rows))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(netscape, "\tSAPISID\t") {
			t.Fatalf("the cookies were not read:\n%s", stripValues(netscape))
		}
		c := censusOf(stats.stamps, pass)
		if !c.Stamped {
			t.Fatal("a store with updateTime must read as stamped")
		}
		sid := censusGroup(t, c, CensusSID)
		if !slices.Equal(sid.Renewed, []string{"SID"}) || !slices.Equal(sid.Unchanged, []string{"SAPISID"}) {
			t.Errorf("sid = renewed %v unchanged %v, want [SID] and [SAPISID]", sid.Renewed, sid.Unchanged)
		}
		if got := censusGroup(t, c, CensusSIDCC).Renewed; !slices.Equal(got, []string{"SIDCC"}) {
			t.Errorf("sidcc renewed = %v", got)
		}
		if got := censusGroup(t, c, CensusPSIDTS).Unchanged; !slices.Equal(got, []string{"__Secure-1PSIDTS", "__Secure-3PSIDTS"}) {
			t.Errorf("psidts unchanged = %v", got)
		}
		google := censusGroup(t, c, CensusGoogleAccount)
		if !slices.Equal(google.Unchanged, []string{"LSID", "SID"}) {
			t.Errorf("google_account unchanged = %v, want the google.com rows only", google.Unchanged)
		}
		if google.Expires == nil || !google.Expires.Equal(pass.Add(100*24*time.Hour)) {
			t.Errorf("google_account expires = %v, want the group's earliest expiry", google.Expires)
		}
		// A row with a NULL stamp in a stamped store is unknown, not stale.
		tw := censusGroup(t, c, CensusTwitch)
		if !slices.Equal(tw.Unknown, []string{"auth-token"}) || len(tw.Unchanged) != 0 || tw.RenewedAt != nil {
			t.Errorf("twitch = %+v, want auth-token unknown", tw)
		}
		oldest, ok := c.oldestGroup()
		if !ok || oldest.Group != CensusGoogleAccount {
			t.Errorf("oldest group = %v (%v), want google_account", oldest.Group, ok)
		}
	})

	t.Run("without the stamp column", func(t *testing.T) {
		netscape, stats, err := readFirefoxCookies(writeCensusProfile(t, false, rows))
		if err != nil {
			t.Fatalf("a store without updateTime must read as it always has: %v", err)
		}
		if !strings.Contains(netscape, "\tSAPISID\t") || !strings.Contains(netscape, "\tauth-token\t") {
			t.Fatalf("the cookies were not read:\n%s", stripValues(netscape))
		}
		c := censusOf(stats.stamps, pass)
		if c.Stamped {
			t.Error("a store without updateTime must not read as stamped")
		}
		for _, g := range c.Groups {
			if len(g.Renewed) != 0 || len(g.Unchanged) != 0 || len(g.Unknown) == 0 || g.RenewedAt != nil {
				t.Errorf("%s = %+v, want every name unknown", g.Group, g)
			}
		}
		if _, ok := c.oldestGroup(); ok {
			t.Error("no group has an age when nothing is stamped")
		}
	})
}

// stripValues keeps a Netscape blob's names for a failure message and drops
// its values.
func stripValues(netscape string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(netscape, "\n") {
		if f := strings.Split(line, "\t"); len(f) >= 6 {
			b.WriteString(f[0] + " " + f[5] + "\n")
		}
	}
	return b.String()
}

// TestAStampOfAnUnexpectedKindDoesNotCostTheCookie: updateTime is scanned
// untyped, so text in the column reads as "no stamp" and the row is still a
// cookie. Mutant: scanning it into sql.NullInt64 drops the row as a scan
// error.
func TestAStampOfAnUnexpectedKindDoesNotCostTheCookie(t *testing.T) {
	dir := writeCensusProfile(t, true, []censusRow{{name: "SAPISID", host: ".youtube.com"}})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, firefoxCookieDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE moz_cookies SET updateTime = 'yesterday'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	netscape, stats, err := readFirefoxCookies(dir)
	if err != nil || !strings.Contains(netscape, "\tSAPISID\t") {
		t.Fatalf("the row was lost: err=%v scanErrors=%d", err, stats.scanErrors)
	}
	if len(stats.stamps) != 1 || !stats.stamps[0].updated.IsZero() {
		t.Errorf("stamps = %+v, want one row with no stamp", stats.stamps)
	}
}

// TestFirefoxStampTimeReadsTheUnitOffTheMagnitude: microseconds are what
// Firefox writes; a store that kept milliseconds or seconds lands on the same
// instant instead of in 1970 or the far future.
func TestFirefoxStampTimeReadsTheUnitOffTheMagnitude(t *testing.T) {
	want := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for name, v := range map[string]int64{"micro": want.UnixMicro(), "milli": want.UnixMilli(), "seconds": want.Unix()} {
		if got := firefoxStampTime(v); !got.Equal(want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if !firefoxStampTime(0).IsZero() || !firefoxStampTime(-5).IsZero() {
		t.Error("zero and negative stamps mean no stamp")
	}
}

// censusPass runs one profile-import pass over a census fixture and returns
// the service and everything it logged.
func censusPass(t *testing.T, withStamp bool) (*AutoCookieService, *levelTaggedLogger) {
	t.Helper()
	// The fixture's "renewed" rows are stamped after the pass starts.
	profileDir := writeCensusProfile(t, withStamp, censusFixtureRows(time.Now().Add(time.Minute)))
	logger := &levelTaggedLogger{}
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), logger)
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	if _, err := s.RefreshCookiesDetailed(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	return s, logger
}

// censusLines returns the pass's census lines, each rendered with its fields.
func censusLines(logger *levelTaggedLogger) []string {
	var out []string
	for _, ln := range logger.lines {
		if ln.level != "info" || !strings.HasPrefix(ln.msg, "cookie renewal census") {
			continue
		}
		line := ln.msg
		for i := 0; i+1 < len(ln.args); i += 2 {
			line += fmt.Sprintf(" %v=%v", ln.args[i], ln.args[i+1])
		}
		out = append(out, line)
	}
	return out
}

// TestAPassLogsOneCensusLineAndPublishesIt: one Info line per pass names what
// was renewed and what was not, with the age of the group renewed longest
// ago, and the status carries the same census.
//
// Mutants: recordCensus not called from the pass; the stamps of the read not
// handed to the pass (every name then reads unknown).
func TestAPassLogsOneCensusLineAndPublishesIt(t *testing.T) {
	s, logger := censusPass(t, true)

	lines := censusLines(logger)
	if len(lines) != 1 {
		t.Fatalf("census lines = %d, want exactly one per pass: %q", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{
		"renewed=sid[SID] sidcc[SIDCC] unchanged=",                                                        // renewed
		"psidts[__Secure-1PSIDTS,__Secure-3PSIDTS]", "login_info[LOGIN_INFO]", "google_account[LSID,SID]", // unchanged
		"unknown=twitch[auth-token]", // unknown: no stamp on the row
		"oldest_group=google_account", "oldest_age=", "oldest_expires=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("census line lacks %q:\n%s", want, line)
		}
	}

	census := s.GetStatus().RenewalCensus
	if census == nil || !census.Stamped {
		t.Fatalf("status census = %+v, want the stamped census of the pass", census)
	}
	if got := censusGroup(t, *census, CensusSID).Renewed; !slices.Equal(got, []string{"SID"}) {
		t.Errorf("status sid renewed = %v", got)
	}
}

// TestACensusWithoutStampsSaysUnknown: a store with no write stamp gets the
// line that says so, and no name is reported unchanged.
func TestACensusWithoutStampsSaysUnknown(t *testing.T) {
	s, logger := censusPass(t, false)

	lines := censusLines(logger)
	if len(lines) != 1 {
		t.Fatalf("census lines = %d, want one: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "records no write time") || !strings.Contains(lines[0], "unchanged=none") ||
		!strings.Contains(lines[0], "renewed=none") || strings.Contains(lines[0], "oldest_group") {
		t.Errorf("census line must say unknown and claim nothing:\n%s", lines[0])
	}
	if census := s.GetStatus().RenewalCensus; census == nil || census.Stamped {
		t.Errorf("status census = %+v, want an unstamped census", census)
	}
}

// TestTheCensusNeverCarriesACookieValue seeds every row's value with a
// sentinel and looks for it everywhere the census reaches: the read's stats,
// every line the pass logged at any level, the status JSON and the line both
// UIs draw.
func TestTheCensusNeverCarriesACookieValue(t *testing.T) {
	for _, withStamp := range []bool{true, false} {
		s, logger := censusPass(t, withStamp)

		_, stats, err := readFirefoxCookies(s.profileDir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%+v", stats); strings.Contains(got, censusSentinel) {
			t.Errorf("stamp=%v: the read stats hold a cookie value", withStamp)
		}
		for _, ln := range logger.lines {
			if got := ln.msg + " " + fmt.Sprint(ln.args...); strings.Contains(got, censusSentinel) {
				t.Errorf("stamp=%v: a %s line holds a cookie value: %s", withStamp, ln.level, ln.msg)
			}
		}
		status := s.GetStatus()
		raw, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), censusSentinel) {
			t.Errorf("stamp=%v: the status JSON holds a cookie value", withStamp)
		}
		if !strings.Contains(string(raw), `"renewalCensus"`) {
			t.Errorf("stamp=%v: the status JSON has no census to check", withStamp)
		}
		if strings.Contains(RenewalLine(status, time.Now()), censusSentinel) {
			t.Errorf("stamp=%v: the renewal line holds a cookie value", withStamp)
		}
	}
}

// TestStampsOfNetscapeReportsNamesWithNoStamp: a read with no write stamps
// (Chromium over CDP, the DPAPI fallback) is judged from the rows it
// returned: names and expiries, every one unknown.
func TestStampsOfNetscapeReportsNamesWithNoStamp(t *testing.T) {
	netscape := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1900000000\tSID\t" + censusSentinel + "\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\t" + censusSentinel + "\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tPREF\t" + censusSentinel + "\n"
	stamps := stampsOfNetscape(netscape)
	if strings.Contains(fmt.Sprintf("%+v", stamps), censusSentinel) {
		t.Fatal("the stamps hold a cookie value")
	}
	c := censusOf(stamps, time.Now())
	if c.Stamped || len(c.Groups) != 2 {
		t.Fatalf("census = %+v, want two unstamped groups", c)
	}
	sid := censusGroup(t, c, CensusSID)
	if !slices.Equal(sid.Unknown, []string{"SID"}) || sid.Expires == nil || sid.Expires.Unix() != 1900000000 {
		t.Errorf("sid = %+v", sid)
	}
	if got := censusGroup(t, c, CensusLoginInfo).Unknown; !slices.Equal(got, []string{"LOGIN_INFO"}) {
		t.Errorf("login_info unknown = %v (the #HttpOnly_ row must be read)", got)
	}
}

// TestRenewalLineParity is the Go half of
// web/tests/fixtures/renewal-line-parity.json; the JS half is
// web/tests/cookie-badge.test.mjs. Each case's status is decoded as the wire
// type, so the keys the dashboard reads are the keys Go sends.
func TestRenewalLineParity(t *testing.T) {
	raw, err := os.ReadFile("../../web/tests/fixtures/renewal-line-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string          `json:"name"`
			Now    time.Time       `json:"now"`
			Status json.RawMessage `json:"status"`
			Line   string          `json:"line"`
			JSOnly bool            `json:"jsOnly"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, c := range fixture.Cases {
		if c.JSOnly {
			continue
		}
		var status AutoCookieStatus
		dec := json.NewDecoder(strings.NewReader(string(c.Status)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&status); err != nil {
			t.Fatalf("%s: status: %v", c.Name, err)
		}
		if got := RenewalLine(status, c.Now); got != c.Line {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Line)
		}
		ran++
	}
	if ran < 10 {
		t.Fatalf("only %d cases ran", ran)
	}
}

// TestNextVisitDue: the status's next daily visit is the later of the start
// hold, a day (plus jitter) after the last renewing pass and a running retry
// wait; nothing while no loop runs or a visit would launch nothing; a last pass stamped in the future counts
// as none, as it does for the visit itself.
func TestNextVisitDue(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	jitter := 4 * time.Minute

	if _, ok := nextVisitDue(now, at(-time.Hour), dailyVisit{}); ok {
		t.Error("no loop, no due time")
	}
	if _, ok := nextVisitDue(now, at(-time.Hour), dailyVisit{started: started, noSource: true}); ok {
		t.Error("a visit that would launch nothing has no due time")
	}
	cases := []struct {
		name string
		last *time.Time
		v    dailyVisit
		want time.Time
	}{
		{"a pass two hours ago", at(-2 * time.Hour), dailyVisit{started: started, jitter: jitter}, now.Add(22*time.Hour + jitter)},
		{"never renewed", nil, dailyVisit{started: started}, started.Add(dailyVisitStartHold)},
		{"just started, long overdue", at(-72 * time.Hour), dailyVisit{started: now}, now.Add(dailyVisitStartHold)},
		{"overdue with a retry wait", at(-72 * time.Hour), dailyVisit{started: started, retryAt: now.Add(30 * time.Minute)}, now.Add(30 * time.Minute)},
		{"a last pass in the future", at(3 * time.Hour), dailyVisit{started: started}, started.Add(dailyVisitStartHold)},
	}
	for _, c := range cases {
		got, ok := nextVisitDue(now, c.last, c.v)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s: due = %v (%v), want %v", c.name, got, ok, c.want)
		}
	}
}

// TestStatusCarriesTheLastReplyRenewal: the status says when a YouTube reply
// last renewed a cookie the jar kept, and nothing before one has.
func TestStatusCarriesTheLastReplyRenewal(t *testing.T) {
	jar, path, _ := renewalJar(t)
	s := NewAutoCookieService(t.TempDir(), path, jar, nopAutoCookieLogger{})
	if got := s.GetStatus().ReplyRenewedAt; got != nil {
		t.Fatalf("ReplyRenewedAt = %v before any renewal", got)
	}
	before := time.Now().Add(-time.Second)
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/feed/x", jar.GetCookieHeader(), 200, sidccRenewal))
	got := s.GetStatus().ReplyRenewedAt
	if got == nil || got.Before(before) {
		t.Fatalf("ReplyRenewedAt = %v, want the time of the renewal", got)
	}
}
