package tui

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// A hide_finished_age_days change made from the DASHBOARD has no TUI-side
// event at all: before this the TUI re-read the threshold only when its own
// settings overlay was opened and closed, so the two UIs disagreed about
// which Finished jobs are archived indefinitely (CORE-11). The 60 s archive
// sweep now re-reads it.
//
// Mutant: making syncHideFinishedAge a no-op (or comparing against the
// config the App booted with instead of the store) — the list keeps the boot
// threshold.
func TestSyncHideFinishedAgePicksUpAWebSideChange(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 30}
	store := config.NewStore(cfg, "")

	a := NewApp()
	a.SetConfigStore(store)
	a.SetConfig(cfg)
	if got := a.taskList.HideFinishedAgeDays(); got != 30 {
		t.Fatalf("boot threshold = %v, want 30", got)
	}

	// The dashboard's PUT writes through the store.
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	a.syncHideFinishedAge()
	if got := a.taskList.HideFinishedAgeDays(); got != 0.5 {
		t.Errorf("after the sweep the list threshold = %v, want 0.5", got)
	}
}

// The sweep is the call site that makes the Web -> TUI direction automatic:
// the operator never has to open the settings overlay, and the latency is
// bounded by the once-a-minute archive sweep the list already runs.
//
// Mutant: deleting the a.syncHideFinishedAge() call from the tickMsg sweep —
// the row stays in the active section forever even though the dashboard has
// long since archived it.
func TestTheArchiveSweepRereadsTheThreshold(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 30}
	store := config.NewStore(cfg, "")

	a := NewApp()
	a.SetConfigStore(store)
	a.SetConfig(cfg)
	a.taskList.SetSize(60, 12)
	a.taskList.SetJobs([]*database.Job{{
		ID: "a", Title: "THIRTEEN HOURS OLD", Status: database.StatusFinished,
		UpdatedAt: time.Now().Add(-13 * time.Hour).Format(time.RFC3339),
	}})
	if a.taskList.archivedSet["a"] {
		t.Fatal("a thirteen-hour-old Finished job is active under a 30-day threshold")
	}

	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	// The sweep fires on the first tick after a minute; a fresh App has
	// never swept, so this tick is that one.
	a.Update(tickMsg{})

	if got := a.taskList.HideFinishedAgeDays(); got != 0.5 {
		t.Fatalf("the sweep did not re-read the threshold: %v, want 0.5", got)
	}
	if !a.taskList.archivedSet["a"] {
		t.Error("the row must be archived once the sweep picks up the dashboard's 12-hour threshold")
	}
}

// The sync must leave the list exactly as SetHideFinishedAgeDays does — a
// changed threshold ends in a rebuild, so the memoised frame cannot outlive
// it (spec §5 / CORE-2), and an UNCHANGED threshold must not rebuild at all,
// which is what keeps a once-a-minute store read off the render path.
//
// Mutant: calling SetHideFinishedAgeDays unconditionally — every sweep
// rebuilds the virtual list and moves rebuildSeq, invalidating a cache that
// had nothing to invalidate.
func TestSyncHideFinishedAgeRebuildsOnlyOnAChange(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 30}
	store := config.NewStore(cfg, "")

	a := NewApp()
	a.SetConfigStore(store)
	a.SetConfig(cfg)

	before := a.taskList.rebuildSeq
	a.syncHideFinishedAge()
	if a.taskList.rebuildSeq != before {
		t.Error("an unchanged threshold must not rebuild the list")
	}

	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}
	a.syncHideFinishedAge()
	if a.taskList.rebuildSeq == before {
		t.Error("a changed threshold must end in a rebuild so the render cache key moves")
	}
}

// With no store wired (every Set* is optional until cmd/moombox calls it,
// and the tests above are not the only App constructed without one) the sync
// is a no-op rather than a nil dereference on the once-a-second tick.
//
// Mutant: dropping the nil guard — the tick panics for any App built without
// SetConfigStore.
func TestSyncHideFinishedAgeWithoutAStoreIsANoOp(t *testing.T) {
	a := NewApp()
	before := a.taskList.HideFinishedAgeDays()
	a.syncHideFinishedAge()
	if got := a.taskList.HideFinishedAgeDays(); got != before {
		t.Errorf("threshold moved without a config store: %v -> %v", before, got)
	}
}
