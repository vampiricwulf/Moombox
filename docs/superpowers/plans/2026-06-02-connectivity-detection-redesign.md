# Connectivity Detection Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the connectivity oracle an accurate real-reachability probe on every platform, keep recovery instant, and make the upcoming-stream loops tolerant so connectivity loss (or a premature "online") can never error a waiting stream.

**Architecture:** One shared pure-Go TCP-dial reachability probe replaces the Windows wininet heuristic and the Linux single-dial. Recovery stays single-poll (no hysteresis). The fix that actually protects streams is consumer-side: a pure error classifier (`classifyProbeErr`) + a decision helper (`probeErrorDecision`) so network-class probe failures never count toward `maxConsecutiveProbeErrors`; probes also feed the passive tracker, and an offline "probe-anyway" floor keeps a wrongly-offline oracle from stranding a stream.

**Tech Stack:** Go 1.25, `net` (Dialer/DialContext), `errors`/`net/url` classification, `BurntSushi/toml` config, existing `connectivity.Monitor` + package-level `SetConnectivityReporter` pattern.

Design spec: `docs/superpowers/specs/2026-06-02-connectivity-detection-redesign-design.md`

---

## File Structure

- Create `internal/connectivity/probe.go` — shared reachability probe (pure).
- Create `internal/connectivity/probe_test.go` — probe tests.
- Modify `internal/connectivity/monitor.go` — `probeTargets` field, `SetProbeTargets`, default `checkFn` closure.
- Delete `internal/connectivity/monitor_windows.go`, `internal/connectivity/monitor_unix.go`.
- Modify `internal/config/types.go` — `ConnectivityConfig` + field.
- Modify `internal/config/config.go` — default, auto-persist detection, validation (`net` import).
- Modify `cmd/moombox/services.go` — `SetProbeTargets` + `worker.SetConnectivityReporter`.
- Create `internal/worker/probe_classify.go` — `classifyProbeErr` + `probeErrorDecision`.
- Create `internal/worker/probe_classify_test.go` — table tests (the regression coverage).
- Create `internal/worker/connectivity.go` — package reporter (mirrors `monitor/feed.go`).
- Create `internal/worker/connectivity_test.go` — reporter round-trip.
- Modify `internal/worker/stream_processor_youtube.go` — offline floor + decision helper.
- Modify `internal/worker/stream_processor_twitch.go` — offline floor + decision helper.

---

### Task 1: Shared reachability probe

**Files:**
- Create: `internal/connectivity/probe.go`
- Test: `internal/connectivity/probe_test.go`

- [ ] **Step 1: Write the failing test**

```go
package connectivity

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestReachabilityProbe_SuccessOnLiveTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// One dead target + one live target: race must report the live one.
	ok := reachabilityProbe(context.Background(), []string{"127.0.0.1:1", ln.Addr().String()})
	if !ok {
		t.Fatal("expected reachable when at least one target accepts")
	}
}

func TestReachabilityProbe_FailWhenAllDead(t *testing.T) {
	// 127.0.0.1:1 is reserved/closed; the dial fails fast.
	start := time.Now()
	ok := reachabilityProbe(context.Background(), []string{"127.0.0.1:1"})
	if ok {
		t.Fatal("expected unreachable when no target accepts")
	}
	if time.Since(start) > probeRaceTimeout+time.Second {
		t.Fatalf("probe took too long: %v", time.Since(start))
	}
}

func TestReachabilityProbe_EmptyTargetsUsesDefaults(t *testing.T) {
	// Empty slice must fall back to defaultProbeTargets (don't panic / don't
	// treat as trivially reachable). We can't assume network access in CI, so
	// only assert it doesn't panic and returns a bool within the deadline.
	_ = reachabilityProbe(context.Background(), nil)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/connectivity/ -run TestReachabilityProbe -v`
Expected: FAIL — `undefined: reachabilityProbe` / `undefined: probeRaceTimeout`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/connectivity/probe.go`:

```go
package connectivity

import (
	"context"
	"net"
	"time"
)

// defaultProbeTargets are anycast HTTPS endpoints used to verify real internet
// reachability when no override is configured. Port 443 (not DNS/53) because
// some networks block outbound DNS but allow HTTPS. Multiple targets so a
// single host's outage can't cause a false-offline.
//
// The user-facing default lives in config.DefaultProbeTargets and is wired in
// via Monitor.SetProbeTargets; this slice is the in-package safety fallback for
// when SetProbeTargets is never called or is passed an empty list.
var defaultProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

// probeRaceTimeout bounds the whole multi-target race. A live anycast target
// answers in tens of ms; a dead/blackholed network fails within this window.
const probeRaceTimeout = 3 * time.Second

// reachabilityProbe races TCP dials to targets and returns true as soon as ANY
// handshake completes. A completed TCP handshake proves actual routability to a
// live host — unlike Windows' InternetGetConnectedState, which only reports
// adapter/route state and can report "connected" during a real outage. Pure
// Go, no CGo; identical on every platform.
func reachabilityProbe(ctx context.Context, targets []string) bool {
	if len(targets) == 0 {
		targets = defaultProbeTargets
	}
	ctx, cancel := context.WithTimeout(ctx, probeRaceTimeout)
	defer cancel() // cancels still-in-flight dials once we have a winner

	resultCh := make(chan bool, len(targets)) // buffered: late senders never block
	var d net.Dialer
	for _, t := range targets {
		go func(addr string) {
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				resultCh <- false
				return
			}
			_ = conn.Close()
			resultCh <- true
		}(t)
	}
	for range targets {
		if <-resultCh {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/connectivity/ -run TestReachabilityProbe -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/connectivity/probe.go internal/connectivity/probe_test.go
git commit -m "feat(connectivity): shared pure-Go multi-target reachability probe"
```

---

### Task 2: Wire probe into Monitor; delete platform heuristics

**Files:**
- Modify: `internal/connectivity/monitor.go`
- Delete: `internal/connectivity/monitor_windows.go`, `internal/connectivity/monitor_unix.go`

- [ ] **Step 1: Add probeTargets field + SetProbeTargets + default checkFn closure**

In `internal/connectivity/monitor.go`, add `probeTargets []string` to the `Monitor` struct (after `passive *PassiveTracker`):

```go
	passive      *PassiveTracker
	probeTargets []string // reachability-probe targets; nil → defaultProbeTargets
	logger       logger
```

In `NewMonitorWithInterval`, remove `checkFn: checkInternetConnected,` from the struct literal and set the closure after construction:

```go
	m := &Monitor{
		callbacks:    make(map[uint64]func(online bool)),
		pollInterval: interval,
		passive:      NewPassiveTracker(),
		logger:       log,
	}
	// Default probe: a real multi-target TCP reachability check (shared across
	// platforms). Reads m.probeTargets dynamically so SetProbeTargets can
	// override it during init. Injectable for tests via newTestMonitor.
	m.checkFn = func() bool { return reachabilityProbe(context.Background(), m.probeTargets) }
	m.online.Store(true)
	return m
```

Add the setter (near `IsOnline`):

```go
// SetProbeTargets overrides the reachability-probe targets (wired from config).
// Call once during init, BEFORE Start(); not safe to call concurrently with
// the poll goroutine. An empty/nil slice falls back to defaultProbeTargets.
func (m *Monitor) SetProbeTargets(targets []string) {
	m.probeTargets = targets
}
```

Add `"context"` to the imports if not already present (it is — `Start` uses it).

- [ ] **Step 2: Delete the platform heuristic files**

```bash
git rm internal/connectivity/monitor_windows.go internal/connectivity/monitor_unix.go
```

Expected: `checkInternetConnected` no longer exists anywhere. (It was referenced only by the old struct literal, now replaced.)

- [ ] **Step 3: Run build + tests to verify it compiles and existing tests pass**

Run: `go build ./internal/connectivity/ && go test ./internal/connectivity/ -v`
Expected: PASS. `newTestMonitor` injects `checkFn` directly so it is unaffected; `NewMonitor(nil)` / `NewMonitorWithInterval` signatures are unchanged.

- [ ] **Step 4: Commit**

```bash
git add internal/connectivity/monitor.go
git commit -m "feat(connectivity): monitor uses shared reachability probe on all platforms; drop wininet heuristic"
```

---

### Task 3: Config `[connectivity] probe_targets`

**Files:**
- Modify: `internal/config/types.go`
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestValidate_ConnectivityProbeTargets(t *testing.T) {
	cfg := Defaults()
	if len(cfg.Connectivity.ProbeTargets) == 0 {
		t.Fatal("defaults must populate connectivity.probe_targets")
	}

	// Malformed entry is reported by Validate.
	bad := Defaults()
	bad.Connectivity.ProbeTargets = []string{"not-a-host-port"}
	errs := Validate(bad)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "connectivity.probe_targets") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a probe_targets validation error, got %v", errs)
	}

	// Normalize replaces a malformed list with defaults.
	norm := Defaults()
	norm.Connectivity.ProbeTargets = []string{"not-a-host-port"}
	Normalize(norm)
	if len(norm.Connectivity.ProbeTargets) == 0 || norm.Connectivity.ProbeTargets[0] == "not-a-host-port" {
		t.Fatalf("Normalize should restore defaults, got %v", norm.Connectivity.ProbeTargets)
	}
}
```

Ensure `strings` is imported in `config_test.go` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidate_ConnectivityProbeTargets -v`
Expected: FAIL — `cfg.Connectivity undefined`.

- [ ] **Step 3a: Add the config type**

In `internal/config/types.go`, add the field to `MoomboxConfig` (after `Memory`):

```go
	Memory        MemoryConfig         `toml:"memory" json:"memory"`
	Connectivity  ConnectivityConfig   `toml:"connectivity" json:"connectivity"`
```

And add the type (after `MemoryConfig`):

```go
// ConnectivityConfig holds internet-reachability probe settings.
type ConnectivityConfig struct {
	// ProbeTargets are host:port endpoints the connectivity monitor TCP-dials
	// to verify real internet reachability (first success wins). Defaults to
	// public anycast resolvers on :443. Override if your network blocks
	// outbound connections to them.
	ProbeTargets []string `toml:"probe_targets" json:"probe_targets"`
}
```

- [ ] **Step 3b: Add the default, auto-persist detection, and validation**

In `internal/config/config.go`:

Add the exported default near the top of the file (package level, after the imports):

```go
// DefaultProbeTargets is the user-facing default for connectivity.probe_targets.
// Kept in sync with connectivity/probe.go's in-package fallback.
var DefaultProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}
```

In `Defaults()`, add after the `Memory:` block (before the closing `}`):

```go
		Memory: MemoryConfig{
			GoSoftLimitMB:      256,
			SidecarSoftLimitMB: 200,
			SidecarHardLimitMB: 512,
		},
		Connectivity: ConnectivityConfig{
			ProbeTargets: DefaultProbeTargets,
		},
```

In `loadFromFile`, add a new-section detection beside the existing ones:

```go
	if _, hasBgutils := raw["bgutils"]; !hasBgutils {
		cfg.NeedsAutoPersist = true
	}
	if _, hasConnectivity := raw["connectivity"]; !hasConnectivity {
		cfg.NeedsAutoPersist = true
	}
```

Add `"net"` to the `config.go` import block.

In `validateOrNormalize`, add a block (e.g., after the `ClientTokenTTLDays` block):

```go
	// connectivity.probe_targets: each entry must be host:port. Empty list or
	// any malformed entry falls back to the defaults (Normalize); Validate
	// reports each malformed entry.
	if len(cfg.Connectivity.ProbeTargets) == 0 {
		fail("connectivity.probe_targets is empty")
		if !reportOnly {
			cfg.Connectivity.ProbeTargets = defaults.Connectivity.ProbeTargets
		}
	} else {
		for _, t := range cfg.Connectivity.ProbeTargets {
			if _, _, err := net.SplitHostPort(t); err != nil {
				fail("connectivity.probe_targets entry %q must be host:port: %v", t, err)
				if !reportOnly {
					cfg.Connectivity.ProbeTargets = defaults.Connectivity.ProbeTargets
					break
				}
			}
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestValidate_ConnectivityProbeTargets -v && go build ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/types.go internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add [connectivity] probe_targets with validation + defaults"
```

---

### Task 4: Wire config targets into the monitor

**Files:**
- Modify: `cmd/moombox/services.go:149-150`

- [ ] **Step 1: Insert SetProbeTargets before Start**

In `cmd/moombox/services.go`, change:

```go
	s.connMon = connectivity.NewMonitor(log)
	s.connMon.Start(s.ctx)
```

to:

```go
	s.connMon = connectivity.NewMonitor(log)
	s.connMon.SetProbeTargets(s.cfg.Connectivity.ProbeTargets) // before Start: poll goroutine reads targets
	s.connMon.Start(s.ctx)
```

(Verify `s.cfg` is populated before this line — it is; config load precedes connMon construction.)

- [ ] **Step 2: Build**

Run: `go build ./cmd/moombox/`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add cmd/moombox/services.go
git commit -m "feat(connectivity): wire configured probe_targets into the monitor"
```

---

### Task 5: Probe error classifier

**Files:**
- Create: `internal/worker/probe_classify.go`
- Test: `internal/worker/probe_classify_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/worker/probe_classify_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestClassifyProbeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want probeErrClass
	}{
		{"nil", nil, classServer},
		{"context canceled", context.Canceled, classCancelled},
		{"deadline exceeded", context.DeadlineExceeded, classNetwork},
		{"url wrapping opError", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}}, classNetwork},
		{"dns error", &net.DNSError{Err: "no such host", Name: "x"}, classNetwork},
		{"unexpected eof", io.ErrUnexpectedEOF, classNetwork},
		{"http 503 string", fmt.Errorf("WEB API error: HTTP 503"), classNetwork},
		{"http 429 string", fmt.Errorf("ANDROID_VR API error: HTTP 429"), classNetwork},
		{"tls string", fmt.Errorf("tls: handshake failure"), classNetwork},
		{"http 404 string", fmt.Errorf("WEB API error: HTTP 404"), classServer},
		{"unknown defaults to network", errors.New("something weird happened"), classNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeErr(tc.err); got != tc.want {
				t.Errorf("classifyProbeErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestProbeErrorDecision(t *testing.T) {
	if d := probeErrorDecision(context.Canceled); !d.cancelled {
		t.Error("cancelled error should yield cancelled decision")
	}
	if d := probeErrorDecision(&net.DNSError{Err: "no such host"}); d.count || d.report != reportFailure {
		t.Errorf("network error: want count=false report=failure, got %+v", d)
	}
	if d := probeErrorDecision(fmt.Errorf("WEB API error: HTTP 404")); !d.count || d.report != reportSuccess {
		t.Errorf("server error: want count=true report=success, got %+v", d)
	}
	if d := probeErrorDecision(errors.New("mystery")); d.count {
		t.Error("unknown error must NOT count (asymmetric default)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/ -run "TestClassifyProbeErr|TestProbeErrorDecision" -v`
Expected: FAIL — `undefined: classifyProbeErr` etc.

- [ ] **Step 3: Write the implementation**

Create `internal/worker/probe_classify.go`:

```go
package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
)

// probeErrClass categorizes a probe/fetch error so the upcoming-stream wait
// loops can treat a connectivity blip differently from a definitive service
// refusal.
type probeErrClass int

const (
	classNetwork   probeErrClass = iota // transient/connectivity — DO NOT count
	classServer                         // definitive service verdict — count
	classCancelled                      // ctx cancelled — abandon
)

// classifyProbeErr categorizes a probe error. ASYMMETRIC DEFAULT: an
// unrecognized error is treated as classNetwork (do-not-count). Under the
// cost model (a false "give up" wrongly errors a waiting stream; a false
// "keep waiting" only delays giving up), a missed classification must only
// ever delay giving up.
//
// Verified against internal/youtube/player_api_strategy.go: transport errors
// reach us raw (lastErr=err, :534) or %w-wrapped ("read body"/"parse response",
// :543/:564) so errors.As matches the inner net error; only HTTP status
// failures are flattened to the string "<client> API error: HTTP <code>"
// (:552/:556), handled by the string fallback below.
func classifyProbeErr(err error) probeErrClass {
	if err == nil {
		return classServer
	}
	if errors.Is(err, context.Canceled) {
		return classCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classNetwork
	}

	// url.Error wraps the transport error; recurse on the inner error.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return classifyProbeErr(urlErr.Err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return classNetwork
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return classNetwork
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return classNetwork
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return classNetwork
	}

	// String fallback for lossy wraps where the transport detail was flattened.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "http 429"), // rate limited — transient
		strings.Contains(msg, "http 5"), // 5xx — server transient
		strings.Contains(msg, "tls"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "eof"):
		return classNetwork
	case strings.Contains(msg, "http 4"): // 4xx (non-429) — definitive client error
		return classServer
	}
	return classNetwork // asymmetric default
}

// probeReport tells the wait loop whether/how to feed the passive connectivity
// tracker after a probe error.
type probeReport int

const (
	reportNone    probeReport = iota
	reportFailure             // ReportFailure: the network looks down
	reportSuccess             // ReportSuccess: the request reached the service
)

// probeDecision is the pure decision the wait loops act on for a probe error.
type probeDecision struct {
	count     bool        // increment consecutiveErrors (only definitive failures)
	cancelled bool        // ctx cancelled → return cancelled
	report    probeReport // how to feed the passive tracker
}

// probeErrorDecision maps a probe error to the loop's reaction.
func probeErrorDecision(err error) probeDecision {
	switch classifyProbeErr(err) {
	case classCancelled:
		return probeDecision{cancelled: true, report: reportNone}
	case classNetwork:
		return probeDecision{count: false, report: reportFailure}
	default: // classServer
		return probeDecision{count: true, report: reportSuccess}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worker/ -run "TestClassifyProbeErr|TestProbeErrorDecision" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/probe_classify.go internal/worker/probe_classify_test.go
git commit -m "feat(worker): probe error classifier — network failures never count toward give-up"
```

---

### Task 6: Worker connectivity reporter

**Files:**
- Create: `internal/worker/connectivity.go`
- Modify: `cmd/moombox/services.go:151-153`
- Test: `internal/worker/connectivity_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/worker/connectivity_test.go`:

```go
package worker

import "testing"

type fakeReporter struct{ fails, oks int }

func (f *fakeReporter) ReportFailure(string) { f.fails++ }
func (f *fakeReporter) ReportSuccess(string) { f.oks++ }

func TestWorkerConnectivityReporterRoundTrip(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })
	f := &fakeReporter{}
	SetConnectivityReporter(f)

	reportProbeResult("probe/youtube", true)
	reportProbeResult("probe/youtube", false)
	if f.fails != 1 || f.oks != 1 {
		t.Fatalf("want fails=1 oks=1, got fails=%d oks=%d", f.fails, f.oks)
	}

	SetConnectivityReporter(nil)
	reportProbeResult("probe/youtube", true) // must be a no-op, not panic
	if f.fails != 1 {
		t.Fatalf("nil reporter must not forward, got fails=%d", f.fails)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/ -run TestWorkerConnectivityReporterRoundTrip -v`
Expected: FAIL — `undefined: SetConnectivityReporter` / `reportProbeResult`.

- [ ] **Step 3: Write the implementation (mirrors internal/monitor/feed.go)**

Create `internal/worker/connectivity.go`:

```go
package worker

import "sync/atomic"

// ConnectivityReporter is the subset of connectivity.Monitor the worker's
// probe loops invoke so their YouTube/Twitch probe outcomes feed the passive
// outage tracker (internal/connectivity/passive.go) — letting a service-only
// failure contribute to the global online/offline signal.
type ConnectivityReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// connReporter is an atomic.Pointer so SetConnectivityReporter is race-free
// against in-flight probes. main wires it once at startup.
var connReporter atomic.Pointer[ConnectivityReporter]

// SetConnectivityReporter wires the package-wide connectivity reporter for the
// worker's probe loops. Safe to call concurrently; nil clears it.
func SetConnectivityReporter(r ConnectivityReporter) {
	if r == nil {
		connReporter.Store(nil)
		return
	}
	connReporter.Store(&r)
}

// reportProbeResult forwards a probe outcome to the installed reporter, if any.
// tag identifies the subsystem ("probe/youtube", "probe/twitch") so the passive
// tracker can count distinct-subsystem failures toward its offline threshold.
func reportProbeResult(tag string, failed bool) {
	rp := connReporter.Load()
	if rp == nil {
		return
	}
	if failed {
		(*rp).ReportFailure(tag)
	} else {
		(*rp).ReportSuccess(tag)
	}
}
```

- [ ] **Step 4: Wire it in services.go**

In `cmd/moombox/services.go`, beside the existing reporter wiring (currently lines 151-153):

```go
	utils.SetConnectivityReporter(s.connMon)
	engine.SetConnectivityReporter(s.connMon)
	monitor.SetConnectivityReporter(s.connMon)
	worker.SetConnectivityReporter(s.connMon)
```

(Verify `worker` is imported in `services.go`; it is — `dlWorker` is constructed there.)

- [ ] **Step 5: Run test + build to verify**

Run: `go test ./internal/worker/ -run TestWorkerConnectivityReporterRoundTrip -v && go build ./cmd/moombox/`
Expected: PASS + build success.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/connectivity.go internal/worker/connectivity_test.go cmd/moombox/services.go
git commit -m "feat(worker): package connectivity reporter; probes feed the passive tracker"
```

---

### Task 7: Apply decision + floor in the YouTube wait loop

**Files:**
- Modify: `internal/worker/stream_processor_youtube.go` (`waitForLive`, ~lines 21-152)

- [ ] **Step 1: Add the offline-floor constant**

In `internal/worker/stream_processor.go`, add to the `const` block (near `maxConsecutiveProbeErrors`):

```go
	// offlineProbeFloor throttles probing while the oracle reports offline.
	// We probe anyway (not skip) so a wrongly-offline oracle can't strand a
	// waiting stream — network errors no longer count — but no faster than
	// this to avoid hammering during a real outage / chat-surge early wakes.
	offlineProbeFloor = 60 * time.Second
```

- [ ] **Step 2: Declare the floor timestamp in waitForLive**

In `waitForLive` (`stream_processor_youtube.go`), beside the other loop-local state (near `consecutiveErrors := 0`, ~line 29):

```go
	consecutiveErrors := 0
	var lastOfflineProbe time.Time // zero ⇒ first offline encounter probes immediately
```

- [ ] **Step 3: Replace the hard offline skip (current ~lines 125-129)**

Replace:

```go
		// Skip probe when offline — don't burn error counter
		if sp.isOnline != nil && !sp.isOnline() {
			sp.logger.Debug("skipping probe — device offline", "videoID", job.VideoID)
			continue
		}
```

with:

```go
		// When the oracle reports offline, still probe occasionally (floor) so
		// a wrongly-offline oracle can't strand a waiting stream. Safe because
		// network-class errors no longer count (see probeErrorDecision), and a
		// success self-corrects the oracle via reportProbeResult.
		if sp.isOnline != nil && !sp.isOnline() {
			if time.Since(lastOfflineProbe) < offlineProbeFloor {
				sp.logger.Debug("skipping probe — device offline (within floor)", "videoID", job.VideoID)
				continue
			}
			lastOfflineProbe = time.Now()
		}
```

- [ ] **Step 4: Replace the error-counting block (current ~lines 139-151)**

Replace:

```go
		if err := probeErr; err != nil {
			consecutiveErrors++
			sp.logger.Warn("probe error", "videoID", job.VideoID, "err", err, "consecutive", consecutiveErrors)
			if consecutiveErrors >= maxConsecutiveProbeErrors {
				sp.stopEarlyChat(chatDl)
				// Wrap with ErrNonActionable so worker.setJobError suppresses
				// the user notification — exhausted probe retries mean the
				// stream isn't going to come up regardless of further work
				// (audit cross-cutting.md C3 follow-up).
				return nil, fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)
			}
			continue
		}
		consecutiveErrors = 0
```

with:

```go
		if probeErr != nil {
			d := probeErrorDecision(probeErr)
			switch d.report {
			case reportFailure:
				reportProbeResult("probe/youtube", true)
			case reportSuccess:
				reportProbeResult("probe/youtube", false)
			}
			if d.cancelled {
				sp.stopEarlyChat(chatDl)
				return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
			}
			if d.count {
				consecutiveErrors++
				sp.logger.Warn("probe error (definitive)", "videoID", job.VideoID, "err", probeErr, "consecutive", consecutiveErrors)
				if consecutiveErrors >= maxConsecutiveProbeErrors {
					sp.stopEarlyChat(chatDl)
					// Wrap with ErrNonActionable so worker.setJobError suppresses
					// the user notification — exhausted DEFINITIVE retries mean
					// the stream isn't coming up regardless of further work.
					return nil, fmt.Errorf("max probe errors: %w (%w)", probeErr, ErrNonActionable)
				}
			} else {
				// Network-class failure: the internet/service is unreachable.
				// Keep waiting through the outage — do not burn the budget.
				sp.logger.Debug("probe network error — not counting, still waiting", "videoID", job.VideoID, "err", probeErr)
			}
			continue
		}
		consecutiveErrors = 0
```

- [ ] **Step 5: Build + run the worker tests**

Run: `go build ./internal/worker/ && go test ./internal/worker/ -v`
Expected: PASS (existing tests + Task 5/6 tests). The loop change is a mechanical application of the unit-tested `probeErrorDecision`.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/stream_processor.go internal/worker/stream_processor_youtube.go
git commit -m "fix(worker): YouTube wait loop tolerates connectivity loss; network probe errors no longer error the stream"
```

---

### Task 8: Apply decision + floor in the Twitch wait loop

**Files:**
- Modify: `internal/worker/stream_processor_twitch.go` (`waitForTwitchLive`, ~lines 300-354)

- [ ] **Step 1: Declare the floor timestamp**

Beside `consecutiveErrors := 0` (~line 307):

```go
	consecutiveErrors := 0
	var lastOfflineProbe time.Time
```

- [ ] **Step 2: Replace the hard offline skip (current ~lines 336-339)**

Replace:

```go
		if sp.isOnline != nil && !sp.isOnline() {
			sp.logger.Debug("skipping Twitch probe — device offline", "login", login)
			continue
		}
```

with:

```go
		if sp.isOnline != nil && !sp.isOnline() {
			if time.Since(lastOfflineProbe) < offlineProbeFloor {
				sp.logger.Debug("skipping Twitch probe — device offline (within floor)", "login", login)
				continue
			}
			lastOfflineProbe = time.Now()
		}
```

- [ ] **Step 3: Replace the error-counting block (current ~lines 341-354)**

Replace:

```go
		streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			consecutiveErrors++
			sp.logger.Warn("twitch poll error", "channel", login, "err", err, "consecutive", consecutiveErrors)
			if consecutiveErrors >= maxConsecutiveProbeErrors {
				// Wrap with ErrNonActionable so worker.setJobError suppresses
				// the user notification — the retry budget exhausted means
				// the stream isn't going to come up regardless of further
				// probes (audit cross-cutting.md C3 follow-up).
				return nil, fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)
			}
			continue
		}
		consecutiveErrors = 0
```

with:

```go
		streamInfo, err := sp.tw.GetStreamInfo(ctx, login)
		if err != nil {
			d := probeErrorDecision(err)
			switch d.report {
			case reportFailure:
				reportProbeResult("probe/twitch", true)
			case reportSuccess:
				reportProbeResult("probe/twitch", false)
			}
			if d.cancelled {
				return nil, nil
			}
			if d.count {
				consecutiveErrors++
				sp.logger.Warn("twitch poll error (definitive)", "channel", login, "err", err, "consecutive", consecutiveErrors)
				if consecutiveErrors >= maxConsecutiveProbeErrors {
					// Wrap with ErrNonActionable so worker.setJobError suppresses
					// the user notification — exhausted DEFINITIVE retries mean
					// the stream isn't coming up regardless of further probes.
					return nil, fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)
				}
			} else {
				sp.logger.Debug("twitch network error — not counting, still waiting", "channel", login, "err", err)
			}
			continue
		}
		consecutiveErrors = 0
```

- [ ] **Step 4: Build + test**

Run: `go build ./internal/worker/ && go test ./internal/worker/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/stream_processor_twitch.go
git commit -m "fix(worker): Twitch wait loop tolerates connectivity loss (same fix as YouTube)"
```

---

### Task 9: Full verification

**Files:** none (verification only; small doc touch-ups if warranted)

- [ ] **Step 1: Build + vet the whole module**

Run: `go build ./... && go vet ./...`
Expected: both clean.

- [ ] **Step 2: Full test suite**

Run: `go test ./...`
Expected: all `ok` / no-test, zero `FAIL`.

- [ ] **Step 3: Race detector on the changed packages**

Run: `go test -race ./internal/connectivity/ ./internal/worker/ ./internal/config/`
Expected: PASS, no race reports.

- [ ] **Step 4: Confirm no stale references to deleted code**

Run: `git grep -n "checkInternetConnected" || echo "clean"`
Expected: `clean` (no references remain).

- [ ] **Step 5: Commit any doc updates (if made)**

```bash
git add -A
git commit -m "docs: note connectivity-detection redesign behavior" || echo "nothing to commit"
```

---

## Self-Review

- **Spec coverage:** Layer 1 (shared probe) → Tasks 1-2,4; Layer 2 (no hysteresis) → no poll() change, preserved; Layer 3 (classifier) → Tasks 5,7,8; Layer 4 (probes feed oracle) → Tasks 6,7,8; Layer 5 (offline floor) → Tasks 7,8; Layer 6 (config) → Task 3. Deferred items remain deferred. All covered.
- **Placeholder scan:** every code step contains complete code; commands have expected output. No TBD/TODO.
- **Type consistency:** `probeErrClass{classNetwork,classServer,classCancelled}`, `probeReport{reportNone,reportFailure,reportSuccess}`, `probeDecision{count,cancelled,report}`, `probeErrorDecision(err) probeDecision`, `reportProbeResult(tag,failed)`, `SetConnectivityReporter`, `SetProbeTargets`, `reachabilityProbe(ctx,targets)`, `defaultProbeTargets`/`config.DefaultProbeTargets`, `ConnectivityConfig.ProbeTargets`, `offlineProbeFloor` — all referenced consistently across tasks.
- **Verification note:** terminal "video is gone" outcomes arrive via the info path (HTTP 200 with terminal `StreamStatus`/`PlayabilityError`), handled by the existing `switch probeInfo.StreamStatus` in `waitForLive` — not via the error counter. The error counter now only fires on definitive (4xx-non-429) probe errors, which is the intended failsafe.
