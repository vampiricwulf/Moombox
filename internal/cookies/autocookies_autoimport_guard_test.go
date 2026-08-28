package cookies

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ytAuthCookieFile writes a cookies.txt holding YouTube auth rows and returns
// its path. The values are fixtures, not credentials.
func ytAuthCookieFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfixture\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tfixture\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture cookies.txt: %v", err)
	}
	return path
}

// guardSkipped is the exact line the periodic loop logs when the rule stands
// a tick down. Compared for equality, never containment — the loop has three
// skip lines and two of them share a prefix.
const guardSkipped = "periodic auto-cookie refresh skipped — a browser-free import " +
	"may only run when there is nothing to lose"

// TestPeriodicBrowserlessTickImportsOnlyWithNothingToLose is the owner's rule
// at its second automatic site.
//
// ONE rule for every automatic browser-free import: run only when there is no
// cookies.txt to lose. The boot seed is the first site; this is the other. On a
// browserless host with auto_enabled on, every tick IS an import, and nothing
// between two ticks changes a mounted profile — so a tick that finds a cookie
// file would re-read identical bytes, on a timer, over credentials that may be
// working.
//
// This was never an exotic configuration: the Settings page's auto-cookie
// switch just writes the config, so a browserless operator toggling it is one
// click away from here.
func TestPeriodicBrowserlessTickImportsOnlyWithNothingToLose(t *testing.T) {
	run := func(t *testing.T, cookiePath string) (*recordingCookieLogger, *atomic.Int64) {
		t.Helper()
		log := &recordingCookieLogger{}
		s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath,
			NewCookieJar(), log)
		s.detectBrowser = func() *DetectedBrowser { return nil } // a browserless host
		var verified atomic.Int64
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		const interval = 20 * time.Millisecond
		s.StartPeriodicRefresh(ctx, interval)
		// Wait for a decision either way, then let more ticks run so a rule
		// that only holds on the first one fails.
		waitFor(func() bool {
			return verified.Load() > 0 || log.count(guardSkipped) > 0
		}, 10*time.Second)
		time.Sleep(5 * interval)
		return log, &verified
	}

	t.Run("no cookies.txt — imports once, then stands down", func(t *testing.T) {
		// THE JUNCTION GUARD, and the rule's own convergence.
		//
		// Without this row the two below prove only that the loop is dead. And
		// the second half is the rule closing itself: the first import WRITES
		// cookies.txt, so every later tick finds something to lose and stops.
		// A browserless install therefore reads its profile once and then goes
		// quiet on its own — no "re-read identical bytes on a timer", with no
		// separate latch to get wrong.
		log, verified := run(t, filepath.Join(t.TempDir(), "cookies.txt"))
		afterFirstWait := verified.Load()
		if afterFirstWait == 0 {
			t.Fatal("a browserless tick with no cookie file did not import. That install has nothing " +
				"to lose and everything to gain from reading the profile — refusing it is how a " +
				"container ends up with no cookies at all")
		}
		if log.count(guardSkipped) == 0 {
			t.Error("the loop imported and then never stood down. The import writes cookies.txt, so " +
				"from the next tick on there IS something to lose and the rule must say so")
		}
		// Nothing more may happen from here: the file now has rows.
		time.Sleep(200 * time.Millisecond)
		if got := verified.Load(); got != afterFirstWait {
			t.Errorf("imports kept running after cookies.txt was written (%d -> %d). The rule has to "+
				"see its own output, or a browserless install re-imports on every tick forever",
				afterFirstWait, got)
		}
	})

	t.Run("cookies.txt holds cookies — stands down", func(t *testing.T) {
		log, verified := run(t, ytAuthCookieFile(t))
		if got := verified.Load(); got != 0 {
			t.Errorf("a browserless tick imported over an existing cookies.txt %d times. Nothing "+
				"between two ticks changes a mounted profile, so this re-reads identical bytes on a "+
				"timer over credentials that may be working — R F is how those get replaced", got)
		}
		if log.count(guardSkipped) == 0 {
			t.Error("the loop never reached the guard, so the zero above proves nothing")
		}
	})

	t.Run("cookies.txt unreadable — stands down", func(t *testing.T) {
		// Arc 2's lesson at this site too: unreadable is not absent.
		path := ytAuthCookieFile(t)
		real := readCookieFile
		readCookieFile = func(name string) ([]byte, error) {
			if name == path {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
			}
			return real(name)
		}
		t.Cleanup(func() { readCookieFile = real })

		log, verified := run(t, path)
		if got := verified.Load(); got != 0 {
			t.Errorf("a browserless tick imported %d times over a cookies.txt it could not read. A "+
				"permission or mount blip is not an absent file — see ErrCookieFileUnreadable", got)
		}
		if log.count(guardSkipped) == 0 {
			// Worded for what actually goes wrong here. If the rule starts
			// reading an unreadable file as an absent one, the tick is NOT
			// stood down — it runs, and the only thing that then prevents the
			// loss is RefreshCookiesDetailed's own merge abort. That is a real
			// second line of defence and it is why the count above can still be
			// zero, which is exactly why this assertion cannot be dropped: the
			// rule must refuse on its own, not be rescued downstream.
			t.Error("the tick was never stood down by the rule. Either the loop is dead, or an " +
				"unreadable cookies.txt is being read as an absent one and the pass ran — leaving " +
				"the merge's ErrCookieFileUnreadable abort as the only thing between a permission " +
				"blip and a destroyed cookie file")
		}
	})
}

// TestPeriodicTickWithABrowserIgnoresTheImportGuard is the scope boundary that
// keeps the timer worth having.
//
// The rule is about IMPORTS. Refreshing a live cookies.txt through a headless
// browser is the entire purpose of this timer, and every install that has a
// browser also has a cookies.txt — so a guard applied to the whole tick rather
// than to the browser-free case would switch the feature off for everyone who
// uses it, silently, with every cookies test still green.
//
// Driven through the chromium seam, so the browser is "present" and reached
// without a process ever starting.
func TestPeriodicTickWithABrowserIgnoresTheImportGuard(t *testing.T) {
	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}

	log := &recordingCookieLogger{}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar, log)
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
	}
	var launched atomic.Int64
	realChromium := refreshChromiumCookies
	refreshChromiumCookies = func(_ *AutoCookieService, _ context.Context, _ *DetectedBrowser) (string, bool, error) {
		launched.Add(1)
		return "", false, errors.New("stubbed: no browser is started in tests")
	}
	t.Cleanup(func() { refreshChromiumCookies = realChromium })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartPeriodicRefresh(ctx, 20*time.Millisecond)

	if !waitFor(func() bool { return launched.Load() > 0 }, 10*time.Second) {
		t.Fatalf("the periodic timer never ran a browser refresh on a host that HAS a browser and a "+
			"populated cookies.txt — which is the only configuration this timer exists for. The "+
			"browser-free import rule must not apply to a tick that would launch a browser "+
			"(LastError=%q, guard stand-downs=%d)", lastErrorSnapshot(s), log.count(guardSkipped))
	}
}

// TestManualRefreshImportsRegardlessOfTheCookieFile is the boundary the rule
// must never cross.
//
// R F, the dashboard's shift+click and the Settings page's profile-import
// button are explicit gestures, and "update the mounted profile, then trigger
// the import" is the designated workflow on a browserless host. Importing over
// a cookies.txt that already has rows is not a side effect there — it is the
// whole point, and it is the only cookie path a container has.
//
// It covers the RECOVERY path too, which is automatic and exempt on different
// grounds: it fires only on a conclusive not-authenticated, so the guard's
// premise ("these credentials might be working") is already false when it runs.
// Both exemptions live or die together, because all four external callers reach
// the same function — which is asserted here, at that function.
//
// Asserted through RefreshCookiesDetailed, which is what all four call.
//
// PAIRED WITH TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers — DO NOT
// DELETE EITHER AS REDUNDANT. That one reads the call graph, so it puts a new
// caller in the diff even on a path no test exercises; but it matches selector
// calls, so a method value (`rule := s.automaticImportGuard; rule()`) evades
// it. That mutation was run: the structural test passed and this one killed it.
// Each covers the other's blind spot and neither covers its own.
func TestManualRefreshImportsRegardlessOfTheCookieFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cookiePath func(t *testing.T) string
	}{
		{"no cookies.txt", func(t *testing.T) string { return filepath.Join(t.TempDir(), "cookies.txt") }},
		{"cookies.txt already holds auth cookies", ytAuthCookieFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), tc.cookiePath(t),
				NewCookieJar(), &recordingCookieLogger{})
			s.detectBrowser = func() *DetectedBrowser { return nil }
			var verified atomic.Int64
			s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
			s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

			result, err := s.RefreshCookiesDetailed(context.Background())
			if err != nil {
				t.Fatalf("the manual path must not error here: %v", err)
			}
			if !result.Ran {
				t.Error("R F / shift+click declined. The manual triggers are explicit gestures and " +
					"must import whatever the cookie file holds — gating them on an absent " +
					"cookies.txt removes the only cookie path a browserless host has")
			}
			if verified.Load() == 0 {
				t.Error("the manual path did not import")
			}
		})
	}
}

// TestRowCounterOverCountsByConstruction pins the sentence automaticImportGuard's
// doc rests its correctness on.
//
// The rule does not need to be accurate, it needs zero false "nothing to lose"
// answers — a wrong "nothing to lose" imports over credentials the operator may
// not miss until a recording fails, while a wrong "something to lose" costs one
// ungated R F. countNetscapeCookieRows delivers that because it counts LINES
// rather than valid cookies, so everything it cannot understand still counts.
//
// Asserted in BOTH directions. Over-counting alone would be satisfied by a
// counter that returns 1 for everything, which would mean no install ever seeds
// — including the container this whole path exists for.
func TestRowCounterOverCountsByConstruction(t *testing.T) {
	t.Run("anything unparseable still counts", func(t *testing.T) {
		for _, tc := range []struct{ name, body string }{
			{"a malformed row", "not\ta\tvalid\tcookie\n"},
			{"a row for an unrelated domain", ".example.com\tTRUE\t/\tFALSE\t0\tfoo\tbar\n"},
			{"an httponly row behind its # prefix", "#HttpOnly_.x.com\tTRUE\t/\tTRUE\t0\tA\tb\n"},
			{"an expired row", ".youtube.com\tTRUE\t/\tTRUE\t1\tSID\tv\n"},
			{"a single junk word", "garbage\n"},
		} {
			if got := countNetscapeCookieRows(tc.body); got == 0 {
				t.Errorf("%s counted 0. Anything the counter cannot understand must still read as "+
					"'something to lose' — under-counting is the direction that destroys a cookie "+
					"file, and the guard has no second opinion to fall back on", tc.name)
			}
		}
	})

	t.Run("but a file with nothing in it counts zero", func(t *testing.T) {
		for _, tc := range []struct{ name, body string }{
			{"empty", ""},
			{"header comment only", "# Netscape HTTP Cookie File\n"},
			{"blank lines only", "\n\n  \n\t\n"},
		} {
			if got := countNetscapeCookieRows(tc.body); got != 0 {
				t.Errorf("%s counted %d. A file with no cookies in it has nothing to lose, and "+
					"reading it as occupied means a freshly-mounted container profile is never "+
					"imported at all", tc.name, got)
			}
		}
	})
}

// TestAutomaticImportGuardAnswersTheRuleAndNothingElse keeps the rule narrow.
//
// Which sites may call it is asserted structurally next door, in
// TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers.
func TestAutomaticImportGuardAnswersTheRuleAndNothingElse(t *testing.T) {
	profile := writeWALCookieProfile(t, youtubeAuthRows())

	// The guard must NOT re-answer questions its callers already asked — a
	// browser being present is decideStartupSeed's business, not the rule's,
	// and the periodic site asks it separately so it can scope the rule to a
	// browser-free tick. If the guard grew that check, the periodic site would
	// stand down on every host that has a browser.
	s := NewAutoCookieService(profile, filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	if got := s.automaticImportGuard(); got != autoImportOK {
		t.Errorf("automaticImportGuard() = %v with a browser installed and no cookies.txt, want %v. "+
			"The rule answers one question — is there anything to lose — and the periodic site "+
			"depends on that: it asks about the browser itself so the rule applies only to a "+
			"browser-free tick", got, autoImportOK)
	}
}
