package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCookieFile = `# Netscape HTTP Cookie File
# This is a generated file! Do not edit.

.youtube.com	TRUE	/	TRUE	0	SAPISID	abc123sapisid
.youtube.com	TRUE	/	TRUE	0	__Secure-3PAPISID	def456sapisid3p
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	logininfo123
.youtube.com	TRUE	/	TRUE	0	SID	sid123
.youtube.com	TRUE	/	TRUE	0	HSID	hsid123
#HttpOnly_.youtube.com	TRUE	/	TRUE	0	SSID	ssid123
.google.com	TRUE	/	TRUE	0	SAPISID	google_sapisid
.twitch.tv	TRUE	/	FALSE	0	auth-token	twitchtoken123
.twitch.tv	TRUE	/	FALSE	0	login	testuser
.example.com	TRUE	/	FALSE	0	irrelevant	shouldbeskipped
`

func TestLoadCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	// YouTube SAPISID should prefer youtube.com over google.com
	sapisid := jar.GetSapisid()
	if sapisid != "abc123sapisid" {
		t.Errorf("expected youtube.com SAPISID, got %q", sapisid)
	}

	// Login info
	if jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO") != "logininfo123" {
		t.Error("expected LOGIN_INFO cookie")
	}

	// Has auth
	if !jar.HasYouTubeAuthCookies() {
		t.Error("expected HasYouTubeAuthCookies to be true")
	}

	// HttpOnly_ cookies should be parsed
	if jar.GetCookieFor(PlatformYouTube, "SSID") != "ssid123" {
		t.Error("expected SSID from HttpOnly_ line")
	}

	// Twitch
	if jar.GetTwitchAuthToken() != "twitchtoken123" {
		t.Error("expected Twitch auth token")
	}
	if !jar.HasTwitchAuthCookies() {
		t.Error("expected HasTwitchAuthCookies to be true")
	}

	// Irrelevant domain should be skipped — from BOTH jars.
	assertAbsent(t, jar, "irrelevant", "the .example.com row is on no tracked domain")
}

func TestCookieHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	header := jar.GetCookieHeader()
	if header == "" {
		t.Error("expected non-empty cookie header")
	}
	if !strings.Contains(header, "SAPISID=") {
		t.Error("expected SAPISID in cookie header")
	}

	// The unqualified header is the YouTube one, so the fixture's two Twitch
	// rows must not appear in it. Before the jar was partitioned they did:
	// every authenticated youtube.com request carried the Twitch session's
	// auth-token and login cookies along with it.
	for _, pair := range strings.Split(header, "; ") {
		name, _, _ := strings.Cut(pair, "=")
		if name == "auth-token" || name == "login" {
			t.Errorf("YouTube Cookie header carries the Twitch cookie %q: %q", name, header)
		}
	}
	// And the Twitch header carries those two and no YouTube cookie.
	twitch := jar.GetCookieHeaderFor(PlatformTwitch)
	if twitch != "auth-token=twitchtoken123; login=testuser" {
		t.Errorf("Twitch header = %q, want exactly the two twitch.tv rows", twitch)
	}
}

// TestCookieHeaderDeterministicOrder verifies that GetCookieHeader emits
// cookies in a stable sorted order regardless of map iteration randomization
// (cookies.md #1, locked down per #59). Without sort, two consecutive calls
// can produce different Cookie headers, defeating HTTP-level debugging and
// occasionally tripping YouTube endpoints that inspect __Secure-* ordering.
func TestCookieHeaderDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	first := jar.GetCookieHeader()
	if first == "" {
		t.Fatal("expected non-empty cookie header")
	}

	// Repeated calls must return the byte-identical header.
	for i := range 50 {
		if got := jar.GetCookieHeader(); got != first {
			t.Fatalf("cookie header order drifted on iteration %d:\n first: %s\n   got: %s", i, first, got)
		}
	}

	// Pairs must be alphabetically sorted by name.
	pairs := strings.Split(first, "; ")
	prev := ""
	for _, p := range pairs {
		name, _, _ := strings.Cut(p, "=")
		if prev != "" && name < prev {
			t.Errorf("cookie header not sorted: %q comes after %q in %q", name, prev, first)
		}
		prev = name
	}
}

func TestGenerateAuthorizationHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	auth := jar.GenerateAuthorizationHeader("https://www.youtube.com")
	if auth == "" {
		t.Fatal("expected non-empty auth header")
	}
	if !strings.Contains(auth, "SAPISIDHASH") {
		t.Error("expected SAPISIDHASH in auth header")
	}
}

// TestGenerateAuthorizationHeaderOriginAllowlist verifies that bogus origins
// do not produce an Authorization header — the allowlist prevents emitting a
// SAPISIDHASH bound to an attacker-controlled origin (finding #12).
func TestGenerateAuthorizationHeaderOriginAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	for _, ok := range []string{
		"https://www.youtube.com",
		"https://studio.youtube.com",
		"https://music.youtube.com",
	} {
		if jar.GenerateAuthorizationHeader(ok) == "" {
			t.Errorf("expected allowlisted origin %q to produce header", ok)
		}
	}
	for _, bad := range []string{
		"https://evil.example",
		"https://youtube.com.evil.example",
		"http://www.youtube.com", // wrong scheme
		"",
	} {
		if got := jar.GenerateAuthorizationHeader(bad); got != "" {
			t.Errorf("expected origin %q to be rejected, got %q", bad, got)
		}
	}
}

// TestMakeSidAuthorizationKnownVector locks down the SAPISIDHASH format
// against a known (timestamp, sid, origin) input so future refactors can't
// silently break Google's hash scheme (finding #58).
func TestMakeSidAuthorizationKnownVector(t *testing.T) {
	// sha1("1234567890 test-sid https://www.youtube.com") computed once:
	// printf '%s' "1234567890 test-sid https://www.youtube.com" | sha1sum
	// = 96d13ea75f0bf102d71aebdfdaa599807584b65c
	got := makeSidAuthorization("SAPISIDHASH", "test-sid", "https://www.youtube.com", 1234567890)
	want := "SAPISIDHASH 1234567890_96d13ea75f0bf102d71aebdfdaa599807584b65c"
	if got != want {
		t.Errorf("SAPISIDHASH mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEmptyJar(t *testing.T) {
	jar := NewCookieJar()
	if jar.HasYouTubeAuthCookies() {
		t.Error("empty jar should not have auth cookies")
	}
	if jar.GetCookieHeader() != "" {
		t.Error("empty jar should have empty cookie header")
	}
}

func TestNonExistentFile(t *testing.T) {
	jar := NewCookieJar()
	err := jar.Load("/nonexistent/path/cookies.txt")
	if err != nil {
		t.Error("loading non-existent file should not error")
	}
	if !jar.IsEmpty() {
		t.Error("should be empty after loading non-existent file")
	}
}

func TestMalformedCookieLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	// Test with malformed lines: too few fields, empty name, empty domain
	content := `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	0	SAPISID	valid_sapisid
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	valid_login
short_line	only_two_fields
	TRUE	/	TRUE	0		empty_name_value
.youtube.com	TRUE	/	TRUE	0		empty_cookie_name
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	// Valid cookies should be loaded
	if jar.GetCookieFor(PlatformYouTube, "SAPISID") != "valid_sapisid" {
		t.Error("expected valid SAPISID cookie")
	}
	if jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO") != "valid_login" {
		t.Error("expected valid LOGIN_INFO cookie")
	}
	// Empty name cookie should be skipped
	assertAbsent(t, jar, "", "an empty cookie name is never stored")
}

func TestReloadCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	initial := `.youtube.com	TRUE	/	TRUE	0	SAPISID	first_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	first_login
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	if jar.GetCookieFor(PlatformYouTube, "SAPISID") != "first_value" {
		t.Error("expected first_value")
	}

	// Update cookie file
	updated := `.youtube.com	TRUE	/	TRUE	0	SAPISID	second_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	second_login
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := jar.Reload(); err != nil {
		t.Fatal(err)
	}

	if jar.GetCookieFor(PlatformYouTube, "SAPISID") != "second_value" {
		t.Error("expected second_value after reload")
	}
}

// TestDomainMatchers locks down the suffix-anchored domain matchers so the
// substring-match hazard (e.g. "fakegoogle.com.evil.tld") does not regress
// (finding #39 in reports/cookies.md).
func TestDomainMatchers(t *testing.T) {
	cases := []struct {
		fn     func(string) bool
		name   string
		domain string
		want   bool
	}{
		// isYouTubeDomain
		{isYouTubeDomain, "youtube exact", "youtube.com", true},
		{isYouTubeDomain, "youtube subdomain", "www.youtube.com", true},
		{isYouTubeDomain, "youtube subdomain with dot prefix", ".www.youtube.com", true},
		{isYouTubeDomain, "youtube music subdomain", "music.youtube.com", true},
		{isYouTubeDomain, "evil substring", "fakeyoutube.com.evil.tld", false},
		{isYouTubeDomain, "youtube in middle", "foo.youtube.com.bar", false},
		{isYouTubeDomain, "unrelated", "example.com", false},
		// isGoogleDomain
		{isGoogleDomain, "google exact", "google.com", true},
		{isGoogleDomain, "accounts.google.com", "accounts.google.com", true},
		{isGoogleDomain, "fakegoogle", "fakegoogle.com", false},
		{isGoogleDomain, "google substring not subdomain", "notgoogle.com", false},
		// isTwitchDomain
		{isTwitchDomain, "twitch exact", "twitch.tv", true},
		{isTwitchDomain, "twitch subdomain", "api.twitch.tv", true},
		{isTwitchDomain, "twitch substring", "twitch.tv.evil.tld", false},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.domain); got != tc.want {
			t.Errorf("%s: %q -> got %v, want %v", tc.name, tc.domain, got, tc.want)
		}
	}
}

// TestLoadRejectsLookalikeDomains ensures a cookie entry whose domain merely
// embeds "youtube.com" as a substring is filtered out during Load so it
// cannot leak into the Cookie header (finding #39).
func TestLoadRejectsLookalikeDomains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := `# Netscape HTTP Cookie File
fakeyoutube.com.evil.tld	TRUE	/	TRUE	0	SAPISID	attacker_value
.youtube.com	TRUE	/	TRUE	0	SAPISID	legit_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	legit_login
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if jar.GetSapisid() != "legit_value" {
		t.Errorf("lookalike domain must not win SAPISID, got %q", jar.GetSapisid())
	}
}

// TestLoadPreservesStateOnReadError verifies that Load does not clobber
// previously loaded cookies when a subsequent read fails for a reason other
// than "file does not exist" (finding #28 in reports/cookies.md).
func TestLoadPreservesStateOnReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte(testCookieFile), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if jar.GetSapisid() != "abc123sapisid" {
		t.Fatal("expected SAPISID loaded before error")
	}

	// Point at a directory to force a read error that isn't os.IsNotExist.
	if err := jar.Load(dir); err == nil {
		t.Fatal("expected error loading a directory")
	}

	// State must be preserved: valid auth cookies from the first Load are still present.
	if got := jar.GetSapisid(); got != "abc123sapisid" {
		t.Errorf("expected jar state preserved after failed Load, got SAPISID=%q", got)
	}
	if !jar.HasYouTubeAuthCookies() {
		t.Error("expected HasYouTubeAuthCookies still true after failed Load")
	}
}

// TestLoadTabInValue ensures values containing tabs are preserved rather
// than truncated at the first tab (finding #16 in reports/cookies.md).
func TestLoadTabInValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	// SAPISID value contains a literal tab mid-string; strings.Split would
	// leave 8 parts instead of 7 — the fix joins parts[6:] with "\t".
	content := ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tpart1\tpart2\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetSapisid(); got != "part1\tpart2" {
		t.Errorf("expected SAPISID with embedded tab preserved, got %q", got)
	}
}

type recordingLogger struct {
	debugs [][]any
}

func (r *recordingLogger) Debug(msg string, args ...any) {
	r.debugs = append(r.debugs, append([]any{msg}, args...))
}

// TestLoadLogsMalformedLines ensures SetLogger + a malformed line produces
// a Debug log (finding #16 in reports/cookies.md).
func TestLoadLogsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tok\n" +
		"malformed\tline\twith\ttoo\tfew\tfields\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := &recordingLogger{}
	jar := NewCookieJar()
	jar.SetLogger(logger)
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	if len(logger.debugs) == 0 {
		t.Fatal("expected at least one Debug log for malformed line")
	}
	foundMalformed := false
	for _, entry := range logger.debugs {
		if msg, ok := entry[0].(string); ok && strings.Contains(msg, "malformed") {
			foundMalformed = true
			break
		}
	}
	if !foundMalformed {
		t.Errorf("expected Debug log containing 'malformed', got %v", logger.debugs)
	}
}

func TestSapisidCookieVariants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	content := `.youtube.com	TRUE	/	TRUE	0	__Secure-1PAPISID	sapisid1p_val
.youtube.com	TRUE	/	TRUE	0	__Secure-3PAPISID	sapisid3p_val
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	logininfo
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	jar.Load(path)

	// Without SAPISID, GetSapisid should fall back to __Secure-3PAPISID
	if jar.GetSapisid() != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val, got %q", jar.GetSapisid())
	}

	sapisid, sapisid1p, sapisid3p := jar.GetSapisidCookies()
	if sapisid != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val fallback, got %q", sapisid)
	}
	if sapisid1p != "sapisid1p_val" {
		t.Errorf("expected sapisid1p_val, got %q", sapisid1p)
	}
	if sapisid3p != "sapisid3p_val" {
		t.Errorf("expected sapisid3p_val, got %q", sapisid3p)
	}
}

// TestHasAnyYouTubeAuthCookie distinguishes "this platform was never
// configured" from "it was configured and the session has since been
// partially cleared". HasYouTubeAuthCookies cannot: it needs SAPISID AND
// LOGIN_INFO, and YouTube clears LOGIN_INFO on rotation-invalidation, so a
// half-dead file reads as never-configured and the auth-loss path stays
// silent forever.
func TestHasAnyYouTubeAuthCookie(t *testing.T) {
	tests := []struct {
		name    string
		cookies map[string]string
		wantAny bool
		wantAll bool
	}{
		{"empty jar", map[string]string{}, false, false},
		{"complete set", map[string]string{"SAPISID": "a", "LOGIN_INFO": "b"}, true, true},
		{"LOGIN_INFO cleared — configured but broken", map[string]string{"SAPISID": "a"}, true, false},
		{"SAPISID cleared — configured but broken", map[string]string{"LOGIN_INFO": "b"}, true, false},
		{"3PAPISID only", map[string]string{"__Secure-3PAPISID": "a"}, true, false},
		{"secure SID only", map[string]string{"__Secure-1PSID": "a"}, true, false},
		{"non-auth cookie only", map[string]string{"PREF": "x"}, false, false},
		{"empty values do not count", map[string]string{"SAPISID": "", "LOGIN_INFO": ""}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := NewCookieJar()
			for name, value := range tt.cookies {
				j.youtube[name] = cookieEntry{value: value}
			}
			if got := j.HasAnyYouTubeAuthCookie(); got != tt.wantAny {
				t.Errorf("HasAnyYouTubeAuthCookie() = %v, want %v", got, tt.wantAny)
			}
			if got := j.HasYouTubeAuthCookies(); got != tt.wantAll {
				t.Errorf("HasYouTubeAuthCookies() = %v, want %v", got, tt.wantAll)
			}
		})
	}
}

// TestHasAnyTwitchAuthCookie: the same "was this ever configured" question on
// the Twitch side. auth-token is the credential itself, and it can be pruned
// out of the file on expiry while twilight-user survives — so a jar that is
// plainly a configured session reads as never-configured under the narrow
// predicate. See twitchAuthCookieNames.
func TestHasAnyTwitchAuthCookie(t *testing.T) {
	tests := []struct {
		name    string
		cookies map[string]string
		wantAny bool
		wantAll bool
	}{
		{"empty jar", map[string]string{}, false, false},
		{"auth-token present", map[string]string{"auth-token": "t"}, true, true},
		{"expired auth-token pruned away", map[string]string{"twilight-user": `{"id":"1"}`}, true, false},
		{"both", map[string]string{"auth-token": "t", "twilight-user": `{"id":"1"}`}, true, true},
		// login/name are deliberately NOT markers — see twitchAuthCookieNames.
		// Not a cross-site concern (Load admits twitch.tv rows only); they are
		// out because nothing establishes Twitch sets them only for signed-in
		// visitors, and this predicate raises an operator-facing alarm.
		{"unconfirmed markers stay out", map[string]string{"login": "x", "name": "y"}, false, false},
		{"empty values do not count", map[string]string{"auth-token": "", "twilight-user": ""}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := NewCookieJar()
			for name, value := range tt.cookies {
				j.twitch[name] = cookieEntry{value: value}
			}
			if got := j.HasAnyTwitchAuthCookie(); got != tt.wantAny {
				t.Errorf("HasAnyTwitchAuthCookie() = %v, want %v", got, tt.wantAny)
			}
			if got := j.HasTwitchAuthCookies(); got != tt.wantAll {
				t.Errorf("HasTwitchAuthCookies() = %v, want %v", got, tt.wantAll)
			}
		})
	}
}

// TestAuthPredicatesReadOnlyTheirOwnJar pins the routing rather than the name
// sets: every predicate is fed its own platform's credential planted in the
// OTHER platform's jar, and must report "not configured".
//
// Load can no longer produce this state — a twitch.tv auth-token cannot reach
// the youtube map — so the fixture is staged directly on the fields. That is
// the point: the state is unreachable through Load precisely BECAUSE these
// predicates each read one jar, and a predicate that scanned both would pass
// every other test in this file while quietly re-merging the two platforms.
func TestAuthPredicatesReadOnlyTheirOwnJar(t *testing.T) {
	misplaced := NewCookieJar()
	// Twitch credentials sitting in the YouTube jar.
	misplaced.youtube["auth-token"] = cookieEntry{value: "t"}
	misplaced.youtube["twilight-user"] = cookieEntry{value: `{"id":"1"}`}
	// YouTube credentials sitting in the Twitch jar.
	misplaced.twitch["SAPISID"] = cookieEntry{value: "s"}
	misplaced.twitch["LOGIN_INFO"] = cookieEntry{value: "l"}

	checks := []struct {
		name string
		got  bool
	}{
		{"HasAnyTwitchAuthCookie", misplaced.HasAnyTwitchAuthCookie()},
		{"HasTwitchAuthCookies", misplaced.HasTwitchAuthCookies()},
		{"HasAnyYouTubeAuthCookie", misplaced.HasAnyYouTubeAuthCookie()},
		{"HasYouTubeAuthCookies", misplaced.HasYouTubeAuthCookies()},
	}
	for _, c := range checks {
		if c.got {
			t.Errorf("%s() = true on a jar where that platform's cookies live in the OTHER jar — "+
				"the predicate is scanning both maps", c.name)
		}
	}
	// Same for the value accessors, so a "fix" that routed only the booleans
	// cannot pass.
	if got := misplaced.GetTwitchAuthToken(); got != "" {
		t.Errorf("GetTwitchAuthToken() = %q, want empty — it read the YouTube jar", got)
	}
	if got := misplaced.GetSapisid(); got != "" {
		t.Errorf("GetSapisid() = %q, want empty — it read the Twitch jar", got)
	}
	if got := misplaced.YouTubeIdentity(); got != "" {
		t.Errorf("YouTubeIdentity() = %q, want empty — it read the Twitch jar", got)
	}
}

// TestAuthCookieNameListsDoNotDrift pins youtubeAuthCookieNames as a SUBSET of
// essentialYouTubeCookies. The same cookie names are now written out in three
// places — essentialYouTubeCookies (which names decide what gets kept when a
// file is parsed), youtubeAuthCookieNames (which decide "was YouTube ever
// configured"), and isGoogleOnlyAuthName in refresh.go (which decide what gets
// written back to the google.com domain). A name that reaches the "configured"
// list but not the "keep it" list would be unreachable: the jar would drop it
// at load and the predicate could never see it. Subset, not equality —
// essentialYouTubeCookies deliberately also keeps non-auth cookies (PREF,
// CONSENT, YSC, the rotating SIDTS/SIDCC pair) that must NOT count as evidence
// the platform was configured.
func TestAuthCookieNameListsDoNotDrift(t *testing.T) {
	for _, name := range youtubeAuthCookieNames {
		if !essentialYouTubeCookies[name] {
			t.Errorf("youtubeAuthCookieNames has %q, which essentialYouTubeCookies drops at parse time — the predicate can never observe it", name)
		}
	}
	if len(youtubeAuthCookieNames) == 0 {
		t.Fatal("youtubeAuthCookieNames is empty — the subset check above would pass vacuously")
	}
	// The two names the auth-loss gate actually turns on. A refactor that
	// trimmed the list down to the SAPISID+LOGIN_INFO pair would silently
	// re-create the bug this predicate exists to close.
	for _, must := range []string{"SAPISID", "__Secure-3PAPISID", "LOGIN_INFO"} {
		found := false
		for _, name := range youtubeAuthCookieNames {
			if name == must {
				found = true
			}
		}
		if !found {
			t.Errorf("youtubeAuthCookieNames is missing %q", must)
		}
	}
}
