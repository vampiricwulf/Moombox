package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// The periodic check's gates, each read AFTER the check's GitHub round trip,
// so a change the operator made while it was in flight still decides: a
// release marked skipped — by the dashboard's or the TUI's Skip, or by the
// boot the launcher rolled back to — surfaces nowhere, and nor does any
// release once checks are turned off. What they guard is everything the check
// lights: the server's pending release (which the next /api/status hands every
// dashboard), the update_available frame, the TUI's badge, and the "Update
// Available" notification — which goes out once per release, not once per
// daily tick while the same release sits pending.
//
// Mutants: the skip gate compared to something else (`release.TagName ==
// skipped+"\x00"`) — a skipped release is surfaced; the toggle gate dropped
// (`if !enabled && false`) — a release found after checks were turned off is
// surfaced; the per-release dedupe dropped (`*lastNotifiedTag ==
// release.TagName+"\x00"`) — the second tick notifies again; the config read
// moved ahead of checkForUpdate — a skip or a disable made during the check is
// missed.
func TestThePeriodicCheckGates(t *testing.T) {
	log, err := logger.New(filepath.Join(t.TempDir(), "update.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	upd, err := updater.New("2.8.10", log)
	if err != nil {
		t.Fatal(err)
	}
	orig := checkForUpdate
	t.Cleanup(func() { checkForUpdate = orig })
	pendingRelease(t, nil)

	found := &updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"}
	// check runs one periodic check that finds v9.9.9; during its round trip
	// it applies inFlight to the config, as an operator's Skip or settings
	// save would.
	type outcome struct {
		pending  *updater.ReleaseInfo
		tui      []tui.UpdateStatusMsg
		notified int
		frame    string // the first frame a dashboard got after the check
	}
	check := func(t *testing.T, store *config.Store, inFlight func(*config.MoomboxConfig), lastNotified *string, rec *notificationtest.Recorder) outcome {
		t.Helper()
		routes.SharedUpdateInfo.Store(nil)
		checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) {
			if inFlight != nil {
				if err := store.Update(inFlight); err != nil {
					t.Fatalf("in-flight config change: %v", err)
				}
			}
			return found, nil
		}
		hub := web.NewWebSocketHub(sweepTestLogger{})
		next := connectDashboard(t, hub)
		ch := make(chan tui.UpdateStatusMsg, 4)
		before := len(rec.Calls())
		checkAndBroadcastUpdate(context.Background(), upd, hub, rec, ch, log, store, lastNotified)
		hub.Broadcast("sentinel", nil) // what a dashboard gets next when the check sent nothing
		var out outcome
		out.pending = routes.SharedUpdateInfo.Load()
		for len(ch) > 0 {
			out.tui = append(out.tui, <-ch)
		}
		out.notified = len(rec.ByEvent("update_available")) - before
		out.frame = next().Type
		return out
	}
	enabled := func() *config.Store {
		c := config.Defaults()
		c.Updates.AutoCheckUpdates = true
		return config.NewStore(c, "")
	}

	t.Run("a new release is surfaced, and notified once", func(t *testing.T) {
		store, rec := enabled(), notificationtest.New()
		var last string
		for tick := 1; tick <= 2; tick++ {
			out := check(t, store, nil, &last, rec)
			if out.pending != found {
				t.Errorf("tick %d: pending = %v, want v9.9.9", tick, out.pending)
			}
			if len(out.tui) != 1 || out.tui[0].TagName != "v9.9.9" || out.tui[0].Version != "9.9.9" {
				t.Errorf("tick %d: TUI got %+v, want v9.9.9's badge", tick, out.tui)
			}
			if out.frame != "update_available" {
				t.Errorf("tick %d: dashboard got %q, want update_available", tick, out.frame)
			}
			want := 0
			if tick == 1 {
				want = 1
			}
			if out.notified != want {
				t.Errorf("tick %d: %d Update Available notifications, want %d — one per release, not one per tick", tick, out.notified, want)
			}
		}
	})
	for _, tc := range []struct {
		name     string
		inFlight func(*config.MoomboxConfig)
	}{
		{"a release skipped during the check surfaces nowhere", func(c *config.MoomboxConfig) { c.Updates.SkippedVersion = "v9.9.9" }},
		{"checks turned off during the check surface nothing", func(c *config.MoomboxConfig) { c.Updates.AutoCheckUpdates = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var last string
			out := check(t, enabled(), tc.inFlight, &last, notificationtest.New())
			if out.pending != nil || len(out.tui) != 0 || out.notified != 0 || out.frame != "sentinel" {
				t.Errorf("surfaced: pending %v, TUI %+v, %d notifications, dashboard frame %q", out.pending, out.tui, out.notified, out.frame)
			}
		})
	}
}
