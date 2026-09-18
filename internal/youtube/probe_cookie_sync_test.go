package youtube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestProbeVideoStatusAuthenticatedSyncsTheCookieJar covers the path owner
// decision O-H made hot: the 30 s quality monitor now reaches YouTube through
// this probe instead of through GetVideoInfo, which has always re-read the
// cookie file first (service.go, `Auth.SyncCookies`). Without the same sync
// here, a cookie file rotated on disk — by the refresh service, by a Web/TUI
// import, by the user replacing it — would not reach the probe at all: the
// probe would keep sending dead credentials until something else on the job
// happened to run the cascade.
//
// The context is cancelled before the call, so the player request itself never
// leaves the process: the sync happens before it, which is exactly the
// ordering under test, and the test stays offline.
//
// Mutants this kills:
//   - the SyncCookies call removed from ProbeVideoStatusAuthenticated → the
//     jar still holds the pre-rotation value
//   - the sync moved after the player call → same, since the call is cancelled
func TestProbeVideoStatusAuthenticatedSyncsTheCookieJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const before = "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tstale-sapisid\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tstale-login-info\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	svc := NewService(jar, noopLogger{})
	if !strings.Contains(svc.GetCookieHeader(), "stale-sapisid") {
		t.Fatalf("cookie header does not carry the initial value: %q", svc.GetCookieHeader())
	}

	// Rotate the file on disk, the way the refresh service does. The value is
	// a different length so no (size, mtime) memo can mistake it for the old
	// file.
	const after = "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\trotated-sapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\trotated-login-info-value\n"
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("rotate cookie file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The error is the cancelled request; only the sync that preceded it is
	// under test.
	_, _ = svc.ProbeVideoStatusAuthenticated(ctx, "dQw4w9WgXcQ")

	if got := svc.GetCookieHeader(); !strings.Contains(got, "rotated-sapisid-value") {
		t.Errorf("cookie header after the probe = %q; want the rotated value — "+
			"the authenticated probe did not re-read the cookie file", got)
	}
}
