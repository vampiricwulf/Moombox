package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	webassets "github.com/vampiricwulf/Moombox/web"
)

// settingsVM loads the SHIPPED settings.js module into a goja runtime.
//
// The only transforms are stripping the ES `import` statement and the `export`
// keyword, neither of which goja parses; nothing else is rewritten, so what
// runs here is the source the binary serves. Line endings are normalised first
// because web/public is a mix of CRLF and LF.
//
// The module is DOM-coupled, but only inside methods — the class body and the
// module-level constants evaluate with no document at all, which is what makes
// both the constant reader and the populate probe below possible.
func settingsVM(t *testing.T) *goja.Runtime {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile("public/modules/settings.js")
	if err != nil {
		t.Fatalf("read the embedded settings.js: %v", err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	src = regexp.MustCompile(`(?s)import \{[^}]*\} from "\./utils\.js";`).ReplaceAllString(src, "")
	src = strings.ReplaceAll("\n"+src, "\nexport ", "\n")

	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("settings.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// populateProbe is the harness that RUNS SettingsController.populateConfigForm
// against a stub DOM and reports, for every element id, the value that landed
// on it.
//
// It stubs rather than mocks: `document.getElementById` hands back a Proxy that
// swallows any property or method the real elements would carry, so the method
// under test runs to completion without this harness having to enumerate the
// DOM surface of forty-odd Shoelace controls. Only `value` and `checked` are
// recorded, because those are the two ways a config value reaches a control —
// directly, or through app.setInputValue.
//
// The four methods stubbed out are the ones populateConfigForm calls that reach
// past the form: two render other panels, one wires an event listener, and
// updateAutoCookieUI is Task B's territory. None of them assigns a config value
// to a restart-required control.
const populateProbe = `
globalThis.__probeConfigForm = function (cfg) {
  const seen = {};
  const els = {};
  const makeEl = (id) =>
    new Proxy(
      {
        id,
        addEventListener() {},
        removeEventListener() {},
        getAttribute() { return ""; },
        setAttribute() {},
        removeAttribute() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
        appendChild() {},
        insertAdjacentElement() {},
        remove() {},
        focus() {},
        classList: { add() {}, remove() {}, contains() { return false; }, toggle() {} },
        style: { setProperty() {}, removeProperty() {}, getPropertyValue() { return ""; } },
        children: [],
      },
      {
        get(t, p) { return p in t ? t[p] : undefined; },
        set(t, p, v) {
          t[p] = v;
          if (p === "value" || p === "checked") seen[t.id] = v;
          return true;
        },
      },
    );
  globalThis.document = {
    getElementById(id) {
      if (!els[id]) els[id] = makeEl(id);
      return els[id];
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createElement(tag) { return makeEl("created:" + tag); },
    addEventListener() {},
  };

  const inst = Object.create(SettingsController.prototype);
  inst.app = {
    config: cfg,
    activePlatforms: {},
    setInputValue(id, v) { seen[id] = v; },
    getInputValue() { return ""; },
    getInputNumber() { return 0; },
    showToast() {},
  };
  for (const m of ["_wireTemplatePreview", "updateAutoCookieUI", "renderChannelsList", "renderNotificationsList"]) {
    inst[m] = function () {};
  }
  inst._netAccessListenerAdded = true;
  inst._dirtyListenersAdded = true;
  inst.populateConfigForm();
  return seen;
};
`

// runPopulate feeds one config object through populateConfigForm and returns
// the id -> value map the probe recorded.
func runPopulate(t *testing.T, vm *goja.Runtime, cfg map[string]any) map[string]any {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get("__probeConfigForm"))
	if !ok {
		t.Fatal("the populate probe did not install")
	}
	out, err := fn(goja.Undefined(), vm.ToValue(cfg))
	if err != nil {
		t.Fatalf("populateConfigForm threw — the browser would fail the same way: %v", err)
	}
	seen, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T, want the id map", out.Export())
	}
	return seen
}

// nestedConfig builds {section: {key: value}} from a dotted config path.
func nestedConfig(path string, value any) map[string]any {
	at := strings.LastIndex(path, ".")
	return map[string]any{path[:at]: map[string]any{path[at+1:]: value}}
}
