package tui

import (
	"net/http"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// Opening the action menu evaluated every NeedsJob filter over every job, and
// A R's / A M's filters call HasStagingFiles / HasSegmentFiles — os.ReadDir
// per eligible job, on the bubbletea event goroutine. 1,000 directory reads
// at 1,000 jobs, seconds of freeze on a NAS staging dir (CORE-9). The menu's
// "no jobs" is a STATUS question; the disk is consulted when the action is
// actually chosen.
//
// Mutant: dropping StatusFilter from the noJobs computation in Open() — the
// probe counters are non-zero.
func TestActionMenuOpenTouchesNoDisk(t *testing.T) {
	a := NewApp()
	staging, segments := 0, 0
	a.HasStagingFiles = func(string) bool { staging++; return true }
	a.HasSegmentFiles = func(string) bool { segments++; return true }

	jobs := make([]*database.Job, 0, 300)
	for i := range 300 {
		st := database.StatusError
		if i%3 == 0 {
			st = database.StatusCancelled
		}
		jobs = append(jobs, &database.Job{
			ID:       "j" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Title:    "t",
			Status:   st,
			Platform: "youtube",
		})
	}
	a.actionMenu.SetJobs(jobs)
	a.actionMenu.Open(a.buildMenuItems())

	if staging != 0 || segments != 0 {
		t.Errorf("opening the menu probed the disk: HasStagingFiles=%d HasSegmentFiles=%d, want 0/0", staging, segments)
	}
}

// The status-only twin must still disable an action that genuinely has no
// candidate — otherwise the cheap path turns every entry green.
//
// Mutant: making StatusFilter always return true.
func TestActionMenuStillDisablesImpossibleActions(t *testing.T) {
	a := NewApp()
	a.HasStagingFiles = func(string) bool { return true }
	a.HasSegmentFiles = func(string) bool { return true }
	a.actionMenu.SetJobs([]*database.Job{
		{ID: "a", Title: "t", Status: database.StatusLive, Platform: "youtube"},
	})
	a.actionMenu.Open(a.buildMenuItems())

	found := false
	for _, it := range a.actionMenu.mainList.Items() {
		mi, ok := it.(menuActionItem)
		if !ok || mi.action == nil || mi.action.Chord != "A R" {
			continue
		}
		found = true
		if !mi.noJobs {
			t.Error("A R must be disabled when no job has a resumable status")
		}
	}
	if !found {
		t.Fatal("A R was not in the menu")
	}
}

// The disk probe did not vanish — it moved to SELECTION. Enter on A R builds
// the job selector through JobFilter, which is where HasStagingFiles belongs:
// once, for the action the operator actually picked.
//
// Mutant: dropping HasStagingFiles from A R's JobFilter (or pointing the
// selector at StatusFilter) — the counter stays 0 and the selector offers a
// job with no staging directory to resume from.
func TestActionMenuProbesDiskOnSelection(t *testing.T) {
	a := NewApp()
	staging := 0
	a.HasStagingFiles = func(string) bool { staging++; return true }
	a.HasSegmentFiles = func(string) bool { return true }
	a.actionMenu.SetJobs([]*database.Job{
		{ID: "a", Title: "t", Status: database.StatusError, Platform: "youtube"},
	})
	a.actionMenu.Open(a.buildMenuItems())
	if staging != 0 {
		t.Fatalf("open probed the disk %d times, want 0", staging)
	}

	// Walk the cursor to A R and press Enter — that is the job-selector build.
	for i, it := range a.actionMenu.mainList.Items() {
		if mi, ok := it.(menuActionItem); ok && mi.action != nil && mi.action.Chord == "A R" {
			a.actionMenu.mainList.Select(i)
			break
		}
	}
	if sel := a.actionMenu.selectedAction(); sel == nil || sel.Chord != "A R" {
		t.Fatalf("cursor is not on A R: %v", sel)
	}
	a.actionMenu.HandleKey(keyEnter)

	if staging != 1 {
		t.Errorf("selecting A R probed the disk %d times, want exactly 1", staging)
	}
	if len(a.actionMenu.filtered) != 1 {
		t.Errorf("job selector holds %d jobs, want 1", len(a.actionMenu.filtered))
	}
}

// TOOL-18: the TUI's apiClient was the one hand-built &http.Transport{}
// outside internal/httpx, which declares itself the single source of truth
// for Moombox's *http.Client shapes. A loopback call that skipped the shared
// keep-alive tuning re-handshaked on every poll.
//
// Mutant: restoring `base := http.DefaultTransport` / a bare
// &http.Transport{TLSClientConfig: …} — MaxIdleConnsPerHost is 0, not 8.
func TestApiClientUsesHttpxTransport(t *testing.T) {
	want := httpx.NewTransport(httpx.TransportOptions{})

	for _, tc := range []struct {
		name  string
		https bool
	}{{"plain", false}, {"https", true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Network.HTTPSEnabled = tc.https
			a := NewApp()
			a.SetConfigStore(config.NewStore(cfg, ""))

			c := a.apiClient()
			if c.Timeout != 30*time.Second {
				t.Errorf("client timeout = %v, want 30s", c.Timeout)
			}
			tok, ok := c.Transport.(*internalTokenTransport)
			if !ok {
				t.Fatalf("transport = %T, want *internalTokenTransport", c.Transport)
			}
			base, ok := tok.base.(*http.Transport)
			if !ok {
				t.Fatalf("base transport = %T, want *http.Transport from httpx", tok.base)
			}
			if base.MaxIdleConns != want.MaxIdleConns ||
				base.MaxIdleConnsPerHost != want.MaxIdleConnsPerHost ||
				base.IdleConnTimeout != want.IdleConnTimeout ||
				base.TLSHandshakeTimeout != want.TLSHandshakeTimeout ||
				base.ExpectContinueTimeout != want.ExpectContinueTimeout ||
				base.ForceAttemptHTTP2 != want.ForceAttemptHTTP2 {
				t.Errorf("base transport is not the httpx shape: MaxIdleConns=%d/%d PerHost=%d/%d Idle=%v/%v TLSHandshake=%v/%v ExpectContinue=%v/%v HTTP2=%v/%v",
					base.MaxIdleConns, want.MaxIdleConns,
					base.MaxIdleConnsPerHost, want.MaxIdleConnsPerHost,
					base.IdleConnTimeout, want.IdleConnTimeout,
					base.TLSHandshakeTimeout, want.TLSHandshakeTimeout,
					base.ExpectContinueTimeout, want.ExpectContinueTimeout,
					base.ForceAttemptHTTP2, want.ForceAttemptHTTP2)
			}

			// The self-signed loopback certificate is still skipped on HTTPS,
			// and verification is still ON when it is off.
			skip := base.TLSClientConfig != nil && base.TLSClientConfig.InsecureSkipVerify
			if skip != tc.https {
				t.Errorf("InsecureSkipVerify = %v, want %v (TLSClientConfig=%+v)", skip, tc.https, base.TLSClientConfig)
			}

			// The base URL the client is pointed at is unchanged.
			scheme := "http"
			if tc.https {
				scheme = "https"
			}
			if got, wantURL := a.apiBaseURL(), scheme+"://127.0.0.1:774"; got != wantURL {
				t.Errorf("apiBaseURL = %q, want %q", got, wantURL)
			}
		})
	}
}
