// Package sidecar manages the embedded BotGuard Node subprocess.
//
// The sidecar is a Node.js process running our bgutil-sidecar JS code
// (JSDOM + bgutils-js) under a real V8 engine. Moombox spawns one per
// process, pipes JSON-RPC requests to it via stdin/stdout, and consumes
// PO tokens that pass Google's BotGuard timing fingerprint check (which
// our pure-goja path can't satisfy because the interpreter runs ~100x
// faster than V8 -- see "Why a Sidecar" in docs/spec/platform-services.md).
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
// Moombox does. In the other direction, the readPump goroutine watches
// stdout for EOF and marks the sidecar unhealthy when the CHILD dies (a
// crash, a V8 OOM-abort) so callers short-circuit at once and the
// supervisor brings it back.
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
	"unicode/utf8"
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
	// OnUnhealthy, when non-nil, is called ONCE each time the sidecar goes
	// from healthy to unhealthy for a reason other than Stop — stdout EOF
	// after a crash or a V8 OOM-abort, a readPump panic, or a stdin write the
	// child has not drained within RequestTimeout. It runs ON THE GOROUTINE
	// THAT NOTICED (readPump, or the stall watchdog's timer) with the health
	// flag already flipped and the pending requests already drained, so it
	// MUST NOT block: the Supervisor wired to it does a non-blocking channel
	// send and nothing else. A panic in the callback is recovered and logged
	// rather than taking that goroutine down.
	OnUnhealthy func(reason string)
	Logger      Logger
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
	// parallel must not interleave their JSON lines on the wire. A one-slot
	// channel rather than a sync.Mutex so that waiting for it can be given
	// up when the waiter's context ends: the holder may be a write the child
	// has stopped draining (see writeRequest), and a mutex would queue every
	// later call — Stop's own shutdown RPC included — behind it for as long
	// as the kernel keeps that write blocked.
	writeSem chan struct{}
	// childGen counts the children startLocked has spawned. A write's stall
	// watchdog records the generation it was armed against and condemns no
	// other: a callback delayed across a restart must not mark the
	// replacement child unhealthy for the old child's wedge.
	childGen atomic.Uint64

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

	// stderrTail keeps the child's last few stderr lines (stderrPump), so a
	// child that dies before ready can say why: Node's own fatal errors —
	// a module that will not load, a missing shared library — are
	// unprefixed and logged at Debug, and "sidecar exited before ready" was
	// all the operator was told. Reset at every start.
	stderrMu   sync.Mutex
	stderrTail []string
	// reextracted latches the one re-extraction a child that died before
	// ready earns: a tree that is broken under a valid stamp (a payload cut
	// short by a crash) is never re-extracted otherwise, while one that
	// still fails after a fresh extraction is the environment, not the
	// files, and is not rewritten again every retry.
	reextracted atomic.Bool

	// Lifecycle. stopping signals to readPump/stderrPump that a teardown is
	// under way and they should exit silently rather than mark unhealthy.
	stopping  atomic.Bool
	pumpsDone sync.WaitGroup

	// Lifecycle serialisation. Start, Stop and Restart all mutate the process
	// fields above and each other's guards, and in production they run on
	// different goroutines: shutdown.go calls Stop while the supervisor loop
	// may be inside Restart. lifecycleMu makes exactly one of them run at a
	// time, so the reset of a dead child's state can never interleave with a
	// teardown reading it. It is held ACROSS Start (Restart's included).
	lifecycleMu sync.Mutex

	// stateMu guards the two fields Stop must reach WITHOUT waiting for
	// lifecycleMu. That is what lets a Stop cancel a Restart already inside
	// Start instead of queueing behind its whole StartupTimeout budget.
	stateMu sync.Mutex
	stopped bool // Stop() was called; terminal
	// Cancels the RUNNING Restart, if any — registered under lifecycleMu and
	// cleared before it is released, so a Restart merely queued for that lock
	// can never displace the one Stop has to cancel.
	restartCancel context.CancelFunc

	// start is a test seam for the one transition a unit test cannot run (it
	// spawns Node). Production leaves it as startLocked.
	start func(ctx context.Context) error
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

// ErrStopped is returned by Restart once Stop has been called. Stop is
// terminal — cmd/moombox only calls it from the shutdown path — so a
// supervisor restart that races shutdown must not bring the child back.
var ErrStopped = errors.New("sidecar: stopped")

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
	s := &Sidecar{
		cfg:      cfg,
		pending:  make(map[uint64]chan rpcResponse),
		writeSem: make(chan struct{}, 1),
	}
	s.start = s.startLocked
	return s
}

// Start extracts the embedded blobs (if needed), launches the Node
// subprocess pinned to a Windows Job Object, and waits for the sidecar
// to emit a `ready` notification on stdout signaling that server.js has
// finished its synchronous init (jsdom module load + JSDOM construction).
// Returns an error if extraction, launch, or the ready handshake fails;
// the caller should fall back to the goja path on error.
func (s *Sidecar) Start(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.startLocked(ctx)
}

// startLocked is Start's body. The caller holds lifecycleMu — which is why
// its failure paths call teardownLocked rather than Stop: Stop would deadlock
// on the mutex, and worse, it would latch the terminal stopped flag and make
// every future supervisor restart a no-op.
func (s *Sidecar) startLocked(ctx context.Context) error {
	// The terminal latch, not just the live child. teardownLocked ends by
	// clearing s.cmd, so the guard below alone would let a Start after a real
	// Stop extract, spawn and hand-shake a Node child that nothing supervises
	// and nothing reaps. Restart already latches before it reaches here, so
	// this costs one atomic read on the one path that can still be wrong.
	if s.isStopped() {
		return ErrStopped
	}
	if s.cmd != nil {
		return errors.New("sidecar: already started")
	}
	// A start whose context has already ended is over before it begins. It
	// used to extract, spawn Node and only then see the cancelled handshake,
	// spending the 2 s teardown grace of a quitting process's shutdown budget
	// on a child nobody would use.
	if err := ctx.Err(); err != nil {
		return err
	}

	cacheDir, err := s.resolveCacheDir()
	if err != nil {
		return err
	}
	s.cacheDir = cacheDir

	if err := extractIfNeeded(cacheDir); err != nil {
		return fmt.Errorf("extract sidecar payload: %w", err)
	}
	if err := ctx.Err(); err != nil { // extraction can take seconds
		return err
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

	// Under the write slot, mirroring Restart's own reset of the same fields:
	// writeRequest reads s.stdin under that lock, and a caller stalled
	// between its healthy.Load() and writeRequest can span the whole restart
	// window. The race detector cannot see it (the stall has to cross the
	// ladder's 5 s floor), which is exactly why the lock is the fix rather
	// than an argument that it cannot happen. Uncancellable on purpose: the
	// only holder that can keep us waiting is a write stranded on the child
	// teardownLocked has just killed, and that returns the moment the kernel
	// reports the broken pipe.
	s.writeSem <- struct{}{}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderr
	s.readyCh = make(chan struct{})
	s.childGen.Add(1)
	<-s.writeSem

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
	s.writeSem <- struct{}{}
	s.job = job
	<-s.writeSem

	s.stderrMu.Lock()
	s.stderrTail = nil
	s.stderrMu.Unlock()
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
			_ = s.teardownLocked() // joins the pumps: the stderr tail is complete
			if !s.reextracted.Swap(true) {
				if err := os.Remove(filepath.Join(cacheDir, "version.txt")); err == nil {
					s.cfg.Logger.Warn("sidecar exited before ready; its extracted files will be rewritten on the next start",
						"cacheDir", cacheDir)
				}
			}
			return fmt.Errorf("ready: %w%s", s.readyErr, s.stderrTailSuffix())
		}
	case <-readyCtx.Done():
		_ = s.teardownLocked()
		return fmt.Errorf("ready: %w%s", readyCtx.Err(), s.stderrTailSuffix())
	}

	s.healthy.Store(true)
	s.cfg.Logger.Info("sidecar started", "cacheDir", cacheDir, "pid", cmd.Process.Pid)
	return nil
}

// Stop gracefully shuts down the sidecar. Sends a shutdown JSON-RPC,
// waits briefly, then hard-kills + closes the Job Object. Safe to call
// multiple times; subsequent calls are no-ops.
//
// Total wall-time bound: ~3s (1s for the JSON-RPC bye response, 2s for
// the process to exit on its own, then Kill). This stays well below the
// shutdown.go force-exit budget so a hung sidecar can't starve the rest
// of Moombox's shutdown sequence (web server, DB unsubscribe, DB close).
//
// Stop is also TERMINAL: once it has been called the handle stays down, and a
// Restart racing it from the supervisor goroutine returns ErrStopped rather
// than resurrecting a child nothing will reap.
//
// The latch and the cancel happen BEFORE lifecycleMu is taken, on purpose. A
// Restart in flight holds that mutex for its whole StartupTimeout budget, so
// queueing behind it would add up to a minute to shutdown; cancelling its
// context instead makes the restart unwind and hand the mutex over.
func (s *Sidecar) Stop() error {
	s.stateMu.Lock()
	s.stopped = true
	cancel := s.restartCancel
	s.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.teardownLocked()
}

// isStopped reports whether Stop has been called.
func (s *Sidecar) isStopped() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.stopped
}

// teardownLocked stops whatever child exists and leaves the handle quiescent.
// The caller holds lifecycleMu.
//
// Idempotent by SHAPE rather than by a sync.Once: it clears s.cmd on the way
// out, so a second call takes the no-child fast path. The Once it replaces was
// the bug — Restart had to reset it, and that reset raced Stop's Do.
func (s *Sidecar) teardownLocked() error {
	var firstErr error
	// Mark stopping so readPump exits silently on EOF instead of
	// flagging the sidecar unhealthy mid-shutdown. Crucially, do NOT
	// flip s.healthy yet -- the graceful shutdown JSON-RPC below goes
	// through call(), which short-circuits on !healthy.
	s.stopping.Store(true)

	if s.cmd == nil || s.cmd.Process == nil {
		s.healthy.Store(false)
		return nil
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

	// Last: with no cmd, a second teardown takes the fast path above. This is
	// what makes teardownLocked idempotent without a sync.Once for Restart to
	// reset — and Restart's own reset can rely on the pumps being gone.
	s.cmd = nil
	return firstErr
}

// Restart brings a dead sidecar back ON THE SAME HANDLE: it stops whatever is
// left of the old child, resets the per-process state, and runs Start again.
//
// The handle's identity is preserved deliberately. cmd/moombox stores one
// *Sidecar on its run state for shutdown and hands the same pointer to
// PotProvider, so swapping in a fresh instance would leave both pointing at a
// corpse — the shutdown path would stop the dead one and leak the live one.
// What DOES have to be rebuilt is the cipher sidecar solver: its per-player
// "already sent" map describes the memory of the child that just died. That
// rebuild belongs to the caller (Supervisor.SetOnUp).
//
// Restart is serialised against Stop and against itself by lifecycleMu, and it
// loses to Stop: a restart that starts after Stop is a no-op returning
// ErrStopped, and a Stop that arrives mid-restart cancels it. Concurrent RPC
// callers are safe throughout: healthy is false for the whole window, call()
// short-circuits on it, and writeRequest refuses a nil stdin.
func (s *Sidecar) Restart(ctx context.Context) error {
	// A context of our own so Stop can cut the startup short without touching
	// the caller's.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cheap entry check off the lifecycle mutex: a Restart that arrives after
	// shutdown gives up without queueing behind anything.
	if s.isStopped() {
		return ErrStopped
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// Register the cancel only once THIS Restart is the one running. Doing it
	// before the lock let a second, queued Restart overwrite the running one's
	// cancel, and its deferred clear (which ran after the unlock, LIFO) then
	// wiped the survivor's — leaving Stop with nothing to cancel and a whole
	// startup budget to wait out on lifecycleMu.
	//
	// The latch is re-read inside the same stateMu section that registers, so
	// this is atomic against Stop's latch-then-read: either Stop sees this
	// cancel, or this sees Stop's latch and never reaches start.
	s.stateMu.Lock()
	if s.stopped {
		s.stateMu.Unlock()
		return ErrStopped
	}
	s.restartCancel = cancel
	s.stateMu.Unlock()
	// Registered AFTER defer s.lifecycleMu.Unlock(), so LIFO runs it FIRST:
	// the slot is cleared while this Restart still holds the lifecycle lock,
	// before any queued Restart can put its own cancel there.
	defer func() {
		s.stateMu.Lock()
		s.restartCancel = nil
		s.stateMu.Unlock()
	}()

	// teardownLocked is idempotent and, when a child existed, waits for both
	// pumps — so afterwards nothing else touches the fields reset below.
	_ = s.teardownLocked()

	// Uncancellable, like startLocked's: teardownLocked killed the child, so
	// a writer still holding the slot is about to get its broken pipe back.
	s.writeSem <- struct{}{}
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.stderr = nil
	s.job = nil
	s.readyOnce = sync.Once{}
	s.readyCh = nil
	s.readyErr = nil
	s.stopping.Store(false)
	s.healthy.Store(false)
	<-s.writeSem

	s.pendingMu.Lock()
	s.pending = make(map[uint64]chan rpcResponse)
	s.pendingMu.Unlock()

	if err := s.start(rctx); err != nil {
		// Stop cancelled rctx: the start did not fail, it was called off. The
		// raw context error would read as a transient failure to the
		// supervisor — a spurious "restart failed" Warn on an ordinary
		// shutdown, and one more ladder rung burnt against a handle that can
		// never come back.
		if s.isStopped() {
			return ErrStopped
		}
		return err
	}

	// Stop latched while the child was coming up: it cancelled rctx, but a
	// start that had already seen its ready event returns nil and leaves a
	// live Node process behind. Take it back down — Stop wins.
	if s.isStopped() {
		_ = s.teardownLocked()
		return ErrStopped
	}
	return nil
}

// IsHealthy reports whether the sidecar is currently usable. False after
// Start fails, after Stop is called, after readPump observes stdout EOF (the
// child died), or after a stdin write stalled past RequestTimeout (the child
// stopped reading — see writeRequest).
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

// GeneratePlayerPoToken asks the sidecar to mint a PO token for the
// Innertube PLAYER request, bound to a videoID (yt-dlp's
// PoTokenContext.PLAYER -> (video_id, VIDEO_ID) mapping). Unlike
// GenerateGvsPoToken this does NOT set freshMinter: player calls happen on
// every probe/refresh (every live job re-fetches every few minutes, plus
// monitor polls), so forcing a fresh BotGuard run per call would be a
// severe regression. The sidecar's own cached minter (~6h TTL, shared
// across bindings) still serves this call when valid; challenge, when
// non-empty (the watch page's attestation challenge), is only consulted by
// the sidecar WHEN it actually has to build a new minter — keeping player
// tokens session-coherent with GVS tokens without adding mint cost. Pass ""
// when no watch-page challenge is available (falls back to the sidecar's
// /att/get flow, today's behavior).
func (s *Sidecar) GeneratePlayerPoToken(ctx context.Context, binding, challenge string) (string, error) {
	params := map[string]any{"binding": binding}
	if challenge != "" {
		params["challenge"] = challenge
	}
	var result struct {
		PoToken string `json:"poToken"`
	}
	if err := s.call(ctx, "generatePoToken", params, &result); err != nil {
		return "", err
	}
	if result.PoToken == "" {
		return "", errors.New("sidecar returned empty poToken")
	}
	return result.PoToken, nil
}

// GvsMintResult carries a minted GVS PO token plus the provenance fields the
// worker's "[POT] GVS mint" log line reports (spec §4.6): which challenge
// source built the minter and whether it was regenerated for this mint.
type GvsMintResult struct {
	PoToken      string `json:"poToken"`
	MinterSource string `json:"minterSource"` // "homepage" | "challenge" | "att_get"
	MinterFresh  bool   `json:"minterFresh"`
}

// GenerateGvsPoToken mints a GVS (segment-URL) PO token with the
// fresh-minter-per-mint policy: the sidecar regenerates its BotGuard minter
// for this call. Challenge sourcing follows upstream 495a47f's preference
// order: the sidecar's own homepage (ytcfg, ytAtN) pair first — the
// EVENT_ID-coherent source the binding experiment demands — then the
// supplied watch-page challenge when non-empty, then its /att/get fetch.
func (s *Sidecar) GenerateGvsPoToken(ctx context.Context, binding, challenge string) (GvsMintResult, error) {
	params := map[string]any{"binding": binding, "freshMinter": true}
	if challenge != "" {
		params["challenge"] = challenge
	}
	var result GvsMintResult
	if err := s.call(ctx, "generatePoToken", params, &result); err != nil {
		return GvsMintResult{}, err
	}
	if result.PoToken == "" {
		return GvsMintResult{}, errors.New("sidecar returned empty poToken")
	}
	return result, nil
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

// Stats holds the sidecar's internal counters as getStats reports them.
// Nothing in production reads them today — the tests exercise the
// round-trip — and CachedSessions is always 0: server.js keeps no session
// cache (that layer lives in PotProvider) and only ever resets the counter.
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

	if err := s.writeRequest(ctx, rpcRequest{ID: id, Method: method, Params: params}); err != nil {
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

// lockWrite takes the stdin write slot, or gives up when ctx ends first. The
// field resets in startLocked and Restart take the slot directly instead,
// because waiting out a stranded write is their point: they run after
// teardownLocked has killed the child that was holding it up.
func (s *Sidecar) lockWrite(ctx context.Context) error {
	select {
	case s.writeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockWrite releases the slot lockWrite took.
func (s *Sidecar) unlockWrite() { <-s.writeSem }

// writeRequest serialises one request line onto the child's stdin. ctx bounds
// BOTH the wait for the write slot and the write itself. A child that has
// stopped reading stdin — V8 wedged, or a solveCipher preprocess running
// synchronously on its event loop — lets the pipe fill, and a SolveCipher
// line carrying the player JS is ~3 MB, so the Write blocks until the child
// drains it or dies. The write therefore runs on its own goroutine, which
// keeps the slot until the kernel lets the Write return, while the caller
// is free to leave at ctx.Done(). Leaving does not cancel the line: it still
// lands whole, and the next writer still waits behind it.
//
// A Write that outlasts RequestTimeout is the same failure as a request
// wedged inside V8 and is handled the same way: the sidecar is marked
// unhealthy so the supervisor replaces the child, and killing the child is
// what frees the stranded writer. (Closing our end is not enough everywhere:
// on Windows an anonymous pipe's WriteFile in progress outlives CloseHandle
// on the parent's side and returns only once the child's end is gone.)
func (s *Sidecar) writeRequest(ctx context.Context, req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	stdin := s.stdin
	if stdin == nil {
		// Between a crash and the supervisor's Restart there is no pipe. A
		// caller that passed the healthy check microseconds before
		// markUnhealthy flipped it must get an error, not a nil dereference.
		s.unlockWrite()
		return errors.New("sidecar: not running")
	}
	gen := s.childGen.Load()

	done := make(chan error, 1)
	go func() {
		var werr error
		defer func() {
			if r := recover(); r != nil {
				werr = fmt.Errorf("stdin write panic: %v", r)
			}
			s.unlockWrite()
			done <- werr
		}()
		if s.cfg.RequestTimeout > 0 {
			stall := time.AfterFunc(s.cfg.RequestTimeout, func() {
				defer func() {
					if r := recover(); r != nil {
						s.cfg.Logger.Error("sidecar: stall watchdog panic", "panic", fmt.Sprint(r))
					}
				}()
				// Only the child this write was armed against, and not one
				// a teardown is already taking down.
				if s.childGen.Load() != gen || s.stopping.Load() {
					return
				}
				s.markUnhealthy("stdin write stalled for " + s.cfg.RequestTimeout.String())
			})
			defer stall.Stop()
		}
		_, werr = stdin.Write(data)
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("stdin write: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

	// Scan stops on the child's exit (a real EOF) or on an error of its own —
	// a line past the 1 MiB cap, a read failure — with the child possibly
	// still alive. The reason is what the dashboard, the Discord embed and
	// the supervisor log all show, so it says which.
	reason := "stdout EOF"
	if err := scanner.Err(); err != nil {
		reason = "stdout read: " + err.Error()
		if !s.stopping.Load() {
			s.cfg.Logger.Warn("sidecar: stdout scanner error", "err", err)
		}
	}

	// If stdout EOF'd before the sidecar emitted ready, unblock Start with
	// an explicit error rather than letting it hang until StartupTimeout.
	// readyOnce makes this a no-op when ready was already signaled.
	s.readyOnce.Do(func() {
		s.readyErr = errors.New("sidecar exited before ready")
		close(s.readyCh)
	})

	if !s.stopping.Load() {
		s.markUnhealthy(reason)
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
		if !isHarmlessJSDOMStderr(line) {
			s.noteStderr(line)
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

// stderrTailLines and stderrTailLineBytes bound what stderrTail keeps.
const (
	stderrTailLines     = 6
	stderrTailLineBytes = 300
)

// noteStderr adds one stderr line to stderrTail, keeping the last few. A
// Node fatal error ends in stack frames, a "{ code: … }" block and a
// "Node.js vX" footer; kept, those pushed the line that says what went wrong
// out of the tail, so they are not kept.
func (s *Sidecar) noteStderr(line string) {
	if !informativeStderr(line) {
		return
	}
	if len(line) > stderrTailLineBytes {
		cut := stderrTailLineBytes
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	s.stderrTail = append(s.stderrTail, line)
	if len(s.stderrTail) > stderrTailLines {
		s.stderrTail = s.stderrTail[len(s.stderrTail)-stderrTailLines:]
	}
}

// informativeStderr reports whether a stderr line says something beyond a
// stack frame or a bracket — see noteStderr.
func informativeStderr(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "at ") || strings.HasPrefix(t, "Node.js v") {
		return false
	}
	return strings.Trim(t, "{}^ ") != ""
}

// stderrTailSuffix renders stderrTail for an error that ends a start: empty
// when the child said nothing, else " — the child said: …".
func (s *Sidecar) stderrTailSuffix() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	if len(s.stderrTail) == 0 {
		return ""
	}
	parts := make([]string, len(s.stderrTail))
	for i, l := range s.stderrTail {
		parts[i] = strings.TrimSpace(l)
	}
	return " — the child said: " + strings.Join(parts, " | ")
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
// error so callers blocked on a response wake up promptly, then hands the
// reason to OnUnhealthy so a supervisor can bring the child back.
func (s *Sidecar) markUnhealthy(reason string) {
	if !s.healthy.CompareAndSwap(true, false) {
		return
	}
	s.cfg.Logger.Warn("sidecar marked unhealthy", "reason", reason)
	s.drainPending(reason)
	if s.cfg.OnUnhealthy != nil {
		s.callOnUnhealthy(reason)
	}
}

// callOnUnhealthy isolates the callback from readPump: a panic in a
// supervisor's notify path must not kill the goroutine draining stdout.
func (s *Sidecar) callOnUnhealthy(reason string) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("sidecar: OnUnhealthy panic", "panic", fmt.Sprint(r))
		}
	}()
	s.cfg.OnUnhealthy(reason)
}

func (s *Sidecar) drainPending(reason string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, ch := range s.pending {
		select {
		// call() prefixes every response error with "sidecar: ", as it does
		// the child's own, so the caller reads "sidecar: unhealthy: stdout
		// EOF" — one prefix, not the doubled "sidecar: sidecar unhealthy: …".
		case ch <- rpcResponse{ID: id, Error: "unhealthy: " + reason}:
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
