package cookies

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// jarSnapshot renders BOTH jars' ENTIRE stored state — every name with the
// platform that holds it, its value, its winning domain and its captured
// expiry — in a canonical order.
//
// The platform prefix is what makes this a snapshot of the PARTITION and not
// merely of a merged view: a row that migrated between jars changes the
// snapshot even when its value and domain are untouched. Comparing snapshots
// is what makes the permutation test an assertion about the jar rather than
// about one lucky accessor.
func jarSnapshot(j *CookieJar) string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	lines := make([]string, 0, len(j.youtube)+len(j.twitch))
	for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
		for name, entry := range j.jarFor(p) {
			lines = append(lines, fmt.Sprintf("%s/%s|%s|%d|%s", p, name, entry.domain, entry.expiry, entry.value))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// lookupEntry reads one platform's stored entry without failing when it is
// absent, so a test can assert ABSENCE from a named jar.
func lookupEntry(j *CookieJar, p Platform, name string) (cookieEntry, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	entry, ok := j.jarFor(p)[name]
	return entry, ok
}

// entryOf reads one platform's stored entry, so a test can assert on the
// domain and expiry the jar actually kept and not merely on the value a public
// accessor happens to surface.
func entryOf(t *testing.T, j *CookieJar, p Platform, name string) cookieEntry {
	t.Helper()
	entry, ok := lookupEntry(j, p, name)
	if !ok {
		t.Fatalf("cookie %q is not in the %s jar at all", name, p)
	}
	return entry
}

// assertAbsent fails unless the name is in NEITHER jar. Used for rows the
// admission rule must reject outright, where "the header does not contain it"
// would also pass for a row that was admitted to the other platform.
func assertAbsent(t *testing.T, j *CookieJar, name, why string) {
	t.Helper()
	for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
		if e, ok := lookupEntry(j, p, name); ok {
			t.Errorf("%q is in the %s jar (value %q from %q), want it admitted to no jar at all — %s",
				name, p, e.value, e.domain, why)
		}
	}
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
	entry := entryOf(t, jar, PlatformYouTube, "SAPISID")
	if entry.value != "expired-sapisid" {
		t.Errorf("stored value = %q, want %q — an expired row was dropped at parse time", entry.value, "expired-sapisid")
	}
	if entry.expiry != 1000000000 {
		t.Errorf("stored expiry = %d, want 1000000000 — field 5 was not captured", entry.expiry)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "expired-sapisid" {
		t.Errorf("GetCookieFor = %q, want %q", got, "expired-sapisid")
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
	// wantWinners maps a platform to the domain that must win each contested
	// name in THAT platform's jar, under every ordering. Asserted on the
	// STORED DOMAIN rather than the value, so a fixture whose values were
	// reshuffled cannot fake a pass.
	wantWinners map[Platform]map[string]string
	// wantAbsent names cookies that must be in NEITHER jar. Distinct from
	// wantWinners on purpose: "the youtube jar holds the Google value" is a
	// ranking claim, while "the Twitch row is nowhere" is the admission claim,
	// and only the second one distinguishes a partition from a comparator.
	wantAbsent []string
}{
	{
		// The admission fix, in the fixture that used to justify a
		// cross-platform tier.
		//
		// The old admission clause carried NO domain guard on
		// essentialYouTubeCookies, so a .twitch.tv row named SID was admitted
		// and stored under the bare name "SID" — the same slot Google's real
		// auth SID occupies. Neither is a YouTube domain, so file order decided
		// which survived, and a tier (youtube < google < twitch) was added to
		// arbitrate. With admission decided by domain first, the Twitch SID is
		// never admitted at all: not to the youtube jar (wrong domain) and not
		// to the twitch jar (SID is not an essential Twitch cookie).
		//
		// The Google domains are chosen to LOSE to ".twitch.tv" on ranking:
		// "google.com" is host-only (rule 3 favours the dotted Twitch domain)
		// and "accounts.google.com" has three labels (rule 2 favours the
		// two-label Twitch domain). So the stated winner is only reachable if
		// the Twitch row was never admitted — with ".google.com" here instead,
		// the lexical backstop would hand Google the win anyway and the fixture
		// would pass without the partition.
		name: "a twitch-domain auth name is admitted nowhere",
		rows: []string{
			cookieRow(".twitch.tv", "0", "SID", "sid-from-twitch"),
			cookieRow("google.com", "0", "SID", "sid-from-google-host"),
			cookieRow("accounts.google.com", "0", "SID", "sid-from-google-accounts"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "login-from-youtube"),
		},
		// Among the two google rows the 2-label one beats the 3-label one by
		// rule 2; the twitch row is not a competitor because it is not present.
		wantWinners: map[Platform]map[string]string{
			PlatformYouTube: {"SID": "google.com", "LOGIN_INFO": ".youtube.com"},
		},
		wantAbsent: []string{"SAPISID"}, // nothing in this fixture supplies one
	},
	{
		// Rule 1 in isolation, the one cross-domain preference the split KEEPS:
		// Google auth cookies legitimately live on both youtube.com and
		// google.com, and the YouTube-domain copy is the intended winner.
		// Rule 1 must overrule rule 2 here — ".google.com" has fewer labels
		// than "www.youtube.com", so without rule 1 the Google copy wins.
		name: "youtube beats google even with more labels",
		rows: []string{
			cookieRow(".google.com", "0", "SAPISID", "sapisid-from-google"),
			cookieRow("www.youtube.com", "0", "SAPISID", "sapisid-from-youtube-www"),
		},
		wantWinners: map[Platform]map[string]string{
			PlatformYouTube: {"SAPISID": "www.youtube.com"},
		},
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
		wantWinners: map[Platform]map[string]string{
			PlatformYouTube: {"CONSENT": "google.com", "YSC": ".youtube.com"},
		},
	},
	{
		// Rules 2 and 3 INSIDE the twitch jar. Rule 1 cannot fire here — no
		// twitch.tv domain is a youtube.com one — which is what "no tier on the
		// Twitch side" means in practice; the remaining rules still have to
		// produce one deterministic winner, or auth-token falls back to file
		// order and the Twitch credential becomes whichever row was last.
		name: "the twitch jar orders its own domains without a tier",
		rows: []string{
			cookieRow("www.twitch.tv", "0", "auth-token", "token-from-www"),
			cookieRow("twitch.tv", "0", "auth-token", "token-from-host"),
			cookieRow(".twitch.tv", "0", "auth-token", "token-from-dot"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "login-from-youtube"),
		},
		wantWinners: map[Platform]map[string]string{
			// rule 2 drops www (3 labels); rule 3 picks the dotted form.
			PlatformTwitch:  {"auth-token": ".twitch.tv"},
			PlatformYouTube: {"LOGIN_INFO": ".youtube.com"},
		},
	},
	{
		// Rule 4 (lexical backstop), isolated: three domains identical in
		// every earlier rule. Rules 1-3 cannot separate them, so without the
		// backstop they tie and fall to file order — which is exactly what
		// permutation-invariance would catch.
		name: "lexical backstop separates otherwise identical domains",
		rows: []string{
			cookieRow("www.youtube.com", "0", "PREF", "pref-from-www"),
			cookieRow("music.youtube.com", "0", "PREF", "pref-from-music"),
			cookieRow("studio.youtube.com", "0", "PREF", "pref-from-studio"),
			cookieRow(".twitch.tv", "0", "auth-token", "token-from-twitch"),
		},
		wantWinners: map[Platform]map[string]string{
			PlatformYouTube: {"PREF": "music.youtube.com"},
			PlatformTwitch:  {"auth-token": ".twitch.tv"},
		},
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
			// including a wrong one. Pin which row won, in which jar.
			jar := loadRows(t, fx.rows)
			for p, winners := range fx.wantWinners {
				for name, wantDomain := range winners {
					if e := entryOf(t, jar, p, name); e.domain != wantDomain {
						t.Errorf("%s/%s winner domain = %q, want %q", p, name, e.domain, wantDomain)
					}
				}
			}
			for _, name := range fx.wantAbsent {
				assertAbsent(t, jar, name, "no row in this fixture may admit it")
			}
		})
	}
}

// TestTwitchDomainAuthNameIsNeverAdmitted is the correctness case stated on
// its own, at the smallest size that shows it — and it asserts the ADMISSION
// claim, not a ranking one.
//
// Under the old flat map both rows were admitted: the Twitch one because
// essentialYouTubeCookies[name] was checked with no domain guard, the Google
// one because isGoogleAuth matched. They then collided on the bare name, and a
// Twitch-domain SID or SAPISID could evict the credential Moombox actually
// authenticates with. A domain comparator was added to arbitrate that.
//
// Domain-first admission removes the collision instead of resolving it, so the
// assertion has to distinguish "never admitted" from "admitted and then
// out-ranked". Absence from the twitch jar does not do that on its own — a
// wrongly-admitted row lands in the YOUTUBE jar, so the twitch jar is empty of
// it either way.
//
// The discriminator is the choice of Google domain. "google.com" is HOST-ONLY,
// and comparator rule 3 gives a dot-prefixed domain the win over a host-only
// one at equal label count, so a ".twitch.tv" row that reached the youtube jar
// would BEAT it and take the slot. Asserting the Google row still holds the
// slot is therefore only satisfiable if the Twitch row was never admitted.
// With ".google.com" on the Google side the two would tie down to the lexical
// backstop, which happens to favour "google" — and the test would pass for the
// wrong reason.
func TestTwitchDomainAuthNameIsNeverAdmitted(t *testing.T) {
	for _, name := range []string{"SID", "SAPISID", "__Secure-3PAPISID", "__Secure-1PSID"} {
		google := cookieRow("google.com", "0", name, "the-real-google-credential")
		twitch := cookieRow(".twitch.tv", "0", name, "a-stray-twitch-row")

		// Guard the discriminator itself: if the comparator ever stopped
		// preferring the Twitch domain here, the assertions below would go
		// quiet without anyone noticing.
		if compareCookieDomains(".twitch.tv", "google.com") >= 0 {
			t.Fatalf("fixture no longer discriminates: a wrongly-admitted .twitch.tv row would lose to " +
				"google.com on ranking alone, so this test could pass without the admission rule")
		}

		for _, order := range [][]string{{google, twitch}, {twitch, google}} {
			jar := loadRows(t, order)

			// The Google row is intact in the youtube jar — impossible unless
			// the Twitch row never entered the contest.
			e := entryOf(t, jar, PlatformYouTube, name)
			if e.value != "the-real-google-credential" || e.domain != "google.com" {
				t.Errorf("youtube/%s = %q from %q, want %q from %q — a twitch.tv row was admitted to the "+
					"youtube jar and outranked a Google auth cookie",
					name, e.value, e.domain, "the-real-google-credential", "google.com")
			}
			// And it is not in the twitch jar either: a YouTube auth name is
			// not an essential Twitch cookie.
			if e, ok := lookupEntry(jar, PlatformTwitch, name); ok {
				t.Errorf("twitch/%s = %q from %q, want absent — a twitch.tv row carrying a YouTube auth name "+
					"must not be admitted to any jar", name, e.value, e.domain)
			}
		}
	}

	// The converse direction is closed by the same domain-first switch and is
	// worth pinning so a later "simplification" cannot open it: Twitch's own
	// four names are only consulted on the twitch.tv branch. A google.com or
	// youtube.com row carrying one of them is never admitted at all.
	for _, name := range []string{"auth-token", "twilight-user", "login", "name"} {
		jar := loadRows(t, []string{
			cookieRow(".google.com", "0", name, "cross-site-row"),
			cookieRow(".youtube.com", "0", name, "cross-site-row"),
		})
		assertAbsent(t, jar, name, "the Twitch name set must stay domain-guarded")
	}
}

// TestPartitionIsStructural is the claim the two-jar design makes, asserted on
// the structure rather than on any single accessor's answer.
//
// Fixture: a file where .twitch.tv rows carry the exact names Google's auth
// cookies use, alongside the real Google rows. Under one flat name-keyed map
// these are the same map keys and one set had to lose. Under two jars the
// question does not arise, and every permutation of the file must produce the
// identical partition — enumerated programmatically, because two hand-picked
// orderings cannot distinguish a total order from a rule that merely reverses
// the old file-order bias.
//
// THE GOOGLE DOMAINS ARE CHOSEN TO LOSE. Every assertion below sits downstream
// of a junction two different mechanisms can satisfy: "the Google value is in
// the youtube jar" is true both when the Twitch row was never admitted (the
// partition, under test) and when it was admitted and then out-ranked (the
// comparator, which this change deletes). The fixture closes that junction by
// picking Google domains a wrongly-admitted ".twitch.tv" row would BEAT:
//
//   - SID on "google.com" — host-only, so rule 3 hands the win to the dotted
//     ".twitch.tv" at equal label count.
//   - SAPISID on "accounts.google.com" — three labels, so rule 2 hands the win
//     to the two-label ".twitch.tv".
//
// Two different rungs on purpose, so the discrimination does not rest on one.
// With ".google.com" on the Google side both pairs would fall through to the
// lexical backstop, which happens to spell "google" before "twitch" — and the
// test would pass without the partition existing at all.
func TestPartitionIsStructural(t *testing.T) {
	rows := []string{
		cookieRow(".twitch.tv", "0", "SID", "twitch-row-named-SID"),
		cookieRow(".twitch.tv", "0", "SAPISID", "twitch-row-named-SAPISID"),
		cookieRow(".twitch.tv", "0", "auth-token", "the-real-twitch-token"),
		cookieRow("google.com", "0", "SID", "the-real-google-SID"),
		cookieRow("accounts.google.com", "0", "SAPISID", "the-real-google-SAPISID"),
	}

	// Guard the discriminator: if either Google domain ever stopped losing to
	// ".twitch.tv" on ranking, the assertions below would go quiet.
	for _, loser := range []string{"google.com", "accounts.google.com"} {
		if compareCookieDomains(".twitch.tv", loser) >= 0 {
			t.Fatalf("fixture no longer discriminates: a wrongly-admitted .twitch.tv row would lose to %q "+
				"on ranking alone, so this test could pass without the partition", loser)
		}
	}

	assert := func(t *testing.T, jar *CookieJar, order string) {
		t.Helper()

		// 1. The YouTube header carries the Google values — exact pairs, so a
		//    truncated or mangled rendering cannot pass on a substring. Because
		//    the Twitch rows out-rank these domains, this can only hold if they
		//    were never admitted.
		ytPairs := headerPairs(jar.GetCookieHeader())
		for _, want := range []string{"SID=the-real-google-SID", "SAPISID=the-real-google-SAPISID"} {
			if !slices.Contains(ytPairs, want) {
				t.Errorf("[%s] YouTube header pairs = %v, want %q present — a twitch.tv row was admitted "+
					"to the youtube jar and out-ranked the Google row", order, ytPairs, want)
			}
		}
		// The same fact on stored state, and pinning the winning DOMAIN, so a
		// value collision cannot be mistaken for a domain one.
		for name, wantDomain := range map[string]string{"SID": "google.com", "SAPISID": "accounts.google.com"} {
			if e := entryOf(t, jar, PlatformYouTube, name); e.domain != wantDomain {
				t.Errorf("[%s] youtube/%s came from %q, want %q", order, name, e.domain, wantDomain)
			}
		}

		// 2. The Twitch rows named SID/SAPISID are in the twitch jar either —
		//    those names are not essential Twitch cookies. Together with (1)
		//    this pins them to NO jar.
		for _, name := range []string{"SID", "SAPISID"} {
			if e, ok := lookupEntry(jar, PlatformTwitch, name); ok {
				t.Errorf("[%s] twitch/%s = %q, want absent", order, name, e.value)
			}
		}

		// 3. The Twitch jar holds exactly its own credential and nothing else.
		twPairs := headerPairs(jar.GetCookieHeaderFor(PlatformTwitch))
		if want := []string{"auth-token=the-real-twitch-token"}; !slices.Equal(twPairs, want) {
			t.Errorf("[%s] Twitch header pairs = %v, want exactly %v", order, twPairs, want)
		}

		// 4. Neither header carries the other platform's rows.
		for _, p := range ytPairs {
			if strings.HasPrefix(p, "auth-token=") {
				t.Errorf("[%s] YouTube header carries a Twitch cookie: %q", order, p)
			}
		}
	}

	// Every ordering, and the whole partition identical across all of them.
	path := filepath.Join(t.TempDir(), "cookies.txt")
	var want, wantOrder string
	count := 0
	permutations(rows, func(perm []string) {
		if t.Failed() {
			return
		}
		count++
		order := strings.Join(perm, " | ")
		jar := loadRowsInto(t, path, perm)
		assert(t, jar, order)
		got := jarSnapshot(jar)
		if want == "" {
			want, wantOrder = got, order
			return
		}
		if got != want {
			t.Errorf("load order changed the partition.\n  order A: %s\n  order B: %s\n\n  jar A:\n%s\n\n  jar B:\n%s",
				wantOrder, order, want, got)
		}
	})
	if wantCount := 120; count != wantCount { // 5!
		t.Fatalf("enumerated %d orderings of %d rows, want %d — the enumeration is not exhaustive",
			count, len(rows), wantCount)
	}
}

// TestGetCookieReadsTheTwitchJar pins the single most dangerous routing
// decision in the split.
//
// GetCookie reads as a generic accessor and has exactly ONE consumer in the
// tree: internal/twitch/auth.go, fetching "auth-token". Routing it to the
// youtube jar on the strength of its name de-authenticates Twitch, and does so
// SILENTLY — internal/twitch/chat_irc.go logs in with PASS SCHMOOPIIE when the
// token is empty, so chat keeps connecting and merely stops seeing
// subscriber-only messages and badges. Nothing raises a fault.
func TestGetCookieReadsTheTwitchJar(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".twitch.tv", "0", "auth-token", "the-real-twitch-token"),
		cookieRow(".youtube.com", "0", "SAPISID", "a-youtube-cookie"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "a-youtube-cookie"),
	})

	// The exact call internal/twitch/auth.go makes.
	if got := jar.GetCookie("auth-token"); got != "the-real-twitch-token" {
		t.Errorf("GetCookie(%q) = %q, want %q — internal/twitch/auth.go would hand the IRC client an "+
			"empty token and chat_irc.go would fall back to anonymous login without erroring",
			"auth-token", got, "the-real-twitch-token")
	}
	// GetTwitchAuthToken is the same fact by another route; both must agree, so
	// a change that fixes one and not the other cannot pass.
	if got := jar.GetTwitchAuthToken(); got != "the-real-twitch-token" {
		t.Errorf("GetTwitchAuthToken() = %q, want %q", got, "the-real-twitch-token")
	}
	// And it does NOT read the youtube jar: a YouTube cookie name is invisible
	// through it. Asserted with a name that is genuinely present in the other
	// jar, so "" here can only mean "the twitch jar was consulted".
	if got := jar.GetCookie("SAPISID"); got != "" {
		t.Errorf("GetCookie(%q) = %q, want empty — GetCookie must read the Twitch jar only", "SAPISID", got)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "a-youtube-cookie" {
		t.Errorf("GetCookieFor(youtube, SAPISID) = %q, want %q — the cookie must still be reachable "+
			"through the explicit accessor", got, "a-youtube-cookie")
	}
}

// TestGetTwitchCredentialsReadsTheTwitchJar pins the IRC handshake's credential
// pair.
//
// The login names the account the IRC session identifies as, and it must come
// from the SAME twitch.tv rows as the auth-token: the two are presented
// together (PASS + NICK) and a pair drawn from different places is a session
// authenticated as nobody. Reading either from the youtube jar, or from a
// foreign site's row, returns "" — and "" is silent, because chat_irc.go then
// falls all the way back to the anonymous handshake instead of erroring.
//
// The fixture plants .youtube.com rows literally named "login" and "auth-token"
// so that "" here cannot be explained by "that name appears nowhere in the
// file".
func TestGetTwitchCredentialsReadsTheTwitchJar(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".twitch.tv", "0", "auth-token", "the-real-twitch-token"),
		cookieRow(".twitch.tv", "0", "login", "the-real-twitch-login"),
		cookieRow(".youtube.com", "0", "login", "a-youtube-row-wearing-the-twitch-name"),
		cookieRow(".youtube.com", "0", "auth-token", "another-youtube-row-wearing-a-twitch-name"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "a-youtube-cookie"),
	})

	token, login := jar.GetTwitchCredentials()
	if token != "the-real-twitch-token" {
		t.Errorf("GetTwitchCredentials() token = %q, want %q", token, "the-real-twitch-token")
	}
	if login != "the-real-twitch-login" {
		t.Errorf("GetTwitchCredentials() login = %q, want %q — internal/twitch would hand the IRC "+
			"client no nickname and the handshake would drop to anonymous without erroring",
			login, "the-real-twitch-login")
	}
	// GetTwitchAuthToken is the same fact by another route; both must agree, so
	// a change that fixes one and not the other cannot pass.
	if got := jar.GetTwitchAuthToken(); got != token {
		t.Errorf("GetTwitchAuthToken() = %q, want the paired accessor's %q", got, token)
	}
	// A jar holding only the YouTube rows yields nothing: Load admits Twitch
	// cookie names on twitch.tv only, so the foreign rows are not merely
	// out-ranked, they are never stored.
	ytOnly := loadRows(t, []string{
		cookieRow(".youtube.com", "0", "login", "a-youtube-row-wearing-the-twitch-name"),
		cookieRow(".youtube.com", "0", "auth-token", "another-youtube-row-wearing-a-twitch-name"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "a-youtube-cookie"),
	})
	if token, login := ytOnly.GetTwitchCredentials(); token != "" || login != "" {
		t.Errorf("GetTwitchCredentials() = (%q, %q) on a YouTube-only jar, want both empty",
			token, login)
	}
}

// TestGetTwitchCredentialsIsAtomicAcrossReload is the claim the paired accessor
// exists for: the two halves are read under ONE RLock, so no concurrent Reload
// can be observed halfway.
//
// Reload swaps the jar's maps under Lock from the refresh loop, the Twitch
// service and the YouTube auth path — all goroutines other than the one running
// an IRC handshake. Two separate accessors could therefore straddle the swap
// and pair one account's token with another's login: a session that
// authenticates as neither, and whose failure is silent.
//
// The fixture accounts are chosen so a torn pair is *detectable*: token and
// login always carry the same account suffix, so any observed pair whose halves
// disagree can only have come from two different jar states. Run under -race,
// this also fails on an unsynchronised read.
func TestGetTwitchCredentialsIsAtomicAcrossReload(t *testing.T) {
	// Three complete account files, written once. The reload loop below then
	// costs a small read plus a parse — no write — which is what makes the swap
	// frequent enough relative to the reads for a two-lock reader to be caught
	// reliably rather than once in a hundred thousand tries.
	dir := t.TempDir()
	accounts := []string{"alpha", "beta", "gamma"}
	paths := make([]string, len(accounts))
	for i, account := range accounts {
		paths[i] = filepath.Join(dir, "cookies-"+account+".txt")
		content := "# Netscape HTTP Cookie File\n" +
			cookieRow(".twitch.tv", "0", "auth-token", "token-"+account) + "\n" +
			cookieRow(".twitch.tv", "0", "login", "login-"+account) + "\n"
		if err := os.WriteFile(paths[i], []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	jar := NewCookieJar()
	if err := jar.Load(paths[0]); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var writer, readers sync.WaitGroup

	// Writer: swaps the jar between whole accounts as fast as it can. Errors
	// are counted rather than raised — t.Fatal is not valid off the test
	// goroutine.
	var loadErrs, swaps atomic.Int64
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := jar.Load(paths[i%len(paths)]); err != nil {
				loadErrs.Add(1)
				return
			}
			swaps.Add(1)
		}
	}()

	// Readers: every pair they observe must name ONE account. More goroutines
	// than a typical core count, so the scheduler preempts between whatever
	// lock acquisitions the accessor makes.
	var torn, observed atomic.Int64
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 200000 {
				token, login := jar.GetTwitchCredentials()
				if token == "" || login == "" {
					continue // never expected here; not what this test is about
				}
				observed.Add(1)
				if strings.TrimPrefix(token, "token-") != strings.TrimPrefix(login, "login-") {
					torn.Add(1)
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	writer.Wait()

	if n := loadErrs.Load(); n != 0 {
		t.Fatalf("the rotating loader failed %d times; the race window was never opened", n)
	}
	// Both sides must actually have run, or a green result means nothing.
	if observed.Load() == 0 {
		t.Fatal("no credential pair was ever observed; the test proved nothing")
	}
	if swaps.Load() < 100 {
		t.Fatalf("only %d jar swaps happened during %d reads; the race window was too narrow for "+
			"this test to prove anything", swaps.Load(), observed.Load())
	}
	if n := torn.Load(); n != 0 {
		t.Errorf("observed %d torn (token, login) pairs out of %d — the two halves were read under "+
			"separate locks, so a concurrent Reload paired one account's token with another's login",
			n, observed.Load())
	}
}

// TestIsEmptyNeedsBothJarsEmpty: a file that configured only one platform is
// not an empty jar. IsEmpty gates "no cookies at all" messaging, so reading it
// off one map would report a Twitch-only install as unconfigured.
func TestIsEmptyNeedsBothJarsEmpty(t *testing.T) {
	if !NewCookieJar().IsEmpty() {
		t.Error("a fresh jar must be empty")
	}
	twitchOnly := loadRows(t, []string{cookieRow(".twitch.tv", "0", "auth-token", "t")})
	if twitchOnly.IsEmpty() {
		t.Error("a Twitch-only file reports IsEmpty — IsEmpty is reading the YouTube jar alone")
	}
	youtubeOnly := loadRows(t, []string{cookieRow(".youtube.com", "0", "LOGIN_INFO", "l")})
	if youtubeOnly.IsEmpty() {
		t.Error("a YouTube-only file reports IsEmpty — IsEmpty is reading the Twitch jar alone")
	}
	// A file whose every row is rejected still leaves both jars empty.
	rejected := loadRows(t, []string{cookieRow(".example.invalid", "0", "SAPISID", "v")})
	if !rejected.IsEmpty() {
		t.Error("a file of irrelevant-domain rows must leave the jar empty")
	}
}

// TestGetCookieHeaderForIsPlatformScoped: each platform's header carries that
// platform's cookies and nothing else, and an unknown Platform yields "" rather
// than a panic or a merged view.
func TestGetCookieHeaderForIsPlatformScoped(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "yt-login"),
		cookieRow(".youtube.com", "0", "SAPISID", "yt-sapisid"),
		cookieRow(".twitch.tv", "0", "auth-token", "tw-token"),
		cookieRow(".twitch.tv", "0", "login", "tw-login"),
	})

	// Exact pair sets, in the documented sorted-by-name order — not a
	// containment check, which would pass for a header that also carried the
	// other platform's rows.
	if got, want := headerPairs(jar.GetCookieHeaderFor(PlatformYouTube)),
		[]string{"LOGIN_INFO=yt-login", "SAPISID=yt-sapisid"}; !slices.Equal(got, want) {
		t.Errorf("YouTube header = %v, want exactly %v", got, want)
	}
	if got, want := headerPairs(jar.GetCookieHeaderFor(PlatformTwitch)),
		[]string{"auth-token=tw-token", "login=tw-login"}; !slices.Equal(got, want) {
		t.Errorf("Twitch header = %v, want exactly %v", got, want)
	}
	// The unqualified name means YouTube; all ten production callers are
	// YouTube request paths.
	if got, want := jar.GetCookieHeader(), jar.GetCookieHeaderFor(PlatformYouTube); got != want {
		t.Errorf("GetCookieHeader() = %q, want the YouTube header %q", got, want)
	}
	if got := jar.GetCookieHeaderFor(Platform("mastodon")); got != "" {
		t.Errorf("GetCookieHeaderFor(unknown) = %q, want empty", got)
	}
	if got := jar.GetCookieFor(Platform("mastodon"), "auth-token"); got != "" {
		t.Errorf("GetCookieFor(unknown) = %q, want empty", got)
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
	if got := jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "second" {
		t.Errorf("LOGIN_INFO = %q, want %q — duplicate-identical rows still keep last-wins", got, "second")
	}

	// "#HttpOnly_" is stripped before the domain is stored, so a row and its
	// HttpOnly twin are the same domain and tie the same way.
	jar = loadRows(t, []string{
		cookieRow("#HttpOnly_.youtube.com", "0", "SSID", "httponly-first"),
		cookieRow(".youtube.com", "0", "SSID", "plain-second"),
	})
	if got := jar.GetCookieFor(PlatformYouTube, "SSID"); got != "plain-second" {
		t.Errorf("SSID = %q, want %q — the HttpOnly prefix must not create a domain distinction", got, "plain-second")
	}
	if e := entryOf(t, jar, PlatformYouTube, "SSID"); e.domain != ".youtube.com" {
		t.Errorf("stored domain = %q, want %q — the #HttpOnly_ prefix must be stripped before storage", e.domain, ".youtube.com")
	}
}

// TestLoadDomainPreferenceInBothArrivalOrders pins the one cross-domain
// preference that survives the split, in BOTH arrival orders: inside the
// youtube jar, a youtube.com row beats a google.com one. Google auth cookies
// legitimately exist on both domains and the YouTube-domain copy is the
// long-standing intended winner, so this is a real question rather than an
// artefact of the storage shape.
//
// The cross-platform rows this test used to carry are gone with the tier they
// tested. Their replacement is the last case: a PREF row on twitch.tv is not a
// competitor for anything, because it is never admitted.
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
			// Rule 1 must outrank rule 2, or the shorter google domain wins.
			name: "youtube wins even from a longer subdomain",
			rows: []string{
				cookieRow(".google.com", "0", "SID", "from-google"),
				cookieRow("accounts.youtube.com", "0", "SID", "from-youtube-sub"),
			},
			cookie: "SID", wantValue: "from-youtube-sub", wantDomain: "accounts.youtube.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jar := loadRows(t, tt.rows)
			entry := entryOf(t, jar, PlatformYouTube, tt.cookie)
			if entry.value != tt.wantValue {
				t.Errorf("%s value = %q, want %q", tt.cookie, entry.value, tt.wantValue)
			}
			if entry.domain != tt.wantDomain {
				t.Errorf("%s domain = %q, want %q", tt.cookie, entry.domain, tt.wantDomain)
			}
		})
	}

	// A YouTube-essential name on a twitch.tv domain never enters the contest.
	// PREF is not in essentialTwitchCookies either, so it lands nowhere.
	t.Run("a twitch-domain YouTube cookie is not a competitor", func(t *testing.T) {
		for _, order := range [][]string{
			{cookieRow(".google.com", "0", "PREF", "from-google"), cookieRow(".twitch.tv", "0", "PREF", "from-twitch")},
			{cookieRow(".twitch.tv", "0", "PREF", "from-twitch"), cookieRow(".google.com", "0", "PREF", "from-google")},
		} {
			jar := loadRows(t, order)
			if e := entryOf(t, jar, PlatformYouTube, "PREF"); e.value != "from-google" || e.domain != ".google.com" {
				t.Errorf("youtube/PREF = %q from %q, want %q from %q", e.value, e.domain, "from-google", ".google.com")
			}
			if e, ok := lookupEntry(jar, PlatformTwitch, "PREF"); ok {
				t.Errorf("twitch/PREF = %q from %q, want absent — PREF is not an essential Twitch cookie",
					e.value, e.domain)
			}
		}
	})
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
			if got := entryOf(t, jar, PlatformYouTube, "LOGIN_INFO").expiry; got != tt.want {
				t.Errorf("expiry for field %q = %d, want %d", tt.field, got, tt.want)
			}
			// Whatever the field said, the row is still loaded and still sent.
			if got := jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "v" {
				t.Errorf("GetCookieFor = %q, want %q — the expiry field must never gate loading", got, "v")
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

// TestExpiredAuthCookiesFor counts only AUTH cookies, only those whose expiry
// is a positive timestamp already in the past, and only within the platform
// asked about.
//
// The fixture deliberately carries decoys that a looser implementation would
// swallow: a NON-auth essential cookie (PREF) expired long ago, a negative
// expiry, and a session cookie. Each is a way the count could be inflated
// without the rule under test holding.
func TestExpiredAuthCookiesFor(t *testing.T) {
	t.Run("counts expired auth cookies only, per platform", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", expiredLongAgo, "SAPISID", "v"),     // yt auth, expired  -> counts
			cookieRow(".youtube.com", expiredLessLong, "LOGIN_INFO", "v"), // yt auth, expired  -> counts
			cookieRow(".youtube.com", expiresIn2100, "SID", "v"),          // yt auth, live     -> no
			cookieRow(".youtube.com", "0", "HSID", "v"),                   // yt auth, session  -> no
			cookieRow(".youtube.com", "-5", "SSID", "v"),                  // yt auth, negative -> no
			cookieRow(".youtube.com", expiredLongAgo, "PREF", "v"),        // NOT auth          -> no
			cookieRow(".youtube.com", expiredLongAgo, "CONSENT", "v"),     // NOT auth          -> no
			cookieRow(".twitch.tv", expiredLongAgo, "auth-token", "v"),    // tw auth, expired  -> counts (twitch)
			cookieRow(".twitch.tv", expiresIn2100, "twilight-user", "v"),  // tw auth, live     -> no
			cookieRow(".twitch.tv", expiredLongAgo, "login", "v"),         // NOT a tw auth marker -> no
		})
		if got := jar.ExpiredAuthCookiesFor(PlatformYouTube, nowForExpiry); got != 2 {
			t.Errorf("ExpiredAuthCookiesFor(youtube) = %d, want 2 (SAPISID and LOGIN_INFO only)", got)
		}
		if got := jar.ExpiredAuthCookiesFor(PlatformTwitch, nowForExpiry); got != 1 {
			t.Errorf("ExpiredAuthCookiesFor(twitch) = %d, want 1 (auth-token only)", got)
		}
	})

	// The case the owner asked for, and the reason the count cannot stay
	// YouTube-only: an expired Twitch auth-token must be visible ON ITS OWN,
	// not folded into a YouTube number that a healthy YouTube session reports
	// as zero. A dead Twitch token does not error — it downgrades chat capture
	// to anonymous and loses subscriber-only messages and badges — so a zero
	// here is an operator being told nothing is wrong.
	t.Run("a dying Twitch token is visible while YouTube is healthy", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", expiresIn2100, "SAPISID", "v"),
			cookieRow(".youtube.com", expiresIn2100, "LOGIN_INFO", "v"),
			cookieRow(".twitch.tv", expiredLongAgo, "auth-token", "v"),
		})
		if got := jar.ExpiredAuthCookiesFor(PlatformYouTube, nowForExpiry); got != 0 {
			t.Errorf("ExpiredAuthCookiesFor(youtube) = %d, want 0 — the Twitch token was folded into the YouTube count", got)
		}
		if got := jar.ExpiredAuthCookiesFor(PlatformTwitch, nowForExpiry); got != 1 {
			t.Errorf("ExpiredAuthCookiesFor(twitch) = %d, want 1 — an expired Twitch auth-token reported nothing, "+
				"which is the state that silently downgrades chat capture to anonymous", got)
		}
	})

	// And the converse, so a per-platform count that simply reports the whole
	// file twice cannot pass either.
	t.Run("a dying YouTube session is not charged to Twitch", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", expiredLongAgo, "SAPISID", "v"),
			cookieRow(".twitch.tv", expiresIn2100, "auth-token", "v"),
		})
		if got := jar.ExpiredAuthCookiesFor(PlatformYouTube, nowForExpiry); got != 1 {
			t.Errorf("ExpiredAuthCookiesFor(youtube) = %d, want 1", got)
		}
		if got := jar.ExpiredAuthCookiesFor(PlatformTwitch, nowForExpiry); got != 0 {
			t.Errorf("ExpiredAuthCookiesFor(twitch) = %d, want 0 — the YouTube count leaked into Twitch's", got)
		}
	})

	t.Run("a jar of session cookies has none expired on either platform", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "0", "SAPISID", "v"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "v"),
			cookieRow(".youtube.com", "0", "SID", "v"),
			cookieRow(".twitch.tv", "0", "auth-token", "v"),
			cookieRow(".twitch.tv", "0", "twilight-user", "v"),
		})
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := jar.ExpiredAuthCookiesFor(p, nowForExpiry); got != 0 {
				t.Errorf("ExpiredAuthCookiesFor(%s) = %d, want 0 — expiry 0 is a live session cookie, not an ancient one", p, got)
			}
		}
	})

	t.Run("now is the comparison point", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "1500000000", "SAPISID", "v"),
			cookieRow(".twitch.tv", "1500000000", "auth-token", "v"),
		})
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := jar.ExpiredAuthCookiesFor(p, 1499999999); got != 0 {
				t.Errorf("ExpiredAuthCookiesFor(%s, before) = %d, want 0", p, got)
			}
			if got := jar.ExpiredAuthCookiesFor(p, 1500000001); got != 1 {
				t.Errorf("ExpiredAuthCookiesFor(%s, after) = %d, want 1", p, got)
			}
		}
	})

	t.Run("empty, nil and unknown-platform jars", func(t *testing.T) {
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := NewCookieJar().ExpiredAuthCookiesFor(p, nowForExpiry); got != 0 {
				t.Errorf("empty jar (%s) = %d, want 0", p, got)
			}
			if got := (*CookieJar)(nil).ExpiredAuthCookiesFor(p, nowForExpiry); got != 0 {
				t.Errorf("nil jar (%s) = %d, want 0", p, got)
			}
		}
		jar := loadRows(t, []string{cookieRow(".youtube.com", expiredLongAgo, "SAPISID", "v")})
		if got := jar.ExpiredAuthCookiesFor(Platform("mastodon"), nowForExpiry); got != 0 {
			t.Errorf("unknown platform = %d, want 0", got)
		}
	})
}

// TestAuthCookieHorizonFor: the soonest non-zero expiry among one platform's
// AUTH cookies. The fixture puts an even sooner expiry on a non-auth cookie,
// and on the OTHER platform, so a version that scanned everything would return
// the wrong number rather than merely a different one.
func TestAuthCookieHorizonFor(t *testing.T) {
	t.Run("soonest auth expiry, ignoring non-auth and the other platform", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "1600000000", "SAPISID", "v"),
			cookieRow(".youtube.com", "1500000000", "LOGIN_INFO", "v"),  // soonest YouTube auth
			cookieRow(".youtube.com", "0", "SID", "v"),                  // session: skipped
			cookieRow(".youtube.com", "1400000000", "PREF", "v"),        // sooner, but NOT auth
			cookieRow(".twitch.tv", "1300000000", "auth-token", "v"),    // sooner, but not YouTube
			cookieRow(".twitch.tv", "1200000000", "login", "v"),         // sooner, but NOT a tw auth marker
			cookieRow(".twitch.tv", "1350000000", "twilight-user", "v"), // later than auth-token
		})
		if got := jar.AuthCookieHorizonFor(PlatformYouTube); got != 1500000000 {
			t.Errorf("AuthCookieHorizonFor(youtube) = %d, want 1500000000 (LOGIN_INFO)", got)
		}
		if got := jar.AuthCookieHorizonFor(PlatformTwitch); got != 1300000000 {
			t.Errorf("AuthCookieHorizonFor(twitch) = %d, want 1300000000 (auth-token)", got)
		}
	})

	t.Run("zero when no auth cookie carries an expiry", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "0", "SAPISID", "v"),
			cookieRow(".youtube.com", "0", "LOGIN_INFO", "v"),
			cookieRow(".youtube.com", "1400000000", "PREF", "v"), // non-auth expiry must not leak in
			cookieRow(".twitch.tv", "0", "auth-token", "v"),
			cookieRow(".twitch.tv", "1200000000", "login", "v"), // non-auth expiry must not leak in
		})
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := jar.AuthCookieHorizonFor(p); got != 0 {
				t.Errorf("AuthCookieHorizonFor(%s) = %d, want 0 — no auth cookie has an expiry to run out", p, got)
			}
		}
	})

	t.Run("negative expiries are not a horizon", func(t *testing.T) {
		jar := loadRows(t, []string{
			cookieRow(".youtube.com", "-5", "SAPISID", "v"),
			cookieRow(".twitch.tv", "-5", "auth-token", "v"),
		})
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := jar.AuthCookieHorizonFor(p); got != 0 {
				t.Errorf("AuthCookieHorizonFor(%s) = %d, want 0", p, got)
			}
		}
	})

	t.Run("empty, nil and unknown-platform jars", func(t *testing.T) {
		for _, p := range []Platform{PlatformYouTube, PlatformTwitch} {
			if got := NewCookieJar().AuthCookieHorizonFor(p); got != 0 {
				t.Errorf("empty jar (%s) = %d, want 0", p, got)
			}
			if got := (*CookieJar)(nil).AuthCookieHorizonFor(p); got != 0 {
				t.Errorf("nil jar (%s) = %d, want 0", p, got)
			}
		}
		jar := loadRows(t, []string{cookieRow(".youtube.com", "1500000000", "SAPISID", "v")})
		if got := jar.AuthCookieHorizonFor(Platform("mastodon")); got != 0 {
			t.Errorf("unknown platform = %d, want 0", got)
		}
	})
}

// TestCompareCookieDomainsIsATotalOrder checks the comparator directly:
// antisymmetric and returning 0 only for identical strings. Without the last
// property a "tie" could silently mean "two different domains the rule cannot
// separate", which is exactly the file-order dependence this replaces.
//
// The domain list still spans both platforms even though Load never asks a
// cross-platform question any more — totality is a property of the function,
// and a comparator that panicked or tied on an unexpected input would be a
// latent trap for whichever future caller reaches it first.
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
		{".youtube.com", ".google.com", "rule 1: youtube beats google"},
		{"www.youtube.com", ".google.com", "rule 1 overrules rule 2: youtube wins with MORE labels"},
		{"youtube.com", ".google.com", "rule 1 overrules rule 3: youtube wins host-only against a dotted google"},
		{".youtube.com", "www.youtube.com", "rule 2: fewer labels"},
		{"google.com", ".accounts.google.com", "rule 2 overrules rule 3: label count decides before the leading dot"},
		{"twitch.tv", "www.twitch.tv", "rule 2 inside the twitch jar, where rule 1 cannot fire"},
		// Rule 3 is subsumed by rule 4 today ('.' sorts below every leading
		// hostname character), so DELETING it does not change any answer —
		// only reversing it does, and these two pairs catch that. The
		// totality loop above is what catches the other way rule 3 can be
		// lost: drop it AND make the lexical backstop dot-insensitive, and
		// these two domains compare equal. See compareCookieDomains.
		{".youtube.com", "youtube.com", "rule 3: dot-prefixed beats host-only"},
		{".twitch.tv", "twitch.tv", "rule 3: dot-prefixed beats host-only, inside the twitch jar"},
		{"music.youtube.com", "www.youtube.com", "rule 4: lexical backstop, same platform/labels/dot"},
		{"accounts.google.com", "myaccount.google.com", "rule 4: lexical backstop"},
	}
	for _, o := range ordered {
		if got := compareCookieDomains(o.better, o.worse); got >= 0 {
			t.Errorf("compareCookieDomains(%q, %q) = %d, want negative — %s", o.better, o.worse, got, o.why)
		}
	}

	// Rule 1 must be INERT inside the twitch jar. That is what "no tier on the
	// Twitch side" means, and the split gets it from the inputs rather than
	// from a second code path — so it is asserted as behaviour, not assumed.
	//
	// rulesTwoToFour is written out independently here: for every pair of
	// twitch.tv domains the real comparator must return exactly what the
	// rule-1-less tail returns. A rule 1 that ever fired on a twitch pair (say,
	// a resurrected tier that ranked twitch against something) would diverge.
	rulesTwoToFour := func(a, b string) int {
		if la, lb := domainLabelCount(a), domainLabelCount(b); la != lb {
			return la - lb
		}
		da, db := strings.HasPrefix(a, "."), strings.HasPrefix(b, ".")
		if da != db {
			if da {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	}
	twitchDomains := []string{".twitch.tv", "twitch.tv", "www.twitch.tv", "m.twitch.tv", "clips.twitch.tv"}
	for _, a := range twitchDomains {
		for _, b := range twitchDomains {
			got, want := compareCookieDomains(a, b), rulesTwoToFour(a, b)
			if (got < 0) != (want < 0) || (got == 0) != (want == 0) {
				t.Errorf("compareCookieDomains(%q, %q) = %d but rules 2-4 alone give %d — "+
					"rule 1 fired inside the twitch jar", a, b, got, want)
			}
		}
	}
}
