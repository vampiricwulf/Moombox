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
// that timed out says nothing about the player either, and does not spend
// the player's one line: when ejs then really cannot solve its signatures,
// the fall is logged, once, whatever the number of signatures.
//
// Mutants: noteGojaFall logging whatever the sidecar's failure was (a Warn
// with no sidecar installed, and one for the timeout); the timeout spending
// the player's line (no Warn for the real failure); the signature path not
// noting the fall.
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

	half := &staticSolver{sigErr: errors.New("sidecar: solveCipher: timed out"), n: map[string]string{"rawnvalue": "routed-n"}}
	failing := newCompositeSolverWith(half, goja)
	if _, err := RoutedResolveURL(ctx, failing, goja, req); err != nil {
		t.Fatalf("RoutedResolveURL with a sidecar that timed out: %v", err)
	}
	if warns := warnLines(logs.String()); len(warns) != 0 {
		t.Errorf("after a sidecar timeout: Warn lines %q, want none", warns)
	}
	half.sigErr = ErrPlayerJSStale
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

// lockedSidecar is a sidecar stub for solves that run side by side: respond
// runs under mu, and the first call hold picks waits for release before it
// is answered.
type lockedSidecar struct {
	mu      sync.Mutex
	calls   []sidecar.SolveCipherRequest
	respond func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error)
	hold    func(req sidecar.SolveCipherRequest) bool
	held    bool
	started chan struct{}
	release chan struct{}
}

func newLockedSidecar() *lockedSidecar {
	return &lockedSidecar{started: make(chan struct{}), release: make(chan struct{})}
}

func (l *lockedSidecar) SolveCipher(_ context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	l.mu.Lock()
	l.calls = append(l.calls, req)
	wait := l.hold != nil && !l.held && l.hold(req)
	if wait {
		l.held = true
	}
	l.mu.Unlock()
	if wait {
		close(l.started)
		<-l.release
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.respond(req)
}

// shapes is callShapes of the calls made so far.
func (l *lockedSidecar) shapes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return callShapes(l.calls)
}

// countShape is how many of shapes are want.
func countShape(shapes []string, want string) int {
	n := 0
	for _, s := range shapes {
		if s == want {
			n++
		}
	}
	return n
}

// lockedSource is a PlayerSource and TVPlayerSource for solves that run side
// by side. It counts what it is asked.
type lockedSource struct {
	mu        sync.Mutex
	tvFetches int
	removals  int
}

func (s *lockedSource) PlayerJS(string) (string, error) { return "main-js", nil }

func (s *lockedSource) RemovePlayerJS(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removals++
	return nil
}

func (s *lockedSource) TVPlayerJS(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tvFetches++
	return "tv-js", nil
}

func (s *lockedSource) counts() (tvFetches, removals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tvFetches, s.removals
}

// solveNTogether starts the first solve, waits until the sidecar holds one of
// its calls, starts the others, lets them run until they block, then
// releases the held call and returns every solve's answer. Inside a synctest
// bubble.
func solveNTogether(side *sidecarSolver, sc *lockedSidecar, solves int) []error {
	type answer struct {
		out string
		err error
	}
	answers := make(chan answer, solves)
	solve := func(ch string) {
		defer func() {
			if r := recover(); r != nil {
				answers <- answer{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		out, err := side.N(context.Background(), "P1", ch)
		if err == nil && out != "ejs("+ch+")" {
			err = fmt.Errorf("answer %q for challenge %q", out, ch)
		}
		answers <- answer{out, err}
	}
	go solve("n0")
	<-sc.started
	for i := 1; i < solves; i++ {
		go solve(fmt.Sprintf("n%d", i))
	}
	synctest.Wait()
	close(sc.release)
	errs := make([]error, 0, solves)
	for range solves {
		errs = append(errs, (<-answers).err)
	}
	return errs
}

// ejsAnswers answers every n challenge of req as the stub's ejs would.
func ejsAnswers(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	res := sidecar.SolveCipherResult{NResults: map[string]string{}}
	for _, c := range req.NChallenges {
		res.NResults[c] = "ejs(" + c + ")"
	}
	return res, nil
}

// TestSolveArrivingMidRecoveryDoesNotResend: ejs throws preparing a player's
// main body, so the sidecar holds nothing of it. One solve is inside the
// recovery's forced reload when two more arrive. They are within the window
// the recovery opened, so they ask without the JS and are told the player is
// not loaded. They used to send the body again there, each of them, outside
// the first-send gate; now they wait for the recovery and take what it found.
//
// Mutant: the not-loaded re-send made without taking the gate (one more
// JS-carrying call per arrival).
func TestSolveArrivingMidRecoveryDoesNotResend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := newLockedSidecar()
		sc.hold = func(req sidecar.SolveCipherRequest) bool { return req.ForceReload }
		sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			if req.PlayerJS == "" {
				return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
			}
			return sidecar.SolveCipherResult{}, errMainPreprocess
		}
		// No tv body: the main side alone is under test.
		side := newSidecarSolverWith(sc, &fakePlayerSource{playerID: "P1", js: "main-js"})
		for _, err := range solveNTogether(side, sc, 3) {
			if !errors.Is(err, ErrPlayerJSStale) {
				t.Errorf("solve = %v, want ErrPlayerJSStale", err)
			}
		}
		if shapes := sc.shapes(); strings.Join(shapes, " ") != "P1+js P1+js+force P1 P1" {
			t.Errorf("sidecar calls = %v, want the first send, the recovery, and one plain ask per later solve", shapes)
		}
	})
}

// refetchFailsOnce is a player source whose first fetch after an eviction
// fails in transit.
type refetchFailsOnce struct {
	*tvFakeSource
	failed bool
}

func (f *refetchFailsOnce) PlayerJS(playerID string) (string, error) {
	if f.Removed && !f.failed {
		f.failed = true
		return "", fmt.Errorf("%w: connection reset", ErrPlayerJSFetch)
	}
	return f.tvFakeSource.PlayerJS(playerID)
}

// TestRecoveryThatDidNotFinishOpensNoWindow: the sidecar holds a stale copy
// of a player, and the recovery's refetch fails in transit, so the forced
// reload never reaches the sidecar. That recovery opens no ten-minute window:
// the next solve evicts and reloads again, and is answered by the fresh main
// body. With the window kept it got the old copy's stale answer, was refused
// a recovery and went to tv, which then pinned the player to tv though main
// was never retried.
//
// Mutant: cancelEviction dropped (the second solve asks tv; no second
// eviction).
func TestRecoveryThatDidNotFinishOpensNoWindow(t *testing.T) {
	captureSlog(t)
	sc := &fakeSidecar{}
	fresh := false
	sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		if strings.HasSuffix(req.PlayerID, "-tv") {
			return ejsAnswers(req)
		}
		if req.ForceReload {
			fresh = true
		}
		if !fresh {
			return sidecar.SolveCipherResult{}, errors.New("sidecar: ejs solve n: no solutions")
		}
		return ejsAnswers(req)
	}
	src := &refetchFailsOnce{tvFakeSource: &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}}
	side := newSidecarSolverWith(sc, src)
	side.markPlayerSent("P1")
	ctx := context.Background()

	if _, err := side.N(ctx, "P1", "n1"); !errors.Is(err, ErrPlayerJSFetch) {
		t.Fatalf("first N = %v, want the refetch's failure", err)
	}
	got, err := side.N(ctx, "P1", "n2")
	if err != nil || got != "ejs(n2)" {
		t.Fatalf("second N = %q, %v; want the fresh main body's answer", got, err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1 P1+js P1+js+force" || src.removals != 2 || src.tvFetches != 0 {
		t.Errorf("sidecar calls %v, evictions %d, tv fetches %d; want a second recovery on main (2 evictions) and nothing of tv",
			shapes, src.removals, src.tvFetches)
	}
	if side.mem.usesTV("P1") {
		t.Errorf("the player was moved to tv, though its main body solves once reloaded")
	}
}

// TestBurstOfTVSolvesFetchesTheBodyOnce: three solves of a player that is
// solved with its tv body arrive together, and the sidecar does not hold
// that body. One of them fetches it and sends it; the others wait for that
// send and then ask the sidecar what it holds, whatever the send ended in.
//
//   - It solved: the waiters are answered by the loaded body.
//   - "no solutions": the body loaded but does not answer. The waiters get
//     the same from one plain ask each. Each used to fetch the 2.5 MB file
//     and send it again.
//   - ejs threw preparing it: the waiters get that verdict and ask nothing.
//
// Mutants: the send not counted, or the count not read once the gate is
// taken (a fetch and a send per waiter); tvDead not read again once the gate
// is taken (in the last row the waiters fetch and send again).
func TestBurstOfTVSolvesFetchesTheBodyOnce(t *testing.T) {
	noSolutions := errors.New("sidecar: ejs solve n: no solutions")
	for _, tc := range []struct {
		name      string
		sendErr   error // what a JS-carrying ask ends in
		loadedErr error // what the loaded body answers a plain ask with
		wantPlain int
	}{
		{"the body solves", nil, nil, 5},
		{"the body loads and has no solutions", noSolutions, noSolutions, 5},
		{"ejs cannot prepare the body", errors.New("sidecar: ejs preprocess: tv broke"), nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				captureSlog(t)
				sc := newLockedSidecar()
				sc.hold = func(req sidecar.SolveCipherRequest) bool { return req.PlayerJS != "" }
				loaded := false
				sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
					if req.PlayerID != "P1-tv" {
						return sidecar.SolveCipherResult{}, fmt.Errorf("unexpected player key %q", req.PlayerID)
					}
					if req.PlayerJS != "" {
						if isPreprocessError(tc.sendErr) {
							return sidecar.SolveCipherResult{}, tc.sendErr
						}
						loaded = true
					}
					if !loaded {
						return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
					}
					if tc.loadedErr != nil {
						return sidecar.SolveCipherResult{}, tc.loadedErr
					}
					return ejsAnswers(req)
				}
				src := &lockedSource{}
				side := newSidecarSolverWith(sc, src)
				side.mem.setTV("P1")

				for _, err := range solveNTogether(side, sc, 3) {
					if tc.sendErr == nil && err != nil {
						t.Errorf("solve = %v, want the tv body's answer", err)
					}
					if tc.sendErr != nil && !errors.Is(err, ErrPlayerJSStale) {
						t.Errorf("solve = %v, want ErrPlayerJSStale", err)
					}
				}
				shapes := sc.shapes()
				fetches, _ := src.counts()
				if sends, plain := countShape(shapes, "P1-tv+js"), countShape(shapes, "P1-tv"); fetches != 1 || sends != 1 || plain != tc.wantPlain {
					t.Errorf("tv fetches %d, sends %d, plain asks %d (%v); want 1, 1 and %d", fetches, sends, plain, shapes, tc.wantPlain)
				}
			})
		})
	}
}

// TestBurstOfFirstSolvesLeavesMainWithOneWarn: three first solves of a player
// ejs cannot prepare on main arrive together, the usual shape when a
// manifest's formats resolve at once. All three have read "not a tv player"
// before any of them finds out otherwise, and all three end on the tv body.
// The Warn that the player left main for tv is still logged once.
//
// Mutant: the Warn logged by every solve that reached tv through main,
// whatever setTV answered.
func TestBurstOfFirstSolvesLeavesMainWithOneWarn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureSlog(t)
		sc := newLockedSidecar()
		sc.hold = func(sidecar.SolveCipherRequest) bool { return true }
		tvLoaded := false
		sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			if req.PlayerID != "P1-tv" {
				return sidecar.SolveCipherResult{}, errMainPreprocess
			}
			if req.PlayerJS != "" {
				tvLoaded = true
			}
			if !tvLoaded {
				return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
			}
			return ejsAnswers(req)
		}
		side := newSidecarSolverWith(sc, &lockedSource{})
		for _, err := range solveNTogether(side, sc, 3) {
			if err != nil {
				t.Errorf("solve = %v, want the tv body's answer", err)
			}
		}
		warns := warnLines(logs.String())
		if len(warns) != 1 || !strings.Contains(warns[0], "from=main") || !strings.Contains(warns[0], "to=tv") {
			t.Errorf("Warn lines = %q, want one for leaving main for tv", warns)
		}
	})
}

// TestTVPlayerThatStopsSolvingIsStale: a player remembered as a tv one whose
// tv body no longer answers. Main is known not to work, so that is all the
// sidecar has, and the solve ends in ErrPlayerJSStale — the sentinel the
// worker reads as "every format of this player is affected" (the
// challenge-free re-extraction), not as one format's fault. Main is not asked.
//
// Mutant: the tv failure returned without the sentinel.
func TestTVPlayerThatStopsSolvingIsStale(t *testing.T) {
	sc := &fakeSidecar{respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		if len(req.SigChallenges) > 0 {
			return sidecar.SolveCipherResult{}, errors.New("sidecar: ejs solve sig: no solutions")
		}
		return sidecar.SolveCipherResult{}, errors.New("sidecar: ejs solve n: no solutions")
	}}
	side := newSidecarSolverWith(sc, &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"})
	side.mem.setTV("P1")
	ctx := context.Background()
	if _, err := side.N(ctx, "P1", "n1"); !errors.Is(err, ErrPlayerJSStale) {
		t.Errorf("N = %v, want ErrPlayerJSStale", err)
	}
	if _, err := side.Sig(ctx, "P1", "s1"); !errors.Is(err, ErrPlayerJSStale) {
		t.Errorf("Sig = %v, want ErrPlayerJSStale", err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1-tv P1-tv" {
		t.Errorf("sidecar calls = %v, want two plain asks of the tv key", shapes)
	}
}

// TestGojaFallFromBatchWarnsOnce: a Batch whose n entries go to goja is a
// fall like N's, and the two share the player's one Warn.
//
// Mutant: Batch not noting the fall (no Warn until N is asked; here N comes
// second, so the count alone would not tell — the line must be there after
// the Batch).
func TestGojaFallFromBatchWarnsOnce(t *testing.T) {
	logs := captureSlog(t)
	goja := &staticSolver{n: map[string]string{"n1": "goja-n"}}
	c := newCompositeSolverWith(&staticSolver{nErr: fmt.Errorf("%w: ejs preprocess: broke", ErrPlayerJSStale)}, goja)
	ctx := context.Background()
	if _, ns, err := c.Batch(ctx, "P1", nil, []string{"n1"}); err != nil || ns["n1"] != "goja-n" {
		t.Fatalf("Batch = %v, %v; want goja's answer", ns, err)
	}
	if warns := warnLines(logs.String()); len(warns) != 1 || !strings.Contains(warns[0], "challenge=n") {
		t.Fatalf("Warn lines after the Batch = %q, want the one for the fall to goja", warns)
	}
	if _, err := c.N(ctx, "P1", "n1"); err != nil {
		t.Fatalf("N: %v", err)
	}
	if warns := warnLines(logs.String()); len(warns) != 1 {
		t.Errorf("Warn lines after the Batch and an N = %q, want still one", warns)
	}
}

// TestPlayerMemoryIsBounded: the memory holds playerMemorySize players, and
// the oldest record goes when one more is made.
func TestPlayerMemoryIsBounded(t *testing.T) {
	m := newPlayerMemory()
	for i := range playerMemorySize + 1 {
		m.setTV(fmt.Sprintf("player%d", i))
	}
	if len(m.players) != playerMemorySize || m.usesTV("player0") || !m.usesTV("player1") || !m.usesTV(fmt.Sprintf("player%d", playerMemorySize)) {
		t.Errorf("after %d players: %d records, oldest kept = %v; want %d records without the oldest",
			playerMemorySize+1, len(m.players), m.usesTV("player0"), playerMemorySize)
	}
}
