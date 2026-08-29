package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// decodeErrorBody reads a jsonError / jsonErrorCause body.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %q", rec.Body.String())
	}
	return body
}

// TestBrowserReadErrorsReachTheWireWithACause is item (i) of Arc 8 Task 12a.
//
// Both browser-read failures used to fall to the setup handler's default arm —
// `500 {"error": "failed to finish setup"}` — whose own comment in this file
// said it gives no hint. They are two states an operator can act on and one
// they cannot, flattened into the sentence that describes the last of those.
//
// Driven through writeBrowserReadError with a real ResponseRecorder, which is
// the whole of what both handlers do with these errors: the handler line is
// `if writeBrowserReadError(rw, err) { return }`. The service-level route into
// them cannot be driven from this package — reaching FinishSetupDetailed's
// browser read needs a live setup slot with a Chromium in it, and the fields
// that hold one are unexported — so the mapping is asserted where it is
// decided, and the two call sites are one line each.
//
// The producer half lives in internal/cookies: TestCdpCookieReadOutcome asserts
// that each read outcome wraps exactly one of these sentinels and never two.
func TestBrowserReadErrorsReachTheWireWithACause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantCode  int
		wantCause string
	}{
		{
			// A CONDITION on the machine — something holding or intercepting
			// the debugging port — so 409, the same answer a locked cookie DB
			// gets: change it and retry.
			name:      "blocked ladder",
			err:       fmt.Errorf("%w: connection refused", cookies.ErrBrowserLadderBlocked),
			wantCode:  http.StatusConflict,
			wantCause: causeBrowserLadderBlocked,
		},
		{
			// The browser side produced nothing at all. The failure is upstream
			// of this server, which is what separates it from a 500.
			name:      "unanswered read",
			err:       fmt.Errorf("%w: Storage.getCookies is not available", cookies.ErrBrowserReadUnanswered),
			wantCode:  http.StatusBadGateway,
			wantCause: causeBrowserReadUnanswered,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !writeBrowserReadError(rec, tc.err) {
				t.Fatal("writeBrowserReadError declined a sentinel it owns — the handler would fall " +
					"through to its default arm and the cause would be lost")
			}
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			body := decodeErrorBody(t, rec)
			if body["cause"] != tc.wantCause {
				t.Errorf("cause = %q, want %q — the frontend branches on this token, and it is "+
					"deliberately not the message, which will be reworded", body["cause"], tc.wantCause)
			}
			// VERBATIM, like the profile-import failures beside it.
			// cdpCookieReadOutcome composes the only description of what
			// actually stopped the read; substituting a static sentence here is
			// what the default arm already does.
			if body["error"] != tc.err.Error() {
				t.Errorf("error = %q, want the message verbatim: %q", body["error"], tc.err.Error())
			}
		})
	}

	// THE PREMISE. Two causes that rendered the same string would satisfy every
	// row above while giving the frontend nothing to branch on.
	if causeBrowserLadderBlocked == causeBrowserReadUnanswered {
		t.Fatal("premise lost: the two causes are the same token, so the distinction the sentinels " +
			"were split for does not survive onto the wire")
	}
}

// TestUnownedErrorsFallThroughToTheDefaultArm is the other half of item (i):
// an error this helper does not own must reach the handler's own switch
// untouched, and an unrecognised one must still produce the 500.
//
// ErrNoCookiesInProfile is in the table for a specific reason rather than for
// coverage. It is the SIBLING outcome of the same function — "the browser
// answered and the profile holds no YouTube/Twitch cookies" — and it is a
// verdict, which these two are not: FinishSetup turns it into "no login
// detected" and returns no error at all. Claiming it here would answer 409 or
// 502 for a user who simply never signed in.
func TestUnownedErrorsFallThroughToTheDefaultArm(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the empty-profile verdict, which is not a read failure", cookies.ErrNoCookiesInProfile},
		{"a wrapped empty-profile verdict", fmt.Errorf("finish setup: %w", cookies.ErrNoCookiesInProfile)},
		{"an unreadable cookies.txt", cookies.ErrCookieFileUnreadable},
		{"no setup in progress", cookies.ErrNoSetupInProgress},
		{"something with no sentinel at all", errors.New("disk on fire")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if writeBrowserReadError(rec, tc.err) {
				t.Fatalf("writeBrowserReadError claimed %v. Every arm below it in both handlers is "+
					"then unreachable for that error, and the operator gets a browser-read remedy "+
					"for a problem that is not one", tc.err)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("it wrote %q while declining — the handler writes its own answer next, so "+
					"this is two responses on one request", rec.Body.String())
			}
		})
	}
}

// TestFinishSetupStillAnswersItsOtherArms drives the real HTTP handler to show
// the new pre-switch check did not swallow the arms below it. ErrNoSetupInProgress
// is the one failure this package can provoke through the real service, and it
// is the arm immediately after the new check.
func TestFinishSetupStillAnswersItsOtherArms(t *testing.T) {
	refreshSvc := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopRouteLogger{})
	autoSvc := cookies.NewAutoCookieService(t.TempDir(), "", cookies.NewCookieJar(), nopRouteLogger{})

	r := chi.NewRouter()
	CookieRoutes(r, refreshSvc, autoSvc, nil, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-setup/finish", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("finish with no setup in progress: status %d, want 404, body %s",
			rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if _, hasCause := body["cause"]; hasCause {
		t.Errorf("a non-browser-read failure carried a cause key: %v. `cause` names one of the two "+
			"browser-read sentinels; emitting it elsewhere teaches the frontend a token that "+
			"means nothing", body)
	}
	if !strings.Contains(body["error"], "no cookie auto-setup in progress") {
		t.Errorf("error = %q, want the ErrNoSetupInProgress message — the arm below the new "+
			"browser-read check must still be reached", body["error"])
	}
}

// TestAuthStatusPayloadsCarryTheInconclusiveReason is item (v).
//
// AuthStatus.YouTubeError / TwitchError carry WHY a check could not conclude,
// and had no reader anywhere in the tree: the UI could say "could not check"
// and never say what stopped it, so a rate limit, a captive portal and an
// intercepting proxy all rendered identically and none of them named the thing
// to fix.
//
// Constructed directly rather than driven through a RefreshService, because
// what is under test is the PROJECTION — that these two fields reach the wire
// beside `verification`, and that a conclusive check sends an empty string
// rather than a stale one. The separate question, that no producer can ever put
// a response body in them, is pinned at the producers by
// TestAuthStatusErrorsCarryNoResponseBody in internal/cookies; it cannot be
// asserted here, because this projection is a pass-through by design and an
// `<html>` planted in the struct would arrive faithfully.
func TestAuthStatusPayloadsCarryTheInconclusiveReason(t *testing.T) {
	const ytReason = "youtube auth check: unexpected status 429"
	const twReason = "twitch auth check: unexpected status 503"

	t.Run("inconclusive — the reason is carried", func(t *testing.T) {
		status := cookies.AuthStatus{
			HasYouTubeCookies:   true,
			HasTwitchCookies:    true,
			YouTubeVerification: cookies.RefreshUnknown,
			TwitchVerification:  cookies.RefreshUnknown,
			YouTubeError:        ytReason,
			TwitchError:         twReason,
		}
		yt := CookieStatusPayload(status)
		tw := TwitchAuthStatusPayload(status)

		if yt["youtubeError"] != ytReason {
			t.Errorf("youtubeError = %v, want %q", yt["youtubeError"], ytReason)
		}
		if tw["twitchError"] != twReason {
			t.Errorf("twitchError = %v, want %q", tw["twitchError"], twReason)
		}
		// The reason is ADDITIVE: it stands beside the verdict, it does not
		// replace it. A frontend that has not been taught the key must keep
		// behaving exactly as it did.
		if yt["verification"] != "unknown" || tw["verification"] != "unknown" {
			t.Errorf("the verdicts moved: youtube=%v twitch=%v", yt["verification"], tw["verification"])
		}
		if yt["authenticated"] != false || tw["authenticated"] != false {
			t.Errorf("`authenticated` moved: youtube=%v twitch=%v — its meaning is unchanged and "+
				"it is the only key a pre-existing frontend reads",
				yt["authenticated"], tw["authenticated"])
		}
	})

	t.Run("conclusive — no reason to give", func(t *testing.T) {
		// A check that CONCLUDED has no reason, and must not send one. Carrying
		// a leftover string alongside "ok" would put an explanation on screen
		// for a state that has nothing to explain.
		status := cookies.AuthStatus{
			YouTubeAuthenticated: true,
			HasYouTubeCookies:    true,
			HasTwitchCookies:     true,
			YouTubeVerification:  cookies.RefreshOK,
			TwitchVerification:   cookies.RefreshFailed,
		}
		if got := CookieStatusPayload(status)["youtubeError"]; got != "" {
			t.Errorf("youtubeError = %v for a conclusive check, want empty", got)
		}
		if got := TwitchAuthStatusPayload(status)["twitchError"]; got != "" {
			t.Errorf("twitchError = %v for a conclusive check, want empty", got)
		}
	})

	// THE PREMISE for both rows: the two projections must not be reading the
	// same field. YouTube's payload carrying Twitch's reason would satisfy the
	// first row above whenever the fixture happened to set both.
	crossed := cookies.AuthStatus{YouTubeError: ytReason}
	if got := TwitchAuthStatusPayload(crossed)["twitchError"]; got != "" {
		t.Errorf("twitchError = %v, want empty — it is reading AuthStatus.YouTubeError", got)
	}
}
