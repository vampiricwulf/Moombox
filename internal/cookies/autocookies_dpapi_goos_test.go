package cookies

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// recordingDpapiLogger captures every line the extraction logs, by level.
type recordingDpapiLogger struct {
	debug []string
	info  []string
	warn  []string
}

func (l *recordingDpapiLogger) Debug(msg string, args ...any) { l.debug = append(l.debug, msg) }
func (l *recordingDpapiLogger) Info(msg string, args ...any)  { l.info = append(l.info, msg) }
func (l *recordingDpapiLogger) Warn(msg string, args ...any)  { l.warn = append(l.warn, msg) }

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
	out, err := dpapiExtractAsNetscape(logger, "")

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
