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

func readEmbeddedModule(t *testing.T, path string) string {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return string(raw)
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
	// apart, and the strict comparison lives with it.
	utils := readEmbeddedModule(t, "public/modules/utils.js")
	for _, expr := range []string{`verification === "unknown"`, `verification === "failed"`} {
		if !strings.Contains(utils, expr) {
			t.Errorf("utils.js does not contain %q — a loose comparison would make an older "+
				"binary's missing field render as the hedged arm", expr)
		}
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
// is still held, which is the fact this side was guessing at.
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
		if !strings.Contains(body, "await cookieSetupStillRunning()") {
			t.Errorf("%s does not ask the server what actually happened before reporting", path)
		}
		if !strings.Contains(body, "this.app.loadStatus()") {
			t.Errorf("%s does not refresh the status after an abort that may have completed", path)
		}
	}

	utils := readEmbeddedModule(t, "public/modules/utils.js")
	if !strings.Contains(utils, "/api/cookies/auto-status") {
		t.Error("the abort probe does not hit the endpoint that knows whether the setup slot is still held")
	}
	// Three answers, not two: "we could not find out" must stay distinguishable
	// from "the setup finished", or the probe reintroduces the same unearned
	// claim one level down.
	if !strings.Contains(utils, "stillRunning === false") || !strings.Contains(utils, "stillRunning === true") {
		t.Error("the abort report collapses the probe's three answers — a probe that could not " +
			"reach the server must not be rendered as a finished setup")
	}
}
