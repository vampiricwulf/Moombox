package cipher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// fakeSidecar implements sidecarClient with scripted responses.
type fakeSidecar struct {
	calls   []sidecar.SolveCipherRequest
	respond func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error)
}

func (f *fakeSidecar) SolveCipher(_ context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	f.calls = append(f.calls, req)
	return f.respond(req)
}

// fakePlayerSource serves a fixed JS string for a single playerID.
type fakePlayerSource struct {
	playerID string
	js       string
}

func (f *fakePlayerSource) PlayerJS(playerID string) (string, error) {
	if playerID != f.playerID {
		return "", errors.New("unknown playerID in fake source")
	}
	return f.js, nil
}

func TestSidecarSolverSendsPlayerJSOnFirstCallOnly(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "out"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)
	ctx := context.Background()

	if _, err := s.Sig(ctx, "P1", "in"); err != nil {
		t.Fatalf("first Sig: %v", err)
	}
	if _, err := s.Sig(ctx, "P1", "in"); err != nil {
		t.Fatalf("second Sig: %v", err)
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Fatalf("call count: got %d, want %d", got, want)
	}
	if fake.calls[0].PlayerJS == "" {
		t.Errorf("first call should include PlayerJS, got empty")
	}
	if fake.calls[1].PlayerJS != "" {
		t.Errorf("second call should omit PlayerJS, got %q", fake.calls[1].PlayerJS)
	}
}

func TestSidecarSolverRetriesOnPlayerNotLoaded(t *testing.T) {
	calls := 0
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			calls++
			if calls == 1 {
				return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
			}
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "out"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)
	// Pre-mark playerID as sent so the first call would normally omit JS.
	s.markPlayerSent("P1")

	out, err := s.Sig(context.Background(), "P1", "in")
	if err != nil {
		t.Fatalf("Sig: %v", err)
	}
	if out != "out" {
		t.Errorf("Sig result: got %q, want %q", out, "out")
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Fatalf("expected one retry; got %d calls", got)
	}
	if fake.calls[0].PlayerJS != "" {
		t.Errorf("first call should have omitted PlayerJS")
	}
	if fake.calls[1].PlayerJS == "" {
		t.Errorf("retry should include PlayerJS")
	}
}

func TestSidecarSolverDoubleNotLoadedReturnsWrappedError(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)

	_, err := s.Sig(context.Background(), "P1", "in")
	if err == nil {
		t.Fatal("expected error on double ErrPlayerNotLoaded")
	}
	if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		t.Errorf("error should NOT match ErrPlayerNotLoaded sentinel anymore (it's now a permanent failure, not a recoverable cache miss): %v", err)
	}
	if !strings.Contains(err.Error(), "after retry-with-JS") {
		t.Errorf("error should mention 'after retry-with-JS' so logs distinguish from transient miss; got: %v", err)
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Errorf("expected exactly 2 calls (initial + retry); got %d", got)
	}
}
