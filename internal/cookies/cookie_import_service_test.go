package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa",
		// The rollback fixtures' two generations, for the last two subtests.
		// Both generations, deliberately: an exit that named the credential it
		// REJECTED would leak just as surely as one that named the credential
		// it kept.
		previousTwitchToken, pastedTwitchToken, previousSAPISID, pastedSAPISID}
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

	// The two ROLLBACK-INCOMPLETE exits. They are the newest strings this
	// endpoint composes and the most tempting place for a value to ride along:
	// each is about one specific credential that was rejected and another that
	// was being put back, and a diagnostic naming "the row we could not
	// restore" would be the obvious thing to reach for. Both of them go to
	// lastError AND to the dialog, so a leak here is rendered twice.
	//
	// Driven through the two fixtures the rollback tests already build — the
	// same seams, so these subtests test the strings rather than re-testing the
	// mechanism.
	rollbackWrites := func(t *testing.T, second func(path string, data []byte, perm os.FileMode) error) *AutoCookieService {
		t.Helper()
		s, _ := importServiceWithLog(t, importRollbackSeed)
		s.VerifyTwitchAuth = twitchLiveFromJar(s)
		real := writeCookieFile
		writes := 0
		writeCookieFile = func(path string, data []byte, perm os.FileMode) error {
			writes++
			if writes == 1 {
				return real(path, data, perm)
			}
			return second(path, data, perm)
		}
		t.Cleanup(func() { writeCookieFile = real })
		return s
	}

	t.Run("the rollback's write failed", func(t *testing.T) {
		s := rollbackWrites(t, func(string, []byte, os.FileMode) error {
			return errors.New("rename temp cookie file: device or resource busy")
		})
		log, ok := s.logger.(*argRecordingLogger)
		if !ok {
			t.Fatalf("the fixture's logger is %T, not the recorder this scan reads", s.logger)
		}
		_, err := s.ImportCookies(context.Background(), importRollbackPaste)
		scan(t, err, log, s)
	})

	t.Run("the rollback landed and the jar could not read it back", func(t *testing.T) {
		s := rollbackWrites(t, func(path string, _ []byte, _ os.FileMode) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return os.Mkdir(path, 0o755)
		})
		log, ok := s.logger.(*argRecordingLogger)
		if !ok {
			t.Fatalf("the fixture's logger is %T, not the recorder this scan reads", s.logger)
		}
		_, err := s.ImportCookies(context.Background(), importRollbackPaste)
		scan(t, err, log, s)
	})
}

// TestImportClearsAReloginFlagRaisedByTheSetter closes the loop R6 describes:
// the prompt path raises it, a successful import clears it. Task 1 asserted the
// clear against a direct write to the map because the setter did not exist yet;
// this drives the real pair.
//
// The mutation: FlagManualRelogin writing a platform key the clear does not
// read (or vice versa) — the prompt would then be unclearable by the one
// gesture that answers it.
func TestImportClearsAReloginFlagRaisedByTheSetter(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.FlagManualRelogin("youtube")
	if !s.ReloginStatus()["youtube"] {
		t.Fatal("FlagManualRelogin did not raise the YouTube flag")
	}

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if s.ReloginStatus()["youtube"] {
		t.Error("a successful import left the re-login prompt raised — the operator did the thing " +
			"the prompt asked for and is still being nagged about it")
	}
}

// TestImportRejectionLeavesAStandingReloginFlagAlone closes the gap the Task 2
// review found (Finding 1): cookie_import.go's clear runs only past the write,
// gated on YouTubeAccepted/TwitchAccepted — a REJECTED paste (the 422 path:
// not Netscape, no rows, or no credential) returns from prepareCookieImport's
// error before that code ever runs, so nothing raised by FlagManualRelogin
// should move. That claim had no test of its own.
//
// The mutation this closes: an unconditional needsRelogin clear moved ahead of
// the Accepted gate — for instance to the top of ImportCookies, before the
// paste is even validated — which would also fire on this rejected paste and
// silently retract a prompt the operator has not actually answered.
func TestImportRejectionLeavesAStandingReloginFlagAlone(t *testing.T) {
	s, path, _ := importService(t, netscapeHeader+fakeTwitchRows, true)
	s.FlagManualRelogin("youtube")
	if !s.ReloginStatus()["youtube"] {
		t.Fatal("FlagManualRelogin did not raise the YouTube flag")
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeSignedOutRows); !errors.Is(err, ErrImportNoCredential) {
		t.Fatalf("error = %v, want ErrImportNoCredential", err)
	}

	if !s.ReloginStatus()["youtube"] {
		t.Error("a rejected import cleared the YouTube re-login flag — nothing was written, so nothing " +
			"the operator did resolves it")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(before) {
		t.Error("cookies.txt changed on a rejected import — the refusal happens before any write")
	}
}

// TestFlagManualReloginTouchesOnlyTheNamedPlatform. The map is written under
// s.mu by three other paths and read by both UIs; a setter that wrote both keys
// would raise an alarm about a platform nothing concluded anything about, and
// one that wrote an arbitrary key would teach the frontend a platform it cannot
// render.
//
// The mutation: replacing the switch with `s.needsRelogin[platform] = true`.
func TestFlagManualReloginTouchesOnlyTheNamedPlatform(t *testing.T) {
	s, _, _ := importService(t, "", true)
	s.FlagManualRelogin("twitch")

	relogin := s.ReloginStatus()
	if !relogin["twitch"] {
		t.Error("the Twitch flag was not raised")
	}
	if relogin["youtube"] {
		t.Error("flagging Twitch raised YouTube too")
	}

	s.FlagManualRelogin("mastodon")
	if len(s.ReloginStatus()) != 2 {
		t.Errorf("an unrecognised platform was added to the map: %v — the wire shape is two keys and "+
			"the frontend iterates it", s.ReloginStatus())
	}
}

// TestImportCookiesIsRefusedWhileARefreshHoldsTheSlot is COOKIES-1 / owner
// decision O-D. ImportCookies used to take no slot at all, so a paste landing
// inside a browser-refresh or recovery pass's read -> verify (<=12 s) -> write
// gap was overwritten by that pass's merge of the PRE-paste file — and the
// import reported Wrote=true with both platforms ok while it happened. On the
// recovery path the rows that replace the paste are the dead ones that raised
// the alarm.
//
// A 409 the operator retries within ~2 minutes is the trade O-D chose over
// re-reading and re-merging in every pass. The no-lock property is kept: this
// is the SAME sentinel StartSetup and RefreshCookiesDetailed already gate on,
// not a new mutex over cookies.txt.
//
// Mutants:
//   - drop the refreshCmd check -> err is nil, Wrote is true, and the file on
//     disk carries the paste that the pass is about to overwrite.
//   - claim the slot but never release it -> the second half fails: every
//     later import (and every refresh, and StartSetup) is refused forever.
func TestImportCookiesIsRefusedWhileARefreshHoldsTheSlot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	s := NewAutoCookieService(dir, path, NewCookieJar(), nopAutoCookieLogger{})

	// The slot as RefreshCookiesDetailed claims it: a sentinel with no process.
	s.mu.Lock()
	s.refreshCmd = &exec.Cmd{}
	s.mu.Unlock()

	res, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if !errors.Is(err, ErrRefreshInProgress) {
		t.Fatalf("err = %v, want ErrRefreshInProgress — an import that lands inside a pass is silently destroyed by it", err)
	}
	if res.Wrote {
		t.Error("Wrote = true on a refused import — nothing may be written while a pass holds the slot")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cookies.txt exists after a refused import (stat err = %v)", statErr)
	}

	// Releasing the slot lets the retry through, and the import releases the
	// slot it claims.
	s.mu.Lock()
	s.refreshCmd = nil
	s.mu.Unlock()

	res, err = s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if err != nil || !res.Wrote {
		t.Fatalf("the retry after the pass finished must succeed: res=%+v err=%v", res, err)
	}
	s.mu.Lock()
	held := s.refreshCmd
	s.mu.Unlock()
	if held != nil {
		t.Error("ImportCookies did not release the refresh slot — every later refresh, setup and import would be refused for the life of the process")
	}
}

// TestADeclinedRefreshNamesTheHolderAtInfo is the close review's M-CW1(b), the
// operator-visible half of owner decision O-D.
//
// O-D made the import and the refresh pass mutually exclusive over ONE slot, so
// a paste now costs the 30-minute tick that collides with it: the pass declines
// silently, and the operator who pasted a fresh cookies.txt by hand sees a
// dashboard that does not refresh for up to half an hour with nothing in the
// log saying why. The decline is correct; being silent about it is not.
//
// Info, not Debug: the operator running at the default level is exactly the
// person who needs it, and "the refresh you are waiting for did not run, and
// here is what held it" is not a line to go looking for. The holder is named
// because the two holders have different stories — a pass declining behind
// another pass is the pre-existing single-flight and costs nothing, while a
// pass declining behind an IMPORT is the new trade O-D bought.
//
// The holder is identified by the sentinel VALUE, not a new field:
// importSlotSentinel is a package-level *exec.Cmd ImportCookies assigns in
// place of a fresh literal, and killRefreshProcess only ever reads .Process,
// which is nil for either one.
//
// Driven through the REAL slot: the import is held inside its own pre-write
// verification while the refresh runs, exactly as driveImportDuringRefresh does
// it on the route side.
//
// Mutants:
//   - keep the decline at Debug -> no Info line is captured.
//   - name "refresh pass" unconditionally -> the holder assertion fails.
//   - claim a fresh &exec.Cmd{} in ImportCookies instead of the sentinel ->
//     the holder reads "refresh pass" for an import, same failure.
func TestADeclinedRefreshNamesTheHolderAtInfo(t *testing.T) {
	const declineMsg = "skipping cookie refresh — the refresh slot is held"

	t.Run("an import holds it", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "cookies.txt")
		if err := os.WriteFile(path, []byte(importRollbackSeed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
		log := &captureLogger{}
		jar := NewCookieJar()
		if err := jar.Load(path); err != nil {
			t.Fatalf("jar.Load: %v", err)
		}
		s := NewAutoCookieService(dir, path, jar, log)

		// The import parks inside snapshotPlatformAuth's YouTube check, which
		// runs only because the seed gives the jar a YouTube credential to
		// check. Twitch answers immediately so the WaitGroup is not what holds.
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		s.VerifyYouTubeAuth = func(ctx context.Context) (bool, error) {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return true, nil
		}
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

		done := make(chan struct{})
		go func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("the holding import panicked: %v", p)
				}
				close(done)
			}()
			if _, err := s.ImportCookies(context.Background(), importRollbackPaste); err != nil {
				t.Errorf("the holding import failed: %v", err)
			}
		}()

		select {
		case <-entered:
		case <-done:
			t.Fatal("the import finished before it reached its pre-write verification — the " +
				"collision this test needs never happened")
		case <-time.After(60 * time.Second):
			t.Fatal("the import never reached its pre-write verification")
		}

		out, err := s.RefreshCookiesDetailed(context.Background())
		close(release)
		<-done

		if err != nil {
			t.Fatalf("a declined pass is not an error: %v", err)
		}
		if out.Ran {
			t.Error("the pass RAN while an import held the slot — O-D makes the two mutually exclusive")
		}
		if n := log.infoContaining(declineMsg); n != 1 {
			t.Errorf("the decline produced %d Info lines containing %q, want exactly 1 — an operator "+
				"who just pasted a cookies.txt by hand watches the badge stay stale for up to 30 "+
				"minutes and the log says nothing", n, declineMsg)
		}
		if n := log.debugContaining(declineMsg); n != 0 {
			t.Errorf("the decline also landed at Debug (%d lines). The LEVEL is the point: a line an "+
				"operator has to raise the log level to find is a line they do not have", n)
		}
		if n := log.infoContaining("holder=cookie import"); n != 1 {
			t.Errorf("the decline does not name the import as the holder (%d lines) — a pass declining "+
				"behind another pass costs nothing and is the pre-existing single-flight; a pass "+
				"declining behind an import is the trade O-D bought, and only the holder tells them "+
				"apart", n)
		}
	})

	t.Run("another pass holds it", func(t *testing.T) {
		dir := t.TempDir()
		log := &captureLogger{}
		s := NewAutoCookieService(dir, filepath.Join(dir, "cookies.txt"), NewCookieJar(), log)

		// The slot as RefreshCookiesDetailed claims it: a fresh sentinel, not
		// the import's package-level one.
		s.mu.Lock()
		s.refreshCmd = &exec.Cmd{}
		s.mu.Unlock()

		if _, err := s.RefreshCookiesDetailed(context.Background()); err != nil {
			t.Fatalf("a declined pass is not an error: %v", err)
		}
		if n := log.infoContaining("holder=refresh pass"); n != 1 {
			t.Errorf("a pass declining behind another pass named the holder %d times, want 1", n)
		}
		if n := log.infoContaining("holder=cookie import"); n != 0 {
			t.Errorf("a pass declining behind another PASS blamed an import (%d lines)", n)
		}
	})
}
