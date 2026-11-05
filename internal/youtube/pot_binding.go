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
//
// One deliberate deviation from upstream: when a session is authenticated but
// its datasync ID did not survive extraction, yt-dlp warns and mints nothing
// at all ("Missing required Data Sync ID for account", fetch_po_token,
// _video.py:2815-2819) — the experiment on or off — whereas this returns the
// video ID the experiment names, or else visitor data (and, failing that,
// the video ID). Moombox has no user to warn and no way to pass one in by
// hand, so an empty binding would simply mint a token bound to nothing; a
// second-best real binding is strictly more useful, and the provenance log
// names which rule fired. The binding is marked as the guess it is
// (gvsContentBinding's guessed, VideoInfo.GvsBindingGuessed), and the
// strategies count a token minted under it as a missing one (owner decision
// D12, 2026-10-31): a stream whose token is mandatory takes a token-free copy
// where one exists, as upstream's missing_pot skip would, and the guessed
// token rides only a stream that has none.
//
// Upstream's other GVS refusal is not marked a guess: a signed-out session
// with no visitor data, the experiment off ("Missing required Visitor Data",
// fetch_po_token, _video.py:2784-2792). D12 rules on a signed-in session's
// account binding, which the fallback certainly is not; a signed-out
// session's visitor data is simply gone, with no better identity left to
// bind to, and Moombox mints under the video ID — the last-resort fallback
// below, the binding YouTube's own experiment uses — rather than not at all.

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
	binding, kind, _ = gvsContentBinding(videoID, ytcfg, isAuthenticated, channelID)
	return binding, kind
}

// gvsContentBinding is GvsContentBinding, also reporting whether the binding
// is a guess: an authenticated session the experiment does not bind to the
// video ID, whose datasync ID did not survive extraction, so the rule fell
// past the one binding such a session's GVS token carries — upstream's
// "Missing required Data Sync ID for account", for which it mints nothing.
//
// The experiment's video-ID binding is a guess too when the session is
// signed in and its datasync ID did not survive (owner decision, wave FC):
// upstream's external-fetch refusal (fetch_po_token, _video.py:2815-2819,
// yt-dlp b77c9304e) tests `not data_sync_id and self.is_authenticated` with
// no `gvs_bind_to_video_id` exemption — unlike the visitor-data refusal and
// the config-token warning above it, which both exempt the experiment — so a
// signed-in session without its datasync ID mints nothing under the
// experiment either. Signed out, or signed in with the datasync ID present,
// the experiment's binding is the one it names and no guess. withAttestation
// stamps the verdict on VideoInfo.GvsBindingGuessed.
func gvsContentBinding(videoID string, ytcfg *YtcfgData, isAuthenticated bool, channelID string) (binding, kind string, guessed bool) {
	if ytcfg != nil && ytcfg.GvsBindToVideoID && videoID != "" {
		return videoID, BindingVideoID, isAuthenticated && ytcfg.DataSyncID == ""
	}
	if ytcfg != nil {
		if isAuthenticated && ytcfg.DataSyncID != "" {
			return ytcfg.DataSyncID, BindingDataSyncID, false
		}
		if ytcfg.VisitorData != "" {
			return ytcfg.VisitorData, BindingVisitorData, isAuthenticated
		}
	}
	// Nothing session-scoped survived extraction. Prefer the video ID over an
	// empty binding — it is at least stable and is what the experiment path
	// would have used.
	if videoID != "" {
		return videoID, BindingVideoID, isAuthenticated
	}
	return channelID, BindingChannelID, isAuthenticated
}
