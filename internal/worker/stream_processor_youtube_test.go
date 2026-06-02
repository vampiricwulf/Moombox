package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestIsTerminalPlayability locks in the narrow give-up set for the
// upcoming-wait loop: only states that no amount of waiting or re-auth can fix
// are terminal. Members-only / login (handled by the authenticated-probe
// switch) and age-restricted (handled at initial Process / resolvable with
// auth) must NOT be terminal, so a transient or auth-remediable state never
// wrongly errors a waiting stream.
func TestIsTerminalPlayability(t *testing.T) {
	terminal := []youtube.PlayabilityError{
		youtube.PlayabilityPrivate,
		youtube.PlayabilityUnavailable,
		youtube.PlayabilityRegionBlocked,
	}
	for _, p := range terminal {
		if !isTerminalPlayability(p) {
			t.Errorf("expected playability %q to be terminal (give up)", p)
		}
	}

	nonTerminal := []youtube.PlayabilityError{
		youtube.PlayabilityOK,
		youtube.PlayabilityMembersOnly,
		youtube.PlayabilityLoginRequired,
		youtube.PlayabilityAgeRestricted,
		youtube.PlayabilityUnknown,
	}
	for _, p := range nonTerminal {
		if isTerminalPlayability(p) {
			t.Errorf("expected playability %q to be NON-terminal (keep waiting / handled elsewhere)", p)
		}
	}
}
