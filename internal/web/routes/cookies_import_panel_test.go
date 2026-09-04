package routes

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// The import panel, asserted by RUNNING the shipped settings.js against a stub
// DOM — same technique, same reason, as cookies_lasterror_panel_test.go: three
// rounds of review found three defects where an assertion about JS was written
// as a string match and stayed green while the behaviour it named was broken.

// settingsPanelVMWithUtils is settingsPanelVM plus the module settingsPanelVM
// deliberately strips.
//
// settingsPanelVM (cookies_lasterror_panel_test.go:33) removes settings.js's
// `import {...} from "./utils.js"` line, because goja parses no ES modules, and
// nothing puts those helpers back. Its one existing consumer,
// loadAutoCookieStatus, references none of them — importCookies references
// three: serverErrorMessage, cookieSetupAcceptedToast and
// cookieSetupRejectedMessage. In a VM without them each is a ReferenceError
// thrown inside the method's own try/catch, which renders it into the result
// div: no toast is ever pushed, `failure` stays empty because the catch handled
// it, and three of the four tests below would fail against a perfectly correct
// implementation.
//
// utils.js is evaluated FIRST, under utilsVM's transform (strip `export`), so
// the helpers are ordinary global function declarations by the time settings.js
// is parsed. What the assertions then measure is the SHIPPED copy — the same
// cookieSetupAcceptedToast the browser calls, not a stub of it, which is what
// makes the hedged-copy assertion below worth anything.
func settingsPanelVMWithUtils(t *testing.T) *goja.Runtime {
	t.Helper()
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	utils = strings.ReplaceAll("\n"+utils, "\nexport ", "\n")

	settings := readEmbeddedModule(t, "public/modules/settings.js")
	settings = regexp.MustCompile(`(?s)import \{[^}]*\} from "\./utils\.js";`).ReplaceAllString(settings, "")
	settings = strings.ReplaceAll("\n"+settings, "\nexport ", "\n")

	vm := goja.New()
	if _, err := vm.RunString(utils); err != nil {
		t.Fatalf("utils.js does not evaluate — the browser would fail the same way: %v", err)
	}
	if _, err := vm.RunString(settings); err != nil {
		t.Fatalf("settings.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// FormData is stubbed as a recorder rather than skipped. The multipart branch is
// the one a phone uses, and "the file picker posts something" is the whole claim
// worth making about it from here; the server side of the same branch is pinned
// by TestImportRouteAcceptsAMultipartUpload.
const importPanelProbe = `
globalThis.__startImportPanel = function (opts) {
  const els = {};
  const mk = (id) => ({
    id, textContent: "", value: "", files: [], style: {},
    addEventListener() {}, focus() { this.focused = true; }, click() { this.clicked = true; },
  });
  globalThis.document = {
    getElementById(id) { if (!els[id]) els[id] = mk(id); return els[id]; },
    querySelector() { return { click() {} }; },
  };
  globalThis.setTimeout = function (fn) { fn(); return 0; };
  globalThis.FormData = function () { this.parts = {}; };
  globalThis.FormData.prototype.append = function (k, v) { this.parts[k] = v; };
  const sent = {};
  globalThis.fetch = function (url, init) {
    sent.url = url;
    sent.method = init && init.method;
    sent.contentType = init && init.headers && init.headers["Content-Type"];
    sent.bodyIsForm = !!(init && init.body instanceof globalThis.FormData);
    sent.body = init && init.body;
    return {
      ok: opts.ok,
      status: opts.status || (opts.ok ? 200 : 422),
      json() { return opts.body; },
      text() { return JSON.stringify(opts.body); },
    };
  };
  let failure = null;
  globalThis.console = { error(...a) { failure = String(a); } };

  const inst = Object.create(SettingsController.prototype);
  inst.app = { showToast(m, v) { (inst.__toasts = inst.__toasts || []).push({ message: m, variant: v }); },
               loadStatus() {}, escapeHtml(s) { return s; } };
  if (opts.text !== undefined) document.getElementById("cookie-import-text").value = opts.text;
  if (opts.file !== undefined) document.getElementById("cookie-import-file").files = [opts.file];
  inst.importCookies();

  globalThis.__collectImportPanel = function () {
    return {
      failure: failure === null ? "" : failure,
      result: els["cookie-import-result"] ? els["cookie-import-result"].textContent : "",
      color: els["cookie-import-result"] && els["cookie-import-result"].style.color || "",
      toasts: inst.__toasts || [],
      sent: sent,
    };
  };
};
`

type importPanelRun struct {
	failure string
	result  string
	color   string
	toasts  []map[string]any
	sent    map[string]any
}

func runImportPanel(t *testing.T, opts map[string]any) importPanelRun {
	t.Helper()
	vm := settingsPanelVMWithUtils(t)
	if _, err := vm.RunString(importPanelProbe); err != nil {
		t.Fatalf("install the import panel probe: %v", err)
	}
	if err := vm.Set("__opts", opts); err != nil {
		t.Fatalf("hand the probe its options: %v", err)
	}
	// Two RunStrings: the first starts the async handler and, on return, drains
	// the promise job queue that carries the rest of it.
	if _, err := vm.RunString("__startImportPanel(__opts);"); err != nil {
		t.Fatalf("importCookies threw — the browser would fail the same way: %v", err)
	}
	out, err := vm.RunString("__collectImportPanel();")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	raw, ok := out.Export().(map[string]any)
	if !ok {
		t.Fatalf("the probe returned %T", out.Export())
	}
	run := importPanelRun{}
	run.failure, _ = raw["failure"].(string)
	run.result, _ = raw["result"].(string)
	run.color, _ = raw["color"].(string)
	run.sent, _ = raw["sent"].(map[string]any)
	if toasts, ok := raw["toasts"].([]any); ok {
		for _, tv := range toasts {
			if m, ok := tv.(map[string]any); ok {
				run.toasts = append(run.toasts, m)
			}
		}
	}
	if run.failure != "" {
		t.Fatalf("importCookies reported %q — the render did not complete", run.failure)
	}
	return run
}

// TestImportPanelPostsThePasteAndReportsTheVerdict.
//
// The mutations: posting to the wrong URL or with GET (the endpoint has no GET
// and answers 405, so the panel would report a transport error for every
// import); reading `data.success` instead of the per-platform fields (an
// import Moombox could not verify would be toasted as "cookies configured",
// which is the exact claim cookieSetupAcceptedToast exists to refuse); sending
// the paste with any Content-Type but text/plain (readCookieImportBody's
// switch answers 415 to anything else, so a mistaken application/json would
// 415 every paste while every OTHER assertion here — url, method, toasts —
// stayed green, because the fixture records the header but nothing had read
// it back).
func TestImportPanelPostsThePasteAndReportsTheVerdict(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "ok", "twitchVerification": "failed",
		},
	})

	if run.sent["url"] != "/api/cookies/import" {
		t.Errorf("posted to %v, want /api/cookies/import", run.sent["url"])
	}
	if run.sent["method"] != "POST" {
		t.Errorf("method = %v, want POST", run.sent["method"])
	}
	if run.sent["contentType"] != "text/plain" {
		t.Errorf("Content-Type = %v, want text/plain — the server's readCookieImportBody switches "+
			"on this header, and anything but text/plain, empty or application/octet-stream 415s "+
			"every paste", run.sent["contentType"])
	}
	if len(run.toasts) != 1 {
		t.Fatalf("toasts = %v, want exactly one (YouTube accepted, Twitch not)", run.toasts)
	}
	if got, _ := run.toasts[0]["message"].(string); !strings.Contains(got, "YouTube") {
		t.Errorf("toast = %q, want the YouTube one", got)
	}
	if got, _ := run.toasts[0]["variant"].(string); got != "success" {
		t.Errorf("variant = %q, want success for a verified import", got)
	}
}

// TestImportPanelHedgesWhenTheCheckCouldNotConclude. Accepted is not verified:
// the operator's paste was saved and in use, and Moombox could not reach the
// site to confirm it.
//
// The mutation: dropping the verification argument from the toast call, which
// makes the helper's default arm report an unqualified success for a check that
// never concluded.
func TestImportPanelHedgesWhenTheCheckCouldNotConclude(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "unknown", "twitchVerification": "failed",
		},
	})
	if len(run.toasts) != 1 {
		t.Fatalf("toasts = %v, want one", run.toasts)
	}
	msg, _ := run.toasts[0]["message"].(string)
	if !strings.Contains(msg, "could not establish") {
		t.Errorf("toast = %q, want the hedged copy for an inconclusive check", msg)
	}
	if v, _ := run.toasts[0]["variant"].(string); v != "warning" {
		t.Errorf("variant = %q, want warning", v)
	}
}

// TestImportPanelSaysWhenThePasteWasGivenBack is the UI half of the rollback:
// the server verified the pasted rows, rejected them, restored the previous
// ones and re-verified THOSE — so `authenticated` is true and the verification
// reads "ok", truthfully, about credentials the operator did not paste.
//
// Every field the panel read before this change therefore says success. The
// only thing that says otherwise is youtubeImport, and a panel that ignores it
// toasts "YouTube cookies configured" in green over a paste that was thrown
// out — the operator closes the dialog believing their re-authentication
// landed, and finds out at the next members-only stream.
//
// The mutation: dropping the rolled-back arm, or keeping it and still firing
// the accepted toast beside it (two contradictory toasts for one platform).
func TestImportPanelSaysWhenThePasteWasGivenBack(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "ok", "twitchVerification": "failed",
			"youtubeImport": "rolled-back", "twitchImport": "unchanged",
		},
	})
	if len(run.toasts) != 1 {
		t.Fatalf("toasts = %v, want exactly one — the rolled-back toast, and NOT the accepted one "+
			"beside it", run.toasts)
	}
	msg, _ := run.toasts[0]["message"].(string)
	if !strings.Contains(msg, "YouTube") || !strings.Contains(msg, "kept the working ones") {
		t.Errorf("toast = %q, want the copy that says the paste was refused and the previous "+
			"credentials kept", msg)
	}
	if strings.Contains(msg, "configured") {
		t.Errorf("toast = %q — that is the accepted copy, over a paste that was rolled back", msg)
	}
	if v, _ := run.toasts[0]["variant"].(string); v != "warning" {
		t.Errorf("variant = %q, want warning: nothing is broken — the session that worked before the "+
			"paste is still working", v)
	}
}

// TestImportPanelDoesNotClaimAWorkingSessionAfterAFailedRestore is the other
// half of the rolled-back arm, and the reason its toast is guarded on
// `accepted` rather than on the outcome alone.
//
// A rollback re-verifies what it KEPT, and that check can come back
// not-accepted: the previous rows verified before the write and are rejected
// after it, which is what an expiring session mid-import looks like. The
// outcome is still "rolled-back" — the rows really were given back — but
// nothing authenticates any more. Toasting "Moombox kept the working ones it
// already had" beside the inline "No login detected. Try again." answers one
// gesture twice, contradicting itself, and the inline half is the true one.
//
// The mutation: firing the toast on `outcome === "rolled-back"` alone.
func TestImportPanelDoesNotClaimAWorkingSessionAfterAFailedRestore(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{
			"success": true, "authenticated": false, "twitchAuthenticated": false,
			"youtubeVerification": "failed", "twitchVerification": "failed",
			"youtubeImport": "rolled-back", "twitchImport": "unchanged",
		},
	})
	if len(run.toasts) != 0 {
		t.Fatalf("toasts = %v, want none — nothing authenticates, so no toast may say a working "+
			"session was kept", run.toasts)
	}
	if !strings.Contains(run.result, "No login detected") {
		t.Errorf("inline result = %q, want the rejection message: it is the only true answer here",
			run.result)
	}
}

// TestImportPanelShowsTheServersRefusalInline. The three refusals are the whole
// diagnostic value of the endpoint; a panel that renders "HTTP 422" throws it
// away.
//
// The mutation: `throw new Error("HTTP " + response.status)` instead of
// serverErrorMessage(response) — the shape the wizard's finish handler had
// before it was fixed.
func TestImportPanelShowsTheServersRefusalInline(t *testing.T) {
	const refusal = "that cookie file holds no YouTube or Twitch login cookie"
	run := runImportPanel(t, map[string]any{
		"ok": false, "status": 422,
		"text": "# Netscape HTTP Cookie File\n",
		"body": map[string]any{"error": refusal},
	})
	if !strings.Contains(run.result, refusal) {
		t.Errorf("inline result = %q, want the server's own sentence", run.result)
	}
	if run.color == "" {
		t.Error("the refusal is rendered in the panel's default colour and reads as help text")
	}
	if len(run.toasts) != 0 {
		t.Errorf("a refused import still toasted a success: %v", run.toasts)
	}
}

// TestImportPanelUploadsTheChosenFileAsMultipart. With the textarea empty and a
// file chosen, the request must carry the file in a `cookies` part — the part
// name the server reads.
//
// The mutations: posting the File object as a text/plain body (the server reads
// "[object File]" and answers "not a Netscape cookie file"); naming the part
// anything else (400, "the upload has no `cookies` file part").
func TestImportPanelUploadsTheChosenFileAsMultipart(t *testing.T) {
	run := runImportPanel(t, map[string]any{
		"ok":   true,
		"text": "",
		"file": map[string]any{"name": "cookies.txt"},
		"body": map[string]any{
			"success": true, "authenticated": true, "twitchAuthenticated": false,
			"youtubeVerification": "ok", "twitchVerification": "failed",
		},
	})
	if run.sent["bodyIsForm"] != true {
		t.Fatalf("the request body is not a FormData: %v", run.sent["body"])
	}
	if ct := run.sent["contentType"]; ct != nil && ct != "" {
		t.Errorf("Content-Type = %v — a multipart request must let the browser set its own "+
			"boundary; an explicit header makes the body unparseable", ct)
	}
	body, _ := run.sent["body"].(map[string]any)
	parts, _ := body["parts"].(map[string]any)
	if _, ok := parts["cookies"]; !ok {
		t.Errorf("the form has no `cookies` part: %v", parts)
	}
}

// TestReloginPromptTargetsTheImportUnlessTheWizardCanActuallyHelp is the pure
// decision the two header-warning handlers share, run out of the shipped
// utils.js.
//
// FOUR rows and none is padding, because there are two independent reasons the
// wizard is useless and each has its own mutant:
//
//   - drop the hostname test: a phone or a LAN laptop is sent to a wizard that
//     opens a login window on the HOST's screen. The click appears to do
//     nothing and nothing anywhere says why. The server does NOT refuse that
//     client — the loopback gate covers /api/setup/complete, not the cookie
//     setup trio — so this row is the only thing that stops it.
//   - drop the availableBrowsers test: a container operator sitting at the host
//     (docker exec, a local port-forward) is sent to a wizard that has no
//     browser to launch.
//   - invert either: a local desktop operator loses the one-click login for no
//     reason.
//   - the unreadable status is the fourth row: /auto-status can fail, and the
//     panel "import" opens holds BOTH controls, so answering "import" costs a
//     local user one click while answering "wizard" costs everyone else the only
//     route they have.
func TestReloginPromptTargetsTheImportUnlessTheWizardCanActuallyHelp(t *testing.T) {
	vm := utilsVM(t)
	withBrowser := map[string]any{"availableBrowsers": []any{map[string]any{"name": "Firefox"}}}
	noBrowser := map[string]any{"availableBrowsers": []any{}}

	for _, tc := range []struct {
		name     string
		status   any
		hostname string
		want     string
	}{
		{"at the host, with a browser", withBrowser, "localhost", "wizard"},
		{"at the host over IPv6 loopback, with a browser", withBrowser, "[::1]", "wizard"},
		{"at the host, no browser (the container shape)", noBrowser, "127.0.0.1", "import"},
		{"a LAN client of a host that has a browser", withBrowser, "192.168.1.20", "import"},
		{"a tunnelled client of a host that has a browser", withBrowser, "moombox.example.ts.net", "import"},
		{"a hostname that merely contains localhost", withBrowser, "localhost.evil.example", "import"},
		{"a hostname that merely contains 127.0.0.1", withBrowser, "127.0.0.1.evil.example", "import"},
		{"a hostname that merely contains ::1", withBrowser, "::1.evil.example", "import"},
		{"a hostname that merely contains [::1]", withBrowser, "[::1].evil.example", "import"},
		{"the status could not be read, at the host", nil, "localhost", "import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := jsCall(t, vm, "reloginPromptTarget", tc.status, tc.hostname)
			if got != tc.want {
				t.Errorf("reloginPromptTarget(%v, %q) = %v, want %q", tc.status, tc.hostname, got, tc.want)
			}
		})
	}
}
