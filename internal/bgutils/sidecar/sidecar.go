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
// to 5s; RequestTimeout defaults to 30s.
type Config struct {
	CacheDir       string
	StartupTimeout time.Duration
	RequestTimeout time.Duration
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

	// Lifecycle. closed signals to readPump/stderrPump that Stop() was
	// called and they should exit silently rather than mark unhealthy.
	stopOnce  sync.Once
	stopping  atomic.Bool
	pumpsDone sync.WaitGroup
}

type rpcRequest struct {
	ID     uint64                 `json:"id"`
	Method string                 `json:"method"`
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
		cfg.StartupTimeout = 5 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	return &Sidecar{
		cfg:     cfg,
		pending: make(map[uint64]chan rpcResponse),
	}
}

// Start extracts the embedded blobs (if needed), launches the Node
// subprocess pinned to a Windows Job Object, and waits for a ping/pong
// handshake. Returns an error if extraction, launch, or handshake fails;
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

	nodeExe := filepath.Join(cacheDir, "node.exe")
	serverJS := filepath.Join(cacheDir, "src", "server.js")

	cmd := exec.CommandContext(context.Background(), nodeExe, serverJS)
	cmd.Dir = cacheDir
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

	// Pin the child to a Job Object so it dies when Moombox dies. On
	// non-Windows builds processJob is a no-op (the package is currently
	// Windows-only per Moombox's platform target).
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

	// Mark healthy BEFORE the ping so the call() check passes; reverted on
	// handshake failure.
	s.healthy.Store(true)

	pingCtx, cancel := context.WithTimeout(ctx, s.cfg.StartupTimeout)
	defer cancel()
	if err := s.ping(pingCtx); err != nil {
		s.healthy.Store(false)
		_ = s.Stop()
		return fmt.Errorf("ping: %w", err)
	}

	s.cfg.Logger.Info("sidecar started", "cacheDir", cacheDir, "pid", cmd.Process.Pid)
	return nil
}

// Stop gracefully shuts down the sidecar. Sends a shutdown JSON-RPC,
// waits up to 5s, then hard-kills + closes the Job Object.
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
		// next tick.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.callRaw(shutdownCtx, "shutdown", nil)

		// Now stop accepting new work; any inflight requests waiting on
		// channels will be drained below.
		s.healthy.Store(false)

		// Wait briefly for the process to exit on its own. Bound by 5s
		// (independent of StartupTimeout) -- the Node side already
		// scheduled process.exit(0) so this is just signal latency.
		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				s.cfg.Logger.Debug("sidecar exited", "err", err)
			}
		case <-time.After(5 * time.Second):
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

// CacheDir returns the directory where the sidecar's node.exe and JS
// source live, or empty string if Start has not been called. Useful for
// diagnostics and tests.
func (s *Sidecar) CacheDir() string { return s.cacheDir }

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

// ping is the startup health check; not exported because the only legitimate
// use is during Start.
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

// readPump drains stdout line-by-line, decodes each as a JSON-RPC response,
// and routes it to the pending channel keyed by request ID. Exits on EOF
// or stdout close, marking the sidecar unhealthy unless Stop has been called.
func (s *Sidecar) readPump() {
	defer s.pumpsDone.Done()

	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1 MiB max line; PO tokens are << 1 KiB

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
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

	if !s.stopping.Load() {
		s.markUnhealthy("stdout EOF")
	}
}

// stderrPump pipes the sidecar's stderr line-by-line into the Moombox logger
// at Debug level. Useful for surfacing JSDOM warnings ("Not implemented:
// HTMLCanvasElement.getContext()") and BotGuard error traces during
// development.
func (s *Sidecar) stderrPump() {
	defer s.pumpsDone.Done()
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.cfg.Logger.Debug("sidecar stderr", "line", line)
	}
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
