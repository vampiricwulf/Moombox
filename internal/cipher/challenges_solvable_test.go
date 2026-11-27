package cipher

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
)

// TestChallengesSolvable is owner ruling RV-16's predicate ("If it works,
// then we shouldn't treat them as unsolvable"): the player's JS challenges
// are solvable while the sidecar is ready, or while goja is there and has not
// refused the player in question; a player goja refused is unsolvable only
// with the sidecar down, a refusal of another player says nothing about this
// one, an expired refusal ends, and with no goja the sidecar is all there is.
//
// Mutants: ChallengesSolvable returning SidecarReady (the goja rows); the
// refusal check dropped (the refused row); the refusal read against any
// player (the other-player row); refuses ignoring expiry (the expired row);
// a sidecar that is up answered by goja's refusal (the sidecar-up row).
func TestChallengesSolvable(t *testing.T) {
	const (
		refused = "https://www.youtube.com/s/player/aaaa1111/player_ias.vflset/en_US/base.js"
		other   = "https://www.youtube.com/s/player/bbbb2222/player_ias.vflset/en_US/base.js"
	)
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	origNow := resolverNow
	resolverNow = func() time.Time { return clock }
	t.Cleanup(func() { resolverNow = origNow })

	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	g.refuse(CacheKey(refused), ErrExtractorMismatch)

	up := &healthSidecar{fakeSidecar: fakeSidecar{respond: func(sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		return sidecar.SolveCipherResult{}, errors.New("unused")
	}}}
	up.healthy.Store(true)
	src := &fakePlayerSource{playerID: "p1", js: "JS"}
	down := &healthSidecar{}

	for _, tc := range []struct {
		name      string
		c         SidecarSwappable
		playerURL string
		want      bool
	}{
		{"sidecar up, goja refusing the player", NewCompositeSolver(NewSidecarSolver(up, src), g), refused, true},
		{"sidecar up, no goja", NewCompositeSolver(NewSidecarSolver(up, src), nil), other, true},
		{"no sidecar, goja that has not refused the player", NewCompositeSolver(nil, g), other, true},
		{"sidecar down, goja that has not refused the player", NewCompositeSolver(NewSidecarSolver(down, src), g), other, true},
		{"no sidecar, goja refusing the player", NewCompositeSolver(nil, g), refused, false},
		{"sidecar down, goja refusing the player", NewCompositeSolver(NewSidecarSolver(down, src), g), refused, false},
		{"no sidecar, goja, no player named", NewCompositeSolver(nil, g), "", true},
		{"no sidecar, no goja", NewCompositeSolver(nil, nil), other, false},
		{"sidecar down, no goja", NewCompositeSolver(NewSidecarSolver(down, src), nil), other, false},
	} {
		if got := tc.c.ChallengesSolvable(tc.playerURL); got != tc.want {
			t.Errorf("%s: ChallengesSolvable = %v, want %v", tc.name, got, tc.want)
		}
	}

	clock = clock.Add(compileRefusal)
	if !NewCompositeSolver(nil, g).ChallengesSolvable(refused) {
		t.Error("no sidecar, goja whose refusal of the player has expired: ChallengesSolvable = false, want true")
	}
}

// TestChallengesSolvableSeesARefusalGojaMadeItself: the refusal the
// predicate reads is the one a real goja solve writes — a signature through
// the composite (no sidecar) on a player the port cannot prepare refuses it
// under the routed URL (playerURLForID) — and it is seen through every form
// a watch page names that player in: absolute, relative, another locale or
// variant path. A refusal keyed by the whole URL would leave the predicate
// true for the watch page's form.
//
// Mutant: refuses keyed on the URL itself rather than CacheKey (every form
// but the routed one reads solvable).
func TestChallengesSolvableSeesARefusalGojaMadeItself(t *testing.T) {
	const playerID = "eeee5555"
	origClient, origNow := playerHTTPClient, resolverNow
	playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	resolverNow = func() time.Time { return clock }
	t.Cleanup(func() { playerHTTPClient, resolverNow = origClient, origNow })

	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.playerCache.Put(playerURLForID(playerID), "var player = 1;"); err != nil {
		t.Fatal(err)
	}
	c := NewCompositeSolver(nil, g)
	forms := []string{
		"/s/player/" + playerID + "/player_ias.vflset/en_US/base.js",
		"https://www.youtube.com/s/player/" + playerID + "/player_ias.vflset/ja_JP/base.js",
		"https://www.youtube.com/s/player/" + playerID + "/tv-player-ias.vflset/tv-player-ias.js",
	}
	for _, u := range forms {
		if !c.ChallengesSolvable(u) {
			t.Fatalf("before any solve: ChallengesSolvable(%s) = false, want true", u)
		}
	}
	if _, err := c.Sig(context.Background(), playerID, "AAAA"); err == nil {
		t.Fatal("Sig on a player the port cannot prepare answered")
	}
	for _, u := range forms {
		if c.ChallengesSolvable(u) {
			t.Errorf("after goja refused the player: ChallengesSolvable(%s) = true, want false", u)
		}
	}
}
