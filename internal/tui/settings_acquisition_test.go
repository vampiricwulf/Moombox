package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/config"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// TestAcquisitionRowRoundTrips is the dual-UI-parity assertion: the TUI must be
// able to read AND write the mode, or one surface silently owns a setting the
// other can only display.
//
// Both directions, because they fail differently. A missing loadValues entry
// renders an empty row and then writes "" on the next save; a missing
// applyValues entry accepts the edit, reports "Saved", and discards it.
func TestAcquisitionRowRoundTrips(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cookies.Acquisition = "profile"
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.Open(cfg)

	if got := m.values["acquisition"]; got != "profile" {
		t.Errorf("loadValues put %q in the acquisition row, want %q — the row renders the stored "+
			"mode or it overwrites it on the next save", got, "profile")
	}

	m.values["acquisition"] = "auto"
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues rejected a legal mode: %s", m.errorMsg)
	}
	if cfg.Cookies.Acquisition != "auto" {
		t.Errorf("applyValues wrote %q, want %q", cfg.Cookies.Acquisition, "auto")
	}
}

// TestAcquisitionRowIsACycleWithTwoOptions pins the control type. A free-text
// row here would let an operator type a mode the validator then replaces with
// the default behind their back — the setting would appear to accept anything
// and quietly do one thing.
func TestAcquisitionRowIsACycleWithTwoOptions(t *testing.T) {
	var found *fieldDef
	for i := range sections {
		if sections[i].name != "Cookies" {
			continue
		}
		for j := range sections[i].fields {
			if sections[i].fields[j].key == "acquisition" {
				found = &sections[i].fields[j]
			}
		}
	}
	if found == nil {
		t.Fatal("the Cookies section has no acquisition row — the TUI cannot set the mode")
	}
	if found.ftype != fieldCycle {
		t.Errorf("acquisition is %v, want fieldCycle — the enum row type (see network_access, "+
			"log_level); a text row would accept anything and silently normalise", found.ftype)
	}
	want := []string{"auto", "profile"}
	if len(found.options) != len(want) {
		t.Fatalf("acquisition has %d options, want %d: %v", len(found.options), len(want), found.options)
	}
	for i, opt := range want {
		if found.options[i] != opt {
			t.Errorf("option %d = %q, want %q", i, found.options[i], opt)
		}
	}
}

// TestAcquisitionIsNotRestartRequired guards the hot-reload claim. Both UIs
// label a restart-required setting and the dashboard shows a banner; labelling
// this one would tell the operator to restart for a change the very next R F
// already sees, and NOT labelling a setting that needs one is the worse half of
// the same mistake. It is read live through AutoCookieService.AcquisitionMode,
// so it belongs in neither list.
func TestAcquisitionIsNotRestartRequired(t *testing.T) {
	if restartRequiredKeys["acquisition"] {
		t.Error("acquisition is marked restart-required, but AcquisitionMode is consulted per " +
			"refresh pass — the label would be false")
	}
}

// TestForceRefreshFeedbackNamesTheMechanism is the TUI half of the ladder's
// pre-flight sentence. Under "profile" nothing is launched, so the browser
// sentence names a mechanism the operator switched off.
func TestForceRefreshFeedbackNamesTheMechanism(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"auto", "Running browser cookie refresh..."},
		{"", "Running browser cookie refresh..."},
		{"profile", "Importing cookies from the browser profile..."},
	} {
		if got := cookieRefreshFeedback(tc.mode); got != tc.want {
			t.Errorf("cookieRefreshFeedback(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// utilsModuleVM loads the SHIPPED utils.js into a goja runtime, the way
// settingsVM loads settings.js: strip the `export` keyword and nothing else.
func utilsModuleVM(t *testing.T) *goja.Runtime {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile("public/modules/utils.js")
	if err != nil {
		t.Fatalf("read the embedded utils.js: %v", err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	src = regexp.MustCompile(`(?m)^export `).ReplaceAllString(src, "")
	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("utils.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// TestRefreshPreflightSentenceAgreesAcrossSurfaces is the cross-UI pin, built
// the way TestRungThreeSentencesDivergeByDesign pins the rung-3 pair, except
// this pair is meant to AGREE: neither sentence names a per-surface affordance,
// so the two renderers should say the identical thing for every mode.
//
// It pins two things at once. First, against the literal sentences the brief
// specifies (redundant with TestForceRefreshFeedbackNamesTheMechanism, but this
// test stays self-checking even before the second half can run). Second,
// against the dashboard's own cookieRefreshPreflightToast in
// web/public/modules/utils.js, RUN through goja and compared to the Go
// renderer by exact equality — never Contains, because a wording drift that
// keeps one sentence a substring of the other must still fail this test.
//
// utils.js gains cookieRefreshPreflightToast in the parallel Task 5, which is
// not on this branch. Until it lands, the export lookup below reports !ok and
// this test SKIPS the cross-surface half rather than failing the gate on a
// function this branch was never asked to write; the literal-sentence half
// above still runs. Once the branches merge, the skip clause stops firing and
// the exact-equality loop is what actually pins the two surfaces together.
func TestRefreshPreflightSentenceAgreesAcrossSurfaces(t *testing.T) {
	literalWant := []struct{ mode, want string }{
		{"auto", "Running browser cookie refresh..."},
		{"profile", "Importing cookies from the browser profile..."},
		{"", "Running browser cookie refresh..."},
	}
	for _, tc := range literalWant {
		if got := cookieRefreshFeedback(tc.mode); got != tc.want {
			t.Errorf("mode %q: cookieRefreshFeedback = %q, want %q", tc.mode, got, tc.want)
		}
	}

	vm := utilsModuleVM(t)
	fn, ok := goja.AssertFunction(vm.Get("cookieRefreshPreflightToast"))
	if !ok {
		t.Skip("utils.js does not yet export cookieRefreshPreflightToast — that lands in the " +
			"parallel Task 5, not on this branch; the cross-surface pin activates once the two " +
			"branches merge")
	}
	for _, mode := range []string{"auto", "profile", ""} {
		v, err := fn(goja.Undefined(), vm.ToValue(mode))
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if web, tui := v.String(), cookieRefreshFeedback(mode); web != tui {
			t.Errorf("mode %q: the dashboard says %q and the TUI says %q — one gesture, two "+
				"sentences", mode, web, tui)
		}
	}
}

// TestCookieAcquisitionModeReadsTheLiveStore pins WHERE the chord reads from.
// A snapshot taken at App construction would make the sentence stale for the
// whole process, which is the same defect the service's own callback exists to
// avoid — and a nil store must not panic on a chord an operator can press.
func TestCookieAcquisitionModeReadsTheLiveStore(t *testing.T) {
	a := &App{}
	if got := a.cookieAcquisitionMode(); got != "auto" {
		t.Errorf("with no config store the mode is %q, want \"auto\"", got)
	}

	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	a.configStore = store
	if got := a.cookieAcquisitionMode(); got != "auto" {
		t.Errorf("mode = %q, want \"auto\"", got)
	}
	cfg.Cookies.Acquisition = "profile"
	if got := a.cookieAcquisitionMode(); got != "profile" {
		t.Errorf("after a live edit the mode is %q, want \"profile\" — the read is cached", got)
	}
}
