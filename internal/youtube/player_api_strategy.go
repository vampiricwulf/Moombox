package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// fetchWatchPage is FetchWatchPage behind a package var, purely so the cascade
// tests can exercise the whole client chain without a real watch-page round
// trip (the playerRetryBackoffBase seam below exists for the same reason).
// Production never writes it.
var fetchWatchPage = FetchWatchPage

// mismatchTally records, across one extraction, how many client player
// responses were rejected because YouTube answered about a different video and
// how many came back usable. Upstream skips a substituting client with a
// warning and fails the whole extraction only when NOTHING survived
// (_video.py:3181-3187); the tally is how this cascade reproduces both halves
// of that condition.
type mismatchTally struct {
	attempts   int
	mismatched int
	survived   int
	got        string // the first substitute id seen, for the verdict's log line
	// lastErr is the last non-mismatch client failure. It is what the
	// exhaustion verdict wraps, so the client's own "HTTP <code>" survives
	// into the string worker/probe_classify.go reads. Only the LAST is kept:
	// the clients answer the same question, so the earlier errors are
	// duplicates of it or of each other, and all of them are already logged.
	lastErr error
}

// note records one client attempt and passes its result through unchanged, so
// call sites read `result, err := tally.note(p.fetchWithClient(...))`.
//
// A client that failed for some OTHER reason — transport, HTTP status — counts
// as neither a substitute nor a survivor. That is upstream's shape: such a
// client raises, hits `continue` (_video.py:3119-3121) and contributes nothing
// to `prs` either, so it must not veto the verdict.
func (t *mismatchTally) note(info *VideoInfo, err error) (*VideoInfo, error) {
	t.attempts++
	var mm *VideoIDMismatchError
	switch {
	case errors.As(err, &mm):
		t.mismatched++
		if t.got == "" {
			t.got = mm.Got
		}
	case err == nil:
		t.survived++
	default:
		t.lastErr = err
	}
	return info, err
}

// exhausted is upstream's OTHER raise condition, the one beside the IP block:
// `elif not prs: raise ExtractorError('Failed to extract any player response')`
// (_video.py:3188-3189). Nothing usable exists — every client this cascade
// asked errored, and the watch page carried no player response of its own.
//
// It cannot fire on a path that produced anything: the O-I waiting room, a
// members-only verdict and every successful extraction all leave a TV (or
// later) response in survived.
func (t *mismatchTally) exhausted(wpParsed *VideoInfo) bool {
	return t.survived == 0 && wpParsed == nil
}

// ipBlockShape is upstream's `if skipped_clients: ... if not prs: raise`
// (_video.py:3181-3187): at least one client was served a substitute AND
// nothing survived.
//
// Both halves matter. Requiring `mismatched > 0` means the verdict only ever
// fires on a POSITIVE substitution signal, never on a flaky network alone.
// NOT requiring "every attempt mismatched" is the other half: a client that
// failed for another reason produces no usable response either, so letting it
// veto the verdict would turn a real IP block into an empty VideoInfo the
// worker renders as "unhandled status: " — a diagnosis-free dead end.
func (t *mismatchTally) ipBlockShape() bool {
	return t.mismatched > 0 && t.survived == 0
}

// finishExtraction is the single exit both cascades take. It applies
// withAttestation and raises the IP-block verdict when a client was served a
// substitute, no client survived, and the watch page produced nothing usable
// either. wpParsed is that last survivor test: upstream keeps the watch page's
// own player response in `prs` and only raises when `prs` is empty.
//
// Being the single exit is also why the two extraction-wide FormatDiag
// figures are stamped here: the VideoInfo handed back is whichever client won,
// and its own parse only ever saw its own response. The worker reads these to
// explain an extraction that produced no usable formats, so they have to
// describe the whole cascade, not the last client in it.
func (p *PlayerAPI) finishExtraction(ctx context.Context, info *VideoInfo, wp *WatchPageResult, videoID string, tally *mismatchTally, wpParsed *VideoInfo) (*VideoInfo, error) {
	if info != nil {
		info.FormatDiag.DRMSkipped, info.FormatDiag.CollapsedRenditions = extractionStateFrom(ctx).poolCounts()
	}
	if tally.ipBlockShape() && wpParsed == nil {
		// Name the substitute, as upstream's warning does
		// (_video.py:3182-3184). Every per-client mismatch below is logged at
		// Debug, so without this field an operator at the default level sees
		// the verdict and cannot learn WHICH video YouTube served instead.
		p.logger.Warn("[PlayerApi] Innertube clients were served a different video and none survived",
			"videoID", videoID, "substitute", capSubstituteID(tally.got),
			"clients", tally.attempts, "mismatched", tally.mismatched)
		return nil, ErrAllClientsMismatched
	}
	if tally.exhausted(wpParsed) {
		// Upstream's sibling raise (see exhausted). Returning the empty
		// VideoInfo instead hands the worker "unhandled status: " with no
		// cause anywhere in it — the same dead end ipBlockShape's doc names.
		// AFTER the IP-block branch on purpose: a positive substitution
		// signal is a better diagnosis than plain exhaustion, so Task 3's
		// verdict keeps its precedence.
		p.logger.Warn("[PlayerApi] no Innertube client produced a player response",
			"videoID", videoID, "clients", tally.attempts, "lastError", errText(tally.lastErr))
		if tally.lastErr != nil {
			// Wrapped, not replaced: probe_classify.go keys on the
			// "HTTP <code>" this carries.
			return nil, fmt.Errorf("failed to extract any player response: %w", tally.lastErr)
		}
		// Unreachable from either cascade today — both always attempt at
		// least one client, so an exhausted tally always holds its error —
		// but the verdict must not depend on that to stay a sentence.
		return nil, errors.New("failed to extract any player response")
	}
	return withAttestation(info, wp, videoID), nil
}

// errText renders an optional error for a log field without the "%!v(<nil>)"
// an absent one would otherwise print.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// withAttestation stamps the watch page's session verdict, its chat facts and
// the GVS PO-token content binding onto the VideoInfo being returned. Applied
// at every GetVideoInfo* return site explicitly — NOT via
// mergeWatchPageMetadata, which several early returns skip or call with a nil
// source.
//
// The chat facts ride along here for the same reason the binding does: this is
// the one function holding BOTH the info and the page, and FetchWatchPage
// already extracted the continuation, the replay flag and the visitor data on
// its way through. Throwing them away is what made every chat start fetch that
// 1-5 MB page a second time (report #56 / YOUTUBE-10).
//
// The binding is resolved here, at the one point that holds all three inputs
// (the experiment flag and datasync ID from ytcfg, the login state from the
// page), so download strategies never re-derive it and cannot drift apart.
//
// It no longer stamps the page's attestation challenge: that VideoInfo field
// was write-only and went with the rest of the challenge path (owner ruling
// R1, 2026-09-15). The name is kept because every return site names it, and
// it is where a re-wired challenge would be stamped again.
func withAttestation(info *VideoInfo, wp *WatchPageResult, videoID string) *VideoInfo {
	if info == nil {
		return info
	}
	if wp != nil {
		// Carry the chat facts the page already yielded, so setupChatDownloader
		// and tryStartEarlyChat do not fetch this 1-5 MB page again moments
		// from now (report #56 / YOUTUBE-10).
		//
		// The PAGE's own fetch instant, not this extraction's end: the cascade
		// can run for tens of seconds after the page arrived, and measuring
		// the two-minute window from here would trust the token past its real
		// age (close-review Finding 10). A synthesized result (failed fetch)
		// carries no stamp, so fall back to now — it has no continuation
		// either, and Usable() rejects it on that.
		fetchedAt := wp.FetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = time.Now()
		}
		info.Chat = ChatSource{
			FetchedAt:    fetchedAt,
			Continuation: wp.ChatContinuation,
			IsReplay:     wp.ChatIsReplay,
			Err:          wp.ChatErr,
		}
		if wp.Ytcfg != nil {
			info.Chat.VisitorData = wp.Ytcfg.VisitorData
		}
		// Carry YouTube's own login verdict onto the result. It costs a
		// string copy and it is the only thing that can tell a dead cookie
		// file apart from a live session that simply lacks a membership.
		info.SessionAuth = wp.SessionAuth
		info.GvsBinding, info.GvsBindingKind = GvsContentBinding(videoID, wp.Ytcfg, wp.SessionAuth == SessionAuthLoggedIn, info.ChannelID)
		return info
	}
	// No watch page at all (fetch failed): still produce a usable binding
	// rather than leaving the strategies to invent one. SessionAuth stays
	// unknown — absence of evidence, not evidence of a logged-out session.
	info.GvsBinding, info.GvsBindingKind = GvsContentBinding(videoID, nil, false, info.ChannelID)
	return info
}

// ProbeVideoStatus performs a lightweight probe using ANDROID_VR (no cookies
// needed). Marked probe-only: the live strategies re-run it on their own
// interrupt cadence, so its DRM report belongs at Debug (ANDROID_VR is
// cookieless and mints no PLAYER token either way, so O-R does not bite here).
func (p *PlayerAPI) ProbeVideoStatus(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	info, err := p.fetchWithAndroidVR(withProbeOnlyCall(ctx), videoID, visitorData)
	stampManifestSources(info, "android_vr")
	return info, err
}

// ProbeVideoDate fetches ONLY a video's publish date via one WEB-family player
// call, carrying whatever credentials the jar holds.
//
// NOT anonymous, and the distinction is load-bearing for members-only content:
// this goes through fetchWithClientProbe, so Auth.GenerateAPIHeaders attaches
// the jar's Cookie header and the SAPISIDHASH Authorization whenever YouTube
// auth is configured — the probe-only flag (owner decision O-R) skips the
// PLAYER PO token and nothing else. (ProbeVideoStatus is the anonymous one —
// ANDROID_VR through fetchWithCookielessClient.) The comment here claimed
// "anonymous" until 2026-09-18 and sent a sweep looking for a members-only
// date-fetch bug that does not exist; TestProbeVideoDateSendsTheJarsCredentials
// pins it.
//
// The status probes cannot supply dates: microformat (the source of
// publishDate AND liveBroadcastDetails.startTimestamp) is a WEB-client
// response feature, and ANDROID_VR/TV probe responses omit it
// entirely — verified against live YouTube. The date parse rides the normal
// parsePlayerResponse → extractPublishedAt path, so precision semantics
// ("started"/"day", Z-normalized) are identical to every other probe.
//
// A response without a microformat date returns ""/"" with a nil error —
// "YouTube has no date" is a result, not a failure; only transport-level
// errors are errors. Callers gate the retry-vs-proceed decision on that
// distinction (spec §9's two-phase probe).
func (p *PlayerAPI) ProbeVideoDate(ctx context.Context, videoID, visitorData string) (publishedAt, precision string, err error) {
	ytcfg := DefaultYtcfg()
	if visitorData != "" {
		ytcfg.VisitorData = visitorData
	}
	// No watch page fetched on this probe-only path, so no ytcfg beyond the
	// visitor data the caller cached. And no PLAYER PO token: owner decision
	// O-R, see fetchWithClientProbe.
	info, err := p.fetchWithClientProbe(ctx, videoID, constants.WebSafariClient, ytcfg, 0)
	if err != nil {
		return "", "", err
	}
	return info.PublishedAt, info.PublishedPrecision, nil
}

// ProbeVideoStatusAuthenticated performs a lightweight authenticated status probe
// using the TV_DOWNGRADED client with cookies (no watch page, no STS, no cipher).
// Used for polling members-only upcoming streams, and by the worker's one-call
// quality probe. Pass an empty string for visitorData when none has been
// captured yet.
//
// On the probe variant for the DRM report's sake, not for O-R's: TV_DOWNGRADED
// is not WEB-family, so clientAcceptsPlayerPoToken already returns false and
// no token was ever minted here either way. What the marking changes is that
// the tv client's DRM skip — which this exact call meets every 30 s of a
// waiting room, on precisely the accounts the experiment hits — reports at
// Debug instead of Warn.
func (p *PlayerAPI) ProbeVideoStatusAuthenticated(ctx context.Context, videoID, visitorData string) (*VideoInfo, error) {
	ytcfg := DefaultYtcfg()
	if visitorData != "" {
		ytcfg.VisitorData = visitorData
	}
	info, err := p.fetchWithClientProbe(ctx, videoID, constants.TVDowngradedClient, ytcfg, 0)
	stampManifestSources(info, "tv_auth")
	return info, err
}

// captureVisitorData forwards watch-page visitor data to the service cache
// (OnVisitorData → Service.SetVisitorData). Shared by the authenticated AND
// public extraction paths — the public call is load-bearing for anonymous
// users: after invalidate403Caches clears the cache, Init() never re-runs
// (startup-only call site), so this hand-off is the only refill source.
func (p *PlayerAPI) captureVisitorData(ytcfg *YtcfgData) {
	if ytcfg != nil && ytcfg.VisitorData != "" && p.OnVisitorData != nil {
		p.OnVisitorData(ytcfg.VisitorData)
	}
}

// tvSaysWaitingRoom reports the one verdict owner decision O-I short-circuits
// on: the TV authority says the stream is UPCOMING and playability is fine.
//
// An upcoming stream has no formats BY DEFINITION, so the three clients the
// cascade consults to go find some (WEB_CREATOR, VISIONOS, ANDROID_VR — the
// last of them twice) cannot succeed, and a waiting room re-runs them every
// 30 s for its whole duration. PlayabilityOK is required because an upcoming
// MEMBERS-ONLY or login-required stream is precisely what the chain exists
// for.
//
// This does NOT touch the protected "android_vr retained" ruling: that ruling
// keeps android_vr in the client roster against upstream dropping it, and it
// stays there for every other verdict. The cost of the rule is bounded and
// was accepted: up to one 30 s poll of delay in the rare case a cookieless
// client sees the stream live before TV does.
func tvSaysWaitingRoom(result *VideoInfo) bool {
	return result != nil && result.StreamStatus == StreamUpcoming && result.PlayabilityError == PlayabilityOK
}

// GetVideoInfoAuthenticated fetches video info using the full multi-client strategy.
func (p *PlayerAPI) GetVideoInfoAuthenticated(ctx context.Context, videoID string) (*VideoInfo, error) {
	// One extraction's scratch state, shared by every player response this
	// cascade parses (see extractionState) — it is what keeps the DRM-skip
	// report to one line per extraction rather than one per client.
	ctx = withExtractionState(ctx)

	// Fetch watch page
	if err := p.auth.SyncCookies(); err != nil {
		p.logger.Warn("[PlayerApi] SyncCookies failed", slog.String("error", err.Error()))
	}
	wp, err := fetchWatchPage(ctx, videoID, p.auth.GetCookieHeader())
	if err != nil {
		p.logger.Warn("[PlayerApi] Could not fetch watch page", slog.String("error", err.Error()))
		wp = &WatchPageResult{Ytcfg: DefaultYtcfg()}
	}

	ytcfg := wp.Ytcfg

	// Log what YouTube thought of our session BEFORE any playability verdict
	// exists to argue about. The startup "Cookies loaded" line upstream
	// (completeAuthSet / anyAuthCookie) reports only what a parsed cookie
	// file CONTAINS; this line is the first place the answer to "are those
	// cookies still a session?" appears in the log.
	p.logger.Debug("[PlayerApi] watch page session state",
		"videoID", videoID, "sessionAuth", string(wp.SessionAuth))

	if wp.AttestationChallenge == "" {
		p.logger.Debug("[PlayerApi] no attestation challenge from watch page", "videoID", videoID, "reason", wp.AttestationReason)
	}

	// Capture visitor data for future probe calls
	p.captureVisitorData(ytcfg)

	formatPool := []Format{}
	tally := &mismatchTally{}

	// Extract STS (signatureTimestamp) from player JS for format decryption
	sts := p.extractSTS(ctx, ytcfg.PlayerURL)

	// Parse watch page player response
	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		var wpErr error
		wpParsed, wpErr = p.parsePlayerResponse(withPlayerClient(ctx, "watch_page"), wp.PlayerResponse, ytcfg.PlayerURL, ytcfg, videoID)
		if wpErr != nil {
			// Today the only error this can be is a video-ID mismatch: the page
			// itself was served for another video, which upstream also drops
			// (_video.py:3038). Nothing downstream may use it.
			p.logger.Warn("[PlayerApi] watch-page player response rejected", slog.String("error", wpErr.Error()))
			wpParsed = nil
		} else if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPageAuth)
			stampManifestSources(wpParsed, "watch_page")
		}
	}

	// Try web_embedded first — yt-dlp's lead authenticated client since
	// 2026.08.19 (_DEFAULT_AUTHED_CLIENTS = web_embedded, tv_downgraded,
	// web). It needs no PO token and supports cookies, so it keeps serving
	// formats when POT enforcement or the account experiment bites the
	// others. Lightweight shape (no embed-page fetch — encryptedHostFlags is
	// only needed on the age-restricted path below). Purely a format-pool /
	// DASH contributor: TV below remains the playability/status authority,
	// because web_embedded reports "unavailable" for any embedding-disabled
	// channel and must never drive classification.
	authEmb, authEmbErr := tally.note(p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, false))
	if authEmbErr != nil {
		p.logger.Debug("[PlayerApi] web_embedded (authed cascade) failed", slog.String("error", authEmbErr.Error()))
	} else {
		collectFormats(&formatPool, authEmb.Formats, "web_embedded", AuthLevelWebEmbedded)
	}

	// Try TV client
	result, err := tally.note(p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, sts))
	if err != nil {
		// HTTP error (not a playability error) — log warning and continue to try
		// WEB_CREATOR / VISIONOS / ANDROID_VR instead of returning immediately.
		p.logger.Warn("[PlayerApi] TV client failed, will try other clients", slog.String("error", err.Error()))
		result = &VideoInfo{}
	} else {
		collectFormats(&formatPool, result.Formats, "tv_auth", AuthLevelTVAuth)
		stampManifestSources(result, "tv_auth")
		// The TV client is the playability/status AUTHORITY on this path, so
		// its verdict is the one every downstream error string is derived
		// from — name the client AND the verdict together, or a log reader
		// cannot tell which of five clients produced "members_only".
		p.logger.Debug("[PlayerApi] TV client result",
			"client", constants.TVDowngradedClient.ClientName,
			"formats", len(result.Formats),
			// A client YouTube forced onto SABR reports "formats 0" and so
			// does an empty streamingData; these two fields are what tells
			// them apart in the log (row YOUTUBE-8).
			"urllessFormats", result.FormatDiag.URLlessFormats,
			"sabrForced", result.FormatDiag.SabrForced,
			"dashManifestUrl", result.DashManifestURL != "",
			"hlsManifestUrl", result.HlsManifestURL != "",
			"streamStatus", result.StreamStatus,
			"playability", string(result.PlayabilityError))
	}

	// Owner decision O-I, read once here so every gate below agrees. It is
	// derived from the TV result because TV is this path's playability/status
	// AUTHORITY — the same result the log line above names.
	waitingRoom := tvSaysWaitingRoom(result)

	// Try web_safari client for DASH manifest (preferred over web)
	webResult, webErr := tally.note(p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, sts))
	webLabel := "web_safari"
	webAuthLevel := AuthLevelWebSafari // preferred over standard web
	if webErr != nil {
		p.logger.Warn("[PlayerApi] web_safari client failed, trying web fallback", slog.String("error", webErr.Error()))
		// Fall back to standard web client
		webResult, webErr = tally.note(p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts))
		webLabel = "web"
		webAuthLevel = AuthLevelWeb
		if webErr != nil {
			p.logger.Warn("[PlayerApi] WEB fallback also failed", slog.String("error", webErr.Error()))
		}
	}
	if webErr == nil {
		p.logger.Debug("[PlayerApi] Web client result",
			"client", webLabel,
			"formats", len(webResult.Formats),
			"urllessFormats", webResult.FormatDiag.URLlessFormats,
			"sabrForced", webResult.FormatDiag.SabrForced,
			"dashManifestUrl", webResult.DashManifestURL != "",
			"hlsManifestUrl", webResult.HlsManifestURL != "",
			"streamStatus", webResult.StreamStatus,
			"playability", string(webResult.PlayabilityError))
		collectFormats(&formatPool, webResult.Formats, webLabel, webAuthLevel)
		if webResult.DashManifestURL != "" && result.DashManifestURL == "" {
			p.logger.Info("[PlayerApi] Got DASH manifest URL from web client", "videoID", videoID)
			result.DashManifestURL, result.DashManifestSource = webResult.DashManifestURL, webLabel
		}
	}

	// web_embedded as one more DASH source before the ANDROID_VR round trip
	// below. Under the account experiment this is likely stripped too (the
	// embedded call is cookied), but when present it saves the extra call.
	// PlayabilityOK is required here for the same reason every sibling
	// enrichment requires it (the ANDROID_VR blocks below, and the cookieless
	// chain): a manifest attached to an unplayable response is not a manifest
	// worth adopting — and web_embedded specifically reports "unavailable"
	// for embedding-disabled channels.
	if result.DashManifestURL == "" && authEmbErr == nil &&
		authEmb.PlayabilityError == PlayabilityOK && authEmb.DashManifestURL != "" {
		p.logger.Info("[PlayerApi] Got DASH manifest URL from web_embedded", "videoID", videoID)
		result.DashManifestURL, result.DashManifestSource = authEmb.DashManifestURL, "web_embedded"
	}

	// ANDROID_VR DASH workaround for the YouTube account experiment that
	// strips dashManifestUrl from cookied clients (yt-dlp issue #15274).
	// Symptom: TV+WEB return formats and HLS but no DASH; users on the
	// affected account experiment see this consistently. ANDROID_VR is
	// cookieless and unaffected by the experiment, so it serves as a
	// DASH-only enrichment source.
	//
	// ANDROID_VR stays here even though yt-dlp dropped it from its defaults
	// (2026.08.19: "ALL formats 403'd since 2026.08.17"): that enforcement is
	// selective, not universal — verified 2026-08-24 from this vantage that
	// its live DASH manifest AND segment fetches still return 200 — and no
	// replacement exists: VISIONOS serves HLS only for live (verified same
	// day), and anonymous TV / WEB / WEB_EMBEDDED all refuse to serve live
	// streams without a fuller session. If live DASH downloads sourced here
	// start 403-storming, this fallback is the suspect: there is currently
	// nothing to swap in, so it would have to be dropped (losing
	// --live-from-start addressability for experiment-affected accounts).
	//
	// Live/upcoming only — DASH on those streams unlocks --live-from-start
	// segment addressability, which HLS in YouTube live cannot do. VOD
	// already works fine via the format pool. Skip on members-only and
	// age-restricted (anonymous android_vr would 401; age-restricted has
	// its own web_embedded path below).
	// webResult is nil when both web fetches failed — that means "no DASH
	// from web", so the fallback applies a fortiori (and dereferencing
	// webResult unguarded would panic).
	//
	// vrPrefetch survives this block so the cookieless chain below reuses
	// whatever this fetch produced rather than asking android_vr the same
	// question twice in one extraction (owner decision O-I).
	var vrPrefetch *cookielessPrefetch
	if !waitingRoom &&
		result.DashManifestURL == "" &&
		(webErr != nil || webResult.DashManifestURL == "") &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		result.PlayabilityError != PlayabilityMembersOnly &&
		result.PlayabilityError != PlayabilityAgeRestricted &&
		result.PlayabilityError != PlayabilityLoginRequired {

		vrResult, vrErr := tally.note(p.fetchWithAndroidVR(ctx, videoID, ytcfg.VisitorData))
		vrPrefetch = &cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName, result: vrResult, err: vrErr}
		if vrErr != nil {
			p.logger.Debug("[PlayerApi] ANDROID_VR DASH fallback failed",
				slog.String("error", vrErr.Error()))
		} else if vrResult.PlayabilityError == PlayabilityOK && vrResult.DashManifestURL != "" {
			p.logger.Info("[PlayerApi] DASH manifest sourced via ANDROID_VR fallback",
				"videoID", videoID, "vrFormats", len(vrResult.Formats))
			result.DashManifestURL, result.DashManifestSource = vrResult.DashManifestURL, "android_vr_dash_fallback"
			// Merge ANDROID_VR formats with auth-level dedup. TV/WEB formats
			// win same-itag ties via deduplicateFormats — this comment
			// claimed that from the start, but until 2026-08-15 the ranking
			// said the opposite (AuthLevelAndroidVR was 0, the most
			// preferred), so android_vr silently displaced every WEB format
			// it shared an itag with. It is now the last-resort tier, per
			// yt-dlp's client priority; see the AuthLevel block in types.go.
			// Its formats still fill genuine gaps, they just no longer
			// evict a matching WEB/TV entry.
			collectFormats(&formatPool, vrResult.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
			vrPrefetch.pooled = true
		}
	}

	// Try web_embedded for age-restricted content.
	//
	// The cascade call above already fetched this client, so reuse its result
	// when it came back usable instead of re-fetching. That matters here more
	// than anywhere: age-restricted is one of the playability states the
	// quality monitor re-probes every 30s through the authenticated path, and
	// re-fetching cost TWO extra requests per probe (the player call plus the
	// embed page). encryptedHostFlags — the only thing the heavier variant
	// adds — is needed exactly when the plain shape FAILS, so a result that
	// already succeeded demonstrably did not need it.
	if result.PlayabilityError == PlayabilityAgeRestricted {
		embResult, embErr := authEmb, authEmbErr
		if embErr == nil && embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] Age-restricted content detected, reusing cascade web_embedded result", "videoID", videoID)
		} else {
			p.logger.Info("[PlayerApi] Age-restricted content detected, retrying web_embedded with encryptedHostFlags", "videoID", videoID)
			embResult, embErr = tally.note(p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, true))
		}

		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			// Formats from the cascade call are already pooled; re-collecting
			// the same slice would double every entry (dedup keeps one, but
			// the pool should not carry known duplicates either way).
			if embResult != authEmb {
				collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			}
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(ctx, formatPool)
			return p.finishExtraction(ctx, embResult, wp, videoID, tally, wpParsed)
		} else if embResult != authEmb {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	// If TV fails, try WEB_CREATOR. Skipped outright on a waiting-room verdict
	// (owner decision O-I): len(result.Formats) == 0 is TRUE for every
	// upcoming stream, which is what used to drag the whole fallback chain
	// into every 30 s poll.
	if !waitingRoom && (result.PlayabilityError == PlayabilityMembersOnly ||
		result.PlayabilityError == PlayabilityLoginRequired ||
		len(result.Formats) == 0) {

		wcResult, wcErr := tally.note(p.fetchWithClient(ctx, videoID, constants.WebCreatorClient, ytcfg, sts))
		if wcErr != nil {
			p.logger.Warn("[PlayerApi] WEB_CREATOR failed, will try other clients", slog.String("error", wcErr.Error()))
			// Fall through to ANDROID_VR / watch page below
			wcResult = &VideoInfo{}
		}

		collectFormats(&formatPool, wcResult.Formats, "web_creator", AuthLevelWebCreator)
		stampManifestSources(wcResult, "web_creator")
		p.logger.Debug("[PlayerApi] WEB_CREATOR result",
			"client", constants.WebCreatorClient.ClientName,
			"formats", len(wcResult.Formats),
			"urllessFormats", wcResult.FormatDiag.URLlessFormats,
			"sabrForced", wcResult.FormatDiag.SabrForced,
			"streamStatus", wcResult.StreamStatus,
			"playability", string(wcResult.PlayabilityError))

		// Try the cookieless fallback clients if WEB_CREATOR also fails or has
		// inadequate formats. VISIONOS first (yt-dlp's lead default since
		// 2026.08.19), then ANDROID_VR — retained behind it because upstream's
		// all-formats-403 enforcement on android_vr is selective (verified
		// still fully working from here 2026-08-24), and because it is the
		// only cookieless source of a live dashManifestUrl. NOT because it
		// covers "Made for kids": upstream marks BOTH clients as unable to
		// serve those (_base.py repeats the same note above each), so
		// android_vr adds nothing there. Upstream's made-for-kids fallback is
		// web_embedded then tv_downgraded, which this chain does not attempt.
		if wcResult.PlayabilityError == PlayabilityLoginRequired ||
			wcResult.StreamStatus == StreamNotAStream ||
			len(wcResult.Formats) == 0 ||
			!hasAdequateFormats(wcResult) {

			if wcResult.PlayabilityError != PlayabilityMembersOnly {
				if cfResult := p.tryCookielessFallbacks(ctx, videoID, ytcfg.VisitorData, &formatPool, tally, vrPrefetch); cfResult != nil {
					mergeWatchPageMetadata(cfResult, wpParsed)
					cfResult.Formats = deduplicateFormats(ctx, formatPool)
					return p.finishExtraction(ctx, cfResult, wp, videoID, tally, wpParsed)
				}
			}

			// Fall back to watch page if all clients failed.
			//
			// This is the return that supplies the verdict on the
			// authenticated members-only path — TV says members_only,
			// WEB_CREATOR agrees with zero formats, the cookieless chain is
			// skipped because members-only content cannot come from an
			// anonymous client — so the error string the operator reads is
			// built from THIS result. Name its source or that string has no
			// traceable origin in the log.
			if wpParsed != nil {
				wpParsed.Formats = deduplicateFormats(ctx, formatPool)
				p.logger.Debug("[PlayerApi] returning watch-page parse (all clients exhausted)",
					"client", "watch_page",
					"videoID", videoID,
					"formats", len(wpParsed.Formats),
					// The page's embedded player response carries streamingData
					// like any client's, and can be SABR-forced like any
					// client's — and this is the line the authenticated
					// members-only verdict is read from.
					"urllessFormats", wpParsed.FormatDiag.URLlessFormats,
					"sabrForced", wpParsed.FormatDiag.SabrForced,
					"streamStatus", wpParsed.StreamStatus,
					"playability", string(wpParsed.PlayabilityError))
				return p.finishExtraction(ctx, wpParsed, wp, videoID, tally, wpParsed)
			}
		}

		if wpParsed != nil {
			mergeWatchPageMetadata(wcResult, wpParsed)
		}
		wcResult.Formats = deduplicateFormats(ctx, formatPool)
		return p.finishExtraction(ctx, wcResult, wp, videoID, tally, wpParsed)
	}

	return p.finishExtraction(ctx, finalizeVideoInfo(ctx, result, wpParsed, formatPool), wp, videoID, tally, wpParsed)
}

// GetVideoInfoPublic fetches video info without authentication.
func (p *PlayerAPI) GetVideoInfoPublic(ctx context.Context, videoID string) (*VideoInfo, error) {
	// One extraction's scratch state — see GetVideoInfoAuthenticated.
	ctx = withExtractionState(ctx)

	wp, err := fetchWatchPage(ctx, videoID, "")
	if err != nil {
		// Not fatal (the Innertube clients below carry the extraction), but
		// silence here previously hid consent-wall and network failures on
		// the anonymous path entirely — the authenticated path has always
		// logged this.
		p.logger.Warn("[PlayerApi] Could not fetch watch page (public)", slog.String("error", err.Error()))
		wp = &WatchPageResult{Ytcfg: DefaultYtcfg()}
	}

	if wp.AttestationChallenge == "" {
		p.logger.Debug("[PlayerApi] no attestation challenge from watch page", "videoID", videoID, "reason", wp.AttestationReason)
	}

	// Capture visitor data for future probe calls (parity with the
	// authenticated path). Load-bearing for anonymous users: after a 403
	// credential refresh clears the service cache, this is the only refill.
	p.captureVisitorData(wp.Ytcfg)

	formatPool := []Format{}
	tally := &mismatchTally{}

	// Extract STS for public path too
	stsPublic := p.extractSTS(ctx, wp.Ytcfg.PlayerURL)

	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		var wpErr error
		wpParsed, wpErr = p.parsePlayerResponse(withPlayerClient(ctx, "watch_page"), wp.PlayerResponse, wp.Ytcfg.PlayerURL, wp.Ytcfg, videoID)
		if wpErr != nil {
			// Today the only error this can be is a video-ID mismatch: the page
			// itself was served for another video, which upstream also drops
			// (_video.py:3038). Nothing downstream may use it.
			p.logger.Warn("[PlayerApi] watch-page player response rejected", slog.String("error", wpErr.Error()))
			wpParsed = nil
		} else if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPagePublic)
			stampManifestSources(wpParsed, "watch_page")
		}
	}

	result, err := tally.note(p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, wp.Ytcfg, stsPublic))
	if err != nil {
		// Every TV failure is now a SKIP, not the end of the extraction —
		// the shape GetVideoInfoAuthenticated's own TV arm has always had:
		// log, take an empty result, carry on into the chain. Returning the
		// error, or the watch-page parse, denied VISIONOS and ANDROID_VR their
		// chance to answer at all; the watch-page fallback below still applies
		// when nothing else produces anything.
		if mm, ok := errors.AsType[*VideoIDMismatchError](err); ok {
			// A substitution has its own line because it names the video
			// YouTube served instead — upstream's own warning does
			// (_video.py:3122-3123, :3182-3184), and the generic line below
			// cannot say it.
			p.logger.Warn("[PlayerApi] TV client (public) answered about a different video, skipping it",
				"videoID", videoID, "got", capSubstituteID(mm.Got))
		} else {
			p.logger.Warn("[PlayerApi] TV client failed (public), will try other clients", slog.String("error", err.Error()))
		}
		result = &VideoInfo{}
	} else {
		collectFormats(&formatPool, result.Formats, "tv_public", AuthLevelTVPublic)
		stampManifestSources(result, "tv_public")
		p.logger.Debug("[PlayerApi] TV client result (public)",
			"client", constants.TVDowngradedClient.ClientName,
			"formats", len(result.Formats),
			"urllessFormats", result.FormatDiag.URLlessFormats,
			"sabrForced", result.FormatDiag.SabrForced,
			"streamStatus", result.StreamStatus,
			"playability", string(result.PlayabilityError))
	}

	// The same waiting-room verdict the authenticated cascade reads (owner
	// decision O-I), from the same authority: TV.
	waitingRoom := tvSaysWaitingRoom(result)

	// Try web_embedded for age-restricted content (public path)
	if result.PlayabilityError == PlayabilityAgeRestricted {
		p.logger.Info("[PlayerApi] Age-restricted content detected (public), trying web_embedded", "videoID", videoID)
		embResult, embErr := tally.note(p.fetchWithEmbedded(ctx, videoID, wp.Ytcfg, stsPublic, true))
		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(ctx, formatPool)
			return p.finishExtraction(ctx, embResult, wp, videoID, tally, wpParsed)
		} else {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	// vrPrefetch is the EMPTY half of the shared slot: this path's ANDROID_VR
	// DASH enrichment runs AFTER the chain rather than before it, so there is
	// nothing to hand in — the chain fills this record with whatever it
	// fetched and the enrichment below reads it instead of asking android_vr
	// the same question twice (owner decision O-I; close-review Finding 6).
	vrPrefetch := &cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName}

	// Skipped whole on a waiting-room verdict (owner decision O-I): all three
	// of these conditions hold for every upcoming stream, and none of the
	// clients below can find formats that do not exist yet.
	if !waitingRoom && (result.PlayabilityError == PlayabilityLoginRequired || len(result.Formats) == 0 || !hasAdequateFormats(result)) {
		// VISIONOS first, ANDROID_VR second — same rationale as the
		// authenticated path's cookieless fallback chain.
		if cfResult := p.tryCookielessFallbacks(ctx, videoID, wp.Ytcfg.VisitorData, &formatPool, tally, vrPrefetch); cfResult != nil {
			mergeWatchPageMetadata(cfResult, wpParsed)
			cfResult.Formats = deduplicateFormats(ctx, formatPool)
			return p.finishExtraction(ctx, cfResult, wp, videoID, tally, wpParsed)
		}

		if wpParsed != nil {
			wpParsed.Formats = deduplicateFormats(ctx, formatPool)
			return p.finishExtraction(ctx, wpParsed, wp, videoID, tally, wpParsed)
		}
	}

	// DASH-only enrichment via ANDROID_VR — mirrors the authenticated path
	// (see the comment there for why android_vr is retained despite yt-dlp
	// dropping it: selective enforcement, no cookieless live-DASH substitute).
	// Public live streams hit by the YouTube account-based experiment that
	// strips dashManifestUrl from the cookied/TV path also land here.
	// Skipped on the same waiting-room verdict as the chain above: an upcoming
	// stream has no manifest to source either (owner decision O-I).
	if !waitingRoom &&
		result.DashManifestURL == "" &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		result.PlayabilityError != PlayabilityMembersOnly &&
		result.PlayabilityError != PlayabilityAgeRestricted &&
		result.PlayabilityError != PlayabilityLoginRequired {
		// The chain above may already have asked android_vr in this same
		// extraction; when it did, its answer is in vrPrefetch and this costs
		// no round trip at all (close-review Finding 6).
		vrResult, vrErr, vrPooled := vrPrefetch.result, vrPrefetch.err, vrPrefetch.pooled
		if vrResult == nil && vrErr == nil {
			vrResult, vrErr = tally.note(p.fetchWithAndroidVR(ctx, videoID, wp.Ytcfg.VisitorData))
		}
		if vrErr != nil {
			p.logger.Debug("[PlayerApi] ANDROID_VR DASH fallback (public) failed",
				slog.String("error", vrErr.Error()))
		} else if vrResult != nil && vrResult.PlayabilityError == PlayabilityOK && vrResult.DashManifestURL != "" {
			p.logger.Info("[PlayerApi] DASH manifest sourced via ANDROID_VR fallback (public)",
				"videoID", videoID, "vrFormats", len(vrResult.Formats))
			result.DashManifestURL, result.DashManifestSource = vrResult.DashManifestURL, "android_vr_dash_fallback"
			if !vrPooled {
				collectFormats(&formatPool, vrResult.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
			}
		}
	}

	return p.finishExtraction(ctx, finalizeVideoInfo(ctx, result, wpParsed, formatPool), wp, videoID, tally, wpParsed)
}

// finalizeVideoInfo applies the not_a_stream override + merge + dedup tail
// shared by GetVideoInfoAuthenticated and GetVideoInfoPublic. When the TV
// client classified the video as not_a_stream but the watch page disagreed,
// the watch page's classification wins; if TV still produced adequate
// formats they are kept, otherwise the watch-page parse is returned wholesale
// (audit D3).
func finalizeVideoInfo(ctx context.Context, result, wpParsed *VideoInfo, formatPool []Format) *VideoInfo {
	if result.StreamStatus == StreamNotAStream && wpParsed != nil && wpParsed.StreamStatus != StreamNotAStream {
		if hasAdequateFormats(result) {
			result.StreamStatus = wpParsed.StreamStatus
			result.IsLive = wpParsed.IsLive
			result.IsUpcoming = wpParsed.IsUpcoming
			result.IsPostLiveDVR = wpParsed.IsPostLiveDVR
			mergeWatchPageMetadata(result, wpParsed)
			result.Formats = deduplicateFormats(ctx, formatPool)
			return result
		}
		wpParsed.Formats = deduplicateFormats(ctx, formatPool)
		return wpParsed
	}

	mergeWatchPageMetadata(result, wpParsed)
	result.Formats = deduplicateFormats(ctx, formatPool)
	return result
}

// extractSTS extracts the signatureTimestamp from a player URL.
// Returns 0 if unavailable (missing player URL, no cipher solver, or extraction error).
func (p *PlayerAPI) extractSTS(ctx context.Context, playerURL string) int {
	if playerURL == "" || p.cipherSolver == nil {
		return 0
	}
	stsStr, err := p.cipherSolver.GetSts(ctx, playerURL)
	if err != nil || stsStr == "" {
		return 0
	}
	n, err := strconv.Atoi(stsStr)
	if err != nil {
		return 0
	}
	p.logger.Debug("[PlayerApi] Got signature timestamp", "sts", n)
	return n
}

// fetchWithClientProbe is fetchWithClient for PROBE-ONLY calls — the monitor's
// date probes and the waiting-room status probes. Owner decision O-R: no
// PLAYER PO token is minted.
//
// yt-dlp's WEB PLAYER_PO_TOKEN_POLICY is required=False, recommended=False
// (_base.py:90), so upstream mints none for these at all. Moombox minted one
// per probed VIDEO ID and parked a 6 h sessionCache entry that every later
// mint sweeps O(n) — pure waste for a video that is never downloaded. Real
// download player calls keep their token.
//
// The probe marking also silences the DRM report down in parseFormats: a
// waiting-room poll runs one of these every 30 s, and the Warn the extraction
// path prints once would be a permanent wall here.
func (p *PlayerAPI) fetchWithClientProbe(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	return p.fetchWithClientOpts(ctx, videoID, client, ytcfg, sts, true)
}

func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	return p.fetchWithClientOpts(ctx, videoID, client, ytcfg, sts, false)
}

func (p *PlayerAPI) fetchWithClientOpts(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int, probeOnly bool) (*VideoInfo, error) {
	ctx = withPlayerClient(ctx, client.ClientName)
	if probeOnly {
		ctx = withProbeOnlyCall(ctx)
	}
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.APIKey())
	headers := p.auth.GenerateAPIHeaders(client, ytcfg)

	// Build client context with optional visitorData
	clientCtx := make(map[string]any, len(client.Context))
	maps.Copy(clientCtx, client.Context)
	if ytcfg != nil && ytcfg.VisitorData != "" {
		clientCtx["visitorData"] = ytcfg.VisitorData
	}

	postData := map[string]any{
		"context": map[string]any{
			"client": clientCtx,
		},
		"videoId":        videoID,
		"contentCheckOk": true,
		"racyCheckOk":    true,
	}

	// Always include playbackContext with STS when available
	pbCtx := map[string]any{
		"html5Preference": "HTML5_PREF_WANTS",
	}
	if sts > 0 {
		pbCtx["signatureTimestamp"] = sts
	}
	postData["playbackContext"] = map[string]any{
		"contentPlaybackContext": pbCtx,
	}

	// Inject PO token (serviceIntegrityDimensions.poToken) for WEB-family clients.
	//
	// Bound to the VIDEO ID: yt-dlp binds PoTokenContext.PLAYER to the video
	// ID unconditionally (pot/utils.py get_webpo_content_binding), and it is
	// the golden standard here. Re-activated 2026-08-16 — the 10c2efd revert's
	// suspicion of this binding was exonerated when the stall reproduced on
	// baseline (root cause: ANDROID_VR client ranking, fixed in e9d1388).
	// Minted via the sidecar's cached minter, which since 2026-08-24 the
	// sidecar builds from its homepage (ytcfg, ytAtN) pair with /att/get as
	// fallback (upstream provider parity, 495a47f); the visitorData gate
	// stays as the "session established" precondition it always was, not as
	// the binding.
	//
	// Failure is non-fatal: the request still runs without a token.
	//
	// probeOnly is owner decision O-R (see fetchWithClientProbe): upstream's
	// WEB policy is required=False, and a probed video may never be
	// downloaded at all.
	if !probeOnly && p.potProvider != nil && clientAcceptsPlayerPoToken(client) && ytcfg != nil && ytcfg.VisitorData != "" {
		if poToken, err := p.potProvider.GeneratePoTokenString(ctx, videoID, false); err == nil && poToken != "" {
			postData["serviceIntegrityDimensions"] = map[string]any{"poToken": poToken}
		} else if err != nil {
			p.logger.Debug("[PlayerApi] PO token generation failed, continuing without", slog.String("client", client.ClientName), slog.String("error", err.Error()))
		}
	}

	body, err := json.Marshal(postData)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	return p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "Innertube", videoID)
}

// fetchWithCookielessClient performs a bare player request for the cookieless
// fallback clients (ANDROID_VR, VISIONOS): no auth headers, no STS, no PO
// token — just the client context plus visitor data when available.
func (p *PlayerAPI) fetchWithCookielessClient(ctx context.Context, videoID, visitorData string, client constants.YouTubeClientConfig) (*VideoInfo, error) {
	ctx = withPlayerClient(ctx, client.ClientName)
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.APIKey())

	clientCtx := make(map[string]any, len(client.Context))
	maps.Copy(clientCtx, client.Context)
	if visitorData != "" {
		clientCtx["visitorData"] = visitorData
	}

	postData := map[string]any{
		"context": map[string]any{
			"client": clientCtx,
		},
		"videoId":        videoID,
		"contentCheckOk": true,
		"racyCheckOk":    true,
		"playbackContext": map[string]any{
			"contentPlaybackContext": map[string]any{
				"html5Preference": "HTML5_PREF_WANTS",
			},
		},
	}

	body, err := json.Marshal(postData)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	headers := map[string]string{
		"Content-Type":             "application/json",
		"User-Agent":               client.UserAgent,
		"X-YouTube-Client-Name":    client.ClientID,
		"X-YouTube-Client-Version": client.ClientVersion,
		"Origin":                   "https://www.youtube.com",
	}
	if visitorData != "" {
		headers["X-Goog-Visitor-Id"] = visitorData
	}

	return p.doRetryRequest(ctx, apiURL, body, headers, nil, client.ClientName, videoID)
}

func (p *PlayerAPI) fetchWithAndroidVR(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	return p.fetchWithCookielessClient(ctx, videoID, visitorData, constants.AndroidVRClient)
}

// cookielessPrefetch is the ONE round trip a caller and the cookieless chain
// share for a given client in a given extraction. Owner decision O-I's
// no-behaviour-change half: the ANDROID_VR DASH fallback already fetched
// android_vr, and on an upcoming stream VISIONOS fails hasAdequateFormats, so
// the chain never broke before reaching android_vr a second time.
//
// It works in BOTH directions, because the two paths fetch in opposite orders:
//
//   - HAND-IN (the authenticated path): result and/or err are set, and the
//     chain reuses them instead of paying a second round trip. A FAILED
//     prefetch is reused too — the caller's attempt and this chain's would be
//     the same request in the same extraction, so retrying only buys a second
//     copy of the same error.
//   - WRITE-BACK (the public path): the caller hands in an EMPTY record naming
//     the client it will want, and the chain fills result/err/pooled with what
//     it fetched. The public path's ANDROID_VR enrichment runs AFTER the chain,
//     so without this it asked android_vr the same question twice on the
//     degraded shape (close-review Finding 6, measured calls [7 101 28 28]).
//
// Both nil therefore means "not attempted", never "skip this client"
// (close-review Finding 7).
//
// pooled says the caller ALREADY collected result.Formats into the pool. The
// chain must not collect them again: dedup would keep one either way, but a
// pool that carries known duplicates makes every later count a lie.
type cookielessPrefetch struct {
	clientName string
	result     *VideoInfo
	err        error
	pooled     bool
}

// tryCookielessFallbacks runs the cookieless fallback chain — VISIONOS, then
// ANDROID_VR — collecting every fetched format into the pool at its tier.
// Returns the first result with OK playability and adequate formats, or nil
// when neither client produced one (the pool still holds whatever partial
// formats were fetched). Shared by the authenticated and public paths.
//
// One wrinkle concerns live streams. VISIONOS never returns a
// dashManifestUrl for live (verified 2026-08-24) while ANDROID_VR does, so
// the chain will consult the next client for a manifest to adopt into the
// already-chosen result — but ONLY when the result cannot already be
// segment-addressed without one.
//
// That qualifier is the whole point, and getting it wrong cost a needless
// round trip on every anonymous live extraction. A live response carrying
// split video+audio adaptive URLs routes to the manifest-free path
// (worker.HasManifestlessDashFormats → HasSplitAdaptiveFormats), which the
// strategy switch selects AHEAD of the dashManifestUrl case and which is the
// primary live path since yt-dlp 8c1f07d81 — upstream now skips the live
// DASH manifest entirely. VISIONOS supplies exactly those formats, so a
// VISIONOS-only live result already has full --live-from-start
// addressability and needs no manifest. The DASH manifest survives only as
// the fallback for pools WITHOUT usable split adaptive URLs, and that is the
// single case worth another request.
//
// tally counts the video-ID mismatches this chain contributes, so a
// cascade in which EVERY client was served a substitute can be reported
// as the IP block it is rather than as "no formats". prefetched, when
// non-nil, supplies one client's result instead of fetching it — its attempt
// was already counted in the tally by the caller that made it.
func (p *PlayerAPI) tryCookielessFallbacks(ctx context.Context, videoID, visitorData string, formatPool *[]Format, tally *mismatchTally, prefetched *cookielessPrefetch) *VideoInfo {
	var chosen *VideoInfo
	for _, fb := range []struct {
		client constants.YouTubeClientConfig
		label  string
		level  int
	}{
		{constants.VisionOSClient, "visionos", AuthLevelVisionOS},
		{constants.AndroidVRClient, "android_vr", AuthLevelAndroidVR},
	} {
		var fbResult *VideoInfo
		var fbErr error
		alreadyPooled := false
		// slot is the caller's prefetch for THIS client, in either direction:
		// an answer to reuse, or an empty record to fill.
		var slot *cookielessPrefetch
		if prefetched != nil && prefetched.clientName == fb.client.ClientName {
			slot = prefetched
		}
		if slot != nil && (slot.result != nil || slot.err != nil) {
			fbResult, fbErr, alreadyPooled = slot.result, slot.err, slot.pooled
		} else {
			// Either no prefetch for this client, or an EMPTY one — nil result
			// AND nil error, which is "not attempted", never a veto. Reading it
			// as a veto silently dropped the last-resort client from the chain
			// (close-review Finding 7).
			fbResult, fbErr = tally.note(p.fetchWithCookielessClient(ctx, videoID, visitorData, fb.client))
			if slot != nil {
				// Write back, so the caller reads this answer instead of
				// asking the same client the same question again in the same
				// extraction (close-review Finding 6).
				slot.result, slot.err = fbResult, fbErr
			}
		}
		if fbErr != nil {
			p.logger.Debug("[PlayerApi] cookieless fallback failed",
				slog.String("client", fb.label), slog.String("error", fbErr.Error()))
			continue
		}
		if fbResult == nil {
			// Defensive: every fetch path above returns a result or an error,
			// and an empty prefetch is now fetched rather than skipped.
			continue
		}
		stampManifestSources(fbResult, fb.label)
		if !alreadyPooled {
			collectFormats(formatPool, fbResult.Formats, fb.label, fb.level)
			if slot != nil {
				slot.pooled = true
			}
		}
		p.logger.Debug("[PlayerApi] cookieless fallback result",
			"client", fb.label,
			"formats", len(fbResult.Formats),
			"urllessFormats", fbResult.FormatDiag.URLlessFormats,
			"sabrForced", fbResult.FormatDiag.SabrForced,
			"streamStatus", fbResult.StreamStatus,
			"playability", string(fbResult.PlayabilityError))

		if chosen != nil {
			// Already have a usable result; this client is only still being
			// consulted for a DASH manifest it might carry. Keep going until
			// one actually supplies it — an unconditional break here would
			// silently make a third entry in the list unreachable and falsify
			// the "chain keeps going" contract documented above.
			if fbResult.PlayabilityError == PlayabilityOK && fbResult.DashManifestURL != "" {
				p.logger.Info("[PlayerApi] DASH manifest sourced from cookieless fallback",
					"videoID", videoID, "client", fb.label)
				chosen.DashManifestURL, chosen.DashManifestSource = fbResult.DashManifestURL, fb.label
				break
			}
			continue
		}

		if fbResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(fbResult) {
			chosen = fbResult
			// Consult the next client for a manifest only when this live
			// result has neither a DASH manifest NOR the split adaptive
			// formats that make one unnecessary. Checked against the whole
			// pool, since the strategy switch sees the deduplicated pool
			// rather than this one client's slice.
			needsDash := (fbResult.StreamStatus == StreamLive || fbResult.StreamStatus == StreamUpcoming) &&
				fbResult.DashManifestURL == "" &&
				!HasSplitAdaptiveFormats(*formatPool)
			if !needsDash {
				break
			}
		}
	}
	return chosen
}

// fetchWithEmbedded performs a WEB_EMBEDDED_PLAYER request. fetchEmbedPage
// controls the extra embed-page round trip that sources encryptedHostFlags.
//
// This is a DELIBERATE DIVERGENCE from yt-dlp, not parity with it. Upstream
// attaches encryptedHostFlags to every web_embedded player request
// (_video.py: "there is no harm in including encryptedHostFlags with all
// web_embedded player requests"), sourcing it from the embed-page ytcfg it
// fetches anyway, and names the enforcement experiment
// embeds_enable_encrypted_host_flags_enforcement. Moombox skips that fetch
// on the authed-cascade call because the call is already a second round trip
// on a path that includes mid-download 403 credential recovery, and a third
// would be worse: that recovery runs under min(45s, MaxTimeout/3), as little
// as 10s at the configured floor.
//
// Known failure mode of the trade-off: for an account enrolled in the
// enforcement experiment, the cascade call returns nothing usable, so its
// round trip buys nothing. It is still not harmful — web_embedded only
// contributes to the format pool and never drives classification — but if
// authenticated extractions ever come back short a client, pass true here
// first. The age-restriction bypass already passes true, so that path (where
// web_embedded is load-bearing rather than supplementary) is unaffected.
func (p *PlayerAPI) fetchWithEmbedded(ctx context.Context, videoID string, ytcfg *YtcfgData, sts int, fetchEmbedPage bool) (*VideoInfo, error) {
	ctx = withPlayerClient(ctx, constants.WebEmbeddedClient.ClientName)
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.APIKey())

	// Fetch embed page for encryptedHostFlags
	var embedResult *EmbedPageResult
	if fetchEmbedPage {
		var err error
		embedResult, err = FetchEmbedPage(ctx, videoID)
		if err != nil {
			p.logger.Warn("[PlayerApi] Failed to fetch embed page", slog.String("error", err.Error()))
			// Continue without encryptedHostFlags — it may still work
		}
	}

	headers := p.auth.GenerateAPIHeaders(constants.WebEmbeddedClient, ytcfg)
	// Embedded requests need Referer from the embed URL
	headers["Referer"] = fmt.Sprintf("%s/%s", constants.YouTubeURLs.Embed, videoID)

	// Build client context with thirdParty embedUrl
	clientCtx := make(map[string]any, len(constants.WebEmbeddedClient.Context))
	maps.Copy(clientCtx, constants.WebEmbeddedClient.Context)
	if ytcfg != nil && ytcfg.VisitorData != "" {
		clientCtx["visitorData"] = ytcfg.VisitorData
	}

	postData := map[string]any{
		"context": map[string]any{
			"client": clientCtx,
			"thirdParty": map[string]any{
				"embedUrl": "https://www.reddit.com/",
			},
		},
		"videoId":        videoID,
		"contentCheckOk": true,
		"racyCheckOk":    true,
	}

	// Build playback context with encryptedHostFlags
	pbCtx := map[string]any{
		"html5Preference": "HTML5_PREF_WANTS",
	}
	if sts > 0 {
		pbCtx["signatureTimestamp"] = sts
	}
	if embedResult != nil && embedResult.EncryptedHostFlags != "" {
		pbCtx["encryptedHostFlags"] = embedResult.EncryptedHostFlags
	}
	postData["playbackContext"] = map[string]any{
		"contentPlaybackContext": pbCtx,
	}

	// Inject PO token for WEB_EMBEDDED (same rationale as fetchWithClient):
	// bound to the video ID per upstream's PLAYER-context rule, minted via
	// the sidecar's cached minter (homepage pair, /att/get fallback).
	if p.potProvider != nil && ytcfg != nil && ytcfg.VisitorData != "" {
		if poToken, err := p.potProvider.GeneratePoTokenString(ctx, videoID, false); err == nil && poToken != "" {
			postData["serviceIntegrityDimensions"] = map[string]any{"poToken": poToken}
		} else if err != nil {
			p.logger.Debug("[PlayerApi] PO token generation failed for WEB_EMBEDDED, continuing without", slog.String("error", err.Error()))
		}
	}

	body, err := json.Marshal(postData)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	// Stamped here rather than at each of the three cascade sites that take a
	// web_embedded result in: this client has one label wherever it is asked.
	info, err := p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "WEB_EMBEDDED", videoID)
	stampManifestSources(info, "web_embedded")
	return info, err
}

// innertubeErrorDetailMax bounds what a failure message may quote back from
// YouTube's body. yt-dlp peeks 512 bytes for the same purpose.
const innertubeErrorDetailMax = 512

// innertubeErrorDetail extracts YouTube's own explanation of a non-200
// Innertube response — `{"error":{"message":"…","status":"…"}}`, e.g.
// "Precondition check failed." / "FAILED_PRECONDITION", or "Request is missing
// required authentication credential". Returns "" for any body that is not
// that shape, so a CDN's HTML error page is never quoted into a log line.
//
// The body is DECODED rather than truncated first: chopping at 512 bytes would
// break the JSON and lose the message this exists to surface. The bound is
// applied to the OUTPUT instead, on a rune boundary — YouTube localises
// error.message, and a string slice is a BYTE operation.
func innertubeErrorDetail(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	detail := sanitizeErrorDetail(e.Error.Status + ": " + e.Error.Message)
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ": "))
	detail = strings.TrimSuffix(detail, ":")
	if detail == "" {
		return ""
	}
	if len(detail) > innertubeErrorDetailMax {
		detail = detail[:innertubeErrorDetailMax]
		// Walk back off a rune the cut split. sanitizeErrorDetail left the
		// string valid UTF-8, so this runs at most three times.
		for len(detail) > 0 && !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return detail
}

// sanitizeErrorDetail makes YouTube's own text safe to put in a one-line log
// record and in the job's error column. That text is attacker-adjacent — it
// comes back from a remote service and is not ours to trust — so every control
// character becomes a space: a newline would split the log line, and an ESC is
// the lead byte of an ANSI sequence a terminal would obey. strings.Map decodes
// runes, so invalid UTF-8 in the message is replaced with U+FFFD on the same
// pass and the result is always a valid string.
//
// C0 is not the whole set (close-review Finding 3). The C1 block carries NEL
// (U+0085), which several log viewers break a line on exactly as they do "\n";
// U+2028/U+2029 are LINE and PARAGRAPH SEPARATOR and do the same; and the bidi
// overrides (U+202A-202E) and isolates (U+2066-2069) reverse the VISUAL order
// of everything after them in a terminal or the dashboard's job row, so a
// substitute video id or an HTTP code can be made to read as something else.
//
// Deliberately NOT all of unicode.Cf: U+200D (ZERO WIDTH JOINER) holds
// multi-person emoji together and appears in ordinary localised text.
func sanitizeErrorDetail(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return ' '
		case r == 0x2028, r == 0x2029:
			return ' '
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return ' '
		}
		return r
	}, s)
}

// innertubeHTTPError formats a non-200 Innertube failure. The "<label> API
// error: HTTP <code>" prefix is load-bearing: worker/probe_classify.go matches
// on the "HTTP <code>" substring to decide whether a probe error is transient,
// so YouTube's own text is APPENDED to it, never substituted for it.
func innertubeHTTPError(clientLabel string, status int, body []byte) error {
	if detail := innertubeErrorDetail(body); detail != "" {
		return fmt.Errorf("%s API error: HTTP %d — %s", clientLabel, status, detail)
	}
	return fmt.Errorf("%s API error: HTTP %d", clientLabel, status)
}

// playerRetryBackoffBase is the first retry delay; attempt n waits
// base<<(n-1), i.e. 1 s, 2 s, 4 s. A var rather than a const purely so tests
// can scale the ladder down instead of sleeping for real seconds; production
// never writes it.
var playerRetryBackoffBase = time.Second

// doRetryRequest performs an HTTP POST with retry logic (up to 4 attempts with
// exponential backoff). Retries on transport errors, partial body reads,
// 5xx/429 responses, and JSON unmarshal failures — all of which have been
// observed as transient CDN issues that would otherwise unnecessarily push
// callers through their full fallback chain.
//
// The backoff is bounded by the CALLER's deadline. Mid-download 403 credential
// recovery runs under min(45 s, MaxTimeout/3) — as little as 10 s at the
// configured floor — and the ladder alone is 7 s, so an unconditional sleep
// could spend the whole budget and then report context.DeadlineExceeded,
// throwing away the HTTP status that is the actual reason the caller is being
// told no. A sleep that would not leave the deadline room for the attempt it
// precedes is not taken at all; the last real error is returned instead. The
// guard reserves a full playerRetryBackoffBase beyond the sleep itself for
// that attempt's HTTP round trip — a bare "does the sleep fit" check would
// still let the *request* race the deadline in the narrow window right after
// a sleep that just barely fit.
//
// videoID is the video that was ASKED for; parsePlayerResponse rejects a
// response about any other one (yt-dlp's _invalid_player_response).
func (p *PlayerAPI) doRetryRequest(ctx context.Context, apiURL string, body []byte, headers map[string]string, ytcfg *YtcfgData, clientLabel string, videoID string) (*VideoInfo, error) {
	var playerURL string
	if ytcfg != nil {
		playerURL = ytcfg.PlayerURL
	}

	var lastErr error
	for attempt := range 4 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s (matching p-retry default factor=2, minTimeout=1000)
			delay := playerRetryBackoffBase << (attempt - 1)
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay+playerRetryBackoffBase {
				// Every retry branch below sets lastErr before continuing, so
				// attempt > 0 always has one to return.
				p.logger.Debug("[PlayerApi] retry budget exhausted, returning the last error",
					slog.String("client", clientLabel),
					slog.Int("attempt", attempt+1),
					slog.Duration("wouldSleep", delay))
				return nil, lastErr
			}
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := apiClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if err != nil {
			// Partial body read (truncated CDN response, transient network
			// hiccup). Retry rather than fall through to the next client.
			lastErr = fmt.Errorf("%s read body: %w", clientLabel, err)
			p.logger.Debug("[PlayerApi] Body read failed, retrying",
				slog.String("client", clientLabel),
				slog.Int("attempt", attempt+1),
				slog.String("error", err.Error()))
			continue
		}

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = innertubeHTTPError(clientLabel, resp.StatusCode, respBody)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, innertubeHTTPError(clientLabel, resp.StatusCode, respBody)
		}

		var data map[string]any
		if err := json.Unmarshal(respBody, &data); err != nil {
			// A 200 with unparseable JSON usually means a truncated or
			// HTML-wrapped edge response; retry so a one-off CDN glitch
			// does not knock us off to the next client for good.
			lastErr = fmt.Errorf("%s parse response: %w", clientLabel, err)
			p.logger.Debug("[PlayerApi] JSON unmarshal failed, retrying",
				slog.String("client", clientLabel),
				slog.Int("attempt", attempt+1),
				slog.Int("bodyLen", len(respBody)),
				slog.String("error", err.Error()))
			continue
		}
		return p.parsePlayerResponse(ctx, data, playerURL, ytcfg, videoID)
	}
	return nil, lastErr
}

func hasAdequateFormats(info *VideoInfo) bool {
	hasVideo := false
	hasAudio := false
	for _, f := range info.Formats {
		if f.IsVideo() {
			hasVideo = true
		}
		if f.IsAudio() {
			hasAudio = true
		}
	}
	return hasVideo && hasAudio
}

func mergeWatchPageMetadata(target *VideoInfo, source *VideoInfo) {
	if source == nil {
		return
	}
	// Always prefer watch page's ScheduledStartTime — its microformat
	// liveBroadcastDetails.startTimestamp is the authoritative source that
	// YouTube updates for rescheduled streams. Other clients may only have
	// liveStreamability.scheduledStartTime (epoch) which YouTube does not
	// always update on reschedule.
	if source.ScheduledStartTime != "" {
		target.ScheduledStartTime = source.ScheduledStartTime
	}
	if target.Description == "" {
		target.Description = source.Description
	}
	if target.ThumbnailURL == "" {
		target.ThumbnailURL = source.ThumbnailURL
	}
	if target.Title == UnknownTitleSentinel && source.Title != UnknownTitleSentinel {
		target.Title = source.Title
	}
	if target.ChannelName == UnknownChannelSentinel && source.ChannelName != UnknownChannelSentinel {
		target.ChannelName = source.ChannelName
	}
	if target.ChannelID == "" {
		target.ChannelID = source.ChannelID
	}
	if target.DashManifestURL == "" {
		target.DashManifestURL = source.DashManifestURL
		target.DashManifestSource = source.DashManifestSource
	}
	if target.HlsManifestURL == "" {
		target.HlsManifestURL = source.HlsManifestURL
		target.HlsManifestSource = source.HlsManifestSource
	}
	if target.PlayerURL == "" {
		target.PlayerURL = source.PlayerURL
	}
	if target.LengthSeconds == nil && source.LengthSeconds != nil {
		target.LengthSeconds = source.LengthSeconds
	}
	if target.EndTimestamp == "" {
		target.EndTimestamp = source.EndTimestamp
	}
}

// stampManifestSources records label — the client's Format.Source label — as
// the source of each manifest URL info carries. The cascade calls it where
// each client result enters it (fetchWithEmbedded stamps its own, since that
// client has one label everywhere), so every later hand-off — a DASH
// adoption into another result, the watch-page merge, the return itself —
// moves a URL together with its client. The live strategies read the source
// to decide whether a manifest takes a WebPO GVS token
// (youtube.IsWebPOSource): a visionos or android_vr manifest must ride bare,
// and an unrecorded one is treated the same way — safe, but it would strip
// the token from a TV or WEB manifest, which is why no site may skip this.
func stampManifestSources(info *VideoInfo, label string) {
	if info == nil {
		return
	}
	if info.DashManifestURL != "" {
		info.DashManifestSource = label
	}
	if info.HlsManifestURL != "" {
		info.HlsManifestSource = label
	}
}
