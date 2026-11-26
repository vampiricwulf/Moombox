package monitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// The Twitch monitor's clip sweep (owner decision D26 follow-up 2: "paste
// only, with a monitor opt-in"). A channel whose twitch_archive_clips is on has its
// new clips listed through ClipsCards__User (twitch.API.ListClips, yt-dlp's
// TwitchVideosClipsIE) and each clip no job has been made for handed to
// OnClipFound, which queues it as a Queued backlog job the archive-slots
// scheduler paces. Off — the default — the channel is never listed, and a
// clip reaches Moombox only when it is pasted.

// twitchClipSweepEvery is how often an opted-in channel's clips are listed.
// The live poll runs every 15 s because a broadcast's first minutes are lost
// for good if it is late; a clip is a finished file that stays up, so an
// hour costs nothing but latency and keeps the paging (up to 50 requests a
// channel) off Twitch's GQL budget.
const twitchClipSweepEvery = time.Hour

// ClipListFunc lists one channel's clips created within filter — the
// twitch.Service.ListClips seam, which tests replace (TwitchMonitor.FetchClips).
type ClipListFunc func(ctx context.Context, login string, filter twitch.ClipFilter) ([]twitch.TwitchClipCard, error)

// clipSweeper is the sweep's state: when each channel's listing was last
// tried (sweptAt, the hourly pacing) and when one last came back whole
// (listedAt, what the next listing must reach back to), and whether a sweep
// is running (one at a time, off the poll's goroutine).
type clipSweeper struct {
	mu       sync.Mutex
	sweptAt  map[string]time.Time
	listedAt map[string]time.Time
	sweeping atomic.Bool
}

// clipDayFilterReach is how long ago a channel's last whole listing may be
// for the next one to ask for the last day only: under the day LAST_DAY
// covers, by an hour's margin — one sweep's pacing — so a sweep that runs a
// little late, or a clock a little off Twitch's, still overlaps the last.
const clipDayFilterReach = 23 * time.Hour

// clipFilterFor is the time filter a channel's sweep asks for: the narrowest
// that reaches back to its last whole listing. A channel never listed whole
// in this process asks for the last week (LAST_WEEK, the filter yt-dlp's
// clip page defaults to): it covers a restart, or the install being down,
// for up to a week. So does one that left the opted-in set and came back,
// which the poll forgot (forgetClipChannelsExcept): a fresh opt-in. One listed within the day — every sweep but the first,
// while Twitch answers — asks for the last day (LAST_DAY), which overlaps the
// last listing many times over; the dedupe by slug makes the overlap free.
// One whose last whole listing is older — the listings failed for a day (an
// outage, a re-hashed op until Moombox is updated), or the poll did not run
// (offline, a machine asleep), never the operator switching it off — asks for the week, then the month, so the
// clips made in the gap are listed when the listing comes back rather than
// lost to a LAST_DAY that no longer reaches them. Past a month it stays at
// LAST_MONTH: a first sweep never reaches past a week, and a gap that long
// is not taken as a reason to archive the channel's whole history.
func clipFilterFor(listedAt time.Time, listed bool, now time.Time) twitch.ClipFilter {
	if !listed {
		return twitch.ClipFilterLastWeek
	}
	switch gap := now.Sub(listedAt); {
	case gap <= clipDayFilterReach:
		return twitch.ClipFilterLastDay
	case gap <= 7*24*time.Hour-time.Hour:
		return twitch.ClipFilterLastWeek
	default:
		return twitch.ClipFilterLastMonth
	}
}

// listClips is the injectable seam: FetchClips when a test has wired one,
// else the real client.
func (tm *TwitchMonitor) listClips(ctx context.Context, login string, filter twitch.ClipFilter) ([]twitch.TwitchClipCard, error) {
	if tm.FetchClips != nil {
		return tm.FetchClips(ctx, login, filter)
	}
	return tm.tw.ListClips(ctx, login, filter)
}

// startClipSweep runs sweepClips on a goroutine of its own, unless one is
// still running: the live poll that calls it must never wait on a channel's
// paging, and two sweeps at once would list the same channels twice.
func (tm *TwitchMonitor) startClipSweep(ctx context.Context, channels []config.ChannelConfig) {
	if tm.OnClipFound == nil || len(channels) == 0 || !tm.clips.sweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer tm.clips.sweeping.Store(false)
		defer func() {
			if r := recover(); r != nil {
				tm.logger.Error("twitch clip sweep panic", "panic", r)
			}
		}()
		tm.sweepClips(ctx, channels, time.Now())
	}()
}

// forgetClipChannelsExcept drops the sweep state (sweptAt, listedAt) of every
// channel not in channels, the ones opted in now. A channel that left the set
// — its twitch_archive_clips switched off, the channel disabled or removed —
// and comes back is then a fresh opt-in, whose first sweep asks for the last
// week as after a restart (owner ruling R4-6), not a month's catch-up for
// the time the operator had chosen not to archive its clips. doCheck calls it
// on every online poll, ahead of its own return for no enabled Twitch channel
// and of startClipSweep's for no opted-in one, so the last opted-in channel
// leaving is seen too. An outage does not prune: doCheck returns before it
// while offline, so the catch-up for a real gap stays.
func (tm *TwitchMonitor) forgetClipChannelsExcept(channels []config.ChannelConfig) {
	keep := make(map[string]bool, len(channels))
	for _, ch := range channels {
		keep[ch.ID] = true
	}
	tm.clips.mu.Lock()
	defer tm.clips.mu.Unlock()
	for id := range tm.clips.sweptAt {
		if !keep[id] {
			delete(tm.clips.sweptAt, id)
		}
	}
	for id := range tm.clips.listedAt {
		if !keep[id] {
			delete(tm.clips.listedAt, id)
		}
	}
}

// clipChannelsOf is the channels with twitch_archive_clips on.
func clipChannelsOf(channels []config.ChannelConfig) []config.ChannelConfig {
	var out []config.ChannelConfig
	for _, ch := range channels {
		if ch.TwitchArchiveClips {
			out = append(out, ch)
		}
	}
	return out
}

// sweepClips lists each opted-in channel due a sweep (twitchClipSweepEvery
// since its last, or never swept) and hands OnClipFound every clip no job has
// been made for: one whose ID ("tw_clip_<slug>") is neither in the processing
// history (a job was made, even one since deleted) nor a job now (a pasted
// clip). A channel whose listing fails is marked swept all the same — a
// re-hashed op fails every hour until Moombox is updated, not every 15 s —
// but not listed: its last whole listing stays where it was, so the next
// sweep's filter still reaches back to it (clipFilterFor). The failure is
// logged, at Warn for a re-hashed op, whose message says what to do.
func (tm *TwitchMonitor) sweepClips(ctx context.Context, channels []config.ChannelConfig, now time.Time) {
	for i := range channels {
		if ctx.Err() != nil {
			return
		}
		ch := &channels[i]
		tm.clips.mu.Lock()
		if tm.clips.sweptAt == nil {
			tm.clips.sweptAt = map[string]time.Time{}
			tm.clips.listedAt = map[string]time.Time{}
		}
		last, swept := tm.clips.sweptAt[ch.ID]
		due := !swept || now.Sub(last) >= twitchClipSweepEvery
		if due {
			tm.clips.sweptAt[ch.ID] = now
		}
		listedAt, listed := tm.clips.listedAt[ch.ID]
		tm.clips.mu.Unlock()
		if !due {
			continue
		}

		filter := clipFilterFor(listedAt, listed, now)
		cards, err := tm.listClips(ctx, ch.ID, filter)
		if err == nil {
			// Only while the channel's pacing mark is still this sweep's: a
			// poll that saw the channel leave the opted-in set during the
			// listing forgot it (forgetClipChannelsExcept), and writing the
			// listing back would undo that.
			tm.clips.mu.Lock()
			if at, ok := tm.clips.sweptAt[ch.ID]; ok && at.Equal(now) {
				tm.clips.listedAt[ch.ID] = now
			}
			tm.clips.mu.Unlock()
		} else {
			var changed *twitch.ClipAPIChangedError
			if errors.As(err, &changed) {
				tm.logger.Warn("twitch clip sweep failed: "+changed.Error(), "channel", ch.ID)
			} else {
				tm.logger.Debug("twitch clip sweep failed", "channel", ch.ID, "err", err)
			}
		}
		queued := 0
		for _, card := range cards {
			jobID := twitch.ClipJobID(card.Slug)
			processed, hpErr := tm.db.HasProcessed(jobID)
			if hpErr != nil {
				tm.logger.Debug("HasProcessed query failed", "jobID", jobID, "err", hpErr)
				continue
			}
			if processed || tm.db.JobExists(jobID) {
				continue
			}
			tm.OnClipFound(card, ch)
			queued++
		}
		tm.logger.Debug("twitch clip sweep", "channel", ch.ID, "filter", string(filter), "clips", len(cards), "new", queued)
	}
}
