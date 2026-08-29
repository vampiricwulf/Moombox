package tui

import (
	"strings"
	"testing"

	"github.com/dop251/goja"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// listenerProbe RUNS SettingsController.setupListeners against a stub DOM and
// then FIRES one element's click handler, reporting which App methods it
// reached.
//
// Executed rather than pattern-matched for the usual reason, and for one more
// that is specific to this assertion: what matters is not that the file
// mentions autoCookieRefresh, it is that clicking THIS button reaches it. A
// source match cannot tell a wired listener from a commented-out one, and it
// certainly cannot tell which element the listener is on.
//
// The stub records addEventListener rather than swallowing it, and every App
// method is a recorder, so the click's whole reach is observable.
const listenerProbe = `
globalThis.__probeClick = function (targetId) {
  const handlers = {};
  const els = {};
  const makeEl = (id) =>
    new Proxy(
      {
        id,
        addEventListener(ev, fn) { (handlers[id] = handlers[id] || {})[ev] = fn; },
        removeEventListener() {},
        getAttribute() { return ""; },
        setAttribute() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
        appendChild() {},
        remove() {},
        focus() {},
        click() {},
        classList: { add() {}, remove() {}, contains() { return false; }, toggle() {} },
        style: {},
        children: [],
      },
      { get(t, p) { return p in t ? t[p] : undefined; }, set(t, p, v) { t[p] = v; return true; } },
    );
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = makeEl(id); return els[id]; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createElement(tag) { return makeEl("created:" + tag); },
    addEventListener() {},
    body: makeEl("body"),
  };
  globalThis.window = { addEventListener() {}, removeEventListener() {}, location: { href: "" } };

  const called = [];
  const app = new Proxy(
    { config: {}, activePlatforms: {} },
    {
      get(t, p) {
        if (p in t) return t[p];
        return function () { called.push(p); };
      },
    },
  );

  const inst = Object.create(SettingsController.prototype);
  inst.app = app;
  inst._netAccessListenerAdded = true;
  inst._dirtyListenersAdded = true;
  try { inst.setupListeners(); } catch (e) { return { error: String(e), wired: Object.keys(handlers) }; }

  const h = handlers[targetId] && handlers[targetId].click;
  if (!h) return { error: "no click listener on " + targetId, wired: Object.keys(handlers) };
  h({});
  return { called: called, wired: Object.keys(handlers) };
};
`

// TestProfileImportButtonReachesTheSameRefreshAsShiftClick is the touch-device
// half of the designated Docker workflow.
//
// `R F`'s Web twin is a SHIFT+CLICK, and a modifier key does not exist on a
// phone or a tablet. A mobile-only operator could therefore hold dead cookies,
// an updated browser profile, and no way whatever to trigger the import — on
// exactly the workflow the owner designated for Docker, where the profile
// import is the only cookie path there is.
//
// The assertion is that the new button reaches `app.autoCookieRefresh` — the
// SAME method shift+click calls, so it inherits the same endpoint and the same
// three-rung fallback rather than reimplementing them. `recheckCookies` must
// NOT be reached: that is the plain-click action, a different rung, and wiring
// this button to it would look like it worked while never importing anything.
func TestProfileImportButtonReachesTheSameRefreshAsShiftClick(t *testing.T) {
	vm := settingsVM(t)
	if _, err := vm.RunString(listenerProbe); err != nil {
		t.Fatalf("install the listener probe: %v", err)
	}
	fn, ok := goja.AssertFunction(vm.Get("__probeClick"))
	if !ok {
		t.Fatal("the listener probe did not install")
	}
	out, err := fn(goja.Undefined(), vm.ToValue("btn-import-browser-profile"))
	if err != nil {
		t.Fatalf("setupListeners threw — the browser would fail the same way: %v", err)
	}
	res, _ := out.Export().(map[string]any)
	if res == nil {
		t.Fatalf("the probe returned %T", out.Export())
	}
	if e, bad := res["error"].(string); bad {
		t.Fatalf("clicking btn-import-browser-profile reached nothing: %s\n"+
			"Without it a touch-only operator has no route to the profile import at all — "+
			"shift+click is the only other one, and a phone has no shift key.", e)
	}

	var called []string
	for _, v := range res["called"].([]any) {
		called = append(called, v.(string))
	}
	if !contains(called, "autoCookieRefresh") {
		t.Errorf("the button called %v, not app.autoCookieRefresh. It must reach the same method "+
			"shift+click does — that is what gives it the same endpoint and the same three-rung "+
			"fallback instead of a second implementation that can drift", called)
	}
	if contains(called, "recheckCookies") {
		t.Error("the button reached app.recheckCookies, which is the PLAIN-click action. That runs " +
			"the in-process refresh and never imports from the browser profile, so on a headless " +
			"host it would report success while changing nothing")
	}
}

// TestProfileImportButtonExists is the premise the test above rests on: the
// probe manufactures an element for any id, so a typo would wire a listener to
// nothing and still pass. In a browser getElementById returns null, the `if`
// skips, and the button is inert with no error anywhere.
func TestProfileImportButtonExists(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/index.html")
	if err != nil {
		t.Fatalf("read the embedded index.html: %v", err)
	}
	html := string(raw)
	for _, id := range []string{"btn-import-browser-profile", "auto-cookie-import-now"} {
		if !strings.Contains(html, `id="`+id+`"`) && !strings.Contains(html, `id='`+id+`'`) {
			t.Errorf("index.html has no element %q, so the touch route to the profile import does "+
				"not exist in the shipped page", id)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
