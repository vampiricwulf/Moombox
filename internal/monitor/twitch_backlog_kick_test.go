package monitor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// kickBacklog is a started TwitchBacklog over one opted-in channel whose
// cycles are counted: each cycle lists that channel once and sends on cycles.
type kickBacklog struct {
	b      *TwitchBacklog
	store  *config.Store
	online atomic.Bool
	cycles chan struct{}
}

func newKickBacklog(t *testing.T, firstDelay, gap time.Duration) *kickBacklog {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{{ID: "streamer", Platform: "twitch", TwitchVodBacklog: true}}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))

	k := &kickBacklog{store: store, cycles: make(chan struct{}, 16)}
	k.online.Store(true)
	k.b = NewTwitchBacklog(store, db, nil, &testMonitorLogger{})
	k.b.ListVideos = func(context.Context, string) ([]twitch.TwitchVideo, error) {
		k.cycles <- struct{}{}
		return nil, nil
	}
	k.b.IsOnline = k.online.Load
	k.b.firstDelay, k.b.kickGap, k.b.interval = firstDelay, gap, time.Hour
	return k
}

func (k *kickBacklog) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k.b.Start(ctx)
}

func (k *kickBacklog) wantCycle(t *testing.T, what string) {
	t.Helper()
	select {
	case <-k.cycles:
	case <-time.After(5 * time.Second):
		t.Fatalf("no cycle: %s", what)
	}
}

func (k *kickBacklog) wantNoCycle(t *testing.T, d time.Duration, what string) {
	t.Helper()
	select {
	case <-k.cycles:
		t.Fatalf("a cycle ran: %s", what)
	case <-time.After(d):
	}
}

// Turning twitch_vod_backlog on kicks the loop (kickMonitors calls CheckNow):
// with the hourly tick an hour away, the cycle runs now.
// Mutant: no wake case in the loop — nothing runs until the tick.
func TestTwitchBacklogCheckNowRunsACycle(t *testing.T) {
	k := newKickBacklog(t, 10*time.Millisecond, time.Nanosecond)
	k.start(t)
	k.wantCycle(t, "the first scheduled one")
	k.b.CheckNow()
	k.wantCycle(t, "the kicked one, an hour before the next tick")
}

// A burst of saves is one cycle now and one when the gap has passed, not one
// per save.
// Mutants: no gap (a cycle per kick); a kick inside the gap dropped rather
// than kept (no second cycle).
func TestTwitchBacklogKicksAreSpaced(t *testing.T) {
	const gap = time.Second
	k := newKickBacklog(t, 10*time.Millisecond, gap)
	k.start(t)
	k.wantCycle(t, "the first scheduled one")
	begun := time.Now()
	for range 5 {
		k.b.CheckNow()
	}
	k.wantCycle(t, "the kick kept until the gap had passed")
	if took := time.Since(begun); took < gap/2 {
		t.Errorf("the kicked cycle ran %v after the last one, inside the %v gap", took, gap)
	}
	k.wantNoCycle(t, 100*time.Millisecond, "five kicks made more than one cycle")
}

// A tick that lands offline is owed: CheckOwed, which the connectivity handler
// calls on the way back, runs it at once, although the next tick is an hour
// away. With nothing owed it runs nothing, so a flapping network costs no
// request.
// Mutants: reset to a full hour and forget — nothing runs after the restore;
// CheckOwed that runs whether or not a cycle is owed — the second restore
// lists again.
func TestTwitchBacklogOfflineTickRunsOnRestore(t *testing.T) {
	k := newKickBacklog(t, 10*time.Millisecond, time.Hour)
	k.online.Store(false)
	k.start(t)
	k.wantNoCycle(t, 150*time.Millisecond, "while offline")
	k.b.CheckNow() // a save while offline changes nothing
	k.wantNoCycle(t, 50*time.Millisecond, "kicked while offline")

	k.online.Store(true)
	k.b.CheckOwed()
	k.wantCycle(t, "the owed cycle, on the restore")

	k.b.CheckOwed()
	k.wantNoCycle(t, 150*time.Millisecond, "a restore with no cycle owed")
}

// A kick before the first scheduled cycle does not pull it into the boot
// burst, and CheckNow on a backlog built by hand does not block.
func TestTwitchBacklogKickBeforeFirstCycleIsDropped(t *testing.T) {
	k := newKickBacklog(t, time.Second, time.Nanosecond)
	k.start(t)
	k.b.CheckNow()
	k.b.CheckOwed()
	k.wantNoCycle(t, 100*time.Millisecond, "ahead of the first delay")
	k.wantCycle(t, "the first scheduled one")

	(&TwitchBacklog{}).CheckNow()
	(&TwitchBacklog{}).CheckOwed()
}

// A cycle that found no channel opted in sent nothing, so it does not start
// the gap: turning twitch_vod_backlog on right after it lists the channel at
// once (the boot cycle and every hourly tick are such cycles on an install
// that has the backlog off, and every config save kicks).
// Mutant: run sets last for every cycle — the kick waits out the hour-long gap.
func TestTwitchBacklogEmptyCycleDoesNotStartTheGap(t *testing.T) {
	k := newKickBacklog(t, 10*time.Millisecond, time.Hour)
	if err := k.store.Update(func(c *config.MoomboxConfig) { c.Channels[0].TwitchVodBacklog = false }); err != nil {
		t.Fatal(err)
	}
	k.start(t)
	k.wantNoCycle(t, 200*time.Millisecond, "no channel is opted in")
	k.b.CheckNow() // an unrelated save
	k.wantNoCycle(t, 50*time.Millisecond, "no channel is opted in")

	if err := k.store.Update(func(c *config.MoomboxConfig) { c.Channels[0].TwitchVodBacklog = true }); err != nil {
		t.Fatal(err)
	}
	k.b.CheckNow()
	k.wantCycle(t, "the switch was turned on: listed at once, not a gap later")
}

// A kick waiting out the gap is satisfied by the hourly tick when that comes
// first: the timer it armed does not run a second cycle minutes later.
// Mutant: run leaves the gap timer armed — a cycle follows the tick's by
// gap-interval.
func TestTwitchBacklogTickSettlesAWaitingKick(t *testing.T) {
	k := newKickBacklog(t, 10*time.Millisecond, 2*time.Second)
	k.b.interval = 1500 * time.Millisecond
	k.start(t)
	k.wantCycle(t, "the first scheduled one")
	k.b.CheckNow() // waits for the gap: due 2 s after the first cycle
	k.wantCycle(t, "the tick, 1.5 s after the first cycle")
	k.wantNoCycle(t, 800*time.Millisecond, "the waiting kick ran its own cycle after the tick's")
}
