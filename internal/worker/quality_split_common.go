package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// awaitDownloadOrQualityChange selects between the downloaders returning an
// error on downloadDone and the quality monitor signalling a change on
// qualityChangeCh. Handles the minSegmentDuration gate: when a change comes
// in too soon after the current segment started, log + UpdateBaseline + wait
// for the next event (don't split on noise).
//
// Returns (qualityChanged, downloadErr) once the downloaders exit for any
// reason. qualityChanged == true means cancelCurrent was called and we've
// already drained downloadDone. platformTag ("youtube" / "twitch") is
// attached to log entries.
//
// Shared between orchestrator_youtube.go runLiveStreamDownload and
// orchestrator_twitch.go ExecuteTwitch (audit reports/worker.md F27, narrow
// extraction).
func (o *DownloadOrchestrator) awaitDownloadOrQualityChange(
	downloadDone <-chan error,
	qualityChangeCh <-chan QualityInfo,
	segmentStartTime int64,
	currentQuality QualityInfo,
	monitor *QualityMonitor,
	cancelCurrent func(),
	platformTag, jobID string,
) (qualityChanged bool, downloadErr error) {
	for {
		select {
		case newQ := <-qualityChangeCh:
			if time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration {
				// Too soon — don't split. Reset monitor baseline so it
				// re-detects the change once we're past minSegmentDuration.
				o.logger.Debug("quality change ignored (min segment duration)",
					"platform", platformTag,
					"from", currentQuality.Label,
					"to", newQ.Label,
					"jobID", jobID)
				if monitor != nil {
					monitor.UpdateBaseline(currentQuality)
				}
				continue
			}
			o.logger.Info("proactive quality change detected",
				"platform", platformTag,
				"from", currentQuality.Label,
				"to", newQ.Label,
				"jobID", jobID)
			cancelCurrent()
			downloadErr = <-downloadDone
			return true, downloadErr
		case downloadErr = <-downloadDone:
			return false, downloadErr
		}
	}
}

// launchBackgroundSegmentMux runs o.muxSegment for a completed part
// (quality or gap split) in a goroutine with panic recovery and a detached
// 2-hour context. Increments wg so the caller can wait for all background
// muxes at end of stream.
//
// Detached context rationale: user-cancel must not orphan a partial output
// mid-FFmpeg. The ceiling is generous (codec-copy of a multi-GB segment on
// slow disk/USB/NAS with antivirus scanning can legitimately exceed tens of
// minutes) but bounded so a truly stuck FFmpeg can't pin shutdown
// indefinitely (defer wg.Wait() in the caller means worker.Stop()'s 10s
// grace otherwise can't override the wait).
//
// preMux, when non-nil, runs inside the goroutine before the mux — Twitch
// uses it to inject emotes into the part's rolled chat file (first part
// pays the emote-API resolve; the result is cached for later parts) without
// stalling the split path that must restart the downloader quickly.
//
// platformTag ("youtube" / "twitch") is attached to log entries so
// structured-log consumers can filter per-platform.
func (o *DownloadOrchestrator) launchBackgroundSegmentMux(
	jobCtx *JobContext,
	wg *sync.WaitGroup,
	segIdx int,
	startTime, endTime int64,
	quality QualityInfo,
	muxResult *DownloadResult,
	platformTag string,
	preMux func(context.Context),
) {
	wg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				o.logger.Error("panic in mux segment goroutine",
					"platform", platformTag,
					"panic", fmt.Sprint(r),
					"jobID", jobCtx.Job.ID)
			}
		}()
		defer wg.Done()
		muxCtx, muxCancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer muxCancel()
		if preMux != nil {
			preMux(muxCtx)
		}
		seg, muxErr := o.muxSegment(muxCtx, jobCtx, segIdx, startTime, endTime, quality, muxResult)
		if muxErr != nil {
			o.logger.Error("failed to mux quality segment",
				"platform", platformTag,
				"err", muxErr,
				"jobID", jobCtx.Job.ID)
			return
		}
		if seg != nil {
			o.logger.Info("quality segment muxed",
				"platform", platformTag,
				"segment", segIdx,
				"quality", quality.Label,
				"file", seg.Filename,
				"jobID", jobCtx.Job.ID)
		}
	}()
}

// sendGapSplitNotification delivers the "unrecoverable gap — new part
// started" notification for Twitch live gap splits. No-op when the notifier
// is nil.
func (o *DownloadOrchestrator) sendGapSplitNotification(
	jobCtx *JobContext,
	segmentIndex int,
	quality QualityInfo,
) {
	if o.notifier == nil {
		return
	}
	o.notifier.Send(
		"Twitch Stream Gap — New Part Started",
		fmt.Sprintf("Segments expired before they could be downloaded; part %d is complete and part %d continues at the live edge: %s",
			segmentIndex+1, segmentIndex+2, jobCtx.Job.Title),
		notifications.TypeWarning,
		[]notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "Quality", Value: quality.Label, Inline: true},
			{Name: "Completed Part", Value: fmt.Sprintf("%d", segmentIndex+1), Inline: true},
		},
		notifications.SendOptions{
			URL:       jobCtx.Job.URL,
			Thumbnail: jobCtx.Job.ThumbnailURL,
			Event:     "gap_split",
		},
	)
}

// sendQualitySplitNotification delivers the "Stream quality changed during
// download" notification for the given platform. platformTitle is the
// user-facing prefix ("YouTube" or "Twitch"). No-op when the notifier is nil.
func (o *DownloadOrchestrator) sendQualitySplitNotification(
	jobCtx *JobContext,
	platformTitle string,
	currentQuality, newQuality QualityInfo,
	segmentIndex int,
) {
	if o.notifier == nil {
		return
	}
	o.notifier.Send(
		fmt.Sprintf("%s Quality Split", platformTitle),
		fmt.Sprintf("Stream quality changed during download: %s", jobCtx.Job.Title),
		notifications.TypeDownload,
		[]notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "From", Value: currentQuality.Label, Inline: true},
			{Name: "To", Value: newQuality.Label, Inline: true},
			{Name: "Segment", Value: fmt.Sprintf("%d", segmentIndex+1), Inline: true},
		},
		notifications.SendOptions{
			URL:       jobCtx.Job.URL,
			Thumbnail: jobCtx.Job.ThumbnailURL,
			Event:     "quality_split",
		},
	)
}
