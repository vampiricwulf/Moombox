package cipher

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	mbgoja "github.com/vampiricwulf/Moombox/internal/goja"
	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
)

const (
	hangingPlayer = `while (true) {}`
	workingPlayer = `_result.n = function(x) { return x + "_ok"; };`
)

// compileFixture is a GojaResolver whose player playerURL is on disk with the
// preprocessed script code, behind a player CDN that answers every
// revalidation with 304, so compileSolver runs code without a download.
// gets counts the CDN requests, one per compile that got past the refusal
// list. The compile bound is bound and the refusal clock is *now.
func compileFixture(t *testing.T, playerURL, code string, bound time.Duration) (s *GojaResolver, gets *atomic.Int32, now *time.Time) {
	t.Helper()
	gets = new(atomic.Int32)
	origClient, origBound, origNow := playerHTTPClient, compileTimeout, resolverNow
	playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	compileTimeout = bound
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	now = &clock
	resolverNow = func() time.Time { return *now }
	t.Cleanup(func() { playerHTTPClient, compileTimeout, resolverNow = origClient, origBound, origNow })

	s, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	if err := s.playerCache.Put(playerURL, "var player = 1;"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	setPreprocessed(t, s, playerURL, code)
	return s, gets, now
}

func setPreprocessed(t *testing.T, s *GojaResolver, playerURL, code string) {
	t.Helper()
	if err := s.playerCache.PutPreprocessed(playerURL, code); err != nil {
		t.Fatalf("PutPreprocessed: %v", err)
	}
}

// getSolvers runs GetSolvers with a 30 s backstop, so a compile left
// unbounded fails the test instead of hanging it.
func getSolvers(t *testing.T, s *GojaResolver, ctx context.Context, playerURL string) (*Solvers, error) {
	t.Helper()
	type answer struct {
		solvers *Solvers
		err     error
	}
	ch := make(chan answer, 1)
	go func() {
		sv, err := s.GetSolvers(ctx, playerURL)
		ch <- answer{sv, err}
	}()
	select {
	case a := <-ch:
		return a.solvers, a.err
	case <-time.After(30 * time.Second):
		t.Fatal("GetSolvers still running after 30 s: the compile is not bounded")
		return nil, nil
	}
}

// A player whose script never finishes under goja used to hold the compile
// slot for good, with nothing able to stop it. The compile is now bounded
// (120 s in production), and the player that ran out of it is refused for
// 10 minutes rather than run again for the full bound on every request;
// after that it is compiled again.
//
// Mutants: the compile run without a bound (the backstop fires); no refusal
// (the second request compiles the working script and succeeds); the refusal
// checked only after the fetch (gets goes up); a refusal that never ends (the
// request at +10 min is refused).
func TestGetSolversRefusesAPlayerWhoseCompileTimedOut(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/aaaa1111/player_ias.vflset/en_US/base.js"
	s, gets, now := compileFixture(t, playerURL, hangingPlayer, 100*time.Millisecond)

	if _, err := getSolvers(t, s, context.Background(), playerURL); !errors.Is(err, mbgoja.ErrExecutionTimeout) {
		t.Fatalf("first compile = %v, want ErrExecutionTimeout", err)
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("CDN requests after the first compile = %d, want 1", got)
	}

	// The script on disk now works, but the player is still refused.
	setPreprocessed(t, s, playerURL, workingPlayer)
	*now = now.Add(compileRefusal - time.Second)
	if _, err := getSolvers(t, s, context.Background(), playerURL); !errors.Is(err, ErrCompileRefused) {
		t.Fatalf("request inside the refusal = %v, want ErrCompileRefused", err)
	}
	if got := gets.Load(); got != 1 {
		t.Errorf("CDN requests after a refused request = %d, want still 1", got)
	}

	*now = now.Add(time.Second)
	solvers, err := getSolvers(t, s, context.Background(), playerURL)
	if err != nil {
		t.Fatalf("request once the refusal ended = %v, want a compile", err)
	}
	if out, err := solvers.DecryptN("x"); err != nil || out != "x_ok" {
		t.Errorf("DecryptN = %q, %v; want x_ok", out, err)
	}
}

// A caller queued behind the compile that times out is refused when it gets
// the slot, instead of running the same script for the full bound again.
//
// Mutant: the refusal checked only before the slot (the queued caller runs
// the compile and fails with ErrExecutionTimeout, after a second CDN request).
func TestGetSolversACallerQueuedBehindATimedOutCompileIsRefused(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/bbbb2222/player_ias.vflset/en_US/base.js"
	s, gets, _ := compileFixture(t, playerURL, hangingPlayer, 300*time.Millisecond)

	first := make(chan error, 1)
	go func() {
		_, err := s.GetSolvers(context.Background(), playerURL)
		first <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); gets.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the first compile never reached the CDN")
		}
		time.Sleep(time.Millisecond)
	}
	// The first caller holds the slot and is running the script.
	if _, err := getSolvers(t, s, context.Background(), playerURL); !errors.Is(err, ErrCompileRefused) {
		t.Errorf("queued caller = %v, want ErrCompileRefused", err)
	}
	if err := <-first; !errors.Is(err, mbgoja.ErrExecutionTimeout) {
		t.Errorf("first caller = %v, want ErrExecutionTimeout", err)
	}
	if got := gets.Load(); got != 1 {
		t.Errorf("CDN requests = %d, want 1", got)
	}
}

// A caller whose context ends while the compile runs does not cut the
// compile short: the compile finishes and fills the cache, so the next
// caller gets the solvers without compiling again. Callers with short
// deadlines (a 30 s probe) would otherwise each start from zero.
//
// The cancel comes from compileStarting, so it always lands after
// compileSolver's last context check and before the run. The script runs for
// 200 ms so that a run given the cancelled context is always interrupted,
// never finished first.
//
// Mutant: the caller's context passed to the run (the first call returns
// context.Canceled and the second compiles again, gets going to 2).
func TestGetSolversACallerCancelDoesNotAbortARunningCompile(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/cccc3333/player_ias.vflset/en_US/base.js"
	const slowPlayer = `var end = Date.now() + 200; while (Date.now() < end) {}
_result.n = function(x) { return x + "_ok"; };`
	s, gets, _ := compileFixture(t, playerURL, slowPlayer, 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origStarting := compileStarting
	compileStarting = cancel
	t.Cleanup(func() { compileStarting = origStarting })
	if _, err := getSolvers(t, s, ctx, playerURL); err != nil {
		t.Fatalf("compile whose caller cancelled = %v, want the solvers", err)
	}
	solvers, err := getSolvers(t, s, context.Background(), playerURL)
	if err != nil {
		t.Fatalf("next request = %v, want the cached solvers", err)
	}
	if out, err := solvers.DecryptN("x"); err != nil || out != "x_ok" {
		t.Errorf("DecryptN = %q, %v; want x_ok", out, err)
	}
	if got := gets.Load(); got != 1 {
		t.Errorf("CDN requests = %d, want 1: the next request should hit the cache", got)
	}
}

// The refusal list holds maxTimedOutPlayers players; a further one pushes out
// the one whose refusal ends first, and an ended refusal is dropped.
//
// Mutants: no cap (the list grows to maxTimedOutPlayers+1); the newest pushed
// out instead of the oldest (p0 is still refused).
func TestRefusalListIsBounded(t *testing.T) {
	origNow := resolverNow
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resolverNow = func() time.Time { return clock }
	t.Cleanup(func() { resolverNow = origNow })
	s := &GojaResolver{}

	for i := 0; i <= maxTimedOutPlayers; i++ {
		s.refuse(string(rune('a'+i)), mbgoja.ErrExecutionTimeout)
		clock = clock.Add(time.Second)
	}
	if got := len(s.timedOut); got != maxTimedOutPlayers {
		t.Errorf("refusal list holds %d, want %d", got, maxTimedOutPlayers)
	}
	if !s.refusedUntil("a").IsZero() {
		t.Error("the first player refused is still on the list; it should have been pushed out")
	}
	if s.refusedUntil(string(rune('a' + maxTimedOutPlayers))).IsZero() {
		t.Error("the newest player refused is not on the list")
	}

	clock = clock.Add(compileRefusal)
	if !s.refusedUntil("b").IsZero() {
		t.Error("a refusal past compileRefusal still holds")
	}
}

// A player whose body the port cannot prepare (ErrExtractorMismatch) is
// refused for compileRefusal, like one whose compile timed out: every later
// signature or n used to fetch and parse it again (about 0.1 s each) while
// holding the compile slot. The refusal still wraps
// ErrExtractorMismatch, and a changed body under the player's URL (the
// sidecar's fetch, PlayerJS) ends it.
//
// Mutants: no refusal (the second call fetches and prepares again, gets goes
// to 2); the refusal not wrapping the mismatch (errors.Is fails); a changed
// body not ending it (gets stays at 2 after the third call).
func TestGetSolversRefusesAPlayerThePortCannotPrepare(t *testing.T) {
	const playerID = "dddd4444"
	playerURL := playerURLForID(playerID)
	var gets atomic.Int32
	var body atomic.Value
	body.Store("")
	origClient, origNow := playerHTTPClient, resolverNow
	playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if b := body.Load().(string); b != "" {
			_, _ = w.Write([]byte(b))
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	resolverNow = func() time.Time { return clock }
	t.Cleanup(func() { playerHTTPClient, resolverNow = origClient, origNow })

	s, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	if err := s.playerCache.Put(playerURL, "var player = 1;"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if _, err := s.Sig(context.Background(), playerID, "AAAA"); !errors.Is(err, ErrExtractorMismatch) {
			t.Fatalf("Sig %d = %v, want ErrExtractorMismatch", i, err)
		}
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("CDN requests after two signatures = %d, want 1: the second was to be refused", got)
	}

	body.Store("var player = 2; var other = 3;")
	if _, err := s.PlayerJS(playerID); err != nil {
		t.Fatalf("PlayerJS: %v", err)
	}
	if !s.refusedUntil(CacheKey(playerURL)).IsZero() {
		t.Error("the refusal outlived a changed body")
	}
	_, _ = s.Sig(context.Background(), playerID, "AAAA")
	if got := gets.Load(); got != 3 {
		t.Errorf("CDN requests after the body changed = %d, want 3: the player was to be tried again", got)
	}
}
