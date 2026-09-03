package tui

import (
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestRefreshPostflightMechanismAgreesAcrossSurfaces is the cross-UI pin for
// the post-flight subject, built exactly like
// TestRefreshPreflightSentenceAgreesAcrossSurfaces next door: literal sentences
// first so the test is self-checking, then the SHIPPED utils.js run through
// goja and compared to the Go renderer by exact equality — never Contains,
// because "Browser cookie refresh" is a substring of nothing here but a drift
// that kept one string a prefix of the other must still fail.
//
// The grid is every mechanism against every mode, including the values neither
// side should ever see. That is what pins the PRECEDENCE: a renderer that read
// the mode first would agree with its twin on twelve of the sixteen rows.
//
// The mechanism values arrive as the Go CONSTANTS, so a rename that changes
// their VALUE is caught here too — the JS compares literals, so a changed
// constant falls through its two arms to the mode fallback and the two answers
// part company on the rows where the mode disagrees.
func TestRefreshPostflightMechanismAgreesAcrossSurfaces(t *testing.T) {
	const (
		browserLabel = "Browser cookie refresh"
		importLabel  = "Browser-profile cookie import"
	)
	for _, tc := range []struct{ mechanism, mode, want string }{
		{cookies.RefreshMechanismBrowser, "auto", browserLabel},
		{cookies.RefreshMechanismBrowser, "profile", browserLabel},
		{cookies.RefreshMechanismProfileImport, "auto", importLabel},
		{cookies.RefreshMechanismProfileImport, "profile", importLabel},
		{"", "auto", browserLabel},
		{"", "profile", importLabel},
		{"", "", browserLabel},
	} {
		if got := cookieRefreshMechanismLabel(tc.mechanism, tc.mode); got != tc.want {
			t.Errorf("cookieRefreshMechanismLabel(%q, %q) = %q, want %q",
				tc.mechanism, tc.mode, got, tc.want)
		}
	}
	// The RESULT outranks the mode, and these two rows are the only place that
	// is visible: a renderer that consulted the mode first answers the import
	// label for the first and the browser label for the second.
	if got := cookieRefreshMechanismLabel(cookies.RefreshMechanismBrowser, "profile"); got != browserLabel {
		t.Errorf("a browser pass in profile mode is labelled %q — the mode was consulted ahead of "+
			"what the pass actually did", got)
	}
	if got := cookieRefreshMechanismLabel(cookies.RefreshMechanismProfileImport, "auto"); got != importLabel {
		t.Errorf("an import in auto mode is labelled %q — this is the host-decided case that made "+
			"every post-flight sentence wrong before the mode setting existed", got)
	}

	vm := utilsModuleVM(t)
	fn, ok := goja.AssertFunction(vm.Get("cookieRefreshMechanismLabel"))
	if !ok {
		t.Fatal("utils.js does not export cookieRefreshMechanismLabel — the dashboard's post-flight " +
			"toasts have no shared subject and the two surfaces cannot be held together")
	}
	for _, mechanism := range []string{
		cookies.RefreshMechanismBrowser, cookies.RefreshMechanismProfileImport, "", "headless",
	} {
		for _, mode := range []string{"auto", "profile", "", "browser"} {
			v, err := fn(goja.Undefined(), vm.ToValue(mechanism), vm.ToValue(mode))
			if err != nil {
				t.Fatalf("mechanism %q mode %q: %v", mechanism, mode, err)
			}
			if web, tui := v.String(), cookieRefreshMechanismLabel(mechanism, mode); web != tui {
				t.Errorf("mechanism %q mode %q: the dashboard says %q and the TUI says %q — one "+
					"pass, two names for what ran", mechanism, mode, web, tui)
			}
		}
	}
	// An ABSENT argument, which is what an older binary's payload produces on
	// the dashboard (`data.mechanism` is undefined, not ""). It must land on
	// the mode fallback rather than on some third answer.
	undef, err := fn(goja.Undefined(), goja.Undefined(), vm.ToValue("profile"))
	if err != nil {
		t.Fatalf("undefined mechanism: %v", err)
	}
	if got := undef.String(); got != importLabel {
		t.Errorf("an older binary's payload (no mechanism key) is labelled %q in profile mode, want "+
			"%q — the fallback is the whole reason the key can be additive", got, importLabel)
	}
}

// TestPostFlightSentencesNameTheMechanismThatRan is the TUI half, through the
// real Update loop, because the sentence is assembled at the arm and not in the
// label function.
//
// Three rows, three different ways the subject is reached: the result says
// "import", the result says "browser", and the result says nothing so the mode
// answers. The browser rows are asserted BYTE-IDENTICALLY to what shipped
// before, because feedbackColor classifies by substring and
// TestFeedbackColorWarningMessages pins two of these strings verbatim — this
// change re-subjects the import case and must leave the browser case alone.
func TestPostFlightSentencesNameTheMechanismThatRan(t *testing.T) {
	appIn := func(t *testing.T, mode string) *App {
		t.Helper()
		a := NewApp()
		cfg := config.Defaults()
		cfg.Cookies.Acquisition = mode
		a.configStore = config.NewStore(cfg, "")
		return a
	}

	t.Run("an import that succeeded", func(t *testing.T) {
		a := appIn(t, "profile")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismProfileImport,
		}})
		if want := "Browser-profile cookie import successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the pass launched no browser", a.feedback.msg, want)
		}
	})

	t.Run("a browser pass that succeeded keeps its shipped wording", func(t *testing.T) {
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismBrowser,
		}})
		if want := "Browser cookie refresh successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the browser wording is unchanged by this task", a.feedback.msg, want)
		}
	})

	t.Run("a decline in profile mode falls back to the mode", func(t *testing.T) {
		// refreshDeclined() is the zero RefreshResult: Ran false, Mechanism "".
		// The pass never chose, so the sentence's subject comes from the same
		// setting the pre-flight line used a moment earlier — the two lines
		// have to agree or the operator watched one gesture describe itself
		// twice, differently.
		a := appIn(t, "profile")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{}})
		want := "Browser-profile cookie import declined to run (" + cookies.RefreshDeclinedCauses +
			") — nothing was learned about these cookies"
		if a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, want)
		}
	})

	t.Run("a host-decided import in auto mode", func(t *testing.T) {
		// THE ROW THAT MAKES THE PRECEDENCE OBSERVABLE, and the case that made
		// every post-flight sentence wrong years before cookies.acquisition
		// existed: no browser on the host, so the pass imports while the mode
		// still says "auto". A renderer that consulted the mode would say
		// "Browser cookie refresh successful" here, and did.
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismProfileImport,
		}})
		if want := "Browser-profile cookie import successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the mode was consulted ahead of what the pass did",
				a.feedback.msg, want)
		}
	})

	t.Run("a browser pass that verified but could not confirm keeps its arm", func(t *testing.T) {
		// The one arm that names the browser and is NOT re-subjected: an import
		// forces Renewed true (renewed := importedFromProfile || browserActed),
		// so it is unreachable for one — pinned in internal/cookies by
		// TestRefreshResultCarriesTheMechanismThatRan's Renewed assertion.
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: false, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismBrowser,
		}})
		if want := "Cookies still work, but this pass could not confirm the browser refreshed them"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, want)
		}
	})
}
