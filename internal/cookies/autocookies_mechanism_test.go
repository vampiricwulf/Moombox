package cookies

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefreshResultCarriesTheMechanismThatRan is F2 at the source.
//
// Every post-flight sentence on both surfaces opened with "Browser cookie
// refresh ..." after a pass that launched nothing, because the only thing
// either surface had to go on was cookies.acquisition — and the mode is not the
// answer. A host with no browser installed imports in "auto" mode and always
// has, long before the setting existed. So the pass reports what it actually
// did, and this is the assertion that it does.
//
// FIVE ROWS, and the empty one is the point of the design. Mechanism is
// stamped where the path is CHOSEN (the importedFromProfile decision), so a
// pass that declined above that decision reports "" rather than guessing — and
// "" is what the surfaces fall back to the mode for. A field that guessed would
// be worse than the mode alone, because it would look authoritative.
//
// The second row is the boundary, and it is where a first draft of this test
// was wrong: the browser branch's empty-jar gate (`len(refreshPlatforms()) ==
// 0`) is a decline that sits BELOW the decision, so it carries "browser" — the
// branch was chosen and then declined, and the mode fallback would have said
// the same. The decline that is genuinely above the decision, and cheap to
// drive, is the single-flight slot: refreshCmd non-nil means "already
// refreshing", and the pass returns before it looks at anything.
//
// The fixture is TestAcquisitionModeSelectsTheRefreshPath's, deliberately: same
// synthetic WAL profile, same gatedBrowser at a path that does not exist, so
// nothing here can execute a browser however the branch goes.
//
// The fifth row is the abort exits, and it is the one an earlier pass at this
// test missed entirely (arc review F-2): eight of the eighteen returns are
// ABORTS — real work was attempted and stopped on an error — and none of the
// four rows above drives one, so a defer that stamped only nil-error returns
// (`if retErr == nil { out.Mechanism = mechanism }`) passed the whole package.
// That is not a cosmetic gap: app_update.go's error arm renders
// `mechanismLabel + " failed: " + err` off this exact field, so an abort on a
// browserless host would silently revert to "Browser cookie refresh failed"
// under that mutant, on the surface F2 exists to fix.
//
// Mutations: delete the defer that stamps it; initialise mechanism to a
// non-empty value; swap the two constants at the decision; stamp only when
// retErr == nil.
func TestRefreshResultCarriesTheMechanismThatRan(t *testing.T) {
	newService := func(t *testing.T, mode string, jar *CookieJar) *AutoCookieService {
		t.Helper()
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			jar, nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s
	}

	t.Run("a pass that declined above the decision reports no mechanism", func(t *testing.T) {
		// The single-flight slot is held (the same sentinel refreshFirefox
		// claims), so the pass declines at the "already refreshing" gate —
		// above the launch-vs-import decision, in EVERY mode. Profile mode
		// here, so a stamp that leaked would be the import's, which is not
		// what the mode fallback says for auto. Nothing was chosen, so nothing
		// is claimed.
		s := newService(t, AcquisitionProfile, NewCookieJar())
		s.mu.Lock()
		s.refreshCmd = &exec.Cmd{}
		s.mu.Unlock()
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if result.Ran {
			t.Fatal("premise broken: the in-flight gate no longer declines, so this row is not testing a decline")
		}
		if result.Mechanism != "" {
			t.Errorf("Mechanism = %q for a pass that never chose a path, want \"\" — a guessed "+
				"mechanism reads as authoritative and is exactly the claim F2 is about", result.Mechanism)
		}
	})

	t.Run("a browser-path decline on an empty jar reports the browser", func(t *testing.T) {
		// auto + a browser + an empty jar: the browser branch is CHOSEN, and
		// then its refreshPlatforms() gate declines. That gate is below the
		// decision, so the stamp has already happened — and "browser" is the
		// truth of it. The mode fallback says the same for this row, which is
		// why nothing observable turns on it; what this pins is WHERE the
		// stamp sits, so a later reader does not "fix" the row above by
		// moving the stamp into the branches.
		s := newService(t, AcquisitionAuto, NewCookieJar())
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if result.Ran {
			t.Fatal("premise broken: the empty-jar gate no longer declines")
		}
		if result.Mechanism != RefreshMechanismBrowser {
			t.Errorf("Mechanism = %q, want %q — the browser path was chosen before it declined",
				result.Mechanism, RefreshMechanismBrowser)
		}
	})

	t.Run("the import path names itself", func(t *testing.T) {
		s := newService(t, AcquisitionProfile, NewCookieJar())
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if !result.Ran {
			t.Fatal("premise broken: profile mode no longer takes the import path")
		}
		if result.Mechanism != RefreshMechanismProfileImport {
			t.Errorf("Mechanism = %q, want %q — this pass launched nothing and every sentence "+
				"about it still said \"Browser cookie refresh\"", result.Mechanism, RefreshMechanismProfileImport)
		}
		// The reason both surfaces keep their browser wording on the !Renewed
		// arm: renewed := importedFromProfile || browserActed, and browserActed
		// starts true and is cleared only inside the browser branch, so an
		// import that reaches a verdict always renewed — held shut by two
		// guards, each sufficient alone. This pins the PROPERTY: dropping
		// `importedFromProfile ||` alone survives it (verified), because the
		// initialiser still holds; only a change that lets an import reach a
		// verdict with Renewed false fails it, and that is the change that
		// would start telling a profile-mode operator their BROWSER could not
		// be confirmed.
		if !result.Renewed {
			t.Error("an import that ran reported Renewed = false, which makes the browser-worded " +
				"\"could not confirm the browser refreshed them\" arm reachable for a pass that " +
				"launched no browser")
		}
	})

	t.Run("the browser path names itself", func(t *testing.T) {
		// A jar with YouTube auth puts a platform in refreshPlatforms(), so the
		// pass gets past the gate that declined the first row and takes the
		// launch branch — which cannot execute anything, and does not need to:
		// the stamp happens at the decision, above every launch.
		s := newService(t, AcquisitionAuto, jarWithAuth(t))
		result, _ := s.RefreshCookiesDetailed(context.Background())
		if !result.Ran {
			t.Fatal("premise broken: the browser branch declined, so no mechanism was ever chosen")
		}
		if result.Mechanism != RefreshMechanismBrowser {
			t.Errorf("Mechanism = %q, want %q", result.Mechanism, RefreshMechanismBrowser)
		}
	})

	t.Run("an abort on the browser path still reports the browser", func(t *testing.T) {
		// The abort is the launch guard's own refusal
		// (validateBrowserProfileDirForLaunch), driven through the FULL pass
		// rather than by calling refreshFirefox directly the way
		// TestLaunchGuardHoldsEveryLaunchSiteInEveryMode does. It needs a
		// REAL directory — the earlier "browser profile not found" decline
		// would otherwise fire first and this row would never reach the
		// decision at all — that ALSO matches a dangerous substring:
		// existingDangerousProfileDir is exactly that fixture. With a
		// detected browser and BrowserLaunchAllowed left at its default,
		// importedFromProfile stays false, so refreshFirefox's very first
		// line — `if s.profileDirErr != nil { return "", false,
		// s.profileDirErr }` — returns the guard's own error before any
		// exec.Command is built. No browser is launched either way.
		//
		// This is the fifth row's whole point: an ABORT is still a CHOSEN
		// path (the decision ran before the branch could fail), and
		// app_update.go's error arm (`mechanismLabel + " failed: " + …`)
		// reads Mechanism for exactly this exit.
		dir := existingDangerousProfileDir(t)
		s := NewAutoCookieService(dir, filepath.Join(t.TempDir(), "cookies.txt"),
			jarWithAuth(t), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return AcquisitionAuto }

		result, err := s.RefreshCookiesDetailed(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refusing to launch") {
			t.Fatalf("premise broken: want the launch guard's own refusal, got %v", err)
		}
		if !result.Ran {
			t.Fatal("premise broken: an abort must still report Ran = true (refreshAborted, not refreshDeclined)")
		}
		if result.Mechanism != RefreshMechanismBrowser {
			t.Errorf("Mechanism = %q, want %q — an abort on the browser path must still name the "+
				"mechanism that was chosen before it failed", result.Mechanism, RefreshMechanismBrowser)
		}
	})
}
