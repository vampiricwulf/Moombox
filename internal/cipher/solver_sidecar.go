package cipher

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// sidecarClient is the subset of *sidecar.Sidecar this solver depends on.
// Defined as an interface so unit tests can inject a fake without
// spawning Node.
type sidecarClient interface {
	SolveCipher(ctx context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error)
}

// PlayerSource yields the player JS for a given playerID. The goja
// solver already maintains a per-playerID JS cache for its own
// preprocessing; PlayerSource exposes that cache so the sidecar solver
// can reuse it instead of re-fetching player.js over the network.
type PlayerSource interface {
	PlayerJS(playerID string) (string, error)
}

// sidecarSolver routes cipher work to the sidecar. Tracks which players
// it has already sent JS for in the current sidecar lifetime; on
// ErrPlayerNotLoaded it clears that record and retries once with
// playerJS attached.
type sidecarSolver struct {
	client sidecarClient
	src    PlayerSource

	mu   sync.Mutex
	sent map[string]struct{} // playerIDs already sent in this sidecar lifetime
}

// NewSidecarSolver constructs a sidecarSolver. The client must already
// be Started; the source must yield player JS for any playerID we may
// be asked about.
func NewSidecarSolver(s sidecarClient, src PlayerSource) Solver {
	return newSidecarSolverWith(s, src)
}

func newSidecarSolverWith(s sidecarClient, src PlayerSource) *sidecarSolver {
	return &sidecarSolver{
		client: s,
		src:    src,
		sent:   make(map[string]struct{}),
	}
}

func (s *sidecarSolver) markPlayerSent(playerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[playerID] = struct{}{}
}

func (s *sidecarSolver) clearPlayerSent(playerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sent, playerID)
}

func (s *sidecarSolver) playerSent(playerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sent[playerID]
	return ok
}

// solve issues the JSON-RPC call, attaching playerJS based on whether
// we've sent this player before. On ErrPlayerNotLoaded we drop our
// "sent" record and retry once with playerJS attached.
func (s *sidecarSolver) solve(ctx context.Context, playerID string, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	includeJS := !s.playerSent(playerID)
	result, err := s.callOnce(ctx, playerID, includeJS, sigs, ns)
	if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		s.clearPlayerSent(playerID)
		result, err = s.callOnce(ctx, playerID, true, sigs, ns)
		if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
			// Sidecar still doesn't recognise the player even after we
			// attached the JS. This isn't a transient cache miss — the
			// sidecar dropped or rejected the JS, or it's crashed
			// mid-flight. Use %v (not %w) so errors.Is on
			// ErrPlayerNotLoaded no longer matches: this is a permanent
			// failure, not a recoverable cache miss that a caller should
			// re-try with JS.
			return result, fmt.Errorf("sidecar: player not loaded after retry-with-JS: %v", err)
		}
	}
	return result, err
}

func (s *sidecarSolver) callOnce(ctx context.Context, playerID string, includeJS bool, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	req := sidecar.SolveCipherRequest{
		PlayerID:      playerID,
		SigChallenges: sigs,
		NChallenges:   ns,
	}
	if includeJS {
		js, err := s.src.PlayerJS(playerID)
		if err != nil {
			return sidecar.SolveCipherResult{}, fmt.Errorf("player JS for %s: %w", playerID, err)
		}
		req.PlayerJS = js
	}
	res, err := s.client.SolveCipher(ctx, req)
	if err == nil && includeJS {
		s.markPlayerSent(playerID)
	}
	return res, err
}

func (s *sidecarSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	res, err := s.solve(ctx, playerID, []string{encryptedSig}, nil)
	if err != nil {
		return "", err
	}
	out, ok := res.SigResults[encryptedSig]
	if !ok {
		return "", fmt.Errorf("cipher: sidecar returned no sig result for input")
	}
	return out, nil
}

func (s *sidecarSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	res, err := s.solve(ctx, playerID, nil, []string{encryptedN})
	if err != nil {
		return "", err
	}
	out, ok := res.NResults[encryptedN]
	if !ok {
		return "", fmt.Errorf("cipher: sidecar returned no n result for input")
	}
	return out, nil
}

func (s *sidecarSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	res, err := s.solve(ctx, playerID, sigs, ns)
	if err != nil {
		return nil, nil, err
	}
	// Validate completeness: every requested challenge must appear in
	// the result map. A partial map would otherwise produce empty-string
	// "decrypted" values that silently 403 at the CDN.
	for _, sig := range sigs {
		if _, ok := res.SigResults[sig]; !ok {
			return nil, nil, fmt.Errorf("cipher: sidecar Batch missing sig result for input")
		}
	}
	for _, n := range ns {
		if _, ok := res.NResults[n]; !ok {
			return nil, nil, fmt.Errorf("cipher: sidecar Batch missing n result for input")
		}
	}
	return res.SigResults, res.NResults, nil
}

// Compile-time check: sidecarSolver satisfies cipher.Solver.
var _ Solver = (*sidecarSolver)(nil)
