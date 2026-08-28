package routes

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// TestCookieRefreshOutcomeSeparatesDeclinedFromFailed pins the wire fields the
// manual-refresh toast branches on.
//
// The defect this guards: `success` alone cannot distinguish "the credentials
// were checked and rejected" from "no check happened". The single refresh slot
// is held by the 30-minute periodic tick and by interactive setup, so clicking
// "Refresh now" during either returns refreshDeclined() — a pass that looked at
// nothing — and the toast read "auth verification failed" in the very same
// payload whose cookieStatus reported the session authenticated.
//
// `ran` and `verdict` are additive: `success` keeps its exact old meaning, so
// an older frontend against a newer binary behaves as it did. That is the same
// precedent `renewed` set.
//
// The "conclusively unauthenticated" row is the premise for the others. Without
// it, a payload that simply never reported a failure would satisfy every
// assertion here by saying nothing at all.
func TestCookieRefreshOutcomeSeparatesDeclinedFromFailed(t *testing.T) {
	tests := []struct {
		name        string
		result      cookies.RefreshResult
		wantSuccess bool
		wantRan     bool
		wantVerdict string
	}{
		{
			// refreshDeclined() is the zero value, by construction.
			name:        "declined — the slot was already held",
			result:      cookies.RefreshResult{},
			wantSuccess: false,
			wantRan:     false,
			wantVerdict: "unknown",
		},
		{
			// refreshAborted()'s shape, and every pass whose verification
			// could not reach the service.
			name:        "ran but learned nothing",
			result:      cookies.RefreshResult{Ran: true},
			wantSuccess: false,
			wantRan:     true,
			wantVerdict: "unknown",
		},
		{
			name:        "conclusively unauthenticated",
			result:      cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true},
			wantSuccess: false,
			wantRan:     true,
			wantVerdict: "failed",
		},
		{
			// One healthy platform is enough for the whole-service verdict:
			// authenticated work is possible. Unchanged from before.
			name:        "one platform verified",
			result:      cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed},
			wantSuccess: true,
			wantRan:     true,
			wantVerdict: "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cookieRefreshOutcome(tt.result)
			if got["success"] != tt.wantSuccess {
				t.Errorf("success = %v, want %v", got["success"], tt.wantSuccess)
			}
			if got["ran"] != tt.wantRan {
				t.Errorf("ran = %v, want %v — the toast cannot tell a declined pass "+
					"from a failed one without it", got["ran"], tt.wantRan)
			}
			if got["verdict"] != tt.wantVerdict {
				t.Errorf("verdict = %v, want %q", got["verdict"], tt.wantVerdict)
			}
		})
	}
}

// TestCookieRefreshOutcomeKeepsRenewedIndependent guards the field that is a
// fact about the MECHANISM against being folded into the verdicts, which are
// facts about the CREDENTIALS. A pass can verify both platforms while renewing
// nothing (a browser that never ran, or any launch on a platform with no Job
// Object to drain).
func TestCookieRefreshOutcomeKeepsRenewedIndependent(t *testing.T) {
	verifiedNotRenewed := cookies.RefreshResult{Ran: true, YouTube: cookies.RefreshOK, Renewed: false}
	got := cookieRefreshOutcome(verifiedNotRenewed)
	if got["success"] != true || got["verdict"] != "ok" {
		t.Errorf("a verified pass stopped reading as verified because it renewed nothing: %v", got)
	}
	if got["renewed"] != false {
		t.Errorf("renewed = %v, want false", got["renewed"])
	}
}

// TestAppJSReadsTheFieldsTheHandlerEmits pins the one seam nothing else can
// see: the Web toast's branch conditions are JavaScript, and no JS harness
// exists in-tree.
//
// The realistic drift is a Go-side rename leaving app.js reading `undefined`,
// which is worse than a crash — `undefined === false` is false, so a renamed
// `ran` silently stops the declined arm from ever firing and the toast falls
// back to claiming the cookies could not be established. Reading the field
// names out of cookieRefreshOutcome's own output means a rename fails here
// rather than in the field.
//
// app.js is embedded straight from web/public with no build step, so the source
// and the asset cannot disagree; the only thing at risk is the contract, and
// that is exactly what this reads.
//
// Strict equality is load-bearing and asserted verbatim. It is what makes the
// additive claim hold: against an older binary that emits neither field,
// `undefined === false` and `undefined === "failed"` are both false, so the
// toast degrades to the hedged arm and never to the danger one.
func TestAppJSReadsTheFieldsTheHandlerEmits(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	js := string(raw)

	payload := cookieRefreshOutcome(cookies.RefreshResult{})
	for _, tc := range []struct{ key, expr string }{
		{"ran", "data.ran === false"},
		{"verdict", `data.verdict === "failed"`},
	} {
		if _, ok := payload[tc.key]; !ok {
			t.Fatalf("cookieRefreshOutcome no longer emits %q, but app.js still reads data.%s — "+
				"the toast would compare against undefined", tc.key, tc.key)
		}
		if !strings.Contains(js, tc.expr) {
			t.Errorf("app.js does not contain %q — the Web toast can no longer tell a declined "+
				"pass from a conclusively failed one", tc.expr)
		}
	}

	// success is the legacy field every non-success branch is still gated on;
	// losing it would make all four hedged arms unreachable.
	if !strings.Contains(js, "!data.success") {
		t.Error("app.js no longer gates the refresh toast on data.success")
	}
}

// TestAppJSMatchesTheDeclinedCauses keeps the Web copy of the declined-cause
// list in step with the Go one. The two Go surfaces share
// cookies.RefreshDeclinedCauses; app.js cannot import it, and the phrasing had
// already drifted once ("cookies to refresh" against "cookies worth
// refreshing") with nothing to catch it.
func TestAppJSMatchesTheDeclinedCauses(t *testing.T) {
	raw, err := webassets.PublicFS.ReadFile("public/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	if !strings.Contains(string(raw), cookies.RefreshDeclinedCauses) {
		t.Errorf("app.js does not carry the shared declined-cause list verbatim:\n\t%q\n"+
			"Three surfaces render one exhaustive set; a fourth phrasing means one of them "+
			"is naming a different set of causes.", cookies.RefreshDeclinedCauses)
	}
}
