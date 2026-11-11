package main

import (
	"encoding/json"
	"go/token"
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// cookieCheckParityFile is the table both implementations of "the first
// cookie check has not answered" run against: cookies.AuthStatus.Unchecked
// here, utils.js cookieCheckPending in web/tests/cookie-badge.test.mjs.
const cookieCheckParityFile = "../../web/tests/fixtures/cookie-check-parity.json"

// TestCookieCheckPendingParity runs the Go half of the fixture (owner ruling
// R-H2, CLAUDE.md State machines rule 2). Each row's status is the wire
// payload; the Go side builds the AuthStatus that produces it, checks that
// CookieStatusPayload really emits those keys, and then asks the one
// predicate and the TUI's badge projection.
//
// Mutants: Unchecked returning false (the first row reports no-cookies);
// cookieBadgesFor without its Unchecked arm (the first row's badge is "none").
func TestCookieCheckPendingParity(t *testing.T) {
	raw, err := os.ReadFile(cookieCheckParityFile)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string `json:"name"`
			Status struct {
				Checked       *bool  `json:"checked"`
				Found         bool   `json:"found"`
				Authenticated bool   `json:"authenticated"`
				Verification  string `json:"verification"`
			} `json:"status"`
			Pending bool   `json:"pending"`
			Badge   string `json:"badge"`
			JSOnly  bool   `json:"jsOnly"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]cookies.RefreshVerdict{
		"ok": cookies.RefreshOK, "failed": cookies.RefreshFailed, "unknown": cookies.RefreshUnknown,
	}
	badgeNames := map[tui.CookieStatus]string{
		tui.CookieStatusChecking:    "checking",
		tui.CookieStatusNone:        "none",
		tui.CookieStatusOK:          "ok",
		tui.CookieStatusCookiesOnly: "rejected",
		tui.CookieStatusUnknown:     "unknown",
	}
	ran := 0
	for _, c := range fixture.Cases {
		if c.JSOnly {
			if c.Status.Checked != nil {
				t.Errorf("%s: a jsOnly row is one Go cannot produce, so it has no `checked` key", c.Name)
			}
			continue
		}
		if c.Status.Checked == nil {
			t.Errorf("%s: no `checked` key; mark the row jsOnly", c.Name)
			continue
		}
		verdict, ok := verdicts[c.Status.Verification]
		if !ok {
			t.Errorf("%s: verification %q", c.Name, c.Status.Verification)
			continue
		}
		ran++
		auth := cookies.AuthStatus{
			Checked:              *c.Status.Checked,
			HasYouTubeCookies:    c.Status.Found,
			YouTubeAuthenticated: c.Status.Authenticated,
			YouTubeVerification:  verdict,
		}
		payload := routes.CookieStatusPayload(auth)
		if payload["checked"] != *c.Status.Checked || payload["found"] != c.Status.Found ||
			payload["authenticated"] != c.Status.Authenticated || payload["verification"] != c.Status.Verification {
			t.Errorf("%s: CookieStatusPayload = %v, which is not the row's status", c.Name, payload)
		}
		if tw := routes.TwitchAuthStatusPayload(auth); tw["checked"] != *c.Status.Checked {
			t.Errorf("%s: TwitchAuthStatusPayload checked = %v", c.Name, tw["checked"])
		}
		if got := auth.Unchecked(); got != c.Pending {
			t.Errorf("%s: Unchecked() = %v, want %v", c.Name, got, c.Pending)
		}
		yt, _ := cookieBadgesFor(auth, nil)
		if got := badgeNames[yt]; got != c.Badge {
			t.Errorf("%s: badge = %q (%v), want %q", c.Name, got, yt, c.Badge)
		}
	}
	if ran < 5 {
		t.Errorf("only %d rows ran against Go", ran)
	}
}

// TestUncheckedStatusShowsNeitherNoCookiesNorRelogin is the ruling's own
// sentence for the TUI: while the first check has not answered, both badges
// read Checking, whatever the relogin flags say, and once it has answered the
// flags apply as before.
func TestUncheckedStatusShowsNeitherNoCookiesNorRelogin(t *testing.T) {
	relogin := map[string]bool{"youtube": true, "twitch": true}

	yt, tw := cookieBadgesFor(cookies.AuthStatus{}, relogin)
	if yt != tui.CookieStatusChecking || tw != tui.CookieStatusChecking {
		t.Errorf("unchecked with relogin flags: yt=%v tw=%v, want Checking for both", yt, tw)
	}
	yt, tw = cookieBadgesFor(cookies.AuthStatus{}, nil)
	if yt != tui.CookieStatusChecking || tw != tui.CookieStatusChecking {
		t.Errorf("unchecked: yt=%v tw=%v, want Checking for both (not None)", yt, tw)
	}

	checked := cookies.AuthStatus{Checked: true}
	yt, tw = cookieBadgesFor(checked, nil)
	if yt != tui.CookieStatusNone || tw != tui.CookieStatusNone {
		t.Errorf("checked, nothing configured: yt=%v tw=%v, want None", yt, tw)
	}
	yt, tw = cookieBadgesFor(checked, map[string]bool{"twitch": true})
	if yt != tui.CookieStatusNone || tw != tui.CookieStatusRelogin {
		t.Errorf("checked with a Twitch relogin flag: yt=%v tw=%v", yt, tw)
	}
}

// TestDashboardBindsBeforeTheFirstCookieCheck pins run()'s order (owner
// ruling R-H2 and its amendment): the web server is started, and its bind
// waited for (resolveWebBind, which waits with awaitWebBind), before the cookie startup sequence, so the dashboard is not
// held behind two auth round trips; the seeds and the no-cookie-file mark
// come before the bind, so nothing a dashboard request can trigger observes
// ahead of them; and the sequence is started before runTUI, which no longer
// waits for it either. That the sequence is one goroutine holding the first
// check, the profile seed and the refresh timer in that order is
// TestCookieStartupIsOneGoroutine's.
//
// Mutants: move startCookieSequence back above the web block, or below
// s.runTUI(); move SetUnrecoveredPlatforms or SkipStartupCheck below it.
func TestDashboardBindsBeforeTheFirstCookieCheck(t *testing.T) {
	run, fset := callsInFunc(t, "main.go", "run")
	order := []struct{ recv, name string }{
		{"dlWorker", "Start"},
		{"cookieRefresh", "SetUnrecoveredPlatforms"},
		{"cookieRefresh", "SkipStartupCheck"},
		{"webServer", "Start"},
		{"s", "resolveWebBind"},
		{"", "startCookieSequence"},
		{"upd", "CleanupOldBinary"},
		{"s", "runTUI"},
		{"s", "shutdown"},
	}
	prev := token.NoPos
	for i, c := range order {
		pos := firstCall(run, c.recv, c.name)
		if pos == token.NoPos {
			t.Fatalf("run(): no call to %s.%s", c.recv, c.name)
		}
		if i > 0 && pos < prev {
			t.Errorf("run(): %s.%s (%s) comes before %s.%s", c.recv, c.name, fset.Position(pos), order[i-1].recv, order[i-1].name)
		}
		prev = pos
	}
}
