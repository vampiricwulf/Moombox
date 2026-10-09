package worker

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// vodSlotWaitProgress is the progress line a VOD shows while it queues for a
// download slot (acquireDownloadSlot). The row keeps the status it came in
// with — Upcoming for a VOD stream processing just classified — because
// nothing is downloading yet; ExecuteWithChat writes Downloading once the
// slot is held.
const vodSlotWaitProgress = "Waiting for a download slot..."

// vodInfoMaxAge is how old a VOD's extraction may be when its download
// starts. Stream processing extracts the format URLs BEFORE the download-slot
// wait, and that wait has no bound: behind a busy pool it outlasts the ~6 h a
// googlevideo URL lives, and the whole-file download then fails on its first
// request with a 403 nothing retries. An hour is well inside that lifetime,
// so a download that starts on an older extraction re-extracts first
// (refreshStaleVodInfo).
const vodInfoMaxAge = time.Hour

// expirePathRe reads a googlevideo URL's expiry in its path form
// (.../expire/<unix>/...), which some clients serve instead of the query one.
var expirePathRe = regexp.MustCompile(`/expire/(\d+)(?:/|$)`)

// formatURLExpiry returns the unix expiry googlevideo stamps on a format URL
// (its expire= parameter, or the /expire/<unix>/ path segment), and false
// when the URL carries none — a signatureCipher format before resolution, or
// any URL that is not googlevideo's.
func formatURLExpiry(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	v := u.Query().Get("expire")
	if v == "" {
		if m := expirePathRe.FindStringSubmatch(u.Path); m != nil {
			v = m[1]
		}
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// vodInfoStale reports whether a VOD's extraction must be refreshed before
// its download starts at now: it is vodInfoMaxAge old, or one of its format
// URLs says it expires within vodInfoMaxAge (the URL's own word beats the
// age, which only assumes the usual lifetime).
func vodInfoStale(info *youtube.VideoInfo, extractedAt, now time.Time) bool {
	if info == nil {
		return false
	}
	if now.Sub(extractedAt) >= vodInfoMaxAge {
		return true
	}
	for _, f := range info.Formats {
		if exp, ok := formatURLExpiry(f.URL); ok && exp.Sub(now) < vodInfoMaxAge {
			return true
		}
	}
	return false
}

// processStream is stream processing for one job: streamProc.Process, unless
// a test replaced it (youtube.Service has no stub seam of its own).
func (w *DownloadWorker) processStream(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
	if w.processStreamFn != nil {
		return w.processStreamFn(ctx, job)
	}
	return w.streamProc.Process(ctx, job)
}

// vodRefreshVerdict is the answer a stale VOD's re-extraction got about the
// video itself — a playability refusal, or a status that is no longer a
// finished stream's (judgeRefreshedVodInfo) — as opposed to a fetch that
// failed. It reads as the error it wraps, sentinels included, so setJobError
// routes it exactly as it routes the same verdict from Process. What it adds
// is that it can be told apart: classifyProbeErr cannot place a verdict's
// text and calls it transient, and a backlog VOD was sent back to Queued on
// it (requeueBacklogAfterTransientFailure).
type vodRefreshVerdict struct{ err error }

func (v *vodRefreshVerdict) Error() string { return v.err.Error() }
func (v *vodRefreshVerdict) Unwrap() error { return v.err }

// isVodRefreshVerdict reports whether err is, or wraps, a re-extraction's
// verdict on the video.
func isVodRefreshVerdict(err error) bool {
	_, ok := errors.AsType[*vodRefreshVerdict](err)
	return ok
}

// refreshVodInfo is streamProc.RefreshVodInfo, unless a test replaced it.
func (w *DownloadWorker) refreshVodInfo(ctx context.Context, job *database.Job) (*youtube.VideoInfo, error) {
	if w.refreshVodInfoFn != nil {
		return w.refreshVodInfoFn(ctx, job)
	}
	return w.streamProc.RefreshVodInfo(ctx, job)
}

// refreshStaleVodInfo re-extracts a YouTube VOD whose extraction went stale
// while it waited for its slots, and installs the fresh player response on
// result. A fresh extraction, a Twitch job (its playlist URLs are fetched by
// the orchestrator itself) and anything that is not a VOD pass through
// untouched. The error is the re-extraction's, for processJob to handle as it
// handles stream processing's own.
func (w *DownloadWorker) refreshStaleVodInfo(ctx context.Context, job *database.Job, result *StreamProcessResult, extractedAt time.Time) error {
	if !result.IsVod || job.Platform == "twitch" || !vodInfoStale(result.VideoInfo, extractedAt, time.Now()) {
		return nil
	}
	w.logger.Info("VOD extraction went stale while the download waited for a slot; re-extracting",
		"jobID", job.ID, "extracted", extractedAt.UTC().Format(time.RFC3339))
	info, err := w.refreshVodInfo(ctx, job)
	if err != nil {
		return err
	}
	result.VideoInfo = info
	return nil
}
