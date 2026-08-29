package cookies

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The guide reply bodies these tests are built on.
//
// loggedOutRealShapeBody is the shape MEASURED anonymously against
// https://www.youtube.com/youtubei/v1/guide?prettyPrint=false on 2026-08-27,
// reduced to the two fields that carry the verdict. Both matter: the tracking
// param's value is the STRING "0" (not a JSON false), and the
// mainAppWebResponseContext flag arrives as loggedOut:true with no loggedIn key
// at all. The in-tree fixtures that predate this measurement all use
// `loggedIn:false`, which YouTube does not appear to emit — they are kept
// (a present-and-false flag is still an unambiguous negative) but they are not
// evidence about the wire, so this constant exists alongside them.
const loggedOutRealShapeBody = `{"responseContext":{"serviceTrackingParams":[` +
	`{"service":"GFEEDBACK","params":[{"key":"logged_in","value":"0"},{"key":"visitor_data","value":"redacted"}]},` +
	`{"service":"GUIDED_HELP","params":[{"key":"logged_in","value":"0"}]}],` +
	`"mainAppWebResponseContext":{"loggedOut":true,"trackingParam":"redacted"}}}`

// captivePortalBody is the trigger this whole change exists for: a transparent,
// NON-redirecting intermediary answering our POST with HTML. It is served with
// a 200, from the host we asked, with our Cookie header intact — so it clears
// the status check and clears authResponseIsOurs — and it carries no login
// marker in any form.
const captivePortalBody = `<!doctype html><html><head><title>Network sign-in required</title></head>` +
	`<body><h1>Please authenticate to use this network</h1><form action="/portal"></form></body></html>`

// unmarkedJSONBody is valid JSON with a plausible responseContext that simply
// carries no login marker — one upstream serialisation change away from being
// every install's reply at once. It exercises the JSON path, where
// captivePortalBody exercises the string fallback.
const unmarkedJSONBody = `{"responseContext":{"serviceTrackingParams":[` +
	`{"service":"GFEEDBACK","params":[{"key":"visitor_data","value":"redacted"}]}],` +
	`"mainAppWebResponseContext":{"trackingParam":"redacted"}}}`

// bodyServer serves one fixed 200 body, plus any Set-Cookie headers given.
func bodyServer(t *testing.T, body string, setCookies ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, sc := range setCookies {
			w.Header().Add("Set-Cookie", sc)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- The decisive case: an unrecognisable 200 body is INCONCLUSIVE ---

// TestGuide200WithNoLoginMarkerIsInconclusive is the finding.
//
// Before this change all three body exits ended in `(false, nil)` — the JSON
// parse failing with no positive needle, no logged_in="1" among the tracking
// params, and loggedIn not true. `(false, nil)` is the one answer
// shouldFireRecovery acts on, so a captive portal or corporate proxy that
// answers 200 with HTML produced a CONCLUSIVE "your cookies are dead" about
// cookies that were fine, and paged the operator to re-export them. In a
// container the remedy that alarm names may not even be reachable.
//
// A conclusive negative now requires an explicit negative marker. Everything
// else is an error: we asked, we got an answer, we could not read it.
//
// Both entry points and both body-reader paths are covered. That doubling was
// written when checkYouTubeAuth and checkAndRefreshYouTube were near-duplicate
// copies and a rule applied to one of them was a rule with a hole in it. The
// copies are gone — both now call youtubeGuideExchange — and the doubling is
// kept for what it proves now: that each entry point still REACHES the shared
// exchange and returns its verdict unaltered. A wrapper that swallowed the
// error, or grew a second body reader of its own, would pass on one side and
// fail here.
func TestGuide200WithNoLoginMarkerIsInconclusive(t *testing.T) {
	bodies := map[string]string{
		"captive_portal_html": captivePortalBody, // string-fallback path
		"json_without_marker": unmarkedJSONBody,  // JSON path
		"empty":               "",
	}
	entries := map[string]func(*RefreshService, context.Context) (bool, error){
		"checkAndRefreshYouTube": (*RefreshService).checkAndRefreshYouTube,
		"checkYouTubeAuth":       (*RefreshService).checkYouTubeAuth,
	}

	for bodyName, body := range bodies {
		for entryName, entry := range entries {
			t.Run(bodyName+"/"+entryName, func(t *testing.T) {
				pointYouTubeGuideAt(t, bodyServer(t, body))
				rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

				auth, err := entry(rs, context.Background())

				if auth {
					t.Error("authenticated = true, want false")
				}
				if err == nil {
					t.Fatal("err = nil, want non-nil — a 200 carrying no login marker is not a verdict on our session")
				}
				if !errors.Is(err, errGuideLoginMarkerUnreadable) {
					t.Errorf("err = %v, want errGuideLoginMarkerUnreadable", err)
				}
				// ErrAuthCheckNotAttempted means the question could never be
				// FORMED. Here it was asked and answered, so autocookies'
				// `attempted` flag must stay true.
				if errors.Is(err, ErrAuthCheckNotAttempted) {
					t.Errorf("err wraps ErrAuthCheckNotAttempted; a request did leave the process: %v", err)
				}
			})
		}
	}
}

// TestUnreadableGuideErrorCarriesNoBody: the unreadable body is the SUBJECT of
// this error and must never become its content — a portal page can echo back a
// request, and the request carries the session.
//
// NOT because the string reaches the Web UI and TUI status surfaces. It does
// not; AuthStatus.YouTubeError has no reader. An earlier version of this comment
// said otherwise, and the same false claim stood in the sentinel's own doc
// comment — see errGuideLoginMarkerUnreadable in refresh.go for where the string
// actually goes and for the five log surfaces it fans out to once the operator
// raises the level to DEBUG. Those are the reason this test exists.
//
// Same rule as TestTwitchValidateErrorNamesOnlyTheStatus and the provenance
// errors, which name a status, a host or a header NAME and nothing else.
func TestUnreadableGuideErrorCarriesNoBody(t *testing.T) {
	const secretish = "sapisid-value"
	body := captivePortalBody + "<!-- echoed request: Cookie: SAPISID=" + secretish + " -->"
	pointYouTubeGuideAt(t, bodyServer(t, body))

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	_, err := rs.checkAndRefreshYouTube(context.Background())
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	msg := err.Error()
	for _, leak := range []string{secretish, "login-info-value", "Network sign-in", "doctype", "<html"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the error carries response body material (%q): %q", leak, msg)
		}
	}
	// It must read as "could not tell", never as a failed credential.
	if !strings.Contains(msg, "learned nothing") {
		t.Errorf("error does not read as inconclusive: %q", msg)
	}
}

// rotatedSetCookie is ONE fixture used by both halves of the merge test below.
// It has to be one, or the negative half proves nothing: a Set-Cookie the merge
// path would have discarded anyway (wrong domain, tripped by the substring
// pre-filter, rejected by isYouTubeDomain) would produce an identical "not in
// the jar" result no matter what the verdict logic did.
const rotatedSetCookie = "SAPISID=rotated-by-the-server; Domain=.youtube.com; Path=/"

// TestGuideReplySetCookieMergeFollowsTheVerdict: checkAndRefreshYouTube is also
// the Set-Cookie merge path, and the merge must follow the verdict.
//
// The two halves are inseparable, which is why they share a fixture and a test.
// The negative alone would be the junction pattern this subsystem keeps getting
// caught by: "the value is not in the jar" is satisfied by several mechanisms —
// the domain filter, the atomic write failing, the fixture never having been
// mergeable — and only one of them is the rule being pinned. The positive
// control is what makes the negative mean anything: same fixture, same harness,
// same server, and it DOES land. So when the negative holds, the verdict is the
// only thing that can have stopped it.
//
// Note what the negative half is and is not. It is NOT a regression test for
// F1 — pre-change an unreadable body produced authenticated=false and the
// pre-existing `if authenticated` guard already skipped the merge, so this
// assertion held before too. It is a forward guard: it fails the day someone
// moves the merge ahead of the verdict, which is the shape of mistake that put
// a redirected reply's cookies within reach of the jar in the first place.
func TestGuideReplySetCookieMergeFollowsTheVerdict(t *testing.T) {
	// Positive control: an authenticated reply, so the merge is supposed to
	// run. If this fails the negative below is vacuous.
	t.Run("authenticated_reply_merges", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t,
			`{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":"1"}]}]}}`,
			rotatedSetCookie))

		jar := jarWithAuth(t)
		rs := NewRefreshService(jar, 0, nopLogger{})
		auth, err := rs.checkAndRefreshYouTube(context.Background())
		if err != nil || !auth {
			t.Fatalf("premise broken: auth=%v err=%v, want true/nil", auth, err)
		}

		if header := jar.GetCookieHeader(); !strings.Contains(header, "rotated-by-the-server") {
			t.Fatal("the merge path did not take this fixture even on an authenticated reply — " +
				"the negative case below would prove nothing")
		}
	})

	// The guard: a reply we could not read is not a reply we may write the jar
	// from. We do not know whose session it describes — the same reason the
	// provenance guard bails before the merge.
	t.Run("unreadable_reply_does_not_merge", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t, captivePortalBody, rotatedSetCookie))

		jar := jarWithAuth(t)
		rs := NewRefreshService(jar, 0, nopLogger{})
		if _, err := rs.checkAndRefreshYouTube(context.Background()); err == nil {
			t.Fatal("premise broken: err = nil, want the inconclusive error")
		}

		if header := jar.GetCookieHeader(); strings.Contains(header, "rotated-by-the-server") {
			t.Error("the jar took a Set-Cookie from a reply we could not read")
		}
	})
}

// --- The two controls: the fix must not be a blanket widening ---

// TestGuide200ExplicitPositiveStillAuthenticates is control one. Without it,
// "everything is inconclusive now" would pass the test above.
//
// Every accepted positive form is exercised, including the two that predate the
// measurement. The rule is that accepted positives only ever GROW: a reply that
// authenticated before must still authenticate, or an authenticated session
// loses its verdict and the fix has traded one false alarm for another.
func TestGuide200ExplicitPositiveStillAuthenticates(t *testing.T) {
	bodies := map[string]string{
		// The measured wire shape, mirrored positive.
		"tracking_param_value_1": `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GFEEDBACK","params":[{"key":"logged_in","value":"1"}]}],` +
			`"mainAppWebResponseContext":{"trackingParam":"redacted"}}}`,
		"main_app_logged_in_true": `{"responseContext":{"mainAppWebResponseContext":{"loggedIn":true}}}`,
		// A positive alongside a negative: positive wins, and is looked for
		// first, so no signed-in read can be lost to a stray zero.
		"positive_outranks_negative": `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GUIDED_HELP","params":[{"key":"logged_in","value":"0"}]},` +
			`{"service":"GFEEDBACK","params":[{"key":"logged_in","value":"1"}]}]}}`,
		// String-fallback path: not valid JSON, positive needle present.
		"broken_json_positive_needle": `{"responseContext":{"serviceTrackingParams":[` +
			`{"params":[{"key":"logged_in","value":"1"}]`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			pointYouTubeGuideAt(t, bodyServer(t, body))
			rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

			auth, err := rs.checkAndRefreshYouTube(context.Background())
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !auth {
				t.Error("authenticated = false, want true — an explicit positive marker is a verdict")
			}
		})
	}
}

// TestGuide200ExplicitNegativeIsStillConclusive is control two. A real
// logged-out session must keep returning (false, nil) so recovery still fires
// for credentials that genuinely died — suppressing that would be the opposite
// failure, and just as bad.
//
// The first case is the shape measured on the wire; the rest are the other
// accepted negatives.
func TestGuide200ExplicitNegativeIsStillConclusive(t *testing.T) {
	bodies := map[string]string{
		"measured_wire_shape":      loggedOutRealShapeBody,
		"tracking_param_value_0":   `{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":"0"}]}]}}`,
		"main_app_logged_out_true": `{"responseContext":{"mainAppWebResponseContext":{"loggedOut":true}}}`,
		"main_app_logged_in_false": loggedOutGuideBody,
		// String-fallback path: not valid JSON, negative needle present.
		"broken_json_negative_needle": `{"responseContext":{"serviceTrackingParams":[` +
			`{"params":[{"key":"logged_in","value":"0"}]`,
	}

	for name, body := range bodies {
		for entryName, entry := range map[string]func(*RefreshService, context.Context) (bool, error){
			"checkAndRefreshYouTube": (*RefreshService).checkAndRefreshYouTube,
			"checkYouTubeAuth":       (*RefreshService).checkYouTubeAuth,
		} {
			t.Run(name+"/"+entryName, func(t *testing.T) {
				pointYouTubeGuideAt(t, bodyServer(t, body))
				rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

				auth, err := entry(rs, context.Background())
				if err != nil {
					t.Fatalf("err = %v, want nil — an explicit negative marker is a real verdict", err)
				}
				if auth {
					t.Error("authenticated = true, want false")
				}
			})
		}
	}
}

// TestGuideVerdictMirrorsLivenessVerdictsRule pins the rule this change was
// asked to mirror, at the unit level, against the sibling in
// internal/youtube/watch_page.go: an explicit marker or nothing.
//
// The sibling cannot be imported (internal/youtube/auth.go already imports this
// package, so the dependency only runs one way) and reads a different
// serialisation anyway — booleans in a ytcfg blob there, "1"/"0" strings in a
// JSON object here. So the rule is restated as a table rather than delegated.
func TestGuideVerdictMirrorsLivenessVerdictsRule(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantAuth     bool
		wantReadable bool
	}{
		{"measured anonymous reply", loggedOutRealShapeBody, false, true},
		{"explicit positive", `{"responseContext":{"mainAppWebResponseContext":{"loggedIn":true}}}`, true, true},
		{"no marker at all", unmarkedJSONBody, false, false},
		{"not json at all", captivePortalBody, false, false},
		{"empty body", "", false, false},
		{"empty json object", `{}`, false, false},
		// A marker whose VALUE is one we do not recognise is a marker we found
		// and failed to understand — the sibling's rule for that is Unknown,
		// not false.
		{"unrecognised marker value", `{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":"maybe"}]}]}}`, false, false},
		// loggedOut:false has never been observed. Inferring "signed in" from
		// it would be a guess; not guessing costs an inconclusive result.
		{"logged_out_false_is_not_a_positive", `{"responseContext":{"mainAppWebResponseContext":{"loggedOut":false}}}`, false, false},
		// Whitespace: free on the JSON path, because encoding/json is a real
		// parser. This is the tolerance decision, pinned.
		{"whitespace around the json marker", "{\n  \"responseContext\" : {\n    \"mainAppWebResponseContext\" : {\n      \"loggedIn\" : true\n    }\n  }\n}", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := youtubeGuideAuthVerdict([]byte(tc.body))
			if auth != tc.wantAuth {
				t.Errorf("authenticated = %v, want %v", auth, tc.wantAuth)
			}
			if readable := err == nil; readable != tc.wantReadable {
				t.Errorf("readable = %v (err = %v), want %v", readable, err, tc.wantReadable)
			}
		})
	}
}

// TestUnrelatedParamTypeDriftDoesNotBlindTheReader: encoding/json fails the
// WHOLE body if any single field mistypes, so a param this reader has no
// interest in — visitor_data, cver, e — gaining a numeric value used to
// collapse the entire JSON path to the literal-needle fallback.
//
// That degradation is safe (it lands on inconclusive, not on a false alarm) but
// it is permanent and it is triggered by a field we never asked about, so
// Value is decoded lazily per-param. This pins that: the real logged_in marker
// must still be read with a mistyped sibling next to it, in both directions.
//
// The fixtures put the marker's own keys in the order {"value":…,"key":…}, and
// that detail is what makes this test decisive rather than vacuous. With the
// measured wire order ({"key":…,"value":…}) the literal-needle fallback happens
// to catch the marker anyway, so a type-strict reader and a lazy one give the
// same answer and the test would pass either way — the junction pattern. The
// needles are order-specific, so reversing the pair removes the fallback as an
// explanation and leaves the JSON path as the only thing that can produce a
// verdict. That reversal is also precisely the drift the finding named: one
// serialiser change away.
func TestUnrelatedParamTypeDriftDoesNotBlindTheReader(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantAuth bool
	}{
		{"numeric sibling, positive marker", `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GFEEDBACK","params":[{"value":123,"key":"cver"},{"value":"1","key":"logged_in"}]}]}}`, true},
		{"numeric sibling, negative marker", `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GFEEDBACK","params":[{"value":123,"key":"cver"},{"value":"0","key":"logged_in"}]}]}}`, false},
		{"object sibling, negative marker", `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GFEEDBACK","params":[{"value":{"nested":true},"key":"e"},{"value":"0","key":"logged_in"}]}]}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := youtubeGuideAuthVerdict([]byte(tc.body))
			if err != nil {
				t.Fatalf("err = %v, want nil — an unrelated param's type must not cost us the marker", err)
			}
			if auth != tc.wantAuth {
				t.Errorf("authenticated = %v, want %v", auth, tc.wantAuth)
			}
		})
	}

	// The control for the paragraph above: the SAME mistyped sibling in the
	// measured wire order really is rescued by the fallback, on both readers.
	// Without this, the reversal could look like an arbitrary fixture choice
	// rather than the thing that isolates the JSON path.
	t.Run("wire order is rescued by the fallback either way", func(t *testing.T) {
		body := `{"responseContext":{"serviceTrackingParams":[` +
			`{"service":"GFEEDBACK","params":[{"key":"cver","value":123},{"key":"logged_in","value":"1"}]}]}}`
		auth, err := youtubeGuideAuthVerdict([]byte(body))
		if err != nil || !auth {
			t.Fatalf("auth=%v err=%v, want true/nil", auth, err)
		}
	})

	// The marker's OWN value going non-string is different: that is a marker we
	// found and could not read, which is inconclusive rather than a verdict.
	t.Run("the marker itself is not a string", func(t *testing.T) {
		body := `{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":0}]}]}}`
		auth, err := youtubeGuideAuthVerdict([]byte(body))
		if auth {
			t.Error("authenticated = true, want false")
		}
		if !errors.Is(err, errGuideLoginMarkerUnreadable) {
			t.Errorf("err = %v, want errGuideLoginMarkerUnreadable", err)
		}
	})
}

// TestGuideVerdictFallbackRespectsTheBodyCap: the string fallback promotes at
// most authBodyFallbackLimit bytes to a Go string, so a marker buried past the
// cap is not found — and, under the new rule, that miss is inconclusive rather
// than a verdict. The cap keeps a multi-MB payload out of memory and out of any
// accidental log line (#24).
func TestGuideVerdictFallbackRespectsTheBodyCap(t *testing.T) {
	// Not valid JSON, so the fallback runs; the negative needle sits past the
	// cap.
	body := "<html>" + strings.Repeat("x", authBodyFallbackLimit) + `{"key":"logged_in","value":"0"}`

	auth, err := youtubeGuideAuthVerdict([]byte(body))
	if auth {
		t.Error("authenticated = true, want false")
	}
	if !errors.Is(err, errGuideLoginMarkerUnreadable) {
		t.Errorf("err = %v, want errGuideLoginMarkerUnreadable — a marker past the cap was not read, so nothing was learned", err)
	}
}
