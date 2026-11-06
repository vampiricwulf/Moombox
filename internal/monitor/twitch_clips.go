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
// only, with a monitor opt-in"). A channel whose archive_clips is on has its
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

// clipSweeper is the sweep's state: when each channel was last listed, and
// whether a sweep is running (one at a time, off the poll's goroutine).
type clipSweeper struct {
	mu       sync.Mutex
	sweptAt  map[string]time.Time
	sweeping atomic.Bool
}

// clipFilterFor is the time filter a channel's sweep asks for. The first
// sweep of a channel in this process asks for the last week (LAST_WEEK, the
// filter yt-dlp's clip page defaults to): it covers a restart, or the
// install being down, for up to a week. Every later one, at most
// twitchClipSweepEvery after the last, asks for the last day (LAST_DAY),
// which overlaps it many times over; the dedupe by slug makes the overlap
// free.
func clipFilterFor(swept bool) twitch.ClipFilter {
	if swept {
		return twitch.ClipFilterLastDay
	}
	return twitch.ClipFilterLastWeek
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

// clipChannelsOf is the channels with archive_clips on.
func clipChannelsOf(channels []config.ChannelConfig) []config.ChannelConfig {
	var out []config.ChannelConfig
	for _, ch := range channels {
		if ch.ArchiveClips {
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
// and the failure is logged, at Warn for a re-hashed op, whose message says
// what to do.
func (tm *TwitchMonitor) sweepClips(ctx context.Context, channels []config.ChannelConfig, now time.Time) {
	for i := range channels {
		if ctx.Err() != nil {
			return
		}
		ch := &channels[i]
		tm.clips.mu.Lock()
		if tm.clips.sweptAt == nil {
			tm.clips.sweptAt = map[string]time.Time{}
		}
		last, swept := tm.clips.sweptAt[ch.ID]
		due := !swept || now.Sub(last) >= twitchClipSweepEvery
		if due {
			tm.clips.sweptAt[ch.ID] = now
		}
		tm.clips.mu.Unlock()
		if !due {
			continue
		}

		filter := clipFilterFor(swept)
		cards, err := tm.listClips(ctx, ch.ID, filter)
		if err != nil {
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
