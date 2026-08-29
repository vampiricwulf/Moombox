package cookies

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cookieRow renders one Netscape cookie row. Values are synthetic
// throughout this file — no real cookie value is ever written or read.
func cookieRow(domain, expiry, name, value string) string {
	return strings.Join([]string{domain, "TRUE", "/", "TRUE", expiry, name, value}, "\t")
}

// loadRowsInto writes rows to path and loads a jar from it.
func loadRowsInto(t *testing.T, path string, rows []string) *CookieJar {
	t.Helper()
	content := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// loadRows writes rows to a fresh temp cookie file and loads a jar from it.
func loadRows(t *testing.T, rows []string) *CookieJar {
	t.Helper()
	return loadRowsInto(t, filepath.Join(t.TempDir(), "cookies.txt"), rows)
}

// permutations calls fn with every ordering of rows.
//
// Written out rather than hand-listing orderings because enumerating them ALL
// is the whole point of the invariance test: with two hand-picked orderings, a
// rule that merely reverses the old last-wins bias passes.
func permutations(rows []string, fn func([]string)) {
	work := slices.Clone(rows)
	var rec func(k int)
	rec = func(k int) {
		if k == len(work) {
			fn(slices.Clone(work))
			return
		}
		for i := k; i < len(work); i++ {
			work[k], work[i] = work[i], work[k]
			rec(k + 1)
			work[k], work[i] = work[i], work[k]
		}
	}
	rec(0)
}

// jarSnapshot renders the jar's ENTIRE stored state — every name with its
// value, winning domain and captured expiry — in a canonical order. Comparing
// snapshots is what makes the permutation test an assertion about the jar
// rather than about one lucky accessor.
func jarSnapshot(j *CookieJar) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	lines := make([]string, 0, len(j.cookies))
	for name, entry := range j.cookies {
		lines = append(lines, fmt.Sprintf("%s|%s|%d|%s", name, entry.domain, entry.expiry, entry.value))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// entryOf reads one stored entry, so a test can assert on the domain and
// expiry the jar actually kept and not merely on the value a public accessor
// happens to surface.
func entryOf(t *testing.T, j *CookieJar, name string) cookieEntry {
	t.Helper()
	j.mu.RLock()
	defer j.mu.RUnlock()
	entry, ok := j.cookies[name]
	if !ok {
		t.Fatalf("cookie %q is not in the jar at all", name)
	}
	return entry
}

// headerPairs splits a Cookie header into its exact "name=value" pairs. A
// substring check would pass on a partial or mangled rendering; equality on a
// whole pair will not.
func headerPairs(header string) []string {
	if header == "" {
		return nil
	}
	return strings.Split(header, "; ")
}

// TestLoadCapturesExpiryButNeverFilters is the single most important claim in
// this change, and both halves are asserted deliberately.
//
// The jar CAPTURES Netscape field 5 and does not act on it. A filter added in
// the wrong place satisfies one half and not the other: dropping the row at
// parse time empties the jar AND the header, while a filter inside
// GetCookieHeader would leave the jar looking correct and silently stop
// sending a cookie. So this asserts the stored entry carries the past expiry
// AND that the exact pair still renders in the header.
//
// Filtering here is forbidden for two independent reasons — the autocookies
// credential-loss check depends on the jar and mergeCookieFiles DISAGREEING
// about expired rows, and dropping rows would change what gets sent. See the
// Load doc comment.
func TestLoadCapturesExpiryButNeverFilters(t *testing.T) {
	const longExpired = "1000000000" // 2001-09-09, unambiguously in the past
	jar := loadRows(t, []string{
		cookieRow(".youtube.com", longExpired, "SAPISID", "expired-sapisid"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "session-logininfo"),
	})

	// Half 1: the row is in the jar, with the file's expiry captured verbatim.
	entry := entryOf(t, jar, "SAPISID")
	if entry.value != "expired-sapisid" {
		t.Errorf("stored value = %q, want %q — an expired row was dropped at parse time", entry.value, "expired-sapisid")
	}
	if entry.expiry != 1000000000 {
		t.Errorf("stored expiry = %d, want 1000000000 — field 5 was not captured", entry.expiry)
	}
	if got := jar.GetCookie("SAPISID"); got != "expired-sapisid" {
		t.Errorf("GetCookie = %q, want %q", got, "expired-sapisid")
	}

	// Half 2: it is still SENT. Exact pair, not a substring of the header.
	pairs := headerPairs(jar.GetCookieHeader())
	if !slices.Contains(pairs, "SAPISID=expired-sapisid") {
		t.Errorf("Cookie header pairs = %v, want the expired SAPISID pair present — "+
			"something filtered on expiry between the jar and the header", pairs)
	}
	// And the live one is unaffected, so a wholesale header failure cannot be
	// mistaken for the expiry behaviour under test.
	if !slices.Contains(pairs, "LOGIN_INFO=session-logininfo") {
		t.Errorf("Cookie header pairs = %v, want the session LOGIN_INFO pair present", pairs)
	}
}

// permutationFixtures are sets of colliding rows, each shaped so that ONE of
// the four ordering rules is the rule that decides its contested name — and,
// where the rules could disagree, shaped so the rule under test must overrule
// the next one down. Removing any single rule therefore breaks a fixture.
//
// Small sets on purpose: every ordering of every set is enumerated, and n!
// grows faster than the coverage does.
var permutationFixtures = []struct {
	name string
	rows []string
	// wantWinners maps a contested cookie name to the domain that must win,
	// under every ordering. Asserted on the STORED DOMAIN rather than the
	// value, so a fixture whose values were reshuffled cannot fake a pass.
	wantWinners map[string]string
}{
	{
		// Rule 1 (platform tier), and the case that lifts this whole change
		// from a determinism nicety to a correctness fix.
		//
		// Load's admission clause carries NO domain guard on
		// essentialYouTubeCookies, so a .twitch.tv row named SID is admitted
		// and stored under the bare name "SID" — the same slot Google's real
		// auth SID occupies, which arrives on a .google.com domain. Neither
		// row is a YouTube domain, so the old youtube-beats-everything check
		// never fired and whichever row the FILE listed last became the jar's
		// SID: a stray Twitch-domain SID could displace a live Google auth
		// cookie. Tier order (youtube < google < twitch) settles it.
		name: "tier decides an auth name contested across platforms",
		rows: []string{
			cookieRow(".twitch.tv", "0", "SID", "sid-from-twitch"),
			cookieRow("google.com", "0", "SID", "sid-from-google-host"),
			cookieRow(".google.com", "0", "SID", "sid-from-google-dot"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "login-from-youtube"),
		},
		// google beats twitch by tier; among the two google rows the dotted
		// form beats the host-only one by rule 3.
		wantWinners: map[string]string{"SID": ".google.com", "LOGIN_INFO": ".youtube.com"},
	},
	{
		// Rule 2 (fewer labels), isolated: CONSENT pits a 3-label DOTTED
		// domain against a 2-label host-only one, so rule 2 and rule 3
		// disagree and only rule 2's answer is correct. Delete rule 2 and
		// ".accounts.google.com" wins instead.
		name: "label count outranks the leading dot",
		rows: []string{
			cookieRow(".accounts.google.com", "0", "CONSENT", "consent-from-accounts"),
			cookieRow("google.com", "0", "CONSENT", "consent-from-google-host"),
			cookieRow("www.youtube.com", "0", "YSC", "ysc-from-www"),
			cookieRow(".youtube.com", "0", "YSC", "ysc-from-dot"),
			cookieRow("music.youtube.com", "0", "YSC", "ysc-from-music"),
		},
		wantWinners: map[string]string{"CONSENT": "google.com", "YSC": ".youtube.com"},
	},
	{
		// Rule 4 (lexical backstop), isolated: three domains identical in
		// tier, label count and dot-ness. Rules 1-3 cannot separate them, so
		// without the backstop they tie and fall to file order — which is
		// exactly what permutation-invariance would catch.
		name: "lexical backstop separates otherwise identical domains",
		rows: []string{
			cookieRow("www.youtube.com", "0", "PREF", "pref-from-www"),
			cookieRow("music.youtube.com", "0", "PREF", "pref-from-music"),
			cookieRow("studio.youtube.com", "0", "PREF", "pref-from-studio"),
			cookieRow(".twitch.tv", "0", "auth-token", "token-from-twitch"),
		},
		wantWinners: map[string]string{"PREF": "music.youtube.com", "auth-token": ".twitch.tv"},
	},
}

// TestLoadIsPermutationInvariant enumerates EVERY ordering of each fixture
// programmatically, because two hand-picked orderings cannot tell a total
// order from a rule that merely reverses the old bias: any two-element
// comparison passes under "last wins" read backwards.
//
// The invariant asserted is on the whole jar — every name with its value,
// winning domain and captured expiry — not on one accessor's answer.
func TestLoadIsPermutationInvariant(t *testing.T) {
	factorial := func(n int) int {
		f := 1
		for i := 2; i <= n; i++ {
			f *= i
		}
		return f
	}

	for _, fx := range permutationFixtures {
		t.Run(fx.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cookies.txt")
			var want, wantOrder string
			count := 0
			permutations(fx.rows, func(perm []string) {
				if t.Failed() {
					return
				}
				count++
				got := jarSnapshot(loadRowsInto(t, path, perm))
				if want == "" {
					want, wantOrder = got, strings.Join(perm, " | ")
					return
				}
				if got != want {
					t.Errorf("load order changed the jar.\n  order A: %s\n  order B: %s\n\n  jar A:\n%s\n\n  jar B:\n%s",
						wantOrder, strings.Join(perm, " | "), want, got)
				}
			})
			if wantCount := factorial(len(fx.rows)); count != wantCount {
				t.Fatalf("enumerated %d orderings of %d rows, want %d — the enumeration is not exhaustive",
					count, len(fx.rows), wantCount)
			}

			// Invariance alone is satisfied by ANY deterministic rule,
			// including a wrong one. Pin which row won.
			jar := loadRows(t, fx.rows)
			for name, wantDomain := range fx.wantWinners {
				if e := entryOf(t, jar, name); e.domain != wantDomain {
					t.Errorf("%s winner domain = %q, want %q", name, e.domain, wantDomain)
				}
			}
		})
	}
}

// TestTwitchDomainAuthNameCannotDisplaceGoogle is the correctness case stated
// on its own, at the smallest size that shows it.
//
// Both rows are admitted: the Twitch one because essentialYouTubeCookies[name]
// is checked with no domain guard, the Google one because isGoogleAuth matches.
// Neither is a YouTube domain, so before the tier order existed the winner was
// whichever line the file listed last — meaning a Twitch-domain SID or SAPISID
// could evict the credential Moombox actually authenticates with. Asserted in
// BOTH arrival orders, on the value the auth accessors return, because that is
// the thing that would have been wrong.
func TestTwitchDomainAuthNameCannotDisplaceGoogle(t *testing.T) {
	for _, name := range []string{"SID", "SAPISID", "__Secure-3PAPISID", "__Secure-1PSID"} {
		google := cookieRow(".google.com", "0", name, "the-real-google-credential")
		twitch := cookieRow(".twitch.tv", "0", name, "a-stray-twitch-row")
		for _, order := range [][]string{{google, twitch}, {twitch, google}} {
			jar := loadRows(t, order)
			e := entryOf(t, jar, name)
			if e.value != "the-real-google-credential" || e.domain != ".google.com" {
				t.Errorf("%s = %q from %q, want %q from %q — a twitch.tv row displaced a Google auth cookie",
					name, e.value, e.domain, "the-real-google-credential", ".google.com")
			}
		}
	}

	// The converse direction is closed by a different mechanism and is worth
	// pinning so a later "simplification" of the admission clause cannot open
	// it: Twitch's own four names reach the jar ONLY via isTwitchEssential,
	// which requires isTwitchDomain. A google.com or youtube.com row carrying
	// one of them is never admitted at all.
	for _, name := range []string{"auth-token", "twilight-user", "login", "name"} {
		jar := loadRows(t, []string{
			cookieRow(".google.com", "0", name, "cross-site-row"),
			cookieRow(".youtube.com", "0", name, "cross-site-row"),
		})
		if got := jar.GetCookie(name); got != "" {
			t.Errorf("%s = %q from a non-Twitch domain, want empty — the Twitch name set must stay domain-guarded", name, got)
		}
	}
}

// TestLoadDuplicateIdenticalRowsKeepLastWins marks the boundary of the
// property above. Two rows identical in both name and stored domain are a true
// tie; the order buys permutation-invariance for a SET of distinct rows, not a
// reordering of duplicates.
func TestLoadDuplicateIdenticalRowsKeepLastWins(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "first"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "second"),
	})
	if got := jar.GetCookie("LOGIN_INFO"); got != "second" {
		t.Errorf("LOGIN_INFO = %q, want %q — duplicate-identical rows still keep last-wins", got, "second")
	}

	// "#HttpOnly_" is stripped before the domain is stored, so a row and its
	// HttpOnly twin are the same domain and tie the same way.
	jar = loadRows(t, []string{
		cookieRow("#HttpOnly_.youtube.com", "0", "SSID", "httponly-first"),
		cookieRow(".youtube.com", "0", "SSID", "plain-second"),
	})
	if got := jar.GetCookie("SSID"); got != "plain-second" {
		t.Errorf("SSID = %q, want %q — the HttpOnly prefix must not create a domain distinction", got, "plain-second")
	}
	if e := entryOf(t, jar, "SSID"); e.domain != ".youtube.com" {
		t.Errorf("stored domain = %q, want %q — the #HttpOnly_ prefix must be stripped before storage", e.domain, ".youtube.com")
	}
}

// TestLoadDomainPreferenceInBothArrivalOrders pins the cross-tier rules
// independently of the permutation sweep, in BOTH arrival orders each. The
// youtube-over-google pair is a regression guard on behaviour that already
// held; the google-over-twitch pair is the behaviour this change adds, and it
// is the one that used to be decided by whichever row the file listed last.
func TestLoadDomainPreferenceInBothArrivalOrders(t *testing.T) {
	tests := []struct {
		name       string
		rows       []string
		cookie     string
		wantValue  string
		wantDomain string
	}{
		{
			name: "youtube before google",
			rows: []string{
				cookieRow(".youtube.com", "0", "SAPISID", "from-youtube"),
				cookieRow(".google.com", "0", "SAPISID", "from-google"),
			},
			cookie: "SAPISID", wantValue: "from-youtube", wantDomain: ".youtube.com",
		},
		{
			name: "google before youtube",
			rows: []string{
				cookieRow(".google.com", "0", "SAPISID", "from-google"),
				cookieRow(".youtube.com", "0", "SAPISID", "from-youtube"),
			},
			cookie: "SAPISID", wantValue: "from-youtube", wantDomain: ".youtube.com",
		},
		{
			name: "google before twitch",
			rows: []string{
				cookieRow(".google.com", "0", "PREF", "from-google"),
				cookieRow(".twitch.tv", "0", "PREF", "from-twitch"),
			},
			cookie: "PREF", wantValue: "from-google", wantDomain: ".google.com",
		},
		{
			name: "twitch before google",
			rows: []string{
				cookieRow(".twitch.tv", "0", "PREF", "from-twitch"),
				cookieRow(".google.com", "0", "PREF", "from-google"),
			},
			cookie: "PREF", wantValue: "from-google", wantDomain: ".google.com",
		},
		{
			name: "youtube before twitch",
			rows: []string{
				cookieRow(".youtube.com", "0", "PREF", "from-youtube"),
				cookieRow(".twitch.tv", "0", "PREF", "from-twitch"),
			},
			cookie: "PREF", wantValue: "from-youtube", wantDomain: ".youtube.com",
		},
		{
			name: "twitch before youtube",
			rows: []string{
				cookieRow(".twitch.tv", "0", "PREF", "from-twitch"),
				cookieRow(".youtube.com", "0", "PREF", "from-youtube"),
			},
			cookie: "PREF", wantValue: "from-youtube", wantDomain: ".youtube.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jar := loadRows(t, tt.rows)
			entry := entryOf(t, jar, tt.cookie)
			if entry.value != tt.wantValue {
				t.Errorf("%s value = %q, want %q", tt.cookie, entry.value, tt.wantValue)
			}
			if entry.domain != tt.wantDomain {
				t.Errorf("%s domain = %q, want %q", tt.cookie, entry.domain, tt.wantDomain)
			}
		})
	}
}

// TestLoadExpiryParsing pins the parse convention, which is deliberately
// identical to rowExpired's: TrimSpace then ParseInt, and 0 on any error.
// Unparseable and absent both mean "not expired by this field", the existing
// load-bearing convention.
func TestLoadExpiryParsing(t *testing.T) {
	tests := []struct {
		name  string
		field string
		want  int64
	}{
		{"future timestamp", "4102444800", 4102444800},
		{"session cookie", "0", 0},
		{"negative", "-5", -5},
		{"empty", "", 0},
		{"non-numeric", "never", 0},
		{"float is not an int", "1700000000.5", 0},
		{"padded with spaces", "  1700000000  ", 1700000000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jar := loadRows(t, []string{cookieRow(".youtube.com", tt.field, "LOGIN_INFO", "v")})
			if got := entryOf(t, jar, "LOGIN_INFO").expiry; got != tt.want {
				t.Errorf("expiry for field %q = %d, want %d", tt.field, got, tt.want)
			}
			// Whatever the field said, the row is still loaded and still sent.
			if got := jar.GetCookie("LOGIN_INFO"); got != "v" {
				t.Errorf("GetCookie = %q, want %q — the expiry field must never gate loading", got, "v")
			}
		})
	}
}

const (
	expiredLongAgo  = "1000000000" // 2001-09-09
	expiredLessLong = "1200000000" // 2008-01-10
	expiresIn2100   = "4102444800"
	nowForExpiry    = int64(1700000000) // 2023-11-14
)

// TestExpiredAuthCookies counts only AUTH cookies, and only those whose expiry
// is a positive timestamp already in the past.
//
// The fixture deliberately carries decoys that a looser implementation would
// swallow: a NON-auth essential cookie (PREF) expired long ago, a negative
// expiry, and a session cookie. Each is a way the count could be inflated
// without the rule under test holding.
func TestExpiredAuthCookies(t *testing.T) {
	t.Run("counts expired auth cookies only", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", expiredLongAgo, "SAPISID", "v"),     // auth, expired  -> counts
			cookieRow(".youtube.com", expiredLessLong, "LOGIN_INFO", "v"), // auth, expired  -> counts
			cookieRow(".youtube.com", expiresIn2100, "SID", "v"),          // auth, live     -> no
			cookieRow(".youtube.com", "0", "HSID", "v"),                   // auth, session  -> no
			cookieRow(".youtube.com", "-5", "SSID", "v"),                  // auth, negative -> no
			cookieRow(".youtube.com", expiredLongAgo, "PREF", "v"),        // NOT auth       -> no
			cookieRow(".youtube.com", expiredLongAgo, "CONSENT", "v"),     // NOT auth       -> no
			cookieRow(".twitch.tv", expiredLongAgo, "auth-token", "v"),    // twitch         -> no
			cookieRow(".twitch.tv", expiredLongAgo, "twilight-user", "v"), // twitch        -> no
		})
		if got := jar.ExpiredAuthCookies(nowForExpiry); got != 2 {
			t.Errorf("ExpiredAuthCookies = %d, want 2 (SAPISID and LOGIN_INFO only)", got)
		}
	})

	t.Run("a jar of session cookies has none expired", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "0", "SAPISID", "v"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "v"),
			cookieRow(".youtube.com", "0", "SID", "v"),
		})
		if got := jar.ExpiredAuthCookies(nowForExpiry); got != 0 {
			t.Errorf("ExpiredAuthCookies = %d, want 0 — expiry 0 is a live session cookie, not an ancient one", got)
		}
	})

	t.Run("now is the comparison point", func(t *testing.T) {
		jar := loadRows(t, []string{cookieRow(".youtube.com", "1500000000", "SAPISID", "v")})
		if got := jar.ExpiredAuthCookies(1499999999); got != 0 {
			t.Errorf("ExpiredAuthCookies(before) = %d, want 0", got)
		}
		if got := jar.ExpiredAuthCookies(1500000001); got != 1 {
			t.Errorf("ExpiredAuthCookies(after) = %d, want 1", got)
		}
	})

	t.Run("empty and nil jars", func(t *testing.T) {
		if got := NewCookieJar().ExpiredAuthCookies(nowForExpiry); got != 0 {
			t.Errorf("empty jar = %d, want 0", got)
		}
		if got := (*CookieJar)(nil).ExpiredAuthCookies(nowForExpiry); got != 0 {
			t.Errorf("nil jar = %d, want 0", got)
		}
	})
}

// TestAuthCookieHorizon: the soonest non-zero expiry among AUTH cookies. The
// fixture puts an even sooner expiry on a non-auth cookie so a version that
// scanned the whole jar would return the wrong number rather than merely a
// different one.
func TestAuthCookieHorizon(t *testing.T) {
	t.Run("soonest auth expiry, ignoring non-auth", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "1600000000", "SAPISID", "v"),
			cookieRow(".youtube.com", "1500000000", "LOGIN_INFO", "v"), // soonest auth
			cookieRow(".youtube.com", "0", "SID", "v"),                 // session: skipped
			cookieRow(".youtube.com", "1400000000", "PREF", "v"),       // sooner, but NOT auth
			cookieRow(".twitch.tv", "1300000000", "auth-token", "v"),   // sooner, but not YouTube auth
		})
		if got := jar.AuthCookieHorizon(); got != 1500000000 {
			t.Errorf("AuthCookieHorizon = %d, want 1500000000 (LOGIN_INFO)", got)
		}
	})

	t.Run("zero when no auth cookie carries an expiry", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "0", "SAPISID", "v"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "v"),
			cookieRow(".youtube.com", "1400000000", "PREF", "v"), // non-auth expiry must not leak in
		})
		if got := jar.AuthCookieHorizon(); got != 0 {
			t.Errorf("AuthCookieHorizon = %d, want 0 — no auth cookie has an expiry to run out", got)
		}
	})

	t.Run("negative expiries are not a horizon", func(t *testing.T) {
		jar := loadRows(t, []string{cookieRow(".youtube.com", "-5", "SAPISID", "v")})
		if got := jar.AuthCookieHorizon(); got != 0 {
			t.Errorf("AuthCookieHorizon = %d, want 0", got)
		}
	})

	t.Run("empty and nil jars", func(t *testing.T) {
		if got := NewCookieJar().AuthCookieHorizon(); got != 0 {
			t.Errorf("empty jar = %d, want 0", got)
		}
		if got := (*CookieJar)(nil).AuthCookieHorizon(); got != 0 {
			t.Errorf("nil jar = %d, want 0", got)
		}
	})
}

// TestCompareCookieDomainsIsATotalOrder checks the comparator directly:
// antisymmetric, transitive by construction of its tiers, and returning 0 only
// for identical strings. Without the last property a "tie" could silently mean
// "two different domains the rule cannot separate", which is exactly the
// file-order dependence this replaces.
func TestCompareCookieDomainsIsATotalOrder(t *testing.T) {
	domains := []string{
		".youtube.com", "youtube.com", "www.youtube.com", "music.youtube.com",
		".google.com", "google.com", "accounts.google.com",
		".twitch.tv", "twitch.tv", "www.twitch.tv",
		"example.invalid", // unreachable via Load, must still rank rather than panic
	}
	for _, a := range domains {
		for _, b := range domains {
			ab, ba := compareCookieDomains(a, b), compareCookieDomains(b, a)
			if a == b {
				if ab != 0 {
					t.Errorf("compareCookieDomains(%q, %q) = %d, want 0", a, b, ab)
				}
				continue
			}
			if ab == 0 {
				t.Errorf("compareCookieDomains(%q, %q) = 0 — distinct domains must never tie", a, b)
			}
			if (ab < 0) == (ba < 0) {
				t.Errorf("compareCookieDomains is not antisymmetric for (%q, %q): %d / %d", a, b, ab, ba)
			}
		}
	}

	// Spot-check each rule in isolation, so a comparator that is total but
	// ordered wrongly still fails. Every pair below is chosen so that the rule
	// named is the one that decides it — and where two rules could disagree,
	// so the higher-priority one has to overrule the lower.
	ordered := []struct{ better, worse, why string }{
		{".youtube.com", ".google.com", "rule 1: tier youtube < google"},
		{".google.com", ".twitch.tv", "rule 1: tier google < twitch"},
		{"www.twitch.tv", "example.invalid", "rule 1: a known platform outranks the unreachable default tier"},
		{"google.com", ".twitch.tv", "rule 1 overrules rule 3: tier decides before the leading dot"},
		{".youtube.com", "www.youtube.com", "rule 2: fewer labels"},
		{"google.com", ".accounts.google.com", "rule 2 overrules rule 3: label count decides before the leading dot"},
		// Rule 3 is subsumed by rule 4 today ('.' sorts below every leading
		// hostname character), so DELETING it does not change any answer —
		// only reversing it does, and these two pairs catch that. The
		// totality loop above is what catches the other way rule 3 can be
		// lost: drop it AND make the lexical backstop dot-insensitive, and
		// these two domains compare equal. See compareCookieDomains.
		{".youtube.com", "youtube.com", "rule 3: dot-prefixed beats host-only"},
		{".twitch.tv", "twitch.tv", "rule 3: dot-prefixed beats host-only"},
		{"music.youtube.com", "www.youtube.com", "rule 4: lexical backstop, same tier/labels/dot"},
		{"accounts.google.com", "myaccount.google.com", "rule 4: lexical backstop"},
	}
	for _, o := range ordered {
		if got := compareCookieDomains(o.better, o.worse); got >= 0 {
			t.Errorf("compareCookieDomains(%q, %q) = %d, want negative — %s", o.better, o.worse, got, o.why)
		}
	}
}
