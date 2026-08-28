package cookies

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
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

			gotAll, gotFailures := navigateAllPlatforms(context.Background(), 9222, tt.platforms, navigate)
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
			// Every platform is still attempted: one failure must not skip the
			// sibling, or a YouTube outage would silently stop refreshing
			// Twitch.
			if len(visited) != len(tt.platforms) {
				t.Errorf("navigated %d URLs, want one per platform (%d)", len(visited), len(tt.platforms))
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
