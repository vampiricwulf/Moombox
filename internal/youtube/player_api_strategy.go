package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// withAttestation stamps the watch page's attestation challenge and the GVS
// PO-token content binding onto the VideoInfo being returned. Applied at
// every GetVideoInfo* return site explicitly — NOT via
// mergeWatchPageMetadata, which several early returns skip or call with a nil
// source.
//
// The binding is resolved here, at the one point that holds all three inputs
// (the experiment flag and datasync ID from ytcfg, the login state from the
// page), so download strategies never re-derive it and cannot drift apart.
func withAttestation(info *VideoInfo, wp *WatchPageResult, videoID string) *VideoInfo {
	if info == nil {
		return info
	}
	if wp != nil {
		info.AttestationChallenge = wp.AttestationChallenge
		info.GvsBinding, info.GvsBindingKind = GvsContentBinding(videoID, wp.Ytcfg, wp.IsLoggedIn, info.ChannelID)
		return info
	}
	// No watch page at all (fetch failed): still produce a usable binding
	// rather than leaving the strategies to invent one.
	info.GvsBinding, info.GvsBindingKind = GvsContentBinding(videoID, nil, false, info.ChannelID)
	return info
}

// ProbeVideoStatus performs a lightweight probe using ANDROID_VR (no cookies needed).
func (p *PlayerAPI) ProbeVideoStatus(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	return p.fetchWithAndroidVR(ctx, videoID, visitorData)
}

// ProbeVideoDate fetches ONLY a video's publish date via one anonymous
// WEB-family player call. The status probes cannot supply dates: microformat
// (the source of publishDate AND liveBroadcastDetails.startTimestamp) is a
// WEB-client response feature, and ANDROID_VR/TV probe responses omit it
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
	// No watch page fetched on this probe-only path, so no attestation
	// challenge is available; GeneratePlayerPoToken falls back to the
	// sidecar's own /att/get flow when it needs a fresh minter.
	info, err := p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, 0, "")
	if err != nil {
		return "", "", err
	}
	return info.PublishedAt, info.PublishedPrecision, nil
}

// ProbeVideoStatusAuthenticated performs a lightweight authenticated status probe
// using the TV_DOWNGRADED client with cookies (no watch page, no STS, no cipher).
// Used for polling members-only upcoming streams. Pass an empty string for
// visitorData when none has been captured yet.
func (p *PlayerAPI) ProbeVideoStatusAuthenticated(ctx context.Context, videoID, visitorData string) (*VideoInfo, error) {
	ytcfg := DefaultYtcfg()
	if visitorData != "" {
		ytcfg.VisitorData = visitorData
	}
	return p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, 0, "")
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

// GetVideoInfoAuthenticated fetches video info using the full multi-client strategy.
func (p *PlayerAPI) GetVideoInfoAuthenticated(ctx context.Context, videoID string) (*VideoInfo, error) {
	// Fetch watch page
	if err := p.auth.SyncCookies(); err != nil {
		p.logger.Warn("[PlayerApi] SyncCookies failed", slog.String("error", err.Error()))
	}
	wp, err := FetchWatchPage(ctx, videoID, p.auth.GetCookieHeader())
	if err != nil {
		p.logger.Warn("[PlayerApi] Could not fetch watch page", slog.String("error", err.Error()))
		wp = &WatchPageResult{Ytcfg: DefaultYtcfg()}
	}

	ytcfg := wp.Ytcfg

	if wp.AttestationChallenge == "" {
		p.logger.Debug("[PlayerApi] no attestation challenge from watch page", "videoID", videoID, "reason", wp.AttestationReason)
	}

	// Capture visitor data for future probe calls
	p.captureVisitorData(ytcfg)

	formatPool := []Format{}

	// Extract STS (signatureTimestamp) from player JS for format decryption
	sts := p.extractSTS(ctx, ytcfg.PlayerURL)

	// Parse watch page player response
	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		wpParsed, _ = p.parsePlayerResponse(ctx, wp.PlayerResponse, ytcfg.PlayerURL, ytcfg)
		if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPageAuth)
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
	authEmb, authEmbErr := p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, wp.AttestationChallenge, false)
	if authEmbErr != nil {
		p.logger.Debug("[PlayerApi] web_embedded (authed cascade) failed", slog.String("error", authEmbErr.Error()))
	} else {
		collectFormats(&formatPool, authEmb.Formats, "web_embedded", AuthLevelWebEmbedded)
	}

	// Try TV client
	result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, sts, wp.AttestationChallenge)
	if err != nil {
		// HTTP error (not a playability error) — log warning and continue to try
		// WEB_CREATOR / VISIONOS / ANDROID_VR instead of returning immediately.
		p.logger.Warn("[PlayerApi] TV client failed, will try other clients", slog.String("error", err.Error()))
		result = &VideoInfo{}
	} else {
		collectFormats(&formatPool, result.Formats, "tv_auth", AuthLevelTVAuth)
		p.logger.Debug("[PlayerApi] TV client result",
			"formats", len(result.Formats),
			"dashManifestUrl", result.DashManifestURL != "",
			"hlsManifestUrl", result.HlsManifestURL != "",
			"streamStatus", result.StreamStatus)
	}

	// Try web_safari client for DASH manifest (preferred over web)
	webResult, webErr := p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, sts, wp.AttestationChallenge)
	webLabel := "web_safari"
	webAuthLevel := AuthLevelWebSafari // preferred over standard web
	if webErr != nil {
		p.logger.Warn("[PlayerApi] web_safari client failed, trying web fallback", slog.String("error", webErr.Error()))
		// Fall back to standard web client
		webResult, webErr = p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts, wp.AttestationChallenge)
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
			"dashManifestUrl", webResult.DashManifestURL != "",
			"hlsManifestUrl", webResult.HlsManifestURL != "",
			"streamStatus", webResult.StreamStatus)
		collectFormats(&formatPool, webResult.Formats, webLabel, webAuthLevel)
		if webResult.DashManifestURL != "" && result.DashManifestURL == "" {
			p.logger.Info("[PlayerApi] Got DASH manifest URL from web client", "videoID", videoID)
			result.DashManifestURL = webResult.DashManifestURL
		}
	}

	// web_embedded as one more DASH source before the ANDROID_VR round trip
	// below. Under the account experiment this is likely stripped too (the
	// embedded call is cookied), but when present it saves the extra call.
	if result.DashManifestURL == "" && authEmbErr == nil && authEmb.DashManifestURL != "" {
		p.logger.Info("[PlayerApi] Got DASH manifest URL from web_embedded", "videoID", videoID)
		result.DashManifestURL = authEmb.DashManifestURL
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
	if result.DashManifestURL == "" &&
		(webErr != nil || webResult.DashManifestURL == "") &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		result.PlayabilityError != PlayabilityMembersOnly &&
		result.PlayabilityError != PlayabilityAgeRestricted &&
		result.PlayabilityError != PlayabilityLoginRequired {

		vrResult, vrErr := p.fetchWithAndroidVR(ctx, videoID, ytcfg.VisitorData)
		if vrErr != nil {
			p.logger.Debug("[PlayerApi] ANDROID_VR DASH fallback failed",
				slog.String("error", vrErr.Error()))
		} else if vrResult.PlayabilityError == PlayabilityOK && vrResult.DashManifestURL != "" {
			p.logger.Info("[PlayerApi] DASH manifest sourced via ANDROID_VR fallback",
				"videoID", videoID, "vrFormats", len(vrResult.Formats))
			result.DashManifestURL = vrResult.DashManifestURL
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
		}
	}

	// Try web_embedded for age-restricted content
	if result.PlayabilityError == PlayabilityAgeRestricted {
		p.logger.Info("[PlayerApi] Age-restricted content detected, trying web_embedded", "videoID", videoID)
		embResult, embErr := p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, wp.AttestationChallenge, true)
		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(formatPool)
			return withAttestation(embResult, wp, videoID), nil
		} else {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	// If TV fails, try WEB_CREATOR
	if result.PlayabilityError == PlayabilityMembersOnly ||
		result.PlayabilityError == PlayabilityLoginRequired ||
		len(result.Formats) == 0 {

		wcResult, wcErr := p.fetchWithClient(ctx, videoID, constants.WebCreatorClient, ytcfg, sts, wp.AttestationChallenge)
		if wcErr != nil {
			p.logger.Warn("[PlayerApi] WEB_CREATOR failed, will try other clients", slog.String("error", wcErr.Error()))
			// Fall through to ANDROID_VR / watch page below
			wcResult = &VideoInfo{}
		}

		collectFormats(&formatPool, wcResult.Formats, "web_creator", AuthLevelWebCreator)

		// Try the cookieless fallback clients if WEB_CREATOR also fails or has
		// inadequate formats. VISIONOS first (yt-dlp's lead default since
		// 2026.08.19), then ANDROID_VR — retained behind it because upstream's
		// all-formats-403 enforcement on android_vr is selective (verified
		// still fully working from here 2026-08-24) and it covers videos
		// visionos cannot ("Made for kids" is unavailable on visionos).
		if wcResult.PlayabilityError == PlayabilityLoginRequired ||
			wcResult.StreamStatus == StreamNotAStream ||
			len(wcResult.Formats) == 0 ||
			!hasAdequateFormats(wcResult) {

			if wcResult.PlayabilityError != PlayabilityMembersOnly {
				if cfResult := p.tryCookielessFallbacks(ctx, videoID, ytcfg.VisitorData, &formatPool); cfResult != nil {
					mergeWatchPageMetadata(cfResult, wpParsed)
					cfResult.Formats = deduplicateFormats(formatPool)
					return withAttestation(cfResult, wp, videoID), nil
				}
			}

			// Fall back to watch page if all clients failed
			if wpParsed != nil {
				wpParsed.Formats = deduplicateFormats(formatPool)
				return withAttestation(wpParsed, wp, videoID), nil
			}
		}

		if wpParsed != nil {
			mergeWatchPageMetadata(wcResult, wpParsed)
		}
		wcResult.Formats = deduplicateFormats(formatPool)
		return withAttestation(wcResult, wp, videoID), nil
	}

	return withAttestation(finalizeVideoInfo(result, wpParsed, formatPool), wp, videoID), nil
}

// GetVideoInfoPublic fetches video info without authentication.
func (p *PlayerAPI) GetVideoInfoPublic(ctx context.Context, videoID string) (*VideoInfo, error) {
	wp, err := FetchWatchPage(ctx, videoID, "")
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

	// Extract STS for public path too
	stsPublic := p.extractSTS(ctx, wp.Ytcfg.PlayerURL)

	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		wpParsed, _ = p.parsePlayerResponse(ctx, wp.PlayerResponse, wp.Ytcfg.PlayerURL, wp.Ytcfg)
		if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPagePublic)
		}
	}

	result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, wp.Ytcfg, stsPublic, wp.AttestationChallenge)
	if err != nil {
		if wpParsed != nil {
			wpParsed.Formats = deduplicateFormats(formatPool)
			return withAttestation(wpParsed, wp, videoID), nil
		}
		return nil, err
	}
	collectFormats(&formatPool, result.Formats, "tv_public", AuthLevelTVPublic)

	// Try web_embedded for age-restricted content (public path)
	if result.PlayabilityError == PlayabilityAgeRestricted {
		p.logger.Info("[PlayerApi] Age-restricted content detected (public), trying web_embedded", "videoID", videoID)
		embResult, embErr := p.fetchWithEmbedded(ctx, videoID, wp.Ytcfg, stsPublic, wp.AttestationChallenge, true)
		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(formatPool)
			return withAttestation(embResult, wp, videoID), nil
		} else {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	if result.PlayabilityError == PlayabilityLoginRequired || len(result.Formats) == 0 || !hasAdequateFormats(result) {
		// VISIONOS first, ANDROID_VR second — same rationale as the
		// authenticated path's cookieless fallback chain.
		if cfResult := p.tryCookielessFallbacks(ctx, videoID, wp.Ytcfg.VisitorData, &formatPool); cfResult != nil {
			mergeWatchPageMetadata(cfResult, wpParsed)
			cfResult.Formats = deduplicateFormats(formatPool)
			return withAttestation(cfResult, wp, videoID), nil
		}

		if wpParsed != nil {
			wpParsed.Formats = deduplicateFormats(formatPool)
			return withAttestation(wpParsed, wp, videoID), nil
		}
	}

	// DASH-only enrichment via ANDROID_VR — mirrors the authenticated path
	// (see the comment there for why android_vr is retained despite yt-dlp
	// dropping it: selective enforcement, no cookieless live-DASH substitute).
	// Public live streams hit by the YouTube account-based experiment that
	// strips dashManifestUrl from the cookied/TV path also land here.
	if result.DashManifestURL == "" &&
		(result.StreamStatus == StreamLive || result.StreamStatus == StreamUpcoming) &&
		result.PlayabilityError != PlayabilityMembersOnly &&
		result.PlayabilityError != PlayabilityAgeRestricted &&
		result.PlayabilityError != PlayabilityLoginRequired {
		vrResult, vrErr := p.fetchWithAndroidVR(ctx, videoID, wp.Ytcfg.VisitorData)
		if vrErr != nil {
			p.logger.Debug("[PlayerApi] ANDROID_VR DASH fallback (public) failed",
				slog.String("error", vrErr.Error()))
		} else if vrResult.PlayabilityError == PlayabilityOK && vrResult.DashManifestURL != "" {
			p.logger.Info("[PlayerApi] DASH manifest sourced via ANDROID_VR fallback (public)",
				"videoID", videoID, "vrFormats", len(vrResult.Formats))
			result.DashManifestURL = vrResult.DashManifestURL
			collectFormats(&formatPool, vrResult.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
		}
	}

	return withAttestation(finalizeVideoInfo(result, wpParsed, formatPool), wp, videoID), nil
}

// finalizeVideoInfo applies the not_a_stream override + merge + dedup tail
// shared by GetVideoInfoAuthenticated and GetVideoInfoPublic. When the TV
// client classified the video as not_a_stream but the watch page disagreed,
// the watch page's classification wins; if TV still produced adequate
// formats they are kept, otherwise the watch-page parse is returned wholesale
// (audit D3).
func finalizeVideoInfo(result, wpParsed *VideoInfo, formatPool []Format) *VideoInfo {
	if result.StreamStatus == StreamNotAStream && wpParsed != nil && wpParsed.StreamStatus != StreamNotAStream {
		if hasAdequateFormats(result) {
			result.StreamStatus = wpParsed.StreamStatus
			result.IsLive = wpParsed.IsLive
			result.IsUpcoming = wpParsed.IsUpcoming
			result.IsPostLiveDVR = wpParsed.IsPostLiveDVR
			mergeWatchPageMetadata(result, wpParsed)
			result.Formats = deduplicateFormats(formatPool)
			return result
		}
		wpParsed.Formats = deduplicateFormats(formatPool)
		return wpParsed
	}

	mergeWatchPageMetadata(result, wpParsed)
	result.Formats = deduplicateFormats(formatPool)
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

func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int, challenge string) (*VideoInfo, error) {
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
	if p.potProvider != nil && clientAcceptsPlayerPoToken(client) && ytcfg != nil && ytcfg.VisitorData != "" {
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

	return p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "Innertube")
}

// fetchWithCookielessClient performs a bare player request for the cookieless
// fallback clients (ANDROID_VR, VISIONOS): no auth headers, no STS, no PO
// token — just the client context plus visitor data when available.
func (p *PlayerAPI) fetchWithCookielessClient(ctx context.Context, videoID, visitorData string, client constants.YouTubeClientConfig) (*VideoInfo, error) {
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

	return p.doRetryRequest(ctx, apiURL, body, headers, nil, client.ClientName)
}

func (p *PlayerAPI) fetchWithAndroidVR(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	return p.fetchWithCookielessClient(ctx, videoID, visitorData, constants.AndroidVRClient)
}

// tryCookielessFallbacks runs the cookieless fallback chain — VISIONOS, then
// ANDROID_VR — collecting every fetched format into the pool at its tier.
// Returns the first result with OK playability and adequate formats, or nil
// when neither client produced one (the pool still holds whatever partial
// formats were fetched). Shared by the authenticated and public paths.
//
// One wrinkle keeps the chain from silently costing a capability: VISIONOS
// serves HLS but NEVER a dashManifestUrl for live streams (verified
// 2026-08-24), while ANDROID_VR does. Returning the first adequate result
// would therefore hand back an HLS-only live stream and skip the callers'
// DASH-enrichment step, losing --live-from-start segment addressability for
// every anonymous live capture — caught by TestLivePublicExtraction the day
// this chain landed. So for live/upcoming content missing a DASH manifest,
// the chain keeps going and adopts a later client's manifest into the
// already-chosen result rather than stopping early.
func (p *PlayerAPI) tryCookielessFallbacks(ctx context.Context, videoID, visitorData string, formatPool *[]Format) *VideoInfo {
	var chosen *VideoInfo
	for _, fb := range []struct {
		client constants.YouTubeClientConfig
		label  string
		level  int
	}{
		{constants.VisionOSClient, "visionos", AuthLevelVisionOS},
		{constants.AndroidVRClient, "android_vr", AuthLevelAndroidVR},
	} {
		fbResult, fbErr := p.fetchWithCookielessClient(ctx, videoID, visitorData, fb.client)
		if fbErr != nil {
			p.logger.Debug("[PlayerApi] cookieless fallback failed",
				slog.String("client", fb.label), slog.String("error", fbErr.Error()))
			continue
		}
		collectFormats(formatPool, fbResult.Formats, fb.label, fb.level)

		if chosen != nil {
			// Already have a usable result; this client is only still being
			// consulted for a DASH manifest it might carry.
			if fbResult.PlayabilityError == PlayabilityOK && fbResult.DashManifestURL != "" {
				p.logger.Info("[PlayerApi] DASH manifest sourced from cookieless fallback",
					"videoID", videoID, "client", fb.label)
				chosen.DashManifestURL = fbResult.DashManifestURL
			}
			break
		}

		if fbResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(fbResult) {
			chosen = fbResult
			needsDash := (fbResult.StreamStatus == StreamLive || fbResult.StreamStatus == StreamUpcoming) &&
				fbResult.DashManifestURL == ""
			if !needsDash {
				break
			}
		}
	}
	return chosen
}

// fetchWithEmbedded performs a WEB_EMBEDDED_PLAYER request. fetchEmbedPage
// controls the extra embed-page round trip for encryptedHostFlags: the
// age-restriction bypass wants it, while the plain authed-cascade call
// matches yt-dlp's shape (thirdParty.embedUrl only, no embed-page fetch).
func (p *PlayerAPI) fetchWithEmbedded(ctx context.Context, videoID string, ytcfg *YtcfgData, sts int, challenge string, fetchEmbedPage bool) (*VideoInfo, error) {
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

	return p.doRetryRequest(ctx, apiURL, body, headers, ytcfg, "WEB_EMBEDDED")
}

// doRetryRequest performs an HTTP POST with retry logic (up to 4 attempts with
// exponential backoff). Retries on transport errors, partial body reads,
// 5xx/429 responses, and JSON unmarshal failures — all of which have been
// observed as transient CDN issues that would otherwise unnecessarily push
// callers through their full fallback chain.
func (p *PlayerAPI) doRetryRequest(ctx context.Context, apiURL string, body []byte, headers map[string]string, ytcfg *YtcfgData, clientLabel string) (*VideoInfo, error) {
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
			delay := 1 << (attempt - 1)
			if err := utils.Sleep(ctx, time.Duration(delay)*time.Second); err != nil {
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
			lastErr = fmt.Errorf("%s API error: HTTP %d", clientLabel, resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s API error: HTTP %d", clientLabel, resp.StatusCode)
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
		return p.parsePlayerResponse(ctx, data, playerURL, ytcfg)
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
	}
	if target.HlsManifestURL == "" {
		target.HlsManifestURL = source.HlsManifestURL
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
