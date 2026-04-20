# Connectivity Awareness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect internet connectivity loss and prevent false stream-ended conclusions, with UI notifications and Twitch-specific split-and-mux behavior.

**Architecture:** A centralized `ConnectivityMonitor` service in `internal/connectivity` polls `InternetGetConnectedState` via syscall every 5s, exposes `IsOnline() bool` and `OnStateChange` callbacks. Downloaders gate terminal decisions behind `IsOnline()`; monitors skip polls when offline. Twitch downloads immediately split and mux on connectivity loss. UI shows offline state.

**Tech Stack:** Go syscall (`wininet.dll`), atomic.Bool, sync.RWMutex, Bubble Tea messages, vanilla JS WebSocket events.

**Spec:** `docs/superpowers/specs/2026-04-20-connectivity-awareness-design.md`

**Note:** WebSocket auto-reconnect (spec Goal 5) is already implemented in `web/public/app.js` (lines 867-877: `scheduleReconnect` with exponential backoff capped at 30s). No new task needed for this.

---

## File Structure

| Action | Path | Responsibility |
|--------|------|---------------|
| Create | `internal/connectivity/monitor.go` | ConnectivityMonitor: Windows API polling, state machine, callbacks |
| Create | `internal/connectivity/monitor_test.go` | Unit tests for Monitor (injectable check function) |
| Create | `internal/connectivity/passive.go` | Passive failure tracker: rolling window, caller tags |
| Create | `internal/connectivity/passive_test.go` | Unit tests for passive tracker |
| Modify | `internal/worker/worker.go` | Add `IsOnline` and `OnConnectivityChange` to `DownloadWorkerDeps`, wire through |
| Modify | `internal/worker/orchestrator.go` | Add `isOnline` and `onConnectivityChange` fields, accept in constructor |
| Modify | `internal/worker/orchestrator_twitch.go` | Register connectivity callback, handle offline split |
| Modify | `internal/worker/strategy_youtube_dash.go` | Pass `IsOnline` to `DownloaderOptions` |
| Modify | `internal/worker/strategy_youtube_hls.go` | Pass `IsOnline` to `DownloaderOptions` |
| Modify | `internal/worker/stream_processor_youtube.go` | Guard probe loop with `IsOnline` |
| Modify | `internal/engine/downloader.go` | Add `IsOnline` field to `DownloaderOptions` |
| Modify | `internal/engine/downloader_hls.go` | Gate terminal decisions with offline check + wait |
| Modify | `internal/engine/downloader_dash.go` | Gate terminal decisions with offline check + wait |
| Create | `internal/engine/connectivity_wait.go` | Shared `waitForConnectivity` helper used by both downloaders |
| Create | `internal/engine/connectivity_wait_test.go` | Tests for the wait helper |
| Modify | `internal/monitor/feed.go` | Add `IsOnline` field, skip polls when offline |
| Modify | `internal/monitor/decapi.go` | Add `IsOnline` field, skip polls when offline |
| Modify | `internal/monitor/twitch.go` | Add `IsOnline` field, skip polls when offline |
| Modify | `internal/web/websocket.go` | Add `BroadcastConnectivity` helper |
| Modify | `web/public/app.js` | Handle `"connectivity"` event, show offline banner |
| Modify | `internal/tui/status_bar.go` | Add offline indicator |
| Modify | `internal/tui/app.go` | Wire connectivity state to status bar |
| Modify | `cmd/moombox/main.go` | Create Monitor, wire to all consumers |

---

### Task 1: ConnectivityMonitor Core

**Files:**
- Create: `internal/connectivity/monitor.go`
- Create: `internal/connectivity/monitor_test.go`

- [ ] **Step 1: Write test for Monitor defaults and IsOnline**

```go
// internal/connectivity/monitor_test.go
package connectivity

import (
	"sync/atomic"
	"testing"
)

func TestNewMonitor_DefaultsOnline(t *testing.T) {
	m := NewMonitor(nil)
	if !m.IsOnline() {
		t.Fatal("expected monitor to default to online")
	}
}

func TestMonitor_StateTransition(t *testing.T) {
	var called atomic.Int32
	var lastState atomic.Bool

	m := newTestMonitor(func() bool { return false }) // always offline
	m.OnStateChange(func(online bool) {
		called.Add(1)
		lastState.Store(online)
	})
	m.poll() // first offline poll
	m.poll() // second offline poll (debounce met)

	if m.IsOnline() {
		t.Fatal("expected offline after 2 polls")
	}
	if called.Load() != 1 {
		t.Fatalf("expected 1 callback, got %d", called.Load())
	}
	if lastState.Load() {
		t.Fatal("expected callback with online=false")
	}
}

func TestMonitor_RecoveryOnSingleOnlinePoll(t *testing.T) {
	checkResult := true // start online
	m := newTestMonitor(func() bool { return checkResult })
	m.poll()

	// Go offline
	checkResult = false
	m.poll()
	m.poll() // debounce met

	if m.IsOnline() {
		t.Fatal("expected offline")
	}

	// Single online poll restores
	checkResult = true
	m.poll()

	if !m.IsOnline() {
		t.Fatal("expected online after single success")
	}
}

func TestMonitor_OnStateChange_Unregister(t *testing.T) {
	var called atomic.Int32
	m := newTestMonitor(func() bool { return true })

	unregister := m.OnStateChange(func(online bool) {
		called.Add(1)
	})
	unregister()

	// Force a transition
	m.checkFn = func() bool { return false }
	m.poll()
	m.poll()

	if called.Load() != 0 {
		t.Fatal("callback should not fire after unregister")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:/Git/Moombox && go test ./internal/connectivity/...`
Expected: compilation failure — package doesn't exist yet

- [ ] **Step 3: Write the Monitor implementation**

```go
// internal/connectivity/monitor.go
package connectivity

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	wininet                   = syscall.NewLazyDLL("wininet.dll")
	procInternetGetConnected = wininet.NewProc("InternetGetConnectedState")
)

// pollInterval is how often we check connectivity.
const pollInterval = 5 * time.Second

// Monitor tracks internet connectivity state.
type Monitor struct {
	online        atomic.Bool
	offlinePolls  int // consecutive offline polls (for debounce)
	mu            sync.Mutex
	callbacks     map[uint64]func(online bool)
	nextID        uint64
	cancel        context.CancelFunc
	checkFn       func() bool // injectable for testing
	passive       *PassiveTracker
	logger        logger
}

type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// NewMonitor creates a connectivity monitor. Defaults to online.
func NewMonitor(log logger) *Monitor {
	m := &Monitor{
		callbacks: make(map[uint64]func(online bool)),
		checkFn:   checkInternetConnected,
		passive:   NewPassiveTracker(),
		logger:    log,
	}
	m.online.Store(true)
	return m
}

// newTestMonitor creates a monitor with an injectable check function (for tests).
func newTestMonitor(checkFn func() bool) *Monitor {
	m := &Monitor{
		callbacks: make(map[uint64]func(online bool)),
		checkFn:   checkFn,
		passive:   NewPassiveTracker(),
	}
	m.online.Store(true)
	return m
}

// Start begins the background polling goroutine.
func (m *Monitor) Start(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if m.logger != nil {
					m.logger.Error("connectivity monitor panic", "panic", r)
				}
			}
		}()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case <-ticker.C:
				m.poll()
			}
		}
	}()
}

// Stop cancels the background goroutine.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// IsOnline returns the current connectivity state.
func (m *Monitor) IsOnline() bool {
	return m.online.Load()
}

// OnStateChange registers a callback for connectivity transitions.
// Returns an unregister function.
func (m *Monitor) OnStateChange(fn func(online bool)) func() {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.callbacks[id] = fn
	m.mu.Unlock()

	return func() {
		m.mu.Lock()
		delete(m.callbacks, id)
		m.mu.Unlock()
	}
}

// ReportFailure reports a network-level failure from a subsystem.
func (m *Monitor) ReportFailure(tag string) {
	m.passive.ReportFailure(tag)
	// Check if passive signal should trigger offline
	if m.online.Load() && m.passive.ShouldTriggerOffline() {
		m.transition(false)
	}
}

// ReportSuccess reports a successful network request.
func (m *Monitor) ReportSuccess(tag string) {
	wasPassiveOffline := m.passive.IsTriggered()
	m.passive.ReportSuccess(tag)
	// If passive was offline and now cleared, check if we should go online
	if wasPassiveOffline && !m.passive.IsTriggered() && m.checkFn() {
		m.transition(true)
	}
}

// poll runs one check cycle.
func (m *Monitor) poll() {
	windowsOnline := m.checkFn()
	passiveOffline := m.passive.IsTriggered()
	nowOnline := windowsOnline && !passiveOffline

	wasOnline := m.online.Load()

	if nowOnline {
		m.offlinePolls = 0
		if !wasOnline {
			m.transition(true)
		}
	} else {
		m.offlinePolls++
		// Debounce: require 2 consecutive offline polls
		if wasOnline && m.offlinePolls >= 2 {
			m.transition(false)
		}
	}
}

// transition changes state and fires callbacks.
func (m *Monitor) transition(online bool) {
	old := m.online.Swap(online)
	if old == online {
		return // no actual change
	}
	if online {
		m.offlinePolls = 0
	}

	if m.logger != nil {
		if online {
			m.logger.Info("Internet connectivity restored")
		} else {
			m.logger.Info("Internet connectivity lost")
		}
	}

	m.mu.Lock()
	cbs := make([]func(online bool), 0, len(m.callbacks))
	for _, fn := range m.callbacks {
		cbs = append(cbs, fn)
	}
	m.mu.Unlock()

	for _, fn := range cbs {
		fn(online)
	}
}

// checkInternetConnected calls wininet.dll InternetGetConnectedState.
func checkInternetConnected() bool {
	var flags uint32
	ret, _, _ := procInternetGetConnected.Call(
		uintptr(unsafe.Pointer(&flags)),
		0,
	)
	return ret != 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd D:/Git/Moombox && go test ./internal/connectivity/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/connectivity/
git commit -m "feat(connectivity): add ConnectivityMonitor with Windows API polling"
```

---

### Task 2: Passive Failure Tracker

**Files:**
- Create: `internal/connectivity/passive.go`
- Create: `internal/connectivity/passive_test.go`

- [ ] **Step 1: Write tests for PassiveTracker**

```go
// internal/connectivity/passive_test.go
package connectivity

import (
	"testing"
	"time"
)

func TestPassiveTracker_NoTriggerOnSingleCaller(t *testing.T) {
	pt := NewPassiveTracker()
	for i := 0; i < 10; i++ {
		pt.ReportFailure("engine/fetch")
	}
	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger with only 1 caller tag")
	}
}

func TestPassiveTracker_TriggersOnMultipleCallers(t *testing.T) {
	pt := NewPassiveTracker()
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")

	if !pt.ShouldTriggerOffline() {
		t.Fatal("should trigger: 5 failures from 2 callers")
	}
}

func TestPassiveTracker_SuccessClearsTrigger(t *testing.T) {
	pt := NewPassiveTracker()
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")

	pt.ReportSuccess("engine/fetch")

	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger after success")
	}
	if pt.IsTriggered() {
		t.Fatal("triggered flag should clear on success")
	}
}

func TestPassiveTracker_WindowExpiry(t *testing.T) {
	pt := &PassiveTracker{
		window:   100 * time.Millisecond, // short window for test
		minFails: 5,
		minTags:  2,
	}
	pt.ReportFailure("a")
	pt.ReportFailure("a")
	pt.ReportFailure("a")
	pt.ReportFailure("b")
	pt.ReportFailure("b")

	time.Sleep(150 * time.Millisecond)

	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger after window expiry")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd D:/Git/Moombox && go test ./internal/connectivity/... -run TestPassive`
Expected: compilation failure — PassiveTracker not defined

- [ ] **Step 3: Write PassiveTracker implementation**

```go
// internal/connectivity/passive.go
package connectivity

import (
	"sync"
	"time"
)

const (
	defaultPassiveWindow   = 30 * time.Second
	defaultPassiveMinFails = 5
	defaultPassiveMinTags  = 2
)

type failureEntry struct {
	tag string
	at  time.Time
}

// PassiveTracker tracks network-level failures across subsystems.
type PassiveTracker struct {
	mu        sync.Mutex
	failures  []failureEntry
	triggered bool
	window    time.Duration
	minFails  int
	minTags   int
}

// NewPassiveTracker creates a tracker with default thresholds.
func NewPassiveTracker() *PassiveTracker {
	return &PassiveTracker{
		window:   defaultPassiveWindow,
		minFails: defaultPassiveMinFails,
		minTags:  defaultPassiveMinTags,
	}
}

// ReportFailure records a network-level failure from the given caller tag.
func (pt *PassiveTracker) ReportFailure(tag string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.failures = append(pt.failures, failureEntry{tag: tag, at: time.Now()})
	pt.pruneOld()
}

// ReportSuccess clears the triggered state and the failure window.
func (pt *PassiveTracker) ReportSuccess(tag string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.triggered = false
	pt.failures = pt.failures[:0]
}

// ShouldTriggerOffline checks if the failure pattern warrants an offline declaration.
func (pt *PassiveTracker) ShouldTriggerOffline() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneOld()

	if len(pt.failures) < pt.minFails {
		return false
	}

	tags := make(map[string]struct{})
	for _, f := range pt.failures {
		tags[f.tag] = struct{}{}
	}
	if len(tags) < pt.minTags {
		return false
	}

	pt.triggered = true
	return true
}

// IsTriggered returns whether the passive tracker has declared offline.
func (pt *PassiveTracker) IsTriggered() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.triggered
}

// pruneOld removes entries outside the rolling window. Must hold mu.
func (pt *PassiveTracker) pruneOld() {
	cutoff := time.Now().Add(-pt.window)
	i := 0
	for i < len(pt.failures) && pt.failures[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		pt.failures = pt.failures[i:]
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd D:/Git/Moombox && go test ./internal/connectivity/... -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/connectivity/passive.go internal/connectivity/passive_test.go
git commit -m "feat(connectivity): add passive failure tracker"
```

---

### Task 3: Connectivity Wait Helper for Downloaders

**Files:**
- Create: `internal/engine/connectivity_wait.go`
- Create: `internal/engine/connectivity_wait_test.go`
- Modify: `internal/engine/downloader.go` — add `IsOnline` to `DownloaderOptions`

- [ ] **Step 1: Write test for waitForConnectivity**

```go
// internal/engine/connectivity_wait_test.go
package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForConnectivity_AlreadyOnline(t *testing.T) {
	start := time.Now()
	err := waitForConnectivity(context.Background(), func() bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("should return immediately when online")
	}
}

func TestWaitForConnectivity_WaitsAndReturns(t *testing.T) {
	var online atomic.Bool
	go func() {
		time.Sleep(200 * time.Millisecond)
		online.Store(true)
	}()

	err := waitForConnectivity(context.Background(), func() bool { return online.Load() })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForConnectivity_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForConnectivity(ctx, func() bool { return false })
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd D:/Git/Moombox && go test ./internal/engine/... -run TestWaitForConnectivity`
Expected: compilation failure — `waitForConnectivity` not defined

- [ ] **Step 3: Write the wait helper and add IsOnline to DownloaderOptions**

```go
// internal/engine/connectivity_wait.go
package engine

import (
	"context"
	"time"
)

const connectivityPollInterval = 5 * time.Second

// waitForConnectivity blocks until isOnline returns true or ctx is cancelled.
// Returns nil when online, or ctx.Err() if cancelled.
func waitForConnectivity(ctx context.Context, isOnline func() bool) error {
	if isOnline() {
		return nil
	}
	ticker := time.NewTicker(connectivityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if isOnline() {
				return nil
			}
		}
	}
}
```

Then add `IsOnline` to `DownloaderOptions` in `internal/engine/downloader.go`:

```go
// Add after the CheckStreamStatus field (line 58):
IsOnline           func() bool                    // Returns false if device has no internet
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd D:/Git/Moombox && go test ./internal/engine/... -run TestWaitForConnectivity -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/engine/connectivity_wait.go internal/engine/connectivity_wait_test.go internal/engine/downloader.go
git commit -m "feat(engine): add connectivity wait helper and IsOnline option"
```

---

### Task 4: Guard HLS Downloader Terminal Decisions

**Files:**
- Modify: `internal/engine/downloader_hls.go`

- [ ] **Step 1: Guard the 404/410 playlist conclusion (lines 47-56)**

In `runHlsLoop`, after `if plStatus == 404 || plStatus == 410 {` block, wrap the CheckStreamStatus logic with an offline guard:

```go
// Replace lines 47-56 in downloader_hls.go with:
		if plStatus == 404 || plStatus == 410 {
			if d.opts.IsOnline != nil && !d.opts.IsOnline() {
				d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
				if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
					return err
				}
				consecutiveErrors = 0
				continue
			}
			if d.opts.CheckStreamStatus != nil {
				ended, checkErr := d.opts.CheckStreamStatus(ctx)
				if checkErr != nil {
					if d.opts.IsOnline != nil && !d.opts.IsOnline() {
						d.logger.Debug("stream status check failed while offline, assuming still live")
						if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
							return err
						}
						consecutiveErrors = 0
						continue
					}
					d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
				} else if !ended {
					return ErrQualityLost
				}
			}
			d.streamEnded.Store(true)
			return nil
		}
```

- [ ] **Step 2: Guard the consecutive errors threshold (lines 57-66)**

Replace the `if consecutiveErrors > 5` block:

```go
		consecutiveErrors++
		if consecutiveErrors > 5 {
			if d.opts.IsOnline != nil && !d.opts.IsOnline() {
				d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
				if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
					return err
				}
				consecutiveErrors = 0
				continue
			}
			if d.opts.CheckStreamStatus != nil {
				ended, checkErr := d.opts.CheckStreamStatus(ctx)
				if checkErr != nil {
					if d.opts.IsOnline != nil && !d.opts.IsOnline() {
						d.logger.Debug("stream status check failed while offline, assuming still live")
						if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
							return err
						}
						consecutiveErrors = 0
						continue
					}
					d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
				} else if !ended {
					return ErrQualityLost
				}
			}
			return fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", consecutiveErrors, err)
		}
```

- [ ] **Step 3: Guard the stale detection (around line 150)**

Find the stale detection block (`if staleCount >= 5 && d.opts.CheckStreamStatus != nil`) and add:

```go
		if staleCount >= 5 {
			if d.opts.IsOnline != nil && !d.opts.IsOnline() {
				d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
				if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
					return err
				}
				staleCount = 0
				continue
			}
			if d.opts.CheckStreamStatus != nil {
				ended, _ := d.opts.CheckStreamStatus(ctx)
				if ended {
					d.streamEnded.Store(true)
					return nil
				}
			}
		}
```

- [ ] **Step 4: Run existing HLS tests to verify no regressions**

Run: `cd D:/Git/Moombox && go test ./internal/engine/... -v -run Hls`
Expected: PASS (existing tests pass since IsOnline is nil by default)

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader_hls.go
git commit -m "feat(engine): guard HLS terminal decisions with connectivity check"
```

---

### Task 5: Guard DASH Downloader Terminal Decisions

**Files:**
- Modify: `internal/engine/downloader_dash.go`

- [ ] **Step 1: Guard handleGoneError (line 197-208)**

In `handleGoneError`, before the CheckStreamStatus call at the `*consecutiveGoneErrors > 10` threshold:

```go
func (d *SegmentDownloader) handleGoneError(ctx context.Context, consecutiveGoneErrors *int, hasStartedDownloading bool) error {
	*consecutiveGoneErrors++

	if hasStartedDownloading && *consecutiveGoneErrors > 10 {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*consecutiveGoneErrors = 0
			return nil // Continue loop
		}
		if d.opts.CheckStreamStatus != nil {
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			if checkErr != nil {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.logger.Debug("stream status check failed while offline, assuming still live")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					*consecutiveGoneErrors = 0
					return nil
				}
				d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
			} else if !ended {
				return ErrQualityLost
			}
		}
		d.streamEnded.Store(true)
		return errStreamDone
	}
	// ... rest of function unchanged
```

- [ ] **Step 2: Guard handleHTTPError CheckStreamStatus calls (lines 276-293)**

Wrap each `CheckStreamStatus` call in `handleHTTPError` with offline guard:

```go
	// Check stream status at threshold
	if *sameHeadRetryDelay == liveCheckThreshold && d.opts.CheckStreamStatus != nil {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*sameHeadRetryDelay = 0
			return nil
		}
		ended, _ := d.opts.CheckStreamStatus(ctx)
		if ended {
			return errStreamDone
		}
	}

	// Check status on every probe at cap
	if *sameHeadRetryDelay >= delayCap && d.opts.CheckStreamStatus != nil {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*sameHeadRetryDelay = 0
			return nil
		}
		ended, _ := d.opts.CheckStreamStatus(ctx)
		if ended {
			return errStreamDone
		}
		if hasStartedDownloading {
			return ErrQualityLost
		}
	}
```

- [ ] **Step 3: Guard the NoSegmentTimeout (lines 296-306)**

```go
	// Also check no-segment timeout
	if time.Since(d.lastSegTime) > NoSegmentTimeout {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			d.lastSegTime = time.Now() // Reset timer on recovery
			*sameHeadRetryDelay = 0
			return nil
		}
		if d.opts.CheckStreamStatus != nil && hasStartedDownloading {
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			if checkErr != nil {
				d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
			} else if !ended {
				return ErrQualityLost
			}
		}
		return errStreamDone
	}
```

- [ ] **Step 4: Run existing DASH tests**

Run: `cd D:/Git/Moombox && go test ./internal/engine/... -v -run Dash`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader_dash.go
git commit -m "feat(engine): guard DASH terminal decisions with connectivity check"
```

---

### Task 6: Wire IsOnline Through Worker/Orchestrator

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/orchestrator.go`
- Modify: `internal/worker/strategy_youtube_dash.go`
- Modify: `internal/worker/strategy_youtube_hls.go`

- [ ] **Step 1: Add fields to DownloadWorkerDeps**

In `internal/worker/worker.go`, add to the `DownloadWorkerDeps` struct:

```go
type DownloadWorkerDeps struct {
	CipherSolver        *cipher.Solver
	PotProvider         *bgutils.PotProvider
	TwitchService       *twitch.Service
	Notifier            *notifications.Manager
	IsOnline            func() bool
	OnConnectivityChange func(fn func(online bool)) func()
}
```

- [ ] **Step 2: Flow IsOnline to DownloadOrchestrator**

In `internal/worker/orchestrator.go`, add fields to the struct and constructor:

```go
type DownloadOrchestrator struct {
	muxer                *engine.Muxer
	ffmpegPath           string
	db                   *database.Database
	queue                *JobQueue
	cipherSolver         *cipher.Solver
	potProvider          *bgutils.PotProvider
	notifier             *notifications.Manager
	isOnline             func() bool
	onConnectivityChange func(fn func(online bool)) func()
	logger               logger
}
```

Update `NewDownloadOrchestrator` to accept and store the new fields.

- [ ] **Step 3: Update NewDownloadWorker to pass connectivity deps**

In `NewDownloadWorker`, extract `IsOnline` and `OnConnectivityChange` from deps (like CipherSolver) and pass to `NewDownloadOrchestrator`.

- [ ] **Step 4: Pass IsOnline to DownloaderOptions in YouTube strategies**

In `strategy_youtube_dash.go` where `DownloaderOptions` are built (lines ~196-214, ~225-243), add:

```go
IsOnline: job.IsOnline,
```

Similarly in `strategy_youtube_hls.go` (lines ~145-161).

This requires adding `IsOnline func() bool` to the `JobContext` struct or passing it another way. Check how `job.Logger` gets there and follow the same pattern.

- [ ] **Step 5: Run build to verify compilation**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 6: Commit**

```bash
git add internal/worker/worker.go internal/worker/orchestrator.go internal/worker/strategy_youtube_dash.go internal/worker/strategy_youtube_hls.go
git commit -m "feat(worker): wire IsOnline through orchestrator to downloaders"
```

---

### Task 7: Twitch Orchestrator Connectivity Split

**Files:**
- Modify: `internal/worker/orchestrator_twitch.go`

- [ ] **Step 1: Register OnStateChange callback at download start**

At the start of `ExecuteTwitch`, after the context/cancel setup:

```go
// Register connectivity callback for immediate bail on offline
var offlineCancelled atomic.Bool
var unregisterConn func()
if o.onConnectivityChange != nil {
	unregisterConn = o.onConnectivityChange(func(online bool) {
		if !online {
			offlineCancelled.Store(true)
			cancel() // cancel download context
		}
	})
	defer unregisterConn()
}
```

- [ ] **Step 2: Handle connectivity cancellation after the download loop**

Replace the `if ctx.Err() != nil` block (lines 421-427) with:

```go
if ctx.Err() != nil {
	if offlineCancelled.Load() {
		// Connectivity loss: mux what we have and finalize
		o.logger.Warn("Twitch download interrupted by connectivity loss, muxing captured data", "jobID", jobCtx.Job.ID)

		if o.notifier != nil {
			o.notifier.Send("Twitch Download Split — Connectivity Lost",
				fmt.Sprintf("Internet connectivity lost during download: %s", jobCtx.Job.Title),
				notifications.TypeDownload,
				[]notifications.Field{
					{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
					{Name: "Quality", Value: currentQuality.Label, Inline: true},
					{Name: "Segment", Value: fmt.Sprintf("%d", segmentIndex+1), Inline: true},
				},
				notifications.SendOptions{
					URL:       jobCtx.Job.URL,
					Thumbnail: jobCtx.Job.ThumbnailURL,
					Event:     "connectivity_split",
				},
			)
		}

		// Stop chat
		if twitchChatDl != nil {
			twitchChatDl.Stop()
		}
		// Fall through to muxing logic below (don't return)
	} else {
		// Shutdown/user cancel: preserve staging dir for resume
		if twitchChatDl != nil {
			twitchChatDl.Stop()
		}
		return ctx.Err()
	}
}
```

- [ ] **Step 3: Run build to verify compilation**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/worker/orchestrator_twitch.go
git commit -m "feat(worker): Twitch immediate split-and-mux on connectivity loss"
```

---

### Task 8: Guard YouTube Probe Loop

**Files:**
- Modify: `internal/worker/stream_processor_youtube.go`

- [ ] **Step 1: Add IsOnline to StreamProcessor**

Add `isOnline func() bool` field to `StreamProcessor` struct and a setter:

```go
// In stream_processor.go, add to struct:
isOnline func() bool

// Add setter:
func (sp *StreamProcessor) SetIsOnline(fn func() bool) {
	sp.isOnline = fn
}
```

- [ ] **Step 2: Guard the probe loop (lines 128-143)**

In `waitForLive`, before the probe call, add an offline check:

```go
		// Skip probe when offline — don't burn error counter
		if sp.isOnline != nil && !sp.isOnline() {
			sp.logger.Debug("skipping probe — device offline", "videoID", job.VideoID)
			continue
		}

		// Probe — use lightweight authenticated probe if members-only was detected
		var probeInfo *youtube.VideoInfo
		// ... existing code
```

- [ ] **Step 3: Run build**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/worker/stream_processor.go internal/worker/stream_processor_youtube.go
git commit -m "feat(worker): guard YouTube probe loop with connectivity check"
```

---

### Task 9: Monitor Subsystem Integration

**Files:**
- Modify: `internal/monitor/feed.go`
- Modify: `internal/monitor/decapi.go`
- Modify: `internal/monitor/twitch.go`

- [ ] **Step 1: Add IsOnline to FeedMonitor**

Add `IsOnline func() bool` field to `FeedMonitor` struct (after the `logger` field block, around line 47):

```go
IsOnline func() bool // nil = always online
```

- [ ] **Step 2: Guard the poll in doCheck**

At the top of `doCheck` in `feed.go` (line 196), add:

```go
func (fm *FeedMonitor) doCheck(ctx context.Context) {
	if fm.IsOnline != nil && !fm.IsOnline() {
		fm.logger.Debug("skipping feed poll — offline")
		return
	}
	// ... existing code
```

- [ ] **Step 3: Repeat for DecapiMonitor and TwitchMonitor**

Same pattern: add `IsOnline func() bool` field, check at the top of their `doCheck` equivalent functions.

For `decapi.go`: find the `doCheck`/`runCycle` function and add the same guard.
For `twitch.go`: find the `doCheck`/`runCycle` function and add the same guard.

- [ ] **Step 4: Run build**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/monitor/feed.go internal/monitor/decapi.go internal/monitor/twitch.go
git commit -m "feat(monitor): skip polls when device is offline"
```

---

### Task 10: Web UI — Connectivity Banner and WebSocket Event

**Files:**
- Modify: `internal/web/websocket.go`
- Modify: `web/public/app.js`

- [ ] **Step 1: Add BroadcastConnectivity helper**

In `internal/web/websocket.go`, add after `BroadcastCheckTimers`:

```go
// BroadcastConnectivity sends connectivity state to all clients.
func (hub *WebSocketHub) BroadcastConnectivity(online bool) {
	hub.Broadcast("connectivity", map[string]any{"online": online})
}
```

- [ ] **Step 2: Handle the connectivity event in app.js**

In the `handleMessage` switch in `app.js` (around line 894), add a case:

```javascript
      case "connectivity":
        this.handleConnectivityChange(p);
        break;
```

Add the handler method:

```javascript
  handleConnectivityChange(payload) {
    this.internetOnline = payload.online;
    const banner = document.getElementById("connectivity-banner");
    if (!banner) return;
    if (payload.online) {
      banner.classList.remove("show");
    } else {
      banner.classList.add("show");
    }
  }
```

- [ ] **Step 3: Add the initial_state handling**

In the `case "initial_state":` block (line 895), add:

```javascript
        if (p.connectivity !== undefined) {
          this.handleConnectivityChange({ online: p.connectivity });
        }
```

- [ ] **Step 4: Add the banner HTML element**

Find where the connection-status indicator is placed in the HTML (likely `index.html`) and add a connectivity banner element:

```html
<div id="connectivity-banner" class="connectivity-banner">
  <sl-icon name="wifi-off"></sl-icon>
  Internet connection lost
</div>
```

Add CSS:

```css
.connectivity-banner {
  display: none;
  background: var(--sl-color-warning-600);
  color: white;
  text-align: center;
  padding: 6px 12px;
  font-size: 0.85rem;
  font-weight: 500;
}
.connectivity-banner.show {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
}
```

- [ ] **Step 5: Build to verify Go compilation + review JS manually**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 6: Commit**

```bash
git add internal/web/websocket.go web/public/
git commit -m "feat(web): connectivity banner and WebSocket event"
```

---

### Task 11: TUI Status Bar Offline Indicator

**Files:**
- Modify: `internal/tui/status_bar.go`
- Modify: `internal/tui/app.go`

- [ ] **Step 1: Add offline state to StatusBarModel**

In `status_bar.go`, add to the struct:

```go
// In StatusBarModel struct:
offline bool
```

- [ ] **Step 2: Render the offline indicator in renderMetrics**

At the beginning of `renderMetrics()`, before the batch selection count:

```go
func (m *StatusBarModel) renderMetrics() string {
	var parts []string
	compact := m.width < statusBarCompactThreshold

	// Connectivity indicator
	if m.offline {
		parts = append(parts, statusBarRedStyle.Render("OFFLINE"))
	}

	// Batch selection count
	// ... existing code
```

- [ ] **Step 3: Wire connectivity to TUI app**

In `app.go`, ensure the App struct has access to `IsOnline func() bool`. Add a method or field, and in the Update loop, periodically check or react to a connectivity message to update `statusBar.offline`.

The TUI will receive a `ConnectivityMsg` (a Bubble Tea message type) dispatched by the OnStateChange callback:

```go
// In the TUI messages file or app.go:
type ConnectivityMsg struct {
	Online bool
}
```

In the App's `Update` method, handle it:

```go
case ConnectivityMsg:
	m.statusBar.offline = !msg.Online
	return m, nil
```

- [ ] **Step 4: Run build**

Run: `cd D:/Git/Moombox && go build ./...`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/tui/status_bar.go internal/tui/app.go
git commit -m "feat(tui): show OFFLINE indicator in status bar"
```

---

### Task 12: Wire Everything in main.go

**Files:**
- Modify: `cmd/moombox/main.go`

- [ ] **Step 1: Create and start the ConnectivityMonitor**

After the logger setup but before services that need it (around step 2-3 area in main.go):

```go
// Connectivity monitor
connMon := connectivity.NewMonitor(log)
connMon.Start(ctx)
defer connMon.Stop()
```

Add import: `"github.com/vampiricwulf/Moombox/internal/connectivity"`

- [ ] **Step 2: Pass to DownloadWorkerDeps**

Update the `DownloadWorkerDeps` construction (line ~307):

```go
dlWorker := worker.NewDownloadWorker(db, ytService, cfg, log, &worker.DownloadWorkerDeps{
	CipherSolver:         cipherSolver,
	PotProvider:          potProvider,
	TwitchService:        twService,
	Notifier:             notifyMgr,
	IsOnline:             connMon.IsOnline,
	OnConnectivityChange: connMon.OnStateChange,
})
```

- [ ] **Step 3: Pass IsOnline to StreamProcessor inside NewDownloadWorker**

In `internal/worker/worker.go` inside `NewDownloadWorker`, after `sp.SetNotifier(nm)`, add:

```go
if deps != nil && deps.IsOnline != nil {
	sp.SetIsOnline(deps.IsOnline)
}
```

This is wired internally because StreamProcessor is created inside NewDownloadWorker (not accessible from main.go).

- [ ] **Step 4: Pass to monitors**

After creating each monitor:

```go
feedMon.IsOnline = connMon.IsOnline
decapiMon.IsOnline = connMon.IsOnline
twitchMon.IsOnline = connMon.IsOnline
```

- [ ] **Step 5: Register OnStateChange callbacks**

After all services are created but before `Start()` calls:

```go
connMon.OnStateChange(func(online bool) {
	if online {
		feedMon.CheckNow()
		decapiMon.CheckNow()
		twitchMon.CheckNow()
	}
	wsHub.BroadcastConnectivity(online)
})
```

- [ ] **Step 6: Add connectivity to WebSocket initial state**

In the `wsHub.InitialState` callback (line ~667), add:

```go
"connectivity": connMon.IsOnline(),
```

- [ ] **Step 7: Wire TUI connectivity**

Where the TUI app is created, register a callback that sends the Bubble Tea message:

```go
connMon.OnStateChange(func(online bool) {
	if tuiProgram != nil {
		tuiProgram.Send(tui.ConnectivityMsg{Online: online})
	}
})
```

- [ ] **Step 8: Build and run basic smoke test**

Run: `cd D:/Git/Moombox && go build -o moombox.exe ./cmd/moombox`
Expected: successful compilation

- [ ] **Step 9: Commit**

```bash
git add cmd/moombox/main.go
git commit -m "feat: wire ConnectivityMonitor to all subsystems in main.go"
```

---

### Task 13: Passive ReportFailure/ReportSuccess Integration

**Files:**
- Modify: `internal/utils/http.go`
- Modify: `internal/engine/downloader_fetch.go`

The ConnectivityMonitor needs to be accessible from utility code to call ReportFailure/ReportSuccess. Since `utils` and `engine` can't import `connectivity` (circular deps risk), the Monitor exposes a `Reporter` interface that's passed down.

- [ ] **Step 1: Add a Reporter interface to the connectivity package**

In `internal/connectivity/monitor.go`, add:

```go
// Reporter provides network result reporting. Passed to HTTP utilities.
type Reporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}
```

The Monitor itself satisfies this interface.

- [ ] **Step 2: Add a package-level reporter to utils**

In `internal/utils/http.go`, add:

```go
var connReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// SetConnectivityReporter sets the global connectivity reporter for HTTP utilities.
func SetConnectivityReporter(r interface{ ReportFailure(string); ReportSuccess(string) }) {
	connReporter = r
}
```

- [ ] **Step 3: Call ReportFailure/ReportSuccess in FetchWithTimeout**

In `FetchWithTimeout`, after the HTTP request completes:

```go
resp, err := utilsHTTPClient.Do(req)
if err != nil {
	if connReporter != nil {
		connReporter.ReportFailure("utils/http")
	}
	cancel()
	return nil, nil, err
}
if connReporter != nil {
	connReporter.ReportSuccess("utils/http")
}
```

The key distinction: `err != nil` from `client.Do` means no HTTP response (network failure). Any response (even 4xx/5xx) means the server was reached.

- [ ] **Step 4: Call ReportFailure/ReportSuccess in engine fetch code**

In `internal/engine/downloader_fetch.go`, in `fetchSegment`:

```go
resp, err := engineHTTPClient.Do(req)
if err != nil {
	// Network-level failure — no response received
	// (Report to connectivity monitor if available via opts)
	return nil, 0, err
}
// Got a response (even if error status) — server was reached
```

Since the engine package doesn't have access to the global reporter, the `DownloaderOptions` can carry an optional reporter callback, OR we add a package-level reporter to engine as well. Use the same pattern as utils:

```go
// In engine package:
var connReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

func SetConnectivityReporter(r interface{ ReportFailure(string); ReportSuccess(string) }) {
	connReporter = r
}
```

Call in `fetchSegment` and `fetchChunkWithRetry`.

- [ ] **Step 5: Wire in main.go**

In main.go, after creating the connectivity monitor:

```go
utils.SetConnectivityReporter(connMon)
engine.SetConnectivityReporter(connMon)
```

- [ ] **Step 6: Run build and tests**

Run: `cd D:/Git/Moombox && go build ./... && go test ./...`
Expected: success

- [ ] **Step 7: Commit**

```bash
git add internal/connectivity/monitor.go internal/utils/http.go internal/engine/downloader_fetch.go cmd/moombox/main.go
git commit -m "feat: wire passive ReportFailure/ReportSuccess into HTTP utilities"
```

---

### Task 14: Integration Verification

- [ ] **Step 1: Run full test suite**

Run: `cd D:/Git/Moombox && go test ./...`
Expected: all PASS (no regressions)

- [ ] **Step 2: Run vet**

Run: `cd D:/Git/Moombox && go vet ./...`
Expected: no issues

- [ ] **Step 3: Manual smoke test**

Build and run: `cd D:/Git/Moombox && go build -o moombox.exe ./cmd/moombox && ./moombox.exe`

Verify:
- App starts without errors
- Log shows "Internet connectivity restored" or no connectivity messages (already online)
- Web UI loads, WebSocket connects
- TUI shows no OFFLINE indicator (assuming online)

- [ ] **Step 4: Commit any fixups**

If any fixes were needed during verification, commit them.
