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
//
// A player is asked on its main body first and, when ejs cannot solve with
// that, on its tv body (solve; owner ruling RV-11). The first-send gates and
// the send counts are keyed by the sidecar's player key — the player id for
// the main body, tvPlayerKey for the tv one — so the two bodies of one player
// have one each. The sent record is the main body's alone: a tv solve always
// asks without the JS first.
type sidecarSolver struct {
	client sidecarClient
	src    PlayerSource

	// mem remembers which players are solved with the tv body. The solver's
	// own until a compositeSolver hands it the one it keeps across sidecar
	// restarts (adoptMemory), before the solver is asked anything.
	mem *playerMemory

	mu   sync.Mutex
	sent map[string]struct{} // players whose main body was sent in this sidecar lifetime
	// sending gates the first send of a player's JS: one one-slot channel per
	// player key. Concurrent first solves used to each ship the ~3 MB player
	// and each make the sidecar load it — a synchronous pass on its event
	// loop that stalls every PO-token request queued behind it. Guarded by mu.
	sending map[string]chan struct{}
	// sends counts the JS-carrying asks made per player key, whatever they
	// ended in. A solve that had to wait for a key's gate reads it to tell
	// that another solve sent the body meanwhile. Guarded by mu.
	sends map[string]uint64
}

// adoptMemory replaces the solver's player memory with one that outlives it.
// Called on a solver nothing has asked yet (memoryAdopter).
func (s *sidecarSolver) adoptMemory(m *playerMemory) { s.mem = m }

// NewSidecarSolver constructs a sidecarSolver. The client must already
// be Started; the source must yield player JS for any playerID we may
// be asked about.
func NewSidecarSolver(s sidecarClient, src PlayerSource) Solver {
	return newSidecarSolverWith(s, src)
}

func newSidecarSolverWith(s sidecarClient, src PlayerSource) *sidecarSolver {
	return &sidecarSolver{
		client:  s,
		src:     src,
		mem:     newPlayerMemory(),
		sent:    make(map[string]struct{}),
		sending: make(map[string]chan struct{}),
		sends:   make(map[string]uint64),
	}
}

// claimFirstSend takes the first-send gate for playerID, or gives up when ctx
// ends first. The returned func releases it.
func (s *sidecarSolver) claimFirstSend(ctx context.Context, playerID string) (func(), error) {
	s.mu.Lock()
	gate, ok := s.sending[playerID]
	if !ok {
		gate = make(chan struct{}, 1)
		s.sending[playerID] = gate
	}
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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

// noteSend records that a JS-carrying ask was made under key.
func (s *sidecarSolver) noteSend(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends[key]++
}

// sendCount is how many JS-carrying asks have been made under key.
func (s *sidecarSolver) sendCount(key string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends[key]
}

func (s *sidecarSolver) playerSent(playerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sent[playerID]
	return ok
}

// errTVNotAnswered marks a failed solve whose main body failed on the player
// itself and whose tv ask then ended without an answer about the tv body: a
// timeout, a tv fetch that failed in transit. The error still reads as the
// main body's (ErrPlayerJSStale, for the worker's fallbacks), and the next
// solve asks tv again; compositeSolver.noteGojaFall keeps the player's one
// Warn for a solve that heard from both bodies.
var errTVNotAnswered = errors.New("the ask got no answer about this body")

// solve answers the challenges with the sidecar, on the player's main body
// and, when ejs cannot solve with that one, on its tv body (owner ruling
// RV-11: "stay on main, retry tv, then goja" — goja is the caller's, after
// this has failed).
//
// The tv body is asked only when the main one ended in a failure that is the
// player's own (playerFailure): ejs cannot solve with the body
// (ErrPlayerJSStale), or its file is not there (a 404 or 403 on the fetch).
// The sidecar being down, a timeout or a fetch that failed in transit would
// fail tv alike. It is asked once. When it solves, the player is remembered
// as a tv one (playerMemory) and every later solve skips main; a Warn says
// so, once per player. When it fails too, the main body's error is returned
// with the tv failure beside it, marked errTVNotAnswered when that failure
// was not the tv body's own. A PlayerSource that cannot yield a tv body
// (TVPlayerSource) gets no retry.
func (s *sidecarSolver) solve(ctx context.Context, playerID string, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	if s.mem.usesTV(playerID) {
		res, err := s.solveTV(ctx, playerID, sigs, ns)
		if isStaleJSError(err) {
			// Main is known not to work for this player, so this is all the
			// sidecar has: the same sentinel a failed main body ends in.
			err = fmt.Errorf("%w: tv variant: %v", ErrPlayerJSStale, err)
		}
		return res, err
	}
	res, err := s.solveMain(ctx, playerID, sigs, ns)
	if !playerFailure(err) || ctx.Err() != nil {
		return res, err
	}
	if _, ok := s.src.(TVPlayerSource); !ok {
		return res, err
	}
	tvRes, tvErr := s.solveTV(ctx, playerID, sigs, ns)
	if tvErr != nil {
		if !isStaleJSError(tvErr) && !playerFailure(tvErr) {
			// The tv ask did not get to an answer about the tv body (a
			// timeout, a fetch that failed in transit): the main body's
			// failure stands, and the error says tv is still to be asked.
			return tvRes, fmt.Errorf("%w (tv variant: %w: %v)", err, errTVNotAnswered, tvErr)
		}
		return tvRes, fmt.Errorf("%w (tv variant: %v)", err, tvErr)
	}
	if s.mem.setTV(playerID) {
		slog.Warn("cipher: the sidecar cannot solve with this player's main variant; its tv variant solves, and is used for this player from now on",
			"playerID", playerID, "from", "main", "to", "tv", "err", err)
	}
	return tvRes, nil
}

// solveTV asks the sidecar on the player's tv body, under its own player key
// (tvKeySuffix). The first ask goes without the JS: the sidecar may hold the
// prepared copy in memory or in its store, from before a restart too. Only
// when it does not (ErrPlayerNotLoaded) is the body fetched
// (TVPlayerSource, never PlayerCache) and sent, one send per player at a
// time. There is no stale-JS recovery here: the body sent is the one just
// fetched, and nothing of it is cached on the Go side to evict.
//
// A tv body that cannot be used at all — ejs threw while preparing it, or
// the fetch was answered that there is none — is not fetched and sent again
// within staleVerdictWindow: the solve gets that failure back at once.
func (s *sidecarSolver) solveTV(ctx context.Context, playerID string, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	tvSrc, ok := s.src.(TVPlayerSource)
	if !ok {
		return sidecar.SolveCipherResult{}, fmt.Errorf("cipher: no tv player source for %s", playerID)
	}
	if dead := s.mem.tvDead(playerID); dead != nil {
		return sidecar.SolveCipherResult{}, dead
	}
	key := tvPlayerKey(playerID)
	sends := s.sendCount(key)
	res, err := s.ask(ctx, key, "", false, sigs, ns)
	if !errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		return res, err
	}
	release, err := s.claimFirstSend(ctx, key)
	if err != nil {
		return sidecar.SolveCipherResult{}, err
	}
	defer release()
	if dead := s.mem.tvDead(playerID); dead != nil {
		// Another solve found the body unusable while this one waited.
		return sidecar.SolveCipherResult{}, dead
	}
	if s.sendCount(key) != sends {
		// Another solve sent the body while this one waited for the gate,
		// and the sidecar may hold it whatever that solve ended in ("no
		// solutions" is an answer from a loaded body): ask before fetching
		// about 2.5 MB and sending it again.
		res, err = s.ask(ctx, key, "", false, sigs, ns)
		if !errors.Is(err, sidecar.ErrPlayerNotLoaded) {
			return res, err
		}
	}
	js, err := tvSrc.TVPlayerJS(ctx, playerID)
	if err != nil {
		err = fmt.Errorf("tv player JS for %s: %w", playerID, err)
		// A definite answer (a 404: this player has no tv file) says the
		// same on every solve; a transport failure or a struggling CDN is
		// asked again by the next one.
		if ctx.Err() == nil && !retryablePlayerFetch(err) {
			s.mem.setTVDead(playerID, err)
		}
		return sidecar.SolveCipherResult{}, err
	}
	res, err = s.ask(ctx, key, js, false, sigs, ns)
	s.noteSend(key)
	if isPreprocessError(err) {
		s.mem.setTVDead(playerID, err)
	}
	if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		// As solveMain: %v, so this no longer reads as a miss to retry.
		return res, fmt.Errorf("sidecar: tv player not loaded after it was sent: %v", err)
	}
	return res, err
}

// solveMain issues the JSON-RPC call on the main body, attaching playerJS
// based on whether we've sent this player before. Two recovery paths sit
// alongside the happy path:
//
//   - ErrPlayerNotLoaded (sidecar evicted JS from its in-memory LRU,
//     restart, etc.): drop the sent-record and retry once with JS
//     attached. Permanent failure if it persists.
//
//   - stale-JS errors ("ejs solve sig: no solutions", "ejs solve n: no
//     solutions" — ejs's own, or the sidecar's refusal of an n answer that
//     is the player's exception sentinel — and "ejs preprocess"):
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
//
// The stale-JS recovery runs once per player within staleVerdictWindow, not
// once per solve (owner ruling RV-11): a player ejs cannot solve with used to
// be evicted, refetched (about 3 MB) and prepared twice over by the sidecar
// on every solve. Within the window of the last recovery:
//
//   - a main body ejs threw on while preparing it is not asked at all — that
//     failure is the body's, whatever the challenge — and the solve gets the
//     recovery's ErrPlayerJSStale back at once;
//   - otherwise the sidecar is asked as usual, without the JS first (it
//     holds the body the recovery sent), and a stale-JS error ends in
//     ErrPlayerJSStale with nothing evicted. The ask is still made because
//     "no solutions" can be one challenge's failure (a `tcl` page's n on
//     the main function): the player's other solves must keep being
//     answered.
//
// A recovery whose retry ended in anything but an answer or a stale-JS error
// opens no window (cancelEviction): the next solve runs it again.
//
// The recovery is one at a time per player (the first-send gate), and a solve
// that waited for another's asks once more before deciding anything.
func (s *sidecarSolver) solveMain(ctx context.Context, playerID string, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	if dead := s.mem.mainDead(playerID); dead != nil {
		return sidecar.SolveCipherResult{}, dead
	}
	recoveries, held := s.mem.recoveryState(playerID)
	// Within the window the sidecar holds what the recovery sent, marked as
	// sent or not (a failed solve marks nothing): ask without the JS, and
	// ErrPlayerNotLoaded below sends it if the sidecar has lost it.
	includeJS := !held && !s.playerSent(playerID)
	var release func()
	if includeJS {
		// One first send per player; the solves that waited on it find the
		// player sent and go without the JS.
		var err error
		if release, err = s.claimFirstSend(ctx, playerID); err != nil {
			return sidecar.SolveCipherResult{}, err
		}
		defer release()
		// What the solve ahead of this one found stands for this one too: a
		// burst of first solves on a player ejs cannot prepare would
		// otherwise each send the body and have the sidecar prepare it.
		if dead := s.mem.mainDead(playerID); dead != nil {
			return sidecar.SolveCipherResult{}, dead
		}
		recoveries, held = s.mem.recoveryState(playerID)
		includeJS = !held && !s.playerSent(playerID)
	}
	sends := s.sendCount(playerID)
	result, err := s.callOnce(ctx, playerID, includeJS, false, sigs, ns)

	if errors.Is(err, sidecar.ErrPlayerNotLoaded) && release == nil {
		// The body is to be sent again, and that is one solve's to do at a
		// time, like a first send. A solve that arrived while another's
		// recovery was under way (held, so it asked without the JS and took no
		// gate) used to send the body beside it: for a player ejs cannot
		// prepare the sidecar holds nothing, so every such arrival shipped
		// about 3 MB and cost the sidecar one more failed preparation.
		var gateErr error
		if release, gateErr = s.claimFirstSend(ctx, playerID); gateErr != nil {
			return sidecar.SolveCipherResult{}, gateErr
		}
		defer release()
		if dead := s.mem.mainDead(playerID); dead != nil {
			// What the solve ahead of this one found stands for this one.
			return sidecar.SolveCipherResult{}, dead
		}
		if s.sendCount(playerID) != sends {
			// Another solve sent the body while this one waited: ask what
			// the sidecar holds now before sending anything.
			result, err = s.callOnce(ctx, playerID, false, false, sigs, ns)
		}
	}
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
		// A stale-JS failure of the body just sent is handled below like any
		// other. It used to be returned as it came, which kept it from the
		// recovery and, now, from the tv body.
	}

	if isStaleJSError(err) {
		if release == nil {
			var gateErr error
			if release, gateErr = s.claimFirstSend(ctx, playerID); gateErr != nil {
				return sidecar.SolveCipherResult{}, gateErr
			}
			defer release()
		}
		finished, held := s.mem.recoveryState(playerID)
		if finished != recoveries {
			// Another solve ran the recovery while this one waited for the
			// gate: ask what it left before deciding anything.
			if dead := s.mem.mainDead(playerID); dead != nil {
				return sidecar.SolveCipherResult{}, dead
			}
			result, err = s.callOnce(ctx, playerID, false, false, sigs, ns)
			if !isStaleJSError(err) {
				return result, err
			}
		}
		if held {
			// The recovery already ran within the window: nothing is
			// evicted again.
			return result, fmt.Errorf("%w: %v", ErrPlayerJSStale, err)
		}
		s.mem.noteEviction(playerID)
		defer s.mem.recoveryDone(playerID)

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
		if retryErr != nil && !isStaleJSError(retryErr) {
			// The recovery did not run to its end: the refetch failed, the
			// sidecar timed out, the caller gave up. The sidecar may still
			// hold the old prepared copy, and nothing has been learned about
			// a fresh one, so the window is not kept: the next solve runs the
			// recovery again. Kept, every solve of the next ten minutes got
			// the old copy's stale answer, went to tv, and the first tv
			// success pinned the player there with main never retried.
			s.mem.cancelEviction(playerID)
		}
		if isStaleJSError(retryErr) {
			// Same error after a fresh JS fetch with sidecar reload —
			// either YouTube rotated mid-retry (rare) or our extractor
			// genuinely can't handle this player. Surface as a permanent
			// stale-JS failure so the goja fallback path can decide
			// whether to attempt its own extraction.
			stale := fmt.Errorf("%w: %v", ErrPlayerJSStale, retryErr)
			if isPreprocessError(retryErr) {
				s.mem.setMainDead(playerID, stale)
			}
			return retry, stale
		}
		return retry, retryErr
	}

	return result, err
}

// playerFailure reports whether a failed solve failed on the player itself,
// so that asking again with the same body ends the same: ejs cannot solve
// with it (ErrPlayerJSStale), or the fetch of its file got a definite answer
// that it is not to be had (a 404 for a player YouTube has pulled, a 403).
// A timeout, a sidecar that is down or a fetch that failed in transit is
// none of the player's doing. This is what sends a solve on from the main
// body to the tv one, and what the Warn for a fall to goja is kept for
// (compositeSolver.noteGojaFall).
func playerFailure(err error) bool {
	if errors.Is(err, ErrPlayerJSStale) {
		return true
	}
	return errors.Is(err, ErrPlayerJSFetch) && !retryablePlayerFetch(err)
}

// isStaleJSError matches the substring patterns ejs emits when its
// preprocessed solver can't handle the cached player JS. Pattern-matching
// is fragile by nature; the substrings are stable across the ejs
// versions Moombox has tracked but bear watching during ejs upgrades
// (references/update-all.sh surfaces upstream changes).
//
// "ejs solve n: no solutions" also matches the sidecar's own refusal of an
// n answer that ends with its challenge, the player's exception sentinel
// ("ejs solve n: no solutions (exception sentinel)", bgutil-sidecar's
// solveMany). The fresh-JS retry it buys is Moombox's own: yt-dlp only asks
// its next provider after an invalid response (jsc/_director.py:117-124);
// the sidecar's comment there says what the retry costs.
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

// isPreprocessError reports whether err is ejs failing while it prepares a
// player body (the sidecar's loadPlayer label, one of isStaleJSError's): a
// failure of the body itself, the same for every challenge.
func isPreprocessError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ejs preprocess")
}

// callOnce issues a single SolveCipher round-trip. includeJS attaches
// the latest player JS via PlayerSource (true on first-call-for-this-
// playerID and on every retry path). forceReload tells the sidecar to
// drop its own preprocessed cache for playerID before reloading; it's
// only meaningful when includeJS is also true (no JS = nothing to
// reload to).
func (s *sidecarSolver) callOnce(ctx context.Context, playerID string, includeJS, forceReload bool, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	js := ""
	if includeJS {
		var err error
		if js, err = s.src.PlayerJS(playerID); err != nil {
			return sidecar.SolveCipherResult{}, fmt.Errorf("player JS for %s: %w", playerID, err)
		}
	}
	res, err := s.ask(ctx, playerID, js, forceReload, sigs, ns)
	if includeJS {
		s.noteSend(playerID)
		if err == nil {
			s.markPlayerSent(playerID)
		}
	}
	return res, err
}

// ask is one SolveCipher round-trip under a sidecar player key (the player id,
// or tvPlayerKey), with js attached when it is not "".
func (s *sidecarSolver) ask(ctx context.Context, key, js string, forceReload bool, sigs, ns []string) (sidecar.SolveCipherResult, error) {
	return s.client.SolveCipher(ctx, sidecar.SolveCipherRequest{
		PlayerID:      key,
		PlayerJS:      js,
		SigChallenges: sigs,
		NChallenges:   ns,
		ForceReload:   forceReload,
	})
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

// ready reports whether the sidecar this solver asks is up, for the
// composite's SidecarReady and its typed sig failure (readinessReporter): its
// client's IsHealthy, which *sidecar.Sidecar flips false the moment the
// child dies or a write stalls and true again once a restart is up. A
// client that cannot say — the test fakes — is taken as up.
func (s *sidecarSolver) ready() bool {
	h, ok := s.client.(interface{ IsHealthy() bool })
	return !ok || h.IsHealthy()
}

// probeSigs asks the sidecar the synthetic sig challenges compositeSolver
// learns a player's sig specs from, with none of solve's stale-JS recovery.
// That recovery evicts every cache layer of the player (RemovePlayerJS),
// refetches it and has the sidecar re-preprocess it; it is for a real
// signature the player can no longer answer, not for a probe whose real
// signature was answered a moment before. A player function that throws on
// the control characters of the synthetic challenge makes ejs answer "no
// solutions", which isStaleJSError matches: through solve, every later
// signature of that length used to wipe and refetch a healthy player on
// the sidecar's single-threaded loop, which also serves PO-token mints.
// A sidecar that has lost the player (ErrPlayerNotLoaded) is re-sent it
// once, as solve does; that evicts nothing. A player solved with its tv body
// is probed on that one, the body whose real answer the spec is checked
// against; solveTV has no recovery to bypass.
func (s *sidecarSolver) probeSigs(ctx context.Context, playerID string, sigs []string) (map[string]string, error) {
	var res sidecar.SolveCipherResult
	var err error
	if s.mem.usesTV(playerID) {
		res, err = s.solveTV(ctx, playerID, sigs, nil)
	} else {
		res, err = s.callOnce(ctx, playerID, !s.playerSent(playerID), false, sigs, nil)
		if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
			s.clearPlayerSent(playerID)
			res, err = s.callOnce(ctx, playerID, true, false, sigs, nil)
		}
	}
	if err != nil {
		return nil, err
	}
	for _, sig := range sigs {
		if _, ok := res.SigResults[sig]; !ok {
			return nil, fmt.Errorf("cipher: sidecar probe missing sig result for input")
		}
	}
	return res.SigResults, nil
}

// Compile-time check: sidecarSolver satisfies cipher.Solver.
var _ Solver = (*sidecarSolver)(nil)
