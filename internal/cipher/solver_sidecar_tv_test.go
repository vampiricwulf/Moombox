package cipher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
)

// tvFakeSource is a PlayerSource that can also yield a tv body
// (TVPlayerSource). Its embedded fakePlayerSource refuses any player id but
// its own, so a "<id>-tv" key reaching the source fails the solve.
type tvFakeSource struct {
	*fakePlayerSource
	tvJS      string
	tvErr     error
	tvFetches int
	removals  int
}

func (f *tvFakeSource) TVPlayerJS(_ context.Context, playerID string) (string, error) {
	if playerID != f.playerID {
		return "", errors.New("unknown playerID in fake tv source")
	}
	f.tvFetches++
	return f.tvJS, f.tvErr
}

func (f *tvFakeSource) RemovePlayerJS(playerID string) error {
	f.removals++
	return f.fakePlayerSource.RemovePlayerJS(playerID)
}

// errMainPreprocess is the 2026-11 failure as the sidecar labels it: ejs
// throwing while it prepares the main body.
var errMainPreprocess = errors.New("sidecar: ejs preprocess: a.addOnDisposeCallback is not a function")

// variantSidecar answers like a sidecar whose ejs throws on the main body of
// every player. The tv body ("<id>-tv") is not loaded until it is sent, and
// then solves, unless tvErr is set.
type variantSidecar struct {
	fakeSidecar
	tvLoaded bool
	tvErr    error
}

func newVariantSidecar() *variantSidecar {
	v := &variantSidecar{}
	v.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		if !strings.HasSuffix(req.PlayerID, "-tv") {
			return sidecar.SolveCipherResult{}, errMainPreprocess
		}
		if req.PlayerJS != "" {
			if v.tvErr != nil {
				return sidecar.SolveCipherResult{}, v.tvErr
			}
			v.tvLoaded = true
		}
		if !v.tvLoaded {
			return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
		}
		res := sidecar.SolveCipherResult{SigResults: map[string]string{}, NResults: map[string]string{}}
		for _, c := range req.SigChallenges {
			res.SigResults[c] = "tv-sig(" + c + ")"
		}
		for _, c := range req.NChallenges {
			res.NResults[c] = "tv-n(" + c + ")"
		}
		return res, nil
	}
	return v
}

// callShapes is the sidecar calls made, as "<player key>[+js][+force]".
func callShapes(calls []sidecar.SolveCipherRequest) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		s := c.PlayerID
		if c.PlayerJS != "" {
			s += "+js"
		}
		if c.ForceReload {
			s += "+force"
		}
		out = append(out, s)
	}
	return out
}

// countingGoja is the goja half of a composite: it counts what it is asked.
type countingGoja struct {
	staticSolver
	nCalls int
}

func (g *countingGoja) N(ctx context.Context, playerID, key string) (string, error) {
	g.nCalls++
	return g.staticSolver.N(ctx, playerID, key)
}

func warnLines(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

// TestMainFailsTVSolves: ejs throws preparing the main body (the 2026-11
// shape), and the solve is answered from the tv body of the same player —
// owner ruling RV-11, "stay on main, retry tv, then goja".
//
// The first solve asks main, evicts and asks main again with fresh JS (the
// stale-JS recovery), then asks tv: without the JS first, then with the body
// from the tv source. Exactly one Warn names both variants. The second solve
// asks tv alone, and so does the sig spec probe. Goja is never asked, and the
// tv key never reaches the PlayerSource.
//
// Mutants: the tv retry removed (goja answers); useTV not remembered (the
// second solve asks main again and evicts again); the tv body sent under the
// main key; the Warn made on every solve; probeSigs left on main.
func TestMainFailsTVSolves(t *testing.T) {
	logs := captureSlog(t)
	sc := newVariantSidecar()
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}
	side := newSidecarSolverWith(sc, src)
	goja := &countingGoja{staticSolver: staticSolver{n: map[string]string{"n1": "goja-n", "n2": "goja-n"}}}
	c := newCompositeSolverWith(side, goja)
	ctx := context.Background()

	got, err := c.N(ctx, "P1", "n1")
	if err != nil || got != "tv-n(n1)" {
		t.Fatalf("first N = %q, %v; want the tv body's answer", got, err)
	}
	want := []string{"P1+js", "P1+js+force", "P1-tv", "P1-tv+js"}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != strings.Join(want, " ") {
		t.Fatalf("sidecar calls of the first solve = %v, want %v", shapes, want)
	}
	if sc.calls[3].PlayerJS != "tv-js" {
		t.Errorf("the tv key was sent %q, want the tv source's body", sc.calls[3].PlayerJS)
	}
	if src.tvFetches != 1 || src.removals != 1 {
		t.Errorf("tv fetches = %d, evictions = %d; want 1 and 1", src.tvFetches, src.removals)
	}

	sc.calls = nil
	got, err = c.N(ctx, "P1", "n2")
	if err != nil || got != "tv-n(n2)" {
		t.Fatalf("second N = %q, %v; want the tv body's answer", got, err)
	}
	sig, err := c.Sig(ctx, "P1", "s1")
	if err != nil || sig != "tv-sig(s1)" {
		t.Fatalf("Sig = %q, %v; want the tv body's answer", sig, err)
	}
	if _, err := side.probeSigs(ctx, "P1", []string{"probe"}); err != nil {
		t.Fatalf("probeSigs: %v", err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1-tv P1-tv P1-tv" {
		t.Errorf("sidecar calls once tv is remembered = %v, want three plain asks of the tv key", shapes)
	}
	if src.tvFetches != 1 || src.removals != 1 {
		t.Errorf("after the later solves: tv fetches = %d, evictions = %d; want still 1 and 1", src.tvFetches, src.removals)
	}
	if goja.nCalls != 0 {
		t.Errorf("goja was asked %d times; the sidecar answered every solve", goja.nCalls)
	}
	warns := warnLines(logs.String())
	if len(warns) != 1 || !strings.Contains(warns[0], "from=main") || !strings.Contains(warns[0], "to=tv") ||
		!strings.Contains(warns[0], "addOnDisposeCallback") || !strings.Contains(warns[0], "playerID=P1") {
		t.Errorf("Warn lines = %q, want one naming the player, both variants and the error", warns)
	}
}

// TestMainAndTVFailGojaIsLast: ejs throws on both bodies. n is answered by
// goja, asked once and only after the sidecar has been asked on main and on
// tv; a signature fails with ErrPlayerJSStale, naming the tv failure too.
// One Warn in all: the fall to goja. Leaving main for a tv body that did not
// solve is not announced.
//
// Mutants: goja asked before the tv body; the tv failure dropping the
// ErrPlayerJSStale sentinel; the tv Warn made before tv has solved.
func TestMainAndTVFailGojaIsLast(t *testing.T) {
	logs := captureSlog(t)
	sc := newVariantSidecar()
	sc.tvErr = errors.New("sidecar: ejs preprocess: tv broke too")
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}
	goja := &countingGoja{staticSolver: staticSolver{n: map[string]string{"n1": "goja-n"}}}
	asked := 0
	gojaAfter := -1
	c := newCompositeSolverWith(newSidecarSolverWith(sc, src), &orderedGoja{countingGoja: goja, before: func() {
		gojaAfter = len(sc.calls)
		asked++
	}})
	ctx := context.Background()

	got, err := c.N(ctx, "P1", "n1")
	if err != nil || got != "goja-n" {
		t.Fatalf("N = %q, %v; want goja's answer", got, err)
	}
	if shapes := callShapes(sc.calls); asked != 1 || gojaAfter != len(sc.calls) ||
		strings.Join(shapes, " ") != "P1+js P1+js+force P1-tv P1-tv+js" {
		t.Errorf("goja asked %d time(s), after %d of the sidecar's calls %v; want once, after main, main again and tv", asked, gojaAfter, shapes)
	}

	_, err = c.Sig(ctx, "P1", "s1")
	if !errors.Is(err, ErrPlayerJSStale) || !strings.Contains(err.Error(), "tv broke too") ||
		!strings.Contains(err.Error(), "addOnDisposeCallback") {
		t.Errorf("Sig error = %v, want ErrPlayerJSStale naming the main and the tv failure", err)
	}
	if warns := warnLines(logs.String()); len(warns) != 1 || !strings.Contains(warns[0], "goja") {
		t.Errorf("Warn lines = %q, want the one for the fall to goja", warns)
	}
}

// TestLostPlayerThatCannotBePreparedGoesToTV: the sidecar has lost a player
// Go believes it sent (a restart, an eviction), and ejs throws preparing the
// body when it is sent again. That failure used to be returned as it came,
// short of the stale-JS recovery; it now runs the recovery and then the tv
// body, like the same failure on a first send.
//
// Mutant: the re-send's failure returned without the recovery (goja answers).
func TestLostPlayerThatCannotBePreparedGoesToTV(t *testing.T) {
	captureSlog(t)
	sc := newVariantSidecar()
	inner := sc.respond
	sc.respond = func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
		if req.PlayerID == "P1" && req.PlayerJS == "" {
			return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
		}
		return inner(req)
	}
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}
	side := newSidecarSolverWith(sc, src)
	side.markPlayerSent("P1")

	got, err := side.N(context.Background(), "P1", "n1")
	if err != nil || got != "tv-n(n1)" {
		t.Fatalf("N = %q, %v; want the tv body's answer", got, err)
	}
	if shapes := callShapes(sc.calls); strings.Join(shapes, " ") != "P1 P1+js P1+js+force P1-tv P1-tv+js" {
		t.Errorf("sidecar calls = %v, want the plain ask, the re-send, the recovery and tv", shapes)
	}
}

// orderedGoja runs before ahead of each n it is asked.
type orderedGoja struct {
	*countingGoja
	before func()
}

func (g *orderedGoja) N(ctx context.Context, playerID, key string) (string, error) {
	g.before()
	return g.countingGoja.N(ctx, playerID, key)
}

// TestTVMemorySurvivesASidecarRestart: the supervisor installs a newly built
// sidecar half after every restart. Which players are solved with the tv
// body is the composite's to remember, so the new half asks tv at once.
//
// Mutant: sharePlayerMemory dropped from SetSidecar (the new half asks main,
// evicts and Warns again).
func TestTVMemorySurvivesASidecarRestart(t *testing.T) {
	logs := captureSlog(t)
	src := &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1", js: "main-js"}, tvJS: "tv-js"}
	c := newCompositeSolverWith(newSidecarSolverWith(newVariantSidecar(), src), nil)
	ctx := context.Background()
	if _, err := c.N(ctx, "P1", "n1"); err != nil {
		t.Fatalf("N before the restart: %v", err)
	}

	restarted := newVariantSidecar()
	c.SetSidecar(newSidecarSolverWith(restarted, src))
	if got, err := c.N(ctx, "P1", "n2"); err != nil || got != "tv-n(n2)" {
		t.Fatalf("N after the restart = %q, %v", got, err)
	}
	if shapes := callShapes(restarted.calls); strings.Join(shapes, " ") != "P1-tv P1-tv+js" {
		t.Errorf("the new sidecar was asked %v, want the tv key alone", shapes)
	}
	if src.removals != 1 {
		t.Errorf("evictions = %d, want the one from before the restart", src.removals)
	}
	if warns := warnLines(logs.String()); len(warns) != 1 {
		t.Errorf("Warn lines = %q, want the one from before the restart", warns)
	}
}

// TestTVFetchNeverTouchesThePlayerCache: the main and tv files of one player
// carry one Last-Modified and no ETag, and PlayerCache keys a player by its
// id alone. A tv fetch through it would send the main body's validator, be
// answered 304 and hand back the cached main body. The tv fetch therefore
// goes around the cache: it sends no validator, returns the tv body, and
// leaves the cache directory byte for byte as it was — with a cached main
// body, and with none.
//
// Mutants: TVPlayerJS fetching through playerCache.Fetch (the main body comes
// back, or the tv body lands in the cache); the tv fetch replaying
// If-Modified-Since.
func TestTVFetchNeverTouchesThePlayerCache(t *testing.T) {
	const (
		mainBody     = "// main variant body"
		tvBody       = "// tv variant body, of another length"
		lastModified = "Mon, 23 Nov 2026 05:32:19 GMT"
		tvPath       = "/s/player/" + testPlayerID + "/tv-player-ias.vflset/tv-player-ias.js"
	)
	var mu sync.Mutex
	var tvValidators []string
	orig := playerHTTPClient
	playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tvPath {
			mu.Lock()
			tvValidators = append(tvValidators, r.Header.Get("If-Modified-Since")+r.Header.Get("If-None-Match"))
			mu.Unlock()
		}
		w.Header().Set("Last-Modified", lastModified)
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.URL.Path == tvPath {
			_, _ = w.Write([]byte(tvBody))
			return
		}
		_, _ = w.Write([]byte(mainBody))
	}))
	defer func() { playerHTTPClient = orig }()

	dir := t.TempDir()
	g, err := NewGojaResolver(dir, &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	ctx := context.Background()
	fetchTV := func(when string) {
		t.Helper()
		before := dirSnapshot(t, dir)
		js, err := g.TVPlayerJS(ctx, testPlayerID)
		if err != nil || js != tvBody {
			t.Fatalf("%s: TVPlayerJS = %q, %v; want the tv body", when, js, err)
		}
		if after := dirSnapshot(t, dir); after != before {
			t.Errorf("%s: the tv fetch changed the cache directory:\nbefore: %s\nafter:  %s", when, before, after)
		}
	}

	fetchTV("cold cache")
	if js, err := g.PlayerJS(testPlayerID); err != nil || js != mainBody {
		t.Fatalf("PlayerJS = %q, %v; want the main body", js, err)
	}
	fetchTV("main cached")
	if js, err := g.PlayerJS(testPlayerID); err != nil || js != mainBody {
		t.Errorf("PlayerJS after the tv fetch = %q, %v; want the main body still", js, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tvValidators) != 2 || tvValidators[0] != "" || tvValidators[1] != "" {
		t.Errorf("validators sent with the tv fetches = %q, want two fetches and none", tvValidators)
	}
}

// dirSnapshot is every file under dir with its content, in walk order.
func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		b.WriteString(rel + "=" + string(data) + ";")
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return b.String()
}

// TestTVFetchRefusesANonPlayerID: a player id that is not one (the URL-hash
// fallback of PlayerIDFromURL) names no tv file, and no request is made.
func TestTVFetchRefusesANonPlayerID(t *testing.T) {
	orig := playerHTTPClient
	requests := 0
	playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer func() { playerHTTPClient = orig }()
	if _, err := fetchTVPlayerJS(context.Background(), "../../watch"); !errors.Is(err, ErrPlayerJSFetch) || requests != 0 {
		t.Errorf("fetchTVPlayerJS of a non-id = %v after %d request(s); want ErrPlayerJSFetch and no request", err, requests)
	}
}

// TestGojaAnswersTheSigAloneTheNStaysRouted: a signature the routed solver
// could not answer is goja's to try, last. The n of the same URL is still
// the routed solver's first: it used to go to goja with the signature, in
// one gojaResolver.ResolveURL, without the sidecar having been asked for it.
//
// Mutant: the sig failure handing the whole URL back to
// gojaResolver.ResolveURL (the URL carries goja's n).
func TestGojaAnswersTheSigAloneTheNStaysRouted(t *testing.T) {
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	g.cacheSolvers(CacheKey(nTestPlayerURL), &Solvers{
		Sig: func(s string) (string, error) { return "goja-sig", nil },
		N:   func(s string) (string, error) { return "goja-n", nil },
	})
	routed := &staticSolver{sigErr: ErrPlayerJSStale, n: map[string]string{"rawnvalue": "routed-n"}}
	res, err := RoutedResolveURL(context.Background(), routed, g, ResolveURLRequest{
		PlayerURL:          nTestPlayerURL,
		StreamURL:          "https://example.com/videoplayback?n=rawnvalue",
		EncryptedSignature: "ABCDEF",
		SignatureKey:       "sig",
	})
	if err != nil {
		t.Fatalf("RoutedResolveURL: %v", err)
	}
	if res.URL != "https://example.com/videoplayback?n=routed-n&sig=goja-sig" {
		t.Errorf("URL = %q, want goja's signature and the routed solver's n", res.URL)
	}
}

// mainFetchFails is a player source whose main body cannot be fetched.
type mainFetchFails struct {
	*tvFakeSource
	err error
}

func (f *mainFetchFails) PlayerJS(string) (string, error) { return "", f.err }

// TestMainFileGoneGoesToTV: the main body's fetch gets a definite answer that
// the file is not there. Goja reads the main body only, so it has nothing
// either; the owner's order is main, tv, goja, and the tv body is asked
// before goja, with the same single Warn. A fetch that failed in transit says
// nothing about the player: tv is not asked for it, and goja is next.
//
// Mutants: only ErrPlayerJSStale sending a solve on to tv (goja answers the
// first row); every fetch failure sending it on (tv answers the second).
func TestMainFileGoneGoesToTV(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fetchErr  error
		want      string
		wantWarns int
	}{
		{"not there", fmt.Errorf("%w: %w", ErrPlayerJSFetch, playerFetchStatus(404)), "tv-n(n1)", 1},
		{"failed in transit", fmt.Errorf("%w: connection reset", ErrPlayerJSFetch), "goja-n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			sc := newVariantSidecar()
			src := &mainFetchFails{
				tvFakeSource: &tvFakeSource{fakePlayerSource: &fakePlayerSource{playerID: "P1"}, tvJS: "tv-js"},
				err:          tc.fetchErr,
			}
			goja := &countingGoja{staticSolver: staticSolver{n: map[string]string{"n1": "goja-n"}}}
			c := newCompositeSolverWith(newSidecarSolverWith(sc, src), goja)

			got, err := c.N(context.Background(), "P1", "n1")
			if err != nil || got != tc.want {
				t.Fatalf("N = %q, %v; want %q", got, err, tc.want)
			}
			warns := warnLines(logs.String())
			if len(warns) != tc.wantWarns {
				t.Fatalf("Warn lines = %q, want %d", warns, tc.wantWarns)
			}
			if tc.wantWarns == 1 && (!strings.Contains(warns[0], "from=main") || !strings.Contains(warns[0], "to=tv") || !strings.Contains(warns[0], "status 404")) {
				t.Errorf("Warn = %q, want the move from main to tv with the fetch's answer", warns[0])
			}
		})
	}
}
