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
		{"no sidecar, goja, no player named", NewCompositeSolver(nil, g), "", false},
		{"sidecar up, no player named", NewCompositeSolver(NewSidecarSolver(up, src), g), "", true},
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

// TestChallengesSolvableSeesGojaAnsweringNoN: a player goja prepares but
// whose n function answers with no answer (checkNResult: the exception
// sentinel) does not work, so after that answer the predicate reads it
// unsolvable with the sidecar down (owner ruling RV-16), through every form
// of the player's URL, while the compiled copy stays cached and still runs.
// The mark is the predicate's alone (GetSolvers does not refuse the player),
// a later good n ends it, ResolveURL's own goja n marks too, and a player
// that answers well is never marked.
//
// Mutants: noteNAnswer not marking (the predicate stays true after the failed
// solve); the mark written to the refusal list GetSolvers reads (refusedErr
// non-nil); a good n not clearing it; ResolveURL keeping its bare
// checkNResult (the ResolveURL row).
func TestChallengesSolvableSeesGojaAnsweringNoN(t *testing.T) {
	const bad, good = "ffff6666", "abcd7777"
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	runs, broken := 0, true
	g.cacheSolvers(CacheKey(playerURLForID(bad)), &Solvers{N: func(in string) (string, error) {
		runs++
		if !broken {
			return "fine", nil
		}
		return "enhanced_except_x_" + in, nil
	}})
	g.cacheSolvers(CacheKey(playerURLForID(good)), &Solvers{N: func(in string) (string, error) {
		return "solved", nil
	}})
	c := NewCompositeSolver(nil, g)
	ctx := context.Background()

	if out, err := c.N(ctx, good, "nnnn"); err != nil || out != "solved" {
		t.Fatalf("N on the good player = %q, %v", out, err)
	}
	if !c.ChallengesSolvable(playerURLForID(good)) {
		t.Error("a player goja answered well: ChallengesSolvable = false, want true")
	}

	forms := []string{
		"/s/player/" + bad + "/player_ias.vflset/en_US/base.js",
		"https://www.youtube.com/s/player/" + bad + "/tv-player-ias.vflset/tv-player-ias.js",
	}
	for _, u := range forms {
		if !c.ChallengesSolvable(u) {
			t.Fatalf("before any solve: ChallengesSolvable(%s) = false, want true", u)
		}
	}
	if _, err := c.N(ctx, bad, "nnnn"); err == nil {
		t.Fatal("N on a player whose n answers the exception sentinel answered")
	}
	for _, u := range forms {
		if c.ChallengesSolvable(u) {
			t.Errorf("after goja answered no n: ChallengesSolvable(%s) = true, want false", u)
		}
	}
	if _, err := c.N(ctx, bad, "mmmm"); err == nil || runs != 2 {
		t.Errorf("the cached copy of the marked player: err %v, runs %d; want an error from a second run", err, runs)
	}
	// The mark is ChallengesSolvable's alone: GetSolvers still compiles the
	// player after a per-job 403 drops its copy, for its signatures.
	if err := g.refusedErr(CacheKey(playerURLForID(bad)), bad); err != nil {
		t.Errorf("GetSolvers refuses a player goja answered no n for: %v", err)
	}
	// A later good n ends the mark.
	broken = false
	if _, err := c.N(ctx, bad, "kkkk"); err != nil {
		t.Fatalf("N once the player answers: %v", err)
	}
	if !c.ChallengesSolvable(forms[0]) {
		t.Error("after a good n: ChallengesSolvable = false, want true")
	}

	// ResolveURL runs goja's n itself, and marks the same way.
	const viaURL = "1234abcd"
	g.cacheSolvers(CacheKey(playerURLForID(viaURL)), &Solvers{N: func(in string) (string, error) {
		return "enhanced_except_y_" + in, nil
	}})
	if _, err := g.ResolveURL(ctx, ResolveURLRequest{
		StreamURL: "https://rr1---sn-x.googlevideo.com/videoplayback?itag=18&n=qqqqqqqq",
		PlayerURL: playerURLForID(viaURL),
	}); !errors.Is(err, ErrNUnsolved) {
		t.Fatalf("ResolveURL on a player whose n answers the sentinel: %v, want ErrNUnsolved", err)
	}
	if c.ChallengesSolvable(playerURLForID(viaURL)) {
		t.Error("after ResolveURL got no n answer: ChallengesSolvable = true, want false")
	}
}
