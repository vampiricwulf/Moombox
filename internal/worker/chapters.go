package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Archive chapters (owner decision D23 and its two follow-ups). A Twitch VOD
// is muxed with its moments; a live Twitch capture with its archive VOD's
// moments when that VOD is there at finalize, else with the categories the
// capture saw the broadcast in, timed from the stream start; a YouTube VOD
// with its chapter markers, else its description's timestamps. Only a
// single-file archive carries them: a split job's parts are muxed while the
// broadcast runs, before any list is known.
//
// The Twitch lists are staged (utils.StagedChaptersFile) in the capture's
// own timeline before the finalize, so the mux needs no Twitch service — and
// so a Mux action over a crashed capture still finds them. A live capture's
// raw material, its start in the broadcast and the categories it observed,
// is kept beside them in twitchTimelineFile.

// twitchTimelineFile records, in a live Twitch capture's root staging dir,
// what the live chapter fallback is built from.
const twitchTimelineFile = "twitch_timeline.json"

// twitchTimeline is twitchTimelineFile's content: RecordingStartMs is the
// program date-time of the first segment the root part's file holds (epoch
// ms, 0 until known), and Categories the broadcast's category each time it
// was seen to change, oldest first.
type twitchTimeline struct {
	RecordingStartMs int64                `json:"recordingStartMs,omitempty"`
	Categories       []twitchCategoryMark `json:"categories,omitempty"`
}

// twitchCategoryMark is one observed category: the broadcast was in Title
// from AtMs (epoch ms).
type twitchCategoryMark struct {
	AtMs  int64  `json:"atMs"`
	Title string `json:"title"`
}

// twitchTimelineMu serialises every read-modify-write of a timeline file: the
// category poll and the engine's first-segment report write it from their own
// goroutines.
var twitchTimelineMu sync.Mutex

// readTwitchTimeline reads dir's timeline; a missing or unreadable one is
// empty — the fallback then has nothing, which is no chapters.
func readTwitchTimeline(dir string) twitchTimeline {
	var tl twitchTimeline
	if data, err := os.ReadFile(filepath.Join(dir, twitchTimelineFile)); err == nil {
		_ = json.Unmarshal(data, &tl)
	}
	return tl
}

// updateTwitchTimeline applies change to dir's timeline and writes it back
// when change reports it changed anything. Best effort: a timeline that
// cannot be written costs the fallback chapters, never the capture.
func updateTwitchTimeline(dir string, change func(*twitchTimeline) bool) {
	if dir == "" {
		return
	}
	twitchTimelineMu.Lock()
	defer twitchTimelineMu.Unlock()
	tl := readTwitchTimeline(dir)
	if !change(&tl) {
		return
	}
	data, err := json.Marshal(tl)
	if err != nil {
		return
	}
	_ = utils.WriteFileAtomic(filepath.Join(dir, twitchTimelineFile), data, 0o644)
}

// noteTwitchCategory records that the broadcast is in category at at, when
// that is a change from the last one recorded. An empty category (no game
// set, or a reply without one) records nothing.
func noteTwitchCategory(dir, category string, at time.Time) {
	if category == "" {
		return
	}
	updateTwitchTimeline(dir, func(tl *twitchTimeline) bool {
		if n := len(tl.Categories); n > 0 && tl.Categories[n-1].Title == category {
			return false
		}
		tl.Categories = append(tl.Categories, twitchCategoryMark{AtMs: at.UnixMilli(), Title: category})
		return true
	})
}

// noteTwitchRecordingStart records the program date-time of the first segment
// a fresh root-part file holds. Overwritten each time: the engine reports it
// whenever it STARTS the file, and a file started again (a set-aside restart)
// begins where the new report says.
func noteTwitchRecordingStart(dir string, pdt time.Time) {
	if pdt.IsZero() {
		return
	}
	updateTwitchTimeline(dir, func(tl *twitchTimeline) bool {
		tl.RecordingStartMs = pdt.UnixMilli()
		return true
	})
}

// twitchCategoryPollInterval is how often a live capture reads the
// broadcast's category for the chapter fallback: the precision of an observed
// chapter's start. One StreamMetadata request a minute per capture, beside the
// monitor's every-15-seconds batch for the whole channel list.
const twitchCategoryPollInterval = time.Minute

// pollTwitchCategory notes the broadcast's category every
// twitchCategoryPollInterval until ctx ends. A failed read is skipped; the
// next tick tries again.
func (o *DownloadOrchestrator) pollTwitchCategory(ctx context.Context, dir string, read func(context.Context) (string, error)) {
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("panic in Twitch category poll", "panic", fmt.Sprint(r), "dir", dir)
		}
	}()
	t := time.NewTicker(twitchCategoryPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		category, err := read(ctx)
		if err != nil {
			o.logger.Debug("twitch category poll failed", "err", err, "dir", dir)
			continue
		}
		noteTwitchCategory(dir, category, time.Now())
	}
}

// twitchChapterLookupTimeout bounds the archive-VOD lookup at a live
// capture's finalize: two GQL calls that must not hold the mux.
const twitchChapterLookupTimeout = 30 * time.Second

// twitchArchiveChapters is a live broadcast's archive VOD's chapters, in the
// broadcast's timeline, or nil when the channel has no VOD of it (yet).
func (w *DownloadWorker) twitchArchiveChapters(ctx context.Context, login, startedAt string) []utils.Chapter {
	if w.tw == nil || startedAt == "" {
		return nil
	}
	vodID, err := w.tw.ArchiveVodFor(ctx, login, startedAt)
	if err != nil || vodID == "" {
		if err != nil {
			w.logger.Debug("twitch archive VOD lookup failed; using the observed categories", "err", err, "channel", login)
		}
		return nil
	}
	info, err := w.tw.GetVodInfo(ctx, vodID)
	if err != nil || info == nil {
		return nil
	}
	return info.Chapters
}

// liveTwitchChapters builds a live capture's chapter list in the capture's
// own timeline: the archive VOD's (vodChapters, broadcast timeline) when it
// has any, else the observed categories, each from the moment it was seen —
// both shifted by how far into the broadcast the capture began. Without a
// known broadcast start the VOD list cannot be placed, and the observed list
// is timed from the capture's start; without a recorded capture start the
// first observation (made as the capture began) stands in for it.
func liveTwitchChapters(streamStart time.Time, tl twitchTimeline, vodChapters []utils.Chapter) []utils.Chapter {
	recStartMs := tl.RecordingStartMs
	if recStartMs == 0 && len(tl.Categories) > 0 {
		recStartMs = tl.Categories[0].AtMs
	}
	if recStartMs == 0 {
		return nil
	}
	// observed is the category list in a timeline whose zero is originMs.
	// The first entry is the category the capture found the broadcast in,
	// noted as it began — a few seconds after the first segment's program
	// date-time — so it is taken to run from the timeline's zero rather than
	// leave an untitled sliver in front of it.
	observed := func(originMs int64) []utils.Chapter {
		var out []utils.Chapter
		for i, c := range tl.Categories {
			start := max(c.AtMs-originMs, 0)
			if i == 0 {
				start = 0
			}
			out = append(out, utils.Chapter{StartMs: start, Title: c.Title})
		}
		return out
	}
	if streamStart.IsZero() {
		return observed(recStartMs)
	}
	startMs := streamStart.UnixMilli()
	list := vodChapters
	if len(list) == 0 {
		list = observed(startMs)
	}
	return utils.ShiftChapters(list, recStartMs-startMs)
}

// stageTwitchChapters writes the chapters a Twitch archive is muxed with
// (utils.StagedChaptersFile): a VOD's own, or a live capture's
// (liveTwitchChapters, its archive VOD looked up now). Best effort.
func (o *DownloadOrchestrator) stageTwitchChapters(ctx context.Context, jobCtx *JobContext, variant *TwitchVariantInfo, isVod bool) {
	var list []utils.Chapter
	if isVod {
		list = variant.VodChapters
	} else {
		var start time.Time
		if t, err := time.Parse(time.RFC3339, jobCtx.Job.StreamStartTime); err == nil {
			start = t
		}
		var vodList []utils.Chapter
		if variant.ArchiveChaptersFn != nil && !start.IsZero() {
			lookupCtx, cancel := context.WithTimeout(ctx, twitchChapterLookupTimeout)
			vodList = variant.ArchiveChaptersFn(lookupCtx)
			cancel()
		}
		list = liveTwitchChapters(start, readTwitchTimeline(jobCtx.StagingDir), vodList)
	}
	if err := utils.WriteStagedChapters(jobCtx.StagingDir, list); err != nil {
		o.logger.Warn("could not stage the archive's chapters; it is muxed without them", "err", err, "jobID", jobCtx.Job.ID)
	}
}

// youtubeChapters is the YouTube chapter source the mux asks
// (fetchYouTubeChapters in production); a test replaces it.
var youtubeChapters = fetchYouTubeChapters

// fetchYouTubeChapters is youtube.Service.FetchChapters.
func fetchYouTubeChapters(ctx context.Context, yt *youtube.Service, videoID, description string, durationMs int64) []utils.Chapter {
	return yt.FetchChapters(ctx, videoID, description, durationMs)
}

// archiveChaptersFile is the FFMETADATA file the mux reads its chapters from,
// written into staging beside the media.
const archiveChaptersFile = "chapters.ffmeta"

// prepareArchiveChapters writes the chapter list a single-file archive is
// muxed with and returns its path, "" for none. The staged Twitch list wins;
// a live Twitch job with none staged (the Mux action over a crashed capture)
// is given its observed categories; a YouTube VOD asks youtubeChapters. Every
// list is fitted to the staged media's span (utils.SanitizeChapters) — the
// row's length is the fallback when the probe cannot measure it.
func (o *DownloadOrchestrator) prepareArchiveChapters(ctx context.Context, jobCtx *JobContext, videoPath, audioPath string) string {
	job := jobCtx.Job
	if job == nil || jobCtx.StagingDir == "" {
		return ""
	}
	list, err := utils.ReadStagedChapters(jobCtx.StagingDir)
	if err != nil {
		o.logger.Warn("staged chapters unreadable; muxing without them", "err", err, "jobID", job.ID)
		list = nil
	}
	isTwitch := job.Platform == "twitch"
	if len(list) == 0 && isTwitch && !job.IsVod {
		var start time.Time
		if t, perr := time.Parse(time.RFC3339, job.StreamStartTime); perr == nil {
			start = t
		}
		list = liveTwitchChapters(start, readTwitchTimeline(jobCtx.StagingDir), nil)
	}
	needYouTube := len(list) == 0 && !isTwitch && job.IsVod && jobCtx.YT != nil && job.VideoID != ""
	if len(list) == 0 && !needYouTube {
		return ""
	}

	durationMs := int64(0)
	for _, in := range []string{videoPath, audioPath} {
		if in == "" {
			continue
		}
		if span := o.runFFprobe(ctx, in).spanSec(); span > 0 {
			durationMs = max(durationMs, int64(span*1000))
		}
	}
	if durationMs == 0 && job.LengthSeconds != nil {
		durationMs = int64(*job.LengthSeconds) * 1000
	}
	if durationMs == 0 {
		return ""
	}
	if needYouTube {
		fetchCtx, cancel := context.WithTimeout(ctx, twitchChapterLookupTimeout)
		list = youtubeChapters(fetchCtx, jobCtx.YT, job.VideoID, job.Description, durationMs)
		cancel()
	}
	chapters := utils.SanitizeChapters(list, durationMs)
	if len(chapters) == 0 {
		return ""
	}
	path := filepath.Join(jobCtx.StagingDir, archiveChaptersFile)
	if err := utils.WriteFileAtomic(path, []byte(utils.FFMetadataChapters(chapters)), 0o644); err != nil {
		o.logger.Warn("could not write the archive's chapters; muxing without them", "err", err, "jobID", job.ID)
		return ""
	}
	o.logger.Info("embedding chapters", "jobID", job.ID, "chapters", len(chapters))
	return path
}

// downloadTwitchThumbnail saves a Twitch thumbnail at its original size when
// the CDN serves one, else at the size the URL names (G45): yt-dlp's
// _get_thumbnails pair, the 0x0 form at preference 1 with the sized URL kept
// as the fallback (twitch.py b77c9304e). Whether previews-ttv serves 0x0 for
// a LIVE preview is not confirmed from here; the fallback is what makes
// trying it free. A refusal and a body too small to be an image
// (DownloadFileMinSize's 1000 bytes) both fall through.
func downloadTwitchThumbnail(ctx context.Context, thumbURL, dst string, lg logger) error {
	if original := twitch.OriginalSizeThumbnailURL(thumbURL); original != "" {
		if err := DownloadFileMinSize(ctx, original, dst, 1000, lg); err == nil {
			return nil
		}
	}
	return DownloadFileMinSize(ctx, thumbURL, dst, 1000, lg)
}
