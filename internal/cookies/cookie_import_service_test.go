package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// importService builds an AutoCookieService pointed at a real cookies.txt in a
// temp dir, with both verify callbacks answering `verified`. A real file
// because the whole point of this layer is the merge: a test against a stubbed
// writer is not testing that the sibling platform survived.
func importService(t *testing.T, seed string, verified bool) (*AutoCookieService, string, *CookieJar) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	s := NewAutoCookieService(dir, path, jar, nopAutoCookieLogger{})
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return verified, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return verified, nil }
	return s, path, jar
}

// TestImportCookiesMergesOntoDiskAndReloadsTheJar is the end-to-end claim of
// this layer, asserted on the FILE and on the live jar rather than on the
// returned struct.
//
// The mutations: writing `netscape` instead of the merged text (Twitch
// disappears from the file); dropping the jar.Load (the process keeps serving
// the dead credentials until the 30-minute ticker, which is exactly what
// "immediately apply the updated cookie" rules out).
func TestImportCookiesMergesOntoDiskAndReloadsTheJar(t *testing.T) {
	s, path, jar := importService(t, netscapeHeader+fakeTwitchRows, true)

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if !result.Wrote {
		t.Fatal("Wrote is false after a successful import — the caller's re-check is gated on it")
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back cookies.txt: %v", err)
	}
	if !strings.Contains(string(onDisk), "fake-authtoken-aaaa") {
		t.Error("the Twitch row is gone from cookies.txt — the import replaced instead of merging")
	}
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the pasted YouTube row never reached the file")
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got != "fake-sapisid-aaaa" {
		t.Errorf("the live jar's SAPISID = %q — the jar was not reloaded from the file just written", got)
	}
	if result.YouTube != RefreshOK || !result.YouTubeAccepted {
		t.Errorf("YouTube verdict = %v accepted = %v, want ok/true from a verify that answered true",
			result.YouTube, result.YouTubeAccepted)
	}
}

// TestImportCookiesAbortsOnAnUnreadableExistingFile is Arc 2's S9, at the fifth
// writer.
//
// The mutation: treating a non-ENOENT read error as "no existing file". The
// import then writes ONLY the pasted rows over a file that may hold the other
// platform's working credentials — reproduced end to end during Arc 2 — and the
// operator is told to replace the file that was never the problem.
func TestImportCookiesAbortsOnAnUnreadableExistingFile(t *testing.T) {
	s, path, _ := importService(t, netscapeHeader+fakeTwitchRows, true)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	blip := errors.New("simulated read failure")
	restore := readCookieFile
	readCookieFile = func(string) ([]byte, error) { return nil, blip }
	t.Cleanup(func() { readCookieFile = restore })

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Fatalf("error = %v, want it to wrap ErrCookieFileUnreadable", err)
	}
	if result.Wrote {
		t.Error("Wrote is true on the abort — the caller would run a re-check over a file nobody touched")
	}
	// The read-back gets its OWN error variable. `after, err := ...` would
	// assign to the `err` above rather than declare a new one — `after` is the
	// only new name on the left — so the import's error would be replaced by
	// the read-back's nil, and the last assertion in this test would dereference
	// it. That assertion is the one that catches an abort which stops wrapping
	// readErr, so losing it silently would cost the test its point.
	after, readBackErr := os.ReadFile(path)
	if readBackErr != nil {
		t.Fatalf("read back: %v", readBackErr)
	}
	if string(after) != string(before) {
		t.Error("cookies.txt changed on the read-abort path — the abort exists precisely to leave it alone")
	}
	if !strings.Contains(err.Error(), blip.Error()) {
		t.Errorf("the abort drops the underlying cause: %v", err)
	}
}

// TestImportCookiesNamesTheBindMountOnAFailedWrite. writeFileAtomic ends in a
// rename and a rename cannot replace a single-file bind mount, which is the one
// deployment mistake that produces this in the deployment this endpoint exists
// for.
//
// The mutations: returning the bare write error (the route's default arm then
// answers "cookie import failed", which names nothing the operator can act on);
// setting Wrote before the write succeeds.
func TestImportCookiesNamesTheBindMountOnAFailedWrite(t *testing.T) {
	s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)

	restore := writeCookieFile
	writeCookieFile = func(string, []byte, os.FileMode) error {
		return errors.New("rename temp cookie file: device or resource busy")
	}
	t.Cleanup(func() { writeCookieFile = restore })

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if !errors.Is(err, ErrCookieFileUnwritable) {
		t.Fatalf("error = %v, want it to wrap ErrCookieFileUnwritable", err)
	}
	if result.Wrote {
		t.Error("Wrote is true although the write failed")
	}
	if !strings.Contains(err.Error(), "Docker") {
		t.Errorf("the failed-write message does not name the bind-mount mistake: %v", err)
	}
}

// TestImportCookiesRecordsOnlyTheFailuresItEstablished is the lastError write
// policy at this writer (data-and-storage.md § Auto-Cookie Service).
//
// lastErrorSnapshot is this package's EXISTING test helper
// (autocookies_periodic_start_test.go:48) — do not add a second one. It reads
// the field under the lock its writers hold, which is exactly what
// AutoCookieStatus.LastError projects, and it avoids GetStatus's
// browser/registry detection scan (filesystem I/O and a reg.exe spawn on
// Windows) that the rest of this package neutralises with
// withFreshBrowserDetectCache + stubDetectors.
//
// A REJECTED PASTE is not a state of the install: nothing ran, nothing was
// written, and the caller gets the answer synchronously in the same dialog —
// the same reasoning that keeps FinishSetupDetailed's two guard clauses from
// setting. A read or write failure IS a state of the install, and Settings is
// where an operator looks afterwards.
//
// The mutations: adding a setError to the validation exit (every mistyped
// paste leaves a red line in Settings that no later success clears); removing
// it from the read-abort exit (the abort becomes invisible the moment the
// response is closed).
func TestImportCookiesRecordsOnlyTheFailuresItEstablished(t *testing.T) {
	t.Run("a rejected paste records nothing", func(t *testing.T) {
		s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)
		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeSignedOutRows); !errors.Is(err, ErrImportNoCredential) {
			t.Fatalf("error = %v, want ErrImportNoCredential", err)
		}
		if got := lastErrorSnapshot(s); got != "" {
			t.Errorf("lastError = %q after a rejected paste — a bad paste is not a state of the install", got)
		}
	})

	t.Run("a read abort records itself", func(t *testing.T) {
		s, _, _ := importService(t, netscapeHeader+fakeTwitchRows, true)
		restore := readCookieFile
		readCookieFile = func(string) ([]byte, error) { return nil, errors.New("simulated read failure") }
		t.Cleanup(func() { readCookieFile = restore })

		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err == nil {
			t.Fatal("expected the read abort")
		}
		got := lastErrorSnapshot(s)
		if !strings.Contains(got, ErrCookieFileUnreadable.Error()) {
			t.Errorf("lastError = %q, want the abort's own message", got)
		}
	})
}

// TestImportCookiesClearsTheReloginFlagForAnAcceptedPlatform.
//
// The flag means "go and sign in again"; the operator just did exactly that, by
// the one route a container has. Leaving it raised because the confirming
// request hit a rate limit would nag them about work already done — which is
// why this clears on ACCEPTED, not on verified, exactly as FinishSetupDetailed
// does. The needsRelogin map is written directly here because the exported
// setter arrives in Task 2; that task swaps this fixture for the setter and
// adds the raise side.
//
// The mutations: clearing on `result.YouTube == RefreshOK` (an operator whose
// verify was rate-limited keeps a red re-login badge over a good import);
// clearing both platforms whatever the paste carried (a YouTube-only paste
// silently retracts a live Twitch alarm).
func TestImportCookiesClearsTheReloginFlagForAnAcceptedPlatform(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, errors.New("rate limited") }
	s.mu.Lock()
	s.needsRelogin["youtube"] = true
	s.needsRelogin["twitch"] = true
	s.mu.Unlock()

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}

	relogin := s.ReloginStatus()
	if relogin["youtube"] {
		t.Error("the YouTube re-login flag is still raised after an accepted import — an inconclusive " +
			"check is not evidence against a sign-in the operator just supplied")
	}
	if !relogin["twitch"] {
		t.Error("the Twitch re-login flag was cleared by a YouTube-only paste — nothing in that paste " +
			"says anything about Twitch")
	}
}

// argRecordingLogger keeps the ARGS as well as the message, which is what a leak
// scan has to read. Neither existing recorder in this package does:
// recordingCookieLogger (autocookies_periodic_start_test.go:18) discards them in
// its signature, and capturingLogger keeps messages only. A third recorder,
// narrowly, rather than widening one of those and changing what its own tests
// compare.
//
// The anonymous four-method interface, repeated — never extracted.
type argRecordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *argRecordingLogger) record(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(args...))
}
func (l *argRecordingLogger) Debug(msg string, args ...any) { l.record(msg, args...) }
func (l *argRecordingLogger) Info(msg string, args ...any)  { l.record(msg, args...) }
func (l *argRecordingLogger) Warn(msg string, args ...any)  { l.record(msg, args...) }
func (l *argRecordingLogger) Error(msg string, args ...any) { l.record(msg, args...) }
func (l *argRecordingLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// importServiceWithLog is importService with a logger that keeps what it was
// handed, for the scans below. Verification always answers true; none of these
// subtests gets that far.
func importServiceWithLog(t *testing.T, seed string) (*AutoCookieService, *argRecordingLogger) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	log := &argRecordingLogger{}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	s := NewAutoCookieService(dir, path, jar, log)
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	return s, log
}

// TestImportFailurePathsCarryNoValue extends the security rule to the three
// answers the ROUTE's leak scan cannot reach from outside this package, because
// reaching them needs the read and write seams: the S9 abort, the failed write,
// and a write that landed over a jar that then could not read it back.
//
// What is asserted is narrow and exact. All three wrap an OS error, and an OS
// error carries a PATH — that is not a secret, and the abort's message is
// useless without it. The claim is that no cookie VALUE rides along with it: not
// in the returned error, not in lastError (which both dashboards render), and
// not in any log line.
//
// The mutation: any of the three exits reworded to interpolate the paste, the
// merged text, or a row of either — the shape a "helpful" diagnostic naming the
// offending row would take.
func TestImportFailurePathsCarryNoValue(t *testing.T) {
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa"}
	paste := netscapeHeader + fakeYouTubeRows

	scan := func(t *testing.T, err error, log *argRecordingLogger, s *AutoCookieService) {
		t.Helper()
		if err == nil {
			t.Fatal("expected this path to fail")
		}
		haystack := err.Error() + "\n" + lastErrorSnapshot(s) + "\n" + log.all()
		for _, secret := range secrets {
			if strings.Contains(haystack, secret) {
				t.Fatalf("a cookie value reached an error, lastError or a log line: %q", secret)
			}
		}
	}

	t.Run("the read abort", func(t *testing.T) {
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := readCookieFile
		readCookieFile = func(string) ([]byte, error) { return nil, errors.New("simulated read failure") }
		t.Cleanup(func() { readCookieFile = restore })

		_, err := s.ImportCookies(context.Background(), paste)
		scan(t, err, log, s)
	})

	t.Run("the failed write", func(t *testing.T) {
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := writeCookieFile
		writeCookieFile = func(string, []byte, os.FileMode) error {
			return errors.New("rename temp cookie file: device or resource busy")
		}
		t.Cleanup(func() { writeCookieFile = restore })

		_, err := s.ImportCookies(context.Background(), paste)
		scan(t, err, log, s)
	})

	t.Run("the write landed and the jar could not read it back", func(t *testing.T) {
		// A DIRECTORY where the file belongs: the stubbed write reports success
		// and jar.Load then fails on that same path on both platforms (EISDIR on
		// Linux, a read error on Windows). It is the one error exit with
		// Wrote == true, and the one the route answers with its own fixed
		// sentence.
		s, log := importServiceWithLog(t, netscapeHeader+fakeTwitchRows)
		restore := writeCookieFile
		writeCookieFile = func(path string, _ []byte, _ os.FileMode) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return os.Mkdir(path, 0o755)
		}
		t.Cleanup(func() { writeCookieFile = restore })

		result, err := s.ImportCookies(context.Background(), paste)
		if !result.Wrote {
			t.Fatalf("Wrote = false on the reload failure — the route's re-check is gated on it (err = %v)", err)
		}
		scan(t, err, log, s)
	})
}
