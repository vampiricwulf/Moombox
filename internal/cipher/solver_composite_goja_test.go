package cipher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Owner ruling RV-12: a signature, alone or in a batch, falls through to goja
// last, after the sidecar (its main and tv bodies), the learned sig spec and
// the sidecar's readiness grace, and the composite is the one place that
// decides that order.

// sigCountingGoja is a GojaResolver whose compiled player for id counts the
// real signatures it is asked (not the synthetic challenge sig-spec learning
// sends) and answers every signature with answer.
func sigCountingGoja(t *testing.T, id string, answer func(string) (string, error)) (*GojaResolver, *atomic.Int32) {
	t.Helper()
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	var asked atomic.Int32
	g.cacheSolvers(CacheKey(playerURLForID(id)), &Solvers{
		Sig: func(s string) (string, error) {
			if !strings.ContainsRune(s, 0) {
				asked.Add(1)
			}
			return answer(s)
		},
		N: func(s string) (string, error) { return "goja-n(" + s + ")", nil },
	})
	return g, &asked
}

// TestRoutedResolveURLAsksGojaOncePerSignature: RoutedResolveURL leaves the
// signature to the routed solver, which asks goja itself. A signature the
// sidecar fails costs goja one ask whether goja answers it or not; it used to
// cost two when goja failed too, once in the composite's place and once in
// RoutedResolveURL's own (gojaSig).
//
// Mutants: RoutedResolveURL asking the goja resolver again after the routed
// failure (two asks in the second row); the composite not asking goja (no
// answer in the first).
func TestRoutedResolveURLAsksGojaOncePerSignature(t *testing.T) {
	req := ResolveURLRequest{PlayerURL: nTestPlayerURL, StreamURL: "https://example.com/videoplayback", EncryptedSignature: "ABC", SignatureKey: "sig"}
	side := &staticSolver{sigErr: fmt.Errorf("%w: ejs solve sig: no solutions", ErrPlayerJSStale)}

	g, asked := sigCountingGoja(t, "cb017549", func(string) (string, error) { return "goja-sig", nil })
	res, err := RoutedResolveURL(context.Background(), newCompositeSolverWith(side, g), g, req)
	if err != nil || res.URL != "https://example.com/videoplayback?sig=goja-sig" || asked.Load() != 1 {
		t.Errorf("goja answering: URL %v, err %v, goja asked %d times; want goja's signature from one ask", res, err, asked.Load())
	}

	g, asked = sigCountingGoja(t, "cb017549", func(string) (string, error) { return "", errors.New("goja: no solutions") })
	_, err = RoutedResolveURL(context.Background(), newCompositeSolverWith(side, g), g, req)
	if !errors.Is(err, ErrPlayerJSStale) || asked.Load() != 1 {
		t.Errorf("goja failing too: err %v, goja asked %d times; want ErrPlayerJSStale after one ask", err, asked.Load())
	}
}

// TestRoutedNAsksGojaOncePerChallenge: an n goes to the routed solver alone,
// which asks goja itself, last. A challenge the sidecar fails costs goja one
// ask at both entry points, whether goja answers or not; it used to cost two
// when goja failed or answered with no answer, once in the composite and once
// in solveN's own compile. A sidecar answer that is no answer (the exception
// sentinel) goes on to goja inside the composite, once.
//
// Mutants: solveN asking the goja resolver again after a routed failure (two
// asks in the failing rows); the composite not checking the sidecar's answer
// (the sentinel row installs it, goja never asked); the composite not
// checking goja's (the sentinel-from-goja row resolves).
func TestRoutedNAsksGojaOncePerChallenge(t *testing.T) {
	stale := fmt.Errorf("%w: ejs solve n: no solutions", ErrPlayerJSStale)
	sentinel := "enhanced_except_AAAAAAAA_" + nChallenge
	for _, tc := range []struct {
		name    string
		side    *staticSolver
		goja    func(string) (string, error)
		wantErr bool
	}{
		{"the sidecar fails, goja answers", &staticSolver{nErr: stale}, func(string) (string, error) { return nAnswer, nil }, false},
		{"the sidecar fails, goja fails", &staticSolver{nErr: stale}, func(string) (string, error) { return "", errors.New("goja: no solutions") }, true},
		{"the sidecar fails, goja answers with the sentinel", &staticSolver{nErr: stale}, func(string) (string, error) { return sentinel, nil }, true},
		{"the sidecar answers with the sentinel, goja answers", &staticSolver{n: map[string]string{nChallenge: sentinel}}, func(string) (string, error) { return nAnswer, nil }, false},
	} {
		for _, entry := range []struct {
			name string
			call func(Solver, *GojaResolver) (string, error)
		}{
			{"RoutedDecryptNInURL", func(r Solver, g *GojaResolver) (string, error) {
				return RoutedDecryptNInURL(context.Background(), r, g, nTestPlayerURL, "https://example.com/videoplayback?n="+nChallenge)
			}},
			{"RoutedResolveURL", func(r Solver, g *GojaResolver) (string, error) {
				res, err := RoutedResolveURL(context.Background(), r, g, ResolveURLRequest{PlayerURL: nTestPlayerURL, StreamURL: "https://example.com/videoplayback?n=" + nChallenge})
				if res == nil {
					return "", err
				}
				return res.URL, err
			}},
		} {
			var asked atomic.Int32
			g := gojaWithN(t, func(s string) (string, error) {
				asked.Add(1)
				return tc.goja(s)
			})
			got, err := entry.call(newCompositeSolverWith(tc.side, g), g)
			switch {
			case asked.Load() != 1:
				t.Errorf("%s, %s: goja asked %d times, want once", tc.name, entry.name, asked.Load())
			case tc.wantErr && !errors.Is(err, ErrNUnsolved):
				t.Errorf("%s, %s = %q, %v; want ErrNUnsolved", tc.name, entry.name, got, err)
			case !tc.wantErr && (err != nil || got != "https://example.com/videoplayback?n="+nAnswer):
				t.Errorf("%s, %s = %q, %v; want goja's answer in place", tc.name, entry.name, got, err)
			}
		}
	}
}

// TestSigWaitsOutTheGraceBeforeGoja: a signature that meets an installed
// sidecar down, with no spec for it, waits out the readiness grace for the
// child (PR-R3) and goes to goja only once the grace is spent: goja is never
// asked while a sidecar that may come back is still within it.
//
// Mutants: goja asked before the wait (answered at once); goja not asked
// after it (ErrSidecarUnavailable).
func TestSigWaitsOutTheGraceBeforeGoja(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &reportingSidecar{permutingSidecar: newPermutingSidecar()}
		goja := &specGoja{staticSolver: staticSolver{sig: map[string]string{sigA: "goja-sig"}}, specs: map[string][]int{}}
		c := newCompositeSolverWith(side, goja)
		start := time.Now()
		got, err := c.Sig(context.Background(), specPlayer, sigA)
		if err != nil || got != "goja-sig" {
			t.Fatalf("Sig with the sidecar down for good = %q, %v; want goja's answer", got, err)
		}
		if waited := time.Since(start); waited < sidecarReadyGrace {
			t.Errorf("goja was asked after %v, want after the %v grace", waited, sidecarReadyGrace)
		}
		if batches, _ := side.counts(); batches != 0 {
			t.Errorf("the down sidecar was asked %d times, want none", batches)
		}
	})
}

// TestBatchSignaturesFallToGojaLast: a batch the sidecar cannot answer is
// answered in Sig's order, signature by signature: the spec where there is
// one, goja for the rest, and the n entries from goja. With no sidecar at all
// (use_sidecar = false) it is the same. A mixed batch used to fail whole as
// soon as one signature had no spec.
//
// Mutants: Batch's failed-sidecar arm returning the error for the
// signatures (first row); the no-sidecar arm doing so (second row); goja
// asked for a signature a spec answers (goja's "goja-A" in place of the
// spec's).
func TestBatchSignaturesFallToGojaLast(t *testing.T) {
	sigC := "the third signature"
	goja := &specGoja{
		staticSolver: staticSolver{
			sig: map[string]string{sigA: "goja-A", sigC: "goja-C"},
			n:   map[string]string{"N1": "goja-N1"},
		},
		specs: map[string][]int{},
	}
	want := map[string]string{sigA: scramble(sigA), sigC: "goja-C"}
	failing := newPermutingSidecar()

	// Learn sigA's length from a working sidecar first.
	c := newCompositeSolverWith(failing, goja)
	if _, _, err := c.Batch(context.Background(), specPlayer, []string{sigA}, nil); err != nil {
		t.Fatalf("learning Batch: %v", err)
	}
	failing.setDown(true)

	for name, c := range map[string]*compositeSolver{
		"sidecar failing": c,
		"no sidecar":      newCompositeSolverWith(nil, goja),
	} {
		sr, nr, err := c.Batch(context.Background(), specPlayer, []string{sigA, sigC}, []string{"N1"})
		if err != nil {
			t.Errorf("%s: Batch: %v", name, err)
			continue
		}
		for in, w := range want {
			if sr[in] != w {
				t.Errorf("%s: sig %q = %q, want %q", name, in, sr[in], w)
			}
		}
		if nr["N1"] != "goja-N1" {
			t.Errorf("%s: n = %q, want goja's", name, nr["N1"])
		}
	}
}

// TestGojaAnswerLearnsTheSigSpec: a signature goja answered teaches the
// player's spec through goja, with syntheticSigChallenge, as one the sidecar
// answered does through the sidecar. The challenge holds every character from
// chr(0) up, '%' among them, which the player takes only percent-escaped (as
// ejs hands it over), so this runs the Go port's own solver on a prepared
// player.
//
// Mutants: createSolver not escaping the signature (the challenge throws in
// the player: no spec); Sig not learning from goja's answer (no spec).
func TestGojaAnswerLearnsTheSigSpec(t *testing.T) {
	code, _, err := preprocessPlayer(synthPlayer(builderAlr))
	if err != nil {
		t.Fatalf("preprocessPlayer: %v", err)
	}
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	const id = "cb017549"
	g.cacheSolvers(CacheKey(playerURLForID(id)), solvers)
	c := newCompositeSolverWith(nil, g)

	sig := "A%" + strings.Repeat("x", 47) + "Z" // 50 characters, '%' at 37 in the challenge
	got, err := c.Sig(context.Background(), id, sig)
	if want := "Z%" + strings.Repeat("x", 47) + "A"; err != nil || got != want {
		t.Fatalf("Sig = %q, %v; want goja's %q", got, err, want)
	}
	spec, ok := g.sigSpec(id, len(sig))
	if !ok {
		t.Fatal("no sig spec learned from goja's answer")
	}
	if len(spec) != 50 || spec[0] != 49 || spec[49] != 0 || spec[1] != 1 {
		t.Errorf("spec = %v, want the first and last positions swapped", spec)
	}
}

// TestGojaLearningLeavesTheSidecarFree: goja failing the synthetic challenge
// marks nothing, so the sidecar still learns that length for the player once
// it answers a signature of it.
//
// Mutant: goja's failure kept in refused or failed (the sidecar is never
// asked the challenge).
func TestGojaLearningLeavesTheSidecarFree(t *testing.T) {
	goja := &specGoja{staticSolver: staticSolver{sig: map[string]string{sigA: "goja-A"}}, specs: map[string][]int{}}
	side := newPermutingSidecar()
	side.setDown(true)
	c := newCompositeSolverWith(side, goja)
	ctx := context.Background()
	if got, err := c.Sig(ctx, specPlayer, sigA); err != nil || got != "goja-A" {
		t.Fatalf("Sig with the sidecar failing = %q, %v; want goja's", got, err)
	}
	if _, ok := goja.sigSpec(specPlayer, len(sigA)); ok {
		t.Fatal("a spec was stored from a synthetic challenge goja could not answer")
	}
	side.setDown(false)
	if _, err := c.Sig(ctx, specPlayer, sigB); err != nil {
		t.Fatalf("Sig with the sidecar back: %v", err)
	}
	if _, ok := goja.sigSpec(specPlayer, len(sigA)); !ok {
		t.Error("the sidecar did not learn the length goja had failed to")
	}
}

// TestBatchWaitsOutTheGraceBeforeGoja: a mixed batch that meets an installed
// sidecar down, with no spec for its signature, waits out the readiness
// grace for the child (PR-R3) as Sig does, and only then goes to goja for
// the signature and the n.
//
// Mutants: Batch's not-ready arm failing the signatures with sidecarDown
// (an error); goja asked before the wait (answered at once).
func TestBatchWaitsOutTheGraceBeforeGoja(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &reportingSidecar{permutingSidecar: newPermutingSidecar()}
		goja := &specGoja{
			staticSolver: staticSolver{sig: map[string]string{sigA: "goja-A"}, n: map[string]string{"N1": "goja-N1"}},
			specs:        map[string][]int{},
		}
		c := newCompositeSolverWith(side, goja)
		start := time.Now()
		sr, nr, err := c.Batch(context.Background(), specPlayer, []string{sigA}, []string{"N1"})
		if err != nil || sr[sigA] != "goja-A" || nr["N1"] != "goja-N1" {
			t.Fatalf("Batch with the sidecar down for good = %v, %v, %v; want goja's answers", sr, nr, err)
		}
		if waited := time.Since(start); waited < sidecarReadyGrace {
			t.Errorf("goja was asked after %v, want after the %v grace", waited, sidecarReadyGrace)
		}
	})
}

// gojaCountingSigs is a goja half that counts the signatures it is asked.
type gojaCountingSigs struct {
	staticSolver
	asked atomic.Int32
}

func (g *gojaCountingSigs) Sig(ctx context.Context, p, k string) (string, error) {
	g.asked.Add(1)
	return g.staticSolver.Sig(ctx, p, k)
}

// TestGojaIsNotAskedOnceTheCallerHasGivenUp: a caller whose context has
// ended gets its own error, in Sig and in Batch, and goja is not asked: a
// compiled player answers whatever the context says, and the answer would
// be handed to a caller that has gone.
//
// Mutants: Sig's or Batch's ctx check before goja dropped (goja asked).
func TestGojaIsNotAskedOnceTheCallerHasGivenUp(t *testing.T) {
	goja := &gojaCountingSigs{staticSolver: staticSolver{sig: map[string]string{sigA: "gojaX"}}}
	side := newPermutingSidecar()
	side.setDown(true)
	c := newCompositeSolverWith(side, goja)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, c := range map[string]*compositeSolver{"sidecar failing": c, "no sidecar": newCompositeSolverWith(nil, goja)} {
		if _, _, err := c.Batch(ctx, specPlayer, []string{sigA}, nil); err == nil {
			t.Errorf("%s: Batch with an ended context: no error", name)
		}
		if _, err := c.Sig(ctx, specPlayer, sigA); err == nil {
			t.Errorf("%s: Sig with an ended context: no error", name)
		}
	}
	if n := goja.asked.Load(); n != 0 {
		t.Errorf("goja was asked %d times for a caller that had given up, want none", n)
	}
}
