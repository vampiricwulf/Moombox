package main

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// warnCounter counts Warn lines; everything else is dropped.
type warnCounter struct {
	mu    sync.Mutex
	warns []string
}

func (w *warnCounter) Debug(msg string, args ...any) {}
func (w *warnCounter) Info(msg string, args ...any)  {}
func (w *warnCounter) Error(msg string, args ...any) {}
func (w *warnCounter) Warn(msg string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, msg)
}

func (w *warnCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.warns)
}

// restart loads the state file the way the next process would.
func restart(t *testing.T, path string) *openAlerts {
	t.Helper()
	return loadOpenAlerts(path, &nopLogger{})
}

// TestOpenAlertsMissingOrCorruptMeansNothingOpen: a missing or unreadable state
// file means no alert is open, said with ONE Warn, and is replaced by an empty
// file so the next start reads it silently.
//
// Mutant: skip the replacement write after a missing file (the second start
// warns again).
func TestOpenAlertsMissingOrCorruptMeansNothingOpen(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), openAlertsFileName)
		log := &warnCounter{}
		a := loadOpenAlerts(path, log)
		if log.count() != 1 {
			t.Errorf("Warns = %d, want exactly 1 for a missing file", log.count())
		}
		if got := a.snapshot(); got.Disk != nil || got.SidecarDown || got.Channels != nil || got.Auth != nil {
			t.Errorf("state = %+v, want nothing open", got)
		}
		again := &warnCounter{}
		loadOpenAlerts(path, again)
		if again.count() != 0 {
			t.Errorf("the next start warned %d times — the empty file was not written", again.count())
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), openAlertsFileName)
		if err := os.WriteFile(path, []byte(`{"sidecarDown": true, "disk": {`), 0o644); err != nil {
			t.Fatal(err)
		}
		log := &warnCounter{}
		a := loadOpenAlerts(path, log)
		if log.count() != 1 {
			t.Errorf("Warns = %d, want exactly 1 for a corrupt file", log.count())
		}
		if got := a.snapshot(); got.SidecarDown || got.Disk != nil {
			t.Errorf("state = %+v, want nothing open", got)
		}
	})
}

// TestOpenAlertsDropsWhatNothingCanClose: at boot, a channel no longer
// configured — removed, disabled, or moved to the other platform — and the
// sidecar's outage while the sidecar is off are dropped; everything else
// stays.
//
// Mutants: keep disabled channels (the disabled one survives); drop the
// sidecar clause (the outage survives with the sidecar off).
func TestOpenAlertsDropsWhatNothingCanClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)
	a := restart(t, path)
	a.update(func(d *openAlertsDoc) {
		d.SidecarDown = true
		for _, id := range []string{"UC_kept", "UC_removed", "UC_disabled"} {
			d.setChannel("youtube", id, true)
		}
		d.setChannel("twitch", "kept_login", true)
		d.setChannel("twitch", "UC_kept", true) // a YouTube ID under Twitch
	})

	off := false
	cfg := &config.MoomboxConfig{Channels: []config.ChannelConfig{
		{ID: "UC_kept"},
		{ID: "UC_disabled", Enabled: &off},
		{ID: "kept_login", Platform: "twitch"},
	}}
	cfg.Bgutils.UseSidecar = false
	a.dropUnmonitored(cfg)

	got := restart(t, path).snapshot()
	if !slices.Equal(got.Channels["youtube"], []string{"UC_kept"}) {
		t.Errorf("youtube channels = %v, want [UC_kept]", got.Channels["youtube"])
	}
	if !slices.Equal(got.Channels["twitch"], []string{"kept_login"}) {
		t.Errorf("twitch channels = %v, want [kept_login]", got.Channels["twitch"])
	}
	if got.SidecarDown {
		t.Error("the sidecar outage survived with the sidecar turned off — nothing can ever close it")
	}
}

// TestDiskAlertOpenAcrossARestartGetsItsClose: a space alert and a monitoring
// failure open when the process stops are closed by the next process's first
// reading that clears them, and a repeat inside the cooldown stays quiet
// across the restart as it would have without one. Once closed, the next
// start finds nothing open.
//
// Mutants: drop restoreFrom's level restore (no "Disk Space Recovered"); drop
// its MonitoringFailed restore (no "Disk Monitoring Recovered"); drop the
// persist from onReading (the next process finds nothing to close); restore
// the level without its time (the repeat after the restart is sent at once).
func TestDiskAlertOpenAcrossARestartGetsItsClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)
	now := time.Now()

	rec := notificationtest.New()
	first := newDiskAlerts(rec, &nopLogger{})
	first.restoreFrom(restart(t, path))
	first.setThresholds(90, 95)
	first.onReading(diskReading("critical", 97), "./output", now)
	first.onReadFailure("./output")
	first.onReadFailure("./output")
	if got := len(rec.ByEvent("disk_critical")); got != 1 {
		t.Fatalf("the first process sent %d criticals, want 1", got)
	}

	rec = notificationtest.New()
	second := newDiskAlerts(rec, &nopLogger{})
	second.restoreFrom(restart(t, path))
	second.setThresholds(90, 95)
	second.onReading(diskReading("critical", 97), "./output", now.Add(5*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 0 {
		t.Errorf("a repeat inside the 30-minute cooldown was sent after the restart (%d)", got)
	}
	if got := len(rec.Calls()); got != 1 || rec.Calls()[0].Title != "Disk Monitoring Recovered" {
		t.Fatalf("calls = %+v, want only the monitoring close", rec.Calls())
	}
	second.onReading(diskReading("ok", 50), "./output", now.Add(6*time.Minute))
	if got := rec.ByEvent("disk_ok"); len(got) != 2 || got[1].Title != "Disk Space Recovered" {
		t.Fatalf("disk_ok calls = %+v, want the monitoring close then the space close", got)
	}

	if open := restart(t, path).snapshot().Disk; open != nil {
		t.Errorf("after both closes the next start reads %+v, want nothing open", open)
	}
}

// TestSidecarOutageOpenAcrossARestartGetsItsClose: "BotGuard Sidecar Down"
// sent by one process is closed by the next process's first healthy snapshot,
// and an unhealthy one arms no second alert.
//
// Mutants: drop the persist from fireDown (the next process finds nothing to
// close); drop restoreFrom's seeding (no close, and a second down alert);
// drop the persist from the restore path (the start after that closes it
// again).
func TestSidecarOutageOpenAcrossARestartGetsItsClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)

	a, rec, clk := newTestSidecarAlerts(t)
	a.restoreFrom(restart(t, path))
	a.onHealth(sidecar.Health{Healthy: false, Reason: "spawn failed"})
	clk.last(t).fire()
	if got := len(rec.ByEvent("sidecar_down")); got != 1 {
		t.Fatalf("the first process sent %d sidecar_down, want 1", got)
	}

	b, rec, clk := newTestSidecarAlerts(t)
	b.restoreFrom(restart(t, path))
	b.onHealth(sidecar.Health{Healthy: false, Reason: "still failing"})
	if len(clk.timers) != 0 {
		t.Error("an unhealthy snapshot armed a second down alert for an outage already announced")
	}
	b.onHealth(sidecar.Health{Healthy: true, Restarts: 2})
	if got := len(rec.ByEvent("sidecar_restored")); got != 1 {
		t.Fatalf("sent %d sidecar_restored, want 1 for the outage the previous process announced", got)
	}

	c, rec, _ := newTestSidecarAlerts(t)
	c.restoreFrom(restart(t, path))
	c.onHealth(sidecar.Health{Healthy: true})
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("the start after the close sent %d calls, want none", got)
	}
}

// TestChannelOutageOpenAcrossARestartGetsItsClose: a channel alert one process
// sent is restored into the platform's incident set — and handed back for the
// monitors' trackers — and the first healthy callback in the next process
// sends its close. Each platform keeps its own set.
//
// Mutants: drop the persist from the unhealthy path (nothing to restore);
// drop it from the healthy path (the start after the close closes it again);
// restore into the set without returning the IDs (the trackers are never
// seeded — the returned list is asserted).
func TestChannelOutageOpenAcrossARestartGetsItsClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)

	rec := notificationtest.New()
	yt := newChannelIncidents()
	yt.restoreFrom(restart(t, path), "youtube")
	unhealthy, _ := channelHealthNotifiers(rec, &nopLogger{}, "youtube", yt)
	unhealthy("UC_dead", 20, "404")
	tw := newChannelIncidents()
	tw.restoreFrom(restart(t, path), "twitch")
	twUnhealthy, _ := channelHealthNotifiers(rec, &nopLogger{}, "twitch", tw)
	twUnhealthy("gone_login", 20, "not found")

	rec = notificationtest.New()
	store := restart(t, path)
	yt2 := newChannelIncidents()
	if ids := yt2.restoreFrom(store, "youtube"); !slices.Equal(ids, []string{"UC_dead"}) {
		t.Fatalf("restored youtube channels = %v, want [UC_dead]", ids)
	}
	tw2 := newChannelIncidents()
	if ids := tw2.restoreFrom(store, "twitch"); !slices.Equal(ids, []string{"gone_login"}) {
		t.Fatalf("restored twitch channels = %v, want [gone_login]", ids)
	}
	_, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", yt2)
	healthy("UC_dead")
	if got := rec.ByEvent("channel_healthy"); len(got) != 1 || got[0].Type != notifications.TypeSuccess {
		t.Fatalf("channel_healthy calls = %+v, want the one close", got)
	}

	got := restart(t, path).snapshot().Channels
	if len(got["youtube"]) != 0 || !slices.Equal(got["twitch"], []string{"gone_login"}) {
		t.Errorf("after the YouTube close the next start reads %v, want only the Twitch outage", got)
	}
}

// TestAuthFailureOpenAcrossARestartGetsItsClose: an auth failure one process
// announced comes back with its stamp — so the close fires on the next
// process's recovery, and a repeat inside the 30-minute window stays quiet —
// and the close clears it from the file.
//
// Mutants: drop the seeding from the stored stamps (wasNotified is false
// after the restart, no close); drop the persist from notify (nothing to
// restore); drop it from wasNotified (the start after the close closes again).
func TestAuthFailureOpenAcrossARestartGetsItsClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)
	sent := 0
	send := func(string, string, string, notifications.NotificationType) { sent++ }

	notify, _ := withPersistedAuthFailureCooldown(send, restart(t, path))
	notify("youtube", "Cookie Auto-Refresh Failed", "dead", notifications.TypeError)
	if sent != 1 {
		t.Fatalf("sent %d, want 1", sent)
	}

	store := restart(t, path)
	if got := store.authPlatforms(); !slices.Equal(got, []string{"youtube"}) {
		t.Fatalf("open auth platforms after the restart = %v, want [youtube]", got)
	}
	notify2, wasNotified := withPersistedAuthFailureCooldown(send, store)
	notify2("youtube", "Cookie Auto-Refresh Failed", "dead", notifications.TypeError)
	if sent != 1 {
		t.Errorf("a repeat inside the cooldown was sent after the restart")
	}
	if !wasNotified("youtube") {
		t.Fatal("the failure the previous process announced is not seen as announced — its close never fires")
	}
	if got := restart(t, path).authPlatforms(); len(got) != 0 {
		t.Errorf("after the close the next start reads %v open", got)
	}
}
