package youtube

// GVS PO-token content binding, ported from yt-dlp's
// get_webpo_content_binding (yt_dlp/extractor/youtube/pot/utils.py) and the
// experiment detection that feeds it (_video.py, "Detected experiment to bind
// GVS PO Token to video ID").
//
// Upstream's rule, in order:
//  1. When the html5_generate_content_po_token experiment is active for the
//     session, GVS tokens bind to the VIDEO ID.
//  2. Otherwise, an authenticated session binds to its DATASYNC ID (the
//     account session identifier).
//  3. Otherwise (logged out), it binds to VISITOR DATA.
//
// Moombox previously hardcoded rule 1 for moonarchive parity. That is correct
// only while the experiment is on — and it currently is, verified against a
// live watch page on 2026-08-15 — but a session where YouTube turns it off
// would need datasync binding, and sending a video-ID-bound token there earns
// exactly the silent 403s this whole subsystem exists to avoid. Implementing
// the real rule removes that dependency on an experiment we don't control.
//
// PLAYER-context tokens are NOT covered here: upstream binds those to the
// video ID unconditionally (PoTokenContext.PLAYER → VIDEO_ID).

// Binding kinds, used as the `binding` value in the "[POT] GVS mint" log line
// so a months-later 403 investigation can see which rule produced the token.
const (
	BindingVideoID     = "videoID"
	BindingDataSyncID  = "datasyncID"
	BindingVisitorData = "visitorData"
	BindingChannelID   = "channelID"
)

// GvsContentBinding returns the content binding a GVS (segment-URL) PO token
// must carry, plus a short kind label for logging. videoID is the job's video;
// ytcfg supplies the experiment flag, datasync ID and visitor data captured
// from the watch page.
//
// The channelID fallback has no upstream equivalent: it exists because a
// visitor-data extraction failure would otherwise produce an empty binding,
// and an empty binding mints a token bound to nothing. Callers pass the
// channel as a last resort so the mint still has *a* stable value.
func GvsContentBinding(videoID string, ytcfg *YtcfgData, isAuthenticated bool, channelID string) (binding, kind string) {
	if ytcfg != nil && ytcfg.GvsBindToVideoID && videoID != "" {
		return videoID, BindingVideoID
	}
	if ytcfg != nil {
		if isAuthenticated && ytcfg.DataSyncID != "" {
			return ytcfg.DataSyncID, BindingDataSyncID
		}
		if ytcfg.VisitorData != "" {
			return ytcfg.VisitorData, BindingVisitorData
		}
	}
	// Nothing session-scoped survived extraction. Prefer the video ID over an
	// empty binding — it is at least stable and is what the experiment path
	// would have used.
	if videoID != "" {
		return videoID, BindingVideoID
	}
	return channelID, BindingChannelID
}
