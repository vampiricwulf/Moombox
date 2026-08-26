package main

import (
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// observation is one recorded ObserveLiveness call.
type observation struct {
	platform string
	loggedIn bool
}

// TestRouteLivenessVerdictOnlyRoutesConclusiveAnswers pins the rule the whole
// arc rests on: only an answer YouTube actually gave us reaches the health
// signal.
//
// SessionAuthUnknown is the case that matters. It is what comes back from a
// consent wall, a rate limit, an off-host redirect, a cookie-stripped chain,
// and a jar that was never configured — none of which is evidence about the
// session. Routing it as loggedIn=false would tell an operator with working
// cookies to replace them.
func TestRouteLivenessVerdictOnlyRoutesConclusiveAnswers(t *testing.T) {
	cases := []struct {
		name    string
		verdict youtube.SessionAuthState
		want    []observation
	}{
		{"logged in", youtube.SessionAuthLoggedIn, []observation{{"youtube", true}}},
		{"logged out", youtube.SessionAuthLoggedOut, []observation{{"youtube", false}}},
		{"unknown", youtube.SessionAuthUnknown, nil},
		{"unrecognised value", youtube.SessionAuthState("something-new"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []observation
			routeLivenessVerdict(func(p string, in bool) {
				got = append(got, observation{p, in})
			}, tc.verdict)

			if len(got) != len(tc.want) {
				t.Fatalf("observations = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("observation %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLivenessFromProbeCollapse: every way the account probe can fail to
// produce evidence has to arrive at the refresh service as conclusive=false.
// The error cases are the dangerous half — a rate limit reported as a dead
// session is exactly the false alarm this arc is meant not to introduce.
func TestLivenessFromProbeCollapse(t *testing.T) {
	probeErr := errors.New("account liveness probe: unexpected status 429")

	cases := []struct {
		name           string
		verdict        youtube.SessionAuthState
		err            error
		wantLoggedIn   bool
		wantConclusive bool
	}{
		{"logged in", youtube.SessionAuthLoggedIn, nil, true, true},
		{"logged out", youtube.SessionAuthLoggedOut, nil, false, true},
		{"unrecognised body", youtube.SessionAuthUnknown, nil, false, false},
		{"transport or non-2xx", youtube.SessionAuthUnknown, probeErr, false, false},
		// Defensive: nothing returns a verdict alongside an error today, but
		// if something ever did, the error still wins. We did not observe a
		// qualifying page.
		{"verdict alongside an error", youtube.SessionAuthLoggedOut, probeErr, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn, conclusive := livenessFromProbe(tc.verdict, tc.err)
			if loggedIn != tc.wantLoggedIn || conclusive != tc.wantConclusive {
				t.Errorf("livenessFromProbe(%q, %v) = (%v, %v), want (%v, %v)",
					tc.verdict, tc.err, loggedIn, conclusive, tc.wantLoggedIn, tc.wantConclusive)
			}
		})
	}
}
