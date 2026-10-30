package cookies

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stopStillRunning is the line Stop logs when its wait runs out.
const stopStillRunning = "auto-cookie refresh pass still running after stop"

// passInFlight is a browserless service whose first pass is an import that
// stops in its post-write verify: entered is closed when it gets there. The
// verify honours its context the way the real check's HTTP request does, then
// sleeps through a tail — standing in for the rollback decision and SaveMeta
// that a cancelled pass still runs — long enough that a Stop which did not
// wait has returned well before it ends. The pass then answers "verified", so
// it writes the meta file: present when Stop returns means the pass had
// finished by then.
type passInFlight struct {
	s          *AutoCookieService
	log        *recordingCookieLogger
	cookiePath string
	entered    chan struct{}
	sawCancel  atomic.Bool
	hooked     atomic.Int64
}

func newPassInFlight(t *testing.T) *passInFlight {
	t.Helper()
	// killRefreshProcess polls the slot an import pass holds for this long,
	// and at its production 2 s that poll alone waits out most passes — it
	// would hide a Stop that does not wait. Registered first, so it is
	// restored after every join below.
	withLaunchWindowKillBudget(t, testLaunchWindowKillBudget)
	p := &passInFlight{
		log:        &recordingCookieLogger{},
		cookiePath: filepath.Join(t.TempDir(), "cookies.txt"),
		entered:    make(chan struct{}),
	}
	p.s = NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), p.cookiePath, NewCookieJar(), p.log)
	p.s.detectBrowser = func() *DetectedBrowser { return nil }
	var once sync.Once
	p.s.VerifyYouTubeAuth = func(ctx context.Context) (bool, error) {
		once.Do(func() { close(p.entered) })
		<-ctx.Done()
		p.sawCancel.Store(true)
		time.Sleep(200 * time.Millisecond)
		return true, nil
	}
	p.s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	p.s.OnPassCompleted = func(context.Context) { p.hooked.Add(1) }
	return p
}

// callerContext is a context the CALLER never cancels before Stop, so what
// releases the pass has to be Stop's own. The cleanup cancels it and then
// joins, so a mutant that leaves the pass running still cannot outlive the
// test's directories.
func (p *passInFlight) callerContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if !p.s.waitBackground(30 * time.Second) {
			t.Error("the pass was still running 30 s after its context ended")
		}
	})
	return ctx
}

func (p *passInFlight) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("no pass reached its verify (LastError=%q)", lastErrorSnapshot(p.s))
	}
}

// assertJoined is what must be true the moment Stop returns.
func (p *passInFlight) assertJoined(t *testing.T) {
	t.Helper()
	if !p.sawCancel.Load() {
		t.Error("Stop returned and the pass in flight never saw its context end — Stop did not cancel it, " +
			"so it waited on a verify round trip nothing was going to cut short")
	}
	if _, err := os.Stat(MetaPath(p.cookiePath)); err != nil {
		t.Errorf("Stop returned before the pass in flight had written its meta file (%v) — the caller "+
			"went on as if the service had stopped while the pass was still writing", err)
	}
	if n := p.log.count(stopStillRunning); n != 0 {
		t.Errorf("Stop gave up waiting %d time(s) on a pass that was cancelled and finishing", n)
	}
	if n := p.hooked.Load(); n != 0 {
		t.Errorf("OnPassCompleted fired %d time(s) for a pass that outlived its loop — a re-check under "+
			"a context that has already ended can only fail its round trips and publish that as the status", n)
	}
}

// TestStopWaitsForThePeriodicPassInFlight: Stop cancels the timer's pass in
// flight and waits for it to return, so nothing it starts writes after the
// caller has gone on. Shutdown stops the refresh service and then this one,
// and goes on to the PO token provider, the web server and the database;
// before the join the pass could go on writing cookies.txt and its meta file
// after all of that, and fire OnPassCompleted into the stopped services.
//
// Mutants (checked): Stop without its waitBackground — the meta file is not
// there yet; Stop without endLifetime, and startBackground without the
// AfterFunc that narrows ctx to the lifetime — the verify never sees a cancel
// and Stop gives up after AutoCookieStopWait; the loop goroutine without
// `defer done()` — Stop gives up; notePassCompleted without its ctx test —
// the hook fires.
func TestStopWaitsForThePeriodicPassInFlight(t *testing.T) {
	p := newPassInFlight(t)
	p.s.StartPeriodicRefresh(p.callerContext(t), 20*time.Millisecond)
	p.waitEntered(t)

	p.s.Stop()

	p.assertJoined(t)
}

// TestStopWaitsForTheStartupSeed: the boot seed's goroutine is joined the same
// way, asleep in its delay or mid-import.
//
// Mutants (checked): StartProfileSeed not registering through startBackground
// (its goroutine under the caller's ctx) — mid-import, the meta file is not
// there when Stop returns; the seed's Sleep under the caller's ctx — asleep,
// Stop gives up after AutoCookieStopWait; the seed goroutine without
// `defer done()` — Stop gives up in both.
func TestStopWaitsForTheStartupSeed(t *testing.T) {
	swapSeedWait := func(t *testing.T, d time.Duration) {
		t.Helper()
		prev := profileSeedWait
		profileSeedWait = d
		t.Cleanup(func() { profileSeedWait = prev })
	}

	t.Run("mid-import", func(t *testing.T) {
		swapSeedWait(t, time.Millisecond)
		p := newPassInFlight(t)
		p.s.StartProfileSeed(p.callerContext(t))
		p.waitEntered(t)

		p.s.Stop()

		p.assertJoined(t)
	})

	t.Run("asleep in its delay", func(t *testing.T) {
		swapSeedWait(t, time.Hour)
		p := newPassInFlight(t)
		p.s.StartProfileSeed(p.callerContext(t))
		if p.log.count("browser-free import path and no cookies to lose — seeding cookies from the configured browser profile") != 1 {
			t.Fatal("the seed did not start, so nothing below measures its join")
		}

		p.s.Stop()

		if n := p.log.count(stopStillRunning); n != 0 {
			t.Errorf("Stop gave up waiting for a seed that was only asleep — its delay is not under the "+
				"service's lifetime (%d)", n)
		}
		select {
		case <-p.entered:
			t.Error("the seed imported after Stop")
		default:
		}
	})
}

// TestStopJoinsThePostPassReCheck: the re-check a completed pass fires is part
// of that pass, so Stop's join covers it. cmd/moombox wires OnPassCompleted to
// RefreshService.CheckNow under the ctx the hook is handed; this drives a real
// one over the service's own jar, behind a guide that answers only after a
// slow network's delay — longer than AutoCookieStopWait — and rotates
// __Secure-1PSIDTS when it does. The re-check is on the wire when the stop
// begins, in both of the ways it can begin: the shutdown's order (the run's
// context cancelled, then RefreshService.Stop, which does not reach a CheckNow
// in flight, then this service's Stop), and Stop on its own.
//
// Before, the wiring re-checked under context.Background(): neither cancel
// reached the guide request, Stop gave up after its 2 s, and the re-check
// landed afterwards — rewriting cookies.txt and firing its callbacks after the
// shutdown had moved on to the web server and the database.
//
// Mutant (checked): notePassCompleted handing the hook context.Background() —
// in both subtests Stop gives up, the hook returns after Stop did, and the
// rotation reaches cookies.txt.
func TestStopJoinsThePostPassReCheck(t *testing.T) {
	const rotated = "rotated-after-stop"
	for _, tc := range []struct {
		name     string
		shutdown bool
	}{
		{"the shutdown's order", true},
		{"Stop alone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withLaunchWindowKillBudget(t, testLaunchWindowKillBudget)
			log := &recordingCookieLogger{}
			cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
			jar := NewCookieJar()
			s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar, log)
			s.detectBrowser = func() *DetectedBrowser { return nil }
			s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
			s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
			rs := NewRefreshService(jar, 0, nopLogger{})

			// Only the re-check reaches the guide: the pass's own verify is
			// stubbed above.
			onTheWire, answered := make(chan struct{}), make(chan struct{})
			var wireOnce, answerOnce sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer answerOnce.Do(func() { close(answered) })
				// Read first: the server watches for the client going away
				// only once the request body is consumed.
				_, _ = io.Copy(io.Discard, r.Body)
				wireOnce.Do(func() { close(onTheWire) })
				select {
				case <-r.Context().Done():
				case <-time.After(AutoCookieStopWait + 2*time.Second):
				}
				w.Header().Add("Set-Cookie", "__Secure-1PSIDTS="+rotated+"; Domain=.youtube.com; Path=/; Max-Age=31536000")
				_, _ = w.Write([]byte(loggedInGuideBody))
			}))
			t.Cleanup(srv.Close)
			pointYouTubeGuideAt(t, srv)
			pointTwitchValidateAt(t, statusServer(t, http.StatusOK))

			var hookReturned atomic.Int64
			s.OnPassCompleted = func(ctx context.Context) {
				rs.CheckNow(ctx)
				hookReturned.Store(time.Now().UnixNano())
			}

			// Registered last, so it runs ahead of the server's Close, the
			// guide URL's restore and the TempDir's removal.
			runCtx, cancelRun := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancelRun()
				if !s.waitBackground(30 * time.Second) {
					t.Error("the loop was still running 30 s after its context ended")
				}
			})
			s.StartPeriodicRefresh(runCtx, 20*time.Millisecond)
			select {
			case <-onTheWire:
			case <-time.After(20 * time.Second):
				t.Fatalf("the post-pass re-check never reached the guide (LastError=%q)", lastErrorSnapshot(s))
			}

			if tc.shutdown {
				cancelRun()
				rs.Stop()
			}
			s.Stop()
			stoppedAt := time.Now()

			if n := log.count(stopStillRunning); n != 0 {
				t.Errorf("Stop gave up %d time(s) on a pass whose re-check was on the wire — the cancel "+
					"did not reach the re-check's round trip", n)
			}
			if at := hookReturned.Load(); at == 0 || at > stoppedAt.UnixNano() {
				t.Error("Stop returned with the post-pass re-check still running — the shutdown goes on to " +
					"the web server and the database while it writes cookies.txt and fires its callbacks")
			}
			select {
			case <-answered:
			case <-time.After(AutoCookieStopWait + 12*time.Second):
				t.Fatal("the guide never finished answering")
			}
			if !s.waitBackground(10 * time.Second) {
				t.Fatal("the loop was still running 10 s after the guide had answered")
			}
			if b, err := os.ReadFile(cookiePath); err != nil {
				t.Fatalf("read cookies.txt: %v", err)
			} else if strings.Contains(string(b), rotated) {
				t.Error("the re-check's guide reply was written into cookies.txt after Stop returned")
			}
		})
	}
}

// TestStopIsBoundedByAutoCookieStopWait: a pass that does not observe the
// cancel cannot hold Stop — and so the shutdown — past AutoCookieStopWait, and
// Stop says that it gave up.
//
// Mutants (checked): waitBackground with no time.After arm — Stop does not
// return until the pass does; the Warn dropped — no line says so.
func TestStopIsBoundedByAutoCookieStopWait(t *testing.T) {
	withLaunchWindowKillBudget(t, testLaunchWindowKillBudget)
	// The bound, not its length, is what is under test: shortened so the
	// test does not wait the production 2 s out.
	prevWait := autoCookieStopWait
	autoCookieStopWait = 100 * time.Millisecond
	t.Cleanup(func() { autoCookieStopWait = prevWait })
	log := &recordingCookieLogger{}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()),
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
	s.detectBrowser = func() *DetectedBrowser { return nil }
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		once.Do(func() { close(entered) })
		<-release // ignores its context
		return true, nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		letGo()
		if !s.waitBackground(30 * time.Second) {
			t.Error("the pass did not return once released")
		}
	})

	s.StartPeriodicRefresh(t.Context(), 20*time.Millisecond)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass reached its verify")
	}

	stopped := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Stop panicked: %v", r)
			}
		}()
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(AutoCookieStopWait + 8*time.Second):
		letGo()
		<-stopped
		t.Fatalf("Stop had not returned %s after a pass that ignores its cancel began — the wait is "+
			"unbounded, and the shutdown with it", AutoCookieStopWait+8*time.Second)
	}
	if n := log.count(stopStillRunning); n != 1 {
		t.Errorf("Stop logged %q %d times after giving up on the pass, want 1", stopStillRunning, n)
	}
}

// TestStartAfterStopStartsNothing: once Stop has latched, neither entry point
// starts a goroutine. One that did would Add to the WaitGroup a Stop may be
// waiting on — the misuse sync.WaitGroup forbids — and announce a timer, or a
// seed, that the ended lifetime stops the moment it starts.
//
// Mutant (checked): startBackground without its stopped test — both entry
// points start, and neither says it did not.
func TestStartAfterStopStartsNothing(t *testing.T) {
	log := &recordingCookieLogger{}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()),
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.Stop()
	t.Cleanup(func() { joinBackground(t, s) })

	s.StartPeriodicRefresh(t.Context(), 20*time.Millisecond)
	s.StartProfileSeed(t.Context())

	if !s.waitBackground(200 * time.Millisecond) {
		t.Error("a goroutine is running on a service that was already stopped")
	}
	for msg, want := range map[string]int{
		"auto-cookie periodic refresh not started — the service is stopped":                                     1,
		"startup browser-profile cookie import not started — the service is stopped":                            1,
		"auto-cookie periodic refresh enabled":                                                                  0,
		"browser-free import path and no cookies to lose — seeding cookies from the configured browser profile": 0,
	} {
		if n := log.count(msg); n != want {
			t.Errorf("logged %q %d times, want %d", msg, n, want)
		}
	}
}
