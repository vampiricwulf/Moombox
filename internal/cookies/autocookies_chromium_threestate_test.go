package cookies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// --- the pure decision -------------------------------------------------

// TestCdpCookieReadOutcome is the table the three-state distinction lives or
// dies on. Three inputs, and the middle answer — "the browser answered and
// holds nothing" — is the one that did not exist before: every one of these
// rows used to produce the same sentence, "CDP returned no cookies".
//
// The `some tiers errored, one answered empty` row is deliberately NOT a
// failure. A tier that answers is evidence about the profile; a tier that
// errors is evidence about that tier (Storage.getCookies is simply absent from
// some builds, which is why the fallbacks exist).
func TestCdpCookieReadOutcome(t *testing.T) {
	boom := errors.New("CDP error: 'Storage.getCookies' wasn't found")

	tests := []struct {
		name              string
		anyQuerySucceeded bool
		cookieCount       int
		lastErr           error
		wantEmptyProfile  bool // errors.Is(err, ErrNoCookiesInProfile)
		wantErr           bool
	}{
		{
			// Route 1: the user opened the setup browser and closed it
			// without signing in. Normal, and it has its own message.
			name:              "every tier answered and every answer was empty",
			anyQuerySucceeded: true,
			wantEmptyProfile:  true,
			wantErr:           true,
		},
		{
			// Route 2: nothing could be read. Says nothing about whether the
			// user is signed in, so it must not claim they are not.
			name:             "every tier errored",
			lastErr:          boom,
			wantErr:          true,
			wantEmptyProfile: false,
		},
		{
			// Route 3: the mix. One tier is missing from this build, another
			// answered — and the answer is the part that knows something.
			name:              "some tiers errored and one answered empty",
			anyQuerySucceeded: true,
			lastErr:           boom,
			wantEmptyProfile:  true,
			wantErr:           true,
		},
		{
			name:              "a tier answered with cookies",
			anyQuerySucceeded: true,
			cookieCount:       7,
		},
		{
			// Cookies in hand outrank a sibling tier's failure: the read
			// produced what it was for.
			name:              "cookies despite an earlier tier failing",
			anyQuerySucceeded: true,
			cookieCount:       3,
			lastErr:           boom,
		},
		{
			// Defensive: nothing answered, nothing failed. "We never looked"
			// must not render as "the profile is empty".
			name:    "nothing was attempted",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cdpCookieReadOutcome(tt.anyQuerySucceeded, tt.cookieCount, tt.lastErr)
			if tt.wantErr != (err != nil) {
				t.Fatalf("cdpCookieReadOutcome(...) = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNoCookiesInProfile); got != tt.wantEmptyProfile {
				t.Errorf("errors.Is(err, ErrNoCookiesInProfile) = %v, want %v (err = %v) — "+
					"an empty profile and an unreadable one need different messages and different remedies",
					got, tt.wantEmptyProfile, err)
			}
			// A read that failed must carry WHY. The old terminal error named
			// the three methods and nothing else, so the operator got a list of
			// CDP commands and no cause.
			if tt.wantErr && !tt.wantEmptyProfile && tt.lastErr != nil && !errors.Is(err, tt.lastErr) {
				t.Errorf("the failing tier's error was dropped: %v", err)
			}
		})
	}
}

// --- a stub CDP browser ------------------------------------------------

// cdpAnswer is how one stubbed CDP cookie query behaves.
type cdpAnswer int

const (
	// cdpAnswersEmpty is the zero value on purpose: an unlisted method
	// answers successfully with no cookies, which is the state under test.
	cdpAnswersEmpty cdpAnswer = iota
	cdpAnswersCookies
	cdpFails
)

// stubCookieName / stubCookieValue are fabricated. Nothing in this file reads a
// real cookie store.
const (
	stubCookieName  = "SAPISID"
	stubCookieValue = "stub-not-a-real-credential"
)

// startStubCDP runs an httptest server that speaks enough of the Chrome
// DevTools Protocol for cdpGetCookiesAsNetscape and cdpEnsurePageTarget: the
// two /json endpoints plus a WebSocket that answers the commands they send.
//
// It exists so the three-state classification can be exercised END TO END —
// the pure helper cannot catch the wiring mistake the fix is about, because
// that mistake is in what the tiers report, not in how the report is judged.
// Never launches a browser. TestCdpNavigateReportsAMissingPageTarget is the
// in-repo precedent for the /json half.
func startStubCDP(t *testing.T, answers map[string]cdpAnswer) int {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}
	wsBase := "ws://" + parsed.Host

	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, wsBase+"/devtools/browser/stub")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"type":"page","webSocketDebuggerUrl":%q}]`, wsBase+"/devtools/page/stub")
	})
	mux.HandleFunc("/devtools/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "stub done")
		serveStubCDP(r.Context(), conn, answers)
	})

	return port
}

func serveStubCDP(ctx context.Context, conn *websocket.Conn, answers map[string]cdpAnswer) {
	write := func(payload string) error {
		return conn.Write(ctx, websocket.MessageText, []byte(payload))
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(data, &msg) != nil {
			return
		}
		switch msg.Method {
		case "Storage.getCookies", "Network.getAllCookies", "Network.getCookies":
			switch answers[msg.Method] {
			case cdpFails:
				err = write(fmt.Sprintf(`{"id":%d,"error":{"message":%q}}`,
					msg.ID, "'"+msg.Method+"' wasn't found"))
			case cdpAnswersCookies:
				err = write(fmt.Sprintf(
					`{"id":%d,"result":{"cookies":[{"name":%q,"value":%q,"domain":".youtube.com","path":"/","expires":-1,"httpOnly":false,"secure":true}]}}`,
					msg.ID, stubCookieName, stubCookieValue))
			default:
				// The trap, in wire form: a well-formed answer of zero
				// cookies. It is a SUCCESS.
				err = write(fmt.Sprintf(`{"id":%d,"result":{"cookies":[]}}`, msg.ID))
			}
		case "Page.navigate":
			if err = write(fmt.Sprintf(`{"id":%d,"result":{}}`, msg.ID)); err == nil {
				// So cdpNavigateAndWait returns immediately instead of
				// burning its 30s budget waiting for a load event.
				err = write(`{"method":"Page.loadEventFired","params":{}}`)
			}
		default:
			err = write(fmt.Sprintf(`{"id":%d,"result":{}}`, msg.ID))
		}
		if err != nil {
			return
		}
	}
}

// TestCdpGetCookiesTellsAnEmptyProfileFromAFailedRead is the wiring test: it
// drives the real tier loop against a stub browser, which is the only way to
// catch anyQuerySucceeded being wired to "a query returned COOKIES" rather than
// "a query returned WITHOUT ERROR".
//
// Under that wiring the first row below still produces a plain error, because
// an empty profile returns zero cookies from all three tiers — the exact
// conflation the sentinel exists to remove. Verified against a worktree at
// 2be52a5: rows 1 and 3 fail there.
func TestCdpGetCookiesTellsAnEmptyProfileFromAFailedRead(t *testing.T) {
	tests := []struct {
		name             string
		answers          map[string]cdpAnswer
		wantEmptyProfile bool
		wantErr          bool
		wantCookie       bool
	}{
		{
			name:             "signed out — every tier answers with an empty jar",
			wantEmptyProfile: true,
			wantErr:          true,
		},
		{
			name: "unreadable — every tier fails",
			answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpFails,
				"Network.getAllCookies": cdpFails,
				"Network.getCookies":    cdpFails,
			},
			wantErr: true,
		},
		{
			name: "mixed — the browser tier is missing, a page tier answers empty",
			answers: map[string]cdpAnswer{
				"Storage.getCookies": cdpFails,
			},
			wantEmptyProfile: true,
			wantErr:          true,
		},
		{
			name:       "signed in — the browser tier returns cookies",
			answers:    map[string]cdpAnswer{"Storage.getCookies": cdpAnswersCookies},
			wantCookie: true,
		},
		{
			name: "signed in via a fallback — the browser tier is missing",
			answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpFails,
				"Network.getAllCookies": cdpAnswersCookies,
			},
			wantCookie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := startStubCDP(t, tt.answers)

			netscape, err := cdpGetCookiesAsNetscape(context.Background(), port)
			if tt.wantErr != (err != nil) {
				t.Fatalf("cdpGetCookiesAsNetscape err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNoCookiesInProfile); got != tt.wantEmptyProfile {
				t.Errorf("errors.Is(err, ErrNoCookiesInProfile) = %v, want %v (err = %v) — "+
					"a query that ANSWERS with zero cookies is a successful read of an empty profile, not a failed read",
					got, tt.wantEmptyProfile, err)
			}
			if tt.wantCookie && !strings.Contains(netscape, stubCookieName) {
				t.Errorf("the extracted cookies are missing %s:\n%s", stubCookieName, netscape)
			}
		})
	}
}

// --- what the two callers now do with it -------------------------------

// TestFinishSetupOnAnEmptyChromiumProfileReportsNoLogin is the Chromium half of
// TestFinishSetupTreatsEmptyProfileAsNoLogin, which has covered Firefox alone
// since the translation was written inside the Firefox arm of the if/else.
//
// A Chromium user who opened the setup browser and closed it without signing in
// got the finish route's `default` arm — HTTP 500, "failed to finish setup" —
// for a state that is not a failure. The Web UI throws on any non-2xx, so the
// dialog rendered "HTTP 500" at a user whose only problem was that they had not
// signed in yet.
func TestFinishSetupOnAnEmptyChromiumProfileReportsNoLogin(t *testing.T) {
	captureKills(t) // no PID from this test may reach taskkill
	port := startStubCDP(t, nil)

	s := NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.setupProcess = &os.Process{Pid: -1}
	s.setupBrowser = &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
	s.cdpPort = port
	s.targetPlatform = "youtube"

	ytAuth, twAuth, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("a Chromium setup nobody signed in to must not be a hard error "+
			"(the route maps every error here to a 500): %v", err)
	}
	if ytAuth || twAuth {
		t.Fatalf("FinishSetup on an empty profile = (%v, %v), want (false, false)", ytAuth, twAuth)
	}
	status := s.GetStatus()
	if status.LastError == nil || !strings.Contains(*status.LastError, "no login detected") {
		t.Errorf("LastError should tell the user to sign in, got %v", status.LastError)
	}
}

// stubChromiumRefresh replaces the headless-Chromium refresh step for one test.
// See refreshChromiumCookies for why the seam exists: the downgrade under test
// is downstream of a real browser and a real CDP conversation.
func stubChromiumRefresh(t *testing.T, cookies string, navigated bool, err error) {
	t.Helper()
	prev := refreshChromiumCookies
	refreshChromiumCookies = func(*AutoCookieService, context.Context, *DetectedBrowser) (string, bool, error) {
		return cookies, navigated, err
	}
	t.Cleanup(func() { refreshChromiumCookies = prev })
}

func chromiumBrowser() *DetectedBrowser {
	return &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
}

// TestChromiumRefreshWithAnEmptyProfileKeepsTheExistingCookies is the refresh
// half of the same hoist.
//
// The ErrNoCookiesInProfile downgrade sat inside the Firefox arm, so a Chromium
// profile that came back empty — a browser clearing cookies on exit, the
// mundane explanation the Firefox comment already named — aborted the refresh
// and reported "Cookie Auto-Refresh Failed" at a user whose cookies.txt was
// still perfectly good.
func TestChromiumRefreshWithAnEmptyProfileKeepsTheExistingCookies(t *testing.T) {
	newService := func(t *testing.T) *AutoCookieService {
		t.Helper()
		stubChromiumRefresh(t, "", true, fmt.Errorf("stub CDP read: %w", ErrNoCookiesInProfile))

		cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
		if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewAutoCookieService(t.TempDir(), cookiePath, NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = chromiumBrowser
		// The refresh gate reads the jar, exactly as production does after
		// loading cookies.txt at startup.
		if err := s.jar.Load(cookiePath); err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("the credentials on disk still work", func(t *testing.T) {
		s := newService(t)
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

		ok, err := s.RefreshCookies(context.Background())
		if err != nil {
			t.Fatalf("an empty Chromium profile must fall back to cookies.txt, not fail the refresh: %v", err)
		}
		if !ok {
			t.Fatal("both platforms verified off the existing cookies.txt, so the refresh did succeed")
		}
		if status := s.GetStatus(); status.LastError != nil {
			t.Errorf("nothing is wrong with these credentials: %q", *status.LastError)
		}
		// The fallback must keep what was there. Contributing "" and then
		// writing it would be the data-loss version of this fix.
		data, readErr := os.ReadFile(s.cookiePath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(data), goodTwitchToken) || !strings.Contains(string(data), "previous-sapisid") {
			t.Errorf("the existing credentials did not survive the empty refresh:\n%s", data)
		}
	})

	t.Run("the credentials on disk are dead too", func(t *testing.T) {
		s := newService(t)
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

		ok, err := s.RefreshCookies(context.Background())
		if err != nil {
			t.Fatalf("RefreshCookies: %v", err)
		}
		if ok {
			t.Fatal("nothing verified, so this refresh did not succeed")
		}
		// emptyBrowserProfile now reaches the Chromium path too: an operator
		// whose stored cookies just failed needs to know the browser handed
		// back nothing to replace them with.
		status := s.GetStatus()
		if status.LastError == nil {
			t.Fatal("a failed verification with an empty browser profile must be stated")
		}
		if !strings.Contains(*status.LastError, "no cookies to refresh from") {
			t.Errorf("the status should name the empty profile as the explanation, got %q", *status.LastError)
		}
	})
}
