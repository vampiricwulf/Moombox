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
// dies on. Every one of these rows used to produce the same sentence, "CDP
// returned no cookies".
//
// Three claims are asserted here and each has a named alternative:
//
//   - `some tiers errored, one answered empty` is deliberately NOT a failure.
//     A tier that answers is evidence about the profile; a tier that errors is
//     evidence about that tier (Storage.getCookies is absent from some builds,
//     which is why the fallbacks exist).
//   - ...but that row still has to CARRY the failure it out-voted. The empty
//     verdict is the one place a tier error can be silently swallowed, and this
//     function's whole premise is that swallowing causes is what broke the
//     Chromium read in the first place.
//   - `the ladder was blocked` is deliberately NOT the empty verdict. An answer
//     with no relevant rows only means "the profile is empty" once the
//     fallbacks paired with it have been asked.
//
// wantSentinel is the fourth claim, added in Arc 8 Task 12a. The three verdicts
// above were told apart in PROSE only, so every consumer past this function saw
// one undifferentiated error and the setup route flattened all of them to a 500
// that gave no hint. Each non-nil outcome now wraps a sentinel, and the rows
// assert WHICH — a distinction that exists only in a message is a distinction
// the wire cannot carry.
func TestCdpCookieReadOutcome(t *testing.T) {
	boom := errors.New("CDP error: 'Storage.getCookies' wasn't found")

	// The three sentinels this function may produce, so a row asserting one can
	// deny the other two. Their separation is the point: ErrNoCookiesInProfile
	// is a verdict about the PROFILE, which FinishSetup turns into "no login
	// detected" and RefreshCookiesDetailed downgrades to a fall-back; the other
	// two mean the read never got far enough to have a verdict, so routing
	// either one there would tell a signed-in user they are not signed in.
	allSentinels := []error{ErrNoCookiesInProfile, ErrBrowserLadderBlocked, ErrBrowserReadUnanswered}

	tests := []struct {
		name             string
		read             cdpCookieRead
		wantEmptyProfile bool  // errors.Is(err, ErrNoCookiesInProfile)
		wantSentinel     error // the ONE sentinel this outcome must wrap; nil for a success
		wantErr          bool
		wantCause        bool // the message names lastErr
	}{
		{
			// Route 1: the user opened the setup browser and closed it
			// without signing in. Normal, and it has its own message.
			name:             "every tier answered and every answer was empty",
			read:             cdpCookieRead{anyQuerySucceeded: true},
			wantEmptyProfile: true,
			wantSentinel:     ErrNoCookiesInProfile,
			wantErr:          true,
		},
		{
			// Route 2: nothing could be read. Says nothing about whether the
			// user is signed in, so it must not claim they are not.
			name:         "every tier errored",
			read:         cdpCookieRead{lastErr: boom},
			wantSentinel: ErrBrowserReadUnanswered,
			wantErr:      true,
			wantCause:    true,
		},
		{
			// Route 3: the mix. One tier is missing from this build, another
			// answered — and the answer is the part that knows something. The
			// missing tier is still named: see the doc above.
			name:             "some tiers errored and one answered empty",
			read:             cdpCookieRead{anyQuerySucceeded: true, lastErr: boom},
			wantEmptyProfile: true,
			wantSentinel:     ErrNoCookiesInProfile,
			wantErr:          true,
			wantCause:        true,
		},
		{
			// Route 2b: a query answered, but the fallbacks it needs to be
			// corroborated by never ran. "You did not sign in" is not something
			// this read is in a position to say.
			name: "a tier answered empty but the ladder was blocked",
			read: cdpCookieRead{
				anyQuerySucceeded: true,
				ladderBlocked:     true,
				lastErr:           errors.New("CDP cookie fallback listing failed: connection refused"),
			},
			wantSentinel: ErrBrowserLadderBlocked,
			wantErr:      true,
			wantCause:    true,
		},
		{
			name: "a tier answered with relevant rows",
			read: cdpCookieRead{anyQuerySucceeded: true, relevantRows: 7},
		},
		{
			// Rows in hand outrank both a sibling tier's failure and a blocked
			// ladder: the read produced what it was for.
			name: "relevant rows despite an earlier tier failing",
			read: cdpCookieRead{anyQuerySucceeded: true, relevantRows: 3, ladderBlocked: true, lastErr: boom},
		},
		{
			// Defensive: nothing answered, nothing failed. "We never looked"
			// must not render as "the profile is empty".
			// Same sentinel as "every tier errored", and deliberately: from a
			// caller's side the two ARE one state — no query answered, so
			// nothing was learned — and the only difference is whether the
			// bookkeeping recorded a cause. A sentinel of its own would put a
			// bookkeeping bug on the wire as a distinct operator-facing case.
			name:         "nothing was attempted",
			read:         cdpCookieRead{},
			wantSentinel: ErrBrowserReadUnanswered,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cdpCookieReadOutcome(tt.read)
			if tt.wantErr != (err != nil) {
				t.Fatalf("cdpCookieReadOutcome(%+v) = %v, wantErr %v", tt.read, err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNoCookiesInProfile); got != tt.wantEmptyProfile {
				t.Errorf("errors.Is(err, ErrNoCookiesInProfile) = %v, want %v (err = %v) — "+
					"an empty profile, an unreadable one and a half-finished read need "+
					"different messages and different remedies",
					got, tt.wantEmptyProfile, err)
			}
			// EXACTLY ONE sentinel, asserted in both directions. Checking only
			// the wanted one would be satisfied by an error that wrapped all
			// three, which is the shape that reads as "any of these remedies
			// might apply" — the collapse these sentinels exist to undo.
			for _, sentinel := range allSentinels {
				want := sentinel == tt.wantSentinel
				if got := errors.Is(err, sentinel); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v (err = %v) — the route maps each "+
						"of these to a different status, so an outcome that matches the wrong "+
						"one sends the operator after the wrong problem",
						sentinel, got, want, err)
				}
			}
			// Every failing outcome must carry WHY, the empty verdict included.
			// The old terminal error named the three methods and nothing else,
			// so the operator got a list of CDP commands and no cause.
			if tt.wantCause && !strings.Contains(err.Error(), tt.read.lastErr.Error()) {
				t.Errorf("the failing tier's error was dropped — there is no logger in this "+
					"function, so this string is the only place it could have gone: %v", err)
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
	// cdpAnswersIrrelevant answers successfully with cookies that Moombox
	// throws away — another site's session. Raw count non-zero, relevant count
	// zero: the two predicates that used to disagree.
	cdpAnswersIrrelevant
	cdpFails
)

// stubCookieName / stubCookieValue are fabricated. Nothing in this file reads a
// real cookie store.
const (
	stubCookieName  = "SAPISID"
	stubCookieValue = "stub-not-a-real-credential"
	// stubOtherSiteCookie is on a domain isRelevantDomain rejects, so it
	// survives no filter and appears in no cookies.txt.
	stubOtherSiteCookie = "sessionid"
)

// stubCDPOptions configures one stub browser.
type stubCDPOptions struct {
	// answers is keyed by CDP method; an absent method answers empty.
	answers map[string]cdpAnswer
	// failTargetList makes GET /json fail, which is what stops the page-level
	// fallback ladder from running at all.
	failTargetList bool
	// suppressLoadEvent acks Page.navigate but never follows it with
	// Page.loadEventFired — the connect-then-stall shape (Arc 8 7(e)): the
	// browser accepted the connection and answered Page.enable and
	// Page.navigate, then never loaded anything. cdpNavigateAndWait's read
	// loop is left blocking on the caller's context, so the test supplies a
	// short-lived one rather than waiting out cdpNavigateTimeout, which stays
	// untouched.
	suppressLoadEvent bool
}

// startStubCDP runs an httptest server that speaks enough of the Chrome
// DevTools Protocol for cdpGetCookiesAsNetscape and cdpEnsurePageTarget: the
// two /json endpoints plus a WebSocket that answers the commands they send.
//
// It exists so the three-state classification can be exercised END TO END —
// the pure helper cannot catch the wiring mistake the fix is about, because
// that mistake is in what the tiers report, not in how the report is judged.
// Never launches a browser. TestCdpNavigateReportsAMissingPageTarget is the
// in-repo precedent for the /json half.
func startStubCDP(t *testing.T, opts stubCDPOptions) int {
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
		if opts.failTargetList {
			// The browser is up — /json/version answered a moment ago — but the
			// target list does not arrive, so the page-level tiers cannot run.
			// Hijacked and dropped rather than answered with a 500, so the
			// failure lands on the transport (cookiesHTTPClient.Do returns an
			// error) rather than on the JSON decode.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, hijackErr := hj.Hijack(); hijackErr == nil {
					conn.Close()
					return
				}
			}
			http.Error(w, "stub target listing unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"type":"page","webSocketDebuggerUrl":%q}]`, wsBase+"/devtools/page/stub")
	})
	mux.HandleFunc("/devtools/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "stub done")
		serveStubCDP(r.Context(), conn, opts)
	})

	return port
}

func serveStubCDP(ctx context.Context, conn *websocket.Conn, opts stubCDPOptions) {
	answers := opts.answers
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
			case cdpAnswersIrrelevant:
				// Raw count 1, relevant count 0 — a browser that has been used
				// for something other than YouTube or Twitch.
				err = write(fmt.Sprintf(
					`{"id":%d,"result":{"cookies":[{"name":%q,"value":"stub","domain":".example.com","path":"/","expires":-1,"httpOnly":false,"secure":false}]}}`,
					msg.ID, stubOtherSiteCookie))
			default:
				// The trap, in wire form: a well-formed answer of zero
				// cookies. It is a SUCCESS.
				err = write(fmt.Sprintf(`{"id":%d,"result":{"cookies":[]}}`, msg.ID))
			}
		case "Page.navigate":
			if err = write(fmt.Sprintf(`{"id":%d,"result":{}}`, msg.ID)); err == nil && !opts.suppressLoadEvent {
				// So cdpNavigateAndWait returns immediately instead of
				// burning its 30s budget waiting for a load event.
				err = write(`{"method":"Page.loadEventFired","params":{}}`)
			}
			// suppressLoadEvent: the ack above is the only reply. The loop
			// falls through to conn.Read, which blocks on the caller's ctx
			// until it disconnects — the connect-then-stall shape.
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
// conflation the sentinel exists to remove.
//
// It is also where the two predicates are pinned against each other. Chromium
// used to judge "empty" on the RAW CDP count while readFirefoxCookies judged it
// on the filtered one, so the same situation reached two users as two different
// stories. The `other sites only` rows below fail on a raw-count predicate.
func TestCdpGetCookiesTellsAnEmptyProfileFromAFailedRead(t *testing.T) {
	tests := []struct {
		name             string
		opts             stubCDPOptions
		wantEmptyProfile bool
		wantErr          bool
		wantCookie       bool
		wantErrContains  string
	}{
		{
			name:             "signed out — every tier answers with an empty jar",
			wantEmptyProfile: true,
			wantErr:          true,
		},
		{
			// The predicate row. Raw count 1, relevant count 0 — a browser used
			// for other things and never signed in to either platform. Judged
			// raw this is a success that writes a header-only cookies.txt;
			// judged the way Firefox judges it, it is an empty profile.
			name: "signed out — the profile holds only other sites' cookies",
			opts: stubCDPOptions{answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpAnswersIrrelevant,
				"Network.getAllCookies": cdpAnswersIrrelevant,
				"Network.getCookies":    cdpAnswersIrrelevant,
			}},
			wantEmptyProfile: true,
			wantErr:          true,
		},
		{
			// The ladder row. A raw-count gate stops here at tier 1 and never
			// asks the page-level tiers, so the credentials below are lost and
			// the read is then judged "empty" on an answer it should have
			// corroborated.
			name: "the ladder is not stopped by a tier that answers with other sites' cookies",
			opts: stubCDPOptions{answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpAnswersIrrelevant,
				"Network.getAllCookies": cdpAnswersCookies,
			}},
			wantCookie: true,
		},
		{
			name: "unreadable — every tier fails",
			opts: stubCDPOptions{answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpFails,
				"Network.getAllCookies": cdpFails,
				"Network.getCookies":    cdpFails,
			}},
			wantErr:         true,
			wantErrContains: "wasn't found",
		},
		{
			name: "mixed — the browser tier is missing, a page tier answers empty",
			opts: stubCDPOptions{answers: map[string]cdpAnswer{
				"Storage.getCookies": cdpFails,
			}},
			wantEmptyProfile: true,
			wantErr:          true,
			// The out-voted tier failure has to survive into the message: this
			// function has no logger, so there is nowhere else for it to go.
			wantErrContains: "wasn't found",
		},
		{
			// The ladder could not run, so "the profile is empty" is not
			// something this read is in a position to conclude — even though
			// tier 1 answered.
			name:            "the target listing fails after an empty tier 1",
			opts:            stubCDPOptions{failTargetList: true},
			wantErr:         true,
			wantErrContains: "fallback listing failed",
		},
		{
			name:       "signed in — the browser tier returns cookies",
			opts:       stubCDPOptions{answers: map[string]cdpAnswer{"Storage.getCookies": cdpAnswersCookies}},
			wantCookie: true,
		},
		{
			name: "signed in via a fallback — the browser tier is missing",
			opts: stubCDPOptions{answers: map[string]cdpAnswer{
				"Storage.getCookies":    cdpFails,
				"Network.getAllCookies": cdpAnswersCookies,
			}},
			wantCookie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := startStubCDP(t, tt.opts)

			netscape, err := cdpGetCookiesAsNetscape(context.Background(), port)
			if tt.wantErr != (err != nil) {
				t.Fatalf("cdpGetCookiesAsNetscape err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNoCookiesInProfile); got != tt.wantEmptyProfile {
				t.Errorf("errors.Is(err, ErrNoCookiesInProfile) = %v, want %v (err = %v) — "+
					"a query that ANSWERS with no YouTube/Twitch cookies is a successful read of an "+
					"empty profile, not a failed read",
					got, tt.wantEmptyProfile, err)
			}
			if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Errorf("error should name the cause %q, got %v", tt.wantErrContains, err)
			}
			if tt.wantCookie && !strings.Contains(netscape, stubCookieName) {
				t.Errorf("the extracted cookies are missing %s:\n%s", stubCookieName, netscape)
			}
			// Whatever else happens, a cookie Moombox does not keep must never
			// reach cookies.txt.
			if strings.Contains(netscape, stubOtherSiteCookie) {
				t.Errorf("an irrelevant cookie was written to the cookie file:\n%s", netscape)
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
//
// The second row is the one a real user is likelier to produce: the browser has
// been used, so the profile is not literally empty — it just holds nothing for
// YouTube or Twitch. Under a raw-count predicate that wrote a header-only
// cookies.txt and said nothing at all.
func TestFinishSetupOnAnEmptyChromiumProfileReportsNoLogin(t *testing.T) {
	profiles := map[string]stubCDPOptions{
		"nothing in the profile at all": {},
		"only other sites' cookies": {answers: map[string]cdpAnswer{
			"Storage.getCookies":    cdpAnswersIrrelevant,
			"Network.getAllCookies": cdpAnswersIrrelevant,
			"Network.getCookies":    cdpAnswersIrrelevant,
		}},
	}

	for name, opts := range profiles {
		t.Run(name, func(t *testing.T) {
			captureKills(t) // no PID from this test may reach taskkill
			port := startStubCDP(t, opts)

			cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
			s := NewAutoCookieService(t.TempDir(), cookiePath, NewCookieJar(), nopAutoCookieLogger{})
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
			// A setup that found no credentials must not leave a cookie file
			// behind implying it did.
			if _, statErr := os.Stat(cookiePath); statErr == nil {
				t.Error("an empty setup wrote a cookies.txt — a header-only credential file " +
					"reads as success to everything downstream")
			}
		})
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
