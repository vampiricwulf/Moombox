package cookies

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestNavigateAllPlatformsFoldsEveryFailure is the Chromium half of the hole
// the Firefox screenshot check closed.
//
// refreshChromium's navigation errors were discarded. The only thing consulted
// afterwards was whether the cookie READ succeeded — and that read is satisfied
// by a profile the previous session already populated — so a pass whose
// navigations ALL failed was still credited as having renewed the credentials,
// which is what browserActed downstream turns into Renewed.
//
// The fold is an AND, matching refreshFirefox's allActed: one navigation that
// could not be confirmed leaves the pass unable to claim it renewed that
// platform, and nothing downstream of the merge can tell the difference
// afterwards. Note the shape — false says the pass has no proof, not that the
// browser did nothing.
func TestNavigateAllPlatformsFoldsEveryFailure(t *testing.T) {
	boom := errors.New("no page target found")

	tests := []struct {
		name         string
		platforms    []string
		failOn       map[string]bool
		wantAll      bool
		wantFailures []string
	}{
		{
			name:      "every navigation succeeded",
			platforms: []string{"youtube", "twitch"},
			wantAll:   true,
		},
		{
			name:         "one of two failed",
			platforms:    []string{"youtube", "twitch"},
			failOn:       map[string]bool{"twitch": true},
			wantAll:      false,
			wantFailures: []string{"twitch"},
		},
		{
			// The live defect: this pass proved nothing at all, and used to
			// report a renewal.
			name:         "every navigation failed",
			platforms:    []string{"youtube", "twitch"},
			failOn:       map[string]bool{"youtube": true, "twitch": true},
			wantAll:      false,
			wantFailures: []string{"youtube", "twitch"},
		},
		{
			// Vacuous truth is the wrong default: nothing navigated is not
			// "every navigation succeeded".
			name:      "no platforms",
			platforms: nil,
			wantAll:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var visited []string
			navigate := func(_ context.Context, _ int, target string) error {
				visited = append(visited, target)
				for platform, refreshURL := range platformRefreshURLs {
					if refreshURL == target && tt.failOn[platform] {
						return boom
					}
				}
				return nil
			}

			wantVisited := make([]string, 0, len(tt.platforms))
			for _, platform := range tt.platforms {
				wantVisited = append(wantVisited, platformRefreshURLs[platform])
			}

			gotAll, gotFailures, gotExhausted := navigateAllPlatforms(context.Background(), 9222, tt.platforms, navigate)
			if gotAll != tt.wantAll {
				t.Errorf("allNavigated = %v, want %v — a pass with no proof it navigated "+
					"must not be credited with renewing the credentials", gotAll, tt.wantAll)
			}
			if len(gotFailures) != len(tt.wantFailures) {
				t.Fatalf("failures = %v, want %v", gotFailures, tt.wantFailures)
			}
			for i, want := range tt.wantFailures {
				if gotFailures[i].platform != want {
					t.Errorf("failures[%d].platform = %q, want %q", i, gotFailures[i].platform, want)
				}
				if !errors.Is(gotFailures[i].err, boom) {
					t.Errorf("failures[%d].err = %v, want the navigate error passed through", i, gotFailures[i].err)
				}
			}
			// Every platform is attempted, each exactly once, in order. The
			// WHICH matters as much as the how-many: one failure must not skip
			// the sibling (a YouTube outage would silently stop refreshing
			// Twitch), and a loop that visited one platform twice would satisfy
			// a bare count.
			if !slices.Equal(visited, wantVisited) {
				t.Errorf("navigated %v, want exactly %v", visited, wantVisited)
			}
			// None of these rows produce a budget-exhaustion outcome --
			// that fold has its own table, TestNavigateAllPlatformsFoldsBudgetExhaustion.
			if len(gotExhausted) != 0 {
				t.Errorf("exhausted = %v, want none -- this table only exercises success/failure", gotExhausted)
			}
		})
	}
}

// TestCdpNavigateReportsAMissingPageTarget establishes the premise the fold
// above depends on: the error being folded is real and reachable, not a return
// value that is always nil.
//
// A headless Chromium with no page target is the ordinary case this hits — the
// user closed every tab, or the browser came up without restoring one — and it
// is exactly the state cdpEnsurePageTarget exists to repair on the extraction
// path. Without this row, the AND could be folding a constant.
func TestCdpNavigateReportsAMissingPageTarget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		// A browser-level target only: no "page" entry to drive.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"browser","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/x"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	if err := cdpNavigate(context.Background(), port, "https://www.youtube.com"); err == nil {
		t.Fatal("cdpNavigate returned nil with no page target to navigate — " +
			"the verdict downstream would be folding a value that is always nil")
	}
}

// TestCdpNavigateAndWaitReturnsSentinelOnBudgetExhaustion is the connect-then-
// stall case Arc 8 7(e) decided: a Chromium that accepts the CDP connection,
// answers Page.enable and the Page.navigate ack, and then never fires
// Page.loadEventFired must not be indistinguishable from a page that merely
// took a moment to load.
//
// Never launches a real browser — startStubCDP (autocookies_chromium_threestate_test.go)
// is the fake-websocket-server this test extends via suppressLoadEvent. The
// short-lived ctx, not cdpNavigateTimeout, is what makes the budget expire
// quickly; the production 30s constant is untouched (see the Traps in the
// Arc 8 task 11 brief).
//
// Standing mutation check: revert the exhaustion branch in cdpNavigateAndWait
// to `return nil` and this test fails, because errors.Is(nil, ...) is false.
func TestCdpNavigateAndWaitReturnsSentinelOnBudgetExhaustion(t *testing.T) {
	port := startStubCDP(t, stubCDPOptions{suppressLoadEvent: true})
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/stub", port)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := cdpNavigateAndWait(ctx, wsURL, "https://www.youtube.com")
	if !errors.Is(err, errNavigateBudgetExhausted) {
		t.Fatalf("cdpNavigateAndWait = %v, want errors.Is(err, errNavigateBudgetExhausted) — "+
			"a connect-then-stall browser must be distinguishable from a genuine transport failure, "+
			"not string-matched", err)
	}
}

// TestNavigateAllPlatformsFoldsBudgetExhaustion is 7(e)'s bounded rule: a
// platform whose navigation only exhausted its read budget is "not observed",
// not a failure, and it costs the returned bool (what becomes browserActed at
// the call site) only when EVERY platform ends that way. One slow page beside
// a platform that fired its load event is not connect-then-stall — the
// browser demonstrably works — and must still read as acted.
//
// Standing mutation check: loosen the "every platform" fold in
// navigateAllPlatforms to "any platform" and the second case below (one
// exhausted, one loaded) flips from true to false.
func TestNavigateAllPlatformsFoldsBudgetExhaustion(t *testing.T) {
	transportErr := errors.New("CDP connect: EOF")

	tests := []struct {
		name      string
		outcomes  map[string]error // platform -> the error navigate returns; nil = loaded
		wantActed bool
	}{
		{
			name: "every platform exhausted its budget",
			outcomes: map[string]error{
				"youtube": errNavigateBudgetExhausted,
				"twitch":  errNavigateBudgetExhausted,
			},
			// Connect-then-stall: nothing was confirmed on either platform,
			// so there is no proof the browser did anything.
			wantActed: false,
		},
		{
			name: "one exhausted, the other fired its load event",
			outcomes: map[string]error{
				"youtube": nil,
				"twitch":  errNavigateBudgetExhausted,
			},
			// A slow page next to a working one — the browser demonstrably
			// works, so this is not connect-then-stall.
			wantActed: true,
		},
		{
			name: "one exhausted, the other a genuine transport failure",
			outcomes: map[string]error{
				"youtube": transportErr,
				"twitch":  errNavigateBudgetExhausted,
			},
			// Lands false, but via the PRE-EXISTING P2 rule (a real
			// navigation error always fails the fold) — not because of the
			// new exhaustion rule. The sibling's exhaustion plays no part in
			// this verdict.
			wantActed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platforms := []string{"youtube", "twitch"}
			navigate := func(_ context.Context, _ int, target string) error {
				for platform, refreshURL := range platformRefreshURLs {
					if refreshURL == target {
						return tt.outcomes[platform]
					}
				}
				return nil
			}

			gotActed, gotFailures, gotExhausted := navigateAllPlatforms(context.Background(), 9222, platforms, navigate)
			if gotActed != tt.wantActed {
				t.Errorf("navigateAllPlatforms acted = %v, want %v", gotActed, tt.wantActed)
			}
			// A budget-exhaustion outcome must never be reported as a
			// navFailure — it did not fail, it was merely not observed, and
			// refreshChromium logs the two lists at different levels (Warn
			// vs Debug) because of that distinction.
			for _, f := range gotFailures {
				if errors.Is(f.err, errNavigateBudgetExhausted) {
					t.Errorf("budget exhaustion on %q was folded into navFailures — "+
						"it is not a failure, see the Debug-vs-Warn split in refreshChromium", f.platform)
				}
			}
			wantExhaustedCount := 0
			for _, err := range tt.outcomes {
				if errors.Is(err, errNavigateBudgetExhausted) {
					wantExhaustedCount++
				}
			}
			if len(gotExhausted) != wantExhaustedCount {
				t.Errorf("exhausted = %v, want %d platform(s) named", gotExhausted, wantExhaustedCount)
			}
		})
	}
}
