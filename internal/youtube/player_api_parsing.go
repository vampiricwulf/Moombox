package youtube

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
)

// VideoIDMismatchError reports a player response whose videoDetails.videoId is
// not the video that was asked for. yt-dlp calls this an "invalid player
// response" (_video.py:3022-3026) and its only attested cause is a blocked or
// rate-limited source IP being served a SUBSTITUTE video
// (TeamNewPipe/NewPipe#8713). It is returned rather than logged because the
// cascade's error handling is what skips the client.
type VideoIDMismatchError struct {
	Requested string
	Got       string
}

func (e *VideoIDMismatchError) Error() string {
	return fmt.Sprintf("player response is for video %q, not %q (YouTube served a substitute — the source IP may be rate-limited or blocked)",
		e.Got, e.Requested)
}

// ErrAllClientsMismatched is upstream's terminal verdict when nothing survived
// the check: `raise ExtractorError('All player responses are invalid. Your IP
// is likely being blocked by Youtube')` (_video.py:3181-3187).
var ErrAllClientsMismatched = errors.New("every Innertube client returned a player response for a different video — this IP is likely being blocked by YouTube")

func (p *PlayerAPI) parsePlayerResponse(ctx context.Context, data map[string]any, playerURL string, ytcfg *YtcfgData, requestedVideoID string) (*VideoInfo, error) {
	videoDetails, _ := data["videoDetails"].(map[string]any)

	// yt-dlp's _invalid_player_response (_video.py:3022-3026, applied to the
	// watch page's own response at :3038 and per client at :3122): "YouTube
	// may return a different video player response than expected." Taking a
	// substitute's response would hand this job the substitute's status AND
	// formats — the waiting-room probe reads a VOD substitute as "became VOD"
	// and the orchestrator archives the wrong video.
	//
	// An ABSENT or EMPTY videoId is ACCEPTED, and that MATCHES upstream rather
	// than diverging from it: _invalid_player_response returns the *id*, not a
	// bool, and both call sites test that return for TRUTHINESS —
	// `if pr_id := self._invalid_player_response(pr, video_id):` (:3122) and
	// `if initial_pr and not self._invalid_player_response(...)` (:3038) — so
	// a None or "" id leaves the response in `prs`. Go has no truthiness, so
	// the rule is spelled out here: only a NON-EMPTY id that differs is a
	// substitute.
	//
	// It is load-bearing, not incidental. TV is this cascade's playability
	// AUTHORITY and several of its refusals arrive as a playabilityStatus with
	// no videoDetails at all; rejecting those would discard the verdict every
	// downstream error string is built from.
	if got := getStr(videoDetails, "videoId"); got != "" && got != requestedVideoID {
		return nil, &VideoIDMismatchError{Requested: requestedVideoID, Got: got}
	}

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
	formats, drmSkipped := p.parseFormats(ctx, streamingData)

	// Stream classification. hasFormats asks whether the response CARRIED
	// formats, not whether any survived the parse: a response whose formats
	// are all DRM still proves the broadcast has media, and classifying on the
	// post-filter count turns a finished stream into `upcoming` — a stall the
	// single-client ProbeVideoStatusAuthenticated path never recovers from,
	// for exactly the accounts the tv-client DRM experiment hits.
	streamStatus, isLive, isUpcoming, isPostLiveDVR := classifyStream(videoDetails, playabilityStatus, microformat, len(formats) > 0 || drmSkipped > 0)

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
//
// Every returned timestamp is Z-normalized (RFC3339 UTC). YouTube emits
// startTimestamp with a "+00:00" offset and microformat dates with local
// offsets ("-07:00"), but every consumer compares this value
// LEXICOGRAPHICALLY against a Z-format cutoff (the archive window re-check,
// the walk's exhaustion math, DECAPI's window check) and stores it into
// feed_items.published, whose contract is "RFC3339 UTC — lexicographic order
// IS chronological order". An offset-bearing string mis-compares by hours at
// every window boundary, and once stored at started/day rank it can never be
// corrected (rank ties don't overwrite) — so normalization happens HERE, at
// the one producer. An unparseable startTimestamp is treated as no-date
// (fall through to the microformat date) rather than passed through.
func extractPublishedAt(status string, microformat map[string]any) (ts, precision string) {
	switch status {
	case "vod", "post_live":
		if lbd, ok := getNestedMap(microformat, "liveBroadcastDetails"); ok {
			if v := getStr(lbd, "startTimestamp"); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					return t.UTC().Format(time.RFC3339), "started"
				}
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
// Values with a time component (some microformat dates are full RFC3339
// with a local offset, e.g. "-07:00") are converted to UTC for the same
// lexicographic seam — see extractPublishedAt's doc comment. Anything that
// parses as neither is no-date (""): garbage stored at 'day' rank would
// sort nonsensically forever.
func microformatDate(microformat map[string]any) string {
	v := getStr(microformat, "uploadDate")
	if v == "" {
		v = getStr(microformat, "publishDate")
	}
	if _, err := time.Parse(time.DateOnly, v); err == nil {
		return v + "T23:59:59Z"
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

// extractionStateKey is the context key for extractionState. Its own unexported
// type, so no other package can collide with it.
type extractionStateKey struct{}

// extractionState is the scratch one extraction — one GetVideoInfo* cascade —
// carries across the several player responses it parses. It rides the context
// because the parse path (doRetryRequest → parsePlayerResponse → parseFormats)
// is entered from every fetchWith* helper, and the context is the only
// per-extraction value all of them already thread; the alternative is a new
// parameter on eight call sites that do nothing with it.
//
// The flag is atomic although a cascade runs its clients sequentially today:
// the guarantee "no fetch is in a goroutine" is one a later arc can quietly
// invalidate by parallelising the client sweep, and an unsynchronised write
// would then be a data race that -race only catches if that path happens to be
// exercised. One word buys the class away.
type extractionState struct {
	drmWarned atomic.Bool
}

// withExtractionState opens one extraction's scratch. Called once at the head
// of each cascade; every player response parsed under the returned context
// then shares the same state.
func withExtractionState(ctx context.Context) context.Context {
	return context.WithValue(ctx, extractionStateKey{}, &extractionState{})
}

// extractionStateFrom returns this extraction's state, or nil when the caller
// is not inside a cascade — a direct parse, as several tests make.
func extractionStateFrom(ctx context.Context) *extractionState {
	s, _ := ctx.Value(extractionStateKey{}).(*extractionState)
	return s
}

// firstDRMReport reports whether this extraction has yet to log the DRM-skip
// warning, recording that it now has. Nil (no cascade) reports every time,
// which is the plain per-call behaviour a direct parseFormats caller expects.
func (s *extractionState) firstDRMReport() bool {
	return s == nil || s.drmWarned.CompareAndSwap(false, true)
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
//
// ctx carries the extraction's scratch state (see extractionState): the
// DRM-skip warning is reported once per extraction, not once per response.
//
// The second return is how many entries this response lost to the DRM drop.
// Callers need it because "this response carried no formats" and "every format
// it carried was DRM" are different facts: classification reads the first, and
// only the unfiltered count can tell it apart from an empty streamingData.
func (p *PlayerAPI) parseFormats(ctx context.Context, streamingData map[string]any) ([]Format, int) {
	if streamingData == nil {
		return nil, 0
	}

	var formats []Format
	drmSkipped := 0
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

			// DRM formats are dropped rather than ranked. yt-dlp reports them
			// as skipped (_video.py:3418-3428 — an account-level experiment
			// applies DRM to ALL videos on the tv client, issue #12563) and
			// YoutubeDL.py:2930 filters them out, because muxing encrypted
			// samples produces an unplayable archive. Dropping here rather
			// than in the selector means a clean same-itag format from
			// another client still wins the dedup tie it used to LOSE.
			if drm, ok := f["drmFamilies"].([]any); ok && len(drm) > 0 {
				drmSkipped++
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
			if td := getInt(f, "targetDurationSec"); td > 0 {
				format.TargetDurationSec = td
			}

			if at, ok := f["audioTrack"].(map[string]any); ok {
				format.AudioTrackID = getStr(at, "id")
				format.AudioTrackName = getStr(at, "displayName")
				format.AudioIsDefault = getBool(at, "audioIsDefault")
			}
			format.IsDrc = getBool(f, "isDrc")

			formats = append(formats, format)
		}
	}
	if drmSkipped > 0 && extractionStateFrom(ctx).firstDRMReport() {
		// Warn, not Debug: upstream reports this with report_warning
		// (_video.py:3428, only_once=True), and a silently DRM-stripped format
		// pool is exactly the state an operator needs told about — it is the
		// difference between "this video has no 1080p" and "this ACCOUNT gets
		// no 1080p". The count is per response; the LINE is once per
		// extraction, as upstream's only_once is once per run — an account in
		// the tv-client DRM experiment has DRM formats in nearly every
		// response a cascade collects.
		p.logger.Warn("[PlayerApi] skipped DRM-protected formats",
			"count", drmSkipped,
			"note", "a YouTube account experiment applies DRM to all videos on the tv client — yt-dlp issue #12563")
	}
	// Beside the DRM count, the other thing this response loses on the way to
	// the consumers: the alternate audio renditions deduplicateFormats will
	// collapse away, one per itag beyond the preferred one. Nothing downstream
	// can see them once the pool is built, so the diagnosis is recorded here.
	// Silent on an ordinary response, which carries no track fields at all.
	if collapsible := countCollapsibleRenditions(formats); collapsible > 0 {
		p.logger.Debug("[PlayerApi] response carries alternate audio renditions",
			"collapsed", collapsible,
			"note", "dubbed and DRC renditions; the pool keeps one per itag — the original-language, non-DRC one")
	}
	return formats, drmSkipped
}

// countCollapsibleRenditions counts the track-carrying entries that will lose
// their itag to a preferred sibling: for each itag, every rendition beyond the
// first. Only entries with a track id or a DRC flag are counted, so an
// ordinary response — where an itag can still appear in both adaptiveFormats
// and formats — reports nothing.
func countCollapsibleRenditions(formats []Format) int {
	seen := map[int]bool{}
	collapsible := 0
	for i := range formats {
		f := &formats[i]
		if f.AudioTrackID == "" && !f.IsDrc {
			continue
		}
		if seen[f.Itag] {
			collapsible++
			continue
		}
		seen[f.Itag] = true
	}
	return collapsible
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

// ageGateReasons are yt-dlp's AGE_GATE_REASONS reason substrings
// (_video.py:2900-2903, the reason substrings on 2901). They are the
// load-bearing half of the match: upstream's status entries in the same tuple
// are lower-case and are substring-matched against the raw upper-case
// `status`, so upstream in practice detects an age gate through the REASON
// text.
var ageGateReasons = []string{"confirm your age", "age-restricted", "inappropriate"}

// isAgeGateReason reports whether a playability reason (already lower-cased)
// names an age gate.
func isAgeGateReason(reasonLower string) bool {
	for _, r := range ageGateReasons {
		if strings.Contains(reasonLower, r) {
			return true
		}
	}
	return false
}

// hasDesktopLegacyAgeGate mirrors upstream's first test,
// `traverse_obj(player_response, ('playabilityStatus',
// 'desktopLegacyAgeGateReason'))` (_video.py:2896-2897) — a TRUTHINESS test,
// so a key present but zero/empty/false is not an age gate.
//
// Every shape encoding/json can produce for an `any` is spelled out, including
// the two containers: Python calls an empty dict or list falsy, so a
// `desktopLegacyAgeGateReason` of `{}` or `[]` is NOT a gate. Only an
// unreachable type reaches the default arm.
func hasDesktopLegacyAgeGate(status map[string]any) bool {
	switch v := status["desktopLegacyAgeGateReason"].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	default:
		return true
	}
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

	// Age gates, ported from _is_agegated (_video.py:2894-2904). This runs
	// BEFORE the status switch because the shapes it catches are spread across
	// three different status codes (AGE_CHECK_REQUIRED, UNPLAYABLE,
	// LOGIN_REQUIRED) — and AFTER the upcoming check, because a waiting room
	// is not an error and must never be classified as one.
	//
	// `status == "OK"` is excluded for that same reason: a response YouTube
	// says is PLAYABLE is not an error either, and every shape this block
	// exists to catch is non-OK. Without the exclusion an OK response that
	// merely carries desktopLegacyAgeGateReason or an age-flavoured reason
	// would classify age_restricted, and checkPlayability
	// (internal/worker/stream_processor.go) aborts the job on every non-ok
	// verdict — with the notification SUPPRESSED for age_restricted. Upstream
	// cannot hit this: its _is_agegated only ever appends clients
	// (_video.py:3157-3175), it never overrides a playability verdict.
	//
	// It cannot steal a members-only verdict either: none of the three
	// substrings appears in YouTube's membership reason text, which says "Join
	// this channel to get access to members-only content".
	if statusCode != "OK" && (hasDesktopLegacyAgeGate(status) || isAgeGateReason(reasonLower)) {
		return PlayabilityAgeRestricted, reason
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
	case "AGE_VERIFICATION_REQUIRED", "AGE_CHECK_REQUIRED":
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

// formatKey is yt-dlp's get_stream_id (_video.py:3396-3397): itag alone is not
// an identity. A dubbed video lists several itag-140 entries from ONE client
// differing only by audioTrack.id, and the DRC rendition of a track is a
// separate stream, not a variant — keying on itag alone kept whichever was
// listed first and silently discarded the rest.
type formatKey struct {
	itag         int
	audioTrackID string
	isDrc        bool
}

// deduplicateFormats reduces the collected pools to the format list the rest
// of Moombox sees. It runs upstream's identity first — one entry per
// (itag, audioTrack.id, isDrc), lowest auth level winning a stream that
// several clients returned — and then COLLAPSES each itag to the single
// rendition the audio-track preference would pick.
//
// That collapse is this port's one deliberate divergence from yt-dlp, which
// keeps every rendition and carries the identity into its format ids
// ("140-drc", _video.py:3450-3456). Moombox's consumers look formats up by
// itag alone — SelectBestDashStream picks among same-itag entries by
// bandwidth, and resolveFormatURLByItag returns the first entry of an itag at
// setup and on every 403 credential refresh — so leaving several renditions of
// one itag in the pool lets the live path archive a dub, or splice a second
// language into a file that is already half written. Collapsing here means
// every itag-keyed lookup resolves to exactly the rendition the selector
// prefers, on the VOD path, the manifestless DASH path and the 403 refresh
// alike, without any consumer needing to learn the 3-part identity.
func deduplicateFormats(pool []Format) []Format {
	byStream := make(map[formatKey]Format)
	for _, f := range pool {
		if f.URL == "" {
			continue
		}
		key := formatKey{itag: f.Itag, audioTrackID: f.AudioTrackID, isDrc: f.IsDrc}
		existing, exists := byStream[key]
		if !exists {
			byStream[key] = f
			continue
		}
		fAuth := authLevelOf(&f)
		eAuth := authLevelOf(&existing)
		if fAuth < eAuth {
			byStream[key] = f
		}
	}

	result := make([]Format, 0, len(byStream))
	for _, f := range byStream {
		result = append(result, f)
	}
	// Ordered by the whole key so the output is deterministic across map
	// iterations — the itag-only sort stopped being total the moment one itag
	// could appear more than once.
	slices.SortFunc(result, func(a, b Format) int {
		if c := cmp.Compare(a.Itag, b.Itag); c != 0 {
			return c
		}
		if c := cmp.Compare(a.AudioTrackID, b.AudioTrackID); c != 0 {
			return c
		}
		return cmp.Compare(boolOrder(a.IsDrc), boolOrder(b.IsDrc))
	})
	return collapseToPreferredRendition(result)
}

// collapseToPreferredRendition keeps one rendition per itag: the highest
// audioTrackScore (which already ranks a clean rendition above its DRC twin),
// then the existing lowest-auth-level tie-break. streams arrives sorted by the
// whole 3-part key, so the winners come out in ascending itag order — the same
// order, row for row, that the pre-collapse dedup produced for every response
// without an audioTrack or an isDrc flag.
//
// Video itags are untouched: they carry no track fields, so one itag can only
// hold one stream and the loop hands it straight back.
func collapseToPreferredRendition(streams []Format) []Format {
	preferred := make(map[int]Format, len(streams))
	order := make([]int, 0, len(streams))
	for _, f := range streams {
		best, seen := preferred[f.Itag]
		if !seen {
			preferred[f.Itag] = f
			order = append(order, f.Itag)
			continue
		}
		if fScore, bestScore := audioTrackScore(&f), audioTrackScore(&best); fScore != bestScore {
			if fScore > bestScore {
				preferred[f.Itag] = f
			}
			continue
		}
		if authLevelOf(&f) < authLevelOf(&best) {
			preferred[f.Itag] = f
		}
	}

	result := make([]Format, 0, len(order))
	for _, itag := range order {
		result = append(result, preferred[itag])
	}
	return result
}

// boolOrder gives false < true, so the clean rendition sorts before its DRC twin.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
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

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
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
