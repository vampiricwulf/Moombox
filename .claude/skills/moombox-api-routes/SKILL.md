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
Global middleware is applied automatically via `r.Use()` in `server.go` — recovery, CORS, security headers, CSRF, IP gate, max body size, compression, and auth. You do **not** add these per-route.

Per-route middleware via `r.With()` is only for **additional** constraints:
- Rate limiting: `r.With(deps.RateLimiter.Middleware).Post(...)`
- Loopback-only: `r.With(web.LoopbackOnly).Post(...)`

### 4. Wire in Main
`cmd/moombox/main.go` — Construct deps struct and call registration function alongside the other route registrations.

### 5. Frontend Fetch
`web/public/` — Add fetch call in the appropriate module. Use relative `/api/` paths. Send `Content-Type: application/json`.

### 6. TUI HTTP Call
If TUI needs this endpoint — use the internal HTTP client which injects `X-Internal-Token` header automatically via custom `RoundTripper` (`internalTokenTransport` in `app.go`). This bypasses CSRF validation via constant-time comparison in middleware.

### 7. WebSocket Broadcast (if real-time)
If this endpoint triggers state changes that clients need to see in real-time, add a WebSocket broadcast (see below).

---

## Adding a WebSocket Event

### 1. Choose Broadcast Method
`internal/web/websocket.go`:
- `Broadcast(type, payload)` — immediate, generic broadcast
- `BroadcastJobUpdate(data)` — per-job update; no hub-level throttle (upstream rate is bounded by `ProgressTracker.maybeUpdate`'s 16ms gate for the high-frequency path)
- `BroadcastJobsUpdate(data)` — full job list (add/delete, threshold changes)
- `BroadcastJobDeleted(jobID)` — targeted row removal; clients drop the row immediately
- `BroadcastCheckTimers(data)` — next monitor check times
- `BroadcastLog(line)` — log line + ring buffer storage (200 lines)

### 2. Wire the Source
Connect the event source to the broadcast in `cmd/moombox/monitor_callbacks.go`. Common patterns:
- **Database subscriber**: `db.OnJobChange(func(ev) { wsHub.BroadcastJobUpdate(ev.Job) })`
- **Direct call**: From a service callback (e.g., disk status, update check)
- **Log subscriber**: `log.Subscribe()` channel → `BroadcastLog()`

### 3. Frontend Handler
`web/public/app.js` — Add case in WebSocket message handler switch on `msg.type`. Existing types: `initial_state`, `jobs_update`, `job_update`, `job_deleted`, `log`, `check_timers`, `disk_status`, `update_available`, `connectivity`, `pong`.

### 4. TUI Handler
TUI does **not** receive WebSocket messages. Instead, it gets data via:
- Database callbacks: `db.OnJobUpdate()`, `db.OnJobsChange()` → sends custom `tea.Msg` types (e.g., `JobUpdateMsg`, `JobsUpdateMsg`)
- Log subscription: `log.Subscribe()` channel → `LogBatchMsg`
- Service callbacks: wired in `main.go` → sends typed messages via non-blocking channel

Define your custom `tea.Msg` type in `app.go`, send via non-blocking channel select, handle in `Update()`.

### 5. Initial State
If new clients need the current value on connect, include it in the `InitialState` provider function set in `main.go`. This sends the value to clients immediately on WebSocket connect.

## Message Format
```go
type WSMessage struct {
    Type    string `json:"type"`    // e.g., "job_update", "disk_status"
    Payload any    `json:"payload"`
}
```

## Common Mistakes

- Forgetting to wire deps in `main.go` — route registers but deps are nil
- Adding per-route auth/CSRF when it's already applied globally
- Calling `BroadcastJobUpdate` after `BroadcastJobDeleted` for the same ID — the late update will re-add the row via the client's upsert handler
- Not including new event in initial state — clients that connect after the event miss it
- Wiring TUI via WebSocket instead of database callbacks — TUI uses direct subscriptions
