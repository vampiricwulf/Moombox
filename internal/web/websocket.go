package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	wsWriteTimeout   = 10 * time.Second
	wsPingInterval   = 30 * time.Second
	wsThrottleWindow = 100 * time.Millisecond
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
type WebSocketHub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
	closed  bool

	// Per-job throttle state (leading + trailing edge)
	throttleMu         sync.Mutex
	throttleTimers     map[string]*time.Timer
	throttleTimestamps map[string]time.Time
	throttlePending    map[string]any // Latest pending data for trailing edge

	// Initial state provider (set by main.go)
	InitialState InitialStateProvider

	// AuthCheck verifies whether a WebSocket upgrade request is allowed.
	// Called during upgrade for external (non-loopback, non-private) connections
	// when auth is required. Return true to accept, false to reject.
	// If nil, all connections are accepted (no auth configured).
	AuthCheck func(r *http.Request) bool

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
}

// NewWebSocketHub creates a new WebSocket hub.
func NewWebSocketHub(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *WebSocketHub {
	return &WebSocketHub{
		clients:            make(map[*wsClient]struct{}),
		throttleTimers:     make(map[string]*time.Timer),
		throttleTimestamps: make(map[string]time.Time),
		throttlePending:    make(map[string]any),
		logger:             logger,
	}
}

// HandleUpgrade is the HTTP handler for WebSocket upgrade.
func (hub *WebSocketHub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Verify authentication for external connections (matching TypeScript verifyWsClient)
	if hub.AuthCheck != nil {
		ip := ExtractIP(r)
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
	}

	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	clientCount := len(hub.clients)
	hub.mu.Unlock()

	hub.logger.Debug("websocket connected", "clients", clientCount)

	// Send initial state immediately
	hub.sendInitialState(client)

	// Start server-initiated ping goroutine to keep connection alive
	go hub.pingPump(client)

	// Keep connection alive and handle messages
	go hub.readPump(client)
}

// allowedOriginPatterns builds a list of host patterns for the WebSocket
// upgrade. The library already allows same-origin (Origin host == Request host),
// so these patterns only need to cover cross-origin localhost/LAN aliases.
// Patterns are matched against the Origin header's host using filepath.Match.
func (hub *WebSocketHub) allowedOriginPatterns(r *http.Request) []string {
	host := r.Host
	if host == "" {
		return nil // no extra patterns; library still allows same-origin
	}
	// Strip port from host
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}

	// Allow any port on the same hostname (e.g., dev server on a different port)
	var patterns []string
	patterns = append(patterns, hostname+":*")

	// For loopback, also allow the localhost/127.0.0.1 aliases
	if hostname == "127.0.0.1" || hostname == "::1" {
		patterns = append(patterns, "localhost", "localhost:*")
	}
	if strings.EqualFold(hostname, "localhost") {
		patterns = append(patterns, "127.0.0.1", "127.0.0.1:*")
	}
	return patterns
}

func (hub *WebSocketHub) sendInitialState(client *wsClient) {
	var data map[string]any
	if hub.InitialState != nil {
		data = hub.InitialState()
	} else {
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

	msg := WSMessage{Type: "initial_state", Payload: data}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		hub.logger.Error("failed to marshal initial state", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
	defer cancel()
	if err = client.conn.Write(ctx, websocket.MessageText, msgBytes); err != nil {
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
			// Check if hub is closed before attempting ping
			hub.mu.Lock()
			closed := hub.closed
			hub.mu.Unlock()
			if closed {
				return
			}

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
		client.cancel() // Cancel the detached context to stop pingPump
		hub.mu.Lock()
		if !hub.closed {
			delete(hub.clients, client)
		}
		remaining := len(hub.clients)
		hub.mu.Unlock()
		client.conn.Close(websocket.StatusNormalClosure, "")

		// If no clients remain, clear all pending throttle timers
		if remaining == 0 {
			hub.throttleMu.Lock()
			for _, timer := range hub.throttleTimers {
				timer.Stop()
			}
			hub.throttleTimers = make(map[string]*time.Timer)
			hub.throttleTimestamps = make(map[string]time.Time)
			hub.throttlePending = make(map[string]any)
			hub.throttleMu.Unlock()
		}
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
	hub.mu.Lock()
	n := len(hub.clients)
	hub.mu.Unlock()
	if n == 0 {
		return
	}

	msg := WSMessage{Type: msgType, Payload: payload}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}

	hub.mu.Lock()
	clients := make([]*wsClient, 0, len(hub.clients))
	for c := range hub.clients {
		clients = append(clients, c)
	}
	hub.mu.Unlock()

	for _, client := range clients {
		ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
		err := client.conn.Write(ctx, websocket.MessageText, msgBytes)
		cancel()
		if err != nil {
			hub.mu.Lock()
			delete(hub.clients, client)
			hub.mu.Unlock()
			client.cancel()
			client.conn.Close(websocket.StatusInternalError, "write failed")
		}
	}
}

// BroadcastJobUpdate sends a throttled job update with leading + trailing edge.
// Leading edge: sends immediately if 100ms since last send.
// Trailing edge: schedules a timer to ensure the final state is always delivered.
func (hub *WebSocketHub) BroadcastJobUpdate(jobID string, data any) {
	hub.throttleMu.Lock()
	defer hub.throttleMu.Unlock()

	now := time.Now()
	lastUpdate, exists := hub.throttleTimestamps[jobID]

	if !exists || now.Sub(lastUpdate) >= wsThrottleWindow {
		// Leading edge: enough time has passed, send immediately
		hub.throttleTimestamps[jobID] = now

		// Cancel any pending trailing-edge timer
		if timer, ok := hub.throttleTimers[jobID]; ok {
			timer.Stop()
			delete(hub.throttleTimers, jobID)
		}
		delete(hub.throttlePending, jobID)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					hub.logger.Error("panic in job update broadcast", "panic", r)
				}
			}()
			hub.Broadcast("job_update", data)
		}()
	} else {
		// Trailing edge: schedule/reschedule the final send
		hub.throttlePending[jobID] = data

		if timer, ok := hub.throttleTimers[jobID]; ok {
			timer.Stop()
		}
		hub.throttleTimers[jobID] = time.AfterFunc(wsThrottleWindow, func() {
			defer func() {
				if r := recover(); r != nil {
					hub.logger.Error("panic in throttle trailing edge", "panic", r)
				}
			}()
			hub.throttleMu.Lock()
			pending := hub.throttlePending[jobID]
			delete(hub.throttlePending, jobID)
			delete(hub.throttleTimers, jobID)
			hub.throttleTimestamps[jobID] = time.Now()
			hub.throttleMu.Unlock()

			if pending != nil {
				hub.Broadcast("job_update", pending)
			}

			// Clean up the timestamp after the throttle window so the map
			// doesn't grow unbounded over the process lifetime.
			time.AfterFunc(wsThrottleWindow, func() {
				defer func() {
					if r := recover(); r != nil {
						hub.logger.Error("panic in throttle cleanup", "panic", r)
					}
				}()
				hub.throttleMu.Lock()
				// Only delete if no new activity has occurred since our trailing send
				if t, ok := hub.throttleTimestamps[jobID]; ok && time.Since(t) >= wsThrottleWindow {
					delete(hub.throttleTimestamps, jobID)
				}
				hub.throttleMu.Unlock()
			})
		})
	}
}

// BroadcastJobsUpdate sends the full job list (on add/delete).
func (hub *WebSocketHub) BroadcastJobsUpdate(data any) {
	hub.Broadcast("jobs_update", data)
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
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.clients)
}

// CleanupJob removes all throttle state for a specific job ID.
// Call when a job is deleted or reaches a terminal state to prevent
// the throttle maps from growing unbounded over the process lifetime.
func (hub *WebSocketHub) CleanupJob(jobID string) {
	hub.throttleMu.Lock()
	defer hub.throttleMu.Unlock()

	if timer, ok := hub.throttleTimers[jobID]; ok {
		timer.Stop()
		delete(hub.throttleTimers, jobID)
	}
	delete(hub.throttleTimestamps, jobID)
	delete(hub.throttlePending, jobID)
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

	hub.throttleMu.Lock()
	for _, timer := range hub.throttleTimers {
		timer.Stop()
	}
	hub.throttleTimers = nil
	hub.throttleTimestamps = nil
	hub.throttlePending = nil
	hub.throttleMu.Unlock()
}
