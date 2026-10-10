package cookies

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

// TestUnrecoveredPlatformClosesOnItsFirstHealthyCheck pins the restart half of
// the auth_recovered close. A platform whose failure a previous process
// announced (SetUnrecoveredPlatforms) fires OnAuthRecovered on the first check
// that finds it authenticated, conclusively — a first pass, where hasChecked
// is false, and a pass whose baseline SetExpectedPlatforms seeded as
// authenticated alike, neither of which is a transition this process can see —
// and exactly once. A platform not named stays silent on its first pass, as
// it always has.
//
// Mutants: drop reopenYT or reopenTW from its recovered condition (that
// platform's first-pass close never fires); never clear the flag (the close
// fires again on the next pass).
func TestUnrecoveredPlatformClosesOnItsFirstHealthyCheck(t *testing.T) {
	recorder := func(rs *RefreshService) *[]string {
		var got []string
		rs.OnAuthRecovered = func(p string) { got = append(got, p) }
		return &got
	}

	t.Run("twitch, first pass", func(t *testing.T) {
		rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
		rs.SetUnrecoveredPlatforms([]string{"twitch"})
		got := recorder(rs)
		rs.doRefresh(context.Background())
		rs.doRefresh(context.Background())
		if !slices.Equal(*got, []string{"twitch"}) {
			t.Errorf("OnAuthRecovered fired %v, want [twitch] once", *got)
		}
	})

	t.Run("twitch, baseline seeded authenticated", func(t *testing.T) {
		rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
		rs.SetExpectedPlatforms([]string{"twitch"})
		rs.SetUnrecoveredPlatforms([]string{"twitch"})
		got := recorder(rs)
		rs.doRefresh(context.Background())
		if !slices.Equal(*got, []string{"twitch"}) {
			t.Errorf("OnAuthRecovered fired %v, want [twitch]", *got)
		}
	})

	t.Run("twitch, not named", func(t *testing.T) {
		rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
		got := recorder(rs)
		rs.doRefresh(context.Background())
		if len(*got) != 0 {
			t.Errorf("OnAuthRecovered fired %v for a platform no previous process left open", *got)
		}
	})

	t.Run("twitch, dead first check then repaired", func(t *testing.T) {
		rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusUnauthorized)
		rs.SetUnrecoveredPlatforms([]string{"twitch"})
		got := recorder(rs)
		rs.doRefresh(context.Background())
		if len(*got) != 0 {
			t.Fatalf("OnAuthRecovered fired %v on a check that found the platform dead", *got)
		}
		pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
		rs.doRefresh(context.Background())
		rs.doRefresh(context.Background())
		if !slices.Equal(*got, []string{"twitch"}) {
			t.Errorf("OnAuthRecovered fired %v, want [twitch] once, on the repair", *got)
		}
	})

	t.Run("youtube, first pass", func(t *testing.T) {
		pointYouTubeGuideAt(t, bodyServer(t, loggedInGuideBody))
		pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))
		rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
		rs.SetUnrecoveredPlatforms([]string{"youtube"})
		got := recorder(rs)
		rs.doRefresh(context.Background())
		rs.doRefresh(context.Background())
		if !slices.Equal(*got, []string{"youtube"}) {
			t.Errorf("OnAuthRecovered fired %v, want [youtube] once", *got)
		}
	})
}
