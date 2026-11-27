package cipher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// TestBrokenPlayerIsEvictedOncePerWindow: ejs throws preparing both bodies
// of a player. Every solve used to evict the player's caches, refetch its
// main body and have the sidecar prepare it twice over. Now the first solve
// does that once (and fetches tv once), and until staleVerdictWindow has
// passed the later ones go straight to goja with no sidecar call, no
// eviction and no fetch. After the window the recovery runs once more.
//
// One Warn in all says the player fell to goja, with the main failure and
// the tv failure in it; the signature RoutedResolveURL then hands to goja
// for the same player adds none.
//
// Mutants: the mainDead / tvDead short-cuts removed (each solve asks the
// sidecar again); noteEviction dropped (each solve evicts); the window never
// ending; the goja Warn made per solve, or once per caller rather than once
// per player.
func TestBrokenPlayerIsEvictedOncePerWindow(t *testing.T) {
	logs := captureSlog(t)
	sc := newVariantSidecar()
	sc.tvErr = errors.New("sidecar: ejs preprocess: tv broke too")
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "cb017549", js: "main-js"}, tvJS: "tv-js"}
	goja, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	gojaN := 0
	goja.cacheSolvers(CacheKey(nTestPlayerURL), &Solvers{N: func(c string) (string, error) {
		gojaN++
		return "goja(" + c + ")", nil
	}})
	c := newCompositeSolverWith(newSidecarSolverWith(sc, src), goja)
	now := time.Unix(1_800_000_000, 0)
	c.players.now = func() time.Time { return now }
	ctx := context.Background()

	for i := range 3 {
		ch := fmt.Sprintf("challenge%d", i)
		if got, err := c.N(ctx, "cb017549", ch); err != nil || got != "goja("+ch+")" {
			t.Fatalf("N %d = %q, %v; want goja's answer", i, got, err)
		}
		now = now.Add(time.Minute)
	}
	if shapes := callShapes(sc.calls); len(shapes) != 4 || src.removals != 1 || src.tvFetches != 1 || gojaN != 3 {
		t.Errorf("three solves within the window: sidecar calls %v, evictions %d, tv fetches %d, goja solves %d; want the first solve's 4 calls, 1, 1 and 3",
			shapes, src.removals, src.tvFetches, gojaN)
	}

	// A signature of the same player: the routed solver fails it at once,
	// and goja (which has no sig function for it) is asked last.
	_, err = RoutedResolveURL(ctx, c, goja, ResolveURLRequest{
		PlayerURL: nTestPlayerURL, StreamURL: "https://example.com/videoplayback", EncryptedSignature: "ABC",
	})
	if !errors.Is(err, ErrPlayerJSStale) || len(sc.calls) != 4 {
		t.Errorf("signature within the window: err = %v after %d sidecar calls; want ErrPlayerJSStale and no new call", err, len(sc.calls))
	}

	now = now.Add(staleVerdictWindow)
	if _, err := c.N(ctx, "cb017549", "later"); err != nil {
		t.Fatalf("N after the window: %v", err)
	}
	if len(sc.calls) != 8 || src.removals != 2 || src.tvFetches != 2 {
		t.Errorf("after the window: sidecar calls %d, evictions %d, tv fetches %d; want one more recovery (8, 2, 2)",
			len(sc.calls), src.removals, src.tvFetches)
	}

	warns := warnLines(logs.String())
	if len(warns) != 1 || !strings.Contains(warns[0], "goja") || !strings.Contains(warns[0], "playerID=cb017549") ||
		!strings.Contains(warns[0], "addOnDisposeCallback") || !strings.Contains(warns[0], "tv broke too") {
		t.Errorf("Warn lines = %q, want one saying the player fell to goja, with the main and the tv failure", warns)
	}
}

// TestOneBadChallengeDoesNotSilenceThePlayer: "no solutions" can be one
// challenge's failure, on both bodies. The recovery it sets off runs once
// per window, but the player is still asked: its other challenges are
// answered by the sidecar, and the bad one costs two plain asks, with no JS
// attached, nothing evicted and nothing fetched.
//
// Mutants: every stale-JS failure treated as the body's (the good challenge
// goes to goja); the window not applied to "no solutions" (the bad challenge
// evicts again); the JS attached again within the window.
func TestOneBadChallengeDoesNotSilenceThePlayer(t *testing.T) {
	captureSlog(t)
	tvLoaded := false
	sc := &fakeSidecar{}
	sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		if strings.HasSuffix(req.PlayerID, "-tv") {
			if req.PlayerJS != "" {
				tvLoaded = true
			}
			if !tvLoaded {
				return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
			}
		}
		res := sidecar.SolveCipherResult{NResults: map[string]string{}}
		for _, ch := range req.NChallenges {
			if ch == "bad" {
				return sidecar.SolveCipherResult{}, errors.New("sidecar: ejs solve n: no solutions (exception sentinel)")
			}
			res.NResults[ch] = "ejs(" + ch + ")"
		}
		return res, nil
	}
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}
	goja := &countingGoja{staticSolver: staticSolver{n: map[string]string{"bad": "goja-bad"}}}
	c := newCompositeSolverWith(newSidecarSolverWith(sc, src), goja)
	ctx := context.Background()

	if got, err := c.N(ctx, "P1", "bad"); err != nil || got != "goja-bad" {
		t.Fatalf("N(bad) = %q, %v; want goja's answer", got, err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1+js P1+js+force P1-tv P1-tv+js" || src.removals != 1 {
		t.Fatalf("first bad solve: calls %v, evictions %d; want the recovery and the tv ask, 1 eviction", shapes, src.removals)
	}

	sc.calls = nil
	if got, err := c.N(ctx, "P1", "good"); err != nil || got != "ejs(good)" {
		t.Errorf("N(good) = %q, %v; want the sidecar's answer", got, err)
	}
	if got, err := c.N(ctx, "P1", "bad"); err != nil || got != "goja-bad" {
		t.Errorf("second N(bad) = %q, %v; want goja's answer", got, err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1 P1 P1-tv" || src.removals != 1 || src.tvFetches != 1 {
		t.Errorf("solves within the window: calls %v, evictions %d, tv fetches %d; want three plain asks, still 1 eviction and 1 fetch",
			shapes, src.removals, src.tvFetches)
	}
	if goja.nCalls != 2 {
		t.Errorf("goja solves = %d, want the two bad challenges only", goja.nCalls)
	}
}

// TestTVFetchVerdict: a tv file that is not there (a definite answer) is not
// asked for again within the window; a fetch that failed in transit is, by
// the next solve.
//
// Mutants: no verdict on a definite answer (two fetches); a verdict on every
// fetch failure (one fetch in the transit case).
func TestTVFetchVerdict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fetchErr    error
		wantFetches int
	}{
		{"not there", fmt.Errorf("%w: %w", ErrPlayerJSFetch, playerFetchStatus(404)), 1},
		{"failed in transit", fmt.Errorf("%w: connection reset", ErrPlayerJSFetch), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureSlog(t)
			src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvErr: tc.fetchErr}
			side := newSidecarSolverWith(newVariantSidecar(), src)
			for range 2 {
				if _, err := side.N(context.Background(), "P1", "n1"); !errors.Is(err, ErrPlayerJSStale) {
					t.Fatalf("N = %v, want ErrPlayerJSStale", err)
				}
			}
			if src.tvFetches != tc.wantFetches {
				t.Errorf("tv fetches over two solves = %d, want %d", src.tvFetches, tc.wantFetches)
			}
		})
	}
}

// TestGojaFallWarnsOnlyWhenTheSidecarWasAsked: with no sidecar half
// installed ([bgutils] use_sidecar = false) goja is all there is, and
// nothing is logged for using it, for n or for a signature. A sidecar half
// that failed a signature logs the fall once, whatever the number of
// signatures.
//
// Mutants: noteGojaFall logging with no sidecar installed; the signature
// path not noting the fall.
func TestGojaFallWarnsOnlyWhenTheSidecarWasAsked(t *testing.T) {
	goja, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	goja.cacheSolvers(CacheKey(nTestPlayerURL), &Solvers{
		Sig: func(string) (string, error) { return "goja-sig", nil },
		N:   func(string) (string, error) { return "goja-n", nil },
	})
	req := ResolveURLRequest{PlayerURL: nTestPlayerURL, StreamURL: "https://example.com/videoplayback?n=rawnvalue", EncryptedSignature: "ABC"}
	ctx := context.Background()

	logs := captureSlog(t)
	for range 2 {
		if _, err := RoutedResolveURL(ctx, newCompositeSolverWith(nil, goja), goja, req); err != nil {
			t.Fatalf("RoutedResolveURL with no sidecar: %v", err)
		}
	}
	if warns := warnLines(logs.String()); len(warns) != 0 {
		t.Errorf("with no sidecar installed: Warn lines %q, want none", warns)
	}

	failing := newCompositeSolverWith(&staticSolver{sigErr: ErrPlayerJSStale, n: map[string]string{"rawnvalue": "routed-n"}}, goja)
	for range 2 {
		if _, err := RoutedResolveURL(ctx, failing, goja, req); err != nil {
			t.Fatalf("RoutedResolveURL with a failing sidecar: %v", err)
		}
	}
	if warns := warnLines(logs.String()); len(warns) != 1 || !strings.Contains(warns[0], "challenge=sig") {
		t.Errorf("with a sidecar that failed two signatures: Warn lines %q, want one for the signature", warns)
	}
}

// gatedSidecar fails every solve as stale until a forced reload has gone
// through, and holds that reload until release is closed.
type gatedSidecar struct {
	mu      sync.Mutex
	fresh   bool
	forced  int
	started chan struct{}
	release chan struct{}
}

func (g *gatedSidecar) SolveCipher(_ context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	if req.ForceReload {
		g.mu.Lock()
		g.forced++
		g.mu.Unlock()
		close(g.started)
		<-g.release
		g.mu.Lock()
		g.fresh = true
		g.mu.Unlock()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.fresh {
		return sidecar.SolveCipherResult{}, errors.New("sidecar: ejs solve n: no solutions")
	}
	res := sidecar.SolveCipherResult{NResults: map[string]string{}}
	for _, c := range req.NChallenges {
		res.NResults[c] = "ejs(" + c + ")"
	}
	return res, nil
}

// TestConcurrentStaleSolvesRecoverOnce: two solves of one player meet a
// stale cached copy at the same moment. The recovery is one at a time per
// player: the second waits for the first's, asks again, and is answered by
// the body the first one reloaded. It used to evict and reload the player a
// second time.
//
// Mutants: the recovery not taking the per-player gate; the waiter not
// asking again (it evicts a second time, or fails as stale and leaves main).
func TestConcurrentStaleSolvesRecoverOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &gatedSidecar{started: make(chan struct{}), release: make(chan struct{})}
		src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "stale", freshJS: "fresh"}, tvJS: "tv-js"}
		side := newSidecarSolverWith(sc, src)
		side.markPlayerSent("P1")

		type answer struct {
			out string
			err error
		}
		answers := make(chan answer, 2)
		solve := func(ch string) {
			defer func() {
				if r := recover(); r != nil {
					answers <- answer{err: fmt.Errorf("panic: %v", r)}
				}
			}()
			out, err := side.N(context.Background(), "P1", ch)
			answers <- answer{out, err}
		}
		go solve("a")
		<-sc.started // the first solve is inside its forced reload
		go solve("b")
		synctest.Wait() // the second has failed its first ask and waits for the gate
		close(sc.release)
		for range 2 {
			if a := <-answers; a.err != nil || !strings.HasPrefix(a.out, "ejs(") {
				t.Errorf("solve = %q, %v; want the sidecar's answer", a.out, a.err)
			}
		}
		if src.removals != 1 || sc.forced != 1 {
			t.Errorf("evictions = %d, forced reloads = %d; want 1 and 1", src.removals, sc.forced)
		}
		if side.mem.usesTV("P1") {
			t.Errorf("the player was moved to tv, though its main body solves once reloaded")
		}
	})
}

// firstCallGate is a sidecar whose ejs throws preparing every body, and
// whose first call waits for release.
type firstCallGate struct {
	mu      sync.Mutex
	calls   []sidecar.SolveCipherRequest
	started chan struct{}
	release chan struct{}
}

func (g *firstCallGate) SolveCipher(_ context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	g.mu.Lock()
	g.calls = append(g.calls, req)
	first := len(g.calls) == 1
	g.mu.Unlock()
	if first {
		close(g.started)
		<-g.release
	}
	return sidecar.SolveCipherResult{}, errMainPreprocess
}

// TestBurstOfFirstSolvesOnABrokenPlayerSendsItOnce: several solves of a
// player nothing has sent yet arrive together (streams going live at once),
// and ejs throws preparing its body. One of them sends it and runs the
// recovery; the ones that waited for the first-send gate take what it found,
// and ask the sidecar nothing. Each used to send the body and have the
// sidecar prepare it again.
//
// Mutant: the memory not read again once the gate is taken (one more send
// per waiter).
func TestBurstOfFirstSolvesOnABrokenPlayerSendsItOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &firstCallGate{started: make(chan struct{}), release: make(chan struct{})}
		src := &fakePlayerSource{playerID: "P1", js: "main-js"}
		side := newSidecarSolverWith(sc, src)
		errs := make(chan error, 3)
		solve := func() {
			defer func() {
				if r := recover(); r != nil {
					errs <- fmt.Errorf("panic: %v", r)
				}
			}()
			_, err := side.N(context.Background(), "P1", "n1")
			errs <- err
		}
		go solve()
		<-sc.started // the first solve is inside its first send
		go solve()
		go solve()
		synctest.Wait() // the other two wait for the gate
		close(sc.release)
		for range 3 {
			if err := <-errs; !errors.Is(err, ErrPlayerJSStale) {
				t.Errorf("solve = %v, want ErrPlayerJSStale", err)
			}
		}
		if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1+js P1+js+force" {
			t.Errorf("sidecar calls = %v, want the first solve's send and its recovery only", shapes)
		}
	})
}
