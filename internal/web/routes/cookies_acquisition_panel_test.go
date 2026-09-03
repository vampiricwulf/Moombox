package routes

import (
	"regexp"
	"strings"
	"testing"
)

// TestAcquisitionSelectIsInTheShippedPanel asserts the control exists in the
// asset the binary actually serves, with both values and no others.
//
// Bracketed to the <sl-select id="cfg-cookies-acquisition">...</sl-select>
// block specifically, not the whole file: other selects on this page also
// carry value="auto"-shaped options, so a file-wide Contains cannot tell this
// control's own options from a sibling's. The option values are parsed with a
// quote-agnostic regex (["']) rather than a literal `value="browser"` Contains
// — a single-quoted `value='browser'` is exactly as real an HTML attribute as
// a double-quoted one, and the ruling ("no browser option, in any form") does
// not care which quote character wrote it.
func TestAcquisitionSelectIsInTheShippedPanel(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	if !strings.Contains(html, `id="cfg-cookies-acquisition"`) {
		t.Fatal("the cookie acquisition select is not in index.html — the dashboard cannot set the mode")
	}

	selectRe := regexp.MustCompile(`(?s)<sl-select[^>]*\bid="cfg-cookies-acquisition"[^>]*>(.*?)</sl-select>`)
	m := selectRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("could not bracket the <sl-select id=\"cfg-cookies-acquisition\">...</sl-select> block " +
			"— the markup was restructured and this assertion is reading nothing")
	}
	block := m[1]

	optionRe := regexp.MustCompile(`<sl-option[^>]*\bvalue=["']([^"']+)["']`)
	got := map[string]bool{}
	for _, mm := range optionRe.FindAllStringSubmatch(block, -1) {
		got[mm[1]] = true
	}
	want := map[string]bool{"auto": true, "profile": true}
	for v := range want {
		if !got[v] {
			t.Errorf("cfg-cookies-acquisition has no option value=%q", v)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("cfg-cookies-acquisition offers an unexpected option value=%q — only auto and "+
				"profile are allowed (e.g. a \"browser\" option was ruled out; the API rejects it and "+
				"the select would save a mode the server refuses)", v)
		}
	}
}

// settingsPanelAcquisitionProbe installs two globals for driving settings.js's
// SettingsController methods through settingsPanelVM against a minimal stub
// DOM: __runPopulateConfigForm(config) and __startSaveConfigProbe(value).
//
// This replaces two unanchored substring checks a same-prefix decoy defeated:
// renaming the payload key to `cookieAcquisitionMode: acquisition,` still
// passed a bare Contains(body, "acquisition,") (the substring matches the
// VALUE reference's tail, not the key), and reading
// `config.cookies?.acquisitionMode` still passed Contains(body,
// "cookies?.acquisition") (a prefix match). RUNNING the shipped methods and
// reading back what they actually did closes both gaps: a wrong wire key or a
// wrong config path changes the observed VALUE, which a substring anchored on
// vocabulary alone cannot depend on.
//
// Only the DOM surface and instance methods populateConfigForm/saveConfig
// actually touch are stubbed — populateBrowserSelector-style narrowing, same
// reason settingsPanelVMWithUtils's sibling probes give: driving the rest of
// either method's forty-odd other fields has nothing to do with this control.
const settingsPanelAcquisitionProbe = `
function __mkAcquisitionEl(id) {
  return {
    id, value: "", checked: false, loading: false, disabled: false,
    style: { display: "", setProperty(prop, val) { this[prop] = val; } },
    dataset: {},
    _attrs: {},
    getAttribute(name) { return this._attrs[name] === undefined ? null : this._attrs[name]; },
    setAttribute(name, val) { this._attrs[name] = val; },
    querySelectorAll() { return []; },
    addEventListener() {},
    querySelector() { return null; },
    remove() {},
  };
}

globalThis.__runPopulateConfigForm = function (config) {
  const els = {};
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = __mkAcquisitionEl(id); return els[id]; },
    querySelector() { return null; },
  };
  const inst = Object.create(SettingsController.prototype);
  inst.app = {
    config,
    setInputValue(id, value) {
      const el = document.getElementById(id);
      el.value = value ?? "";
    },
    getInputValue(id) { return document.getElementById(id).value; },
  };
  // Narrowed exactly like settingsPanelVMWithUtils's importPanelProbe narrows
  // populateBrowserSelector: these are other fields' business.
  inst._wireTemplatePreview = function () {};
  inst.updateAutoCookieUI = function () {};
  inst._addRestartBadges = function () {};
  inst.loadSecurityStatus = function () {};
  inst.populateConfigForm();
  globalThis.__collectAcquisitionSelect = function () {
    return els["cfg-cookies-acquisition"] ? els["cfg-cookies-acquisition"].value : undefined;
  };
};

globalThis.__startSaveConfigProbe = function (acquisitionValue) {
  const els = {};
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = __mkAcquisitionEl(id); return els[id]; },
  };
  document.getElementById("cfg-cookies-acquisition").value = acquisitionValue;

  let sentBody = null;
  globalThis.fetch = function (url, init) {
    if (url === "/api/config") sentBody = JSON.parse(init.body);
    return { ok: true, json() { return {}; } };
  };

  const inst = Object.create(SettingsController.prototype);
  inst.app = {
    config: {},
    getInputValue(id) {
      const v = document.getElementById(id).value;
      return typeof v === "string" ? v.trim() : (v !== undefined && v !== null ? String(v) : "");
    },
    getInputNumber(id) {
      const v = document.getElementById(id).value;
      if (typeof v === "number") return isNaN(v) ? undefined : v;
      const s = typeof v === "string" ? v.trim() : "";
      return s ? Number(s) : undefined;
    },
    showToast() {},
    loadStatus() {},
  };
  inst._updateUnsavedIndicator = function () {};
  inst._checkRestartRequired = function () {};

  // sentBody is captured synchronously inside the fetch stub above, before
  // saveConfig's own await suspends — so it is already set by the time this
  // call returns, regardless of anything the async continuation does next.
  inst.saveConfig();

  globalThis.__collectSaveConfigProbe = function () {
    return sentBody;
  };
};
`

// TestSaveConfigSendsAcquisition RUNS the shipped saveConfig against a select
// holding each real value and reads the captured PUT /api/config body's
// cookies.acquisition key — not a text match on the source.
//
// A wrong wire key (renaming the payload property while keeping the value
// variable named `acquisition`) leaves cookies.acquisition absent from the
// captured body, which this test can see and a substring check could not.
func TestSaveConfigSendsAcquisition(t *testing.T) {
	vm := settingsPanelVM(t)
	if _, err := vm.RunString(settingsPanelAcquisitionProbe); err != nil {
		t.Fatalf("install the settings-panel acquisition probe: %v", err)
	}
	for _, mode := range []string{"auto", "profile"} {
		t.Run(mode, func(t *testing.T) {
			if err := vm.Set("__acquisitionValue", mode); err != nil {
				t.Fatalf("hand the probe the select's value: %v", err)
			}
			if _, err := vm.RunString("__startSaveConfigProbe(__acquisitionValue);"); err != nil {
				t.Fatalf("saveConfig threw — the browser would fail the same way: %v", err)
			}
			out, err := vm.RunString("__collectSaveConfigProbe();")
			if err != nil {
				t.Fatalf("collect the captured PUT /api/config body: %v", err)
			}
			sent, ok := out.Export().(map[string]any)
			if !ok {
				t.Fatal("saveConfig never called fetch(\"/api/config\", ...) — no body was captured")
			}
			cookies, ok := sent["cookies"].(map[string]any)
			if !ok {
				t.Fatal("the PUT /api/config body carries no cookies object")
			}
			if got := cookies["acquisition"]; got != mode {
				t.Errorf("PUT /api/config body's cookies.acquisition = %v, want %q — the setting "+
					"would silently never save under the real wire key", got, mode)
			}
		})
	}
}

// TestPopulateConfigFormReadsAcquisition is the other half. RUNS the shipped
// populateConfigForm against a fake config and reads back what it set the
// select's value to.
//
// Reading the wrong config path (e.g. `config.cookies?.acquisitionMode`)
// leaves the select at its own default for a config that DOES carry
// `acquisition`, which this test can see and a substring check on the source
// text could not (a same-prefix identifier still contains the substring
// being matched).
func TestPopulateConfigFormReadsAcquisition(t *testing.T) {
	vm := settingsPanelVM(t)
	if _, err := vm.RunString(settingsPanelAcquisitionProbe); err != nil {
		t.Fatalf("install the settings-panel acquisition probe: %v", err)
	}
	for _, tc := range []struct {
		name       string
		configJSON string
		want       string
	}{
		{"profile", `{"cookies":{"acquisition":"profile"}}`, "profile"},
		{"absent config key defaults to auto", `{"cookies":{}}`, "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := vm.Set("__configJSON", tc.configJSON); err != nil {
				t.Fatalf("hand the probe its config: %v", err)
			}
			if _, err := vm.RunString("__runPopulateConfigForm(JSON.parse(__configJSON));"); err != nil {
				t.Fatalf("populateConfigForm threw — the browser would fail the same way: %v", err)
			}
			out, err := vm.RunString("__collectAcquisitionSelect();")
			if err != nil {
				t.Fatalf("collect the select's value: %v", err)
			}
			got, _ := out.Export().(string)
			if got != tc.want {
				t.Errorf("populateConfigForm set cfg-cookies-acquisition to %q, want %q — a save "+
					"right after load would silently overwrite the stored mode", got, tc.want)
			}
		})
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
