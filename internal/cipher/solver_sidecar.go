package cipher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// sidecarClient is the subset of *sidecar.Sidecar this solver depends on.
// Defined as an interface so unit tests can inject a fake without
// spawning Node.
type sidecarClient interface {
	SolveCipher(ctx context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error)
}

// PlayerSource yields the player JS for a given playerID and lets the
// sidecar solver invalidate that player's caches end-to-end when the
// sidecar reports a stale-JS error. The goja solver already maintains
// a per-playerID JS cache for its own preprocessing; PlayerSource
// exposes that cache so the sidecar solver can reuse it instead of
// re-fetching player.js over the network.
//
// RemovePlayerJS drops the disk cache, the preprocessed sidecar, the
// in-memory compiled solver, and any STS entry keyed on this player so
// the next PlayerJS call refetches from YouTube. Errors are best-effort
// (file may be held by another reader on Windows); a non-nil return
// signals "couldn't even fully evict" rather than "JS still cached."
type PlayerSource interface {
	PlayerJS(playerID string) (string, error)
	RemovePlayerJS(playerID string) error
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
// we've sent this player before. Two recovery paths sit alongside the
// happy path:
//
//   - ErrPlayerNotLoaded (sidecar evicted JS from its in-memory LRU,
//     restart, etc.): drop the sent-record and retry once with JS
//     attached. Permanent failure if it persists.
//
//   - stale-JS errors ("ejs solve sig: no solutions" / "ejs preprocess"):
//     YouTube has rotated the player and our cache is serving the old
//     algorithm to ejs. Evict every layer (disk + preprocessed + meta +
//     in-memory solver via PlayerSource.RemovePlayerJS, plus the
//     sent-record), then retry once with fresh JS AND ForceReload=true
//     so the sidecar's own preprocessed cache for this playerID is
//     dropped before reload. Persistent failure surfaces as
//     ErrPlayerJSStale — likely an actual extractor bug, not a transient
//     state issue.
//
// Both paths are bounded to one retry; the goal is reactive recovery,
// not infinite-loop tolerance for genuinely-broken cipher state.
func (s *sidecarSolver) solve(ctx context.Context, playerID string, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	includeJS := !s.playerSent(playerID)
	result, err := s.callOnce(ctx, playerID, includeJS, false, sigs, ns)

	if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		s.clearPlayerSent(playerID)
		result, err = s.callOnce(ctx, playerID, true, false, sigs, ns)
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
		return result, err
	}

	if isStaleJSError(err) {
		// Evict every cache layer for this player. Best-effort; even if
		// RemovePlayerJS fails partially (e.g., Windows file lock from a
		// concurrent reader), the retry below sends fresh JS with
		// ForceReload=true so the sidecar drops its own copy regardless.
		// A removal failure means the next PlayerJS call could return
		// stale JS though — log so a persistent ErrPlayerJSStale that's
		// actually masked-eviction-failure is diagnosable.
		if removeErr := s.src.RemovePlayerJS(playerID); removeErr != nil {
			slog.Debug("cipher: stale-JS recovery: RemovePlayerJS partial failure",
				"playerID", playerID, "err", removeErr)
		}
		s.clearPlayerSent(playerID)

		retry, retryErr := s.callOnce(ctx, playerID, true, true, sigs, ns)
		if isStaleJSError(retryErr) {
			// Same error after a fresh JS fetch with sidecar reload —
			// either YouTube rotated mid-retry (rare) or our extractor
			// genuinely can't handle this player. Surface as a permanent
			// stale-JS failure so the goja fallback path can decide
			// whether to attempt its own extraction.
			return retry, fmt.Errorf("%w: %v", ErrPlayerJSStale, retryErr)
		}
		return retry, retryErr
	}

	return result, err
}

// isStaleJSError matches the substring patterns ejs emits when its
// preprocessed solver can't handle the cached player JS. Pattern-matching
// is fragile by nature; the substrings are stable across the ejs
// versions Moombox has tracked but bear watching during ejs upgrades
// (references/update-all.sh surfaces upstream changes).
func isStaleJSError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "ejs solve sig: no solutions"):
		return true
	case strings.Contains(msg, "ejs solve n: no solutions"):
		return true
	case strings.Contains(msg, "ejs preprocess"):
		return true
	}
	return false
}

// callOnce issues a single SolveCipher round-trip. includeJS attaches
// the latest player JS via PlayerSource (true on first-call-for-this-
// playerID and on every retry path). forceReload tells the sidecar to
// drop its own preprocessed cache for playerID before reloading; it's
// only meaningful when includeJS is also true (no JS = nothing to
// reload to).
func (s *sidecarSolver) callOnce(ctx context.Context, playerID string, includeJS, forceReload bool, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	req := sidecar.SolveCipherRequest{
		PlayerID:      playerID,
		SigChallenges: sigs,
		NChallenges:   ns,
		ForceReload:   forceReload,
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
