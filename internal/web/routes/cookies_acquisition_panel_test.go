package routes

import (
	"regexp"
	"strings"
	"testing"
)

// TestAcquisitionSelectIsInTheShippedPanel asserts the control exists in the
// asset the binary actually serves, with both values. A settings field with
// no control is the checklist's dual-UI-parity mistake: the TUI can set it and
// the dashboard silently cannot.
func TestAcquisitionSelectIsInTheShippedPanel(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	if !strings.Contains(html, `id="cfg-cookies-acquisition"`) {
		t.Fatal("the cookie acquisition select is not in index.html — the dashboard cannot set the mode")
	}
	for _, v := range []string{`value="auto"`, `value="profile"`} {
		if !strings.Contains(html, v) {
			t.Errorf("index.html has no option %s for cfg-cookies-acquisition", v)
		}
	}
	if strings.Contains(html, `value="browser"`) {
		t.Error("index.html offers a \"browser\" acquisition option — that value was ruled out; " +
			"the API rejects it and the select would save a mode the server refuses")
	}
}

// TestSaveConfigSendsAcquisition brackets the assertion to saveConfig, because
// a file-wide Contains passes on a literal that appears in a sibling helper.
// The payload key is what PUT /api/config validates and applies; a control that
// renders and never reaches the body is the "validates but never persists"
// failure, and it is invisible from the UI.
func TestSaveConfigSendsAcquisition(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/modules/settings.js"), "saveConfig")
	if !strings.Contains(body, "cfg-cookies-acquisition") {
		t.Error("saveConfig never reads the acquisition select")
	}
	if !strings.Contains(body, "acquisition,") && !strings.Contains(body, "acquisition:") {
		t.Error("saveConfig builds no cookies.acquisition key — the setting would silently never save")
	}
}

// TestPopulateConfigFormReadsAcquisition is the other half. Without it the
// control renders empty on every load and a save writes whatever the browser
// defaulted the select to — quietly resetting the operator's mode.
func TestPopulateConfigFormReadsAcquisition(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/modules/settings.js"), "populateConfigForm")
	if !strings.Contains(body, "cfg-cookies-acquisition") {
		t.Error("populateConfigForm never fills the acquisition select — it renders empty and a " +
			"save then overwrites the stored mode with the select's own default")
	}
	if !strings.Contains(body, "cookies?.acquisition") {
		t.Error("populateConfigForm does not read config.cookies.acquisition")
	}
}

// TestRefreshToastNamesTheMechanism RUNS the shipped helper and reads the
// sentence back: what the operator sees is what evaluates.
//
// The rung-1 sentence is a claim only one of the two modes can support. In
// "profile" mode nothing launches, so "Running browser cookie refresh..."
// describes a mechanism the operator switched off — the same class of unearned
// cause as telling a gated operator to install a browser they already have.
// The undefined row is an OLDER BINARY behind a newer dashboard: no
// acquisition key at all must read as auto.
func TestRefreshToastNamesTheMechanism(t *testing.T) {
	vm := utilsVM(t)
	for _, tc := range []struct {
		name string
		mode any
		want string
	}{
		{"auto", "auto", "Running browser cookie refresh..."},
		{"empty", "", "Running browser cookie refresh..."},
		{"absent (older binary)", nil, "Running browser cookie refresh..."},
		{"profile", "profile", "Importing cookies from the browser profile..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := jsCall(t, vm, "cookieRefreshPreflightToast", tc.mode).(string)
			if got != tc.want {
				t.Errorf("cookieRefreshPreflightToast(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// TestAutoCookieRefreshUsesThePreflightHelper pins the call SITE. The helper
// above can be perfect and unused; the bracket says autoCookieRefresh calls it,
// with the live config's mode, and no longer carries its own sentence.
func TestAutoCookieRefreshUsesThePreflightHelper(t *testing.T) {
	src := readEmbeddedModule(t, "public/app.js")
	code := jsCode(jsBlock(t, src, "async autoCookieRefresh() {"))
	if !strings.Contains(code, "cookieRefreshPreflightToast(this.config?.cookies?.acquisition)") {
		t.Error("autoCookieRefresh does not call cookieRefreshPreflightToast with the live " +
			"config's acquisition mode — the toast cannot name the mechanism")
	}
	if strings.Contains(code, "Running browser cookie refresh...") {
		t.Error("autoCookieRefresh still carries the browser sentence inline — two copies of one " +
			"sentence is how the surfaces drift")
	}
	// The import, matched as a parsed statement rather than a file-wide
	// Contains: the name must sit inside the braces of the utils.js import.
	utilsImport := regexp.MustCompile(`import \{[^}]*\bcookieRefreshPreflightToast\b[^}]*\} from "\./modules/utils\.js"`)
	if !utilsImport.MatchString(src) {
		t.Error("app.js does not import cookieRefreshPreflightToast from ./modules/utils.js — the " +
			"call above would throw ReferenceError in the browser")
	}
}
