package engine

import (
	"context"
	"time"
)

const (
	// proactiveRefreshLead is how long before a URL's own expiry the timer
	// renews it: well inside what a renewal takes (a player request, a
	// re-resolve, a re-mint, each bounded by the caller's refresh timeout),
	// so the renewed URL is in place before any segment is refused.
	proactiveRefreshLead = 30 * time.Minute
	// proactiveRefreshEvery is the longest a downloader goes without a
	// renewal, whatever its URL says: yt-dlp's url_feed re-extracts every
	// 18000 s during a live-from-start download, refused segments or not
	// (extractor/youtube/_video.py _live_adaptive_fragments, from adbc4ec4b
	// #888). It is the whole cadence for a URL that names no expiry.
	proactiveRefreshEvery = 5 * time.Hour
	// proactiveRefreshRetry is the wait after a renewal that installed no
	// new URL — the callback failed, kept the URL on purpose, or the
	// cooldown held it (a 403 recovery had just run) — before the timer
	// asks again. Every ask is a player request, so it is not hurried; past
	// the expiry the 403 recovery paths take over in any case.
	proactiveRefreshRetry = 5 * time.Minute
)

// proactiveRefreshDue is when a downloader whose URL is url, last renewed at
// renewed, renews it next: proactiveRefreshLead before the URL's own expiry
// (expiry, false when it names none), and never later than
// proactiveRefreshEvery after the last renewal — min(expire − 30 min, 5 h),
// audit G48.
func proactiveRefreshDue(renewed time.Time, url string, expiry func(string) (time.Time, bool)) time.Time {
	due := renewed.Add(proactiveRefreshEvery)
	if exp, ok := expiry(url); ok {
		if t := exp.Add(-proactiveRefreshLead); t.Before(due) {
			due = t
		}
	}
	return due
}

// runProactiveRefresh renews the downloader's credentials before its URL
// expires (audit G48), on its own goroutine beside the download loop, until
// ctx ends — Start cancels it when the loop returns. Ported from yt-dlp's
// url_feed, which re-extracts the player response on a timer during a live
// download rather than waiting to be refused (_video.py
// _prepare_live_from_start_formats / _live_adaptive_fragments). Moombox went
// reactive only: a URL that expired below the head was renewed by the 403
// recovery (refreshCredentials), but at the live edge that recovery never
// opens — behindHeadTailPending needs a head the expired URL no longer
// learns — so an expiry there cost a gone burst, a status check,
// ErrQualityLost and a full downloader rebuild, and the cipher failure
// callback invalidated the shared solver on the way for nothing. Moombox
// renews a little earlier than upstream when the URL says so: at
// min(expiry − proactiveRefreshLead, last renewal + proactiveRefreshEvery),
// where yt-dlp waits out its 5 h whatever expire= says.
//
// Each renewal calls OnProactiveRefresh under the cooldown the 403 recovery
// uses (installCredentials), so the two never run back to back, and installs
// its answer through SetBaseURL/SetPoToken as that recovery does — nothing in
// flight is interrupted. A renewal that installs no new URL is asked again
// proactiveRefreshRetry later; a URL the recovery has swapped in meanwhile is
// not renewed on top of it when the timer next fires — the cadence restarts
// there, the swapped URL's own expiry setting the next renewal.
func (d *SegmentDownloader) runProactiveRefresh(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("[Downloader] proactive credential renewal panicked; the 403 recovery paths remain", "panic", r)
		}
	}()
	seen := d.getBaseURL()
	renewed := time.Now()
	var retryAt time.Time
	for {
		due := proactiveRefreshDue(renewed, seen, d.opts.URLExpiry)
		if due.Before(retryAt) {
			due = retryAt
		}
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if d.isCancelled() {
			return
		}
		if cur := d.getBaseURL(); cur != seen {
			// The 403 recovery renewed the URL since this wait began: the
			// cadence starts over here, on the URL it installed.
			seen, renewed, retryAt = cur, time.Now(), time.Time{}
			continue
		}
		d.installCredentials(d.opts.OnProactiveRefresh, "[Downloader] credentials renewed before the URL expires")
		if cur := d.getBaseURL(); cur != seen {
			seen, renewed, retryAt = cur, time.Now(), time.Time{}
			continue
		}
		d.logger.Warn("[Downloader] proactive credential renewal installed no new URL — asking again shortly",
			"retryIn", proactiveRefreshRetry)
		retryAt = time.Now().Add(proactiveRefreshRetry)
	}
}
