package worker

import (
	"context"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// playbackWaitProgress opens the progress line a YouTube download shows while
// it waits out the pre-roll ad of the player response its URLs came from
// (waitForPlayback), as a job waiting out an outage shows
// networkWaitProgress; playbackWaitLine adds the time left.
const playbackWaitProgress = "Waiting out YouTube's pre-roll ad"

// playbackWaitRefresh is how often the line's time left is rewritten. Each
// rewrite is a row write and a broadcast, and a pre-roll wait is seconds
// long, so a few are enough.
const playbackWaitRefresh = 5 * time.Second

// playbackWaitLine is the progress line itself, "Waiting out YouTube's
// pre-roll ad... (12s left)". The "... (" is the TUI's wait marker
// (internal/tui isWaitProgress), which every wait line carries.
func playbackWaitLine(left time.Duration) string {
	return playbackWaitProgress + "... (" + utils.FormatDurationHuman(left) + " left)"
}

// latestPlayback returns the latest AvailableAt among the formats a download
// is about to fetch (nil ones skipped), and the client of the format that set
// it — upstream's max(f.get('available_at') or 0 for f in requested_formats),
// video and audio alike, since the download starts both together.
func latestPlayback(formats ...*youtube.Format) (at time.Time, source string) {
	for _, f := range formats {
		if f != nil && f.AvailableAt.After(at) {
			at, source = f.AvailableAt, f.Source
		}
	}
	return at, source
}

// formatByItag returns the first format carrying itag, or nil — the entry
// resolveFormatURLByItag resolves and formatSourceByItag names.
func formatByItag(formats []youtube.Format, itag int) *youtube.Format {
	for i := range formats {
		if formats[i].Itag == itag {
			return &formats[i]
		}
	}
	return nil
}

// playbackWaitLeft is how long a download of a stream in status must still
// wait at now before requesting media that is available from at: upstream's
// `available_at - int(time.time())`, so the second now falls in counts whole,
// as on upstream's integer clock. Zero when nothing waits: a live or
// post-live stream (youtube.PlaybackWaitApplies), or an instant already past
// — which no ad, the zero time, always is.
func playbackWaitLeft(status youtube.StreamStatus, at, now time.Time) time.Duration {
	if !youtube.PlaybackWaitApplies(status) {
		return 0
	}
	return max(at.Sub(now.Truncate(time.Second)), 0)
}

// waitForPlayback holds a YouTube download's first media request until the
// player response its URLs came from allows one: the downloader half of
// yt-dlp's available_at ("Sleeping N seconds as required by the site",
// downloader/common.py), the extraction half being youtube.Format.AvailableAt.
// A signed-in non-Premium account's player response carries a pre-roll ad,
// and every URL it yields is refused with a 403 until the ad has played or
// could have been skipped.
//
// Each YouTube strategy calls it last, once its URLs are chosen and its
// manifest (if any) fetched — upstream's sleep comes after the extraction,
// manifest fetches included, and before real_download — so the engine's
// first request (the VOD size probe, the first segment, the HLS variant
// playlist) is made after it. The row's progress line says what it waits for
// and how long is left, cleared however the wait ends, and one Info line
// names the seconds and the client. Nothing to wait for does nothing and
// logs nothing. Returns ctx's error when ctx ends first — a cancel or a
// shutdown — which the strategy hands back as its setup error.
func waitForPlayback(ctx context.Context, job *JobContext, status youtube.StreamStatus, at time.Time, source, tag string) error {
	now := time.Now()
	left := playbackWaitLeft(status, at, now)
	if left <= 0 {
		return nil
	}
	end := now.Add(left)
	job.Logger.Info("waiting out the player response's pre-roll ad before the first media request, as YouTube requires",
		"jobID", job.Job.ID, "tag", tag, "seconds", left.Seconds(), "client", source)
	job.DB.UpdateJobFields(job.Job.ID, map[string]any{"progress": playbackWaitLine(left)})
	defer job.DB.UpdateJobFields(job.Job.ID, map[string]any{"progress": ""})
	done := time.NewTimer(left)
	defer done.Stop()
	refresh := time.NewTicker(playbackWaitRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-done.C:
			return nil
		case <-refresh.C:
			job.DB.UpdateJobFields(job.Job.ID, map[string]any{"progress": playbackWaitLine(time.Until(end))})
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForFreshPlayback is the same wait for a URL a credential refresh took
// from a FRESH player response, f being the format it resolved: the response
// carries a pre-roll ad of its own, and its URL is refused until that ad is
// over exactly as the first one was — the field case was three fresh tv_auth
// URLs in a row, each refused at once. It waits on ctx before the URL is
// handed to the engine, logging one Info line; the progress line is the
// download's own while it runs, so it is left alone. Reports false — the URL
// must not be installed — when ctx ends first, or when ctx's deadline falls
// before the wait would end, in which case it does not wait at all (a Warn
// says so): a refresh bound too tightly for the ad keeps the current URL, as
// a refresh that failed would.
func waitForFreshPlayback(ctx context.Context, job *JobContext, status youtube.StreamStatus, f *youtube.Format, tag string) bool {
	if f == nil {
		return true
	}
	left := playbackWaitLeft(status, f.AvailableAt, time.Now())
	if left <= 0 {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < left {
		job.Logger.Warn("credential refresh: the fresh player response's pre-roll ad outlasts the refresh budget — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "seconds", left.Seconds(), "client", f.Source)
		return false
	}
	job.Logger.Info("credential refresh: waiting out the fresh player response's pre-roll ad before using its URL, as YouTube requires",
		"jobID", job.Job.ID, "tag", tag, "seconds", left.Seconds(), "client", f.Source)
	return utils.Sleep(ctx, left) == nil
}
