package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/tui"
)

func drainTimers(ch chan tui.CheckTimersMsg) []tui.CheckTimersMsg {
	var out []tui.CheckTimersMsg
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// TestTUIScheduleForwarderDecodes: the schedule and the startup seed both
// go through the decode — 0 (no channels) a zero Next, -1 the checking
// sentinel, a real instant its UnixMilli.
//
// Mutants: restore the old `if ms > 0` seed filter (the 0 and -1 seeds send
// nothing) or a raw time.UnixMilli decode (0 reads as a real instant).
func TestTUIScheduleForwarderDecodes(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixMilli()
	cases := []struct {
		name string
		ms   int64
		want time.Time
	}{
		{"no channels", 0, time.Time{}},
		{"checking", tui.MonitorCheckingSentinelMs, tui.MonitorCheckingTime()},
		{"scheduled", at, time.UnixMilli(at)},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/seed", func(t *testing.T) {
			ch := make(chan tui.CheckTimersMsg, 4)
			f := newTUIScheduleForwarder(ch, tui.MonitorTwitch)
			f.seed(func() int64 { return tc.ms })
			got := drainTimers(ch)
			if len(got) != 1 || got[0].Monitor != tui.MonitorTwitch || !got[0].Next.Equal(tc.want) {
				t.Fatalf("seed(%d) sent %+v, want one Next=%v", tc.ms, got, tc.want)
			}
		})
		t.Run(tc.name+"/schedule", func(t *testing.T) {
			ch := make(chan tui.CheckTimersMsg, 4)
			f := newTUIScheduleForwarder(ch, tui.MonitorFeed)
			f.send(tc.ms)
			got := drainTimers(ch)
			if len(got) != 1 || got[0].Monitor != tui.MonitorFeed || !got[0].Next.Equal(tc.want) {
				t.Fatalf("send(%d) sent %+v, want one Next=%v", tc.ms, got, tc.want)
			}
		})
	}
}

// TestTUIScheduleForwarderSeedNeverLandsAfterASchedule: the startup seed
// reads the monitor's value once the wrapper is installed. A check that
// finished in between had already sent its countdown, and the seed's stale
// "checking" landed after it, so the header showed "…" for a whole poll
// interval. Once the schedule has sent, the seed sends nothing.
//
// Mutant: drop the fired check in seed — the stale -1 arrives last.
func TestTUIScheduleForwarderSeedNeverLandsAfterASchedule(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixMilli()
	ch := make(chan tui.CheckTimersMsg, 4)
	f := newTUIScheduleForwarder(ch, tui.MonitorFeed)
	f.send(at)
	f.seed(func() int64 { return tui.MonitorCheckingSentinelMs }) // a stale read
	got := drainTimers(ch)
	if len(got) == 0 || !got[len(got)-1].Next.Equal(time.UnixMilli(at)) {
		t.Fatalf("last message %+v, want the schedule's countdown last", got)
	}

	// The other order: a seed first, then the schedule supersedes it.
	f2 := newTUIScheduleForwarder(ch, tui.MonitorFeed)
	f2.seed(func() int64 { return tui.MonitorCheckingSentinelMs })
	f2.send(at)
	got = drainTimers(ch)
	if len(got) != 2 || !got[1].Next.Equal(time.UnixMilli(at)) {
		t.Fatalf("seed then schedule sent %+v, want the countdown last", got)
	}
}
