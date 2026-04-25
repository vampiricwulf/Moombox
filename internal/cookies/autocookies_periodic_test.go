package cookies

import (
	"testing"
)

// nopAutoCookieLogger is a minimal logger sink for the periodic-refresh tests
// — they don't exercise log output, only the skip-decision contract.
type nopAutoCookieLogger struct{}

func (nopAutoCookieLogger) Debug(msg string, args ...any) {}
func (nopAutoCookieLogger) Info(msg string, args ...any)  {}
func (nopAutoCookieLogger) Warn(msg string, args ...any)  {}
func (nopAutoCookieLogger) Error(msg string, args ...any) {}

// TestShouldSkipPeriodicRefresh covers the three states of the
// HasActiveJobs callback contract added for audit reports/cookies.md #23.
func TestShouldSkipPeriodicRefresh(t *testing.T) {
	tests := []struct {
		name string
		// hook is left nil to skip wiring HasActiveJobs entirely; otherwise
		// the test installs hook as the callback before the assertion.
		hook func() bool
		want bool
	}{
		{
			name: "nil callback preserves legacy always-fire behaviour",
			hook: nil,
			want: false,
		},
		{
			name: "callback returns true (active jobs exist) — refresh proceeds",
			hook: func() bool { return true },
			want: false,
		},
		{
			name: "callback returns false (no active jobs) — skip refresh",
			hook: func() bool { return false },
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
			if tc.hook != nil {
				s.HasActiveJobs = tc.hook
			}
			if got := s.shouldSkipPeriodicRefresh(); got != tc.want {
				t.Errorf("shouldSkipPeriodicRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShouldSkipPeriodicRefreshCallbackInvocationCount makes sure each
// tick consults the callback exactly once — a test buffer against future
// "cache the result" optimisations that could mask a job appearing
// between the initial check and the headless-Chrome launch.
func TestShouldSkipPeriodicRefreshCallbackInvocationCount(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	var calls int
	s.HasActiveJobs = func() bool {
		calls++
		return calls > 1 // first call says "no active", second says "active"
	}

	if got := s.shouldSkipPeriodicRefresh(); !got {
		t.Errorf("first call: shouldSkipPeriodicRefresh = false, want true (no active)")
	}
	if got := s.shouldSkipPeriodicRefresh(); got {
		t.Errorf("second call: shouldSkipPeriodicRefresh = true, want false (active appeared)")
	}
	if calls != 2 {
		t.Errorf("HasActiveJobs invocation count = %d, want 2", calls)
	}
}
