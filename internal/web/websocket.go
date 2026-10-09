package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	wsWriteTimeout = 10 * time.Second
	// wsPingInterval is also the worst-case latency of a pending resync: the
	// ping tick is the only place a snapshot can be flushed without a
	// broadcast to ride on (see flushResync), so on a completely quiet hub a
	// ghost row survives up to one tick. That it equals wsLagLogInterval's 30 s
	// is a coincidence — the two bound unrelated things and may be tuned apart.
	wsPingInterval = 30 * time.Second
	// wsMaxMessageSize bounds a CLIENT frame. The only message a client sends
	// is {"type":"ping"} — 15 bytes (web/public/app.js) — so 4 KiB is ~273×
	// headroom; the old 1 MiB was an unauthenticated read budget per socket on
	// a lan install, json.Unmarshal'ed in full (WEB-11). Server frames are
	// unaffected — this is a read limit.
	wsMaxMessageSize = 4 << 10 // 4 KiB
	// wsReadIdleTimeout bounds how long a single Conn.Read may block
	// waiting for a DATA frame from the client. Control frames do not count:
	// the library answers pings and consumes pongs inside the same Read,
	// under the same deadline, and server→client traffic never returns a
	// server-side Read at all. So the client must send a data frame at least
	// this often — the dashboard sends {"type":"ping"} every 15 s
	// (web/public/app.js) — or the connection is closed and the client
	// reconnects. That is also what reaps a peer whose TCP stack still ACKs
	// past an app-level zombie.
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
	// Seq is set on "log" frames only: the line's number in the logger's
	// ring (logger.Line.Seq). initial_state carries the number of its newest
	// line as payload.logSeq, and the dashboard skips a log frame at or below
	// it — see BroadcastLog. Beside the payload rather than inside it, so a
	// tab still running the previous app.js after an update keeps reading a
	// plain string payload.
	Seq uint64 `json:"seq,omitempty"`
}

// InitialStateProvider supplies data for the initial state message.
type InitialStateProvider func() map[string]any

// WebSocketHub manages WebSocket connections and broadcasts.
//
// hub.mu protects the clients map and closed flag. RWMutex rather than Mutex
// so Broadcast's snapshot and ClientCount can use the cheaper RLock path.
// Audit reports/web.md Q-20.
type WebSocketHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	closed  bool

	// jobVersions is the newest write version broadcast per job, in either
	// frame (see jobVersioned); jobRowVersions is the newest a job_update
	// carried, and has an entry only once one has been broadcast since the
	// job's last job_deleted; jobLastTick is the job_progress frame sent at
	// jobVersions while it is newer than every job_update. All three are
	// guarded by jobVerMu — which is held across the check AND the enqueue,
	// so two job frames can never reach the clients in the opposite order to
	// their writes.
	jobVerMu       sync.Mutex
	jobVersions    map[string]uint64
	jobRowVersions map[string]uint64
	jobLastTick    map[string]any

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

	// LocalPeer reports whether that IP is one the network_access mode
	// trusts as local — the peers AuthMiddleware waives (isLocalIPFor, which
	// counts 100.64.0.0/10 on lan). Set by NewServer; nil falls back to the
	// mode-free loopback-or-private test.
	LocalPeer func(ip string) bool

	// OriginCheck decides whether an upgrade's Origin header is acceptable,
	// and returns the authority it was compared against so the refusal log
	// line can name the pair the decision actually used (fix-round-1 item 7 —
	// on a trusted-proxy deployment, r.Host can differ from the effective
	// host originAllowed compared, which made the raw-r.Host line misleading
	// on exactly the deployment this check exists for).
	// Set by NewServer to the SAME decision CORSMiddleware and CSRFMiddleware
	// make (originAllowed, internal/web/middleware.go): X-Forwarded-Host from a
	// trusted proxy, port-exact, certificate-attested on external/public.
	// Nil FAILS CLOSED: an upgrade whose request carries an Origin header is
	// refused (fix-round-1 item 3 — a silently-open upgrade is a worse default
	// than a spuriously-refused one). A request with NO Origin header is
	// unaffected either way — non-browser clients send none. NewServer always
	// wires the real check; only a WebSocketHub built outside it (test
	// harnesses today) can leave this nil.
	OriginCheck func(r *http.Request) (allowed bool, comparedHost string)

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
	// needsResync is set by a drop and CLAIMED by a later enqueue (or by the
	// ping tick, see flushResync), which then sends the full initial-state
	// snapshot in place of an incremental frame. Claimed with CompareAndSwap
	// before the snapshot is built, so a racing drop can only ever cause an
	// EXTRA snapshot, never a missed one.
	needsResync atomic.Bool
	// lastResync is the UnixNano of the last CLAIM (not the last delivery):
	// stamped where needsResync is cleared, so a snapshot that fails to build
	// or gets evicted still costs the interval. Seeded at connect from the
	// initial state the client is handed there.
	lastResync atomic.Int64
}

// NewWebSocketHub creates a new WebSocket hub.
func NewWebSocketHub(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *WebSocketHub {
	return &WebSocketHub{
		clients:        make(map[*wsClient]struct{}),
		jobVersions:    make(map[string]uint64),
		jobRowVersions: make(map[string]uint64),
		jobLastTick:    make(map[string]any),
		logger:         logger,
	}
}

// HandleUpgrade is the HTTP handler for WebSocket upgrade.
func (hub *WebSocketHub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// The upgrade runs outside the router, so RecoveryMiddleware never sees a
	// panic here — the DB-backed AuthCheck, or the snapshot built for the new
	// client — and net/http's own recover writes to the discarded ErrorLog.
	// Logged here instead, with a registered client removed rather than left
	// in the hub with no reader or pinger.
	var client *wsClient
	accepted := false
	defer func() {
		if rvr := recover(); rvr != nil {
			hub.logger.Error("panic in websocket upgrade", "panic", rvr, "remoteAddr", r.RemoteAddr)
			if client != nil {
				hub.removeClient(client, "upgrade panic")
			}
			if !accepted {
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}
	}()

	// Verify authentication for external connections (matching TypeScript verifyWsClient)
	if hub.AuthCheck != nil {
		ip := ExtractIP(r)
		if hub.ClientIP != nil {
			ip = hub.ClientIP(r)
		}
		local := isLoopback(ip) || isPrivateIP(ip)
		if hub.LocalPeer != nil {
			local = hub.LocalPeer(ip)
		}
		if !local {
			if !hub.AuthCheck(r) {
				hub.logger.Debug("websocket upgrade rejected: auth required", "ip", ip)
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}
		}
	}

	// Origin policy, decided here rather than by the library. The library's
	// own check cannot express it: authenticateOrigin returns nil
	// unconditionally when Origin == Host, which is exactly the pair a
	// DNS-rebinding page controls, and its OriginPatterns are matched with
	// path.Match so a port can only be wildcarded or spelled literally.
	// An EMPTY Origin stays acceptable — non-browser clients send none, and
	// the library allowed them too. A nil OriginCheck fails CLOSED (fix-round-1
	// item 3): only a WebSocketHub built without NewServer reaches this, and a
	// silently-open upgrade is a worse default than a spuriously-refused one —
	// comparedHost then falls back to r.Host, the only authority available.
	if origin := r.Header.Get("Origin"); origin != "" {
		comparedHost := r.Host
		allowed := false
		if hub.OriginCheck != nil {
			allowed, comparedHost = hub.OriginCheck(r)
		}
		if !allowed {
			hub.logger.Warn("websocket upgrade rejected: origin refused",
				"origin", clipForLog(origin),
				"host", clipForLog(comparedHost))
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The check above IS the origin policy; skipping the library's leaves
		// exactly one (sweep chain-close F6).
		InsecureSkipVerify: true,
	})
	if err != nil {
		hub.logger.Error("websocket upgrade failed", "err", err)
		return
	}
	accepted = true

	// Set read limit to prevent oversized messages
	conn.SetReadLimit(int64(wsMaxMessageSize))

	// Use a detached context so the WebSocket connection is NOT killed by the
	// HTTP server's ReadTimeout. The request context (r.Context()) is cancelled
	// after ReadTimeout (30s), which would close the WebSocket prematurely.
	// The connection lifetime is managed by pingPump/readPump instead.
	ctx, cancel := context.WithCancel(context.Background())
	client = &wsClient{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
		writes: make(chan []byte, wsWriteQueueSize),
	}
	// Start the resync rate-limit clock BEFORE the client joins hub.clients:
	// the snapshot sendInitialState is about to write IS a resync, and from the
	// moment the client is in the map a concurrent broadcast can drop a frame
	// and — against a zero lastResync — immediately claim a second full
	// snapshot to run alongside the first. Seeding after that send left exactly
	// that window open. atomic.Int64 has no literal form, so this is the line
	// right after the composite literal rather than a field in it.
	client.lastResync.Store(time.Now().UnixNano())

	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	clientCount := len(hub.clients)
	hub.mu.Unlock()

	hub.logger.Debug("websocket connected", "clients", clientCount)

	// Send initial state immediately, DIRECTLY to the conn (not through the
	// queue) so the snapshot can't be displaced by queue-overflow drops — and
	// BEFORE the write loop starts. Broadcasts from the moment of registration
	// wait in client.writes until then, so every one of them follows the
	// snapshot. With the loop already running, one could overtake it: a
	// job_deleted committed after the snapshot's GetAllJobs went out first (a
	// no-op on an empty client), and the snapshot then restored the deleted
	// row. Replaying them after it is correct — they are idempotent upserts
	// and deletes. Log frames are appends, not upserts: a line logged since
	// registration (this Debug line, at DEBUG) is in the snapshot too, and
	// the client skips its frame by number (BroadcastLog, logSeq). A tab
	// that falls behind in its first second gets its next full snapshot one
	// wsResyncMinInterval from the seed above, not stacked straight on top of
	// this one.
	hub.sendInitialState(client)
	if client.ctx.Err() != nil {
		return // the snapshot write failed and the client is already gone
	}

	// Per-client write loop drains `writes`.
	go hub.writePump(client)

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

// wsResyncMinInterval bounds how often ONE client may be handed a full
// initial-state snapshot in place of an incremental frame. A client that
// never drains drops — and so re-arms — on EVERY frame, and a snapshot costs
// a GetAllJobs plus a whole-state marshal; without this bound a single slow
// tab would provoke one of those per broadcast, i.e. tens per second while a
// download is running. One second is far below any human-visible ghost-row
// lifetime and far above the broadcast rate it is there to decouple from.
//
// It bounds the FLOOR, not the ceiling: the worst-case wait for a pending
// resync is one wsPingInterval (30 s), because the ping tick is the only flush
// a quiet hub gets. That 30 s equals wsLagLogInterval above by coincidence,
// not by design — nothing couples the two.
const wsResyncMinInterval = 1 * time.Second

// noteLag emits at most one "WS client lagging" line per client per
// wsLagLogInterval. The DROP COUNT is incremented by the caller — the two
// call sites disagree about whether the dropped frame still needs a resync,
// and a helper that hid that decision would be the easiest place to get it
// wrong.
//
// Two defences, in order. The T1-9 fix was to stop WARNING on every drop:
// queueOrDrop used to, and the app logger's subscriber
// (cmd/moombox/monitor_callbacks.go) broadcasts each line back to every client
// — including the full queue that just dropped, which dropped again and warned
// again. One lagging client could hold that loop up for a 10 s stalled write,
// writing the log file and every per-job buffer on each turn.
//
// PRIMARY: the LEVEL. Debug sits below the default INFO threshold, so on a
// default install the line is discarded at the logger and never reaches the
// forwarder at all — the feedback loop has no edge to run on. That is what
// makes the drop path safe, not the rate limit; Debug is also the honest
// level, because the resync restores the client's STATE for everything
// initial_state carries, so a drop is not by itself an operator problem.
//
// SECONDARY: the rate limit is what bounds the loop for the operator who HAS
// turned Debug on — at most one line per client per wsLagLogInterval (30 s),
// which is the one configuration where the broadcast-back edge exists at all.
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

// resyncSnapshot claims this client's pending resync and builds the snapshot
// that replaces the frame the caller was about to send. Returns nil when
// there is nothing armed, when wsResyncMinInterval gates the claim, or when
// the snapshot cannot be built — in every one of those cases the caller sends
// its original incremental frame instead.
//
// It carries its OWN recover, and deliberately not the silent one the enqueue
// step below has: this is where hub.InitialState runs, an application closure
// that reads the database. Under the enqueue's catch-all a panic in there
// would be swallowed with no log line at all, and the frame would vanish
// before either select without even counting a drop — one bad provider and
// every broadcast to every client disappears in silence.
//
// A failed build does NOT re-arm the flag. initialStateBytes reports a marshal
// failure through the app logger, whose subscriber broadcasts that line
// straight back to this same client; re-arming would make that line claim the
// flag, rebuild, fail, log again — an unbounded loop doing a full GetAllJobs
// per turn, and reachable for real (a NaN in any job float64 makes
// json.Marshal fail permanently). Leaving it cleared costs the client the
// ghost row it would have kept anyway before this mechanism existed, and
// leaves the single Error as the operator's signal.
//
// Not re-arming breaks the PER-FRAME loop; it does not bound that Error line on
// its own. A client that is permanently behind re-arms from the other
// direction — enqueue's eviction sets needsResync on every drop, independently
// of anything decided here — so the flag comes straight back and the next
// frame tries the build again. wsResyncMinInterval is what turns that into at
// most one failed build, and so one Error line, per second per client. The two
// are jointly load-bearing: remove either and the log flood returns.
func (hub *WebSocketHub) resyncSnapshot(client *wsClient) (snap []byte) {
	defer func() {
		if r := recover(); r != nil {
			// snap stays nil — the caller falls back to its incremental frame.
			hub.logger.Error("panic building the resync snapshot", "panic", r)
		}
	}()

	if !client.needsResync.Load() {
		return nil // the common case: one atomic load per frame
	}
	now := time.Now().UnixNano()
	// Interval check BEFORE the CompareAndSwap. Claiming first and checking
	// afterwards would clear the flag inside a window it is not allowed to
	// act in, and a concurrent call that IS past the interval would then find
	// nothing armed — the resync lost rather than delayed.
	if now-client.lastResync.Load() < int64(wsResyncMinInterval) {
		return nil
	}
	if !client.needsResync.CompareAndSwap(true, false) {
		return nil // another goroutine claimed it
	}
	// Stamp at the CLAIM, unconditionally: a build that returns nil, and a
	// snapshot the queue later evicts, must both still cost the interval.
	// Stamping on delivery instead would never advance for the one client
	// that needs the bound most — the one whose queue is always full.
	client.lastResync.Store(now)
	return hub.initialStateBytes()
}

// flushResync hands a client a pending snapshot without waiting for the next
// broadcast. The flag is otherwise only ever CONSUMED by an enqueue, so on a
// quiet hub — a job_deleted drops a frame, the frame right after it falls
// inside wsResyncMinInterval and goes out incremental, and then nothing is
// broadcast for minutes — the ghost row this whole mechanism exists to retire
// would outlive it. Called from the ping tick, which bounds that to one
// wsPingInterval.
func (hub *WebSocketHub) flushResync(client *wsClient) {
	if snap := hub.resyncSnapshot(client); snap != nil {
		hub.enqueue(client, snap, true)
	}
}

// queueOrDrop pushes a marshalled frame into a client's write queue, first
// giving a pending resync the chance to REPLACE it with a full initial-state
// snapshot. Replace, not precede: the snapshot restates everything
// initial_state carries (jobs, logs, check timers, connectivity, backfill,
// hideFinishedAgeDays), so substituting it loses nothing for those types. The
// two broadcast types it does NOT carry, disk_status and update_available,
// are recovered on the next reconnect or the next daily check rather than
// immediately — acceptable against a ghost row that never goes away.
//
// Returns false when the client is gone (channel closed by removeClient).
// Audit reports/web.md C-7.
func (hub *WebSocketHub) queueOrDrop(client *wsClient, msg []byte) bool {
	resync := false
	if snap := hub.resyncSnapshot(client); snap != nil {
		msg = snap
		resync = true
	}
	return hub.enqueue(client, msg, resync)
}

// enqueue pushes one frame into a client's write queue. On a full queue the
// OLDEST queued frame is dropped (it's stale state for a client that's
// already behind) and the new frame replaces it. `resync` says the frame
// being pushed is a full-state snapshot, which changes what an eviction
// means — see the comment on the drop.
//
// The recover here is narrow BY DESIGN: it exists for exactly one thing, the
// send-on-closed-channel race with removeClient, and stays silent because
// that race is expected. Nothing that can fail in an interesting way — the
// snapshot build above, in particular — belongs underneath it.
func (hub *WebSocketHub) enqueue(client *wsClient, msg []byte, resync bool) bool {
	defer func() {
		// Recover from a send-on-closed-channel race with removeClient.
		if r := recover(); r != nil {
			// Already removed — caller doesn't need to retry.
		}
	}()

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
			// evicted is almost always older than that snapshot (a whole queue
			// can turn over during a GetAllJobs build, so "almost"), and
			// re-arming here would fight wsResyncMinInterval for no gain.
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

// initialStateBytes marshals the snapshot a freshly-connected client receives.
// Returns nil when it cannot be marshalled. Shared with queueOrDrop's resync
// path so a re-synced client is handed byte-for-byte what a new connection
// gets — one payload shape, one client-side handler.
//
// The payload is exactly what InitialState returns: the hub adds no "logs"
// key of its own any more (WEB-14). ws_wiring.go's provider always carries one
// from the logger's own ring, which is the only ring there is.
//
// Locking: takes no lock of its own, and callers must not hold hub.mu.
// queueOrDrop reaches this from inside Broadcast, i.e. from inside a database
// subscriber; that is safe because Database dispatches subscribers AFTER
// releasing db.mu, Broadcast releases hub.mu before its enqueue loop, and the
// backfill producer releases backfillMu before its Broadcast. Do not move any
// of those unlocks inside a broadcast.
func (hub *WebSocketHub) initialStateBytes() []byte {
	var data map[string]any
	if hub.InitialState != nil {
		data = hub.InitialState()
	}
	if data == nil {
		data = make(map[string]any)
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
			// The live client is the one place a pending resync can be flushed
			// without a broadcast to ride on. One atomic load per client per
			// wsPingInterval when nothing is pending, which is the normal case.
			//
			// This is a SECOND entry point into hub.InitialState — the
			// application closure behind the snapshot (GetAllJobs, a config
			// read, the log ring, the backfill map) — reached from the ping
			// goroutine rather than from a broadcast. It is safe because those
			// are leaf acquisitions and NO lock is held here: hub.mu is taken
			// and released entirely inside the ping-failure branch above, and
			// the select itself is lock-free. Anything added around this call
			// that holds hub.mu would put a database read under the hub lock.
			if client.needsResync.Load() {
				hub.flushResync(client)
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
		// here forever. Only a client DATA frame returns the Read and
		// starts a fresh deadline (see wsReadIdleTimeout): pongs are
		// handled inside the Read under this same deadline, and broadcasts
		// flow the other way. The dashboard's 15 s {"type":"ping"} is what
		// keeps a healthy browser connection under it.
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
	hub.broadcastMessage(WSMessage{Type: msgType, Payload: payload})
}

// broadcastMessage is Broadcast for a message already built — the one way a
// frame carrying more than a type and a payload (a log line's Seq) goes out.
func (hub *WebSocketHub) broadcastMessage(msg WSMessage) {
	hub.mu.RLock()
	n := len(hub.clients)
	hub.mu.RUnlock()
	if n == 0 {
		return
	}

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

// BroadcastJobUpdate sends a single-job update to all clients: a whole job row,
// for a change a progress tick does not make (status transition, error, chat
// status, mux output, a new job, a trim edit).
//
// No per-job throttle, and none is owed: the high-frequency caller
// (OnJobChange via ProgressTracker.maybeUpdate, bounded by the configured
// progress interval — downloader.progress_interval_ms, 16ms by default) now
// goes to BroadcastJobProgress below, and the callers left here (OnJobAdded,
// OnTrimsChanged, the transition paths) are event-driven and low rate.
// An earlier per-job throttle here raced against the unthrottled
// BroadcastJobDeleted: a trailing-edge job_update could arrive after a delete
// and resurrect the row via the client's upsert handler.
func (hub *WebSocketHub) BroadcastJobUpdate(data any) {
	hub.broadcastJobFrame("job_update", data)
}

// jobVersioned is a job frame that knows which database write produced it —
// *database.Job and the progress frame both carry Job.Version. Named here
// rather than imported so the hub stays ignorant of internal/database.
type jobVersioned interface {
	JobVersion() (id string, version uint64)
}

// broadcastJobFrame broadcasts a job frame unless the hub has already sent a
// LATER write of what it carries. The database notifies after releasing its
// lock, so two writers' frames can arrive here in the opposite order to their
// writes; the older one, sent last, put a stale row on every tab — a progress
// tick read back before a Muxing write, landing after it, showed the job
// Downloading for the whole mux. A frame with no version (0, or a payload that
// carries none) is always sent.
//
// What "later" means differs by frame. A job_progress carries the progress
// columns and the status, which every frame carries, so it is dropped behind
// any later frame. A job_update carries the whole row, and a later
// job_progress restates none of the rest of it: a title or twitch_quality
// write whose frame the next tick overtook (the tick read back after the
// write, so its row held the new value, but its frame does not carry it) was
// dropped, and the write never reached any tab. So a job_update is dropped
// only behind a later job_update. One that is older than a tick already sent
// takes the clients' progress columns and status back to its own write, so
// that tick's frame is sent again after it, and the clients hold the newest
// of every column.
//
// The first job_update for a job is always sent, however old: clients add a
// row only from a job_update (a job_progress for a row they do not hold is
// dropped), so dropping the row that introduces the job — a JobAdded overtaken
// by the job's first progress tick — would leave it off every tab until the
// next full resync.
func (hub *WebSocketHub) broadcastJobFrame(msgType string, data any) {
	v, ok := data.(jobVersioned)
	if !ok {
		hub.Broadcast(msgType, data)
		return
	}
	id, version := v.JobVersion()
	if version == 0 {
		hub.Broadcast(msgType, data)
		return
	}
	hub.jobVerMu.Lock()
	defer hub.jobVerMu.Unlock()
	if msgType != "job_update" {
		if version <= hub.jobVersions[id] {
			return
		}
		hub.jobVersions[id] = version
		hub.jobLastTick[id] = data
		hub.Broadcast(msgType, data)
		return
	}
	if rowVersion, sent := hub.jobRowVersions[id]; sent && version <= rowVersion {
		return
	}
	hub.jobRowVersions[id] = version
	hub.Broadcast(msgType, data)
	if version > hub.jobVersions[id] {
		hub.jobVersions[id] = version
		delete(hub.jobLastTick, id)
	} else if tick, ok := hub.jobLastTick[id]; ok {
		hub.Broadcast("job_progress", tick)
	}
}

// BroadcastJobProgress sends the slim per-tick frame: only the fields a
// download's progress write actually moves — one per configured progress
// interval, ~60 Hz at the 16ms default. The CADENCE is identical to
// job_update's — this makes each update cheaper, never rarer (the protected
// ruling) — and the client merges the frame onto the row it already holds. The
// payload shape is the caller's (cmd/moombox/job_progress.go); this hub stays
// deliberately ignorant of internal/database.
func (hub *WebSocketHub) BroadcastJobProgress(data any) {
	hub.broadcastJobFrame("job_progress", data)
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
	hub.jobVerMu.Lock()
	delete(hub.jobVersions, jobID)
	delete(hub.jobRowVersions, jobID)
	delete(hub.jobLastTick, jobID)
	hub.jobVerMu.Unlock()
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

// clipLogLine caps a single log line so one multi-megabyte panic dump cannot
// be pushed whole to every connected tab.
//
// The cut walks back to a rune boundary: a byte index can land inside a
// multi-byte rune (a CJK title in a job line, an em dash in an error), and
// json.Marshal then repairs the broken tail to U+FFFD — visible garbage at the
// boundary rather than a clean truncation. At most three bytes are given up.
func clipLogLine(line string) string {
	const maxLineLen = 4096
	if len(line) <= maxLineLen {
		return line
	}
	n := maxLineLen
	for n > 0 && !utf8.RuneStart(line[n]) {
		n--
	}
	return line[:n] + "... (truncated)"
}

// BroadcastLog sends a log line to all clients, with the sequence number the
// logger's ring gave it.
//
// The hub keeps NO buffer of its own: the logger owns the only ring
// (logger.RecentLines), ws_wiring.go always puts it in the initial-state
// payload, and the hub's copy was appended on every line and never read
// (WEB-14).
//
// The number is what keeps a line from showing twice. A client joins the hub
// BEFORE its snapshot reads the ring — the order that leaves no gap — so a
// line logged in between is in the snapshot AND reaches the client as a
// frame. Every frame here goes out after the snapshot is written (see
// HandleUpgrade), so the dashboard drops the ones at or below the snapshot's
// payload.logSeq: the hub's own "websocket connected" line, at DEBUG, was
// such a line on every connect (W24-14).
func (hub *WebSocketHub) BroadcastLog(line string, seq uint64) {
	hub.broadcastMessage(WSMessage{Type: "log", Payload: clipLogLine(line), Seq: seq})
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
