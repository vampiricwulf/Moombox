package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// startThumbnailFile names the copy of a YouTube live capture's thumbnail
// taken as the capture starts (stageStartThumbnail), in the job's staging
// root. The finalize still fetches the thumbnail fresh and that one wins;
// this copy is what the archive gets when that fetch fails — the video was
// deleted right after the stream ended, or the connection is down as the
// capture ends, which is one of the ways a capture ends. Before it a YouTube
// archive's thumbnail was the one asset that depended on the video still
// existing after the stream (owner ruling R2-X2).
//
// Its own name, not Twitch's "thumbnail.jpg": copyAssets takes a staged
// "thumbnail.*" ahead of any fetch, and a Twitch job's staged thumbnail
// counts as staging (HasStagingFiles) as it always has. This file does
// neither.
const startThumbnailFile = "start-thumbnail.jpg"

// startThumbnailTimeout bounds the whole early fetch, every size together.
const startThumbnailTimeout = 30 * time.Second

// ytThumbnailQualities is the order a YouTube thumbnail's sizes are asked
// for. Order matters because only `maxresdefault` (1280x720 or more), `hq720`
// (1280x720) and `mqdefault` (320x180) are natively 16:9 -- the others
// (sddefault 640x480, hqdefault 480x360, default 120x90) are 4:3 with black
// bars baked into the image. Once the Web UI prefers the local file, those
// 4:3 sources letterbox visibly inside the dashboard's 16:9 thumb box. Try
// the two HD sizes first, in yt-dlp's order (`'maxresdefault', 'hq720', ...`,
// _video.py:4113-4127, cca80fe61): a video with no maxresdefault often still
// has hq720, and without it one saved the 320x180 mqdefault. Then mqdefault
// for clean 16:9 framing, and only then the letterboxed sizes. Upstream ranks
// sddefault and hqdefault above mqdefault, by size; keeping the 16:9 one
// ahead of them is Moombox's own choice, for the framing.
var ytThumbnailQualities = []string{"maxresdefault", "hq720", "mqdefault", "hqdefault", "sddefault", "default"}

// fetchYouTubeThumbnail saves videoID's thumbnail to dst, the first size in
// ytThumbnailQualities that ytThumbnailBase serves, and reports whether one
// was saved. It stops asking once ctx is done.
func fetchYouTubeThumbnail(ctx context.Context, videoID, dst string, lg logger) bool {
	for _, quality := range ytThumbnailQualities {
		if ctx.Err() != nil {
			return false
		}
		thumbURL := fmt.Sprintf("%s/%s/%s.jpg", ytThumbnailBase, videoID, quality)
		if DownloadFileMinSize(ctx, thumbURL, dst, 1000, lg) == nil {
			return true
		}
	}
	return false
}

// isStagingMetadataFile reports whether name, a file in a job's staging
// directory, is a record about a recording rather than anything a run could
// resume or mux: the yt:stretch display-aspect record and the start thumbnail
// (with the temporary name its download writes under). Both are written
// before a capture's first media byte, so a job holding only them has
// nothing staged (HasStagingFiles, PreviewReinitialize).
func isStagingMetadataFile(name string) bool {
	return name == displayAspectFile || name == startThumbnailFile || name == startThumbnailFile+".tmp"
}

// stageStartThumbnail fetches a YouTube live capture's thumbnail into its
// staging root (startThumbnailFile) in the background, as Twitch's capture
// does for its own. It never fails or delays the capture: a failed fetch is a
// Debug line, and the returned stop cancels a fetch still running and waits
// for it, so nothing is written into staging after the capture returns (the
// worker removes staging right after). Call stop on the way out.
//
// A staging root that already holds the file is left alone: a restarted or
// resumed capture keeps the copy from its first start.
func (o *DownloadOrchestrator) stageStartThumbnail(parentCtx context.Context, jobCtx *JobContext) (stop func()) {
	// Read here, not in the goroutine: the finalize replaces jobCtx.Job.
	jobID, videoID, dst := jobCtx.Job.ID, jobCtx.Job.VideoID, filepath.Join(jobCtx.StagingDir, startThumbnailFile)
	if videoID == "" || fileExists(dst) {
		return func() {}
	}
	if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(parentCtx, startThumbnailTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				o.logger.Error("panic in start thumbnail download", "panic", fmt.Sprint(r), "jobID", jobID)
			}
		}()
		if !fetchYouTubeThumbnail(ctx, videoID, dst, o.logger) {
			o.logger.Debug("no thumbnail was staged as the capture started; the finalize fetches it",
				"jobID", jobID)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
