package cipher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// permutingSidecar stands in for the sidecar's sig solving with a transform
// of the shape real players use: it moves characters and drops some, and
// never reads what they are, so the synthetic challenge learns it exactly.
// down makes every call fail, as a dead or restarting sidecar does.
type permutingSidecar struct {
	mu        sync.Mutex
	down      bool
	err       error
	transform func(string) string
	// batches counts Batch calls; synthetic counts the synthetic challenges
	// they carried.
	batches   int
	synthetic int
}

// scramble reverses, swaps two positions and drops the first character.
func scramble(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	if len(r) > 4 {
		r[1], r[4] = r[4], r[1]
	}
	return string(r[1:])
}

func newPermutingSidecar() *permutingSidecar {
	return &permutingSidecar{transform: scramble}
}

func (p *permutingSidecar) failing() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		if p.err != nil {
			return p.err
		}
		return errors.New("sidecar: stdout EOF")
	}
	return nil
}

func (p *permutingSidecar) Sig(_ context.Context, _, s string) (string, error) {
	if err := p.failing(); err != nil {
		return "", err
	}
	return p.transform(s), nil
}

func (p *permutingSidecar) N(_ context.Context, _, n string) (string, error) {
	if err := p.failing(); err != nil {
		return "", err
	}
	return "n-" + n, nil
}

func (p *permutingSidecar) Batch(_ context.Context, _ string, sigs, ns []string) (map[string]string, map[string]string, error) {
	p.mu.Lock()
	p.batches++
	for _, s := range sigs {
		if strings.ContainsRune(s, 0) {
			p.synthetic++
		}
	}
	p.mu.Unlock()
	if err := p.failing(); err != nil {
		return nil, nil, err
	}
	sr := make(map[string]string, len(sigs))
	for _, s := range sigs {
		sr[s] = p.transform(s)
	}
	nr := make(map[string]string, len(ns))
	for _, n := range ns {
		nr[n] = "n-" + n
	}
	return sr, nr, nil
}

func (p *permutingSidecar) setDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

func (p *permutingSidecar) counts() (batches, synthetic int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batches, p.synthetic
}

const (
	specPlayer = "abcd1234"
	sigA       = "AOq0QJ8wRAIgXyZ-0123456789_abcdefghij" // 37 characters
	sigB       = "AOq0QJ8wRgIhAPq-9876543210_jihgfedcba" // same length, other content
)

func newSpecResolver(t *testing.T, dir string) *GojaResolver {
	t.Helper()
	g, err := NewGojaResolver(dir, &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	return g
}

// The composite's spec store is the goja resolver it is built with: that is
// what puts the specs beside the player cache and drops them with it.
//
// Mutant: newCompositeSolverWith not setting specs.
func TestCompositeSolverKeepsSigSpecsInTheGojaResolver(t *testing.T) {
	g := newSpecResolver(t, t.TempDir())
	c, ok := NewCompositeSolver(nil, g).(*compositeSolver)
	if !ok {
		t.Fatalf("NewCompositeSolver is not a *compositeSolver")
	}
	if c.specs != sigSpecStore(g) {
		t.Fatalf("composite specs = %v, want the goja resolver it was built with", c.specs)
	}
}

// Audit G49. The first signature the sidecar answers for a player teaches
// the composite that player's spec for its length (yt-dlp's
// chr(0..L-1) challenge, _video.py:3311-3334); while the sidecar is then
// down or gone, another signature of that length is answered from the spec,
// and the answer is the sidecar's own. A length never seen is still refused.
//
// Mutants: learnSigSpecs returning at once (nothing learned: the outage
// refuses sigB); Sig's error arm not consulting the spec; Sig's nil-sidecar
// arm not consulting it.
func TestASigSpecAnswersSignaturesWhileTheSidecarIsDown(t *testing.T) {
	g := newSpecResolver(t, t.TempDir())
	side := newPermutingSidecar()
	c := newCompositeSolverWith(side, g)
	ctx := context.Background()

	got, err := c.Sig(ctx, specPlayer, sigA)
	if err != nil || got != scramble(sigA) {
		t.Fatalf("Sig(sigA) with the sidecar up = %q, %v; want %q", got, err, scramble(sigA))
	}
	if _, synth := side.counts(); synth != 1 {
		t.Fatalf("synthetic challenges asked = %d, want 1", synth)
	}

	side.setDown(true)
	got, err = c.Sig(ctx, specPlayer, sigB)
	if err != nil || got != scramble(sigB) {
		t.Fatalf("Sig(sigB) with the sidecar failing = %q, %v; want %q from the spec", got, err, scramble(sigB))
	}

	c.SetSidecar(nil)
	got, err = c.Sig(ctx, specPlayer, sigB)
	if err != nil || got != scramble(sigB) {
		t.Fatalf("Sig(sigB) with no sidecar = %q, %v; want %q from the spec", got, err, scramble(sigB))
	}
	if _, err := c.Sig(ctx, specPlayer, sigB+"x"); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Sig of an unlearned length with no sidecar: err = %v, want ErrSidecarUnavailable", err)
	}
	if _, err := c.Sig(ctx, "other999", sigB); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Sig for another player with no sidecar: err = %v, want ErrSidecarUnavailable", err)
	}
}

// Learned once per player and length: later signatures of a learned length
// add no synthetic challenge.
//
// Mutant: learnSigSpecs skipping the stored-spec check (every signature asks
// the synthetic challenge again).
func TestASigSpecIsLearnedOncePerLength(t *testing.T) {
	g := newSpecResolver(t, t.TempDir())
	side := newPermutingSidecar()
	c := newCompositeSolverWith(side, g)
	ctx := context.Background()
	for _, s := range []string{sigA, sigB, sigA} {
		if _, err := c.Sig(ctx, specPlayer, s); err != nil {
			t.Fatalf("Sig: %v", err)
		}
	}
	if _, synth := side.counts(); synth != 1 {
		t.Fatalf("synthetic challenges asked = %d, want 1 for one length", synth)
	}
}

// The spec is on disk: a restarted process (a fresh resolver on the same
// cache dir) answers from it with the sidecar off.
//
// Mutant: PutSigSpec not writing the file.
func TestASigSpecSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	c1 := newCompositeSolverWith(newPermutingSidecar(), newSpecResolver(t, dir))
	if _, err := c1.Sig(context.Background(), specPlayer, sigA); err != nil {
		t.Fatalf("Sig: %v", err)
	}

	c2 := newCompositeSolverWith(nil, newSpecResolver(t, dir))
	got, err := c2.Sig(context.Background(), specPlayer, sigB)
	if err != nil || got != scramble(sigB) {
		t.Fatalf("Sig after a restart with no sidecar = %q, %v; want %q", got, err, scramble(sigB))
	}
}

// InvalidateSolver and RemovePlayerJS, which the sidecar's stale-JS recovery
// calls, drop the player's specs with the rest of its cache.
//
// Mutant: PlayerCache.Remove leaving the sigspec file.
func TestInvalidatingAPlayerDropsItsSigSpecs(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(g *GojaResolver)
	}{
		{"InvalidateSolver", func(g *GojaResolver) { g.InvalidateSolver(playerURLForID(specPlayer)) }},
		{"RemovePlayerJS", func(g *GojaResolver) { _ = g.RemovePlayerJS(specPlayer) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newSpecResolver(t, t.TempDir())
			c := newCompositeSolverWith(newPermutingSidecar(), g)
			if _, err := c.Sig(context.Background(), specPlayer, sigA); err != nil {
				t.Fatalf("Sig: %v", err)
			}
			if _, ok := g.sigSpec(specPlayer, len(sigA)); !ok {
				t.Fatalf("no spec learned")
			}
			tc.drop(g)
			c.SetSidecar(nil)
			if _, err := c.Sig(context.Background(), specPlayer, sigB); !errors.Is(err, ErrSidecarUnavailable) {
				t.Fatalf("Sig after %s with no sidecar: err = %v, want ErrSidecarUnavailable", tc.name, err)
			}
		})
	}
}

// A synthetic answer that does not reproduce the sidecar's answer for the
// real signature is not stored, and is not asked again for that length.
// Here the transform reads the characters (it is no permutation), as a spec
// cannot capture.
//
// Mutants: the reproduction check dropped (the bad spec is stored and the
// outage answers from it); the refusal not remembered (the second signature
// asks the synthetic challenge again).
func TestASigSpecThatDoesNotReproduceTheSidecarIsNotKept(t *testing.T) {
	g := newSpecResolver(t, t.TempDir())
	side := newPermutingSidecar()
	side.transform = func(s string) string {
		if strings.ContainsRune(s, 0) {
			return scramble(s)
		}
		return strings.ToUpper(scramble(s))
	}
	c := newCompositeSolverWith(side, g)
	ctx := context.Background()
	for _, s := range []string{sigA, sigB} {
		if _, err := c.Sig(ctx, specPlayer, s); err != nil {
			t.Fatalf("Sig: %v", err)
		}
	}
	if _, synth := side.counts(); synth != 1 {
		t.Errorf("synthetic challenges asked = %d, want 1 (a refused length is not asked again)", synth)
	}
	c.SetSidecar(nil)
	if out, err := c.Sig(ctx, specPlayer, sigB); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Sig with no sidecar = %q, %v; want ErrSidecarUnavailable (no spec kept)", out, err)
	}
}

// A sidecar call that failed because the caller's context ended is the
// caller's error, surfaced as it is, not an outage for the spec to cover.
//
// Mutant: Sig's error arm not checking ctx.Err().
func TestAnEndedContextIsNotAnsweredFromTheSigSpec(t *testing.T) {
	g := newSpecResolver(t, t.TempDir())
	side := newPermutingSidecar()
	c := newCompositeSolverWith(side, g)
	if _, err := c.Sig(context.Background(), specPlayer, sigA); err != nil {
		t.Fatalf("Sig: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	side.mu.Lock()
	side.down, side.err = true, context.Canceled
	side.mu.Unlock()
	if out, err := c.Sig(ctx, specPlayer, sigB); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sig with an ended context = %q, %v; want context.Canceled", out, err)
	}
	if _, _, err := c.Batch(ctx, specPlayer, []string{sigB}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Batch with an ended context: err = %v, want context.Canceled", err)
	}
}

// specGoja is a goja half that answers n and keeps specs in memory, for the
// Batch arms.
type specGoja struct {
	staticSolver
	mu    sync.Mutex
	specs map[string][]int
}

func (s *specGoja) sigSpec(playerID string, length int) ([]int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[sigSpecKey(playerID, length)]
	return spec, ok
}

func (s *specGoja) putSigSpec(playerID string, length int, spec []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[sigSpecKey(playerID, length)] = spec
}

// Batch learns from a sidecar batch, and while the sidecar is down answers
// the sigs from the specs and the ns from goja; one sig with no spec still
// fails the batch, as before.
//
// Mutants: Batch not learning on success; the failed-sidecar arm returning
// the error with specs present; the nil-sidecar arm returning
// ErrSidecarUnavailable with specs present.
func TestBatchAnswersSigsFromSpecsWhileTheSidecarIsDown(t *testing.T) {
	goja := &specGoja{staticSolver: staticSolver{n: map[string]string{"N1": "gojaN1"}}, specs: map[string][]int{}}
	side := newPermutingSidecar()
	c := newCompositeSolverWith(side, goja)
	ctx := context.Background()

	if _, _, err := c.Batch(ctx, specPlayer, []string{sigA}, []string{"N1"}); err != nil {
		t.Fatalf("Batch with the sidecar up: %v", err)
	}

	side.setDown(true)
	sr, nr, err := c.Batch(ctx, specPlayer, []string{sigB}, []string{"N1"})
	if err != nil {
		t.Fatalf("Batch with the sidecar failing: %v", err)
	}
	if sr[sigB] != scramble(sigB) || nr["N1"] != "gojaN1" {
		t.Fatalf("Batch with the sidecar failing = %v, %v; want sig from the spec and n from goja", sr, nr)
	}
	if _, _, err := c.Batch(ctx, specPlayer, []string{sigB, sigB + "x"}, nil); err == nil {
		t.Fatalf("Batch with one sig of an unlearned length: want the sidecar's error")
	}

	c.SetSidecar(nil)
	sr, _, err = c.Batch(ctx, specPlayer, []string{sigB}, nil)
	if err != nil || sr[sigB] != scramble(sigB) {
		t.Fatalf("Batch with no sidecar = %v, %v; want sig from the spec", sr, err)
	}
	if _, _, err := c.Batch(ctx, specPlayer, []string{sigB + "x"}, nil); !errors.Is(err, ErrSidecarUnavailable) {
		t.Fatalf("Batch of an unlearned length with no sidecar: err = %v, want ErrSidecarUnavailable", err)
	}
}

// A stored spec with an index past the signature is no answer.
func TestApplySigSpecRefusesAnOutOfRangeIndex(t *testing.T) {
	if out, ok := applySigSpec([]rune("abc"), []int{2, 0, 3}); ok {
		t.Fatalf("applySigSpec with index 3 on 3 runes = %q, want refused", out)
	}
	if out, ok := applySigSpec([]rune("abc"), []int{2, 0}); !ok || out != "ca" {
		t.Fatalf("applySigSpec = %q, %v; want ca", out, ok)
	}
}
