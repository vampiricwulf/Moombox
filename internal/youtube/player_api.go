package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

var pathNParamRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// apiClient is a shared HTTP client with a 30s timeout (matching TS fetchWithTimeout).
var apiClient = &http.Client{Timeout: 30 * time.Second}

// PlayerAPI handles interactions with YouTube's Innertube player API.
type PlayerAPI struct {
	auth         *Auth
	apiKey       string
	cipherSolver *cipher.Solver
	// OnVisitorData is called when visitor data is extracted from a watch page.
	OnVisitorData func(visitorData string)
	logger        interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// NewPlayerAPI creates a new PlayerAPI instance.
func NewPlayerAPI(auth *Auth, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) *PlayerAPI {
	return &PlayerAPI{
		auth:   auth,
		apiKey: constants.DefaultAPIKey,
		logger: logger,
	}
}

// SetCipherSolver sets the cipher solver for signature/n-param decryption.
func (p *PlayerAPI) SetCipherSolver(solver *cipher.Solver) {
	p.cipherSolver = solver
}

// SetAPIKey updates the API key used for YouTube API requests.
func (p *PlayerAPI) SetAPIKey(key string) {
	if key != "" {
		p.apiKey = key
	}
}

// ProbeVideoStatus performs a lightweight probe using ANDROID_VR (no cookies needed).
func (p *PlayerAPI) ProbeVideoStatus(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	return p.fetchWithAndroidVR(ctx, videoID, visitorData)
}

// ProbeVideoStatusAuthenticated performs a lightweight authenticated status probe
// using the TV_DOWNGRADED client with cookies (no watch page, no STS, no cipher).
// Used for polling members-only upcoming streams.
func (p *PlayerAPI) ProbeVideoStatusAuthenticated(ctx context.Context, videoID string, visitorData ...string) (*VideoInfo, error) {
	ytcfg := DefaultYtcfg()
	if len(visitorData) > 0 && visitorData[0] != "" {
		ytcfg.VisitorData = visitorData[0]
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
	var sts int
	if ytcfg.PlayerURL != "" && p.cipherSolver != nil {
		stsStr, stsErr := p.cipherSolver.GetSts(ctx, ytcfg.PlayerURL)
		if stsErr == nil && stsStr != "" {
			if n, err := strconv.Atoi(stsStr); err == nil {
				sts = n
				p.logger.Debug("[PlayerApi] Got signature timestamp", "sts", sts)
			}
		}
	}

	// Parse watch page player response
	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		wpParsed, _ = p.parsePlayerResponse(ctx, wp.PlayerResponse, ytcfg.PlayerURL, ytcfg)
		if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPage)
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

	// Try WEB client for DASH manifest
	webResult, webErr := p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts)
	if webErr != nil {
		p.logger.Warn("[PlayerApi] WEB client failed", slog.String("error", webErr.Error()))
	} else {
		p.logger.Debug("[PlayerApi] WEB client result",
			"formats", len(webResult.Formats),
			"dashManifestUrl", webResult.DashManifestURL != "",
			"hlsManifestUrl", webResult.HlsManifestURL != "",
			"streamStatus", webResult.StreamStatus)
		collectFormats(&formatPool, webResult.Formats, "web", AuthLevelWeb)
		if webResult.DashManifestURL != "" && result.DashManifestURL == "" {
			p.logger.Info("[PlayerApi] Got DASH manifest URL from WEB client", "videoID", videoID)
			result.DashManifestURL = webResult.DashManifestURL
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

	// If TV result looks like not_a_stream but watch page says otherwise,
	// use watch page's stream classification but keep TV's formats if adequate
	if result.StreamStatus == StreamNotAStream && wpParsed != nil {
		if wpParsed.StreamStatus != StreamNotAStream {
			if hasAdequateFormats(result) {
				// TV has good formats — keep them, override stream classification
				result.StreamStatus = wpParsed.StreamStatus
				result.IsLive = wpParsed.IsLive
				result.IsUpcoming = wpParsed.IsUpcoming
				result.IsPostLiveDVR = wpParsed.IsPostLiveDVR
				mergeWatchPageMetadata(result, wpParsed)
				result.Formats = deduplicateFormats(formatPool)
				return result, nil
			}
			// TV formats inadequate — fall back to watch page entirely
			wpParsed.Formats = deduplicateFormats(formatPool)
			return wpParsed, nil
		}
	}

	// Merge watch page metadata
	mergeWatchPageMetadata(result, wpParsed)
	result.Formats = deduplicateFormats(formatPool)
	return result, nil
}

// GetVideoInfoPublic fetches video info without authentication.
func (p *PlayerAPI) GetVideoInfoPublic(ctx context.Context, videoID string) (*VideoInfo, error) {
	wp, err := FetchWatchPage(ctx, videoID, "")
	if err != nil {
		wp = &WatchPageResult{Ytcfg: DefaultYtcfg()}
	}

	formatPool := []Format{}

	// Extract STS for public path too
	var stsPublic int
	if wp.Ytcfg.PlayerURL != "" && p.cipherSolver != nil {
		stsStr, stsErr := p.cipherSolver.GetSts(ctx, wp.Ytcfg.PlayerURL)
		if stsErr == nil && stsStr != "" {
			if n, err := strconv.Atoi(stsStr); err == nil {
				stsPublic = n
			}
		}
	}

	var wpParsed *VideoInfo
	if wp.PlayerResponse != nil {
		wpParsed, _ = p.parsePlayerResponse(ctx, wp.PlayerResponse, wp.Ytcfg.PlayerURL, wp.Ytcfg)
		if wpParsed != nil {
			collectFormats(&formatPool, wpParsed.Formats, "watch_page", AuthLevelWatchPage)
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

	// not_a_stream override: if TV says not_a_stream but watch page disagrees
	if result.StreamStatus == StreamNotAStream && wpParsed != nil {
		if wpParsed.StreamStatus != StreamNotAStream {
			if hasAdequateFormats(result) {
				result.StreamStatus = wpParsed.StreamStatus
				result.IsLive = wpParsed.IsLive
				result.IsUpcoming = wpParsed.IsUpcoming
				result.IsPostLiveDVR = wpParsed.IsPostLiveDVR
				mergeWatchPageMetadata(result, wpParsed)
				result.Formats = deduplicateFormats(formatPool)
				return result, nil
			}
			wpParsed.Formats = deduplicateFormats(formatPool)
			return wpParsed, nil
		}
	}

	mergeWatchPageMetadata(result, wpParsed)
	result.Formats = deduplicateFormats(formatPool)
	return result, nil
}

func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.apiKey)
	headers := p.auth.GenerateAPIHeaders(client, ytcfg)

	// Build client context with optional visitorData
	clientCtx := make(map[string]interface{})
	for k, v := range client.Context {
		clientCtx[k] = v
	}
	if ytcfg != nil && ytcfg.VisitorData != "" {
		clientCtx["visitorData"] = ytcfg.VisitorData
	}

	postData := map[string]interface{}{
		"context": map[string]interface{}{
			"client": clientCtx,
		},
		"videoId":        videoID,
		"contentCheckOk": true,
		"racyCheckOk":    true,
	}

	// Always include playbackContext with STS when available
	pbCtx := map[string]interface{}{
		"html5Preference": "HTML5_PREF_WANTS",
	}
	if sts > 0 {
		pbCtx["signatureTimestamp"] = sts
	}
	postData["playbackContext"] = map[string]interface{}{
		"contentPlaybackContext": pbCtx,
	}

	body, err := json.Marshal(postData)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	var req *http.Request
	// Retry on 5xx/429 errors (matching TS fetchWithTimeout + p-retry behavior)
	var data map[string]interface{}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s (matching p-retry default factor=2, minTimeout=1000)
			delay := time.Duration(1<<(attempt-1)) * time.Second
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
		// Re-create request with fresh reader on each attempt (body bytes are reused)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
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

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("Innertube API error: HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Innertube API error: HTTP %d", resp.StatusCode)
		}

		if err := json.Unmarshal(respBody, &data); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}
		var playerURL string
		if ytcfg != nil {
			playerURL = ytcfg.PlayerURL
		}
		return p.parsePlayerResponse(ctx, data, playerURL, ytcfg)
	}
	return nil, lastErr
}

func (p *PlayerAPI) fetchWithAndroidVR(ctx context.Context, videoID string, visitorData string) (*VideoInfo, error) {
	apiURL := fmt.Sprintf("%s/player?key=%s", constants.YouTubeURLs.API, p.apiKey)

	clientCtx := make(map[string]interface{})
	for k, v := range constants.AndroidVRClient.Context {
		clientCtx[k] = v
	}
	if visitorData != "" {
		clientCtx["visitorData"] = visitorData
	}

	postData := map[string]interface{}{
		"context": map[string]interface{}{
			"client": clientCtx,
		},
		"videoId":        videoID,
		"contentCheckOk": true,
		"racyCheckOk":    true,
		"playbackContext": map[string]interface{}{
			"contentPlaybackContext": map[string]interface{}{
				"html5Preference": "HTML5_PREF_WANTS",
			},
		},
	}

	body, err := json.Marshal(postData)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Retry on 5xx/429 errors (matching TS fetchWithTimeout + p-retry behavior)
	var data map[string]interface{}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s (matching p-retry default factor=2, minTimeout=1000)
			delay := time.Duration(1<<(attempt-1)) * time.Second
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, err
			}
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}

		resp, err := apiClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("ANDROID_VR API error: HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ANDROID_VR API error: HTTP %d", resp.StatusCode)
		}

		if err := json.Unmarshal(respBody, &data); err != nil {
			return nil, err
		}
		return p.parsePlayerResponse(ctx, data, "", nil)
	}
	return nil, lastErr
}

func (p *PlayerAPI) parsePlayerResponse(ctx context.Context, data map[string]interface{}, playerURL string, ytcfg *YtcfgData) (*VideoInfo, error) {
	videoDetails, _ := data["videoDetails"].(map[string]interface{})
	streamingData, _ := data["streamingData"].(map[string]interface{})
	playabilityStatus, _ := data["playabilityStatus"].(map[string]interface{})
	microformat, _ := getNestedMap(data, "microformat", "playerMicroformatRenderer")

	// Parse playability
	playErr, playReason := parsePlayabilityStatus(playabilityStatus)

	// Parse formats (with cipher decryption if available)
	formats := p.parseFormatsWithCipher(ctx, streamingData, playerURL)

	// Stream classification
	streamStatus, isLive, isUpcoming, isPostLiveDVR := classifyStream(videoDetails, playabilityStatus, microformat, len(formats) > 0)

	// Metadata
	title := getStr(videoDetails, "title")
	channelName := getStr(videoDetails, "author")
	channelID := getStr(videoDetails, "channelId")
	description := getStr(videoDetails, "shortDescription")

	if title == "" && ytcfg != nil {
		title = ytcfg.Title
	}
	if channelName == "" && ytcfg != nil {
		channelName = ytcfg.Author
	}
	if channelID == "" && ytcfg != nil {
		channelID = ytcfg.ChannelID
	}
	if description == "" && ytcfg != nil {
		description = ytcfg.Description
	}
	if title == "" {
		title = "Unknown Title"
	}
	if channelName == "" {
		channelName = "Unknown Channel"
	}

	// Thumbnail
	thumbnailURL := ""
	if ytcfg != nil {
		thumbnailURL = ytcfg.ThumbnailURL
	}
	if thumbs, ok := getNestedSlice(videoDetails, "thumbnail", "thumbnails"); ok && len(thumbs) > 0 {
		if last, ok := thumbs[len(thumbs)-1].(map[string]interface{}); ok {
			if url, ok := last["url"].(string); ok {
				thumbnailURL = url
			}
		}
	}

	// Scheduled start time
	scheduledStartTime := ""
	if lbd, ok := getNestedMap(microformat, "liveBroadcastDetails"); ok {
		scheduledStartTime = getStr(lbd, "startTimestamp")
	}
	if scheduledStartTime == "" {
		// Try liveStreamability
		if epoch := getDeepStr(playabilityStatus, "liveStreamability", "liveStreamabilityRenderer", "offlineSlate", "liveStreamOfflineSlateRenderer", "scheduledStartTime"); epoch != "" {
			if ts, err := strconv.ParseInt(epoch, 10, 64); err == nil {
				scheduledStartTime = time.Unix(ts, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	// Fallback to uploadDate / publishDate from microformat
	if scheduledStartTime == "" {
		if ud := getStr(microformat, "uploadDate"); ud != "" {
			scheduledStartTime = ud
		} else if pd := getStr(microformat, "publishDate"); pd != "" {
			scheduledStartTime = pd
		}
	}

	// Length
	var lengthSeconds *int
	if ls := getStr(videoDetails, "lengthSeconds"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 {
			lengthSeconds = &n
		}
	}

	// End timestamp
	endTimestamp := ""
	if lbd, ok := getNestedMap(microformat, "liveBroadcastDetails"); ok {
		endTimestamp = getStr(lbd, "endTimestamp")
	}

	return &VideoInfo{
		Title:              title,
		ChannelName:        channelName,
		ChannelID:          channelID,
		Description:        description,
		ThumbnailURL:       thumbnailURL,
		Formats:            formats,
		PlayerURL:          playerURL,
		StreamStatus:       streamStatus,
		IsLive:             isLive,
		IsUpcoming:         isUpcoming,
		IsPostLiveDVR:      isPostLiveDVR,
		LengthSeconds:      lengthSeconds,
		EndTimestamp:        endTimestamp,
		ScheduledStartTime: scheduledStartTime,
		DashManifestURL:    getStr(streamingData, "dashManifestUrl"),
		HlsManifestURL:     getStr(streamingData, "hlsManifestUrl"),
		PlayabilityError:   playErr,
		PlayabilityReason:  playReason,
	}, nil
}

// parseFormatsWithCipher parses formats from streaming data, decrypting signatures and
// n-parameters using the cipher solver when available.
func (p *PlayerAPI) parseFormatsWithCipher(ctx context.Context, streamingData map[string]interface{}, playerURL string) []Format {
	if streamingData == nil {
		return nil
	}

	var formats []Format
	for _, key := range []string{"formats", "adaptiveFormats"} {
		arr, ok := streamingData[key].([]interface{})
		if !ok {
			continue
		}
		for _, item := range arr {
			f, ok := item.(map[string]interface{})
			if !ok {
				continue
			}

			formatURL := getStr(f, "url")
			sigCipher := getStr(f, "signatureCipher")

			// Handle signatureCipher (older format without direct URL)
			if formatURL == "" && sigCipher != "" && playerURL != "" && p.cipherSolver != nil {
				decrypted := p.decryptSignatureCipher(ctx, sigCipher, playerURL)
				if decrypted != "" {
					formatURL = decrypted
				}
			}

			if formatURL == "" {
				continue
			}

			// Decrypt n-parameter to avoid throttling
			if playerURL != "" && p.cipherSolver != nil {
				formatURL = p.decryptNParam(ctx, formatURL, playerURL)
			}

			format := Format{
				Itag:     getInt(f, "itag"),
				URL:      formatURL,
				MimeType: getStr(f, "mimeType"),
				Bitrate:  getInt(f, "bitrate"),
			}

			if w := getInt(f, "width"); w > 0 {
				format.Width = &w
			}
			if h := getInt(f, "height"); h > 0 {
				format.Height = &h
			}
			if fps := getInt(f, "fps"); fps > 0 {
				format.Fps = &fps
			}

			format.ContentLength = getStr(f, "contentLength")
			format.QualityLabel = getStr(f, "qualityLabel")
			format.AudioQuality = getStr(f, "audioQuality")
			format.AudioSampleRate = getStr(f, "audioSampleRate")

			formats = append(formats, format)
		}
	}
	return formats
}

// decryptSignatureCipher decodes a signatureCipher string, decrypts the signature,
// and returns the fully resolved URL.
func (p *PlayerAPI) decryptSignatureCipher(ctx context.Context, sigCipher, playerURL string) string {
	params, err := url.ParseQuery(sigCipher)
	if err != nil {
		p.logger.Debug("[PlayerApi] Failed to parse signatureCipher", slog.String("error", err.Error()))
		return ""
	}

	streamURL := params.Get("url")
	encSig := params.Get("s")
	sigKey := params.Get("sp")
	if sigKey == "" {
		sigKey = "signature"
	}

	if streamURL == "" || encSig == "" {
		return ""
	}

	solvers, err := p.cipherSolver.GetSolvers(ctx, playerURL)
	if err != nil {
		p.logger.Warn("[PlayerApi] Failed to get cipher solvers", slog.String("error", err.Error()))
		return ""
	}

	if solvers.Sig == nil {
		p.logger.Debug("[PlayerApi] No signature solver available")
		return ""
	}

	decryptedSig, err := solvers.DecryptSig(encSig)
	if err != nil {
		p.logger.Warn("[PlayerApi] Signature decryption failed", slog.String("error", err.Error()))
		return ""
	}

	// Append the decrypted signature to the stream URL using string append
	// to preserve original parameter order (Go's url.Values.Encode() sorts
	// parameters alphabetically, which breaks YouTube's URL signature).
	sep := "&"
	if !strings.Contains(streamURL, "?") {
		sep = "?"
	}
	finalURL := streamURL + sep + url.QueryEscape(sigKey) + "=" + url.QueryEscape(decryptedSig)

	// NOTE: Do NOT decrypt the n-parameter here — the caller
	// (parseFormatsWithCipher) always calls decryptNParam() on the returned
	// URL. Decrypting here would cause double-decryption, corrupting the
	// n-param and triggering HTTP 403 from YouTube's CDN.

	return finalURL
}

// decryptNParam decrypts the n-parameter in a URL to avoid throttling.
// Uses string replacement to preserve original URL parameter order —
// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
// YouTube's URL signature verification and causes HTTP 403.
func (p *PlayerAPI) decryptNParam(ctx context.Context, rawURL, playerURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	nParam := u.Query().Get("n")
	if nParam == "" {
		return rawURL
	}

	solvers, err := p.cipherSolver.GetSolvers(ctx, playerURL)
	if err != nil {
		p.logger.Warn("[PlayerApi] N-param solver unavailable", slog.String("error", err.Error()))
		return rawURL
	}

	decryptedN, err := solvers.DecryptN(nParam)
	if err != nil {
		p.logger.Warn("[PlayerApi] N-param decryption failed", slog.String("error", err.Error()))
		return rawURL
	}

	// Replace n= value directly in the raw URL to preserve parameter order.
	// The n-param value is base64url (alphanumeric + _ + -), so it doesn't
	// require URL encoding and appears literally in the URL.
	return strings.Replace(rawURL, "n="+nParam, "n="+decryptedN, 1)
}

// DecryptDashManifestUrl decrypts the n-parameter in a DASH manifest URL.
func (p *PlayerAPI) DecryptDashManifestUrl(ctx context.Context, dashURL, playerURL string) string {
	if playerURL == "" || p.cipherSolver == nil {
		return dashURL
	}
	return p.decryptNParam(ctx, dashURL, playerURL)
}

// DecryptNParamInUrl decrypts the n-parameter in any URL.
// Handles both path-based /n/{value}/ format and query string ?n= format.
func (p *PlayerAPI) DecryptNParamInUrl(ctx context.Context, rawURL, playerURL string) string {
	if playerURL == "" || p.cipherSolver == nil {
		return rawURL
	}

	// Check for n parameter in path: /n/{encrypted_value}/
	if m := pathNParamRe.FindStringSubmatch(rawURL); m != nil {
		encryptedN := m[1]
		solvers, err := p.cipherSolver.GetSolvers(ctx, playerURL)
		if err == nil && solvers.N != nil {
			decryptedN, err := solvers.DecryptN(encryptedN)
			if err == nil && decryptedN != encryptedN {
				return strings.Replace(rawURL, "/n/"+encryptedN+"/", "/n/"+decryptedN+"/", 1)
			}
		}
	}

	// Also check query string n param
	return p.decryptNParam(ctx, rawURL, playerURL)
}

func parsePlayabilityStatus(status map[string]interface{}) (PlayabilityError, string) {
	if status == nil {
		return PlayabilityUnknown, ""
	}

	statusCode := getStr(status, "status")
	reason := getStr(status, "reason")
	if reason == "" {
		if msgs, ok := status["messages"].([]interface{}); ok && len(msgs) > 0 {
			reason, _ = msgs[0].(string)
		}
	}

	reasonLower := strings.ToLower(reason)

	// Upcoming indicators are not errors
	if statusCode == "LIVE_STREAM_OFFLINE" ||
		(statusCode == "UNPLAYABLE" && strings.Contains(reasonLower, "live event will begin")) {
		return PlayabilityOK, ""
	}

	switch statusCode {
	case "OK":
		return PlayabilityOK, ""
	case "LOGIN_REQUIRED":
		if strings.Contains(reasonLower, "member") || strings.Contains(reasonLower, "join") {
			return PlayabilityMembersOnly, reason
		}
		return PlayabilityLoginRequired, reason
	case "UNPLAYABLE":
		if strings.Contains(reasonLower, "member") {
			return PlayabilityMembersOnly, reason
		}
		if strings.Contains(reasonLower, "private") {
			return PlayabilityPrivate, reason
		}
		if strings.Contains(reasonLower, "country") || strings.Contains(reasonLower, "region") || strings.Contains(reasonLower, "not available in your") {
			return PlayabilityRegionBlocked, reason
		}
		if strings.Contains(reasonLower, "unavailable") {
			return PlayabilityUnavailable, reason
		}
		return PlayabilityUnknown, reason
	case "AGE_VERIFICATION_REQUIRED":
		return PlayabilityAgeRestricted, reason
	case "ERROR":
		if strings.Contains(reasonLower, "private") || strings.Contains(reasonLower, "unavailable") {
			return PlayabilityUnavailable, reason
		}
		return PlayabilityUnknown, reason
	default:
		return PlayabilityUnknown, reason
	}
}

func classifyStream(videoDetails, playabilityStatus, microformat map[string]interface{}, hasFormats bool) (StreamStatus, bool, bool, bool) {
	isLiveContent, _ := videoDetails["isLiveContent"].(bool)
	isLiveNow, _ := videoDetails["isLive"].(bool)
	isUpcomingVD, _ := videoDetails["isUpcoming"].(bool)

	var lbd map[string]interface{}
	if microformat != nil {
		lbd, _ = microformat["liveBroadcastDetails"].(map[string]interface{})
	}

	if lbd != nil {
		if isNow, ok := lbd["isLiveNow"].(bool); ok && isNow {
			isLiveNow = true
		}
	}

	isPostLiveDVR := lbd != nil && getStr(lbd, "endTimestamp") != "" && !isLiveNow

	// Check playability for upcoming
	status := getStr(playabilityStatus, "status")
	reason := strings.ToLower(getStr(playabilityStatus, "reason"))
	isUpcomingFromPlayability := status == "LIVE_STREAM_OFFLINE" ||
		(status == "UNPLAYABLE" && strings.Contains(reason, "live event will begin"))

	// Premiere detection: has scheduled start but not marked as live content,
	// and reason contains "premiere" or videoDetails says upcoming.
	isPremiere := lbd != nil && getStr(lbd, "startTimestamp") != "" && !isLiveContent &&
		(strings.Contains(reason, "premiere") || isUpcomingVD)
	isPremiereNow := isPremiere && isLiveNow
	isUpcomingPremiere := isPremiere && !isLiveNow && !hasFormats

	if isUpcomingFromPlayability {
		return StreamUpcoming, false, true, false
	}
	if isUpcomingPremiere {
		return StreamUpcoming, false, true, false
	}
	if isLiveNow || isPremiereNow {
		return StreamLive, true, false, false
	}
	if lbd == nil && !isLiveContent && !isPremiere {
		return StreamNotAStream, false, false, false
	}
	if !hasFormats && (lbd != nil || isLiveContent) {
		return StreamUpcoming, false, true, false
	}
	if isPostLiveDVR {
		return StreamPostLive, false, false, true
	}
	return StreamVOD, false, false, false
}

func collectFormats(pool *[]Format, formats []Format, source string, authLevel int) {
	for i := range formats {
		formats[i].Source = source
		formats[i].AuthLevel = &authLevel
		*pool = append(*pool, formats[i])
	}
}

func deduplicateFormats(pool []Format) []Format {
	byItag := make(map[int]Format)
	for _, f := range pool {
		if f.URL == "" {
			continue
		}
		existing, exists := byItag[f.Itag]
		if !exists {
			byItag[f.Itag] = f
			continue
		}
		fAuth := 999
		if f.AuthLevel != nil {
			fAuth = *f.AuthLevel
		}
		eAuth := 999
		if existing.AuthLevel != nil {
			eAuth = *existing.AuthLevel
		}
		if fAuth < eAuth {
			byItag[f.Itag] = f
		}
	}

	result := make([]Format, 0, len(byItag))
	for _, f := range byItag {
		result = append(result, f)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Itag < result[j].Itag
	})
	return result
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
	if target.ScheduledStartTime == "" {
		target.ScheduledStartTime = source.ScheduledStartTime
	}
	if target.Description == "" {
		target.Description = source.Description
	}
	if target.ThumbnailURL == "" {
		target.ThumbnailURL = source.ThumbnailURL
	}
	if target.Title == "Unknown Title" && source.Title != "Unknown Title" {
		target.Title = source.Title
	}
	if target.ChannelName == "Unknown Channel" && source.ChannelName != "Unknown Channel" {
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

// JSON helper functions

func getStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func getInt(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

func getNestedMap(m map[string]interface{}, keys ...string) (map[string]interface{}, bool) {
	current := m
	for _, key := range keys {
		if current == nil {
			return nil, false
		}
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func getNestedSlice(m map[string]interface{}, keys ...string) ([]interface{}, bool) {
	if len(keys) == 0 {
		return nil, false
	}
	// Navigate to parent
	parent := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := parent[key].(map[string]interface{})
		if !ok {
			return nil, false
		}
		parent = next
	}
	arr, ok := parent[keys[len(keys)-1]].([]interface{})
	return arr, ok
}

func getDeepStr(m map[string]interface{}, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	parent := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := parent[key].(map[string]interface{})
		if !ok {
			return ""
		}
		parent = next
	}
	return getStr(parent, keys[len(keys)-1])
}
