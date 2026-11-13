package main

import (
	"maps"
	"sync"
	"sync/atomic"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// channelMonitorsPush tells both UIs when the set of failing channels
// changes (owner ruling R-H1): the dashboard as a channel_monitors frame
// carrying every monitor's health list, from which it derives the flag with
// its mirror of monitor.ChannelFailing, and the TUI as the derived set.
//
// Nothing here is state of record. `last` only remembers what was pushed, so
// that a monitor cycle that changed no channel's flag sends nothing; the flag
// itself is derived from the monitors' health on every read (snapshot,
// failing).
type channelMonitorsPush struct {
	snapshot func() monitor.ChannelMonitors
	hub      holdsBroadcaster

	// tui receives each new failing set while the TUI runs; nil without one.
	tui atomic.Pointer[func(tui.FailingChannels)]

	// mu serialises check, so two monitors finishing a cycle together push
	// their snapshots in the order they were read.
	mu   sync.Mutex
	last tui.FailingChannels
}

// failingChannelsOf applies the one predicate (monitor.FailingChannels) to
// both platforms and returns each failing channel's newest error, keyed as
// the TUI's channel list looks it up.
func failingChannelsOf(c monitor.ChannelMonitors) tui.FailingChannels {
	out := tui.FailingChannels{}
	for id, h := range monitor.FailingChannels(c.YouTube) {
		out[tui.FailingChannelKey("youtube", id)] = h.LastError
	}
	for id, h := range monitor.FailingChannels(c.Twitch) {
		out[tui.FailingChannelKey("twitch", id)] = h.LastError
	}
	return out
}

// failing is the current failing set, derived now.
func (p *channelMonitorsPush) failing() tui.FailingChannels {
	return failingChannelsOf(p.snapshot())
}

// check derives the failing set and pushes it when it differs from the last
// one pushed: a channel started or stopped failing, or its newest error
// changed. Called after every monitor cycle, from the monitors' goroutines.
func (p *channelMonitorsPush) check() {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.snapshot()
	now := failingChannelsOf(snap)
	if maps.Equal(now, p.last) {
		return
	}
	p.last = now
	if p.hub != nil {
		p.hub.Broadcast("channel_monitors", snap)
	}
	if fn := p.tui.Load(); fn != nil {
		(*fn)(maps.Clone(now))
	}
}

// channelMonitors is each monitor's health list by platform: the additive
// channelMonitors field of /api/status and initial_state, and the
// channel_monitors frame's payload.
func (s *runState) channelMonitors() monitor.ChannelMonitors {
	return monitor.ChannelMonitors{
		YouTube: [][]monitor.ChannelHealth{s.feedMon.Health(), s.decapiMon.Health()},
		Twitch:  [][]monitor.ChannelHealth{s.twitchMon.Health()},
	}
}
