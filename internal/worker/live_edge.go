package worker

import (
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Why a live YouTube capture recorded from the live edge instead of the
// broadcast's start (owner decision PR-R2), as the row's live_edge_reason
// stores it and both UIs show it: "Recorded from the live edge: <reason>,
// joined H:MM:SS in". Each names the setup arm that put the capture on the
// live HLS playlist, whose window starts near the live edge, not at sq=0.
const (
	// setupMarathonOr: the broadcast was older than YouTube keeps its
	// segments (marathonLiveEdge, owner decision 10).
	liveEdgeMarathon = "the stream had been live longer than YouTube keeps its segments (~120 h)"
	// evictedStartToLiveEdge: the start was gone though the row's age did
	// not say so (owner decision 7).
	liveEdgeEvicted = "the stream's start was already gone from YouTube's ~120 h window"
	// setupOrFallback's DRM arm (owner decision FE-1), live: the live HLS
	// playlist's window starts near the live edge.
	liveEdgeDRM = "every DASH format of a kind the job needs was DRM-protected"
	// The same arm on a post-live stream (isVod, StreamPostLive): its HLS
	// manifest holds only the broadcast's last ~2 hours — yt-dlp marks a
	// post-live stream's non-from-start formats "(Last 2 hours)"
	// (_video.py b77c9304e, adjust_incomplete_format: "Post-live DASH and
	// m3u8 manifests only have the last ~2 hours"). A finished VOD's HLS
	// manifest is the whole video and marks nothing.
	liveEdgePostLiveDRM = "every DASH format of a kind the job needs was DRM-protected, and a post-live stream's HLS holds only its last ~2 h"
	// setupOrFallback's missing_pot arm (owner decision D7).
	liveEdgeMissingPot = "no GVS token could be minted for the from-start formats (missing_pot)"
	// noJSFromStartOrHls's HLS arm (owner decision D7, FD decision 1).
	liveEdgeNoJS = "the player's JS challenges could not be solved and no split pair that needs none was left"
)

// withLiveEdge marks result as a live capture that fell to the live edge for
// reason. A nil result stays nil.
func withLiveEdge(result *DownloadResult, reason string) *DownloadResult {
	if result != nil {
		result.LiveEdgeReason = reason
	}
	return result
}

// withLiveEdgeErr is withLiveEdge over a setup's (result, err): the reason is
// set only on a setup that succeeded.
func withLiveEdgeErr(reason string) func(*DownloadResult, error) (*DownloadResult, error) {
	return func(result *DownloadResult, err error) (*DownloadResult, error) {
		if err != nil {
			return result, err
		}
		return withLiveEdge(result, reason), nil
	}
}

// drmLiveEdgeReason is the live_edge_reason of a setup the DRM arm put on the
// HLS manifest: liveEdgeDRM for a live capture, liveEdgePostLiveDRM for a
// post-live one (it reaches the setup as isVod, but its HLS is the last ~2 h,
// not the whole broadcast), "" for a finished VOD.
func drmLiveEdgeReason(isVod bool, info *youtube.VideoInfo) string {
	switch {
	case !isVod:
		return liveEdgeDRM
	case info != nil && (info.StreamStatus == youtube.StreamPostLive || info.IsPostLiveDVR):
		return liveEdgePostLiveDRM
	}
	return ""
}

// recordLiveEdgeStart records on the job's row that its live capture is
// recording from the live edge, and why (owner decision PR-R2), where until
// now only a job-log Warn said so while a missing tail got a details row and
// a Resume gate (incomplete_tail). YouTube has no gap-fill, so the operator
// is the only one who can fetch the missing start, and the details row —
// with join_offset_seconds, which the finalize writes — tells them how much.
//
// One notification ("live_edge", a Job Lifecycle event like gap_split, on
// by default as that is) is sent the first time the recording falls back:
// when the row held no reason yet. A Resume or restart of the same recording
// that falls back again rewrites the reason without a second notification;
// a Reinitialize clears it with the rest of the row, so its new recording
// notifies afresh. Only a capture's first setup in the job's first part is
// recorded (ExecuteWithChat skips a resume into a later part) — a mid-stream
// path switch (switchLivePath) keeps the earlier part, recorded from the
// start, and is not a late start.
func (o *DownloadOrchestrator) recordLiveEdgeStart(jobCtx *JobContext, reason string) {
	if jobCtx == nil || jobCtx.Job == nil || reason == "" || o.db == nil {
		return
	}
	jobID := jobCtx.Job.ID
	prior := ""
	if j, err := o.db.GetJob(jobID); err == nil && j != nil {
		prior = j.LiveEdgeReason
	}
	o.db.UpdateJobFields(jobID, map[string]any{"live_edge_reason": reason})
	jobCtx.Job.LiveEdgeReason = reason
	if prior != "" || o.notifier == nil {
		return
	}
	f := NotifyFacts(jobCtx.Job)
	o.notifier.Send(
		"Recording From the Live Edge",
		fmt.Sprintf("The start of the stream cannot be recorded (%s); recording from the live edge instead: %s",
			reason, notifications.EscapeMarkdown(jobCtx.Job.Title)),
		notifications.TypeWarning,
		[]notifications.Field{
			{Name: "Channel", Value: notifications.EscapeMarkdown(jobCtx.Job.ChannelName), Inline: true},
		},
		notifications.SendOptions{
			URL:       f.URL,
			Thumbnail: f.ThumbnailURL,
			Event:     "live_edge",
			Author:    notifyAuthor(f),
			Platform:  f.Platform,
			JobID:     f.ID,
		},
	)
}
