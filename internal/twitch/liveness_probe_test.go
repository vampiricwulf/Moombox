package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// probeRoundTripper routes every request to one canned reply.
type probeRoundTripper func(*http.Request) (*http.Response, error)

func (f probeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// installProbeStub swaps the package-level twitchHTTPClient for one that
// answers the GQL endpoint with (status, body) and refuses every other host,
// and restores it in t.Cleanup. It returns the request counter so a test can
// assert that a guard REFUSED to send rather than sent and ignored.
//
// The swap is why no test in this file may call t.Parallel — the var is shared
// with every other test in the package, exactly as installHLSStub documents.
func installProbeStub(t *testing.T, status int, body string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })

	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			// A request anywhere else is a defect in this stub, not a pass.
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
		calls.Add(1)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			Request:    req,
		}, nil
	})}
	return &calls
}

// tokenReply renders the real GQL success shape around one token document.
// The signature is always empty: nothing in this file is a signed anything.
func tokenReply(t *testing.T, tokenValue string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"streamPlaybackAccessToken": map[string]any{
				"value":     tokenValue,
				"signature": "",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// probeService builds a Service over a jar written from `rows` (the package's
// row helper, auth_test.go), so a cookieless install is `probeService(t)`.
func probeService(t *testing.T, rows ...string) *Service {
	t.Helper()
	jar := hlsJar(t, filepath.Join(t.TempDir(), "cookies.txt"), rows...)
	return NewService(jar, nopLogger{})
}

const probeFixtureToken = "test-token-aaaa"

// TestProbeSessionLivenessReadsTheSessionFromTheToken is the whole verdict
// rule, end to end through the real method.
//
// MUTATION CLOSED: swapping the returned pair for PlaybackTokenSession's
// arguments, or collapsing the absent-key arm into "anonymous". The last is
// the expensive one — a key Twitch renames would then mark every install's
// session dead on the day of the change.
func TestProbeSessionLivenessReadsTheSessionFromTheToken(t *testing.T) {
	for _, tc := range []struct {
		name           string
		tokenValue     string
		wantSignedIn   bool
		wantConclusive bool
		wantErr        bool
	}{
		{"signed in", `{"user_id":12345678,"channel":"somechannel"}`, true, true, false},
		{"anonymous", `{"user_id":null,"channel":"somechannel"}`, false, true, false},
		{"renamed key", `{"userId":12345678}`, false, false, true},
		{"not a json object", `not-json`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, http.StatusOK, tokenReply(t, tc.tokenValue))
			svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

			signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")

			if calls.Load() != 1 {
				t.Fatalf("GQL requests = %d, want 1 — the assertions below say nothing about a probe that never ran", calls.Load())
			}
			if signedIn != tc.wantSignedIn || conclusive != tc.wantConclusive {
				t.Errorf("got (signedIn=%v, conclusive=%v), want (%v, %v)", signedIn, conclusive, tc.wantSignedIn, tc.wantConclusive)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err != nil = %v, want %v", err != nil, tc.wantErr)
			}
		})
	}
}

// TestProbeSessionLivenessRefusesWithoutInputs: both guards must refuse BEFORE
// the request, and the counter is what proves "refused" rather than "sent and
// ignored".
//
// MUTATION CLOSED (cookieless): dropping the auth-token guard. Twitch answers
// an unauthenticated playback-token request with an ANONYMOUS token by design,
// so the probe would report signedIn=false conclusively — a permanent false
// "your Twitch session is dead" on every cookieless install.
// MUTATION CLOSED (no channel): dropping the login guard. safeLoginRe turns
// both "" and "!!!" into an empty channelName — a request that cannot answer,
// sent every tick.
func TestProbeSessionLivenessRefusesWithoutInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []string
		channel string
	}{
		{"cookieless install", nil, "somechannel"},
		{"no channel configured", []string{row(".twitch.tv", "auth-token", probeFixtureToken)}, ""},
		{"login with no usable characters", []string{row(".twitch.tv", "auth-token", probeFixtureToken)}, "!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, http.StatusOK, tokenReply(t, `{"user_id":null}`))
			svc := probeService(t, tc.rows...)

			signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), tc.channel)

			if got := calls.Load(); got != 0 {
				t.Errorf("GQL requests = %d, want 0 — the probe asked anyway", got)
			}
			if signedIn || conclusive {
				t.Errorf("got (signedIn=%v, conclusive=%v), want (false, false)", signedIn, conclusive)
			}
			if !errors.Is(err, ErrLivenessProbeNotAttempted) {
				t.Errorf("err = %v, want ErrLivenessProbeNotAttempted", err)
			}
		})
	}
}

// TestProbeSessionLivenessAuthRefusalIsInconclusive: a 401/403 is NOT a
// signed-out verdict.
//
// gqlRequest raises ErrTwitchAuthExpired for 403 as well as 401, and 403 is an
// edge block as often as a credential verdict; whether a genuinely expired
// auth-token even surfaces here is unmeasured (field-test gate 18). Reporting
// it as conclusive would send an operator to re-export working credentials.
//
// MUTATION CLOSED: mapping the auth arm to (false, true). The sentinel is
// asserted alongside so the arm stays identifiable in a future measurement.
func TestProbeSessionLivenessAuthRefusalIsInconclusive(t *testing.T) {
	calls := installProbeStub(t, http.StatusUnauthorized, `{"error":"Unauthorized"}`)
	svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

	signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")

	if calls.Load() != 1 {
		t.Fatalf("GQL requests = %d, want 1", calls.Load())
	}
	if signedIn || conclusive {
		t.Errorf("got (signedIn=%v, conclusive=%v), want (false, false) — a refusal is not a verdict", signedIn, conclusive)
	}
	if !errors.Is(err, ErrTwitchAuthExpired) {
		t.Errorf("err = %v, want it to wrap ErrTwitchAuthExpired so the arm stays identifiable", err)
	}
}

// TestProbeSessionLivenessErrorsCarryNoResponseBody is the leak barrier, and
// the reason this method does not pass upstream errors through: gqlRequest
// interpolates string(respData) into its 4xx and auth errors, Task 3's closure
// logs the result at Debug, and Moombox fans the log out over the WebSocket
// stream to both UIs — so an intermediary's page echoing the Authorization
// header lands on two screens. Same hazard validateErrorDetail clamps for.
//
// MUTATION CLOSED: `return false, false, err` on either error arm — and,
// since a refused request must never be read as a verdict, flipping
// `conclusive` (or `signedIn`) to true on either arm as well. The pair is
// asserted here, not just the error text, or that second mutant survives
// having discarded them with `_, _, err :=`.
func TestProbeSessionLivenessErrorsCarryNoResponseBody(t *testing.T) {
	// A body shaped like the things that must never escape: a bearer header,
	// a token document, and the fixture credential itself.
	const leakyBody = `{"echo":"Authorization: OAuth ` + probeFixtureToken + `","user_id":12345678}`

	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			installProbeStub(t, status, leakyBody)
			svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

			signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")
			if err == nil {
				t.Fatal("want an error from a refused request")
			}
			if signedIn || conclusive {
				t.Errorf("got (signedIn=%v, conclusive=%v), want (false, false) — a refused request is not a verdict", signedIn, conclusive)
			}
			for _, secret := range []string{probeFixtureToken, "Authorization", "OAuth", "user_id", "12345678"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the returned error carried %q: %q", secret, err.Error())
				}
			}
		})
	}
}
