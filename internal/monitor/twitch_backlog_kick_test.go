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

	k := &kickBacklog{cycles: make(chan struct{}, 16)}
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
	const gap = 400 * time.Millisecond
	k := newKickBacklog(t, 10*time.Millisecond, gap)
	k.start(t)
	k.wantCycle(t, "the first scheduled one")
	begun := time.Now()
	for range 5 {
		k.b.CheckNow()
		time.Sleep(5 * time.Millisecond)
	}
	k.wantCycle(t, "the kick kept until the gap had passed")
	if took := time.Since(begun); took < gap/2 {
		t.Errorf("the kicked cycle ran %v after the last one, inside the %v gap", took, gap)
	}
	k.wantNoCycle(t, 100*time.Millisecond, "five kicks made more than one cycle")
}

// A tick that lands offline is owed: the kick the connectivity handler sends
// on the way back runs it at once, although the gap has not passed and the
// next tick is an hour away.
// Mutant: reset to a full hour and forget — nothing runs after the restore.
func TestTwitchBacklogOfflineTickRunsOnRestore(t *testing.T) {
	k := newKickBacklog(t, 10*time.Millisecond, time.Hour)
	k.online.Store(false)
	k.start(t)
	k.wantNoCycle(t, 150*time.Millisecond, "while offline")
	k.b.CheckNow() // a save while offline changes nothing
	k.wantNoCycle(t, 50*time.Millisecond, "kicked while offline")

	k.online.Store(true)
	k.b.CheckNow()
	k.wantCycle(t, "the owed cycle, on the restore")
}

// A kick before the first scheduled cycle does not pull it into the boot
// burst, and CheckNow on a backlog built by hand does not block.
func TestTwitchBacklogKickBeforeFirstCycleIsDropped(t *testing.T) {
	k := newKickBacklog(t, 300*time.Millisecond, time.Nanosecond)
	k.start(t)
	k.b.CheckNow()
	k.wantNoCycle(t, 100*time.Millisecond, "ahead of the first delay")
	k.wantCycle(t, "the first scheduled one")

	(&TwitchBacklog{}).CheckNow()
}
