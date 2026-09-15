package routes

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/webtest"
)

// ytdlpCardProbe runs SettingsController.loadYtdlpPluginStatus against one
// /api/ytdlp-plugin/status body and reports what landed on the card. The DOM
// stub carries only what the method touches: textContent, style, the badge's
// variant, and the install button's querySelector/prepend/append.
//
// style.display starts at the sentinel rather than "" so that "the alert is
// shown" is a real assertion. An element the method never touched would
// otherwise read back as "" — exactly what showing it looks like — and a test
// asserting display == "" would pass with the toggle line deleted.
const ytdlpCardProbe = `
globalThis.__startYtdlpCard = function (body) {
  const els = {};
  const mk = () => ({
    textContent: "",
    variant: "",
    style: { display: "__untouched__" },
    children: [],
    querySelector() { return null; },
    prepend(x) { this.children.unshift(x); },
    append(x) { if (typeof x === "string") { this.textContent += x; } else { this.children.push(x); } },
  });
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = mk(); return els[id]; },
    createElement() { return mk(); },
  };
  globalThis.fetch = function () { return { ok: true, json() { return body; } }; };
  let failure = null;
  globalThis.console = { error(...a) { failure = String(a); } };
  const inst = Object.create(SettingsController.prototype);
  inst.loadYtdlpPluginStatus();
  // Collected in a SECOND call: the method is async, so everything past its
  // first await runs when goja drains the job queue as this RunString returns.
  globalThis.__collectYtdlpCard = function () {
    const out = { __failure: { text: failure === null ? "" : failure, variant: "", display: "" } };
    for (const id of Object.keys(els)) {
      out[id] = { text: els[id].textContent, variant: els[id].variant || "", display: els[id].style.display || "" };
    }
    return out;
  };
};
`

type ytdlpCardElement struct {
	text    string
	variant string
	display string
}

func renderYtdlpCard(t *testing.T, body map[string]any) map[string]ytdlpCardElement {
	t.Helper()
	vm := webtest.SettingsVM(t)
	if _, err := vm.RunString(ytdlpCardProbe); err != nil {
		t.Fatalf("install the plugin-card probe: %v", err)
	}
	if err := vm.Set("__body", body); err != nil {
		t.Fatalf("hand the probe its response body: %v", err)
	}
	if _, err := vm.RunString("__startYtdlpCard(__body);"); err != nil {
		t.Fatalf("loadYtdlpPluginStatus threw — the browser would fail the same way: %v", err)
	}
	out, err := vm.RunString("__collectYtdlpCard();")
	if err != nil {
		t.Fatalf("collect the rendered card: %v", err)
	}
	raw, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T, want the per-element map", out.Export())
	}
	got := map[string]ytdlpCardElement{}
	for id, v := range raw {
		m, _ := v.(map[string]any)
		el := ytdlpCardElement{}
		el.text, _ = m["text"].(string)
		el.variant, _ = m["variant"].(string)
		el.display, _ = m["display"].(string)
		got[id] = el
	}
	if failure := got["__failure"].text; failure != "" {
		t.Fatalf("loadYtdlpPluginStatus reported %q — the render did not complete", failure)
	}
	delete(got, "__failure")
	return got
}

func pluginStatusBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"installed": true, "pluginDir": "/plug", "currentPort": 774,
		"httpsEnabled": false, "installedPort": nil, "portMismatch": false,
		"extractedPath": "/plug/moombox", "unparseable": false,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// TestPluginCardFlagsAnUnrecognisedFile: an installed-but-unparseable plugin
// used to render the green "Installed" badge and no warning at all.
//
// Mutant: dropping the unparseable arm from the badge chain leaves the badge
// reading "Installed" with variant success and fails this.
func TestPluginCardFlagsAnUnrecognisedFile(t *testing.T) {
	got := renderYtdlpCard(t, pluginStatusBody(map[string]any{"unparseable": true}))

	badge := got["ytdlp-plugin-status-badge"]
	if badge.variant != "warning" || badge.text != "Unrecognized file" {
		t.Errorf("badge = %q/%q, want warning/\"Unrecognized file\"", badge.variant, badge.text)
	}
	// "" is what the method writes to SHOW an element. The stub seeds
	// "__untouched__", so this also rejects the element never being written at
	// all — mutant: delete the unparseableWarning.style.display line.
	if got["ytdlp-unparseable-warning"].display != "" {
		t.Errorf("the unrecognised-file alert must be shown, display = %q", got["ytdlp-unparseable-warning"].display)
	}
	if got["ytdlp-port-mismatch-warning"].display != "none" {
		t.Error("the port-mismatch alert is about two known ports and must stay hidden")
	}
	// The alert says "Click Reinstall"; this is the button it names. An
	// unparseable file is installed-and-not-mismatched, which is the state
	// that sets _ytdlpReinstall — so the click passes force and the advice
	// works. A button reading "Update" or "Install" here would make the
	// alert's instruction point at nothing.
	if btn := got["ytdlp-install-btn"].text; btn != "Reinstall" {
		t.Errorf("install button reads %q, want \"Reinstall\" — the alert tells the operator to click it", btn)
	}
}

// TestPluginCardHealthyInstallIsUnchanged pins the path that must not move.
//
// Mutant: checking unparseable before installed (or forgetting the && in the
// new arm) turns a healthy install into a warning and fails this.
func TestPluginCardHealthyInstallIsUnchanged(t *testing.T) {
	got := renderYtdlpCard(t, pluginStatusBody(map[string]any{"installedPort": 774}))

	badge := got["ytdlp-plugin-status-badge"]
	if badge.variant != "success" || badge.text != "Installed" {
		t.Errorf("badge = %q/%q, want success/\"Installed\"", badge.variant, badge.text)
	}
	if got["ytdlp-unparseable-warning"].display != "none" {
		t.Error("the unrecognised-file alert must stay hidden for a healthy install")
	}
}

// TestPluginCardAlertsExistInMarkup is the premise the probe cannot supply.
//
// document.getElementById manufactures an element for any id asked of it, so a
// typo'd or absent id renders perfectly against nothing here. In a browser it
// returns null and the very next line — unparseableWarning.style.display = … —
// throws; loadYtdlpPluginStatus's own catch turns that into the red "Error"
// badge, so the WHOLE card degrades rather than just the alert going missing.
//
// Mutant: removing either sl-alert from index.html fails this.
func TestPluginCardAlertsExistInMarkup(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	for _, id := range []string{"ytdlp-unparseable-warning", "ytdlp-port-mismatch-warning"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html has no %s element: getElementById returns null in the browser, the "+
				"next style write throws, and the whole plugin card renders as Error", id)
		}
	}
}
