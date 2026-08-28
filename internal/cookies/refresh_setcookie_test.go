package cookies

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests cover processYouTubeSetCookies, which had zero test references
// before this file existed. Everything here is offline: a bare *http.Response
// carrying hand-written Set-Cookie headers is all the function reads.
//
// Each case notes whether it discriminates the fix from the pre-fix code.
// A case that would also pass against the unfixed function is labelled a
// regression guard so nobody later mistakes it for a proof.

// captureLogger records Info-level messages so a test can assert that an
// operator-visible event was actually reported. Only names are recorded —
// never values.
type captureLogger struct {
	mu   sync.Mutex
	info []string
}

func (c *captureLogger) record(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info = append(c.info, msg)
}

func (c *captureLogger) Debug(msg string, args ...any) {}
func (c *captureLogger) Info(msg string, args ...any)  { c.record(msg) }
func (c *captureLogger) Warn(msg string, args ...any)  {}
func (c *captureLogger) Error(msg string, args ...any) {}

func (c *captureLogger) infoContaining(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.info {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// netscapeRow is one parsed line of a Netscape cookie file, split the way
// CookieJar.Load splits it (fields 6.. are one value that may contain tabs).
type netscapeRow struct {
	raw      string
	fields   []string
	domain   string
	httpOnly bool
	expiry   string
	name     string
	value    string
}

func readCookieRows(t *testing.T, path string) []netscapeRow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	var rows []netscapeRow
	for _, line := range strings.Split(string(data), "\n") {
		// Trim ONLY the line ending. TrimSpace would eat the trailing tab of a
		// row whose value is empty and turn a 7-field row into a 6-field one,
		// which would hide exactly the row this test file is looking for.
		// (CookieJar.Load does TrimSpace, which is why an emptied row is
		// invisible to the jar while still sitting in the file forever.)
		trimmed := strings.Trim(line, "\r\n")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		httpOnly := false
		if after, ok := strings.CutPrefix(trimmed, "#HttpOnly_"); ok {
			httpOnly = true
			trimmed = after
		} else if strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Split(trimmed, "\t")
		if len(fields) < 7 {
			continue
		}
		rows = append(rows, netscapeRow{
			raw:      line,
			fields:   fields,
			domain:   fields[0],
			httpOnly: httpOnly,
			expiry:   fields[4],
			name:     fields[5],
			value:    strings.Join(fields[6:], "\t"),
		})
	}
	return rows
}

func rowsNamed(rows []netscapeRow, name string) []netscapeRow {
	var out []netscapeRow
	for _, r := range rows {
		if r.name == name {
			out = append(out, r)
		}
	}
	return out
}

func rowFor(t *testing.T, rows []netscapeRow, name, domain string) netscapeRow {
	t.Helper()
	for _, r := range rows {
		if r.name == name && r.domain == domain {
			return r
		}
	}
	t.Fatalf("no %s row under %s (have %d rows)", name, domain, len(rows))
	return netscapeRow{}
}

// setCookieResponse builds the only part of an *http.Response that
// processYouTubeSetCookies reads.
func setCookieResponse(headers ...string) *http.Response {
	h := make(http.Header)
	for _, sc := range headers {
		h.Add("Set-Cookie", sc)
	}
	return &http.Response{Header: h}
}

// newSetCookieFixture writes initial into a temp cookies.txt, loads a jar from
// it and returns a RefreshService wired to that jar.
func newSetCookieFixture(t *testing.T, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, initial string) (*RefreshService, *CookieJar, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return NewRefreshService(jar, 0, logger), jar, path
}

// TestProcessSetCookiesDeletesRowOnPastExpires covers S7: the standard
// deletion form (a 1970 Expires, and any other past date) must remove the row
// instead of rewriting it with an empty value and expiry 0 — a shape
// rowExpired can never prune, so the dead row would outlive every sweep.
//
// Discriminates: pre-fix the row survived with value "" and expiry "0".
func TestProcessSetCookiesDeletesRowOnPastExpires(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"dash-1970", "LOGIN_INFO=; Domain=.youtube.com; Expires=Thu, 01-Jan-1970 00:00:00 GMT; Path=/"},
		{"rfc1123-1970", "LOGIN_INFO=; Domain=.youtube.com; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/"},
		{"past-non-epoch", "LOGIN_INFO=; Domain=.youtube.com; Expires=Fri, 01-Jan-2021 00:00:00 GMT; Path=/"},
		{"max-age-zero", "LOGIN_INFO=; Max-Age=0; Domain=.youtube.com; Path=/"},
		{"max-age-negative", "LOGIN_INFO=; Max-Age=-1; Domain=.youtube.com; Path=/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			initial := "# Netscape HTTP Cookie File\n" +
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfixture-login\n" +
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tYSC\tfixture-ysc\n"
			rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

			rs.processYouTubeSetCookies(setCookieResponse(tc.header))

			rows := readCookieRows(t, path)
			if got := rowsNamed(rows, "LOGIN_INFO"); len(got) != 0 {
				t.Errorf("deletion cookie left %d LOGIN_INFO row(s) behind; first has expiry %q and value len %d",
					len(got), got[0].expiry, len(got[0].value))
			}
			// The unrelated row must not be collateral damage.
			if got := rowsNamed(rows, "YSC"); len(got) != 1 || got[0].value != "fixture-ysc" {
				t.Errorf("unrelated YSC row was disturbed: %+v", got)
			}
		})
	}
}

// TestProcessSetCookiesDeletionOfEssentialLogsInfo pins the operator-visible
// report required by S7 part 4: losing an essential name is a session event,
// not a debug detail.
//
// Discriminates: pre-fix there was no deletion concept at all, so no log.
func TestProcessSetCookiesDeletionOfEssentialLogsInfo(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfixture-login\n"
	log := &captureLogger{}
	rs, _, _ := newSetCookieFixture(t, log, initial)

	rs.processYouTubeSetCookies(setCookieResponse(
		"LOGIN_INFO=; Domain=.youtube.com; Expires=Thu, 01-Jan-1970 00:00:00 GMT; Path=/"))

	if n := log.infoContaining("deleted an essential cookie"); n != 1 {
		t.Errorf("expected exactly 1 Info log naming the essential deletion, got %d (%v)", n, log.info)
	}
}

// TestProcessSetCookiesPreservesTabbedValue covers S10: CookieJar.Load treats
// fields 6.. as one value that may legitimately contain tabs, so a row can
// split into 8+ parts. Setting parts[6] and re-joining left the old value's
// tail dangling past the new value.
//
// Discriminates: pre-fix the rewritten row had 8 fields and the trailing
// fragment of the old value survived on the end of the new one.
func TestProcessSetCookiesPreservesTabbedValue(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tFALSE\t2000000000\tPREF\told-head\told-tail\n"
	rs, jar, path := newSetCookieFixture(t, nopLogger{}, initial)

	// Sanity: the jar really does carry the tab through, which is what makes
	// the 8-field row a legal input rather than a corrupt one.
	if got := jar.GetCookie("PREF"); got != "old-head\told-tail" {
		t.Fatalf("fixture precondition failed: jar read PREF as %q", got)
	}

	rs.processYouTubeSetCookies(setCookieResponse("PREF=fresh-value; Domain=.youtube.com; Path=/"))

	row := rowFor(t, readCookieRows(t, path), "PREF", ".youtube.com")
	if len(row.fields) != 7 {
		t.Errorf("rewritten row has %d tab-separated fields, want exactly 7: %q", len(row.fields), row.raw)
	}
	if row.value != "fresh-value" {
		t.Errorf("rewritten PREF value = %q, want %q", row.value, "fresh-value")
	}
	if strings.Contains(row.raw, "old-tail") {
		t.Errorf("tail of the replaced value survived into the rewritten row: %q", row.raw)
	}
}

// TestProcessSetCookiesDeletionIsDomainScoped covers S7 part 3. A file that
// carries the same name on both .youtube.com and .google.com is normal; a
// deletion scoped to one of them must not take the other with it.
//
// Discriminates: pre-fix updates were keyed by name alone, so both rows were
// rewritten empty and the .google.com credential was destroyed.
func TestProcessSetCookiesDeletionIsDomainScoped(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-google\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-youtube\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	rs.processYouTubeSetCookies(setCookieResponse(
		"SAPISID=; Domain=.youtube.com; Expires=Thu, 01-Jan-1970 00:00:00 GMT; Path=/"))

	rows := readCookieRows(t, path)
	for _, r := range rowsNamed(rows, "SAPISID") {
		if r.domain == ".youtube.com" {
			t.Errorf("the .youtube.com SAPISID row should have been deleted, got %q", r.raw)
		}
	}
	google := rowFor(t, rows, "SAPISID", ".google.com")
	if google.value != "fixture-google" {
		t.Errorf(".google.com SAPISID was collateral damage: value = %q, want unchanged", google.value)
	}
	if google.expiry != "2000000000" {
		t.Errorf(".google.com SAPISID expiry was rewritten to %q, want unchanged", google.expiry)
	}
}

// TestProcessSetCookiesPerDomainValues covers the other half of the name+domain
// keying fix: one response may carry the same name for two domains, each with
// its own value.
//
// Discriminates: pre-fix the name-keyed map kept only the last header, and
// then wrote that one value onto both rows.
func TestProcessSetCookiesPerDomainValues(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t1000\tSAPISID\tstale-google\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1000\tSAPISID\tstale-youtube\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	rs.processYouTubeSetCookies(setCookieResponse(
		"SAPISID=fresh-youtube; Domain=.youtube.com; Path=/",
		"SAPISID=fresh-google; Domain=.google.com; Path=/",
	))

	rows := readCookieRows(t, path)
	if got := rowFor(t, rows, "SAPISID", ".youtube.com").value; got != "fresh-youtube" {
		t.Errorf(".youtube.com SAPISID = %q, want fresh-youtube", got)
	}
	if got := rowFor(t, rows, "SAPISID", ".google.com").value; got != "fresh-google" {
		t.Errorf(".google.com SAPISID = %q, want fresh-google", got)
	}
}

// TestProcessSetCookiesHttpOnlyInsertion covers the S10 secondary item: the
// attribute was never parsed and the prefix was never emitted, so an HttpOnly
// cookie was inserted as an ordinary row.
//
// Discriminates: pre-fix the inserted row had no #HttpOnly_ prefix.
func TestProcessSetCookiesHttpOnlyInsertion(t *testing.T) {
	rs, _, path := newSetCookieFixture(t, nopLogger{}, "# Netscape HTTP Cookie File\n")

	rs.processYouTubeSetCookies(setCookieResponse(
		"__Secure-3PSID=fixture-3psid; Domain=.youtube.com; Path=/; Secure; HttpOnly",
		"YSC=fixture-ysc; Domain=.youtube.com; Path=/",
	))

	rows := readCookieRows(t, path)
	secure := rowFor(t, rows, "__Secure-3PSID", ".youtube.com")
	if !secure.httpOnly {
		t.Errorf("HttpOnly cookie inserted without the #HttpOnly_ prefix: %q", secure.raw)
	}
	if len(secure.fields) != 7 {
		t.Errorf("inserted row has %d fields, want 7: %q", len(secure.fields), secure.raw)
	}
	// A cookie without the attribute must NOT gain the prefix.
	plain := rowFor(t, rows, "YSC", ".youtube.com")
	if plain.httpOnly {
		t.Errorf("non-HttpOnly cookie gained the #HttpOnly_ prefix: %q", plain.raw)
	}
}

// TestProcessSetCookiesDeletionClearsIdentity pins trap 3: deleting LOGIN_INFO
// flips YouTubeIdentity() to "" mid-process, which moves the liveness baseline
// and can fire the witnessed-transition notification. That is correct — the
// session genuinely died — and it is pinned here so it is a decision rather
// than a field surprise.
//
// Does NOT discriminate on its own: an emptied LOGIN_INFO and a deleted one
// both read back as "" through the jar, which is exactly why S7's damage was
// invisible to the identity logic. The row assertion below is the part that
// fails against the unfixed code.
func TestProcessSetCookiesDeletionClearsIdentity(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-sapisid\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfixture-login\n"
	rs, jar, path := newSetCookieFixture(t, nopLogger{}, initial)

	before := jar.YouTubeIdentity()
	if before == "" {
		t.Fatal("fixture precondition failed: SAPISID + LOGIN_INFO must produce an identity")
	}

	rs.processYouTubeSetCookies(setCookieResponse(
		"LOGIN_INFO=; Domain=.youtube.com; Expires=Thu, 01-Jan-1970 00:00:00 GMT; Path=/"))

	if after := jar.YouTubeIdentity(); after != "" {
		t.Errorf("identity after deleting LOGIN_INFO = %q, want empty", after)
	}
	if got := rowsNamed(readCookieRows(t, path), "LOGIN_INFO"); len(got) != 0 {
		t.Errorf("LOGIN_INFO row survived the deletion: %q", got[0].raw)
	}
}

// TestProcessSetCookiesFutureExpiryStillUpdates is a regression guard, not a
// proof: it passes against the unfixed code too. It exists so that "treat a
// past Expires as a deletion" cannot quietly become "treat every Expires as a
// deletion".
func TestProcessSetCookiesFutureExpiryStillUpdates(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1000\tLOGIN_INFO\tstale-login\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	rs.processYouTubeSetCookies(setCookieResponse(
		"LOGIN_INFO=fresh-login; Domain=.youtube.com; Expires=Sat, 01-Jan-2050 00:00:00 GMT; Path=/"))

	row := rowFor(t, readCookieRows(t, path), "LOGIN_INFO", ".youtube.com")
	if row.value != "fresh-login" {
		t.Errorf("LOGIN_INFO value = %q, want fresh-login", row.value)
	}
	if row.expiry == "1000" || row.expiry == "0" {
		t.Errorf("LOGIN_INFO expiry = %q, want the future Expires", row.expiry)
	}
}

// TestProcessSetCookiesUnparseableExpiresKeepsDefault is a regression guard.
// An Expires the parser cannot read must fall back to the one-year default,
// NOT to a zero value that the new past-expiry rule would read as a deletion.
func TestProcessSetCookiesUnparseableExpiresKeepsDefault(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1000\tLOGIN_INFO\tstale-login\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	rs.processYouTubeSetCookies(setCookieResponse(
		"LOGIN_INFO=fresh-login; Domain=.youtube.com; Expires=not-a-date; Path=/"))

	rows := readCookieRows(t, path)
	if got := rowsNamed(rows, "LOGIN_INFO"); len(got) != 1 {
		t.Fatalf("an unreadable Expires must not delete the row; found %d rows", len(got))
	}
	row := rowFor(t, rows, "LOGIN_INFO", ".youtube.com")
	if row.value != "fresh-login" {
		t.Errorf("LOGIN_INFO value = %q, want fresh-login", row.value)
	}
}
