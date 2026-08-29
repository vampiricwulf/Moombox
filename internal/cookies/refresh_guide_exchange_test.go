package cookies

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// This file covers the folded guide exchange (youtubeGuideExchange) and the
// declared-origin parameter on the write path.
//
// The fold removed a ~60-line copy: checkYouTubeAuth and checkAndRefreshYouTube
// now share one request/gate/verdict function and differ in exactly one thing —
// whether they merge Set-Cookie afterwards. (They appeared to differ in their
// URL too; the two URL vars held the same string.) That merge asymmetry is the
// thing worth a test, and it is the first one below.

// The authenticated fixture is loggedInGuideBody from refresh_liveness_test.go —
// an explicit logged_in="1" tracking param, the positive counterpart of
// loggedOutGuideBody.

// TestVerifyPathNeverWritesTheJar is the rollback invariant, and it is the
// reason the shared exchange returns its response instead of acting on it.
//
// CheckYouTubeAuth is wired into AutoCookieService.VerifyYouTubeAuth
// (cmd/moombox/services.go), and checkPlatformAuth runs it on the ROLLBACK path
// of a profile import: its answer decides whether the previous cookies are
// restored. If the shared exchange merged Set-Cookie headers itself, the verify
// call would write the jar from the very response being used to judge the
// import — the bad import would rewrite the credentials it was about to be
// rolled back for, and the restore would land on a file that had already moved.
//
// The two halves are inseparable and share one server and one fixture, for the
// same reason TestGuideReplySetCookieMergeFollowsTheVerdict does. "The rotated
// value is not in the file" is satisfied by several mechanisms — a Set-Cookie
// the domain filter rejects, a fixture that was never mergeable, a jar with no
// file path — and only one of them is the rule being pinned. The refresh half is
// the positive control: same headers, same body, same harness, and it DOES land.
// So when the verify half holds, the entry point's own choice is the only thing
// that can have stopped it.
func TestVerifyPathNeverWritesTheJar(t *testing.T) {
	// An AUTHENTICATED reply, because that is the only path on which
	// checkAndRefreshYouTube merges at all. A logged-out fixture would make the
	// verify half pass for the wrong reason.
	t.Run("verify_does_not_merge", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t, loggedInGuideBody, rotatedSetCookie))

		jar := jarWithAuth(t)
		path := jar.GetFilePath()
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture cookie file: %v", err)
		}
		beforeCookie := jar.GetCookieFor(PlatformYouTube, "SAPISID")
		if beforeCookie == "" {
			t.Fatal("premise broken: the fixture jar holds no SAPISID to rotate")
		}

		rs := NewRefreshService(jar, 0, nopLogger{})
		auth, err := rs.CheckYouTubeAuth(context.Background())
		if err != nil || !auth {
			t.Fatalf("premise broken: auth=%v err=%v, want true/nil — the reply says logged_in=1", auth, err)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("re-read cookie file: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("the verify path rewrote cookies.txt (%d bytes -> %d bytes); "+
				"an import rollback would now be deciding against a file the check itself moved",
				len(before), len(after))
		}
		if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != beforeCookie {
			t.Error("the verify path changed the in-memory SAPISID value")
		}
	})

	// The control. Without it the assertion above is vacuous.
	t.Run("refresh_does_merge", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t, loggedInGuideBody, rotatedSetCookie))

		jar := jarWithAuth(t)
		path := jar.GetFilePath()
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture cookie file: %v", err)
		}

		rs := NewRefreshService(jar, 0, nopLogger{})
		auth, err := rs.checkAndRefreshYouTube(context.Background())
		if err != nil || !auth {
			t.Fatalf("premise broken: auth=%v err=%v, want true/nil", auth, err)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("re-read cookie file: %v", err)
		}
		if bytes.Equal(before, after) {
			t.Fatal("the refresh path did not take this fixture either — " +
				"the verify assertion above proves nothing")
		}
		if !strings.Contains(string(after), "rotated-by-the-server") {
			t.Errorf("the refresh path rewrote the file without landing the rotated value:\n%s", string(after))
		}
		if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "rotated-by-the-server" {
			t.Errorf("jar SAPISID after the refresh = %q, want the rotated value", got)
		}
	})
}

// TestGuideExchangeHandsBackNoResponseOnAnyError pins the structural half of
// the invariant above, at the seam where it lives.
//
// TestVerifyPathNeverWritesTheJar covers the behaviour: the verify entry point
// does not merge. This covers the belt underneath it — youtubeGuideExchange
// returns a nil response on EVERY error path, so "a reply we could not read is
// not a reply anyone may write the jar from" is a fact about the return values
// rather than a rule the callers have to remember. A behavioural test cannot see
// this: checkAndRefreshYouTube returns early on err != nil, so handing a
// response back beside an error changes no observable outcome today. It changes
// what the next caller is handed, which is the whole reason the guarantee is
// written down.
//
// The authenticated case is the control: without it "always nil" would pass, and
// the refresh path would have nothing to merge from.
func TestGuideExchangeHandsBackNoResponseOnAnyError(t *testing.T) {
	t.Run("errors", func(t *testing.T) {
		t.Run("unreadable body", func(t *testing.T) {
			pointYouTubeGuideAt(t, bodyServer(t, captivePortalBody, rotatedSetCookie))
			rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
			assertNoResponseWithError(t, rs)
		})
		t.Run("non-200", func(t *testing.T) {
			pointYouTubeGuideAt(t, statusServer(t, http.StatusTooManyRequests))
			rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
			assertNoResponseWithError(t, rs)
		})
		t.Run("bounced through another host", func(t *testing.T) {
			srv, hops, carried := redirectChain(t, "localhost", "Cookie", http.StatusOK, loggedInGuideBody)
			pointYouTubeGuideAt(t, srv)
			rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
			assertNoResponseWithError(t, rs)
			requireBouncedChain(t, hops, carried, "Cookie")
		})
		t.Run("no sapisidhash", func(t *testing.T) {
			srv, _ := countingGuide(t, loggedInGuideBody)
			pointYouTubeGuideAt(t, srv)
			jar := jarWithAuth(t)
			clearCookieValue(jar, PlatformYouTube, "SAPISID")
			assertNoResponseWithError(t, NewRefreshService(jar, 0, nopLogger{}))
		})
	})

	// Not an error, and still nothing to write from: no request was made.
	t.Run("never configured", func(t *testing.T) {
		srv, _ := countingGuide(t, loggedInGuideBody)
		pointYouTubeGuideAt(t, srv)

		rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
		auth, resp, err := rs.youtubeGuideExchange(context.Background())
		if auth || err != nil {
			t.Errorf("auth=%v err=%v, want false/nil", auth, err)
		}
		if resp != nil {
			t.Error("a check that never left the process handed back a response")
		}
	})

	// The control.
	t.Run("readable reply", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t, loggedInGuideBody, rotatedSetCookie))

		rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
		auth, resp, err := rs.youtubeGuideExchange(context.Background())
		if err != nil || !auth {
			t.Fatalf("auth=%v err=%v, want true/nil", auth, err)
		}
		if resp == nil {
			t.Fatal("an authenticated reply handed back no response — the refresh path has nothing to merge")
		}
		// The headers must still be readable after the exchange closed the body,
		// because that is all processYouTubeSetCookies reads.
		if got := resp.Header.Values("Set-Cookie"); len(got) == 0 {
			t.Error("the returned response carries no Set-Cookie headers")
		}
	})
}

func assertNoResponseWithError(t *testing.T, rs *RefreshService) {
	t.Helper()
	auth, resp, err := rs.youtubeGuideExchange(context.Background())
	if err == nil {
		t.Fatal("premise broken: err = nil, want the inconclusive error")
	}
	if auth {
		t.Error("authenticated = true, want false")
	}
	if resp != nil {
		t.Error("an error path handed back a response — a caller could merge Set-Cookie " +
			"headers from an exchange this function refused to read a verdict from")
	}
}

// TestGuideGatesReportAnUnattemptedCheck pins the two gates that must NOT read
// as a verdict, through both entry points.
//
// Post-fold both entry points run the same gates, so this is a test of the
// WIRING as much as of the rule: it fails if either wrapper swallows the
// sentinel, or reaches the network when no request could be formed. The
// hit counter is what makes the second half of that real.
//
// The empty-Cookie-header gate has no case here because it is unreachable
// through the jar, not because it is unchecked: HasAnyYouTubeAuthCookie is true
// only when some youtube entry holds a non-empty value, and GetCookieHeaderFor
// emits a pair for every entry it has — so a jar that passes the first gate
// always produces a non-empty header. The gate stays as a structural guard for
// a future accessor that could return "".
func TestGuideGatesReportAnUnattemptedCheck(t *testing.T) {
	entries := map[string]func(*RefreshService, context.Context) (bool, error){
		"checkAndRefreshYouTube": (*RefreshService).checkAndRefreshYouTube,
		"checkYouTubeAuth":       (*RefreshService).checkYouTubeAuth,
	}

	for name, entry := range entries {
		t.Run("no_sapisidhash/"+name, func(t *testing.T) {
			srv, hits := countingGuide(t, loggedInGuideBody)
			pointYouTubeGuideAt(t, srv)

			jar := jarWithAuth(t)
			clearCookieValue(jar, PlatformYouTube, "SAPISID")
			rs := NewRefreshService(jar, 0, nopLogger{})

			auth, err := entry(rs, context.Background())
			if got := hits.Load(); got != 0 {
				t.Errorf("server hits = %d, want 0 — no Authorization header could be built", got)
			}
			if auth {
				t.Error("authenticated = true, want false")
			}
			if !errors.Is(err, ErrAuthCheckNotAttempted) {
				t.Errorf("err = %v, want it to wrap ErrAuthCheckNotAttempted — "+
					"a check that could not be made is not a verdict", err)
			}
		})

		t.Run("never_configured/"+name, func(t *testing.T) {
			srv, hits := countingGuide(t, loggedInGuideBody)
			pointYouTubeGuideAt(t, srv)

			rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})

			auth, err := entry(rs, context.Background())
			if got := hits.Load(); got != 0 {
				t.Errorf("server hits = %d, want 0 — an unconfigured platform must not be probed", got)
			}
			// The ONLY gate allowed to answer (false, nil): there is no session
			// to have an opinion about, so a silent negative is the truth.
			if auth || err != nil {
				t.Errorf("auth=%v err=%v, want false/nil", auth, err)
			}
		})
	}
}

// --- the declared origin on the write path ---

// TestUnscopedUpdateStaysInsideTheDeclaredOrigin covers the enforcement that
// replaced updateCookieFile's unstated assumption.
//
// A Set-Cookie with no Domain= is host-scoped to the response that carried it,
// and resolveRowUpdate used to resolve that scope by asking isYouTubeDomain —
// correct while processYouTubeSetCookies was the only caller, and silently wrong
// in the DESTROY direction the day a second one appears. The origin is now
// declared, so the same unscoped deletion reaches different rows depending on
// where it came from.
//
// Discriminates in the Google direction: against the hardcoded rule the
// .youtube.com row was the one deleted and the .google.com row was the one that
// survived — exactly inverted. The YouTube direction is a regression guard on
// the behaviour that must not have changed.
func TestUnscopedUpdateStaysInsideTheDeclaredOrigin(t *testing.T) {
	const initial = "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-google\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-youtube\n"

	cases := []struct {
		name      string
		origin    cookieOrigin
		wantGone  string // the domain whose row the deletion may reach
		wantAlive string // the domain it may not
	}{
		{"declared youtube", originYouTube, ".youtube.com", ".google.com"},
		{"declared google", originGoogle, ".google.com", ".youtube.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

			// No Domain= on the key: the unscoped case, the only one the origin
			// decides.
			updates := map[cookieUpdateKey]cookieUpdate{
				{Name: "SAPISID"}: {Delete: true},
			}
			if err := rs.updateCookieFile(updates, tc.origin); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}

			rows := rowsNamed(readCookieRows(t, path), "SAPISID")
			var gone, alive bool
			for _, r := range rows {
				switch r.domain {
				case tc.wantGone:
					gone = true
				case tc.wantAlive:
					alive = true
					if r.value == "" {
						t.Errorf("the %s row survived as an empty shell: %q", r.domain, r.raw)
					}
				}
			}
			if gone {
				// The premise: without this, "the other row survived" is also
				// what a deletion that matched nothing at all would produce.
				t.Errorf("the %s row survived a deletion declared as coming from %s", tc.wantGone, tc.origin)
			}
			if !alive {
				t.Errorf("the %s row was destroyed by an unscoped deletion from %s — "+
					"a host-scoped Set-Cookie reached outside its own site", tc.wantAlive, tc.origin)
			}
		})
	}
}

// TestUnscopedRefreshCrossesDomainsOnlyInsideTheDeclaredPlatform covers the
// other half of the same enforcement: sameCookiePlatform's Domain-less default,
// which used to be the literal "google".
//
// Rule 3 is the grow-broadly half — a value refresh may cross domain variants
// within ONE platform, which is what keeps a .google.com twin from going stale
// while .youtube.com moves on. A Domain-less update has no domain of its own to
// classify, so the platform has to come from the caller. Hardcoded, a Twitch
// caller's unscoped refresh would classify as Google and land on Google rows.
//
// Discriminates: against the hardcoded default the .youtube.com row is rewritten
// with the Twitch caller's value. The Google case is the control that stops
// "never cross" from passing — the asymmetry is deliberate and the grow half
// must still grow.
//
// The ROW COUNT is asserted alongside the value, and it is not decoration. An
// update that no row accepts is not dropped: it falls through to the insertion
// loop, which used to invent a domain from the cookie NAME alone and append a
// brand-new row. Checking one row's value cannot see that — rowFor returns the
// first match — so "the Twitch caller did not overwrite the YouTube row" was
// true while the same caller was quietly adding a .google.com row of its own.
// Declining to match and declining to write have to be asserted together.
func TestUnscopedRefreshCrossesDomainsOnlyInsideTheDeclaredPlatform(t *testing.T) {
	const initial = "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-youtube-sid\n"

	cases := []struct {
		name      string
		origin    cookieOrigin
		wantValue string
	}{
		// google.com and youtube.com are one credential platform, so an
		// unscoped refresh from google.com re-syncs the YouTube twin. Unchanged.
		{"google reaches the youtube twin", originGoogle, "fresh-from-the-caller"},
		// twitch.tv is another platform: growing onto its rows would be
		// destruction, and so would growing off them.
		{"twitch does not", originTwitch, "fixture-youtube-sid"},
		// An undeclared origin classifies as nothing and matches nothing — the
		// narrow direction, which is the safe one.
		{"an undeclared origin does not", cookieOrigin(""), "fixture-youtube-sid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

			updates := map[cookieUpdateKey]cookieUpdate{
				{Name: "SID"}: {Value: "fresh-from-the-caller", Expiry: 2100000000},
			}
			if err := rs.updateCookieFile(updates, tc.origin); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}

			rows := readCookieRows(t, path)
			if got := len(rowsNamed(rows, "SID")); got != 1 {
				var where []string
				for _, r := range rowsNamed(rows, "SID") {
					where = append(where, r.domain)
				}
				t.Errorf("the file holds %d SID rows %v, want the 1 it started with — "+
					"an update from %q that no row accepted was INSERTED under a domain "+
					"invented from the cookie name", got, where, tc.origin)
			}
			row := rowFor(t, rows, "SID", ".youtube.com")
			if row.value != tc.wantValue {
				t.Errorf(".youtube.com SID = %q, want %q — an unscoped update from %q "+
					"was classified into the wrong platform", row.value, tc.wantValue, tc.origin)
			}
		})
	}
}

// TestInsertionStaysInsideTheDeclaredPlatform covers the third place the origin
// decides something, and the one that is easy to miss.
//
// Declining to MATCH is not declining to WRITE. Every update that
// resolveRowUpdate turns down falls through to the insertion loop, and that loop
// derives a domain from the cookie NAME alone — it cannot tell whose response
// the name arrived in. So before this check existed, a caller whose updates were
// refused by rules 2 and 3 still appended a brand-new row, under a domain nobody
// declared: an originTwitch caller's unscoped "SID" landed a .google.com row in
// the Google jar. That is WIDER than the hardcoded youtube.com assumption the
// origin parameter replaced, which is the opposite of what declaring it is for.
//
// The same-platform case is the control. youtube.com and google.com are one
// credential platform, so a youtube.com response must still be able to insert a
// .google.com row — that is the grow-broadly half, and refusing it would break
// every real rotation.
func TestInsertionStaysInsideTheDeclaredPlatform(t *testing.T) {
	cases := []struct {
		name       string
		key        cookieUpdateKey
		origin     cookieOrigin
		wantDomain string // "" means: nothing may be written
	}{
		// The fallback path: no Domain=, so the domain is invented from the name
		// (isGoogleOnlyAuthName routes SID to .google.com).
		{"unscoped name from a twitch caller", cookieUpdateKey{Name: "SID"}, originTwitch, ""},
		{"unscoped name from no caller at all", cookieUpdateKey{Name: "SID"}, cookieOrigin(""), ""},
		{"unscoped name from a google caller", cookieUpdateKey{Name: "SID"}, originGoogle, ".google.com"},

		// The explicit-Domain path: checked on the same rule, so a key carrying a
		// cross-platform Domain= cannot slip past by naming its own destination.
		{"explicit twitch domain from a youtube caller",
			cookieUpdateKey{Name: "auth-token", Domain: ".twitch.tv"}, originYouTube, ""},
		{"explicit youtube domain from no caller at all",
			cookieUpdateKey{Name: "LOGIN_INFO", Domain: ".youtube.com"}, cookieOrigin(""), ""},
		// A domain on no known platform is refused too. The zero-origin case is
		// the one that isolates the `insertPlatform == ""` clause: an unplaceable
		// domain and an undeclared origin both classify as "", so a bare equality
		// test would read the pair as a MATCH and write the row. The youtube case
		// beside it is a regression guard only — it is refused either way, since
		// "" and "google" differ.
		{"unplaceable domain from no caller at all",
			cookieUpdateKey{Name: "LOGIN_INFO", Domain: ".example.invalid"}, cookieOrigin(""), ""},
		{"unplaceable domain from a youtube caller",
			cookieUpdateKey{Name: "LOGIN_INFO", Domain: ".example.invalid"}, originYouTube, ""},

		// The controls — one platform, two domains, still allowed.
		{"explicit google domain from a youtube caller",
			cookieUpdateKey{Name: "SAPISID", Domain: ".google.com"}, originYouTube, ".google.com"},
		{"explicit youtube domain from a youtube caller",
			cookieUpdateKey{Name: "LOGIN_INFO", Domain: ".youtube.com"}, originYouTube, ".youtube.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const initial = "# Netscape HTTP Cookie File\n"
			rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

			updates := map[cookieUpdateKey]cookieUpdate{
				tc.key: {Value: "inserted-value", Expiry: 2100000000},
			}
			if err := rs.updateCookieFile(updates, tc.origin); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}

			rows := rowsNamed(readCookieRows(t, path), tc.key.Name)
			if tc.wantDomain == "" {
				if len(rows) != 0 {
					t.Errorf("origin %q inserted %q under %q; nothing may be written outside the declared platform",
						tc.origin, tc.key.Name, rows[0].domain)
				}
				// The file must be untouched, not merely free of this row.
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != initial {
					t.Errorf("the file changed on a refused insertion:\n%q", string(after))
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("origin %q wrote %d %q rows, want 1 under %q — the control is not controlling",
					tc.origin, len(rows), tc.key.Name, tc.wantDomain)
			}
			if rows[0].domain != tc.wantDomain {
				t.Errorf("%q landed under %q, want %q", tc.key.Name, rows[0].domain, tc.wantDomain)
			}
			if rows[0].value != "inserted-value" {
				t.Errorf("%q value = %q, want the inserted value", tc.key.Name, rows[0].value)
			}
		})
	}
}

// --- 11(d): the last two unguarded essentialYouTubeCookies[name] reads ---

// levelLogger records Info and Warn messages separately. captureLogger next door
// records Info only, and the second half of this test needs the Warn side.
type levelLogger struct {
	mu   sync.Mutex
	info []string
	warn []string
}

func (l *levelLogger) Debug(msg string, args ...any) {}
func (l *levelLogger) Error(msg string, args ...any) {}

func (l *levelLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.info = append(l.info, msg)
}

func (l *levelLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warn = append(l.warn, msg)
}

func (l *levelLogger) count(msgs []string, sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

func (l *levelLogger) infoCount(sub string) int { return l.count(l.info, sub) }
func (l *levelLogger) warnCount(sub string) int { return l.count(l.warn, sub) }

// TestEssentialCookieLoggingIsDomainGuarded covers 11(d): the two
// essentialYouTubeCookies[name] reads inside updateCookieFile now require the
// ROW's domain to be YouTube or Google as well.
//
// Several names in that map — PREF, CONSENT, YSC, LOGIN_INFO, SID and the rest
// of the Google auth set — are not YouTube-exclusive strings, so an unguarded
// name lookup calls any row carrying one of them an essential YouTube cookie.
// Both reads select log SEVERITY only and gate no mutation, which is why nothing
// was wrong on the wire; the guard exists because this was the last copy of a
// shape the plan removed from jar.Load and isEssentialCookie, and a reader would
// reasonably lift it somewhere it does decide something.
//
// The rows are built directly through updateCookieFile because they cannot
// arrive any other way: CookieJar.Load's domain-aware admission no longer lets a
// .twitch.tv row named SID into the YouTube jar at all.
//
// Discriminates: against the unguarded reads the Twitch subtests logged at Info
// and Warn respectively. The Google subtests are the controls that stop "log
// nothing, ever" from passing.
func TestEssentialCookieLoggingIsDomainGuarded(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		cases := []struct {
			name     string
			row      string
			domain   string
			origin   cookieOrigin
			wantInfo int
		}{
			{"twitch row is not an essential youtube cookie",
				".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-twitch-sid\n",
				".twitch.tv", originTwitch, 0},
			{"google row is",
				".google.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-google-sid\n",
				".google.com", originGoogle, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				log := &levelLogger{}
				rs, _, path := newSetCookieFixture(t, log, "# Netscape HTTP Cookie File\n"+tc.row)

				// Scoped to the row's own domain, so rule 1 matches it whatever
				// the origin says — the deletion branch is reached in both cases
				// and only the log severity can differ.
				updates := map[cookieUpdateKey]cookieUpdate{
					{Name: "SID", Domain: tc.domain}: {Delete: true},
				}
				if err := rs.updateCookieFile(updates, tc.origin); err != nil {
					t.Fatalf("updateCookieFile: %v", err)
				}

				if got := rowsNamed(readCookieRows(t, path), "SID"); len(got) != 0 {
					t.Fatalf("premise broken: the row was not deleted, so the deletion branch never ran: %q", got[0].raw)
				}
				if got := log.infoCount("deleted an essential cookie"); got != tc.wantInfo {
					t.Errorf("Info logs naming an essential deletion = %d, want %d (info=%v)", got, tc.wantInfo, log.info)
				}
			})
		}
	})

	t.Run("refused empty value", func(t *testing.T) {
		cases := []struct {
			name     string
			row      string
			domain   string
			origin   cookieOrigin
			wantWarn int
		}{
			{"twitch row is not an essential youtube cookie",
				".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-twitch-sid\n",
				".twitch.tv", originTwitch, 0},
			{"google row is",
				".google.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-google-sid\n",
				".google.com", originGoogle, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				log := &levelLogger{}
				rs, _, path := newSetCookieFixture(t, log, "# Netscape HTTP Cookie File\n"+tc.row)

				// Empty value, no expiry and no Max-Age: the refusal branch.
				updates := map[cookieUpdateKey]cookieUpdate{
					{Name: "SID", Domain: tc.domain}: {Value: "", Expiry: 2000000000},
				}
				if err := rs.updateCookieFile(updates, tc.origin); err != nil {
					t.Fatalf("updateCookieFile: %v", err)
				}

				row := rowFor(t, readCookieRows(t, path), "SID", tc.domain)
				if row.value == "" {
					t.Fatal("premise broken: the existing value was blanked, so the refusal branch never ran")
				}
				if got := log.warnCount("refused to blank an essential cookie"); got != tc.wantWarn {
					t.Errorf("Warn logs naming a refused blanking = %d, want %d (warn=%v)", got, tc.wantWarn, log.warn)
				}
			})
		}
	})
}
