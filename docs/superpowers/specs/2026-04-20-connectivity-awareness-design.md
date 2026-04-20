# Connectivity Awareness Design

**Date:** 2026-04-20
**Status:** Approved

## Problem

Moombox has no way to distinguish between "the internet is down" and "the stream ended." When the device loses connectivity, downloads can falsely conclude a stream has ended (hitting consecutive error thresholds, getting 403/410-like failures from DNS/timeout, or having `CheckStreamStatus` probes fail and assuming "ended"). Monitors silently skip failed polls with no notification. The Web UI's WebSocket has no reconnection logic.

## Goals

1. Detect internet connectivity loss using Windows system APIs + passive failure correlation
2. Prevent false stream-ended conclusions during outages
3. Notify the user of connectivity state in both Web UI and TUI
4. Skip wasteful monitor polls while offline, resume immediately on recovery
5. Add WebSocket auto-reconnect to the Web UI

## Non-Goals

- Linux/Mac support (Windows-only, per project constraints)
- Freezing download goroutines (keep retry loops running, just suppress conclusions)
- Catch-up burst polling on recovery (single immediate poll per monitor is sufficient)
- Distinguishing between partial connectivity scenarios (e.g., can reach YouTube but not Twitch)

## Design

### 1. ConnectivityMonitor Service

**Package:** `internal/connectivity`

**Struct:**
```go
type Monitor struct {
    online     atomic.Bool
    mu         sync.RWMutex
    callbacks  []func(online bool)
    cancel     context.CancelFunc
    logger     interface {
        Debug(msg string, args ...any)
        Info(msg string, args ...any)
        Warn(msg string, args ...any)
        Error(msg string, args ...any)
    }
}
```

**Public API:**
- `NewMonitor(logger) *Monitor` — creates monitor, defaults to online
- `Start(ctx context.Context)` — spawns background goroutine polling `InternetGetConnectedState` every 5 seconds
- `Stop()` — cancels the background goroutine
- `IsOnline() bool` — atomic read of cached state (~1ns, safe from any goroutine)
- `OnStateChange(fn func(online bool))` — registers callback for state transitions

**Windows API:**
- Calls `wininet.dll` → `InternetGetConnectedState` via `syscall.NewLazyDLL` / `NewLazyProc`
- No CGo required — pure syscall
- Returns bool (connected/not) + flags (LAN/modem/proxy — ignored)

**Passive backup:**
- Exposes `ReportFailure()` and `ReportSuccess()` for subsystems to call after HTTP requests
- Called from shared HTTP utilities (`utils.FetchBody`, `utils.FetchWithTimeout`) and segment fetch code (`engine.fetchSegment`, `engine.fetchChunkWithRetry`)
- Tracks failure counts within a 30-second rolling window
- Two trigger paths:
  - Windows API says offline → offline (after debounce)
  - Windows API says online BUT 5+ failures from 2+ distinct callers with zero successes in the 30s window → also offline (handles "connected to WiFi but no internet" edge case)
- Handles edge case where Windows reports "connected" but traffic can't actually flow

**State transition logic:**
- Requires 2 consecutive offline polls (10 seconds) before declaring offline — prevents flapping on momentary glitches
- Returns to online on a single successful poll — recovery should be fast
- Callbacks fire only on actual transitions (online→offline or offline→online), not every poll tick
- State transitions logged at Info level: "Internet connectivity lost" / "Internet connectivity restored"

### 2. Download Subsystem Integration

**New field on `DownloaderOptions`:**
```go
IsOnline func() bool // Returns false if device has no internet connectivity
```

**Behavior:** At every decision point where the downloader would conclude a stream has ended, it first checks `IsOnline()`. If offline:

1. **Suppress the terminal conclusion** — don't return `errStreamDone`, `ErrQualityLost`, or break out of the download loop
2. **Enter a connectivity wait loop** — sleep 5s, re-check `IsOnline()`, repeat until online or context cancelled
3. **Resume normally** — when online returns, the existing retry logic continues from where it was

**Decision points to guard:**

| File | Line | Current behavior | Change |
|------|------|-----------------|--------|
| `downloader_dash.go` | `handleGoneError()` ~210 | After 10+ consecutive 403/410, calls CheckStreamStatus | If offline, skip conclusion, wait |
| `downloader_dash.go` | `handleHTTPError()` ~276-285 | CheckStreamStatus at backoff thresholds | If offline, don't call CheckStreamStatus, wait |
| `downloader_dash.go` | `handleHTTPError()` ~300 | No segments for 10 min → check status | If offline, reset the 10-min timer, wait |
| `downloader_hls.go` | ~34 | 404/410 on playlist + CheckStreamStatus → ended | If offline, skip conclusion, wait |
| `downloader_hls.go` | ~48 | 5+ consecutive errors + CheckStreamStatus → ended | If offline, skip conclusion, wait |
| `downloader_hls.go` | ~150 | 5 stale iterations + CheckStreamStatus → ended | If offline, skip conclusion, wait |

**CheckStreamStatus guarding:** When `CheckStreamStatus` returns an error (probe failed), the current code logs "assuming ended" in several places. With connectivity awareness: if offline, treat the error as "assume still live" instead of "assume ended."

**What stays the same:** All existing retry logic, backoff timers, segment fetch retries. The connectivity check is purely a gate on terminal decisions — it doesn't change the retry behavior itself.

### 3. Monitor Subsystem Integration

**Affected monitors:** FeedMonitor, DecapiMonitor, TwitchMonitor

Each monitor receives `IsOnline func() bool` via its constructor or a setter method.

**Behavior:**
- At the top of each poll function, check `IsOnline()`. If offline, skip the poll body (log at Debug, return early). The timer resets normally.
- No new poll loop — uses the existing timer cycle.

**Recovery via OnStateChange callback:**
- Registered in `main.go`: when transitioning offline→online, call `CheckNow()` on all three monitors
- This triggers an immediate poll through the existing mechanism, avoiding the worst-case wait of one full poll interval (up to 10 min for RSS feeds)

### 4. Web UI Integration

**WebSocket event:**
- New broadcast event type: `"connectivity"` with payload `{ "online": true/false }`
- Fired via `OnStateChange` callback registered in `main.go`
- Included in initial state sent on WebSocket connect (so new connections know current status)

**Offline banner:**
- When a `"connectivity": { "online": false }` event arrives, display a banner/bar at the top of the page: "Internet connection lost — downloads and monitoring paused"
- Auto-dismiss when `"connectivity": { "online": true }` arrives
- CSS class toggles (e.g., `.connectivity-banner.show`) — no complex state management

**WebSocket auto-reconnect (client-side JS):**
- On `WebSocket.onclose`, start exponential backoff reconnection: 1s → 2s → 4s → 8s → 16s → cap at 30s
- On successful reconnect, server sends full initial state via existing `sendInitialState()` mechanism
- While disconnected from WS (but server might be fine), show "Reconnecting to server..." indicator
- Clear all reconnect state on successful connection

### 5. TUI Integration

**Status bar indicator:**
- When offline, the TUI status bar shows `[OFFLINE]` (or similar short indicator)
- The TUI app receives `IsOnline func() bool` and subscribes to `OnStateChange` for re-render triggers
- State change triggers a Bubble Tea `Cmd` message to update the status bar

### 6. Wiring in main.go

The ConnectivityMonitor is created early in the startup sequence:

```go
// Between steps 2 (database) and 3 (YouTube service)
connMon := connectivity.NewMonitor(log)
connMon.Start(ctx)
defer connMon.Stop()
```

**Consumer wiring:**
- **Download worker/orchestrators:** Pass `connMon.IsOnline` when building `DownloaderOptions` in the strategy files
- **Monitors:** Pass `connMon.IsOnline` to each monitor constructor
- **OnStateChange callbacks:**
  ```go
  connMon.OnStateChange(func(online bool) {
      if online {
          feedMon.CheckNow()
          decapiMon.CheckNow()
          twitchMon.CheckNow()
      }
      wsHub.Broadcast("connectivity", map[string]any{"online": online})
  })
  ```
- **Initial state:** Add `"connectivity": connMon.IsOnline()` to the `wsHub.InitialState` callback
- **TUI:** Pass `connMon.IsOnline` to TUI app; register separate `OnStateChange` for Bubble Tea message dispatch

### 7. Logging

| Event | Level | Message |
|-------|-------|---------|
| State transition → offline | Info | "Internet connectivity lost" |
| State transition → online | Info | "Internet connectivity restored" |
| Monitor poll skipped (offline) | Debug | "Skipping feed poll — offline" |
| Download conclusion suppressed | Warn | "Stream end signal suppressed — device offline, waiting for connectivity" |
| CheckStreamStatus error while offline | Debug | "Stream status check failed while offline, assuming still live" |
| Passive failure reported | Debug | "Connectivity failure reported" (with caller info) |
