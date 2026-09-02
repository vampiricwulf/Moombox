package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitchAuthLossVocabularyCoversEveryDowngradeReason is the drift pin
// internal/cookies asks for and cannot write itself.
//
// internal/cookies mirrors internal/twitch's AuthDowngrade* tokens BY VALUE,
// because internal/twitch imports internal/cookies and the dependency only
// runs one way. This package imports BOTH, so it is the only place the two
// vocabularies can be compared.
//
// The mutation this catches is the realistic one: a fifth AuthDowngrade* route
// added upstream with no arm in twitchAuthLossMessage, or a token whose value
// is edited on one side. Either lands the operator on the generic
// "the saved Twitch login could not be used", which names no remedy.
//
// It asserts through the SERVICE rather than against a copy of the sentences:
// a table of expected strings here would be a second source of truth and would
// pass while the mapping was wrong in both places.
func TestTwitchAuthLossVocabularyCoversEveryDowngradeReason(t *testing.T) {
	reasons := []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
	}

	// The sentence an unrecognised token renders, discovered rather than
	// hardcoded, so this test cannot drift from the default arm's wording.
	generic := twitchAuthLossSentence(t, "a-token-no-arm-was-ever-written-for")

	seen := map[string]string{}
	for _, reason := range reasons {
		got := twitchAuthLossSentence(t, reason)
		if got == generic {
			t.Errorf("%q renders the generic sentence — internal/cookies has no arm for it", reason)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q render the same sentence %q", prev, reason, got)
		}
		seen[got] = reason
	}
}

// twitchAuthLossSentence drives one reason through the real RefreshService and
// returns the sentence it published. No network: NoteTwitchAuthLoss makes no
// request, and with no callbacks wired it invokes nothing.
// It uses an EMPTY jar on purpose: with no Twitch cookies configured,
// shouldFireRecovery declines, so no callback fires and nothing here can reach
// a network or a notifier.
//
// nopWorkerLogger is the package's existing discard logger
// (stream_processor_twitch_credentials_test.go:403) and already satisfies the
// anonymous logger interface cookies.NewRefreshService takes.
func twitchAuthLossSentence(t *testing.T, reason string) string {
	t.Helper()
	rs := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopWorkerLogger{})
	rs.NoteTwitchAuthLoss(reason)
	got := rs.GetStatus().TwitchError
	if got == "" {
		t.Fatalf("NoteTwitchAuthLoss(%q) published no reason at all", reason)
	}
	return got
}
