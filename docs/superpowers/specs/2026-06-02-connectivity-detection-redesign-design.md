# Connectivity Detection Redesign — Accurate Offline/Online + Resilient Upcoming Streams

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner decision on file:** speed-first recovery; rely on consumer-side resilience so premature "online" cannot error a stream.

## 1. Problem

Upcoming streams were being moved to `Error` ("max probe errors") during an internet
outage, when they should have stayed in `Upcoming` and resumed once the network returned.

There are **two independent root causes**, and both must be fixed or the bug recurs:

1. **The global connectivity oracle reports adapter state, not reachability.**
   On Windows, `internal/connectivity/monitor_windows.go` uses
   `InternetGetConnectedState` (WinINet). Microsoft's own docs state a `TRUE` result
   "does not guarantee that a connection to a specific host can be established" — it
   reflects whether an adapter/route is configured, and is documented to keep
   reporting `TRUE` after the cable is unplugged until something actually fails. So the
   monitor can report **online while the internet is genuinely down**. Linux
   (`monitor_unix.go`) already does a real TCP dial to `1.1.1.1:443`; the platforms
   diverge, which is itself the defect. Recovery also flips online on a **single** good
   poll (`monitor.go` `poll()`), so one spurious "connected" reading flips the state.

2. **The upcoming-stream loop counts the wrong failures.**
   `internal/worker/stream_processor_youtube.go` `waitForLive` increments
   `consecutiveErrors` on **every** probe failure (line ~139) and errors the stream
   after `maxConsecutiveProbeErrors = 10` (line ~142). Its only protection is the
   `if !sp.isOnline() { continue }` skip-guard (line ~126), which helps **only when the
   oracle is correctly offline**. When the oracle is wrongly online (root cause 1) — or
   during the unavoidable race window between outage onset and the oracle's debounced
   offline transition — real network failures accumulate and kill a waiting stream. The
   Twitch loop (`stream_processor_twitch.go`) shares the same counter and bug.

### Causal chain (verified against the code)
outage (or premature "online") → `isOnline()` returns true → `waitForLive` does **not**
skip → `yt.ProbeVideoStatus` fails with a network error → `consecutiveErrors++` → after
10 → `return ... fmt.Errorf("max probe errors: %w (%w)", err, ErrNonActionable)` → job
goes to `Error`.

## 2. Goals & non-goals

**Goals**
- Upcoming (and live/downloading) jobs must not error due to connectivity loss or a
  transient/premature "online" flip.
- Recovery stays **fast** (resume on the first good signal — owner's explicit choice).
- End the Windows/Linux probe divergence; make the active probe an actual reachability
  test on every platform, at **no added recovery latency**.
- Pure Go, no CGo. Resource-efficient (24/7). Fit the existing `Monitor` +
  `OnStateChange` + worker/engine architecture without changing the `IsOnline() bool`
  contract.

**Non-goals (deliberately deferred — documented, not built)**
- Confirm-before-online **hysteresis** (N consecutive good probes). Rejected because it
  adds recovery latency, conflicting with the speed-first decision; Layer 3 makes it
  unnecessary.
- Captive-portal HTTP body-probe (NCSI-style exact-body match).
- `NotifyAddrChange` / NLM COM event triggers for sub-second recovery.
- Service-specific oracle probing of YouTube/Twitch hosts (the consumer classifier
  already handles service-specific outages).
- A graded multi-state machine (LinkDown/Unvalidated/CaptivePortal/Online).

Each is sound but adds cost/surface without addressing *this* bug. Captive-portal and
link-event triggers are the most likely future follow-ups.

## 3. Design (5 layers)

### Layer 1 — One accurate, shared reachability probe
New file `internal/connectivity/probe.go` (platform-agnostic). **Delete**
`monitor_windows.go` and `monitor_unix.go` and their `checkInternetConnected`.

```go
// defaultProbeTargets: anycast HTTPS endpoints. Port 443 (not 53) because some
// networks block outbound DNS but allow HTTPS. Multiple targets so one host's
// outage can't cause a false-offline. Overridable via [connectivity] probe_targets.
var defaultProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

const probeRaceTimeout = 3 * time.Second

// reachabilityProbe races TCP dials to targets; returns true as soon as ANY
// handshake completes. A completed handshake proves real routability to a live
// host — unlike InternetGetConnectedState, which only reports adapter state.
func reachabilityProbe(ctx context.Context, targets []string) bool {
    if len(targets) == 0 { targets = defaultProbeTargets }
    ctx, cancel := context.WithTimeout(ctx, probeRaceTimeout)
    defer cancel() // cancels still-in-flight dials once we have a winner
    resultCh := make(chan bool, len(targets)) // buffered: late senders never block
    var d net.Dialer
    for _, t := range targets {
        go func(addr string) {
            conn, err := d.DialContext(ctx, "tcp", addr)
            if err != nil { resultCh <- false; return }
            _ = conn.Close()
            resultCh <- true
        }(t)
    }
    for range targets {
        if <-resultCh { return true }
    }
    return false
}
```

Monitor wiring: `Monitor` gains a `probeTargets []string` field, defaulting to
`defaultProbeTargets` in the constructor. Its default `checkFn` becomes a closure
`func() bool { return reachabilityProbe(context.Background(), m.probeTargets) }` (reads
`m.probeTargets` dynamically). `checkFn` stays injectable so existing
`newTestMonitor(checkFn)` tests are unchanged. To override the targets from config
**without changing the `NewMonitor` / `NewMonitorWithInterval` signatures** (which several
tests call), add `func (m *Monitor) SetProbeTargets([]string)`; `cmd/moombox/services.go`
calls it once **before** `Start()`. Targets are set during single-threaded init and not
mutated concurrently with the poll goroutine.

Cost: ~3 SYNs per 5s poll when online (winner replies in ms, connection closed
immediately); up to one `probeRaceTimeout` window per poll when offline. Negligible.

### Layer 2 — Recovery stays instant (no hysteresis)
No change to `poll()`'s online direction: a single good poll flips online immediately.
Keep the existing 2-poll **leave** debounce and the passive tracker's fast trip-to-offline.
The prior fix (`poll()` uses `passive.IsTriggeredPruned()`) stays. This honors the
speed-first decision; Layer 3 makes premature flips harmless.

### Layer 3 — Consumer error classification (the real fix)
New file `internal/worker/probe_classify.go`:

```go
type probeErrClass int
const (
    classNetwork   probeErrClass = iota // transient/connectivity — DO NOT count
    classServer                         // definitive service verdict — count
    classCancelled
)

// classifyProbeErr categorizes a probe/fetch error. ASYMMETRIC DEFAULT:
// an unrecognized error is treated as classNetwork (do-not-count). Under the
// cost model a missed classification only DELAYS giving up; it never wrongly
// errors a waiting stream.
func classifyProbeErr(err error) probeErrClass {
    if err == nil { return classServer }
    if errors.Is(err, context.Canceled) { return classCancelled }
    if errors.Is(err, context.DeadlineExceeded) { return classNetwork }

    var urlErr *url.Error
    if errors.As(err, &urlErr) && urlErr.Err != nil { return classifyProbeErr(urlErr.Err) }

    var netErr net.Error
    if errors.As(err, &netErr) { return classNetwork }
    var opErr *net.OpError
    if errors.As(err, &opErr) { return classNetwork }
    var dnsErr *net.DNSError
    if errors.As(err, &dnsErr) { return classNetwork }
    if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) { return classNetwork }

    // String fallback for doRetryRequest's lossy wraps (transport detail is
    // flattened to a message for 5xx/429 — see player_api_strategy.go).
    msg := strings.ToLower(err.Error())
    switch {
    case strings.Contains(msg, "http 429"),                 // rate limited — transient
        strings.Contains(msg, "http 5"),                    // 5xx — server transient
        strings.Contains(msg, "tls"),
        strings.Contains(msg, "timeout"),
        strings.Contains(msg, "connection reset"),
        strings.Contains(msg, "no such host"),
        strings.Contains(msg, "eof"):
        return classNetwork
    case strings.Contains(msg, "http 4"):                   // 4xx (non-429) — definitive
        return classServer
    }
    return classNetwork // asymmetric default
}
```

Integration in `waitForLive` (and the Twitch loop's equivalent):

```go
if probeErr != nil {
    switch classifyProbeErr(probeErr) {
    case classCancelled:
        return &StreamProcessResult{ShouldDownload: false, Error: "cancelled"}, nil
    case classNetwork:
        // Connectivity/transient — keep waiting; feed the oracle.
        sp.reportFailure("probe/youtube")
        // (do NOT touch consecutiveErrors)
        continue
    case classServer:
        consecutiveErrors++
        sp.reportSuccess("probe/youtube") // request reached the service
        if consecutiveErrors >= maxConsecutiveProbeErrors {
            // genuinely unactionable — give up
            return nil, fmt.Errorf("max probe errors: %w (%w)", probeErr, ErrNonActionable)
        }
        continue
    }
}
consecutiveErrors = 0
```

`maxConsecutiveProbeErrors` now means "the service keeps definitively refusing," not
"the network blipped." The "video is genuinely gone" path is primarily handled where it
already is — the **info path** (a parseable HTTP 200 with a terminal `StreamStatus` /
`PlayabilityError`), not the error counter. *Implementation must verify that terminal
statuses (removed/private/unavailable) surfacing via the info path still cause the loop
to give up appropriately; if a gap exists it is handled there, not via the error counter.*

### Layer 4 — Probes feed the oracle
Today the YouTube/Twitch probes do not report to the passive tracker at all. Add a
minimal reporter to `StreamProcessor` (defined as a local interface to avoid importing
`connectivity`):

```go
type connectivityReporter interface {
    ReportFailure(tag string)
    ReportSuccess(tag string)
}
```

Wired from `cmd/moombox/services.go` with `s.connMon` (which implements it). `classNetwork`
→ `ReportFailure("probe/youtube")`; `classServer`/success → `ReportSuccess("probe/youtube")`.
Nil-safe (`sp.reportFailure`/`reportSuccess` no-op when unset, matching the `isOnline`
pattern).

### Layer 5 — Probe-anyway floor (oracle-independent safety)
Replace `waitForLive`'s hard offline skip with a throttled floor so a *wrongly-offline*
oracle can never strand a waiting stream:

```go
const offlineProbeFloor = 60 * time.Second
// ...
if sp.isOnline != nil && !sp.isOnline() {
    if time.Since(lastOfflineProbe) < offlineProbeFloor {
        sp.logger.Debug("skipping probe — device offline (within floor)", "videoID", job.VideoID)
        continue
    }
    lastOfflineProbe = time.Now()
    // fall through and probe anyway — network errors don't count (Layer 3),
    // and a success self-corrects the oracle via ReportSuccess (Layer 4).
}
```

Because network errors no longer count, probing while the oracle says offline is safe;
this makes the consumer correct **regardless** of which way the oracle errs, so the two
layers are independent. Throttling to once per `offlineProbeFloor` guards against
chat-surge early wakes hammering during a real outage.

### Layer 6 — Config
New `[connectivity]` section, config-file level (expert knob; no Web UI/TUI surfacing in
v1):

```toml
[connectivity]
probe_targets = ["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"]
```

- Validation: each entry must be a valid `host:port`; empty list falls back to the
  defaults; reject malformed entries with a clear error.
- Migration: non-destructive — absent section uses defaults (`config.go`
  `migrateOldFormat` / defaults).

## 4. Components & boundaries
- `internal/connectivity/probe.go` — pure reachability probe. Input: targets. Output:
  bool. No state.
- `internal/connectivity/monitor.go` — unchanged contract (`IsOnline()`, `OnStateChange`),
  now holds `probeTargets`, default `checkFn` = probe. Delete the two platform files.
- `internal/worker/probe_classify.go` — pure `error → class`. Table-testable in isolation.
- `internal/worker/stream_processor*.go` — consume the classifier; add reporter +
  offline floor.
- `internal/config` — new `ConnectivityConfig{ ProbeTargets []string }`.

## 5. Error handling
- Probe goroutines never leak: buffered result channel + `defer cancel()` on early win.
- Classifier is total (always returns a class; nil → `classServer` defensively).
- Reporter/`isOnline` calls are nil-safe.
- A successful probe while the oracle is offline feeds `ReportSuccess`, letting the
  monitor self-correct.

## 6. Testing
- `probe_test.go`: success against a local `net.Listen` target; failure against a
  reserved unreachable address; race returns on first success; honors timeout; empty
  targets → defaults.
- `probe_classify_test.go`: table-driven over `*url.Error`-wrapped `*net.OpError`,
  `*net.DNSError`, `context.DeadlineExceeded`, `io.ErrUnexpectedEOF`, "HTTP 429",
  "HTTP 503", "HTTP 404", "tls: ...", and an unknown error → asymmetric default
  (`classNetwork`); definitive 4xx → `classServer`; cancelled → `classCancelled`.
- `stream_processor_*_test.go`: **regression** — feed N network-class probe errors and
  assert the job is NOT errored and `consecutiveErrors` stays 0; feed
  `maxConsecutiveProbeErrors` server-class errors and assert it errors. Same for Twitch.
- `monitor_test.go`: existing injected-`checkFn` tests stay green; add a test wiring the
  real probe via a stubbed dialer/loopback listener.
- Full suite green: `go build ./...`, `go vet ./...`, `go test ./...`, plus `-race` on
  `internal/connectivity` and `internal/worker`.

## 7. Risks & mitigations
- **Probe targets blocked** (corp/DNS-filtered nets block resolver IPs on :443) →
  false-offline. Mitigated: configurable `probe_targets`; Layer 5 still probes; Layer 3
  means streams don't error regardless.
- **Classifier mislabels a definitive error as network** → stream waits longer than
  ideal rather than erroring. Accepted by design (asymmetric default); the info path
  handles genuine give-up.
- **Deleting the wininet path** → confirm no other references to `checkInternetConnected`
  exist before removal.

## 8. Out of scope / follow-ups
Captive-portal body-probe; `NotifyAddrChange`/NLM link-event triggers; service-specific
reachability state surfaced in the UI; recovery hysteresis. Track as future enhancements
gated on real-world demand.
