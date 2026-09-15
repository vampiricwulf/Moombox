package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	wsWriteTimeout   = 10 * time.Second
	wsPingInterval   = 30 * time.Second
	wsMaxMessageSize = 1024 * 1024 // 1MB (match TS maxPayload)
	maxLogBuffer     = 200         // Trim log ring buffer to this size
	// wsReadIdleTimeout bounds how long a single Conn.Read may block
	// waiting for a frame. Longer than 2× wsPingInterval (30s) so that
	// a normally-responsive client — which keeps the peer alive via
	// automatic pong replies — is never closed mid-session. If the
	// peer goes silent AND the ping loop somehow still succeeds (e.g.,
	// kernel-level TCP keepalives ACKing past an app-level zombie),
	// the Read will eventually error out and readPump will tear down.
	wsReadIdleTimeout = 90 * time.Second
	// wsWriteQueueSize bounds the per-client outbound queue so a stalled
	// client can't accumulate unbounded backpressure (memory blow-up plus
	// a 10s× per-message Broadcast stall on every subsequent send). On
	// queue overflow the oldest queued frame is dropped with a warn log.
	// 16 is comfortably above the typical burst (initial state + a couple
	// of job updates) without letting a slow client buffer megabytes of
	// stale state. Audit reports/web.md C-7.
	wsWriteQueueSize = 16
)

// WSMessage is a WebSocket message sent to clients.
// Uses "payload" field to match the TypeScript frontend protocol.
type WSMessage struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// InitialStateProvider supplies data for the initial state message.
type InitialStateProvider func() map[string]any

// WebSocketHub manages WebSocket connections and broadcasts.
//
// hub.mu protects the clients map and closed flag. RWMutex matches
// logBufMu's shape for consistency, and lets Broadcast's snapshot and
// ClientCount use the cheaper RLock path. Audit reports/web.md Q-20.
type WebSocketHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	closed  bool

	// Initial state provider (set by main.go)
	InitialState InitialStateProvider

	// AuthCheck verifies whether a WebSocket upgrade request is allowed.
	// Called during upgrade for external (non-loopback, non-private) connections
	// when auth is required. Return true to accept, false to reject.
	// If nil, all connections are accepted (no auth configured).
	AuthCheck func(r *http.Request) bool

	// ClientIP resolves the effective client IP for the upgrade-path trust
	// decision (trusted_proxies / X-Forwarded-For aware). Set by NewServer;
	// nil falls back to the raw peer address.
	ClientIP func(*http.Request) string

	// Log buffer for initial state (ring buffer)
	logBufMu sync.RWMutex
	logBuf   []string

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

type wsClient struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// writes is the per-client outbound queue drained by writePump. A
	// closed `writes` (set by removeClient) signals the writePump to exit
	// without depending on ctx cancellation timing. Audit reports/web.md
	// C-7.
	writes chan []byte

	// drops counts frames this client will never receive. Incremented on
	// every queue-overflow drop; see noteLag for why the LINE is rate
	// limited while the count is not.
	drops atomic.Uint64
	// lastLagLog is the UnixNano of the last "WS client lagging" line.
	lastLagLog atomic.Int64
	// needsResync is set by a drop and CLAIMED by the next enqueue, which
	// then sends the full initial-state snapshot in place of an incremental
	// frame. Claimed with CompareAndSwap before the snapshot is built, so a
	// racing drop can only ever cause an EXTRA snapshot, never a missed one.
	needsResync atomic.Bool
}

// NewWebSocketHub creates a new WebSocket hub.
func NewWebSocketHub(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *WebSocketHub {
	return &WebSocketHub{
		clients: make(map[*wsClient]struct{}),
		logger:  logger,
	}
}

// HandleUpgrade is the HTTP handler for WebSocket upgrade.
func (hub *WebSocketHub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Verify authentication for external connections (matching TypeScript verifyWsClient)
	if hub.AuthCheck != nil {
		ip := ExtractIP(r)
		if hub.ClientIP != nil {
			ip = hub.ClientIP(r)
		}
		if !isLoopback(ip) && !isPrivateIP(ip) {
			if !hub.AuthCheck(r) {
				hub.logger.Debug("websocket upgrade rejected: auth required", "ip", ip)
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Validate same-origin: nhooyr.io/websocket checks Origin vs Host by default.
		// Override with a custom check to allow loopback/LAN variants (localhost,
		// 127.0.0.1, 192.168.x.x, etc.) that may not match Host exactly.
		OriginPatterns: hub.allowedOriginPatterns(r),
	})
	if err != nil {
		hub.logger.Error("websocket upgrade failed", "err", err)
		return
	}

	// Set read limit to prevent oversized messages
	conn.SetReadLimit(int64(wsMaxMessageSize))

	// Use a detached context so the WebSocket connection is NOT killed by the
	// HTTP server's ReadTimeout. The request context (r.Context()) is cancelled
	// after ReadTimeout (30s), which would close the WebSocket prematurely.
	// The connection lifetime is managed by pingPump/readPump instead.
	ctx, cancel := context.WithCancel(context.Background())
	client := &wsClient{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
		writes: make(chan []byte, wsWriteQueueSize),
	}

	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	clientCount := len(hub.clients)
	hub.mu.Unlock()

	hub.logger.Debug("websocket connected", "clients", clientCount)

	// Per-client write loop drains `writes`. sendInitialState below writes
	// DIRECTLY to the conn (not through the queue) so the snapshot can't be
	// displaced by queue-overflow drops; that's safe because coder/websocket
	// serializes concurrent writers internally — a broadcast queued during
	// the initial-state write waits its turn rather than interleaving.
	go hub.writePump(client)

	// Send initial state immediately
	hub.sendInitialState(client)

	// Start server-initiated ping goroutine to keep connection alive
	go hub.pingPump(client)

	// Keep connection alive and handle messages
	go hub.readPump(client)
}

// writePump drains client.writes onto the websocket connection one frame
// at a time. Per-client serialisation lets Broadcast's hot path enqueue
// non-blocking and decouple slow consumers from fast ones. Audit
// reports/web.md C-7.
func (hub *WebSocketHub) writePump(client *wsClient) {
	defer func() {
		if r := recover(); r != nil {
			hub.logger.Error("panic in websocket writePump", "panic", r)
		}
	}()
	for {
		select {
		case <-client.ctx.Done():
			return
		case msg, ok := <-client.writes:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
			err := client.conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				hub.removeClient(client, "write failed")
				return
			}
		}
	}
}

// wsLagLogInterval bounds how often ONE client's drops may produce a log line.
const wsLagLogInterval = 30 * time.Second

// noteLag emits at most one "WS client lagging" line per client per
// wsLagLogInterval. The DROP COUNT is incremented by the caller — the two
// call sites disagree about whether the dropped frame still needs a resync,
// and a helper that hid that decision would be the easiest place to get it
// wrong.
//
// The rate limit is the T1-9 fix: queueOrDrop used to Warn on every drop, and
// the app logger's subscriber (cmd/moombox/monitor_callbacks.go) broadcasts
// each line back to every client — including the full queue that just dropped,
// which dropped again and warned again. One lagging client could hold that loop
// up for a 10 s stalled write, writing the log file and every per-job buffer on
// each turn. Debug rather than Warn because the resync above already recovers
// the frame, and at most one line per client per 30 s because that bound is
// what actually breaks the loop.
func (hub *WebSocketHub) noteLag(client *wsClient) {
	now := time.Now().UnixNano()
	last := client.lastLagLog.Load()
	if now-last < int64(wsLagLogInterval) {
		return
	}
	if !client.lastLagLog.CompareAndSwap(last, now) {
		return // another goroutine just logged for this client
	}
	hub.logger.Debug("WS client lagging", "drops", client.drops.Load())
}

// logDropTotal reports one client's lifetime drop count as it goes away.
// Called from readPump's deferred cleanup, which is the ONE teardown point
// every removal path reaches: removeClient, pingPump and Close all cancel
// client.ctx, which unblocks Conn.Read and runs that defer exactly once.
func (hub *WebSocketHub) logDropTotal(client *wsClient) {
	if n := client.drops.Load(); n > 0 {
		hub.logger.Debug("WS client disconnected after dropping frames", "drops", n)
	}
}

// queueOrDrop pushes a marshalled frame into a client's write queue.
// On a full queue the OLDEST queued frame is dropped (it's stale state
// for a client that's already behind) and the new frame replaces it.
// Returns false when the client is gone (channel closed by
// removeClient). Audit reports/web.md C-7.
func (hub *WebSocketHub) queueOrDrop(client *wsClient, msg []byte) bool {
	defer func() {
		// Recover from a send-on-closed-channel race with removeClient.
		if r := recover(); r != nil {
			// Already removed — caller doesn't need to retry.
		}
	}()

	// Claim a pending resync BEFORE building anything: CompareAndSwap means a
	// drop racing this call re-arms the flag and costs at most one extra
	// snapshot, where clearing it afterwards could swallow that drop entirely.
	resync := client.needsResync.CompareAndSwap(true, false)
	if resync {
		if snap := hub.initialStateBytes(); snap != nil {
			msg = snap
		} else {
			client.needsResync.Store(true) // try again on the next frame
			resync = false
		}
	}

	select {
	case client.writes <- msg:
		return true
	default:
	}

	// Drop oldest, then push the new frame. If we lose another race,
	// the new frame is dropped silently (caller sees true).
	select {
	case <-client.writes:
		client.drops.Add(1)
		if !resync {
			// A frame the client never saw is gone and nothing queued after it
			// restates it. When we ARE carrying a snapshot, the frame we just
			// evicted is older than that snapshot and therefore superseded —
			// re-arming there would hand a permanently-full queue a fresh
			// full-state marshal on every single broadcast.
			client.needsResync.Store(true)
		}
		hub.noteLag(client)
	default:
	}
	select {
	case client.writes <- msg:
		return true
	default:
		client.drops.Add(1)
		client.needsResync.Store(true)
		hub.noteLag(client)
		return true
	}
}

// removeClient closes the client's write channel + cancels its ctx,
// removes it from the hub, and closes the underlying websocket. Safe
// to call multiple times — idempotent on the closed-channel guard.
func (hub *WebSocketHub) removeClient(client *wsClient, reason string) {
	hub.mu.Lock()
	if _, ok := hub.clients[client]; !ok {
		hub.mu.Unlock()
		return
	}
	delete(hub.clients, client)
	hub.mu.Unlock()

	client.cancel()
	// Close on a closed channel panics; the recover() in queueOrDrop
	// guards the senders. The drain in writePump exits on `!ok`.
	defer func() {
		_ = recover()
	}()
	close(client.writes)
	client.conn.Close(websocket.StatusInternalError, reason)
}

// allowedOriginPatterns builds a list of host patterns for the WebSocket
// upgrade. The library already allows same-origin (Origin host == Request
// host), so these patterns only need to cover cross-origin localhost/LAN
// aliases. Patterns are matched against the Origin header's host using
// filepath.Match.
//
// Audit reports/web.md S-17 — when the TLS certificate is loaded, its SANs
// (DNSNames + IPAddresses) become the trusted hostname allowlist. r.Host
// is attacker-controlled (HTTP Host header), so falling back to it could
// let a browser pointed at a malicious DNS entry mapping to 127.0.0.1
// pass the origin check. Cert SANs are server-controlled, so trusting
// them is safe.
func (hub *WebSocketHub) allowedOriginPatterns(r *http.Request) []string {
	var patterns []string

	// Cert-SAN allowlist — preferred trust source. Only set after
	// LoadOrGenerateTLSConfig has run; before that we fall back to the
	// historical r.Host derivation (cert SANs land before the listener
	// starts so this is only ever nil in test harnesses).
	if CurrentCertSANs != nil {
		for _, san := range CurrentCertSANs.SANs() {
			patterns = append(patterns, san, san+":*")
			// Loopback aliasing — cert covers 127.0.0.1, browsers may
			// present "localhost" in the Origin header.
			if san == "127.0.0.1" || san == "::1" {
				patterns = append(patterns, "localhost", "localhost:*")
			}
		}
	}

	host := r.Host
	if host == "" {
		return patterns
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	patterns = append(patterns, hostname+":*")
	if hostname == "127.0.0.1" || hostname == "::1" {
		patterns = append(patterns, "localhost", "localhost:*")
	}
	if strings.EqualFold(hostname, "localhost") {
		patterns = append(patterns, "127.0.0.1", "127.0.0.1:*")
	}
	return patterns
}

// initialStateBytes marshals the snapshot a freshly-connected client receives.
// Returns nil when it cannot be marshalled. Shared with queueOrDrop's resync
// path so a re-synced client is handed byte-for-byte what a new connection
// gets — one payload shape, one client-side handler.
//
// Locking: takes only logBufMu, and callers must hold neither hub.mu nor
// logBufMu. queueOrDrop reaches this from inside Broadcast, i.e. from inside a
// database subscriber; that is safe because Database dispatches subscribers
// AFTER releasing db.mu, Broadcast releases hub.mu before its enqueue loop,
// BroadcastLog releases logBufMu before broadcasting, and the backfill producer
// releases backfillMu before its Broadcast. Do not move any of those unlocks
// inside a broadcast.
func (hub *WebSocketHub) initialStateBytes() []byte {
	var data map[string]any
	if hub.InitialState != nil {
		data = hub.InitialState()
	}
	if data == nil {
		data = make(map[string]any)
	}

	// Include log buffer (fallback if InitialState didn't provide logs)
	if data["logs"] == nil {
		hub.logBufMu.RLock()
		if hub.logBuf != nil {
			data["logs"] = hub.logBuf
		}
		hub.logBufMu.RUnlock()
	}

	msgBytes, err := json.Marshal(WSMessage{Type: "initial_state", Payload: data})
	if err != nil {
		hub.logger.Error("failed to marshal initial state", "err", err)
		return nil
	}
	return msgBytes
}

func (hub *WebSocketHub) sendInitialState(client *wsClient) {
	msgBytes := hub.initialStateBytes()
	if msgBytes == nil {
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
	defer cancel()
	if err := client.conn.Write(ctx, websocket.MessageText, msgBytes); err != nil {
		// Client failed to receive initial state — remove and close
		hub.mu.Lock()
		delete(hub.clients, client)
		hub.mu.Unlock()
		client.cancel()
		client.conn.Close(websocket.StatusInternalError, "initial state write failed")
	}
}

// pingPump sends periodic pings to keep the connection alive and detect stale clients.
func (hub *WebSocketHub) pingPump(client *wsClient) {
	defer func() {
		if r := recover(); r != nil && hub.logger != nil {
			hub.logger.Error("websocket pingPump panic", "panic", r)
		}
	}()
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-client.ctx.Done():
			return
		case <-ticker.C:
			// Audit Q-7: Close() cancels every client.ctx, so the select
			// above is a sufficient stop signal — the previous explicit
			// hub.closed check under hub.mu was a TOCTOU (closed could
			// flip between the unlock and the Ping) that the cancelled
			// ctx + Ping error already cover.
			ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
			err := client.conn.Ping(ctx)
			cancel()
			if err != nil {
				// Client is unresponsive — close connection
				client.cancel()
				hub.mu.Lock()
				if !hub.closed {
					delete(hub.clients, client)
				}
				hub.mu.Unlock()
				client.conn.Close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

func (hub *WebSocketHub) readPump(client *wsClient) {
	defer func() {
		if r := recover(); r != nil {
			hub.logger.Error("panic in WebSocket readPump", "panic", r)
		}
		// The one teardown point every removal path reaches: removeClient,
		// pingPump and Close all cancel client.ctx, which unblocks the Read
		// below and runs this defer exactly once.
		hub.logDropTotal(client)
		client.cancel() // Cancel the detached context to stop pingPump
		hub.mu.Lock()
		if !hub.closed {
			delete(hub.clients, client)
		}
		hub.mu.Unlock()
		client.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		// Bound each Read with an idle timeout. The detached client.ctx
		// has no expiry, so without this a stale peer (TCP still ACKing
		// at the kernel but app-level silent) could park the goroutine
		// here forever. The timeout is generous enough (>2× ping
		// interval) that a healthy but chatty-less client is never
		// reaped mid-session — library-internal pong handling refreshes
		// nothing, but any message (including the browser's pong frame
		// being processed) surfaces soon enough that Read returns well
		// before wsReadIdleTimeout on a live connection that's actually
		// exchanging traffic via BroadcastLog/job updates.
		readCtx, readCancel := context.WithTimeout(client.ctx, wsReadIdleTimeout)
		msgType, data, err := client.conn.Read(readCtx)
		readCancel()
		if err != nil {
			return
		}

		// Library already enforces wsMaxMessageSize via SetReadLimit,
		// which closes the connection before Read returns on oversize
		// frames, so no redundant length check is needed here.
		if msgType != websocket.MessageText {
			continue
		}

		// Parse and handle client messages
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			client.conn.Close(websocket.StatusInvalidFramePayloadData, "Invalid JSON")
			return
		}

		if len(msg.Type) > 50 {
			client.conn.Close(websocket.StatusPolicyViolation, "Invalid message format")
			return
		}

		switch msg.Type {
		case "ping":
			pong := WSMessage{Type: "pong"}
			pongBytes, _ := json.Marshal(pong)
			ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
			err := client.conn.Write(ctx, websocket.MessageText, pongBytes)
			cancel()
			if err != nil {
				return // Connection broken; readPump cleanup will handle removal
			}
		}
	}
}

// Broadcast sends a message to all connected clients.
func (hub *WebSocketHub) Broadcast(msgType string, payload any) {
	hub.mu.RLock()
	n := len(hub.clients)
	hub.mu.RUnlock()
	if n == 0 {
		return
	}

	msg := WSMessage{Type: msgType, Payload: payload}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}

	hub.mu.RLock()
	clients := make([]*wsClient, 0, len(hub.clients))
	for c := range hub.clients {
		clients = append(clients, c)
	}
	hub.mu.RUnlock()

	for _, client := range clients {
		hub.queueOrDrop(client, msgBytes)
	}
}

// BroadcastJobUpdate sends a single-job update to all clients. No per-job
// throttle: the high-frequency caller (OnJobChange via ProgressTracker.maybeUpdate)
// is already bounded to ~60Hz/job by progressUpdateInterval (16ms), and the
// other callers (OnJobAdded, OnTrimsChanged) are event-driven and low rate.
// An earlier per-job throttle here raced against the unthrottled
// BroadcastJobDeleted: a trailing-edge job_update could arrive after a delete
// and resurrect the row via the client's upsert handler.
func (hub *WebSocketHub) BroadcastJobUpdate(data any) {
	hub.Broadcast("job_update", data)
}

// BroadcastJobsUpdate sends the full job list (on add/delete).
func (hub *WebSocketHub) BroadcastJobsUpdate(data any) {
	hub.Broadcast("jobs_update", data)
}

// BroadcastJobDeleted notifies all connected clients that a job was removed.
// The frontend uses this to drop the row from its local state immediately,
// without waiting for a full-list rebroadcast (which races against pending
// job_update messages and can leave stale "Cancelled" rows visible).
func (hub *WebSocketHub) BroadcastJobDeleted(jobID string) {
	hub.Broadcast("job_deleted", map[string]any{"id": jobID})
}

// BroadcastCheckTimers sends next check times for monitors.
func (hub *WebSocketHub) BroadcastCheckTimers(data any) {
	hub.Broadcast("check_timers", data)
}

// BroadcastConnectivity sends connectivity state to all clients.
func (hub *WebSocketHub) BroadcastConnectivity(online bool) {
	hub.Broadcast("connectivity", map[string]any{"online": online})
}

// BroadcastLog sends a log line to all clients and stores in buffer.
func (hub *WebSocketHub) BroadcastLog(line string) {
	// Truncate very long log lines to prevent buffer bloat
	const maxLineLen = 4096
	if len(line) > maxLineLen {
		line = line[:maxLineLen] + "... (truncated)"
	}

	// Add to ring buffer
	hub.logBufMu.Lock()
	hub.logBuf = append(hub.logBuf, line)
	if len(hub.logBuf) > maxLogBuffer*2 {
		hub.logBuf = append([]string{}, hub.logBuf[len(hub.logBuf)-maxLogBuffer:]...)
	}
	hub.logBufMu.Unlock()

	hub.Broadcast("log", line)
}

// GetLogBuffer returns the current log buffer.
func (hub *WebSocketHub) GetLogBuffer() []string {
	hub.logBufMu.RLock()
	defer hub.logBufMu.RUnlock()
	result := make([]string, len(hub.logBuf))
	copy(result, hub.logBuf)
	return result
}

// ClientCount returns the number of connected clients.
func (hub *WebSocketHub) ClientCount() int {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	return len(hub.clients)
}

// Close disconnects all clients.
func (hub *WebSocketHub) Close() {
	hub.mu.Lock()
	hub.closed = true
	for client := range hub.clients {
		client.cancel()
		client.conn.Close(websocket.StatusGoingAway, "server shutdown")
		delete(hub.clients, client)
	}
	hub.mu.Unlock()
}
