package routes

import (
	"encoding/json"
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

// jsFunctionBody slices one function out of a module so an assertion can name
// the SITE it is about.
//
// A file-wide strings.Contains cannot, and repeatedly did not: the same literal
// appears in sibling helpers, so changing one of them leaves a whole-file check
// green. That junction defect was found three rounds running in this file.
//
// Bracketing is the mitigation for the source-shape assertions that remain
// (which URL the probe calls, which module passes the wizard flag). It is not
// the mitigation for assertions about BEHAVIOUR — those moved to
// cookies_setup_utilsvm_test.go, which runs the module instead.
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

// jsCallArgs returns the top-level arguments of the first call to fn in src.
//
// Written because bracketing was NOT enough. The reviewer's mutation dropped
// `{ wizard: true }` from the call and left the literal in a trailing comment
// on the same line — inside the bracketed slice — and a Contains check passed.
// Narrowing the window does not help when the decoy sits inside the window; the
// fix is to stop asking whether text appears and start asking what the call
// actually passes.
//
// Scans from the call token to its matching close paren, tracking nesting and
// string literals, so a comment cannot contribute an argument. Returns nil when
// there is no such call.
func jsCallArgs(src, fn string) []string {
	at := strings.Index(src, fn+"(")
	if at < 0 {
		return nil
	}
	rest := src[at+len(fn)+1:]

	var args []string
	var current strings.Builder
	depth := 0
	var quote rune
	for i, r := range rest {
		if quote != 0 {
			current.WriteRune(r)
			if r == quote && (i == 0 || rest[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
			current.WriteRune(r)
		case '(', '{', '[':
			depth++
			current.WriteRune(r)
		case ')', '}', ']':
			if r == ')' && depth == 0 {
				args = append(args, strings.TrimSpace(current.String()))
				return args
			}
			depth--
			current.WriteRune(r)
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(current.String()))
				current.Reset()
				continue
			}
			current.WriteRune(r)
		default:
			current.WriteRune(r)
		}
	}
	return nil
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

// TestSetupDialogsReadTheVerificationFieldsTheHandlerEmits pins the Go↔JS seam,
// in the shape TestAppJSReadsTheFieldsTheHandlerEmits established for the
// refresh toast.
//
// The realistic drift is a Go-side rename leaving the modules reading
// `undefined`, which is worse than a crash: `undefined === "unknown"` is false,
// so a renamed field silently stops the hedged arm from ever firing and every
// setup goes back to claiming an unqualified success.
//
// SCOPE, since it narrowed: this test now asserts only that the two names line
// up across the seam — the handler emits them and both dialogs read them. What
// the copy DOES with the values, including the backward-compat property that
// the comparison runs positively (`=== "unknown"`, never `!== "ok"`), is
// executed in cookies_setup_utilsvm_test.go rather than matched here. It was a
// string prohibition until the review demonstrated that inverting one of the
// two helpers left it green.
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

	// What the copy helpers DO with those fields — including the whole
	// backward-compat property, which used to be a prohibition on the string
	// `verification !==` — is executed in
	// TestCookieSetupCopySpeaksFromTheVerificationField. A source match could
	// not distinguish an inverted comparison in one helper from an inverted one
	// in the other, and did not.
	//
	// One source assertion remains here because it is about VOCABULARY rather
	// than behaviour: the three states must be worded in the phrasing
	// cookies.RefreshVerdict.String() already established across the
	// manual-refresh surfaces, not in a fourth one. Running the helper cannot
	// tell a correct fourth phrasing from a correct reuse.
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	if !strings.Contains(utils, "could not establish") {
		t.Error("utils.js words the inconclusive setup outcome in something other than the " +
			"\"could not establish\" wording the manual-refresh surfaces already use")
	}
}

// TestAutoStatusEmitsTheFieldsTheAbortProbeReads is the Go half of the abort
// probe's contract, and it is the half that was missing.
//
// The probe's three discriminators were pinned only on the JavaScript side, so
// a Go-side JSON tag rename — `lastError` to `last_error`, say — would leave
// the probe reading undefined and the report silently falling to its hedged
// arm forever. Safe direction, but a permanent one, and nothing would fail.
//
// Marshalled rather than reflected over, because the tag is only half the
// story: a field that stops being serialised at all (omitempty on a nil
// pointer, an embedded rename) reaches the browser the same way.
func TestAutoStatusEmitsTheFieldsTheAbortProbeReads(t *testing.T) {
	raw, err := json.Marshal(cookies.AutoCookieStatus{})
	if err != nil {
		t.Fatalf("marshal AutoCookieStatus: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal AutoCookieStatus: %v", err)
	}

	for _, field := range []string{"setupInProgress", "lastError", "lastRefresh"} {
		if _, ok := wire[field]; !ok {
			t.Errorf("/api/cookies/auto-status no longer emits %q, but cookieSetupProbe reads "+
				"status.%s — the abort report would fall to its hedged arm for every user, "+
				"permanently and silently", field, field)
		}
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

		// Asserted on the CALL, not on the text. Two earlier attempts at this
		// failed: file-wide, and then bracketed-but-still-Contains, which the
		// review defeated by leaving the literal in a trailing comment inside
		// the bracket. jsCallArgs parses to the matching paren, so only a real
		// argument counts.
		//
		// Only the wizard has badges to disclaim, so the option is required in
		// one module and prohibited in the other. What it DOES to the copy is
		// executed, not matched — see
		// TestCookieSetupAbortReportNeverClaimsAnUnknownSave, which renders both.
		args := jsCallArgs(body, "cookieSetupAbortReport")
		if len(args) < 2 {
			t.Errorf("%s's abort arm does not call cookieSetupAbortReport(probe, baseline): %v", path, args)
			continue
		}
		wantsWizard := path == "public/modules/setup.js"
		hasWizard := len(args) >= 3 && strings.Contains(args[2], "wizard")
		switch {
		case wantsWizard && !hasWizard:
			t.Errorf("%s's abort arm passes no wizard option, so its save arm claims a save "+
				"without saying it cannot record WHICH platform — cookieYTDone/cookieTWDone are "+
				"the sole source of the active_platforms the wizard then writes, and this path "+
				"lights neither. Args: %v", path, args)
		case !wantsWizard && hasWizard:
			t.Errorf("%s is not the wizard and has no badges to disclaim. Args: %v", path, args)
		}
	}

	// The endpoint is source shape: which URL the probe calls cannot be
	// observed from its return value, and it is the whole point of the fix.
	// Everything else about the probe and the report — the fields it carries,
	// every arm it renders, and its promise never rejecting — is executed in
	// cookies_setup_utilsvm_test.go. Those assertions used to live here as
	// string matches, and three of them were shown not to hold.
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	probe := jsFunctionBody(t, utils, "cookieSetupProbe")
	if !strings.Contains(probe, "/api/cookies/auto-status") {
		t.Error("the abort probe does not hit the endpoint that knows what became of the setup")
	}
	// Also source shape, and deliberately so: proving the BOUND by execution
	// would need a host event loop with a real clock, which this harness does
	// not have (its setTimeout never fires). The bound was measured directly
	// instead — 5007 ms against a stub that never answers — and this keeps the
	// mechanism from being deleted afterwards.
	if !strings.Contains(probe, "controller.abort()") || !strings.Contains(probe, "COOKIE_SETUP_PROBE_TIMEOUT_MS") {
		t.Error("the abort probe is unbounded — the one situation it exists for is a server that " +
			"is not answering, and it would strand the user there with nothing rendered")
	}
	if !strings.Contains(probe, "clearTimeout") {
		t.Error("the abort probe leaks its timeout timer")
	}

	// The stamp comparison, the unreadable-baseline rule and the precedence of
	// the recorded-error arm over the save arm are all executed now. The last
	// of those used to be an index comparison between "probe.lastError" and
	// "were saved" in this source, which any earlier textual mention of the
	// field defeated; the behaviour test renders a probe carrying BOTH and
	// asserts which sentence comes out.
}
