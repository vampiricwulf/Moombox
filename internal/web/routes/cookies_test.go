package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
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
