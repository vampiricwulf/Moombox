package youtube

import (
	"cmp"
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
)

func (p *PlayerAPI) parsePlayerResponse(ctx context.Context, data map[string]any, playerURL string, ytcfg *YtcfgData) (*VideoInfo, error) {
	videoDetails, _ := data["videoDetails"].(map[string]any)
	streamingData, _ := data["streamingData"].(map[string]any)
	playabilityStatus, _ := data["playabilityStatus"].(map[string]any)
	microformat, _ := getNestedMap(data, "microformat", "playerMicroformatRenderer")

	// Parse playability
	playErr, playReason := parsePlayabilityStatus(playabilityStatus)

	// Parse formats. Cipher decryption is deferred to post-selection by
	// the worker strategies (Stage 3 of the cipher pipeline rework) — see
	// docs/plans/cipher-pipeline-rework.md. The returned formats may
	// carry EncryptedSig populated and a raw `url=` value in URL; the
	// strategy resolves them via cipher.ResolveFormatURL right before
	// constructing SegmentDownloaders.
	formats := p.parseFormats(streamingData)

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
		title = UnknownTitleSentinel
	}
	if channelName == "" {
		channelName = UnknownChannelSentinel
	}

	// Thumbnail
	thumbnailURL := ""
	if ytcfg != nil {
		thumbnailURL = ytcfg.ThumbnailURL
	}
	if thumbs, ok := getNestedSlice(videoDetails, "thumbnail", "thumbnails"); ok && len(thumbs) > 0 {
		if last, ok := thumbs[len(thumbs)-1].(map[string]any); ok {
			if u, ok := last["url"].(string); ok {
				thumbnailURL = u
			}
		}
	}

	// Scheduled start time
	scheduledStartTime := extractScheduledStartTime(microformat, playabilityStatus)

	// Published date (spec §12) — status-aware, deliberately independent of
	// extractScheduledStartTime above (see extractPublishedAt doc comment).
	publishedAt, publishedPrecision := extractPublishedAt(string(streamStatus), microformat)

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
		EndTimestamp:       endTimestamp,
		ScheduledStartTime: scheduledStartTime,
		DashManifestURL:    getStr(streamingData, "dashManifestUrl"),
		HlsManifestURL:     getStr(streamingData, "hlsManifestUrl"),
		PlayabilityError:   playErr,
		PlayabilityReason:  playReason,
		PublishedAt:        publishedAt,
		PublishedPrecision: publishedPrecision,
	}, nil
}

// extractScheduledStartTime tries multiple sources for the scheduled start time.
func extractScheduledStartTime(microformat, playabilityStatus map[string]any) string {
	// liveBroadcastDetails.startTimestamp
	if lbd, ok := getNestedMap(microformat, "liveBroadcastDetails"); ok {
		if ts := getStr(lbd, "startTimestamp"); ts != "" {
			return ts
		}
	}

	// liveStreamability epoch
	if epoch := getDeepStr(playabilityStatus, "liveStreamability", "liveStreamabilityRenderer", "offlineSlate", "liveStreamOfflineSlateRenderer", "scheduledStartTime"); epoch != "" {
		if ts, err := strconv.ParseInt(epoch, 10, 64); err == nil {
			return time.Unix(ts, 0).UTC().Format(time.RFC3339)
		}
	}

	// Fallback to uploadDate / publishDate from microformat
	if ud := getStr(microformat, "uploadDate"); ud != "" {
		return ud
	}
	if pd := getStr(microformat, "publishDate"); pd != "" {
		return pd
	}

	return ""
}

// extractPublishedAt returns the probe's authoritative publish date for the
// feed-history ladder (spec §12). Status-aware: liveBroadcastDetails.
// startTimestamp is a REAL broadcast start only for past streams
// (vod/post_live) — for upcoming it is the FUTURE schedule and must never
// feed the result, so upcoming/live are deliberately absent from the switch
// below. The microformat uploadDate/publishDate fallback (precision "day")
// covers the normal plain-vod case, since an ended stream WITH an
// endTimestamp classifies post_live instead of vod. The liveStreamability
// epoch that extractScheduledStartTime falls back to is never consulted
// here — deliberately NOT reusing extractScheduledStartTime, which
// conflates a future scheduled start with a publish date.
func extractPublishedAt(status string, microformat map[string]any) (ts, precision string) {
	switch status {
	case "vod", "post_live":
		if lbd, ok := getNestedMap(microformat, "liveBroadcastDetails"); ok {
			if v := getStr(lbd, "startTimestamp"); v != "" {
				return v, "started"
			}
		}
		if v := microformatDate(microformat); v != "" {
			return v, "day"
		}
	case "not_a_stream":
		if v := microformatDate(microformat); v != "" {
			return v, "day"
		}
	}
	return "", ""
}

// microformatDate returns the microformat upload/publish date, preferring
// uploadDate. microformat is already the unwrapped playerMicroformatRenderer
// map (same shape classifyStream and extractScheduledStartTime consume).
//
// A bare YYYY-MM-DD is normalized to <date>T23:59:59Z — the NEWEST instant
// consistent with the imprecise value. The ladder compares timestamps
// lexically, and a bare date would compare as midnight (the OLDEST instant),
// excluding boundary items in exactly the direction the spec §12 skew-new
// rule forbids ("nothing is excluded on a date we have not verified").
// Values that already carry a time component (some microformat dates are
// full RFC3339 with offset) pass through unchanged.
func microformatDate(microformat map[string]any) string {
	v := getStr(microformat, "uploadDate")
	if v == "" {
		v = getStr(microformat, "publishDate")
	}
	if _, err := time.Parse(time.DateOnly, v); err == nil {
		return v + "T23:59:59Z"
	}
	return v
}

// parseFormats extracts format metadata + raw stream URLs from a
// streamingData map. Cipher decryption (sig + n) is intentionally NOT
// performed here — strategies do it post-selection via
// cipher.ResolveFormatURL on the chosen format(s) only. This avoids
// the 7-26 cipher calls per stream setup that the previous
// parseFormatsWithCipher implementation paid even though most formats
// were discarded by selection.
//
// For sigCipher entries, the raw `url=` part of the cipher string is
// stored in Format.URL and the `s` / `sp` parts in EncryptedSig /
// SigKey. For direct entries, Format.URL holds the inline URL and
// EncryptedSig is empty. Selection still uses Format.URL != "" as
// the "format has a URL" signal — we always set URL to something
// fetchable-after-resolve.
func (p *PlayerAPI) parseFormats(streamingData map[string]any) []Format {
	if streamingData == nil {
		return nil
	}

	var formats []Format
	// adaptiveFormats is iterated first so that when an itag appears in both
	// arrays the DASH/adaptive entry lands in the pool first and wins the
	// same-auth-level tiebreak in deduplicateFormats. adaptiveFormats carry
	// contentLength + clean byte ranges and are the preferred source for
	// live→post-live transitions where formats[] can temporarily point at
	// muxed URLs that 404 once the broadcast wraps up.
	for _, key := range []string{"adaptiveFormats", "formats"} {
		arr, ok := streamingData[key].([]any)
		if !ok {
			continue
		}
		for _, item := range arr {
			f, ok := item.(map[string]any)
			if !ok {
				continue
			}

			formatURL := getStr(f, "url")
			sigCipher := getStr(f, "signatureCipher")
			var encSig, sigKey string

			// Handle signatureCipher (entry without direct URL) by parsing
			// out the raw URL + sig material; defer the actual sig
			// decryption to the strategy's post-selection resolve step.
			if formatURL == "" && sigCipher != "" {
				params, parseErr := url.ParseQuery(sigCipher)
				if parseErr != nil {
					p.logger.Debug("[PlayerApi] Failed to parse signatureCipher",
						slog.String("error", parseErr.Error()))
					continue
				}
				formatURL = params.Get("url")
				encSig = params.Get("s")
				sigKey = params.Get("sp")
				if sigKey == "" {
					sigKey = "signature"
				}
				if formatURL == "" || encSig == "" {
					continue
				}
			}

			if formatURL == "" {
				continue
			}

			format := Format{
				Itag:         getInt(f, "itag"),
				URL:          formatURL,
				MimeType:     getStr(f, "mimeType"),
				Bitrate:      getInt(f, "bitrate"),
				EncryptedSig: encSig,
				SigKey:       sigKey,
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

// decryptNParam decrypts the n-parameter in a URL to avoid throttling.
// On any failure it returns the raw URL unchanged, so callers that prefer
// a best-effort behaviour (manifest/VOD refreshers) can keep using it. For
// the parser's own format list, use decryptNParamStrict, which signals
// failure so the caller can drop the format.
//
// Uses string replacement to preserve original URL parameter order —
// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
// YouTube's URL signature verification and causes HTTP 403.
func (p *PlayerAPI) decryptNParam(ctx context.Context, rawURL, playerURL string) string {
	out, _ := p.decryptNParamStrict(ctx, rawURL, playerURL)
	if out == "" {
		return rawURL
	}
	return out
}

// decryptNParamStrict is like decryptNParam but returns (newURL, true) only
// when the URL either had no n-param to decrypt or the decryption succeeded.
// When the URL has an n-param but decryption fails (solver unavailable,
// Goja error, etc.) it returns ("", false) so the caller can drop the
// format. Keeping a throttled URL in the pool would just 403 at the CDN
// and waste retries.
func (p *PlayerAPI) decryptNParamStrict(ctx context.Context, rawURL, playerURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}

	// Extract the raw (percent-encoded) n-param for accurate string matching.
	rawN, nParam := cipher.RawQueryParam(u.RawQuery, "n")
	if rawN == "" || nParam == "" {
		// No n-param to decrypt — URL is already usable.
		return rawURL, true
	}

	decryptedN, err := p.decryptN(ctx, playerURL, nParam)
	if err != nil {
		p.logger.Warn("[PlayerApi] N-param decryption failed", slog.String("error", err.Error()))
		return "", false
	}

	// Replace the first occurrence of n=<value> that is a proper query parameter
	// to avoid false-matching within other parameter values.
	// url.QueryEscape on decryptedN is a no-op today — n-param values are
	// drawn from a URL-safe alphabet (alphanumeric + '-_'). Kept for safety
	// in case YouTube ever widens that alphabet to include reserved chars.
	for _, prefix := range []string{"?", "&"} {
		old := prefix + "n=" + rawN
		if strings.Contains(rawURL, old) {
			return strings.Replace(rawURL, old, prefix+"n="+url.QueryEscape(decryptedN), 1), true
		}
	}
	// The decrypt succeeded but the n= token wasn't in a recognisable
	// query position — treat as pass-through.
	return rawURL, true
}

// DecryptDashManifestUrl decrypts the n-parameter in a DASH manifest URL.
func (p *PlayerAPI) DecryptDashManifestUrl(ctx context.Context, dashURL, playerURL string) string {
	if playerURL == "" || !p.hasCipher() {
		return dashURL
	}
	return p.decryptNParam(ctx, dashURL, playerURL)
}

// DecryptNParamInUrl decrypts the n-parameter in any URL.
// Handles both path-based /n/{value}/ format and query string ?n= format.
func (p *PlayerAPI) DecryptNParamInUrl(ctx context.Context, rawURL, playerURL string) string {
	if playerURL == "" || !p.hasCipher() {
		return rawURL
	}

	// Check for n parameter in path: /n/{encrypted_value}/
	if m := pathNParamRe.FindStringSubmatch(rawURL); m != nil {
		encryptedN := m[1]
		decryptedN, err := p.decryptN(ctx, playerURL, encryptedN)
		if err == nil && decryptedN != encryptedN {
			return strings.Replace(rawURL, "/n/"+encryptedN+"/", "/n/"+decryptedN+"/", 1)
		}
	}

	// Also check query string n param
	return p.decryptNParam(ctx, rawURL, playerURL)
}

// isUpcomingFromPlayability returns true when YouTube's playabilityStatus
// indicates the stream is scheduled but not yet live. statusCode is the raw
// `status` field; reasonLower must already be lower-cased. Shared between
// parsePlayabilityStatus (which suppresses the "error" classification for
// these) and classifyStream (which routes them to StreamUpcoming).
func isUpcomingFromPlayability(statusCode, reasonLower string) bool {
	return statusCode == "LIVE_STREAM_OFFLINE" ||
		(statusCode == "UNPLAYABLE" && strings.Contains(reasonLower, "live event will begin"))
}

func parsePlayabilityStatus(status map[string]any) (PlayabilityError, string) {
	if status == nil {
		return PlayabilityUnknown, ""
	}

	statusCode := getStr(status, "status")
	reason := getStr(status, "reason")
	if reason == "" {
		if msgs, ok := status["messages"].([]any); ok && len(msgs) > 0 {
			reason, _ = msgs[0].(string)
		}
	}

	reasonLower := strings.ToLower(reason)

	// Upcoming indicators are not errors. The same check is duplicated in
	// classifyStream — keep both call sites going through the helper so a
	// reason-string change only has to be edited in one place (audit D1).
	if isUpcomingFromPlayability(statusCode, reasonLower) {
		return PlayabilityOK, ""
	}

	switch statusCode {
	case "OK":
		return PlayabilityOK, ""
	case "LOGIN_REQUIRED":
		if strings.Contains(reasonLower, "member") || strings.Contains(reasonLower, "join") {
			return PlayabilityMembersOnly, reason
		}
		if strings.Contains(reasonLower, "age") {
			return PlayabilityAgeRestricted, reason
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

func classifyStream(videoDetails, playabilityStatus, microformat map[string]any, hasFormats bool) (StreamStatus, bool, bool, bool) {
	isLiveContent, _ := videoDetails["isLiveContent"].(bool)
	isLiveNow, _ := videoDetails["isLive"].(bool)
	isUpcomingVD, _ := videoDetails["isUpcoming"].(bool)

	var lbd map[string]any
	if microformat != nil {
		lbd, _ = microformat["liveBroadcastDetails"].(map[string]any)
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
	isUpcomingPlayability := isUpcomingFromPlayability(status, reason)

	// playabilityStatus.liveStreamability is the renderer YouTube attaches
	// to scheduled streams that haven't gone live yet. Some probes
	// (notably ANDROID_VR on unpublished premieres) return this without
	// a full microformat or videoDetails.isUpcoming flag — the raw
	// fallthrough would misclassify those as not_a_stream and end the
	// polling cycle. Detect it independently.
	_, hasLiveStreamability := getNestedMap(playabilityStatus, "liveStreamability", "liveStreamabilityRenderer")

	// Premiere detection: has scheduled start but not marked as live content,
	// and reason contains "premiere" or videoDetails says upcoming.
	isPremiere := lbd != nil && getStr(lbd, "startTimestamp") != "" && !isLiveContent &&
		(strings.Contains(reason, "premiere") || isUpcomingVD)
	isPremiereNow := isPremiere && isLiveNow
	isUpcomingPremiere := isPremiere && !isLiveNow && !hasFormats

	if isUpcomingPlayability {
		return StreamUpcoming, false, true, false
	}
	if isUpcomingPremiere {
		return StreamUpcoming, false, true, false
	}
	// videoDetails.isUpcoming overrides isLive for waiting room / offline slate.
	// YouTube may set isLive=true when the scheduled time passes (serving the
	// waiting room as a "live" feed), but isUpcoming=true means the creator
	// hasn't actually started streaming yet.
	if isUpcomingVD && !hasFormats {
		return StreamUpcoming, false, true, false
	}
	if isLiveNow || isPremiereNow {
		return StreamLive, true, false, false
	}
	// liveStreamability with no formats — treat as upcoming. Runs before the
	// lbd==nil && !isLiveContent shortcut so premieres that only expose the
	// renderer aren't mis-classified as not_a_stream.
	if hasLiveStreamability && !hasFormats {
		return StreamUpcoming, false, true, false
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

// collectFormats appends formats into pool with the given source label and
// auth level. Each format is copied into the pool rather than mutated in
// place, so the caller's slice is unaffected — important when the same
// Format slice is referenced by more than one VideoInfo (e.g. when a
// watch-page parse is shared between the public and authenticated paths).
// The shared authLevel pointer is intentional: dedup compares levels by
// value, not identity.
func collectFormats(pool *[]Format, formats []Format, source string, authLevel int) {
	level := authLevel
	for _, f := range formats {
		f.Source = source
		f.AuthLevel = &level
		*pool = append(*pool, f)
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
		fAuth := authLevelOf(&f)
		eAuth := authLevelOf(&existing)
		if fAuth < eAuth {
			byItag[f.Itag] = f
		}
	}

	result := make([]Format, 0, len(byItag))
	for _, f := range byItag {
		result = append(result, f)
	}
	slices.SortFunc(result, func(a, b Format) int {
		return cmp.Compare(a.Itag, b.Itag)
	})
	return result
}

// --- JSON helper functions ---

func getStr(m map[string]any, key string) string {
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

func getInt(m map[string]any, key string) int {
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

func getNestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	current := m
	for _, key := range keys {
		if current == nil {
			return nil, false
		}
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func getNestedSlice(m map[string]any, keys ...string) ([]any, bool) {
	if len(keys) == 0 {
		return nil, false
	}
	// Navigate to parent
	parent := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := parent[key].(map[string]any)
		if !ok {
			return nil, false
		}
		parent = next
	}
	arr, ok := parent[keys[len(keys)-1]].([]any)
	return arr, ok
}

func getDeepStr(m map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	parent := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := parent[key].(map[string]any)
		if !ok {
			return ""
		}
		parent = next
	}
	return getStr(parent, keys[len(keys)-1])
}
