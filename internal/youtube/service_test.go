package youtube

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// noopLogger is a logger implementation that discards everything; used by
// tests that don't care about log output.
type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// newTestService spins up a Service with no cookies/jar wired. The fields
// being tested only touch the visitor-data slot, so the auth/PlayerAPI fields
// can stay as their zero/default values for these tests.
func newTestService(t *testing.T) *Service {
	t.Helper()
	return &Service{
		logger: noopLogger{},
	}
}

// jarServiceFromCookieFile builds a FULLY WIRED Service around a jar loaded
// from a Netscape cookie file written into the test's temp dir.
//
// Use this, not newTestService, for anything that touches auth: newTestService
// leaves Auth nil, and both liveness probes open with s.Auth.HasAnyAuthCookie()
// — so the &Service{} shortcut nil-panics before the code under test runs.
// Shared by the membership and account-probe suites; the cookie-file constants
// live in channel_membership_test.go.
func jarServiceFromCookieFile(t *testing.T, cookieFile string) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFile), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	return NewService(jar, noopLogger{})
}

func TestSetVisitorData_StickyOnFirstWrite(t *testing.T) {
	s := newTestService(t)

	s.SetVisitorData("first")
	if got := s.GetVisitorData(); got != "first" {
		t.Fatalf("expected 'first' after initial set, got %q", got)
	}

	// Second write before TTL must be a no-op — sticky semantics keep the
	// originally-issued visitor data so the POT cache continues hitting.
	s.SetVisitorData("second")
	if got := s.GetVisitorData(); got != "first" {
		t.Errorf("sticky write violated: expected 'first' to be retained, got %q", got)
	}
}

func TestSetVisitorData_EmptyValueIgnored(t *testing.T) {
	s := newTestService(t)

	s.SetVisitorData("real")
	s.SetVisitorData("") // must not clear
	if got := s.GetVisitorData(); got != "real" {
		t.Errorf("empty SetVisitorData should be a no-op, got %q", got)
	}
}

func TestSetVisitorData_TTLAllowsRefresh(t *testing.T) {
	s := newTestService(t)

	s.SetVisitorData("first")
	// Manually backdate the cached value past the TTL window so the next
	// write replaces it. Mirrors the long-running session that should
	// eventually rotate even without an explicit invalidation.
	s.vdMu.Lock()
	s.visitorDataSetAt = time.Now().Add(-(visitorDataTTL + time.Minute))
	s.vdMu.Unlock()

	s.SetVisitorData("second")
	if got := s.GetVisitorData(); got != "second" {
		t.Errorf("expected TTL-aged cache to accept refresh, got %q", got)
	}
}

func TestInvalidateVisitorData_AllowsImmediateRefresh(t *testing.T) {
	s := newTestService(t)

	s.SetVisitorData("first")
	s.InvalidateVisitorData()
	if got := s.GetVisitorData(); got != "" {
		t.Fatalf("expected InvalidateVisitorData to clear cached value, got %q", got)
	}

	// After invalidation, the very next non-empty SetVisitorData must win
	// (no TTL gating since there's no cached value to be sticky against).
	s.SetVisitorData("fresh")
	if got := s.GetVisitorData(); got != "fresh" {
		t.Errorf("expected fresh value after invalidate, got %q", got)
	}
}

func TestSetVisitorData_TimestampUpdatedOnRefresh(t *testing.T) {
	s := newTestService(t)

	s.SetVisitorData("first")
	s.vdMu.RLock()
	t0 := s.visitorDataSetAt
	s.vdMu.RUnlock()
	if t0.IsZero() {
		t.Fatalf("expected visitorDataSetAt to be populated on first write")
	}

	// Force TTL to elapse, refresh, and confirm the timestamp moved forward.
	// Backdate to a value that's clearly older than t0 so the After check
	// is robust to coarse Windows clock granularity (~16ms on some systems).
	backdate := t0.Add(-(visitorDataTTL + time.Minute))
	s.vdMu.Lock()
	s.visitorDataSetAt = backdate
	s.vdMu.Unlock()

	s.SetVisitorData("second")
	s.vdMu.RLock()
	t1 := s.visitorDataSetAt
	s.vdMu.RUnlock()
	if !t1.After(backdate) {
		t.Errorf("expected visitorDataSetAt to advance past backdated value; backdate=%v t1=%v", backdate, t1)
	}
}
