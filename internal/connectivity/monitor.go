package connectivity

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// defaultPollInterval is the production poll cadence. NewMonitor uses this;
// tests and operators with non-default needs use NewMonitorWithInterval to
// override (e.g. faster cadence when stress-testing the transition logic,
// slower cadence on power-constrained hosts). Audit reports/small-packages.md.
const defaultPollInterval = 5 * time.Second

type Monitor struct {
	online atomic.Bool
	// offlinePolls is atomic: poll() increments it on the ticker goroutine
	// while transition(true) — reachable from ReportSuccess on arbitrary
	// HTTP-caller goroutines — resets it.
	offlinePolls atomic.Int32
	mu           sync.Mutex
	callbacks    map[uint64]func(online bool)
	nextID       uint64
	cancel       context.CancelFunc
	started      atomic.Bool
	checkFn      func() bool
	pollInterval time.Duration
	passive      *PassiveTracker
	probeTargets []string // reachability-probe targets; nil → defaultProbeTargets
	logger       logger
}

type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Reporter provides network result reporting. Passed to HTTP utilities.
type Reporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// NewMonitor constructs a connectivity monitor with the default 5s poll
// cadence. Use NewMonitorWithInterval if you need a different cadence.
func NewMonitor(log logger) *Monitor {
	return NewMonitorWithInterval(log, defaultPollInterval)
}

// NewMonitorWithInterval constructs a connectivity monitor with a
// caller-specified poll cadence. Values < 100ms are clamped up to that
// minimum to keep tight test loops from saturating the polling
// goroutine — a tighter check window doesn't catch transitions any
// faster, since the underlying syscall takes ~tens-of-ms anyway.
func NewMonitorWithInterval(log logger, interval time.Duration) *Monitor {
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
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
}

// Start launches the background poll goroutine. Calling Start more than once
// on the same Monitor is a no-op — we ignore subsequent calls rather than
// leaking the first cancel func and spawning a second goroutine.
func (m *Monitor) Start(ctx context.Context) {
	if !m.started.CompareAndSwap(false, true) {
		return
	}
	ctx2, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// Seed state with a synchronous probe so IsOnline() reflects reality before
	// the first tick fires 5 seconds later. Without this, a machine that boots
	// with no network will report online=true for up to 5 seconds.
	if !m.checkFn() {
		m.offlinePolls.Store(2) // skip the debounce — we already know we are offline
		m.poll()
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				if m.logger != nil {
					m.logger.Error("connectivity monitor panic", "panic", r)
				}
			}
		}()
		ticker := time.NewTicker(m.pollInterval)
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

func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Monitor) IsOnline() bool {
	return m.online.Load()
}

// SetProbeTargets overrides the reachability-probe targets (wired from config).
// Call once during init, BEFORE Start(); not safe to call concurrently with
// the poll goroutine. An empty/nil slice falls back to defaultProbeTargets.
func (m *Monitor) SetProbeTargets(targets []string) {
	m.probeTargets = targets
}

// OnStateChange registers a callback invoked whenever connectivity transitions
// online↔offline. Returns an unsubscribe function.
//
// **Latency / serialisation:** callbacks fire serially in the goroutine that
// detected the transition (typically the polling goroutine). A slow callback
// blocks every subsequent subscriber AND delays the next poll. Keep handlers
// short-running; if any work might block, hand off to a goroutine inside the
// callback. Audit reports/small-packages.md.
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

func (m *Monitor) ReportFailure(tag string) {
	m.passive.ReportFailure(tag)
	if m.online.Load() && m.passive.ShouldTriggerOffline() {
		m.transition(false)
	}
}

func (m *Monitor) ReportSuccess(tag string) {
	wasPassiveOffline := m.passive.IsTriggered()
	m.passive.ReportSuccess(tag)
	// A real successful request is direct proof of connectivity, so once it
	// clears the passive latch we transition online immediately — without an
	// extra active probe. Calling checkFn() here would block the CALLER's
	// goroutine (e.g. a wait loop via reportProbeResult, or any HTTP-success
	// path) for up to probeRaceTimeout, and is redundant: the 5s poll loop
	// re-confirms reachability via the active probe regardless. Speed-first
	// recovery (design Layer 2).
	if wasPassiveOffline && !m.passive.IsTriggered() {
		m.transition(true)
	}
}

func (m *Monitor) poll() {
	online := m.checkFn()
	// IsTriggeredPruned (not IsTriggered) so aged-out failures clear the latch
	// here in the polling loop. During an outage every subsystem gates off its
	// network I/O once we go offline, so nothing else ever feeds the passive
	// tracker again — poll() is the only live path, and a non-pruning read
	// would keep the latch stuck true forever, pinning us offline even after
	// the active probe recovers.
	passiveOffline := m.passive.IsTriggeredPruned()
	nowOnline := online && !passiveOffline

	wasOnline := m.online.Load()

	if nowOnline {
		m.offlinePolls.Store(0)
		if !wasOnline {
			m.transition(true)
		}
	} else {
		if polls := m.offlinePolls.Add(1); wasOnline && polls >= 2 {
			m.transition(false)
		}
	}
}

func (m *Monitor) transition(online bool) {
	old := m.online.Swap(online)
	if old == online {
		return
	}
	if online {
		m.offlinePolls.Store(0)
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
