---
name: moombox-api-routes
description: Use when adding a new API endpoint, adding real-time WebSocket events, or modifying route middleware — covers chi registration, deps, handlers, frontend calls, TUI calls, and WebSocket broadcasts
---

# API Routes & WebSocket Events

## Adding an API Route

### 1. Deps Struct
`internal/web/routes/` — Create or extend a `*RouteDeps` struct in the appropriate route file. Each route file defines its own deps.
```go
type FooRouteDeps struct {
    DB     *database.Database
    Logger interface{ Info(string, ...any); Error(string, ...any) }
    // ... only what this route needs
}
```

### 2. Route Registration Function
Same file — export a registration function. All routes use `/api/` prefix (no versioning).
```go
func FooRoutes(r chi.Router, deps *FooRouteDeps) {
    r.Get("/api/foo", func(w http.ResponseWriter, r *http.Request) { ... })
    r.Post("/api/foo", func(w http.ResponseWriter, r *http.Request) { ... })
}
```

### 3. Middleware
Global middleware is applied automatically via `r.Use()` in `internal/web/server.go` — recovery, CORS, security headers, CSRF, IP gate, max body size, compression, and auth. You do **not** add these per-route.

Per-route middleware via `r.With()` is only for **additional** constraints:
- Rate limiting: `r.With(deps.RateLimiter.Middleware).Post(...)`
- Loopback-only: `r.With(web.LoopbackOnly).Post(...)`

### 4. Wire the Route
`cmd/moombox/routes_wiring.go` — in `wireRoutes`, construct the deps struct and call the registration function beside the other route registrations. Every `routes.*Routes(` call is there; `cmd/moombox/main.go` holds none.

### 5. Frontend Fetch
`web/public/` — Add fetch call in the appropriate module. Use relative `/api/` paths. Send `Content-Type: application/json`.

### 6. TUI HTTP Call
If TUI needs this endpoint — use the internal HTTP client which injects `X-Internal-Token` header automatically via custom `RoundTripper` (`internalTokenTransport` in `internal/tui/app_commands.go`). This bypasses CSRF validation via constant-time comparison in middleware.

### 7. WebSocket Broadcast (if real-time)
If this endpoint triggers state changes that clients need to see in real-time, add a WebSocket broadcast (see below).

---

## Adding a WebSocket Event

### 1. Choose Broadcast Method
`internal/web/websocket.go`:
- `Broadcast(type, payload)` — immediate, generic broadcast
- `BroadcastJobUpdate(data)` — a whole job row, for a change a progress tick does not make (status transition, error, chat status, mux output, a new job, a trim edit); no hub-level throttle, and the callers left on it are event-driven
- `BroadcastJobProgress(data)` — the slim per-tick `job_progress` frame (the payload shape is the caller's: `cmd/moombox/job_progress.go`), sent at the same cadence `job_update` used to carry — one per `downloader.progress_interval_ms`, ~60 Hz at the 16 ms default — and merged client-side onto the row the client already holds. Cheaper, never rarer.
- `BroadcastJobsUpdate(data)` — full job list (add/delete, threshold changes)
- `BroadcastJobDeleted(jobID)` — targeted row removal; clients drop the row immediately
- `BroadcastCheckTimers(data)` — next monitor check times
- `Broadcast("channel_monitors", monitor.ChannelMonitors)` — every monitor's health list, sent by `channelMonitorsPush.check` (`cmd/moombox/channel_monitors_push.go`) only when the set of failing channels or a failing channel's newest error changes; the dashboard derives the flag itself with its mirror of `monitor.ChannelFailing`, so never send the derived answer in its place
- `BroadcastConnectivity(online)` — `{online}` when reachability flips
- `Broadcast("status_changed", nil)` — no payload, sent by `nudgeDashboardStatus` (`cmd/moombox/status_nudge.go`) when cookie authentication, a re-login flag or the sidecar's health changes; the tab reads `GET /api/status` again. Cookie status is never pushed in a frame: the web badge is drawn only from a fetch the tab made (owner ruling R2-D1). A new status source that `/api/status` serves calls the same nudge instead of adding a frame
- `BroadcastLog(line, seq)` — one log line, clipped to 4096 bytes on a rune boundary (`clipLogLine`), with its number in the logger's ring as `seq` beside the payload. The hub keeps NO ring of its own (WEB-14): the logger owns the only one, and `cmd/moombox/ws_wiring.go` puts `logger.RecentLines` — the lines and the newest one's number, `logSeq` — into every `initial_state`; the dashboard skips a frame at or below `logSeq`, a line the snapshot already holds.

### 2. Wire the Source
Connect the event source to the broadcast in `cmd/moombox/monitor_callbacks.go`. Common patterns:
- **Database subscriber**: `db.OnJobChange(func(ev) { wsHub.BroadcastJobUpdate(ev.Job) })`
- **Direct call**: From a service callback (e.g., disk status, update check)
- **Log subscriber**: `log.SubscribeLines()` channel → `BroadcastLog(line.Text, line.Seq)` (`wireLogForwarding`)

### 3. Frontend Handler
`web/public/app.js` — add a `case` to the `switch (message.type)` in `handleMessage`, and a row for the new type to the table in `docs/spec/user-interfaces.md` § Server-to-Client Message Types. That table is the one list of existing types; `TestWebSocketFrameTableMatchesTheClient` (`internal/docs/drift_test.go`) fails while the handler and the table differ.

### 4. TUI Handler
TUI does **not** receive WebSocket messages. Instead, it gets data via:
- Database callbacks: `db.OnJobUpdate()`, `db.OnJobsChange()` → sends custom `tea.Msg` types (e.g., `JobUpdateMsg`, `JobsUpdateMsg`)
- Log subscription: `log.SubscribeLines()` channel → `LogBatchMsg`, skipping the lines the `RecentLines` backfill already holds (`forwardTUILogs`)
- Service callbacks: wired in `cmd/moombox/tui_wiring.go` (the monitor's own in `cmd/moombox/monitor_callbacks.go`) → typed messages through a non-blocking channel, or `app.Send` for the ones that need no channel

Define your custom `tea.Msg` type in `internal/tui/app.go`, send via non-blocking channel select, handle in `Update()` (`internal/tui/app_update.go`).

### 5. Initial State
If new clients need the current value on connect, include it in the `InitialState` provider function `wireWebSocket` sets in `cmd/moombox/ws_wiring.go`. This sends the value to clients immediately on WebSocket connect.

## Frame Compression
The hub does not negotiate permessage-deflate: `HandleUpgrade` (`internal/web/websocket.go`) passes no `CompressionMode`, and `TestWebSocketDoesNotNegotiateCompression` holds that. The `initial_state` snapshot is measured (`TestInitialStateSnapshotSize`, `cmd/moombox/ws_initial_state_size_test.go`) and large, but owner ruling R-B4 allows compression only once it is checked against every browser the dashboard is used from, Safari and Firefox included. Do not enable it without that check, never trim a frame's fields to make it smaller, and keep a high-rate frame small (the ~60 Hz `job_progress` frame is about 300 bytes).

## Message Format
```go
type WSMessage struct {
    Type    string `json:"type"`    // e.g., "job_update", "disk_status"
    Payload any    `json:"payload"`
}
```

## Common Mistakes

- Forgetting to wire deps in `cmd/moombox/routes_wiring.go` — route registers but deps are nil
- Adding per-route auth/CSRF when it's already applied globally
- Calling `BroadcastJobUpdate` after `BroadcastJobDeleted` for the same ID — the late update will re-add the row via the client's upsert handler
- Not including new event in initial state — clients that connect after the event miss it
- Wiring TUI via WebSocket instead of database callbacks — TUI uses direct subscriptions
- Ending a credential (a client token, the password) without ending what it admitted — a socket is checked only at the upgrade and a session slides for as long as it is used, so call `InvalidateAllSessions` and `WebSocketHub.CloseAuthenticated` after the row is gone (`endRemoteSessions`, `cmd/moombox/ws_wiring.go`)
