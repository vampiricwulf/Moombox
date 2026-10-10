package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// recordingDpapiLogger captures every line the extraction logs, by level.
//
// It carries an Error method (never asserted on below) purely so it also
// satisfies NewAutoCookieService's logger parameter, which
// TestDpapiFallbackWarnIsGatedOnWindows needs below — the narrower inline
// logger interface dpapiExtractAsNetscape takes doesn't require it, but a
// struct can satisfy both.
type recordingDpapiLogger struct {
	debug []string
	info  []string
	warn  []string
	error []string
}

func (l *recordingDpapiLogger) Debug(msg string, args ...any) { l.debug = append(l.debug, msg) }
func (l *recordingDpapiLogger) Info(msg string, args ...any)  { l.info = append(l.info, msg) }
func (l *recordingDpapiLogger) Warn(msg string, args ...any)  { l.warn = append(l.warn, msg) }
func (l *recordingDpapiLogger) Error(msg string, args ...any) { l.error = append(l.error, msg) }

// TestDpapiExtractIsWindowsOnly: off Windows the fallback does not exist, and
// saying so once at Debug is the whole report. The old path fell through to "no
// Chromium-family profiles found under LOCALAPPDATA" — a Windows-shaped
// sentence the caller then logged at Warn, which reads as a broken install on a
// host where the setting is simply inapplicable.
//
// The profile seam deliberately returns a NON-empty slice: the short-circuit
// has to fire before the scan, and on the Windows dev machine the real scan
// would otherwise find genuine profiles.
//
// Mutant: deleting the runtimeGOOS guard makes the error the LOCALAPPDATA
// sentence (or a real extraction attempt) and fails this.
func TestDpapiExtractIsWindowsOnly(t *testing.T) {
	realGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = realGOOS })
	runtimeGOOS = func() string { return "linux" }

	realFind := dpapiFindBrowserProfiles
	t.Cleanup(func() { dpapiFindBrowserProfiles = realFind })
	dpapiFindBrowserProfiles = func() []dpapi.BrowserProfile {
		return []dpapi.BrowserProfile{{Browser: "chrome", Name: "Default", Path: "/nope"}}
	}

	logger := &recordingDpapiLogger{}
	out, err := dpapiExtractAsNetscape(logger, "", "")

	if out != "" {
		t.Errorf("no cookies can come out on a non-Windows host, got %q", out)
	}
	if !errors.Is(err, dpapi.ErrNotSupported) {
		t.Errorf("err = %v, want dpapi.ErrNotSupported", err)
	}
	if len(logger.debug) != 1 || !strings.Contains(logger.debug[0], "DPAPI fallback is Windows-only") {
		t.Errorf("want exactly one Debug line naming the platform, got %v", logger.debug)
	}
	if len(logger.warn) != 0 {
		t.Errorf("an inapplicable setting is not a failure to warn about, got %v", logger.warn)
	}
}

// TestDpapiFallbackWarnIsGatedOnWindows drives the actual refresh caller
// (RefreshCookies, not dpapiExtractAsNetscape directly) through a failed CDP
// launch with cookies.dpapi_fallback on, and checks the "CDP refresh failed;
// attempting DPAPI fallback" Warn autocookies_refresh.go logs BEFORE ever
// calling dpapiExtractAsNetscape.
//
// On Linux that Warn used to fire unconditionally, once per failed refresh,
// even though the very next line (dpapiExtractAsNetscape itself) is a
// guaranteed no-op there post T4-34 — "attempting" was simply untrue. It is
// now gated on isWindows(); the call to dpapiExtractAsNetscape stays
// unconditional either way, so the Debug explanation still fires off
// Windows.
//
// Mutant: reverting the gate to a bare s.logger.Warn(...) call makes the
// linux case see the "attempting DPAPI fallback" line and fails it.
func TestDpapiFallbackWarnIsGatedOnWindows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		goos     string
		wantWarn bool
	}{
		{name: "windows: a real attempt, worth a Warn", goos: "windows", wantWarn: true},
		{name: "linux: the fallback does not exist, no Warn", goos: "linux", wantWarn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realGOOS := runtimeGOOS
			t.Cleanup(func() { runtimeGOOS = realGOOS })
			runtimeGOOS = func() string { return tc.goos }

			// dpapiFindBrowserProfiles is stubbed regardless of goos: on the
			// "windows" row this keeps the test off the real filesystem (the
			// dev machine here IS Windows) — it only needs SOME return value
			// past the short-circuit, never a genuine scan. On the "linux"
			// row the short-circuit fires first and this is never called.
			realFind := dpapiFindBrowserProfiles
			t.Cleanup(func() { dpapiFindBrowserProfiles = realFind })
			dpapiFindBrowserProfiles = func() []dpapi.BrowserProfile { return nil }

			stubChromiumRefresh(t, "", true, errors.New("stub CDP launch failed"))

			cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
			if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
				t.Fatal(err)
			}
			logger := &recordingDpapiLogger{}
			s := NewAutoCookieService(t.TempDir(), cookiePath, NewCookieJar(), logger)
			s.detectBrowser = chromiumBrowser
			if err := s.jar.Load(cookiePath); err != nil {
				t.Fatal(err)
			}
			s.DpapiFallback = func() bool { return true }
			s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
			s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

			// Both the CDP launch AND the DPAPI fallback fail on every row here
			// (dpapiFindBrowserProfiles returns nil, and the fabricated
			// profile-less "windows" case is exactly as much of a failure as
			// the "linux" no-op is) — so RefreshCookies surfacing the
			// original CDP error is the correct, expected outcome, not a
			// test failure. Only the log lines are under test.
			_, _ = s.RefreshCookies(context.Background())

			gotWarn := dpapiAnyContains(logger.warn, "attempting DPAPI fallback")
			if gotWarn != tc.wantWarn {
				t.Errorf("Warn contains %q = %v, want %v; warns=%v",
					"attempting DPAPI fallback", gotWarn, tc.wantWarn, logger.warn)
			}
			// The call to dpapiExtractAsNetscape is unconditional, so off
			// Windows its Debug explanation must still fire even though the
			// caller's own Warn is suppressed.
			if tc.goos != "windows" {
				if !dpapiAnyContains(logger.debug, "DPAPI fallback is Windows-only") {
					t.Errorf("want the Debug explanation even with the Warn suppressed, got debug=%v", logger.debug)
				}
			}
		})
	}
}
