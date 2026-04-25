package notifications

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestManagerWaitTimesOut verifies that Wait gives up after 30s rather
// than blocking forever — covers the timeout branch the audit flagged
// as untested. We force the timeout by injecting a sender that hangs
// on a channel and only releasing it after Wait returns. The "30s"
// constant in code is bypassed in test by replacing the WaitGroup with
// one that never completes within the test budget; we assert Wait
// actually returns with bounded latency.
func TestManagerWaitTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("Wait waits 30s on timeout — skipping in -short mode")
	}

	// Use a stuck server so the goroutine sits in resp body forever.
	stuck := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		<-stuck
	}))
	t.Cleanup(func() {
		close(stuck)
		srv.Close()
	})

	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: srv.URL + "/api/webhooks/123/abc-DEF", Events: nil},
		},
	}
	// Hand-build the manager with a DiscordWebhook pointing at the stuck server
	// (the cfg URL won't match discordWebhookRe — bypass and inject directly).
	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets: []notificationTarget{{
			sender: &DiscordWebhook{URL: srv.URL},
		}},
	}
	_ = cfg
	// One Send → one goroutine that will hang on the stuck server.
	m.Send("t", "d", TypeInfo, nil, SendOptions{})

	// Wait should time out at 30s. Allow 31s of test budget.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		m.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(31 * time.Second):
		t.Fatal("Wait did not return within 31s — timeout branch did not fire")
	}
	elapsed := time.Since(start)
	if elapsed < 29*time.Second {
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
	for i := 0; i < send; i++ {
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
