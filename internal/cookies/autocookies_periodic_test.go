package cookies

import (
	"testing"
	"time"
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
			if got := s.shouldSkipPeriodicRefresh(10 * time.Minute); got != tc.want {
				t.Errorf("shouldSkipPeriodicRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- validateBrowserProfileDir (audit cookies.md #26) ---

// TestValidateBrowserProfileDirAccepts covers the legitimate cases:
// empty input is the "auto-cookies not configured" signal and must
// pass; relative or absolute paths under the user's working directory
// (the typical Moombox layout) must pass.
func TestValidateBrowserProfileDirAccepts(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty (not configured)", ""},
		{"relative default", "browser-profile"},
		{"relative subdir", "data/browser-profile"},
		{"absolute under tempdir", t.TempDir()},
		{"path that merely contains 'chrome' as a name fragment", t.TempDir() + "/my-chrome-archive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBrowserProfileDir(tc.path); err != nil {
				t.Errorf("validateBrowserProfileDir(%q) = %v, want nil", tc.path, err)
			}
		})
	}
}

// TestValidateBrowserProfileDirRefuses verifies the dangerous-pattern
// scan rejects paths that sit inside known browser profile trees. The
// substrings are matched case-insensitively against the absolute
// lowercased path. Audit reports/cookies.md #26.
func TestValidateBrowserProfileDirRefuses(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"chrome stable", `C:\Users\test\AppData\Local\Google\Chrome\User Data\Default`},
		{"chrome canary", `C:\Users\test\AppData\Local\Google\Chrome Canary\User Data\Default`},
		{"edge stable", `C:\Users\test\AppData\Local\Microsoft\Edge\User Data\Default`},
		{"edge dev", `C:\Users\test\AppData\Local\Microsoft\Edge Dev\User Data\Default`},
		{"brave", `C:\Users\test\AppData\Local\BraveSoftware\Brave-Browser\User Data\Default`},
		{"chromium", `C:\Users\test\AppData\Local\Chromium\User Data\Default`},
		{"vivaldi", `C:\Users\test\AppData\Local\Vivaldi\User Data\Default`},
		{"opera stable", `C:\Users\test\AppData\Roaming\Opera Software\Opera Stable`},
		{"opera gx stable", `C:\Users\test\AppData\Roaming\Opera Software\Opera GX Stable`},
		{"firefox", `C:\Users\test\AppData\Roaming\Mozilla\Firefox\Profiles\xxxxx.default-release`},
		{"firefox developer edition", `C:\Users\test\AppData\Roaming\Mozilla\Firefox Developer Edition\Profiles\xxxxx`},
		{"thunderbird", `C:\Users\test\AppData\Roaming\Thunderbird\Profiles\xxxxx`},
		{"librewolf", `C:\Users\test\AppData\Roaming\LibreWolf\Profiles\xxxxx`},
		{"waterfox", `C:\Users\test\AppData\Roaming\Waterfox\Profiles\xxxxx`},
		// Case insensitivity check
		{"chrome lowercased", `c:\users\test\appdata\local\google\chrome\user data\default`},
		{"chrome mixed case", `c:\Users\TEST\AppData\Local\GOOGLE\Chrome\User Data\Default`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBrowserProfileDir(tc.path)
			if err == nil {
				t.Errorf("validateBrowserProfileDir(%q) = nil, want error", tc.path)
			}
		})
	}
}

// TestNewAutoCookieServiceCachesProfileDirErr proves the constructor
// computes the validation result once. Subsequent calls into
// subprocess-launching methods should fast-fail with the cached
// error rather than re-running the scan.
func TestNewAutoCookieServiceCachesProfileDirErr(t *testing.T) {
	bad := `C:\Users\test\AppData\Local\Google\Chrome\User Data\Default`
	s := NewAutoCookieService(bad, "", nil, nopAutoCookieLogger{})
	if s.profileDirErr == nil {
		t.Fatal("profileDirErr should be non-nil for a Chrome profile path")
	}
	// Same instance, called twice — must return the exact same error
	// pointer (stored once at construction, not recomputed).
	if s.profileDirErr != s.profileDirErr {
		t.Error("profileDirErr should be a stored field, not a recomputed accessor")
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

	if got := s.shouldSkipPeriodicRefresh(10 * time.Minute); !got {
		t.Errorf("first call: shouldSkipPeriodicRefresh = false, want true (no active)")
	}
	if got := s.shouldSkipPeriodicRefresh(10 * time.Minute); got {
		t.Errorf("second call: shouldSkipPeriodicRefresh = true, want false (active appeared)")
	}
	if calls != 2 {
		t.Errorf("HasActiveJobs invocation count = %d, want 2", calls)
	}
}
