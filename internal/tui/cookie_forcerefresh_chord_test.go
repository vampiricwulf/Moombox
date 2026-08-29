package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	webassets "github.com/vampiricwulf/Moombox/web"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// rfIsOffered reports whether R F is reachable by the three routes an operator
// has: the chord dispatcher, the action menu, and the help overlay.
//
// All three, because they are three separate reads of OnForceRefreshCookies and
// a fix that restored one would look complete from the others. Help is derived
// from the menu, so it is asserted through the same path the overlay uses
// rather than by re-deriving the rule here.
func rfIsOffered(t *testing.T, app *App) (dispatch, menu, help bool) {
	t.Helper()

	if _, cmd := app.dispatchAction("R F", nil); cmd != nil {
		dispatch = true
	}

	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}
	for _, it := range items {
		if strings.TrimSpace(it.Chord) == "R F" {
			menu = true
		}
	}

	h := NewHelpModel()
	h.SetMenuItems(items)
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == "R F" {
				help = true
			}
		}
	}
	return dispatch, menu, help
}

// TestForceRefreshChordExistsWheneverItIsWired is the junction guard for the
// wiring change in cmd/moombox, and the reason that change is not cosmetic.
//
// OnForceRefreshCookies used to be assigned only `if s.cfg.Cookies.AutoEnabled`,
// read once at process start. A nil callback is not "the chord does nothing" —
// dispatchAction, buildMenuItems and the help overlay each test the field, so
// the chord DOES NOT EXIST. On an install with the flag off, an operator told
// their cookies were dead had no key to press and no entry naming one; on an
// install where the flag was turned off later, the chord stayed and still
// launched the browser they had just disabled. Both are now decided by the
// callback's presence alone, which cmd/moombox sets unconditionally.
//
// The nil row is what gives the wired row its meaning. Without it, "R F is
// offered" would be satisfied by a build that offers every chord unconditionally
// and could not detect the regression at all.
func TestForceRefreshChordExistsWheneverItIsWired(t *testing.T) {
	t.Run("wired", func(t *testing.T) {
		app := NewApp()
		app.OnForceRefreshCookies = func() (cookies.RefreshResult, error) {
			return cookies.RefreshResult{}, nil
		}

		dispatch, menu, help := rfIsOffered(t, app)
		if !dispatch {
			t.Error("R F dispatched no command although the callback is wired — the chord is inert")
		}
		if !menu {
			t.Error("R F is absent from the action menu although the callback is wired")
		}
		if !help {
			t.Error("R F is absent from help although the callback is wired — an operator cannot " +
				"discover a chord that is documented nowhere")
		}
	})

	t.Run("not wired", func(t *testing.T) {
		app := NewApp()
		app.OnForceRefreshCookies = nil

		dispatch, menu, help := rfIsOffered(t, app)
		if dispatch || menu || help {
			t.Fatalf("premise lost: R F is offered with a nil callback (dispatch=%v menu=%v help=%v). "+
				"The wired row above then proves nothing, and the startup gate this test exists to "+
				"prohibit would have been harmless after all", dispatch, menu, help)
		}
	})
}

// TestForceRefreshFallsBackToTheRecheck pins R F's bottom rung.
//
// R F is a ladder and must never dead-end: launch the headless browser when
// cookies.auto_enabled permits one and a browser is there; otherwise import the
// browser profile immediately; and when there is no profile at all, run the
// in-process Go refresh — the same thing R C runs — and say so.
//
// Two halves, and the second is the one that matters. A message announcing a
// fallback that then does nothing would be this plan's signature defect in a
// new place, so the returned command is EXECUTED and required to produce the
// recheck's own message. The sentence alone would satisfy a broken build.
func TestForceRefreshFallsBackToTheRecheck(t *testing.T) {
	for _, sentinel := range []error{cookies.ErrProfileNotFound, cookies.ErrNoBrowserFound} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			app := NewApp()
			recheckCalls := 0
			app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
				recheckCalls++
				return cookies.RefreshOK, cookies.RefreshUnknown, "", ""
			}

			// Wrapped, because both sentinels reach the TUI wrapped in context
			// by RefreshCookiesDetailed. A check written with == would pass here
			// on a bare sentinel and fail in production.
			_, cmd := app.Update(cookieForceRefreshResultMsg{
				Err: fmt.Errorf("refresh pass gave up: %w", sentinel),
			})

			if app.feedbackMsg != "No browser profile found, running R C instead..." {
				t.Errorf("R F feedback = %q, want the fallback sentence", app.feedbackMsg)
			}
			if cmd == nil {
				t.Fatal("R F announced a fallback and returned no command — the sentence is the whole " +
					"of the behaviour, and nothing actually refreshes")
			}
			if got := cmd(); recheckCalls != 1 {
				t.Fatalf("the fallback command ran the recheck %d times, want 1 (it produced %T) — "+
					"R F must run the same in-process refresh R C runs, not a second implementation",
					recheckCalls, got)
			} else if _, ok := got.(cookieRecheckResultMsg); !ok {
				t.Errorf("the fallback produced %T, want cookieRecheckResultMsg so its result renders "+
					"through the shared cookies.RecheckReport", got)
			}
		})
	}

	// THE PREMISE, and the guard on every profile-import failure.
	//
	// Each of these means the profile IS there and is wrong in a specific way,
	// from a pass that RAN, and each carries the only guidance the operator
	// has. If the fallback simply caught everything, the rows above would still
	// pass while the diagnosis was replaced by a recheck that cannot fix any of
	// them — so these must report the failure, must show the server's message,
	// and must not touch the recheck.
	//
	// ErrProfileDirUnreadable is the one this list was extended for: it wrapped
	// ErrProfileNotFound until a review caught it, which put the compose
	// uid-mismatch case — present-but-unreadable mounted profile, on the
	// designated Docker path — onto the ladder's bottom rung.
	for _, sentinel := range []error{
		cookies.ErrProfileDirUnreadable,
		cookies.ErrProfileNotADirectory,
		cookies.ErrCookieDBNotFound,
		cookies.ErrCookieDBLocked,
		cookies.ErrCookieDBUnreadable,
		cookies.ErrNoCookiesInProfile,
	} {
		t.Run("diagnosable: "+sentinel.Error(), func(t *testing.T) {
			app := NewApp()
			app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
				t.Error("a profile-import failure fell back to the recheck, which cannot fix it and " +
					"throws away the only sentence that says what went wrong")
				return cookies.RefreshUnknown, cookies.RefreshUnknown, "", ""
			}
			const detail = "check ownership and permissions on the mounted profile"
			if _, cmd := app.Update(cookieForceRefreshResultMsg{
				Err: fmt.Errorf("%s: %w", detail, sentinel),
			}); cmd != nil {
				cmd()
			}
			if strings.Contains(app.feedbackMsg, "running R C instead") {
				t.Errorf("R F fell back for a diagnosable profile failure: %q", app.feedbackMsg)
			}
			if !strings.Contains(app.feedbackMsg, "failed") {
				t.Errorf("a profile-import failure no longer reports a failure: %q", app.feedbackMsg)
			}
			if !strings.Contains(app.feedbackMsg, detail) {
				t.Errorf("the server's own guidance did not reach the operator: %q", app.feedbackMsg)
			}
		})
	}

	// The rung below the rung: with no recheck wired there is nothing to fall
	// back TO, and R F must report the error rather than a fallback that never
	// happens.
	t.Run("no recheck wired", func(t *testing.T) {
		app := NewApp()
		app.OnRecheckCookies = nil
		app.Update(cookieForceRefreshResultMsg{Err: cookies.ErrProfileNotFound})
		if strings.Contains(app.feedbackMsg, "running R C instead") {
			t.Errorf("R F promised to run R C with no recheck callback wired: %q", app.feedbackMsg)
		}
	})
}

// The two rung-3 sentences, written out here so the test states the contract
// rather than reading it back from the code it is checking. Both are the
// owner's wording, verbatim, ellipsis included.
const (
	rungThreeTUI = "No browser profile found, running R C instead..."
	rungThreeWeb = "No browser profile found, running a normal cookie refresh instead..."
)

// TestRungThreeSentencesDivergeByDesign pins both halves of one message that is
// deliberately not one string.
//
// R F and the dashboard's shift+click are the same gesture, and their bottom
// rung is the same event — no browser profile, falling back to the in-process
// refresh. The sentences differ because each names ITS OWN affordance: the TUI
// says "R C" because the TUI has chords, the dashboard says "a normal cookie
// refresh" because a plain click on that button is its R C. A dashboard user
// has no R C to press and a TUI user has no button to click, so a single shared
// string would be wrong on one surface whichever way it was written.
//
// That is exactly the shape that drifts silently — two literals, one meaning,
// no compiler between them — so both exact strings are asserted, and so is the
// divergence itself. Equality alone would be satisfied by two sentences that
// had quietly become identical; the swap check below would not.
func TestRungThreeSentencesDivergeByDesign(t *testing.T) {
	// The TUI sentence, EXECUTED — the string the operator actually reads,
	// through the arm that produces it, not the literal in the source.
	app := NewApp()
	app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
		return cookies.RefreshOK, cookies.RefreshOK, "", ""
	}
	app.Update(cookieForceRefreshResultMsg{
		Err: fmt.Errorf("refresh pass gave up: %w", cookies.ErrProfileNotFound),
	})
	if app.feedbackMsg != rungThreeTUI {
		t.Errorf("R F rung 3 renders %q, want %q — this is the owner's wording and is not to be "+
			"paraphrased or \"improved\"", app.feedbackMsg, rungThreeTUI)
	}

	// The Web sentence, read out of the shipped script. Located by its own
	// content rather than by position, and required to be UNIQUE: a second
	// literal opening the same way means there are two rung-3 messages and no
	// way to tell which one a user sees.
	raw, err := webassets.PublicFS.ReadFile("public/app.js")
	if err != nil {
		t.Fatalf("read the embedded app.js: %v", err)
	}
	web := soleJSStringStartingWith(t, strings.ReplaceAll(string(raw), "\r\n", "\n"),
		"No browser profile found")
	if web != rungThreeWeb {
		t.Errorf("the dashboard's rung 3 renders %q, want %q", web, rungThreeWeb)
	}

	// THE DIVERGENCE, asserted rather than left incidental.
	if rungThreeTUI == rungThreeWeb {
		t.Fatal("premise lost: the two sentences are identical, so the assertions above pin one " +
			"string twice and say nothing about either surface naming its own affordance")
	}
	const shared = "No browser profile found, running "
	for _, s := range []struct{ what, got string }{{"the TUI", app.feedbackMsg}, {"the dashboard", web}} {
		if !strings.HasPrefix(s.got, shared) {
			t.Errorf("%s no longer opens with %q, so the two messages have stopped being one event "+
				"worded twice: %q", s.what, shared, s.got)
		}
		if !strings.HasSuffix(s.got, " instead...") {
			t.Errorf("%s no longer closes with \" instead...\": %q", s.what, s.got)
		}
	}

	// Each names its own affordance and NOT the other's — the swap check. The
	// chord is matched as a token, with case and boundaries: a plain substring
	// search for "R C" is satisfied by "browse-R C-ookie", which a mutation run
	// on this file proved.
	chord := regexp.MustCompile(`(^|[^A-Za-z])R C([^A-Za-z]|$)`)
	if !chord.MatchString(app.feedbackMsg) {
		t.Errorf("the TUI's rung 3 does not name the R C chord, which is the one thing a TUI "+
			"operator can press here: %q", app.feedbackMsg)
	}
	if chord.MatchString(web) {
		t.Errorf("the dashboard's rung 3 names the R C chord. There are no chords on that surface — "+
			"a plain click on the refresh button is its R C: %q", web)
	}
	if !strings.Contains(web, "normal cookie refresh") {
		t.Errorf("the dashboard's rung 3 no longer names the affordance a dashboard user has: %q", web)
	}
	if strings.Contains(app.feedbackMsg, "normal cookie refresh") {
		t.Errorf("the TUI's rung 3 describes the mechanism instead of naming the chord: %q",
			app.feedbackMsg)
	}
}

// soleJSStringStartingWith returns the one JS string literal in src that begins
// with prefix, failing if there is not exactly one.
//
// Scans literals rather than slicing the file, so the assertion is about a
// parsed string rather than about text that happens to appear — a prose comment
// quoting the sentence cannot satisfy it, and a second copy of the message
// cannot hide behind the first.
func soleJSStringStartingWith(t *testing.T, src, prefix string) string {
	t.Helper()
	var found []string
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '/':
			// Skip comments, which is where a quoted copy of the sentence is
			// most likely to live — this file documents its own wording.
			if i+1 < len(src) && src[i+1] == '/' {
				if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
					i += nl
				} else {
					i = len(src)
				}
			} else if i+1 < len(src) && src[i+1] == '*' {
				if end := strings.Index(src[i+2:], "*/"); end >= 0 {
					i += 2 + end + 1
				} else {
					i = len(src)
				}
			}
		case '"', '\'', '`':
			quote := src[i]
			var lit strings.Builder
			j := i + 1
			for ; j < len(src); j++ {
				if src[j] == '\\' && j+1 < len(src) {
					lit.WriteByte(src[j+1])
					j++
					continue
				}
				if src[j] == quote {
					break
				}
				lit.WriteByte(src[j])
			}
			if strings.HasPrefix(lit.String(), prefix) {
				found = append(found, lit.String())
			}
			i = j
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("app.js contains no string literal starting %q — the dashboard's rung 3 message is "+
			"gone, so shift+click dead-ends where the TUI falls back", prefix)
	default:
		t.Fatalf("app.js contains %d string literals starting %q (%q) — there is no telling which one "+
			"a user sees, and only one of them can be the pinned wording", len(found), prefix, found)
	}
	return ""
}
