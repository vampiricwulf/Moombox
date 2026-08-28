package routes

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// setupDialogModules are the two frontends that drive the cookie setup dialog.
// Both call the same endpoints and both had the same three defects, so every
// assertion below runs against both — a fix landed in one file only is exactly
// how these two drifted apart before.
var setupDialogModules = []string{
	"public/modules/settings.js",
	"public/modules/setup.js",
}

// readEmbeddedModule returns the asset the binary actually serves, with line
// endings normalised. Some modules in web/public are CRLF and some are LF, and
// a multi-line assertion that happens to work under one is a trap under the
// other; normalising here makes every assertion below ending-agnostic.
func readEmbeddedModule(t *testing.T, path string) string {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// jsFunctionBody slices one exported function out of a module so an assertion
// can name the SITE it is about.
//
// A file-wide strings.Contains cannot: the copy helpers each carry the same
// comparison, so inverting one of them leaves a whole-file check green. That is
// the junction defect this plan keeps rediscovering, and it is only avoidable
// by bracketing.
func jsFunctionBody(t *testing.T, js, name string) string {
	t.Helper()
	var header string
	at := -1
	for _, candidate := range []string{
		"export function " + name + "(",
		"export async function " + name + "(",
		"\nfunction " + name + "(",
	} {
		if i := strings.Index(js, candidate); i >= 0 {
			header, at = candidate, i
			break
		}
	}
	if at < 0 {
		t.Fatalf("utils.js no longer exports %s — the setup dialogs import it", name)
	}
	body := js[at+len(header):]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// TestCookieSetupOutcomeCarriesTheVerificationBesideTheAcceptance pins the
// wire fields the setup dialog branches on.
//
// The defect: `authenticated` alone cannot distinguish "YouTube confirmed this
// sign-in" from "Moombox saved the cookies and could not reach YouTube to ask".
// FinishSetup has computed both states since the tri-state landed and accepts
// the second on purpose — a 429 or a DNS blip is not evidence against a login
// that happened thirty seconds ago — so the dialog lit the same green badge for
// each, asserting a verification that never ran.
//
// The verification fields are ADDITIVE: `authenticated` keeps its exact old
// meaning, so an older frontend against a newer binary behaves as it did. Same
// precedent `ran`/`verdict` set for the refresh outcome.
//
// The "conclusively rejected" row is the premise for the others. Without it, a
// payload that simply never reported a verdict would satisfy every assertion
// here by saying nothing at all.
func TestCookieSetupOutcomeCarriesTheVerificationBesideTheAcceptance(t *testing.T) {
	tests := []struct {
		name        string
		result      cookies.SetupResult
		wantYTAuth  bool
		wantTWAuth  bool
		wantYTVerif string
		wantTWVerif string
	}{
		{
			name: "verified",
			result: cookies.SetupResult{
				YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
			},
			wantYTAuth: true, wantYTVerif: "ok", wantTWVerif: "failed",
		},
		{
			// THE FIX. Accepted and unverified at the same time, which is the
			// pair no two-boolean response could express.
			name: "accepted but the site could not be reached",
			result: cookies.SetupResult{
				YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
			},
			wantYTAuth: true, wantYTVerif: "unknown", wantTWVerif: "failed",
		},
		{
			// Its mirror image: also unknown, also no finding about the
			// credentials, and NOT accepted — no request was ever made.
			name: "extracted cookies that cannot sign a request",
			result: cookies.SetupResult{
				YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed,
			},
			wantYTVerif: "unknown", wantTWVerif: "failed",
		},
		{
			name: "conclusively rejected",
			result: cookies.SetupResult{
				YouTube: cookies.RefreshFailed, Twitch: cookies.RefreshFailed,
			},
			wantYTVerif: "failed", wantTWVerif: "failed",
		},
		{
			name: "both platforms in one setup",
			result: cookies.SetupResult{
				YouTube: cookies.RefreshOK, Twitch: cookies.RefreshUnknown,
				YouTubeAccepted: true, TwitchAccepted: true,
			},
			wantYTAuth: true, wantTWAuth: true, wantYTVerif: "ok", wantTWVerif: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cookieSetupOutcome(tt.result)

			if got["success"] != true {
				t.Errorf("success = %v, want true — the handler only reaches this shape on a "+
					"finish that returned no error", got["success"])
			}
			if got["authenticated"] != tt.wantYTAuth {
				t.Errorf("authenticated = %v, want %v — its meaning must not change",
					got["authenticated"], tt.wantYTAuth)
			}
			if got["twitchAuthenticated"] != tt.wantTWAuth {
				t.Errorf("twitchAuthenticated = %v, want %v", got["twitchAuthenticated"], tt.wantTWAuth)
			}
			if got["youtubeVerification"] != tt.wantYTVerif {
				t.Errorf("youtubeVerification = %v, want %q — the dialog cannot tell a confirmed "+
					"sign-in from one nothing checked without it", got["youtubeVerification"], tt.wantYTVerif)
			}
			if got["twitchVerification"] != tt.wantTWVerif {
				t.Errorf("twitchVerification = %v, want %q", got["twitchVerification"], tt.wantTWVerif)
			}
		})
	}
}

// TestSetupDialogsReadTheVerificationFieldsTheHandlerEmits is the Go↔JS seam
// nothing else can see: the dialog's branch conditions are JavaScript and no JS
// harness exists in-tree. Copied from TestAppJSReadsTheFieldsTheHandlerEmits,
// which pinned the refresh toast's fields the same way.
//
// The realistic drift is a Go-side rename leaving the modules reading
// `undefined`, which is worse than a crash: `undefined === "unknown"` is false,
// so a renamed field silently stops the hedged arm from ever firing and every
// setup goes back to claiming an unqualified success.
//
// The direction of the comparison is load-bearing and asserted verbatim. The
// copy branches on `=== "unknown"`, never on `!== "ok"`, which is what makes
// the additive claim hold in the other direction too: against an older binary
// that emits neither field the value is undefined, matches neither arm, and the
// copy degrades to what that binary's users already see. Inverted, a missing
// field would hedge about every setup against every older build.
func TestSetupDialogsReadTheVerificationFieldsTheHandlerEmits(t *testing.T) {
	payload := cookieSetupOutcome(cookies.SetupResult{})
	for _, key := range []string{"youtubeVerification", "twitchVerification"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("cookieSetupOutcome no longer emits %q, but the setup dialogs still read "+
				"data.%s — they would compare against undefined", key, key)
		}
	}

	for _, path := range setupDialogModules {
		js := readEmbeddedModule(t, path)
		for _, expr := range []string{"data.youtubeVerification", "data.twitchVerification"} {
			if !strings.Contains(js, expr) {
				t.Errorf("%s never reads %s — it can only say \"configured\" or \"no login "+
					"detected\", and the middle state is invisible again", path, expr)
			}
		}
	}

	// The three-way copy lives in one place so the two dialogs cannot drift
	// apart, and the comparison direction lives with it.
	//
	// BRACKETED PER FUNCTION, not file-wide. Both helpers carry the same
	// literal, so a whole-file Contains stays green when only ONE of them is
	// inverted — and the one that carries the backward-compat property is
	// cookieSetupAcceptedToast, the branch an older binary's missing field must
	// fall past. A file-wide check asserting "verbatim" would have been a claim
	// the test did not make.
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	for _, fn := range []string{"cookieSetupAcceptedToast", "cookieSetupRejectedMessage"} {
		body := jsFunctionBody(t, utils, fn)
		if !strings.Contains(body, `verification === "unknown"`) {
			t.Errorf(`%s does not branch on verification === "unknown"`, fn)
		}
		// The exact inversion the doc comment names. Written this way round, a
		// field an older binary never sends would hedge about every setup.
		if strings.Contains(body, "verification !==") {
			t.Errorf("%s branches NEGATIVELY on the verification string. Against an older binary "+
				"the field is undefined, which fails every positive test and passes every negative "+
				"one — the hedged copy would fire for users whose check ran perfectly well", fn)
		}
	}
	if !strings.Contains(jsFunctionBody(t, utils, "cookieSetupAcceptedToast"), `verification === "failed"`) {
		t.Error(`cookieSetupAcceptedToast no longer distinguishes verification === "failed"`)
	}
	// Reuse of the refresh vocabulary, not a fourth phrasing for the same three
	// states. cookies.RefreshVerdict.String() produces these exact words.
	if !strings.Contains(utils, "could not establish") {
		t.Error("utils.js words the inconclusive setup outcome in something other than the " +
			"\"could not establish\" wording the manual-refresh surfaces already use")
	}
}

// TestSetupDialogsSurfaceTheServersOwnMessage is S15.
//
// Four call sites did `throw new Error(\`HTTP ${response.status}\`)`, discarding
// a body the handler had already written: the finish endpoint alone
// discriminates ErrCookieFileUnreadable, ErrCookieDBNotFound and
// ErrCookieDBUnreadable into 422 and ErrCookieDBLocked into 409, each with its
// own text naming the file and the remedy. All four arrived at the user as
// "HTTP 422", which points at none of them.
//
// Asserted as an absence as well as a presence: adding the helper while leaving
// one `HTTP ${response.status}` throw behind fixes three sites out of four.
func TestSetupDialogsSurfaceTheServersOwnMessage(t *testing.T) {
	for _, path := range setupDialogModules {
		js := readEmbeddedModule(t, path)

		if strings.Contains(js, "throw new Error(`HTTP ${response.status}`)") {
			t.Errorf("%s still flattens a discriminated server error to its status code", path)
		}
		if !strings.Contains(js, "await serverErrorMessage(response)") {
			t.Errorf("%s does not read the server's message off the failed response", path)
		}
	}

	// The helper has to actually read the field jsonError writes. `data.error`
	// is the established render idiom across this frontend; the point of S15 is
	// that these paths never asked for it.
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	if !strings.Contains(utils, "data.error") {
		t.Error("serverErrorMessage does not read data.error — jsonError's body is still discarded")
	}
}

// TestSetupAbortReportsRealStatusInsteadOfAssertingATimeout is S17.
//
// Both dialogs said "Cookie extraction timed out. The browser window may still
// be open." on the 60 s abort. Server-side, FinishSetup writes the merged
// cookies.txt and reloads the jar BEFORE it verifies, so that abort can fire
// over work that has already committed — and the message invited the user to
// redo a sign-in that succeeded.
//
// The remedy is to ask. /api/cookies/auto-status reports whether the setup slot
// is still held — but that alone is NOT the answer, which is the second half of
// this test. cleanup() frees the slot on every failure path too, so a report
// built on setupInProgress announces "your cookies were saved" in exactly the
// case the 422 arm guaranteed they were not. lastError and lastRefresh are in
// the same response body and are what make the verdict honest.
//
// The 60 s cap is asserted UNCHANGED, and that is not incidental. Raising it
// looks like a fix for the same symptom and would break a cross-language
// relationship no other test spans: the server-side setup grace window is
// priced against this exact constant.
func TestSetupAbortReportsRealStatusInsteadOfAssertingATimeout(t *testing.T) {
	for _, path := range setupDialogModules {
		js := readEmbeddedModule(t, path)

		if strings.Contains(js, "Cookie extraction timed out") {
			t.Errorf("%s still asserts a timeout over work the server may have committed", path)
		}
		// HARD CONSTRAINT. See the doc comment above.
		if !strings.Contains(js, "controller.abort(), 60000") {
			t.Errorf("%s changed the 60 s client cap. The server-side setup grace window is "+
				"priced against that exact number — report what happened instead of waiting longer", path)
		}

		// The baseline has to be dispatched BEFORE the finish, or the
		// lastRefresh comparison is browser clock against server clock and a
		// skewed host turns a stale stamp into a fresh one. Asserted as an
		// ORDERING, because both lines existing in the wrong order reads
		// identically to a Contains check.
		baselineAt := strings.Index(js, "const abortBaseline = cookieSetupProbe();")
		finishAt := strings.Index(js, `fetch("/api/cookies/auto-setup/finish"`)
		switch {
		case baselineAt < 0:
			t.Errorf("%s takes no pre-finish baseline, so it cannot tell a lastRefresh this "+
				"finish wrote from one that was already there", path)
		case finishAt < 0:
			t.Errorf("%s no longer posts the finish", path)
		case baselineAt > finishAt:
			t.Errorf("%s reads its baseline AFTER starting the finish — the finish may have "+
				"stamped lastRefresh by then, and the comparison silently stops meaning anything", path)
		}

		// Bracketed to the abort handler, not the file. settings.js already
		// calls loadStatus() on the SUCCESS path, so a file-wide search would
		// pass against the unfixed code and prove nothing about this branch.
		const abortArm = `if (e.name === "AbortError") {`
		at := strings.Index(js, abortArm)
		if at < 0 {
			t.Errorf("%s no longer has an abort arm to report from", path)
			continue
		}
		body := js[at+len(abortArm):]
		if end := strings.Index(body, "\n        return;"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "await cookieSetupProbe()") {
			t.Errorf("%s does not ask the server what actually happened before reporting", path)
		}
		if !strings.Contains(body, "await abortBaseline") {
			t.Errorf("%s never reads the baseline it took, so the \"already finished\" arm is "+
				"back to guessing", path)
		}
		if !strings.Contains(body, "this.app.loadStatus()") {
			t.Errorf("%s does not refresh the status after an abort that may have completed", path)
		}
	}

	// Only the wizard can be left with unlit badges, so only the wizard carries
	// the sentence that says so — cookieYTDone/cookieTWDone are the sole source
	// of the active_platforms it is about to write.
	if !strings.Contains(readEmbeddedModule(t, "public/modules/setup.js"), "{ wizard: true }") {
		t.Error("the setup wizard's abort alert claims a save without saying it cannot record " +
			"which platform — the config it then writes would list no active platform")
	}
	if strings.Contains(readEmbeddedModule(t, "public/modules/settings.js"), "{ wizard: true }") {
		t.Error("the Settings panel is not the wizard and has no badges to disclaim")
	}

	utils := readEmbeddedModule(t, "public/modules/utils.js")
	probe := jsFunctionBody(t, utils, "cookieSetupProbe")
	if !strings.Contains(probe, "/api/cookies/auto-status") {
		t.Error("the abort probe does not hit the endpoint that knows what became of the setup")
	}
	// The probe runs BECAUSE the server did not answer in 60 s. Unbounded, a
	// wedged server leaves the user with no alert, no buttons and a countdown
	// still on screen, all behind the await.
	if !strings.Contains(probe, "controller.abort()") || !strings.Contains(probe, "COOKIE_SETUP_PROBE_TIMEOUT_MS") {
		t.Error("the abort probe is unbounded — the one situation it exists for is a server that " +
			"is not answering, and it would strand the user there with nothing rendered")
	}
	if !strings.Contains(probe, "clearTimeout") {
		t.Error("the abort probe leaks its timeout timer")
	}
	// Asserted as READS OFF THE RESPONSE, not as bare field names. A Contains
	// on "lastRefresh" alone survives `lastRefresh: null`, which is precisely
	// what discarding the field looks like.
	for _, read := range []string{"status.setupInProgress", "status.lastError", "status.lastRefresh"} {
		if !strings.Contains(probe, read) {
			t.Errorf("the probe never reads %s. Without it a freed setup slot is one undivided "+
				"outcome again, and the report goes back to guessing which one", read)
		}
	}

	report := jsFunctionBody(t, utils, "cookieSetupAbortReport")
	// Four answers, and the guards are pinned in their EXACT form rather than
	// by field name: a Contains on "probe.lastError" stays green against
	// `if (false && probe.lastError)`, which is what a neutered arm looks like.
	//
	// The limit is real and this test does not pretend otherwise — text cannot
	// prove a branch body still renders anything. It can prove the condition
	// was not quietly disarmed, which is the drift that actually happens.
	for _, guard := range []string{
		"if (!probe.ok) {",
		"if (probe.inProgress) {",
		"if (probe.lastError) {",
		"if (cookieSetupRefreshAdvanced(baseline, probe)) {",
	} {
		if !strings.Contains(report, guard) {
			t.Errorf("the abort report no longer guards on %q — a freed setup slot is not one "+
				"outcome, and collapsing them reintroduces S17's own unearned claim", guard)
		}
	}

	advanced := jsFunctionBody(t, utils, "cookieSetupRefreshAdvanced")
	if !strings.Contains(advanced, "probe.lastRefresh !== baseline.lastRefresh") {
		t.Error("the save verdict no longer compares the two stamps, so it cannot tell a stamp " +
			"this finish wrote from one that was already on disk")
	}
	if !strings.Contains(advanced, "!baseline.ok") {
		t.Error("a baseline that could not be read is being treated as \"there was no stamp " +
			"before\" — the server may have carried one all along, and the wrong answer here " +
			"tells the user their cookies were saved when nothing was")
	}
	// Asserted as an ORDERING: the recorded-error arm has to be reached before
	// anything claims a save, because the 422 that refuses to write cookies.txt
	// frees the slot exactly like a success does.
	errAt := strings.Index(report, "probe.lastError")
	savedAt := strings.Index(report, "were saved")
	switch {
	case savedAt < 0:
		t.Error("the abort report no longer has a save arm to order against")
	case errAt < 0 || errAt > savedAt:
		t.Error("the abort report claims the cookies were saved before it checks whether the " +
			"server recorded an error — that is the 422 case, where nothing was written at all")
	}
}
