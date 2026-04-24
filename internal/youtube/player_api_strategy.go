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

// ProbeVideoStatus performs a lightweight probe using ANDROID_VR (no cookies needed).
func (p *PlayerAPI) ProbeVideoStatus(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	return p.fetchWithAndroidVR(ctx, videoID, visitorData)
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
	return p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, 0)
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

	// Capture visitor data for future probe calls
	if ytcfg.VisitorData != "" && p.OnVisitorData != nil {
		p.OnVisitorData(ytcfg.VisitorData)
	}

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

	// Try TV client
	result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, sts)
	if err != nil {
		// HTTP error (not a playability error) — log warning and continue to try
		// WEB_CREATOR / ANDROID_VR instead of returning immediately.
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
	webResult, webErr := p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, sts)
	webLabel := "web_safari"
	webAuthLevel := AuthLevelWebSafari // preferred over standard web
	if webErr != nil {
		p.logger.Warn("[PlayerApi] web_safari client failed, trying web fallback", slog.String("error", webErr.Error()))
		// Fall back to standard web client
		webResult, webErr = p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts)
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

	// Try web_embedded for age-restricted content
	if result.PlayabilityError == PlayabilityAgeRestricted {
		p.logger.Info("[PlayerApi] Age-restricted content detected, trying web_embedded", "videoID", videoID)
		embResult, embErr := p.fetchWithEmbedded(ctx, videoID, ytcfg, sts)
		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(formatPool)
			return embResult, nil
		} else {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	// If TV fails, try WEB_CREATOR
	if result.PlayabilityError == PlayabilityMembersOnly ||
		result.PlayabilityError == PlayabilityLoginRequired ||
		len(result.Formats) == 0 {

		wcResult, wcErr := p.fetchWithClient(ctx, videoID, constants.WebCreatorClient, ytcfg, sts)
		if wcErr != nil {
			p.logger.Warn("[PlayerApi] WEB_CREATOR failed, will try other clients", slog.String("error", wcErr.Error()))
			// Fall through to ANDROID_VR / watch page below
			wcResult = &VideoInfo{}
		}

		collectFormats(&formatPool, wcResult.Formats, "web_creator", AuthLevelWebCreator)

		// Try ANDROID_VR if WEB_CREATOR also fails or has inadequate formats
		if wcResult.PlayabilityError == PlayabilityLoginRequired ||
			wcResult.StreamStatus == StreamNotAStream ||
			len(wcResult.Formats) == 0 ||
			!hasAdequateFormats(wcResult) {

			if wcResult.PlayabilityError != PlayabilityMembersOnly {
				vrResult, vrErr := p.fetchWithAndroidVR(ctx, videoID, ytcfg.VisitorData)
				if vrErr == nil {
					collectFormats(&formatPool, vrResult.Formats, "android_vr", AuthLevelAndroidVR)
					if vrResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(vrResult) {
						mergeWatchPageMetadata(vrResult, wpParsed)
						vrResult.Formats = deduplicateFormats(formatPool)
						return vrResult, nil
					}
				}
			}

			// Fall back to watch page if all clients failed
			if wpParsed != nil {
				wpParsed.Formats = deduplicateFormats(formatPool)
				return wpParsed, nil
			}
		}

		if wpParsed != nil {
			mergeWatchPageMetadata(wcResult, wpParsed)
		}
		wcResult.Formats = deduplicateFormats(formatPool)
		return wcResult, nil
	}

	return finalizeVideoInfo(result, wpParsed, formatPool), nil
}

// GetVideoInfoPublic fetches video info without authentication.
func (p *PlayerAPI) GetVideoInfoPublic(ctx context.Context, videoID string) (*VideoInfo, error) {
	wp, err := FetchWatchPage(ctx, videoID, "")
	if err != nil {
		wp = &WatchPageResult{Ytcfg: DefaultYtcfg()}
	}

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

	result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, wp.Ytcfg, stsPublic)
	if err != nil {
		if wpParsed != nil {
			wpParsed.Formats = deduplicateFormats(formatPool)
			return wpParsed, nil
		}
		return nil, err
	}
	collectFormats(&formatPool, result.Formats, "tv_public", AuthLevelTVPublic)

	// Try web_embedded for age-restricted content (public path)
	if result.PlayabilityError == PlayabilityAgeRestricted {
		p.logger.Info("[PlayerApi] Age-restricted content detected (public), trying web_embedded", "videoID", videoID)
		embResult, embErr := p.fetchWithEmbedded(ctx, videoID, wp.Ytcfg, stsPublic)
		if embErr != nil {
			p.logger.Warn("[PlayerApi] web_embedded failed", slog.String("error", embErr.Error()))
		} else if embResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(embResult) {
			p.logger.Info("[PlayerApi] web_embedded succeeded for age-restricted content", "videoID", videoID)
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
			mergeWatchPageMetadata(embResult, wpParsed)
			embResult.Formats = deduplicateFormats(formatPool)
			return embResult, nil
		} else {
			collectFormats(&formatPool, embResult.Formats, "web_embedded", AuthLevelWebEmbedded)
		}
	}

	if result.PlayabilityError == PlayabilityLoginRequired || len(result.Formats) == 0 || !hasAdequateFormats(result) {
		vrResult, vrErr := p.fetchWithAndroidVR(ctx, videoID, wp.Ytcfg.VisitorData)
		if vrErr == nil {
			collectFormats(&formatPool, vrResult.Formats, "android_vr", AuthLevelAndroidVR)
			if vrResult.PlayabilityError == PlayabilityOK && hasAdequateFormats(vrResult) {
				mergeWatchPageMetadata(vrResult, wpParsed)
				vrResult.Formats = deduplicateFormats(formatPool)
				return vrResult, nil
			}
		}

		if wpParsed != nil {
			wpParsed.Formats = deduplicateFormats(formatPool)
			return wpParsed, nil
		}
	}

	return finalizeVideoInfo(result, wpParsed, formatPool), nil
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

func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.apiKey)
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
	// Failure is non-fatal — the request still runs without it; PLAYER_PO_TOKEN_POLICY
	// is not yet required upstream, but supplying a token is future-proof and matches
	// yt-dlp's behaviour.
	if p.potProvider != nil && clientAcceptsPlayerPoToken(client) && ytcfg != nil && ytcfg.VisitorData != "" {
		if poToken, err := p.potProvider.GeneratePoTokenString(ctx, ytcfg.VisitorData, false); err == nil && poToken != "" {
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

func (p *PlayerAPI) fetchWithAndroidVR(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.apiKey)

	clientCtx := make(map[string]any, len(constants.AndroidVRClient.Context))
	maps.Copy(clientCtx, constants.AndroidVRClient.Context)
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
		"User-Agent":              constants.AndroidVRClient.UserAgent,
		"X-YouTube-Client-Name":   constants.AndroidVRClient.ClientID,
		"X-YouTube-Client-Version": constants.AndroidVRClient.ClientVersion,
		"Origin":                  "https://www.youtube.com",
	}
	if visitorData != "" {
		headers["X-Goog-Visitor-Id"] = visitorData
	}

	return p.doRetryRequest(ctx, apiURL, body, headers, nil, "ANDROID_VR")
}

func (p *PlayerAPI) fetchWithEmbedded(ctx context.Context, videoID string, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.apiKey)

	// Fetch embed page for encryptedHostFlags
	embedResult, err := FetchEmbedPage(ctx, videoID)
	if err != nil {
		p.logger.Warn("[PlayerApi] Failed to fetch embed page", slog.String("error", err.Error()))
		// Continue without encryptedHostFlags — it may still work
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

	// Inject PO token for WEB_EMBEDDED (same rationale as fetchWithClient).
	if p.potProvider != nil && ytcfg != nil && ytcfg.VisitorData != "" {
		if poToken, err := p.potProvider.GeneratePoTokenString(ctx, ytcfg.VisitorData, false); err == nil && poToken != "" {
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
