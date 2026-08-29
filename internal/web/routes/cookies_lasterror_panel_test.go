package routes

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// AutoCookieStatus.LastError reaching the always-on cookies panel, asserted by
// RUNNING loadAutoCookieStatus against a stub DOM.
//
// The field is the last thing a cookie pass concluded that the operator has to
// act on, and until now the Web read it in exactly one place: the setup
// dialog's abort report. So a browser refresh that recorded "the browser
// profile contained no cookies" or "refusing to overwrite cookies.txt" left the
// settings page looking clean, and the only surface that could have said so was
// one the operator had to be inside a wizard to see. Arc 8 Task 12a wrote the
// field's write policy down — one SET funnel, three earned clears, and
// cleanup() forbidden from clearing — which is what makes a non-empty value
// worth showing permanently rather than only where it was produced.

// settingsPanelVM loads the shipped settings.js into a goja runtime.
//
// Same two transforms settingsVM in internal/tui applies — strip the utils.js
// import and the `export` keyword, neither of which goja parses — and nothing
// else, so what runs is the source the binary serves. Kept here rather than
// shared with that copy because the two packages cannot see each other's test
// helpers; the transform is the module's, not this assertion's.
func settingsPanelVM(t *testing.T) *goja.Runtime {
	t.Helper()
	src := readEmbeddedModule(t, "public/modules/settings.js")
	src = regexp.MustCompile(`(?s)import \{[^}]*\} from "\./utils\.js";`).ReplaceAllString(src, "")
	src = strings.ReplaceAll("\n"+src, "\nexport ", "\n")

	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("settings.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// autoStatusPanelProbe runs SettingsController.loadAutoCookieStatus against one
// /api/cookies/auto-status body and reports what landed on every element.
//
// populateBrowserSelector is stubbed out: it is the browser dropdown's own
// business, it is already covered elsewhere, and running it here would make
// this assertion depend on forty lines of Shoelace DOM that have nothing to do
// with the error line.
const autoStatusPanelProbe = `
globalThis.__startAutoStatusPanel = function (body) {
  const els = {};
  const mk = (id) => ({
    id,
    textContent: "",
    style: {},
    open: false,
    show() { this.open = true; },
    addEventListener() {},
    appendChild() {},
  });
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = mk(id); return els[id]; },
  };
  globalThis.fetch = function () {
    return { ok: true, json() { return body; } };
  };
  // The method's own catch calls console.error; without this the catch itself
  // throws and the whole render disappears into a rejected promise.
  let failure = null;
  globalThis.console = { error(...a) { failure = String(a); } };
  const inst = Object.create(SettingsController.prototype);
  inst.populateBrowserSelector = function () {};
  inst.loadAutoCookieStatus();
  // COLLECTED IN A SECOND CALL, not here. loadAutoCookieStatus is async: its
  // body past the first await is a microtask, and goja drains the job queue
  // when the enclosing RunString returns — so anything read on this line is
  // read before the render has happened.
  globalThis.__collectAutoStatusPanel = function () {
    const out = { __failure: { text: failure === null ? "" : failure, display: "", color: "" } };
    for (const id of Object.keys(els)) {
      out[id] = { text: els[id].textContent, display: els[id].style.display || "", color: els[id].style.color || "" };
    }
    return out;
  };
};
`

type panelElement struct {
	text    string
	display string
	color   string
}

func renderAutoStatusPanel(t *testing.T, body map[string]any) map[string]panelElement {
	t.Helper()
	vm := settingsPanelVM(t)
	if _, err := vm.RunString(autoStatusPanelProbe); err != nil {
		t.Fatalf("install the auto-status panel probe: %v", err)
	}
	if err := vm.Set("__body", body); err != nil {
		t.Fatalf("hand the probe its response body: %v", err)
	}
	// Two RunStrings on purpose: the first starts the async render and, on
	// return, drains the promise job queue that carries the rest of it.
	if _, err := vm.RunString("__startAutoStatusPanel(__body);"); err != nil {
		t.Fatalf("loadAutoCookieStatus threw — the browser would fail the same way: %v", err)
	}
	out, err := vm.RunString("__collectAutoStatusPanel();")
	if err != nil {
		t.Fatalf("collect the rendered panel: %v", err)
	}
	raw, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T, want the per-element map", out.Export())
	}
	got := map[string]panelElement{}
	for id, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("element %q came back as %T", id, v)
		}
		el := panelElement{}
		el.text, _ = m["text"].(string)
		el.display, _ = m["display"].(string)
		el.color, _ = m["color"].(string)
		got[id] = el
	}
	// loadAutoCookieStatus swallows everything into a console.error, so a
	// render that fell over halfway would otherwise present here as "the panel
	// simply did not write that element".
	if failure := got["__failure"].text; failure != "" {
		t.Fatalf("loadAutoCookieStatus reported %q — the panel render did not complete", failure)
	}
	delete(got, "__failure")
	return got
}

// TestCookiePanelShowsLastErrorOnlyWhenThereIsOne is item (iii)'s Web half.
//
// Two directions, and they do not cost the same. Failing to show a recorded
// error leaves the operator with a panel that looks healthy while every archive
// fails; showing an empty one puts a bare warning-coloured line under the
// browser info, which reads as a state of its own. So the element is hidden,
// not merely blanked — a zero-height empty div is not the same as `display:
// none` once the panel gains a border or a margin.
func TestCookiePanelShowsLastErrorOnlyWhenThereIsOne(t *testing.T) {
	const recorded = "browser profile contained no cookies — sign in and run setup again"

	t.Run("a recorded error is shown", func(t *testing.T) {
		got := renderAutoStatusPanel(t, map[string]any{
			"setupInProgress":   false,
			"availableBrowsers": []any{},
			"lastError":         recorded,
		})
		el, seen := got["auto-cookie-last-error"]
		if !seen {
			t.Fatal("loadAutoCookieStatus never touched auto-cookie-last-error — the panel has no " +
				"reader for the field at all, which is the state this task exists to end")
		}
		if !strings.Contains(el.text, recorded) {
			t.Errorf("the panel does not say what was recorded: %q", el.text)
		}
		if el.display == "none" {
			t.Errorf("the line is hidden while carrying %q — the operator would never see it", el.text)
		}
		if el.color == "" {
			t.Error("the line is rendered in the panel's default colour. It is an advisory the " +
				"operator has to act on, and it reads as ordinary help text unstyled")
		}
	})

	t.Run("no recorded error, no line", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"setupInProgress": false, "availableBrowsers": []any{}, "lastError": nil},
			{"setupInProgress": false, "availableBrowsers": []any{}, "lastError": ""},
			{"setupInProgress": false, "availableBrowsers": []any{}},
		} {
			got := renderAutoStatusPanel(t, body)
			el := got["auto-cookie-last-error"]
			if el.text != "" {
				t.Errorf("lastError = %v rendered %q — an absent or empty error is not an error, "+
					"and a bare line under the browser info reads as a state of its own",
					body["lastError"], el.text)
			}
			if el.display != "none" {
				t.Errorf("lastError = %v left the line visible (display = %q)",
					body["lastError"], el.display)
			}
		}
	})

	t.Run("the browser info line is untouched", func(t *testing.T) {
		// The premise for the two above: the error line is an ADDITION, not a
		// rewrite of the line that was already there. Losing "Last refresh" to
		// make room for the error would trade one missing fact for another.
		got := renderAutoStatusPanel(t, map[string]any{
			"setupInProgress":   false,
			"availableBrowsers": []any{},
			"lastError":         recorded,
		})
		info, seen := got["auto-cookie-browser-info"]
		if !seen {
			t.Fatal("auto-cookie-browser-info is no longer written at all")
		}
		if !strings.Contains(info.text, "No supported browser detected") {
			t.Errorf("auto-cookie-browser-info = %q — the pre-existing line stopped reporting the "+
				"empty browser list", info.text)
		}
		if strings.Contains(info.text, recorded) {
			t.Errorf("the error was folded into the browser-info line (%q). It is a secondary line "+
				"UNDER that one, so it can be hidden independently", info.text)
		}
	})
}

// TestCookiePanelErrorElementExists is the premise the probe cannot supply.
//
// document.getElementById manufactures an element for any id asked of it, so a
// typo renders perfectly against nothing. In a browser it returns null, the
// method's own `if (lastErrorEl)` guard swallows it, and the line silently
// never appears — no error, no console warning.
func TestCookiePanelErrorElementExists(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	if !strings.Contains(html, `id="auto-cookie-last-error"`) {
		t.Error("index.html has no auto-cookie-last-error element. getElementById returns null, " +
			"the guard in loadAutoCookieStatus swallows it, and the recorded error is never shown")
	}
}

// TestCookiePanelMarkupIsEmbedded guards the go:embed step, which is the one
// failure this whole file would otherwise report as a JS bug: web/public is
// compiled into the binary, so an edited index.html that was never rebuilt is
// invisible here and to the user alike.
func TestCookiePanelMarkupIsEmbedded(t *testing.T) {
	if _, err := webassets.PublicFS.ReadFile("public/index.html"); err != nil {
		t.Fatalf("index.html is not embedded: %v", err)
	}
}
