package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// pollUnderTest is one monitor driven only through its own methods, so the
// pins below hold for the lifecycle whichever struct carries it.
type pollUnderTest struct {
	start        func(context.Context)
	stop         func()
	checkNow     func()
	next         func() int64
	scheduleNext func(context.Context, time.Time)
}

// warnLog collects Warn messages.
type warnLog struct {
	silentLogger
	mu    sync.Mutex
	warns []string
}

func (w *warnLog) Warn(msg string, _ ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, msg)
}

func (w *warnLog) count(sub string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.warns {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// pollMonitors builds each of the three monitors with store, schedule and
// online hooks, and a logger.
func pollMonitors(store *config.Store, onSchedule func(int64), online func() bool, log *warnLog) map[string]pollUnderTest {
	fm := &FeedMonitor{logger: log, configStore: store, health: newHealthTracker(), ProbeCooldown: NewProbeCooldown(0),
		OnSchedule: onSchedule, IsOnline: online, MetadataTracker: NewMetadataFailureTracker(), now: time.Now}
	dm := &DecapiMonitor{logger: log, configStore: store, health: newHealthTracker(), ProbeCooldown: NewProbeCooldown(0),
		OnSchedule: onSchedule, IsOnline: online}
	tm := &TwitchMonitor{logger: log, configStore: store, health: newHealthTracker(), OnSchedule: onSchedule, IsOnline: online}
	return map[string]pollUnderTest{
		"feed":   {fm.Start, fm.Stop, fm.CheckNow, fm.GetNextCheckAt, fm.scheduleNext},
		"decapi": {dm.Start, dm.Stop, dm.CheckNow, dm.GetNextCheckAt, dm.scheduleNext},
		"twitch": {tm.Start, tm.Stop, tm.CheckNow, tm.GetNextCheckAt, tm.scheduleNext},
	}
}

func channelStore(t *testing.T, kind string) *config.Store {
	if kind == "twitch" {
		return storeWithOneTwitchChannel(t)
	}
	return storeWithOneYouTubeChannel(t)
}

// waitSchedule waits until the probe has seen n published values.
func waitSchedule(t *testing.T, p *reArmProbe, n int) []int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := p.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnSchedule saw %v, want at least %d values", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPollLoop_StartPublishesCheckingThenCountdown pins a cycle's countdown:
// -1 ("checking") while it runs, then a future time that GetNextCheckAt
// answers too. Stop zeroes it, and a CheckNow after Stop runs nothing.
func TestPollLoop_StartPublishesCheckingThenCountdown(t *testing.T) {
	for _, kind := range []string{"feed", "decapi", "twitch"} {
		t.Run(kind, func(t *testing.T) {
			p := &reArmProbe{}
			m := pollMonitors(channelStore(t, kind), p.onSchedule, p.offline, &warnLog{})[kind]
			t.Cleanup(m.stop)
			before := time.Now().UnixMilli()
			m.start(t.Context())
			got := waitSchedule(t, p, 2)
			if got[0] != -1 {
				t.Errorf("first published value = %d, want -1 (checking)", got[0])
			}
			if got[1] <= before || got[1] > before+int64(2*time.Hour/time.Millisecond) {
				t.Errorf("countdown = %d, want a time after the cycle (start %d)", got[1], before)
			}
			if n := m.next(); n != got[1] {
				t.Errorf("GetNextCheckAt = %d, want the published %d", n, got[1])
			}
			m.stop()
			if n := m.next(); n != 0 {
				t.Errorf("GetNextCheckAt after Stop = %d, want 0", n)
			}
			m.checkNow()
			time.Sleep(50 * time.Millisecond)
			if after, entries := p.snapshot(); len(after) != 2 || entries != 1 {
				t.Errorf("after Stop+CheckNow: published %v, cycles %d — want nothing new", after, entries)
			}
		})
	}
}

// TestPollLoop_NoChannelsPublishesZero pins the empty list: -1 while the
// cycle runs, then 0 ("nothing to poll"), with no timer behind it.
func TestPollLoop_NoChannelsPublishesZero(t *testing.T) {
	for _, kind := range []string{"feed", "decapi", "twitch"} {
		t.Run(kind, func(t *testing.T) {
			p := &reArmProbe{}
			m := pollMonitors(storeWithNoChannels(t), p.onSchedule, p.offline, &warnLog{})[kind]
			t.Cleanup(m.stop)
			m.start(t.Context())
			got := waitSchedule(t, p, 2)
			if got[0] != -1 || got[1] != 0 {
				t.Errorf("published %v, want [-1 0]", got)
			}
			if n := m.next(); n != 0 {
				t.Errorf("GetNextCheckAt = %d, want 0", n)
			}
		})
	}
}

// TestPollLoop_FixedRateFloorAndSlowWarn pins fixed-RATE scheduling: the
// delay is the interval minus the cycle's elapsed time, floored at 1 s, and
// a cycle that ran over twice the interval warns once.
func TestPollLoop_FixedRateFloorAndSlowWarn(t *testing.T) {
	for _, kind := range []string{"feed", "decapi", "twitch"} {
		t.Run(kind, func(t *testing.T) {
			p := &reArmProbe{}
			log := &warnLog{}
			m := pollMonitors(channelStore(t, kind), p.onSchedule, p.offline, log)[kind]
			t.Cleanup(m.stop)
			m.start(t.Context())
			waitSchedule(t, p, 2)

			for range 2 {
				now := time.Now().UnixMilli()
				m.scheduleNext(t.Context(), time.Now().Add(-48*time.Hour))
				n := m.next()
				if n < now+500 || n > now+2000 {
					t.Errorf("countdown after an overlong cycle = now%+dms, want the 1 s floor", n-now)
				}
			}
			if c := log.count("takes far longer than"); c != 1 {
				t.Errorf("slow-cycle warn logged %d times, want once", c)
			}
		})
	}
}

// TestPollLoop_KickDuringACycleRunsOnceMore pins pendingKick: CheckNow calls
// landing while a cycle runs are latched into ONE re-run, which publishes
// its own -1 and then the countdown.
func TestPollLoop_KickDuringACycleRunsOnceMore(t *testing.T) {
	for _, kind := range []string{"feed", "decapi", "twitch"} {
		t.Run(kind, func(t *testing.T) {
			p := &reArmProbe{}
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			var once sync.Once
			online := func() bool {
				p.offline()
				entered <- struct{}{}
				once.Do(func() { <-release })
				return false
			}
			m := pollMonitors(channelStore(t, kind), p.onSchedule, online, &warnLog{})[kind]
			t.Cleanup(m.stop)
			m.start(t.Context())
			<-entered // the first cycle is inside its check
			m.checkNow()
			m.checkNow()
			time.Sleep(50 * time.Millisecond) // both kicks have reached the latch
			close(release)
			waitSchedule(t, p, 3)
			time.Sleep(50 * time.Millisecond)
			got, entries := p.snapshot()
			if entries != 2 {
				t.Errorf("cycles run = %d, want 2 (the first, then one re-run for both kicks)", entries)
			}
			if len(got) != 3 || got[0] != -1 || got[1] != -1 || got[2] <= 0 {
				t.Errorf("published %v, want [-1 -1 <time>]", got)
			}
		})
	}
}
