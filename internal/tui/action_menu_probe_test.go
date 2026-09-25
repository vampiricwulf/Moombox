package tui

import (
	"net/http"
	"reflect"
	"strings"
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

// B1: StatusFilter answers "no jobs" from status alone, so an entry whose
// JobFilter then yields nothing is a shape the menu cannot see at open time —
// a COOKIES? or Error YouTube job with no staging directory, the common shape
// for a failure that died before any bytes landed. The entry rendered as an
// ordinary available row and Enter did NOTHING: no mode change, no feedback,
// the menu unchanged. The parent dimmed it with "· no jobs to resume".
//
// The probe has just run when that happens, so the truth the cheap filter
// could not know is known and free: the row is dimmed with its
// DisabledReason there and then, and no probe happens before selection.
//
// Mutant: dropping the write-back in handleMainKey's len(m.filtered) == 0
// branch — noJobs stays false and the rendered row carries no reason.
func TestActionMenuDeadEndEntryDimsOnEnter(t *testing.T) {
	a := NewApp()
	// The job that makes the two filters disagree: resumable by status,
	// nothing on disk to resume from.
	a.HasStagingFiles = func(string) bool { return false }
	a.HasSegmentFiles = func(string) bool { return false }
	a.actionMenu.SetSize(100, 40)
	a.actionMenu.SetJobs([]*database.Job{
		{ID: "a", Title: "t", Status: database.StatusCookies, Platform: "youtube"},
	})
	a.actionMenu.Open(a.buildMenuItems())

	idx, mi := findMenuItem(t, a, "A R")
	if mi.noJobs {
		t.Fatal("A R opened dimmed — this test needs the status-enabled/probe-empty shape to exist")
	}
	a.actionMenu.mainList.Select(idx)

	if got := a.actionMenu.HandleKey(keyEnter); got != "" {
		t.Fatalf("Enter returned %q, want \"\" — a dead-end entry must not dispatch", got)
	}
	if a.actionMenu.mode != menuMain {
		t.Fatalf("mode = %v, want menuMain — the job selector must not open on an empty list", a.actionMenu.mode)
	}

	_, after := findMenuItem(t, a, "A R")
	if !after.noJobs {
		t.Error("A R is still rendered as available after Enter found no job — the operator is left pressing a live-looking row that does nothing")
	}
	if view := a.actionMenu.View(); !strings.Contains(view, "no jobs to resume") {
		t.Errorf("the menu does not name the reason after Enter; view:\n%s", view)
	}
}

// findMenuItem returns the index and item for a chord in the open main list.
func findMenuItem(t *testing.T, a *App, chord string) (int, menuActionItem) {
	t.Helper()
	for i, it := range a.actionMenu.mainList.Items() {
		mi, ok := it.(menuActionItem)
		if ok && mi.action != nil && mi.action.Chord == chord {
			return i, mi
		}
	}
	t.Fatalf("%s was not in the menu", chord)
	return -1, menuActionItem{}
}

// B4: the counted-callback guard above pins only the two probe seams that
// exist today, so a future buildMenuItems() entry probing through a NEW
// App callback reintroduces CORE-9 with every test green. This one is
// structural: every settable func(string) bool field on App — the shape a
// per-job disk probe has — is replaced with a counter, so a new seam is
// covered the day it is added.
//
// Mutant: giving any NeedsJob entry a JobFilter that calls one of those
// callbacks without a StatusFilter beside it (executed on A I) — the count
// is non-zero.
//
// Mutant (Arc A): giving A S a JobFilter that calls JobAsides with no
// StatusFilter beside it — the count is non-zero.
func TestActionMenuOpenCallsNoProbeSeamAtAll(t *testing.T) {
	a := NewApp()
	probes := 0
	v := reflect.ValueOf(a).Elem()
	seams, sawJobAsides := 0, false
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Func || !f.CanSet() {
			continue
		}
		ft := f.Type()
		// The shape of a per-job probe: one string in (the job ID), something
		// out. Widened from func(string) bool when JobAsides — which answers
		// with a struct, not a bool — became the third such seam, so the guard
		// covers a new one by SHAPE rather than by the plan remembering to add
		// it (Arc A).
		if ft.IsVariadic() || ft.NumIn() != 1 || ft.In(0).Kind() != reflect.String || ft.NumOut() == 0 {
			continue
		}
		f.Set(reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
			probes++
			out := make([]reflect.Value, ft.NumOut())
			for j := range out {
				out[j] = reflect.Zero(ft.Out(j))
			}
			return out
		}))
		seams++
		if v.Type().Field(i).Name == "JobAsides" {
			sawJobAsides = true
		}
	}
	if seams == 0 {
		t.Fatal("no single-string-argument func callbacks found on App — re-anchor this guard rather than letting it pass vacuously")
	}
	if !sawJobAsides {
		t.Error("JobAsides was not replaced — the shape filter no longer matches it, so A S's probe is unguarded")
	}

	jobs := make([]*database.Job, 0, 200)
	for i := range 200 {
		st := database.StatusError
		switch i % 4 {
		case 1:
			st = database.StatusCancelled
		case 2:
			st = database.StatusCookies
		case 3:
			st = database.StatusFinished
		}
		jobs = append(jobs, &database.Job{
			ID: "j" + string(rune('a'+i%26)), Title: "t", Status: st, Platform: "youtube",
			IncompleteTail: i%8 == 0, OutputFile: "out.mp4",
		})
	}
	a.actionMenu.SetJobs(jobs)
	a.actionMenu.Open(a.buildMenuItems())

	if probes != 0 {
		t.Errorf("opening the menu called %d probe callback(s) across %d seam(s), want 0 — every NeedsJob entry's open-time filter must be status-only", probes, seams)
	}
}
