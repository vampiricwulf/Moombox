package cipher

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// staticSolver returns canned answers per (method, key); used for both
// the sidecar and goja roles in compositeSolver tests.
type staticSolver struct {
	sig    map[string]string
	sigErr error
	n      map[string]string
	nErr   error
}

func (s *staticSolver) Sig(_ context.Context, _, key string) (string, error) {
	if s.sigErr != nil {
		return "", s.sigErr
	}
	out, ok := s.sig[key]
	if !ok {
		return "", errors.New("staticSolver: sig miss")
	}
	return out, nil
}
func (s *staticSolver) N(_ context.Context, _, key string) (string, error) {
	if s.nErr != nil {
		return "", s.nErr
	}
	out, ok := s.n[key]
	if !ok {
		return "", errors.New("staticSolver: n miss")
	}
	return out, nil
}
func (s *staticSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sigOut := map[string]string{}
	for _, k := range sigs {
		v, err := s.Sig(ctx, playerID, k)
		if err == nil {
			sigOut[k] = v
		}
	}
	nOut := map[string]string{}
	for _, k := range ns {
		v, err := s.N(ctx, playerID, k)
		if err != nil {
			return nil, nil, err
		}
		nOut[k] = v
	}
	return sigOut, nOut, nil
}

func TestCompositeSig_RoutesToSidecar(t *testing.T) {
	side := &staticSolver{sig: map[string]string{"X": "sideX"}}
	goja := &staticSolver{sig: map[string]string{"X": "gojaX"}}
	c := newCompositeSolverWith(side, goja)

	got, err := c.Sig(context.Background(), "P1", "X")
	if err != nil {
		t.Fatalf("Sig: %v", err)
	}
	if got != "sideX" {
		t.Errorf("Sig: got %q, want sideX (sidecar)", got)
	}
}

func TestCompositeSig_DoesNotFallBackToGoja(t *testing.T) {
	side := &staticSolver{sigErr: errors.New("sidecar down")}
	goja := &staticSolver{sig: map[string]string{"X": "gojaX"}}
	c := newCompositeSolverWith(side, goja)

	if _, err := c.Sig(context.Background(), "P1", "X"); err == nil {
		t.Fatalf("expected error when sidecar fails sig; got nil")
	}
}

func TestCompositeN_FallsBackToGojaOnSidecarError(t *testing.T) {
	side := &staticSolver{nErr: errors.New("sidecar down")}
	goja := &staticSolver{n: map[string]string{"Y": "gojaY"}}
	c := newCompositeSolverWith(side, goja)

	got, err := c.N(context.Background(), "P1", "Y")
	if err != nil {
		t.Fatalf("N: %v", err)
	}
	if got != "gojaY" {
		t.Errorf("N: got %q, want gojaY (goja fallback)", got)
	}
}

func TestCompositeN_PrefersSidecar(t *testing.T) {
	side := &staticSolver{n: map[string]string{"Y": "sideY"}}
	goja := &staticSolver{n: map[string]string{"Y": "gojaY"}}
	c := newCompositeSolverWith(side, goja)

	got, err := c.N(context.Background(), "P1", "Y")
	if err != nil {
		t.Fatalf("N: %v", err)
	}
	if got != "sideY" {
		t.Errorf("N: got %q, want sideY (sidecar primary)", got)
	}
}

func TestCompositeWithoutSidecar_GojaOnly(t *testing.T) {
	goja := &staticSolver{
		sig: map[string]string{"X": "gojaX"},
		n:   map[string]string{"Y": "gojaY"},
	}
	c := newCompositeSolverWith(nil, goja)

	if _, err := c.Sig(context.Background(), "P1", "X"); err == nil {
		t.Errorf("expected sig to fail when sidecar absent; got nil")
	}
	got, err := c.N(context.Background(), "P1", "Y")
	if err != nil {
		t.Fatalf("N goja-only: %v", err)
	}
	if got != "gojaY" {
		t.Errorf("N goja-only: got %q, want gojaY", got)
	}
}

func TestCompositeBatch_SigOnlyRoutesToSidecar(t *testing.T) {
	side := &staticSolver{sig: map[string]string{"X": "sideX", "Y": "sideY"}}
	goja := &staticSolver{sig: map[string]string{"X": "gojaX", "Y": "gojaY"}}
	c := newCompositeSolverWith(side, goja)

	sigs, ns, err := c.Batch(context.Background(), "P1", []string{"X", "Y"}, nil)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(ns) != 0 {
		t.Errorf("expected empty n results; got %v", ns)
	}
	if sigs["X"] != "sideX" || sigs["Y"] != "sideY" {
		t.Errorf("expected sidecar values; got %v", sigs)
	}
}

func TestCompositeBatch_NOnlyFallsBackToGojaOnSidecarError(t *testing.T) {
	side := &staticSolver{nErr: errors.New("sidecar n down")}
	goja := &staticSolver{n: map[string]string{"A": "gojaA", "B": "gojaB"}}
	c := newCompositeSolverWith(side, goja)

	_, ns, err := c.Batch(context.Background(), "P1", nil, []string{"A", "B"})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if ns["A"] != "gojaA" || ns["B"] != "gojaB" {
		t.Errorf("expected goja fallback values; got %v", ns)
	}
}

func TestCompositeBatch_MixedAllSidecarHealthy(t *testing.T) {
	side := &staticSolver{
		sig: map[string]string{"X": "sideX"},
		n:   map[string]string{"A": "sideA"},
	}
	goja := &staticSolver{}
	c := newCompositeSolverWith(side, goja)

	sigs, ns, err := c.Batch(context.Background(), "P1", []string{"X"}, []string{"A"})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if sigs["X"] != "sideX" {
		t.Errorf("expected sidecar sig; got %v", sigs)
	}
	if ns["A"] != "sideA" {
		t.Errorf("expected sidecar n; got %v", ns)
	}
}

func TestCompositeBatchSigPartialFromSidecar(t *testing.T) {
	// Sidecar returns A but not B. Use a custom fake to drive that.
	side := &partialBatchSolver{sigs: map[string]string{"A": "sideA"}}
	goja := &staticSolver{}
	c := newCompositeSolverWith(side, goja)

	_, _, err := c.Batch(context.Background(), "P1", []string{"A", "B"}, nil)
	if err == nil {
		t.Fatal("expected error on partial sig batch (composite sig has no fallback)")
	}
}

// partialBatchSolver: helper for the partial-response test. Returns only
// the sigs/ns it knows about — no error, just gaps.
type partialBatchSolver struct {
	sigs map[string]string
	ns   map[string]string
}

func (p *partialBatchSolver) Sig(_ context.Context, _, k string) (string, error) {
	return p.sigs[k], nil
}
func (p *partialBatchSolver) N(_ context.Context, _, k string) (string, error) {
	return p.ns[k], nil
}
func (p *partialBatchSolver) Batch(_ context.Context, _ string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sr := map[string]string{}
	for _, k := range sigs {
		if v, ok := p.sigs[k]; ok {
			sr[k] = v
		}
	}
	nr := map[string]string{}
	for _, k := range ns {
		if v, ok := p.ns[k]; ok {
			nr[k] = v
		}
	}
	// Mimic inner sidecarSolver completeness validation: return an error if
	// any requested key is absent from the result map. The composite no longer
	// re-validates completeness itself (M3 cleanup); the inner solver is the
	// single source of that check.
	for _, k := range sigs {
		if _, ok := sr[k]; !ok {
			return nil, nil, fmt.Errorf("partialBatchSolver: missing sig result for %q", k)
		}
	}
	for _, k := range ns {
		if _, ok := nr[k]; !ok {
			return nil, nil, fmt.Errorf("partialBatchSolver: missing n result for %q", k)
		}
	}
	return sr, nr, nil
}

func TestCompositeBatch_MixedSidecarFailsEntireBatch(t *testing.T) {
	// With the single-round-trip design, a mixed batch (sigs+ns) is issued
	// as one sidecar call. If the sidecar fails and sigs were requested
	// there is no fallback — the entire batch fails. The previous two-call
	// design allowed goja to pick up n even when sidecar n failed, but only
	// if sig had already succeeded in the first call. The new design accepts
	// that tradeoff: one round-trip instead of two, hard failure on sidecar
	// error when sigs are in the batch.
	side := &staticSolver{
		sig:  map[string]string{"X": "sideX"},
		nErr: errors.New("sidecar n down"),
	}
	goja := &staticSolver{
		n: map[string]string{"A": "gojaA", "B": "gojaB"},
	}
	c := newCompositeSolverWith(side, goja)

	_, _, err := c.Batch(context.Background(), "P1", []string{"X"}, []string{"A", "B"})
	if err == nil {
		t.Fatal("expected error: sidecar failed on mixed batch and sig has no fallback")
	}
}

func TestCompositeBatch_MixedSinglesidecarRoundTrip(t *testing.T) {
	// Wrap staticSolver to count Batch calls.
	callCount := 0
	side := &countingBatchSolver{
		inner: &staticSolver{
			sig: map[string]string{"X": "sideX"},
			n:   map[string]string{"A": "sideA"},
		},
		onBatch: func() { callCount++ },
	}
	goja := &staticSolver{}
	c := newCompositeSolverWith(side, goja)

	_, _, err := c.Batch(context.Background(), "P1", []string{"X"}, []string{"A"})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 sidecar Batch call for mixed batch; got %d", callCount)
	}
}

type countingBatchSolver struct {
	inner   Solver
	onBatch func()
}

func (c *countingBatchSolver) Sig(ctx context.Context, p, k string) (string, error) {
	return c.inner.Sig(ctx, p, k)
}
func (c *countingBatchSolver) N(ctx context.Context, p, k string) (string, error) {
	return c.inner.N(ctx, p, k)
}
func (c *countingBatchSolver) Batch(ctx context.Context, p string, sigs, ns []string) (map[string]string, map[string]string, error) {
	c.onBatch()
	return c.inner.Batch(ctx, p, sigs, ns)
}
