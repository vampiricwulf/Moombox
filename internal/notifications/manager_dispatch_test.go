package notifications

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestManagerWaitTimesOut verifies the 30s timeout branch in Wait()
// actually fires when senders never complete. We use a sender that
// blocks on a never-closed channel so the WaitGroup stays held — this
// is the only way to exercise the timeout path, since DiscordWebhook
// has its own 15s ctx deadline that would unblock wg.Wait early.
// Audit reports/small-packages.md notifications Wait timeout.
func TestManagerWaitTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("Wait waits 30s on timeout — skipping in -short mode")
	}

	hangForever := make(chan struct{}) // intentionally never closed
	t.Cleanup(func() {
		// We can't easily release the goroutine — it's wedged on a
		// never-closing channel. The test process exits when the test
		// completes; the wedged goroutine is collected with it.
		_ = hangForever
	})

	hanging := senderFunc(func(string, string, int, []Field, SendOptions) error {
		<-hangForever
		return nil
	})

	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets:   []notificationTarget{{sender: hanging}},
	}
	m.Send("t", "d", TypeInfo, nil, SendOptions{})

	start := time.Now()
	done := make(chan struct{})
	go func() {
		m.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(35 * time.Second):
		t.Fatal("Wait did not return within 35s — timeout branch did not fire")
	}
	elapsed := time.Since(start)
	if elapsed < 28*time.Second {
		t.Errorf("Wait returned in %v — expected ~30s timeout", elapsed)
	}
}

// TestManagerSemaphoreBoundsConcurrency exercises the
// maxInflightNotifications cap added to bound concurrent notification
// goroutines. We send 2× the cap in quick succession and verify that
// at most cap goroutines are in flight at any moment. Audit
// reports/small-packages.md notifications manager unbounded goroutine.
func TestManagerSemaphoreBoundsConcurrency(t *testing.T) {
	var inflight atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})

	hangSender := senderFunc(func(title, description string, color int, fields []Field, opts SendOptions) error {
		now := inflight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return nil
	})

	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets:   []notificationTarget{{sender: hangSender}},
	}

	// Send 2× the cap so the semaphore must drop excess.
	const send = maxInflightNotifications * 2
	for range send {
		m.Send("t", "d", TypeInfo, nil, SendOptions{})
	}

	// Briefly let goroutines park on `<-release`.
	time.Sleep(50 * time.Millisecond)

	if got := peak.Load(); got > maxInflightNotifications {
		t.Errorf("peak in-flight = %d, want ≤ %d", got, maxInflightNotifications)
	}

	close(release)

	// Drain via Wait so the t.Cleanup'd HTTP servers can close.
	doneCh := make(chan struct{})
	go func() {
		m.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Manager did not drain within 2s after release")
	}
}

// senderFunc adapts a function to the sender interface for tests.
type senderFunc func(title, description string, color int, fields []Field, opts SendOptions) error

func (f senderFunc) Send(title, description string, color int, fields []Field, opts SendOptions) error {
	return f(title, description, color, fields, opts)
}

// _ keeps sync.WaitGroup-style imports used by other tests in this
// package consistent — not strictly needed here.
var _ sync.Mutex

// TestNewManagerRejectsDiscordSchemeEdgeCases covers the discord://
// URL forms the audit flagged as untested: extra path segments
// (caller bug accidentally appending /more), empty token, missing
// token entirely. NewManager should Warn and skip without panicking.
func TestNewManagerRejectsDiscordSchemeEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool // true = should be accepted (we keep ID/TOKEN), false = rejected
	}{
		{"missing both segments", "discord://", false},
		{"only ID, no token", "discord://12345", false},
		{"trailing slash drops token", "discord://12345/", false},
		{"three segments — ID/TOKEN kept, /more dropped", "discord://12345/abc/extra", true},
		{"empty ID", "discord:///mytoken", false},
		{"normal two-segment passes", "discord://12345/mytoken", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MoomboxConfig{
				Notifications: []config.NotificationConfig{{URL: tc.url}},
			}
			m := NewManager(cfg, testLogger{})
			if got := m.HasTargets(); got != tc.want {
				t.Errorf("HasTargets for %q: want %v, got %v", tc.url, tc.want, got)
			}
		})
	}
}
