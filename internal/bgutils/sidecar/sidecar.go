// Package sidecar manages the embedded BotGuard Node subprocess.
//
// The sidecar is a Node.js process running our bgutil-sidecar JS code
// (JSDOM + bgutils-js) under a real V8 engine. Moombox spawns one per
// process, pipes JSON-RPC requests to it via stdin/stdout, and consumes
// PO tokens that pass Google's BotGuard timing fingerprint check (which
// our pure-goja path can't satisfy because the interpreter runs ~100x
// faster than V8 -- see docs/investigations/botguard-option-2-results.md).
//
// Lifecycle:
//
//	s := sidecar.New(sidecar.Config{Logger: log})
//	if err := s.Start(ctx); err != nil { /* fall back to goja */ }
//	defer s.Stop()
//
//	token, err := s.GeneratePoToken(ctx, contentBinding)
//
// Crash safety: child is pinned to a Windows Job Object so it dies when
// Moombox does. Internal readPump goroutine watches stdout for EOF and
// marks the sidecar unhealthy on parent crash so callers can short-circuit
// to the fallback path.
package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logger is the structured logging interface Moombox uses everywhere.
// Anonymous struct-shape interface so the sidecar package doesn't introduce
// a hard dependency on any particular logger type.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Config configures a sidecar instance. CacheDir defaults to
// os.UserCacheDir()/Moombox/sidecar when empty. StartupTimeout defaults
// to 60s and is a backstop for a genuinely wedged sidecar — the sidecar
// emits a `ready` notification when its synchronous init completes, so
// healthy startup latency does not race against this deadline.
// RequestTimeout defaults to 90s (sized for a cold-path PO-token mint).
//
// V8HardLimitMB sets V8's --max-old-space-size; hitting this DOES OOM-abort
// the sidecar (V8 has no graceful soft stop). Set it well above the soft
// threshold the parent enforces via TriggerGC. Zero leaves V8's default
// (~512-1500 MB depending on host).
//
// ExposeGC enables Node's --expose-gc so the sidecar can run global.gc()
// on demand. Required for the TriggerGC RPC; harmless when no caller fires
// it.
type Config struct {
	CacheDir       string
	StartupTimeout time.Duration
	RequestTimeout time.Duration
	V8HardLimitMB  int
	ExposeGC       bool
	Logger         Logger
}

// Sidecar manages one Node subprocess running the BotGuard JS sidecar.
// Safe for concurrent use after Start; calls to GeneratePoToken from
// multiple goroutines are serialized at the stdin write boundary and
// multiplexed by request ID on the response side.
type Sidecar struct {
	cfg Config

	// Process state. nil before Start / after Stop.
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	job      *processJob
	cacheDir string

	// Stdin write serialization. Goroutines calling GeneratePoToken in
	// parallel must not interleave their JSON lines on the wire.
	writeMu sync.Mutex

	// Request multiplexing.
	nextReqID atomic.Uint64
	pendingMu sync.Mutex
	pending   map[uint64]chan rpcResponse

	// Health.
	healthy atomic.Bool

	// Startup handshake. The sidecar emits {"event":"ready"} on stdout once
	// server.js finishes its synchronous init (jsdom + bgutils-js module
	// load + JSDOM construction). readPump closes readyCh on the first
	// ready event, OR on stdout EOF before ready (recording readyErr) so
	// Start can distinguish "wedged sidecar" (StartupTimeout fires) from
	// "sidecar exited before ready" (immediate failure).
	readyOnce sync.Once
	readyCh   chan struct{}
	readyErr  error // set inside readyOnce.Do before close(readyCh)

	// Lifecycle. closed signals to readPump/stderrPump that Stop() was
	// called and they should exit silently rather than mark unhealthy.
	stopOnce  sync.Once
	stopping  atomic.Bool
	pumpsDone sync.WaitGroup
}

type rpcRequest struct {
	ID     uint64         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// New constructs a Sidecar. Does not start the subprocess; call Start.
func New(cfg Config) *Sidecar {
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 60 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		// Generous enough for the cold-path mint — a cache-miss
		// generatePoToken runs two network round-trips (challenge fetch +
		// GenerateIT) plus a full BotGuard interpreter pass inside the
		// sidecar, which can take tens of seconds on slow hardware — while
		// still bounding a genuinely wedged V8 (the failure this timeout
		// exists for, where previously callers hung for the job lifetime).
		cfg.RequestTimeout = 90 * time.Second
	}
	return &Sidecar{
		cfg:     cfg,
		pending: make(map[uint64]chan rpcResponse),
	}
}

// Start extracts the embedded blobs (if needed), launches the Node
// subprocess pinned to a Windows Job Object, and waits for the sidecar
// to emit a `ready` notification on stdout signaling that server.js has
// finished its synchronous init (jsdom module load + JSDOM construction).
// Returns an error if extraction, launch, or the ready handshake fails;
// the caller should fall back to the goja path on error.
func (s *Sidecar) Start(ctx context.Context) error {
	if s.cmd != nil {
		return errors.New("sidecar: already started")
	}

	cacheDir, err := s.resolveCacheDir()
	if err != nil {
		return err
	}
	s.cacheDir = cacheDir

	if err := extractIfNeeded(cacheDir); err != nil {
		return fmt.Errorf("extract sidecar payload: %w", err)
	}

	nodeExe := filepath.Join(cacheDir, nodeBinaryName())
	serverJS := filepath.Join(cacheDir, "src", "server.js")

	// Build node args: V8 flags first (must come before the script path),
	// then the script. --max-old-space-size is V8's hard ceiling on the
	// old-generation heap (in MB); --expose-gc lets server.js call
	// global.gc() in response to the triggerGC RPC.
	nodeArgs := []string{}
	if s.cfg.V8HardLimitMB > 0 {
		nodeArgs = append(nodeArgs, fmt.Sprintf("--max-old-space-size=%d", s.cfg.V8HardLimitMB))
	}
	if s.cfg.ExposeGC {
		nodeArgs = append(nodeArgs, "--expose-gc")
	}
	nodeArgs = append(nodeArgs, serverJS)

	cmd := exec.CommandContext(context.Background(), nodeExe, nodeArgs...)
	cmd.Dir = cacheDir
	// Parent-death cleanup, pre-start half: Linux sets PR_SET_PDEATHSIG
	// here (must be configured before fork); Windows is a no-op because
	// the Job Object assigned after Start covers it.
	configureCmdSysProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderr
	s.readyCh = make(chan struct{})

	// Pin the child to a Job Object so it dies when Moombox dies. On
	// Linux processJob is a no-op — PR_SET_PDEATHSIG (configured before
	// Start above) provides the same die-with-parent guarantee there.
	job, err := newProcessJob()
	if err != nil {
		s.cfg.Logger.Warn("sidecar: Job Object create failed, child may outlive parent on crash", "err", err)
	} else {
		if err := job.assign(cmd.Process); err != nil {
			s.cfg.Logger.Warn("sidecar: Job Object assign failed", "err", err)
			job.close()
			job = nil
		}
	}
	s.job = job

	s.pumpsDone.Add(2)
	go s.readPump()
	go s.stderrPump()

	// Wait for server.js to emit `{"event":"ready"}` after its synchronous
	// init finishes. Replaces a prior ping/pong handshake with a 5s deadline
	// that started racing jsdom cold-start after the v2.6.14 jsdom 27→29
	// bump (module parse + DOM construction can exceed 5s on Windows even
	// with a warm filesystem cache, leaving every PO-token request to fall
	// through to the goja path). The deadline below is now a backstop for
	// a hung sidecar, not a metronome operators must keep retuning.
	readyCtx, cancel := context.WithTimeout(ctx, s.cfg.StartupTimeout)
	defer cancel()
	select {
	case <-s.readyCh:
		if s.readyErr != nil {
			_ = s.Stop()
			return fmt.Errorf("ready: %w", s.readyErr)
		}
	case <-readyCtx.Done():
		_ = s.Stop()
		return fmt.Errorf("ready: %w", readyCtx.Err())
	}

	s.healthy.Store(true)
	s.cfg.Logger.Info("sidecar started", "cacheDir", cacheDir, "pid", cmd.Process.Pid)
	return nil
}

// Stop gracefully shuts down the sidecar. Sends a shutdown JSON-RPC,
// waits briefly, then hard-kills + closes the Job Object.
//
// Total wall-time bound: ~3s (1s for the JSON-RPC bye response, 2s for
// the process to exit on its own, then Kill). This stays well below the
// shutdown.go force-exit budget so a hung sidecar can't starve the rest
// of Moombox's shutdown sequence (web server, DB unsubscribe, DB close).
//
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Sidecar) Stop() error {
	var firstErr error
	s.stopOnce.Do(func() {
		// Mark stopping so readPump exits silently on EOF instead of
		// flagging the sidecar unhealthy mid-shutdown. Crucially, do NOT
		// flip s.healthy yet -- the graceful shutdown JSON-RPC below goes
		// through call(), which short-circuits on !healthy.
		s.stopping.Store(true)

		if s.cmd == nil || s.cmd.Process == nil {
			s.healthy.Store(false)
			return
		}

		// Best-effort graceful shutdown via JSON-RPC. The sidecar JS
		// writes its "bye" response and then process.exit(0)s on the
		// next tick. 1s is generous for that round-trip; longer just
		// extends shutdown latency for no benefit.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_ = s.callRaw(shutdownCtx, "shutdown", nil)
		cancel()

		// Now stop accepting new work; any inflight requests waiting on
		// channels will be drained below.
		s.healthy.Store(false)

		// Wait briefly for the process to exit on its own. The Node side
		// already scheduled process.exit(0) so this is just signal
		// latency -- 2s covers a slow tick + kernel reap, beyond which
		// we kill rather than starve the wider shutdown budget.
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// Must still deliver — a lost send would deadlock the
					// receive below.
					done <- fmt.Errorf("sidecar wait panic: %v", r)
				}
			}()
			done <- s.cmd.Wait()
		}()
		select {
		case err := <-done:
			if err != nil {
				s.cfg.Logger.Debug("sidecar exited", "err", err)
			}
		case <-time.After(2 * time.Second):
			s.cfg.Logger.Warn("sidecar shutdown timed out, killing process", "pid", s.cmd.Process.Pid)
			if killErr := s.cmd.Process.Kill(); killErr != nil {
				firstErr = killErr
			}
			<-done
		}

		// Close pipes BEFORE the Job Object so readPump/stderrPump exit
		// on EOF rather than on Job Object teardown which is more abrupt.
		_ = s.stdin.Close()
		_ = s.stdout.Close()
		_ = s.stderr.Close()

		// Close Job Object — kills any straggling children too.
		if s.job != nil {
			s.job.close()
			s.job = nil
		}

		// Drain any remaining pending requests with an error.
		s.drainPending("sidecar stopped")

		// Wait for pumps to finish so callers can observe a quiescent state.
		s.pumpsDone.Wait()
	})
	return firstErr
}

// IsHealthy reports whether the sidecar is currently usable. False after
// Start fails, after Stop is called, or after the readPump observes
// stdout EOF (parent crash recovery).
func (s *Sidecar) IsHealthy() bool { return s.healthy.Load() }

// CacheDir returns the directory where the sidecar's Node binary and JS
// source live, or empty string if Start has not been called. Useful for
// diagnostics and tests.
func (s *Sidecar) CacheDir() string { return s.cacheDir }

// KillForTest hard-kills the underlying Node process. Tests use this to
// simulate a crash so the fallback path can be exercised. Not for use
// in production code -- the production shutdown path is Stop().
func (s *Sidecar) KillForTest() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return errors.New("sidecar: not started")
	}
	return s.cmd.Process.Kill()
}

// GeneratePoToken asks the sidecar to mint a PO token for the given
// content binding. Returns the websafe-encoded token string on success.
//
// Internally caches the BotGuard minter inside the sidecar (single
// minter, served across bindings, ~6h TTL). PotProvider should still
// session-cache results for repeated bindings.
func (s *Sidecar) GeneratePoToken(ctx context.Context, binding string) (string, error) {
	var result struct {
		PoToken string `json:"poToken"`
	}
	if err := s.call(ctx, "generatePoToken", map[string]any{"binding": binding}, &result); err != nil {
		return "", err
	}
	if result.PoToken == "" {
		return "", errors.New("sidecar returned empty poToken")
	}
	return result.PoToken, nil
}

// InvalidateCaches wipes the sidecar's session + minter caches. Mirrors
// PotProvider.InvalidateCaches.
func (s *Sidecar) InvalidateCaches(ctx context.Context) error {
	return s.call(ctx, "invalidateCaches", nil, nil)
}

// InvalidateIT wipes only the sidecar's minter cache (forces a fresh
// BotGuard run on next mint). Mirrors PotProvider.InvalidateIntegrityTokens.
func (s *Sidecar) InvalidateIT(ctx context.Context) error {
	return s.call(ctx, "invalidateIT", nil, nil)
}

// MemoryStats holds the sidecar Node process's self-reported memory
// numbers. RSS is the resident set size (the OS-level "how much RAM
// this process is using" number — what Task Manager shows in the
// Memory column). The V8 heap fields reveal pressure on the JS engine
// itself: HeapUsed is what's currently allocated, HeapTotal is what
// V8 has reserved.
type MemoryStats struct {
	RSS          int64 `json:"rss"`
	HeapTotal    int64 `json:"heapTotal"`
	HeapUsed     int64 `json:"heapUsed"`
	External     int64 `json:"external"`
	ArrayBuffers int64 `json:"arrayBuffers"`
}

// MemoryStats fetches the sidecar Node process's self-reported memory
// numbers. Useful for cumulative memory diagnostics that combine
// Moombox's Go-runtime stats with the sidecar's V8 stats in a single
// log line.
func (s *Sidecar) MemoryStats(ctx context.Context) (MemoryStats, error) {
	var stats MemoryStats
	if err := s.call(ctx, "getMemoryStats", nil, &stats); err != nil {
		return MemoryStats{}, err
	}
	return stats, nil
}

// TriggerGCResult is the before/after memory snapshot returned by the
// triggerGC RPC. Helpful for logging "GC reclaimed N MB" without a separate
// MemoryStats round-trip.
type TriggerGCResult struct {
	Before MemoryStats `json:"before"`
	After  MemoryStats `json:"after"`
}

// TriggerGC asks the sidecar to run a full V8 GC cycle. Requires
// Config.ExposeGC = true at startup; otherwise the sidecar returns an
// error that this method propagates. Used by Moombox to enforce a soft
// memory limit on the sidecar (V8 has no native soft-limit primitive).
func (s *Sidecar) TriggerGC(ctx context.Context) (TriggerGCResult, error) {
	var result TriggerGCResult
	if err := s.call(ctx, "triggerGC", nil, &result); err != nil {
		return TriggerGCResult{}, err
	}
	return result, nil
}

// Stats holds the sidecar's internal counters. Useful for diagnostics
// surfaced in /api/pot.
type Stats struct {
	CachedMinters  int `json:"cachedMinters"`
	CachedSessions int `json:"cachedSessions"`
	MintsTotal     int `json:"mintsTotal"`
	MintsErrored   int `json:"mintsErrored"`
}

// GetStats fetches the sidecar's internal counters.
func (s *Sidecar) GetStats(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := s.call(ctx, "getStats", nil, &stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// SolveCipherRequest is the parameter payload for the solveCipher JSON-RPC
// method. PlayerJS is optional after the first call for a given PlayerID
// in the sidecar's lifetime; subsequent calls may omit it. If the sidecar
// reports "player not loaded", callers should retry with PlayerJS attached.
//
// ForceReload tells the sidecar to drop any cached preprocessed solver
// for PlayerID before loading the attached PlayerJS. Set when the caller
// has detected that the sidecar's cached JS is stale (e.g. an
// "ejs solve sig: no solutions" error after a YouTube-side player
// rotation). Without ForceReload, the sidecar's `if (!entry)` cache
// guard would silently ignore freshly-attached PlayerJS for an already
// known PlayerID.
type SolveCipherRequest struct {
	PlayerID      string   `json:"playerID"`
	PlayerJS      string   `json:"playerJS,omitempty"`
	SigChallenges []string `json:"sigChallenges,omitempty"`
	NChallenges   []string `json:"nChallenges,omitempty"`
	ForceReload   bool     `json:"forceReload,omitempty"`
}

// SolveCipherResult is the response payload. Result maps are keyed by the
// input challenge string. If a request specified empty challenge slices
// for a type, the corresponding result map is empty (not nil).
type SolveCipherResult struct {
	SigResults map[string]string `json:"sigResults"`
	NResults   map[string]string `json:"nResults"`
}

// playerNotLoadedSentinel is the JSON-RPC error body the JS sidecar
// emits when SolveCipher is called for a playerID it has no cached
// preprocessed source for. The Go side detects this via suffix-match
// (the call() helper adds a "sidecar: " prefix) so the prefix can
// evolve without silently breaking sentinel detection.
const playerNotLoadedSentinel = "player not loaded"

// ErrPlayerNotLoaded indicates the sidecar discarded the player JS
// (LRU eviction or restart). The caller should retry SolveCipher with
// PlayerJS populated. Detected via strings.HasSuffix on
// playerNotLoadedSentinel so a future change to the call() error
// prefix doesn't break detection.
var ErrPlayerNotLoaded = errors.New("sidecar: player not loaded")

// SolveCipher solves YouTube sig and/or n cipher challenges against
// the loaded player JS. PlayerJS can be omitted on warm calls; on
// ErrPlayerNotLoaded the caller should re-issue the call with PlayerJS
// populated.
func (s *Sidecar) SolveCipher(ctx context.Context, req SolveCipherRequest) (SolveCipherResult, error) {
	params := map[string]any{
		"playerID":      req.PlayerID,
		"sigChallenges": req.SigChallenges,
		"nChallenges":   req.NChallenges,
	}
	if req.PlayerJS != "" {
		params["playerJS"] = req.PlayerJS
	}
	if req.ForceReload {
		params["forceReload"] = true
	}

	var result SolveCipherResult
	if err := s.call(ctx, "solveCipher", params, &result); err != nil {
		// Match the sidecar's sentinel via suffix so the call() error
		// prefix can evolve without silently breaking detection.
		if strings.HasSuffix(err.Error(), playerNotLoadedSentinel) {
			return SolveCipherResult{}, ErrPlayerNotLoaded
		}
		return SolveCipherResult{}, err
	}
	if result.SigResults == nil {
		result.SigResults = map[string]string{}
	}
	if result.NResults == nil {
		result.NResults = map[string]string{}
	}
	return result, nil
}

// ping exercises the JSON-RPC round-trip. Kept as an unexported method
// for tests after Start switched to the ready-event handshake; production
// code does not call ping directly anymore.
func (s *Sidecar) ping(ctx context.Context) error {
	var result string
	if err := s.call(ctx, "ping", nil, &result); err != nil {
		return err
	}
	if result != "pong" {
		return fmt.Errorf("unexpected ping result: %q", result)
	}
	return nil
}

// call sends a JSON-RPC request and waits for the matching response.
// Decodes the response.Result into `into` if non-nil.
func (s *Sidecar) call(ctx context.Context, method string, params map[string]any, into any) error {
	if !s.healthy.Load() {
		return errors.New("sidecar: unhealthy")
	}

	// Bound every RPC with RequestTimeout. Callers pass long-lived job
	// contexts (live streams run for days), and the sidecar's JS event loop
	// is single-threaded — one request wedged inside V8 would otherwise
	// hang its caller indefinitely while the process still looks healthy
	// (stdout stays open, so readPump never EOFs).
	if s.cfg.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.RequestTimeout)
		defer cancel()
	}

	id := s.nextReqID.Add(1)
	ch := make(chan rpcResponse, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	if err := s.writeRequest(rpcRequest{ID: id, Method: method, Params: params}); err != nil {
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return fmt.Errorf("sidecar: %s", resp.Error)
		}
		if into != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, into); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// callRaw is like call but ignores the response payload. Used for shutdown.
func (s *Sidecar) callRaw(ctx context.Context, method string, params map[string]any) error {
	return s.call(ctx, method, params, nil)
}

func (s *Sidecar) writeRequest(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("stdin write: %w", err)
	}
	return nil
}

// readPump drains stdout line-by-line. Each line is either a notification
// (no `id`, has `event`) or a JSON-RPC response (has `id`). The only
// notification today is `ready`, which closes readyCh so Start can unblock.
// Responses are routed to the pending channel keyed by request ID. Exits
// on EOF or stdout close, marking the sidecar unhealthy unless Stop has
// been called; also signals readyCh with an error if EOF arrives before
// the ready event so a Start that's still waiting fails fast instead of
// hanging until StartupTimeout.
func (s *Sidecar) readPump() {
	defer s.pumpsDone.Done()
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("sidecar: readPump panic", "panic", fmt.Sprint(r))
			// With the pump dead no response will ever arrive: unblock a
			// Start still waiting on ready and fail pending callers.
			s.readyOnce.Do(func() {
				s.readyErr = fmt.Errorf("sidecar readPump panic: %v", r)
				close(s.readyCh)
			})
			if !s.stopping.Load() {
				s.markUnhealthy("readPump panic")
			}
		}
	}()

	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1 MiB max line; PO tokens are << 1 KiB

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Notifications: no `id` field (use *uint64 to detect absence) plus
		// an `event` discriminator. Distinguished from responses before the
		// generic response-decode path so a malformed `event` line doesn't
		// trip the "stdout JSON parse failed" warning.
		var probe struct {
			ID    *uint64 `json:"id"`
			Event string  `json:"event"`
		}
		if err := json.Unmarshal(line, &probe); err == nil && probe.ID == nil && probe.Event != "" {
			switch probe.Event {
			case "ready":
				s.readyOnce.Do(func() { close(s.readyCh) })
			default:
				s.cfg.Logger.Debug("sidecar: unknown event", "event", probe.Event)
			}
			continue
		}

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			s.cfg.Logger.Warn("sidecar: stdout JSON parse failed", "err", err, "line", truncate(string(line), 200))
			continue
		}

		s.pendingMu.Lock()
		ch, ok := s.pending[resp.ID]
		s.pendingMu.Unlock()
		if !ok {
			s.cfg.Logger.Warn("sidecar: response for unknown reqID", "id", resp.ID)
			continue
		}

		select {
		case ch <- resp:
		default:
			// Channel buffer full -- caller already returned (likely via
			// ctx cancel). Discard.
			s.cfg.Logger.Debug("sidecar: discarded late response", "id", resp.ID)
		}
	}

	if err := scanner.Err(); err != nil && !s.stopping.Load() {
		s.cfg.Logger.Warn("sidecar: stdout scanner error", "err", err)
	}

	// If stdout EOF'd before the sidecar emitted ready, unblock Start with
	// an explicit error rather than letting it hang until StartupTimeout.
	// readyOnce makes this a no-op when ready was already signaled.
	s.readyOnce.Do(func() {
		s.readyErr = errors.New("sidecar exited before ready")
		close(s.readyCh)
	})

	if !s.stopping.Load() {
		s.markUnhealthy("stdout EOF")
	}
}

// stderrPump pipes the sidecar's stderr line-by-line into the Moombox logger.
// Lines emitted by server.js carry a severity prefix ([bgutil-sidecar:error]
// or [bgutil-sidecar:warn]); raw JSDOM chatter arrives unprefixed. Routing:
//   - [bgutil-sidecar:error] → Warn  (protocol violations, thrown JS errors)
//   - [bgutil-sidecar:warn]  → Debug (server-recoverable warnings)
//   - known harmless JSDOM   → silent (canvas-not-implemented noise)
//   - everything else        → Debug  (unknown unprefixed stderr)
func (s *Sidecar) stderrPump() {
	defer s.pumpsDone.Done()
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("sidecar: stderrPump panic", "panic", fmt.Sprint(r))
		}
	}()
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "[bgutil-sidecar:error]"):
			// Real server.js error — operator should see this.
			msg := strings.TrimPrefix(line, "[bgutil-sidecar:error] ")
			s.cfg.Logger.Warn("sidecar error", "line", msg)
		case strings.HasPrefix(line, "[bgutil-sidecar:warn]"):
			// Server-recoverable warning — Debug-level, only visible
			// when the operator is investigating.
			msg := strings.TrimPrefix(line, "[bgutil-sidecar:warn] ")
			s.cfg.Logger.Debug("sidecar warn", "line", msg)
		case isHarmlessJSDOMStderr(line):
			// Known harmless JSDOM chatter (e.g. canvas-not-implemented).
			// Skip silently. Adding new entries here is the right place
			// to do it because these come from JSDOM, not our code.
			continue
		default:
			// Untagged stderr we don't recognise — surface at Debug so
			// it's available for investigation but not in user logs.
			s.cfg.Logger.Debug("sidecar stderr", "line", line)
		}
	}
}

// isHarmlessJSDOMStderr reports whether a stderr line is known JSDOM
// chatter we deliberately ignore. JSDOM doesn't carry our severity
// prefix, so we still need a small allowlist for its noise.
func isHarmlessJSDOMStderr(line string) bool {
	// JSDOM emits this when the player JS calls canvas.getContext().
	// JSDOM ships without a canvas implementation by design — the npm
	// `canvas` package is a native C++ binding we can't ship cross-platform.
	return strings.Contains(line, "Not implemented: HTMLCanvasElement")
}

// markUnhealthy flips the healthy flag and drains pending requests with an
// error so callers blocked on a response wake up promptly.
func (s *Sidecar) markUnhealthy(reason string) {
	if !s.healthy.CompareAndSwap(true, false) {
		return
	}
	s.cfg.Logger.Warn("sidecar marked unhealthy", "reason", reason)
	s.drainPending(reason)
}

func (s *Sidecar) drainPending(reason string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, ch := range s.pending {
		select {
		case ch <- rpcResponse{ID: id, Error: "sidecar unhealthy: " + reason}:
		default:
		}
	}
}

// resolveCacheDir derives the on-disk extraction path. Defaults to
// %LOCALAPPDATA%/Moombox/sidecar; tests pass an explicit override.
func (s *Sidecar) resolveCacheDir() (string, error) {
	if s.cfg.CacheDir != "" {
		return s.cfg.CacheDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("user cache dir: %w", err)
	}
	return filepath.Join(base, "Moombox", "sidecar"), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
