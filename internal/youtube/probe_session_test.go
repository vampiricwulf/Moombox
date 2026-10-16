package youtube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// headerTransport answers every player call with adequateOKBody and records
// the request headers.
type headerTransport struct{ headers []http.Header }

func (tr *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.headers = append(tr.headers, req.Header.Clone())
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(adequateOKBody)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// recordPlayerCalls points the player API at a headerTransport for the test.
func recordPlayerCalls(t *testing.T) *headerTransport {
	t.Helper()
	tr := &headerTransport{}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })
	return tr
}

// accountCookies is a cookies.txt signed in as the named account: the
// SAPISID and LOGIN_INFO pair the jar's account (YouTubeIdentity) hashes.
// Name accounts with different lengths, so a rewritten file differs in size
// as well as content and no (size, mtime) reload memo can miss the switch.
func accountCookies(name string) string {
	return "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tsapisid-" + name + "\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tlogininfo-" + name + "\n"
}

// loadAccountJar writes cookieFile into the test's temp dir and loads it,
// returning the jar and the file's path so the test can replace or remove it.
func loadAccountJar(t *testing.T, cookieFile string) (*cookies.CookieJar, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFile), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	if jar.YouTubeIdentity() == "" {
		t.Fatal("setup: the jar holds no account")
	}
	return jar, path
}

// homepageStub serves the YouTube homepage for a test and records the Cookie
// header of every fetch. The page states the session of the account its
// cookies sign in as (sessions[the account's name]), as YouTube's does, and
// none for cookies it does not know.
type homepageStub struct {
	mu      sync.Mutex
	cookies []string
}

func (h *homepageStub) fetches() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cookies...)
}

// homepageFault is how a homepage stub answers one fetch.
type homepageFault int

const (
	homepageAnswers homepageFault = iota // the page, stating the account's session
	homepageFails                        // a 503
	homepageStalls                       // nothing, until the client gives up
)

func stubHomepage(t *testing.T, sessions map[string]string) *homepageStub {
	t.Helper()
	return stubFaultyHomepage(t, sessions, nil)
}

// stubFaultyHomepage is stubHomepage whose fetch number n (1-based, in arrival
// order) is answered as fault(n) says; a nil fault answers every fetch.
func stubFaultyHomepage(t *testing.T, sessions map[string]string, fault func(n int) homepageFault) *homepageStub {
	t.Helper()
	h := &homepageStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		h.mu.Lock()
		h.cookies = append(h.cookies, cookie)
		n := len(h.cookies)
		h.mu.Unlock()
		if fault != nil {
			switch fault(n) {
			case homepageFails:
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			case homepageStalls:
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
				return
			}
		}
		session := ""
		for name, s := range sessions {
			if strings.Contains(cookie, "logininfo-"+name+";") || strings.HasSuffix(cookie, "logininfo-"+name) {
				session = s
			}
		}
		_, _ = io.WriteString(w, `<script>ytcfg.set({`+session+`"INNERTUBE_API_KEY":"k","visitorData":"vd-home"});</script>`)
	}))
	t.Cleanup(srv.Close)
	orig := constants.YouTubeURLs.Base
	constants.YouTubeURLs.Base = srv.URL
	t.Cleanup(func() { constants.YouTubeURLs.Base = orig })
	return h
}

// The sessions the homepage stub states for the accounts these tests use.
const (
	sessionBrandA = `"SESSION_INDEX":"1","DELEGATED_SESSION_ID":"brand-a",`
	sessionBrandB = `"SESSION_INDEX":"2","DELEGATED_SESSION_ID":"brand-b",`
)

// runProbes runs the three probe-only calls through the Service, as the
// waiting room, the quality monitor and the feed monitor do.
func runProbes(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.ProbeVideoStatusAuthenticated(ctx, "test1234567"); err != nil {
		t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
	}
	if _, err := svc.ProbeVideoSchedule(ctx, "test1234567"); err != nil {
		t.Fatalf("ProbeVideoSchedule: %v", err)
	}
	if _, _, err := svc.ProbeVideoDate(ctx, "test1234567"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
}

// The probe-only calls carry the signed-in session the last watch page
// stated — X-Goog-AuthUser and X-Goog-PageId — as every call of a full
// extraction does. A membership held on a brand account exists only under
// that delegated identity: without the headers the probe is judged as the
// primary account and refused, so the members-only waiting room's status
// probe, the schedule probe and the date probe would all read a stream the
// user can archive as one they cannot. A later page that names no session
// (the public path's cookieless page) does not clear it, and nor does a page
// fetched with no account, whatever it states.
//
// Mutants: probeYtcfg building a bare DefaultYtcfg (no headers);
// captureVisitorData overwriting the session with an empty one;
// captureVisitorData not recording the account the session was read under
// (probeYtcfg then never attaches it); captureVisitorData keeping a session
// read under no account.
func TestProbesCarryTheWatchPagesSession(t *testing.T) {
	tr := recordPlayerCalls(t)
	jar, _ := loadAccountJar(t, accountCookies("a"))
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	idx := 2
	p.captureVisitorData(&YtcfgData{VisitorData: "vd", SessionIndex: &idx, DelegatedSessionID: "brand-123"}, jar.YouTubeIdentity())
	p.captureVisitorData(&YtcfgData{VisitorData: "vd-anon"}, "") // a cookieless page: no session, nothing cleared
	other := 5
	// A page fetched with no account keeps nothing, whatever it states.
	p.captureVisitorData(&YtcfgData{SessionIndex: &other, DelegatedSessionID: "brand-anon"}, "")

	ctx := context.Background()
	if _, err := p.ProbeVideoStatusAuthenticated(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
	}
	if _, err := p.ProbeVideoSchedule(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoSchedule: %v", err)
	}
	if _, _, err := p.ProbeVideoDate(ctx, "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
	if len(tr.headers) != 3 {
		t.Fatalf("recorded %d player calls, want 3", len(tr.headers))
	}
	for i, h := range tr.headers {
		if got := h.Get("X-Goog-AuthUser"); got != "2" {
			t.Errorf("call %d: X-Goog-AuthUser = %q, want 2", i, got)
		}
		if got := h.Get("X-Goog-PageId"); got != "brand-123" {
			t.Errorf("call %d: X-Goog-PageId = %q, want brand-123", i, got)
		}
	}
}

// homepageSession reads the session off the homepage HTML, and a homepage
// that states none — a cookieless one, or one whose cookies YouTube no longer
// signs in — clears nothing a watch page had stated.
//
// Mutants: homepageSession not reading SESSION_INDEX, or not reading
// DELEGATED_SESSION_ID; captureVisitorData clearing the session on a page
// that states none. Init's own call is pinned by
// TestInitKeepsTheHomepagesSession, not here.
func TestHomepageStatesTheSession(t *testing.T) {
	got := homepageSession(`ytcfg.set({"SESSION_INDEX":"1","DELEGATED_SESSION_ID":"brand-123","INNERTUBE_API_KEY":"k"});`)
	if got.SessionIndex == nil || *got.SessionIndex != 1 || got.DelegatedSessionID != "brand-123" {
		t.Errorf("homepage session = index %v, delegated %q; want 1 and brand-123", got.SessionIndex, got.DelegatedSessionID)
	}
	if anon := homepageSession(`ytcfg.set({"INNERTUBE_API_KEY":"k"});`); anon.SessionIndex != nil || anon.DelegatedSessionID != "" {
		t.Errorf("cookieless homepage session = %+v, want none", anon)
	}

	jar, _ := loadAccountJar(t, accountCookies("a"))
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	idx := 1
	p.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-123"}, jar.YouTubeIdentity())
	p.captureVisitorData(homepageSession(`ytcfg.set({"INNERTUBE_API_KEY":"k"});`), jar.YouTubeIdentity())
	if got := p.probeYtcfg(""); got.DelegatedSessionID != "brand-123" {
		t.Error("a homepage stating no session cleared the session a watch page had stated")
	}
}

// A page that states a new session replaces the old one whole: an operator
// who moved from a brand account to the primary must not keep sending the
// brand's page id.
func TestANewSessionReplacesTheOldOne(t *testing.T) {
	jar, _ := loadAccountJar(t, accountCookies("a"))
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	idx := 1
	p.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-123"}, jar.YouTubeIdentity())
	zero := 0
	p.captureVisitorData(&YtcfgData{SessionIndex: &zero}, jar.YouTubeIdentity())
	got := p.probeYtcfg("")
	if got.DelegatedSessionID != "" || got.SessionIndex == nil || *got.SessionIndex != 0 {
		t.Errorf("probe ytcfg = index %v, delegated %q; want index 0 and no delegation", got.SessionIndex, got.DelegatedSessionID)
	}
}

// Service.Init hands the homepage's session to the probes, read under the
// account whose cookies fetched the page — so the feed monitor's first
// membership and date probes carry it before any job has fetched a watch
// page — and that read is the account's one: the probes do not fetch the
// homepage again, even when it stated no session.
//
// Mutants: Init's captureVisitorData(homepageSession(html), account) deleted;
// Init capturing under "" instead of the account; Init not claiming the
// account's read (claimSessionRead), so the first probe fetches the homepage
// a second time ("states none" fails).
func TestInitKeepsTheHomepagesSession(t *testing.T) {
	t.Run("states the session", func(t *testing.T) {
		home := stubHomepage(t, map[string]string{"a": sessionBrandA})
		tr := recordPlayerCalls(t)
		jar, _ := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})

		svc.Init(context.Background())
		got := svc.PlayerAPI.probeYtcfg("")
		if got.DelegatedSessionID != "brand-a" || got.SessionIndex == nil || *got.SessionIndex != 1 {
			t.Fatalf("after Init, probe ytcfg = index %v, delegated %q; want 1 and brand-a", got.SessionIndex, got.DelegatedSessionID)
		}

		runProbes(t, svc)
		for i, h := range tr.headers {
			if h.Get("X-Goog-PageId") != "brand-a" || h.Get("X-Goog-AuthUser") != "1" {
				t.Errorf("probe %d: X-Goog-AuthUser = %q, X-Goog-PageId = %q; want 1 and brand-a", i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
			}
		}
		if f := home.fetches(); len(f) != 1 || !strings.Contains(f[0], "logininfo-a") {
			t.Errorf("homepage fetches = %q; want Init's one, with the account's cookies", f)
		}
	})

	t.Run("states none", func(t *testing.T) {
		home := stubHomepage(t, nil)
		tr := recordPlayerCalls(t)
		jar, _ := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})

		svc.Init(context.Background())
		runProbes(t, svc)
		for i, h := range tr.headers {
			if h.Get("X-Goog-PageId") != "" || h.Get("X-Goog-AuthUser") != "" {
				t.Errorf("probe %d: X-Goog-AuthUser = %q, X-Goog-PageId = %q; the homepage stated no session", i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
			}
		}
		if f := home.fetches(); len(f) != 1 {
			t.Errorf("homepage fetched %d times; want Init's one", len(f))
		}
	})
}

// A full extraction's watch page feeds the probes: the session it states is
// what the following probe carries, and having it, the probe needs no
// homepage read of its own.
//
// Mutants: GetVideoInfoAuthenticated's captureVisitorData(ytcfg, account)
// deleted, or capturing under "" (the probe then reads the homepage, which
// states no session here, and carries none); claimSessionRead ignoring the
// account a session is already kept for (the probe fetches the homepage).
func TestTheWatchPagesSessionReachesTheProbes(t *testing.T) {
	home := stubHomepage(t, nil)
	tr := recordPlayerCalls(t)
	idx := 1
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		ytcfg := DefaultYtcfg()
		ytcfg.SessionIndex, ytcfg.DelegatedSessionID = &idx, "brand-watch"
		return &WatchPageResult{Ytcfg: ytcfg}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
	jar, _ := loadAccountJar(t, accountCookies("a"))
	svc := NewService(jar, noopLogger{})

	if _, err := svc.GetVideoInfo(context.Background(), "test1234567"); err != nil {
		t.Fatalf("GetVideoInfo: %v", err)
	}
	tr.headers = nil
	if _, err := svc.ProbeVideoStatusAuthenticated(context.Background(), "test1234567"); err != nil {
		t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
	}
	if len(tr.headers) != 1 {
		t.Fatalf("recorded %d probe calls, want 1", len(tr.headers))
	}
	if h := tr.headers[0]; h.Get("X-Goog-PageId") != "brand-watch" || h.Get("X-Goog-AuthUser") != "1" {
		t.Errorf("probe X-Goog-AuthUser = %q, X-Goog-PageId = %q; want 1 and brand-watch", h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
	}
	if f := home.fetches(); len(f) != 0 {
		t.Errorf("the probe fetched the homepage %d times; the watch page had stated the session", len(f))
	}
}

// The probes' session stays with the account it was read under. Once
// cookies.txt is removed it must not ride along on requests that carry no
// credentials at all; once another account's cookies are imported it must
// not name account A's brand page on account B's requests. Instead the probe
// that first sees account B reads B's session off the homepage, as Init reads
// the startup account's, once for the account — so a runtime import gets the
// brand session from the next monitor cycle, with no watch page needed.
//
// Mutants: probeYtcfg attaching the kept session whatever the jar holds (the
// pre-fix code: "cookies removed" and "states none" fail); probeYtcfg checking
// only that the jar holds an account ("states none" fails); readAccountSession
// dropped from any one of the three probe wrappers (that probe's subtest
// fails); claimSessionRead claiming every time (every subtest that counts
// reads fails), or claiming again an account already asked for whose
// homepage stated none ("states none" fails). Not pinned:
// reading the account after the cookie header rather than before — the
// window is a reload landing between two adjacent reads.
func TestTheProbeSessionStaysWithItsAccount(t *testing.T) {
	setup := func(t *testing.T, sessions map[string]string) (*Service, string, *homepageStub, *headerTransport) {
		t.Helper()
		home := stubHomepage(t, sessions)
		tr := recordPlayerCalls(t)
		jar, path := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})
		svc.Init(context.Background())
		if got := svc.PlayerAPI.probeYtcfg(""); got.DelegatedSessionID != "brand-a" {
			t.Fatalf("setup: after Init the probes carry %q, want brand-a", got.DelegatedSessionID)
		}
		return svc, path, home, tr
	}
	both := map[string]string{"a": sessionBrandA, "other-account": sessionBrandB}

	t.Run("cookies removed", func(t *testing.T) {
		svc, path, home, tr := setup(t, both)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		runProbes(t, svc)
		if svc.HasAnyAuthCookie() {
			t.Fatal("setup: the jar still holds auth cookies after the file was removed")
		}
		for i, h := range tr.headers {
			if h.Get("X-Goog-AuthUser") != "" || h.Get("X-Goog-PageId") != "" {
				t.Errorf("probe %d: no cookies, yet X-Goog-AuthUser = %q, X-Goog-PageId = %q", i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
			}
		}
		if f := home.fetches(); len(f) != 1 {
			t.Errorf("homepage fetched %d times; a jar with no account has no session to read", len(f))
		}
	})

	t.Run("another account whose homepage states none", func(t *testing.T) {
		svc, path, home, tr := setup(t, map[string]string{"a": sessionBrandA})
		if err := os.WriteFile(path, []byte(accountCookies("other-account")), 0o600); err != nil {
			t.Fatal(err)
		}
		runProbes(t, svc)
		for i, h := range tr.headers {
			if !strings.Contains(h.Get("Cookie"), "other-account") {
				t.Fatalf("setup: probe %d sent Cookie %q, want account B's", i, h.Get("Cookie"))
			}
			if h.Get("X-Goog-PageId") != "" || h.Get("X-Goog-AuthUser") != "" {
				t.Errorf("probe %d: account B's cookies with X-Goog-AuthUser = %q, X-Goog-PageId = %q — account A's session", i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
			}
		}
		// One read for account B although it found nothing: the probes after
		// the first do not fetch the homepage again.
		if f := home.fetches(); len(f) != 2 {
			t.Errorf("homepage fetched %d times; want Init's and one for the new account", len(f))
		}
	})

	probes := []struct {
		name  string
		probe func(*Service) error
	}{
		{"status", func(svc *Service) error {
			_, err := svc.ProbeVideoStatusAuthenticated(context.Background(), "test1234567")
			return err
		}},
		{"schedule", func(svc *Service) error {
			_, err := svc.ProbeVideoSchedule(context.Background(), "test1234567")
			return err
		}},
		{"date", func(svc *Service) error {
			_, _, err := svc.ProbeVideoDate(context.Background(), "test1234567")
			return err
		}},
	}
	for _, pc := range probes {
		name, probe := pc.name, pc.probe
		t.Run("another account: "+name+" probe reads its session once", func(t *testing.T) {
			svc, path, home, tr := setup(t, both)
			if err := os.WriteFile(path, []byte(accountCookies("other-account")), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := probe(svc); err != nil {
					t.Fatalf("%s probe: %v", name, err)
				}
			}
			for i, h := range tr.headers {
				if h.Get("X-Goog-PageId") != "brand-b" || h.Get("X-Goog-AuthUser") != "2" {
					t.Errorf("call %d: X-Goog-AuthUser = %q, X-Goog-PageId = %q; want account B's 2 and brand-b", i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
				}
			}
			f := home.fetches()
			if len(f) != 2 {
				t.Fatalf("homepage fetched %d times; want Init's and one for the new account", len(f))
			}
			if !strings.Contains(f[1], "other-account") {
				t.Errorf("the new account's homepage read sent Cookie %q, want account B's", f[1])
			}
		})
	}
}

// A homepage session read that did not come back with a page is not the
// account's one read. Only a page decides it — one stating no session is
// final for the account (TestTheProbeSessionStaysWithItsAccount). A read
// the probe's caller abandoned (its context ended: a cancelled job, the feed
// walk's pass budget running out) is handed back at once, so the next probe
// reads it. A read that failed (a transport error, a 5xx) is handed back
// after sessionReadRetryAfter, so one network blip at an account switch, or
// a network not yet up at boot, does not leave that account's probes without
// its session for the rest of the run, and a waiting room offline does not
// fetch the homepage on every 30 s poll either.
//
// Mutants: readAccountSession not releasing a failed read ("abandoned" and
// "fails" fail); releaseSessionRead holding an abandoned read for the
// cooldown like a failed one ("abandoned" fails); releaseSessionRead
// releasing a failed read at once ("fails" and "the startup read fails": the
// probe inside the cooldown fetches the homepage again); claimSessionRead
// never claiming an asked-for account again, whatever its deadline (the probe
// after the cooldown does not read); Init not releasing its failed read ("the
// startup read fails").
func TestAHomepageReadThatDidNotComeBackIsNotTheAccountsOne(t *testing.T) {
	both := map[string]string{"a": sessionBrandA, "other-account": sessionBrandB}
	// setup boots a Service on account A (Init is homepage fetch 1) whose
	// session-read clock the test moves by hand.
	setup := func(t *testing.T, fault func(n int) homepageFault) (*Service, string, *homepageStub, *headerTransport, *time.Time) {
		t.Helper()
		home := stubFaultyHomepage(t, both, fault)
		tr := recordPlayerCalls(t)
		jar, path := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})
		now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		svc.PlayerAPI.sessionClock = func() time.Time { return now }
		svc.Init(context.Background())
		return svc, path, home, tr, &now
	}
	wantSession := func(t *testing.T, tr *headerTransport, when, authUser, pageID string) {
		t.Helper()
		if len(tr.headers) == 0 {
			t.Fatalf("%s: no probe call recorded", when)
		}
		for i, h := range tr.headers {
			if h.Get("X-Goog-AuthUser") != authUser || h.Get("X-Goog-PageId") != pageID {
				t.Errorf("%s, probe %d: X-Goog-AuthUser = %q, X-Goog-PageId = %q; want %q and %q",
					when, i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"), authUser, pageID)
			}
		}
	}
	switchToB := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(accountCookies("other-account")), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("abandoned by its caller", func(t *testing.T) {
		svc, path, home, tr, _ := setup(t, func(n int) homepageFault {
			if n == 2 {
				return homepageStalls
			}
			return homepageAnswers
		})
		switchToB(t, path)
		// The first probe to see account B runs out of time while B's
		// homepage read is in flight.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _ = svc.ProbeVideoStatusAuthenticated(ctx, "test1234567")
		cancel()
		if f := home.fetches(); len(f) != 2 {
			t.Fatalf("setup: homepage fetched %d times; want Init's and the abandoned one", len(f))
		}

		tr.headers = nil
		runProbes(t, svc)
		wantSession(t, tr, "after the abandoned read", "2", "brand-b")
		if f := home.fetches(); len(f) != 3 {
			t.Errorf("homepage fetched %d times; want Init's, the abandoned one and one read for account B", len(f))
		}
	})

	t.Run("fails", func(t *testing.T) {
		svc, path, home, tr, now := setup(t, func(n int) homepageFault {
			if n == 2 {
				return homepageFails
			}
			return homepageAnswers
		})
		switchToB(t, path)
		runProbes(t, svc)
		wantSession(t, tr, "inside the cooldown", "", "")
		if f := home.fetches(); len(f) != 2 {
			t.Errorf("homepage fetched %d times inside the cooldown; want Init's and the failed one", len(f))
		}

		*now = now.Add(sessionReadRetryAfter + time.Second)
		tr.headers = nil
		runProbes(t, svc)
		wantSession(t, tr, "after the cooldown", "2", "brand-b")
		if f := home.fetches(); len(f) != 3 {
			t.Errorf("homepage fetched %d times; want Init's, the failed one and one read after the cooldown", len(f))
		}
	})

	t.Run("the startup read fails", func(t *testing.T) {
		svc, _, home, tr, now := setup(t, func(n int) homepageFault {
			if n == 1 {
				return homepageFails
			}
			return homepageAnswers
		})
		runProbes(t, svc)
		wantSession(t, tr, "inside the cooldown", "", "")
		if f := home.fetches(); len(f) != 1 {
			t.Errorf("homepage fetched %d times inside the cooldown; want Init's failed one", len(f))
		}

		*now = now.Add(sessionReadRetryAfter + time.Second)
		tr.headers = nil
		runProbes(t, svc)
		wantSession(t, tr, "after the cooldown", "1", "brand-a")
		if f := home.fetches(); len(f) != 2 {
			t.Errorf("homepage fetched %d times; want Init's failed one and one read after the cooldown", len(f))
		}
	})
}

// A release hands back only the claim it was made for. Once the claim has
// moved on to another account — the jar switched again while the first
// account's read was in flight, and a probe claimed the new one — the first
// read's failure must not put the other account's read on a retry deadline,
// nor its abandonment hand that read back.
//
// Mutant: releaseSessionRead not checking that the claim is still the
// account's (both checks fail).
func TestAReleaseLeavesAClaimThatMovedOn(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	p := &PlayerAPI{sessionClock: func() time.Time { return now }}
	if !p.claimSessionRead("account-b") || !p.claimSessionRead("account-c") {
		t.Fatal("setup: the two accounts' reads were not claimed")
	}
	p.releaseSessionRead("account-b", false)
	now = now.Add(sessionReadRetryAfter + time.Second)
	if p.claimSessionRead("account-c") {
		t.Error("account B's failed read put account C's read on B's retry deadline")
	}
	p.releaseSessionRead("account-b", true)
	if p.claimSessionRead("account-c") {
		t.Error("account B's abandoned read handed back account C's")
	}
}

// An account whose kept session a page under another account evicted is read
// again when the jar holds it once more. The operator boots on account A
// (Init reads A's brand session), imports account B and adds a B-member video,
// whose full extraction fetches a watch page under B before any probe has run
// (so B's session takes the one slot and no probe ever claimed B), then
// imports A again — Moombox's own not-a-member remedy. A's probes must carry
// A's session again, read off the homepage once, not go without it because
// A's startup read still counted as done.
//
// Mutant: captureVisitorData not handing back the claim of an account whose
// session it evicted (every probe sends no session, and the homepage is
// fetched once).
func TestAnAccountWhoseSessionWasEvictedIsReadAgain(t *testing.T) {
	home := stubHomepage(t, map[string]string{"a": sessionBrandA, "other-account": sessionBrandB})
	tr := recordPlayerCalls(t)
	jar, path := loadAccountJar(t, accountCookies("a"))
	svc := NewService(jar, noopLogger{})
	svc.Init(context.Background())
	if got := svc.PlayerAPI.probeYtcfg(""); got.DelegatedSessionID != "brand-a" {
		t.Fatalf("setup: after Init the probes carry %q, want brand-a", got.DelegatedSessionID)
	}

	idxB := 2
	orig := fetchWatchPage
	fetchWatchPage = func(_ context.Context, _ string, cookie string) (*WatchPageResult, error) {
		ytcfg := DefaultYtcfg()
		if strings.Contains(cookie, "logininfo-other-account") {
			ytcfg.SessionIndex, ytcfg.DelegatedSessionID = &idxB, "brand-b"
		}
		return &WatchPageResult{Ytcfg: ytcfg}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })

	if err := os.WriteFile(path, []byte(accountCookies("other-account")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetVideoInfo(context.Background(), "test1234567"); err != nil {
		t.Fatalf("GetVideoInfo under account B: %v", err)
	}
	if got := svc.PlayerAPI.probeYtcfg(""); got.DelegatedSessionID != "brand-b" {
		t.Fatalf("setup: after B's watch page the probes carry %q, want brand-b", got.DelegatedSessionID)
	}

	if err := os.WriteFile(path, []byte(accountCookies("a")), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.headers = nil
	runProbes(t, svc)
	for i, h := range tr.headers {
		if !strings.Contains(h.Get("Cookie"), "logininfo-a") {
			t.Fatalf("setup: probe %d sent Cookie %q, want account A's", i, h.Get("Cookie"))
		}
		if h.Get("X-Goog-AuthUser") != "1" || h.Get("X-Goog-PageId") != "brand-a" {
			t.Errorf("probe %d back on account A: X-Goog-AuthUser = %q, X-Goog-PageId = %q; want A's 1 and brand-a",
				i, h.Get("X-Goog-AuthUser"), h.Get("X-Goog-PageId"))
		}
	}
	f := home.fetches()
	if len(f) != 2 {
		t.Fatalf("homepage fetched %d times; want Init's and one read for account A again", len(f))
	}
	if !strings.Contains(f[1], "logininfo-a") {
		t.Errorf("the second homepage read sent Cookie %q, want account A's", f[1])
	}
}
