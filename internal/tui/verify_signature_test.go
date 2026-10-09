package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestVerifySignatureSaysWhichCheckRan: R S verifies the running binary's own
// signature and, when its release publishes a signed manifest, that manifest
// too. The signature alone says only that the key signed these bytes — a
// validly signed binary of another release passes it — so a release that
// publishes no manifest is reported as a signature-only check, in yellow,
// never as the full verification.
//
// Mutants: drop Manifest from the result message (always false) — the
// manifest row reads signature-only; report every success as the full
// verification — the no-manifest row does not say it has none; state success
// for the no-manifest line — it renders green.
func TestVerifySignatureSaysWhichCheckRan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		manifest  bool
		err       error
		want      string
		wantColor int
	}{
		{"the release's manifest checked too", true, nil, "Signature and release manifest verified", 0},
		{"a release with no manifest", false, nil, "no signed manifest, so only the signature was checked", 1},
		{"a failed check", false, errors.New("SHA-256 does not match"), "Signature verification failed: SHA-256 does not match", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp()
			app.width, app.height = 200, 40
			app.OnVerifySignature = func() (bool, error) { return tc.manifest, tc.err }

			_, cmd := app.dispatchAction("R S", nil)
			msg := runCmd(t, cmd)
			if _, ok := msg.(signatureVerifyResultMsg); !ok {
				t.Fatalf("R S answered %#v, want a signatureVerifyResultMsg", msg)
			}
			app.Update(msg)

			if !strings.Contains(app.feedback.msg, tc.want) {
				t.Errorf("feedback %q does not say %q", app.feedback.msg, tc.want)
			}
			if got := renderedRank(t, feedbackColor(app.feedback.msg, app.feedback.sev)); got != tc.wantColor {
				t.Errorf("feedback %q rendered at rank %d, want %d (0 green, 1 yellow, 2 red)", app.feedback.msg, got, tc.wantColor)
			}
		})
	}
}
