package routes

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookieStatusPayloadsCarryThePageFacts: the two keys the dashboard's
// sessionHealth reads (owner ruling KA-4) are on each platform's payload from
// that platform's own facts, as RFC 3339 UTC, and absent when there is nothing
// to say, so a healthy install's payload is what it was.
//
// Mutants: project YouTube's facts onto both payloads (the Twitch row fails);
// send the keys empty instead of omitting them (the last block fails).
func TestCookieStatusPayloadsCarryThePageFacts(t *testing.T) {
	zone := time.FixedZone("PST", -8*3600)
	status := cookies.AuthStatus{
		YouTubePages: cookies.PageHealth{
			LastSignedOut: time.Date(2026, 11, 13, 4, 0, 0, 0, zone),
			Attempts:      2,
			NextAttempt:   time.Date(2026, 11, 13, 5, 0, 0, 0, zone),
		},
		TwitchPages: cookies.PageHealth{LastSignedOut: time.Date(2026, 11, 12, 20, 30, 0, 0, zone)},
	}

	yt := CookieStatusPayload(status)
	if yt["pageSignedOutAt"] != "2026-11-13T12:00:00Z" || yt["nextRecoveryAt"] != "2026-11-13T13:00:00Z" {
		t.Errorf("cookieStatus page keys = %v / %v, want the YouTube facts in UTC", yt["pageSignedOutAt"], yt["nextRecoveryAt"])
	}
	tw := TwitchAuthStatusPayload(status)
	if tw["pageSignedOutAt"] != "2026-11-13T04:30:00Z" {
		t.Errorf("twitchAuthStatus pageSignedOutAt = %v, want Twitch's own", tw["pageSignedOutAt"])
	}
	if _, ok := tw["nextRecoveryAt"]; ok {
		t.Errorf("twitchAuthStatus carries nextRecoveryAt = %v with no recovery decided", tw["nextRecoveryAt"])
	}

	for name, payload := range map[string]map[string]any{
		"cookieStatus":     CookieStatusPayload(cookies.AuthStatus{}),
		"twitchAuthStatus": TwitchAuthStatusPayload(cookies.AuthStatus{}),
	} {
		for _, key := range []string{"pageSignedOutAt", "nextRecoveryAt"} {
			if got, ok := payload[key]; ok {
				t.Errorf("%s carries %s = %v with no signed-out page", name, key, got)
			}
		}
	}
}
