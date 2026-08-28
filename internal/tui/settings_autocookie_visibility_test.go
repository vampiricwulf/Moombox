package tui

import (
	"maps"
	"strings"
	"testing"

	"github.com/dop251/goja"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// autoCookieProbe RUNS SettingsController.updateAutoCookieUI against a stub DOM
// and reports what it did.
//
// It lives beside the populate probe in settings_js_vm_test.go for the same
// reason and with the same transforms — the shipped settings.js is executed,
// not pattern-matched, because a source match passes on code that is commented
// out, shadowed or unreachable.
//
// Three things are recorded, and each answers a different decoy:
//
//   - display: the style.display each element ended up with. Exact strings, so
//     "" (inherit, i.e. shown) and "none" cannot be conflated.
//   - consulted: every id the method asked document for, so the switch's
//     element can be shown to be untouched rather than merely ineffective.
//   - loaded: whether loadAutoCookieStatus ran. It is what fills the browser
//     selector and writes the "no supported browser detected" line, so a
//     selector shown without it is an empty box.
const autoCookieProbe = `
globalThis.__probeAutoCookieUI = function (checked, config) {
  const els = {};
  const mk = (id) => ({ id, checked: checked[id] === true, style: {} });
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = mk(id); return els[id]; },
  };
  const inst = Object.create(SettingsController.prototype);
  let loaded = 0;
  inst.loadAutoCookieStatus = function () { loaded++; };
  inst.app = { config: config };
  inst.updateAutoCookieUI();
  const display = {};
  for (const id of Object.keys(els)) {
    display[id] = "display" in els[id].style ? els[id].style.display : "<untouched>";
  }
  return { display: display, loaded: loaded, consulted: Object.keys(els).sort() };
};
`

type autoCookieRender struct {
	display   map[string]string
	consulted []string
	loaded    bool
}

// renderAutoCookieUI runs the probe for one combination of switch states.
func renderAutoCookieUI(t *testing.T, vm *goja.Runtime, autoEnabled, ytActive, twActive bool) autoCookieRender {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get("__probeAutoCookieUI"))
	if !ok {
		t.Fatal("the auto-cookie probe did not install")
	}
	checked := map[string]any{
		"cfg-auto-cookies-enabled": autoEnabled,
		"cfg-active-youtube":       ytActive,
		"cfg-active-twitch":        twActive,
	}
	// The flag is varied through the config too, so a rewrite that reads it
	// from there instead of from the switch is caught by the same comparison.
	config := map[string]any{"cookies": map[string]any{"auto_enabled": autoEnabled}}

	out, err := fn(goja.Undefined(), vm.ToValue(checked), vm.ToValue(config))
	if err != nil {
		t.Fatalf("updateAutoCookieUI threw — the browser would fail the same way: %v", err)
	}
	raw, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T, want the result object", out.Export())
	}

	got := autoCookieRender{display: map[string]string{}}
	rawDisplay, _ := raw["display"].(map[string]any)
	for id, v := range rawDisplay {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("style.display for %q is %T, want a string", id, v)
		}
		got.display[id] = s
	}
	for _, v := range raw["consulted"].([]any) {
		got.consulted = append(got.consulted, v.(string))
	}
	got.loaded = toInt64(raw["loaded"]) > 0
	return got
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// autoCookieVM loads settings.js plus the probe.
func autoCookieVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := settingsVM(t)
	if _, err := vm.RunString(autoCookieProbe); err != nil {
		t.Fatalf("install the auto-cookie probe: %v", err)
	}
	return vm
}

// TestSetupButtonsAreNotHiddenByTheAutoCookieSwitch is the Web half of the same
// mistake the R F chord was.
//
// The dashboard's setup buttons and browser selector used to be display:none
// whenever cfg-auto-cookies-enabled was off. Setup is ACQUISITION — it is how an
// operator SUPPLIES cookies, it opens a visible browser window, and
// AutoEnabled appears nowhere in internal/cookies precisely so StartSetup stays
// reachable with the flag off. So the install with the flag off is the install
// that most needs those buttons, and it was the one with them hidden. Worse, it
// made the setting unreachable the honest way: a fresh install has the flag
// false by definition, and running setup is what earns turning it on.
//
// The flag governs one timer and one automatic retry. It has no say here.
//
// Asserted by EXECUTING the shipped module, and asserted as an INVARIANCE: the
// same platform toggles with the flag on and with it off must render
// identically, down to the exact display strings. A per-row expectation could be
// satisfied by code that hides something else instead; invariance cannot. The
// flag is varied through both routes it could be read by — the switch element
// and app.config — and the switch's element is additionally required to go
// unconsulted.
func TestSetupButtonsAreNotHiddenByTheAutoCookieSwitch(t *testing.T) {
	vm := autoCookieVM(t)

	for _, tc := range []struct {
		name               string
		ytActive, twActive bool
	}{
		{"both platforms active", true, true},
		{"youtube only", true, false},
		{"twitch only", false, true},
		{"neither platform active", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := renderAutoCookieUI(t, vm, true, tc.ytActive, tc.twActive)
			off := renderAutoCookieUI(t, vm, false, tc.ytActive, tc.twActive)

			if !maps.Equal(on.display, off.display) {
				t.Errorf("the auto-cookie switch still changes what the settings page shows.\n"+
					" flag on:  %v\n flag off: %v\n"+
					"Everything here is acquisition — the browser login that puts cookies on disk, and "+
					"the choice of which browser runs it. The flag adds a refresh timer; it does not "+
					"decide whether an operator may log in", on.display, off.display)
			}
			for _, id := range off.consulted {
				if id == "cfg-auto-cookies-enabled" {
					t.Error("updateAutoCookieUI reads cfg-auto-cookies-enabled. Even if it currently " +
						"renders the same either way, the read is the bug returning: the switch has " +
						"no say over acquisition")
				}
			}
			if !off.loaded {
				t.Error("loadAutoCookieStatus did not run with the flag off, so the browser selector " +
					"that is now always visible would be an empty box and auto-cookie-browser-info " +
					"would never say which browser was detected. It also sets _browserSelectLoaded, " +
					"without which saving the form cannot carry the browser choice")
			}

			// The container and the selector are unconditional; the per-platform
			// buttons still follow their own toggles, which is the one gating
			// that survives.
			want := map[string]string{
				"auto-cookie-actions":          "",
				"auto-cookie-browser-selector": "",
				"btn-auto-cookie-setup-yt":     displayFor(tc.ytActive),
				"btn-auto-cookie-setup-tw":     displayFor(tc.twActive),
			}
			for id, wantDisplay := range want {
				got, seen := off.display[id]
				if !seen {
					t.Errorf("updateAutoCookieUI never touched %q — it can no longer show or hide it", id)
					continue
				}
				if got != wantDisplay {
					t.Errorf("%s style.display = %q, want %q", id, got, wantDisplay)
				}
			}
		})
	}
}

func displayFor(shown bool) string {
	if shown {
		return ""
	}
	return "none"
}

// TestAutoCookieVisibilityIdsExist is the premise the test above rests on.
//
// The probe manufactures an element for any id asked of it, so a typo would
// render "correctly" against nothing. getElementById in a browser returns null
// and the code skips the assignment silently — no error, no console warning, and
// a setup button that stays hidden forever.
func TestAutoCookieVisibilityIdsExist(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/index.html")
	if err != nil {
		t.Fatalf("read the embedded index.html: %v", err)
	}
	html := string(raw)

	vm := autoCookieVM(t)
	got := renderAutoCookieUI(t, vm, false, true, true)
	if len(got.consulted) == 0 {
		t.Fatal("updateAutoCookieUI consulted no elements at all")
	}
	for _, id := range got.consulted {
		if !strings.Contains(html, `id="`+id+`"`) && !strings.Contains(html, `id='`+id+`'`) {
			t.Errorf("updateAutoCookieUI drives element %q, which does not exist in index.html — "+
				"getElementById returns null and the control is never shown or hidden", id)
		}
	}
}
