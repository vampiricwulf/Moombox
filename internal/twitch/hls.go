package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

var (
	hlsBandwidthRe  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	hlsResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	hlsFrameRateRe  = regexp.MustCompile(`FRAME-RATE=([\d.]+)`)
	hlsVideoGroupRe = regexp.MustCompile(`VIDEO="([^"]+)"`)
	hlsCodecsRe     = regexp.MustCompile(`CODECS="([^"]*)"`)
)

// ParseHLSMasterPlaylist parses a Twitch HLS master playlist into variants.
func ParseHLSMasterPlaylist(content string) []TwitchHLSVariant {
	lines := strings.Split(content, "\n")
	var variants []TwitchHLSVariant

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}

		variant := TwitchHLSVariant{}

		// Parse attributes (regex-validated digits; parse errors default to zero)
		if m := hlsBandwidthRe.FindStringSubmatch(line); m != nil {
			variant.Bandwidth, _ = strconv.Atoi(m[1])
		}
		if m := hlsResolutionRe.FindStringSubmatch(line); m != nil {
			variant.Width, _ = strconv.Atoi(m[1])
			variant.Height, _ = strconv.Atoi(m[2])
		}
		if m := hlsFrameRateRe.FindStringSubmatch(line); m != nil {
			variant.FPS, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := hlsVideoGroupRe.FindStringSubmatch(line); m != nil {
			variant.VideoGroup = m[1]
		}
		if m := hlsCodecsRe.FindStringSubmatch(line); m != nil {
			variant.Codecs = m[1]
			variant.VideoCodec = videoCodecFamily(m[1])
		}

		// Next non-empty line is the URL
		for i++; i < len(lines); i++ {
			url := strings.TrimSpace(lines[i])
			if url != "" && !strings.HasPrefix(url, "#") {
				variant.URL = url
				break
			}
		}

		if variant.URL == "" {
			continue
		}

		// Derive name and isSource
		variant.IsSource = variant.VideoGroup == "chunked" || strings.Contains(strings.ToLower(variant.VideoGroup), "source")

		if variant.VideoGroup != "" {
			variant.Name = variant.VideoGroup
		} else if variant.Height > 0 {
			fpsLabel := "30"
			if variant.FPS >= 59 {
				fpsLabel = "60"
			}
			variant.Name = fmt.Sprintf("%dp%s", variant.Height, fpsLabel)
		} else {
			variant.Name = "unknown"
		}

		variants = append(variants, variant)
	}

	return variants
}

// videoCodecFamily normalizes an HLS CODECS attribute to the video family it
// carries: "av01", "hevc", "avc1", or "" when the list holds no recognised
// video codec (an audio-only rendition, or a playlist that sends no CODECS at
// all). Twitch usher playlists DO carry CODECS without the enhanced-broadcast
// opt-in — a pre-enhanced playlist simply lists only H.264 renditions, so
// every source in it reports "avc1".
//
// The list is scanned in order rather than read at index 0: the video entry is
// not always first, and an audio-only rendition's single mp4a entry must not be
// mistaken for one. Matching is on the RFC 6381 ids a PLAYLIST uses
// (av01…/hev1…/hvc1…/avc1…/avc3…), not the short names the usher REQUEST sends.
func videoCodecFamily(codecs string) string {
	for entry := range strings.SplitSeq(codecs, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case strings.HasPrefix(entry, "av01"):
			return "av01"
		case strings.HasPrefix(entry, "hev1"), strings.HasPrefix(entry, "hvc1"):
			return "hevc"
		case strings.HasPrefix(entry, "avc1"), strings.HasPrefix(entry, "avc3"):
			return "avc1"
		}
	}
	return ""
}

// codecRank orders the video families an enhanced broadcast can offer. It is
// the first rung rankAtChosenSize applies once R1 has fixed the size: AV1 3 >
// HEVC 2 > H.264 1, with an ABSENT family at 0.
//
// A pre-enhanced playlist lists only H.264 renditions, so every rendition ties
// at 1 and the frame rate, the SOURCE flag and then the bandwidth decide —
// playlist order is the last rung now, not the first. A CODECS-less playlist
// (an older capture, a fixture) ties the same way one rank lower.
func codecRank(family string) int {
	switch family {
	case "av01":
		return 3
	case "hevc":
		return 2
	case "avc1":
		return 1
	default:
		return 0
	}
}

// preferredFrameRate reports whether a rendition's frame rate is the one
// prefer_60fps asks for: 50 fps and up when it is on, a known rate of 31 and
// below when it is off. The thresholds are the YouTube selectors' own
// (fpsPreference in internal/worker/format_utils.go), so one setting means one
// thing on both platforms; Twitch's NTSC rates (59.94, 29.97) fall on the
// sides they are named for.
//
// The rate is the playlist's FRAME-RATE attribute, which ParseHLSMasterPlaylist
// reads into FPS — Twitch sends it on every rendition, live and VOD. A
// rendition without one (FPS 0) is preferred under neither setting, as an
// unknown rate is on the YouTube side, so it ties with the other unknowns and
// loses to a rendition whose rate is known to match.
func preferredFrameRate(fps float64, prefer60fps bool) bool {
	if prefer60fps {
		return fps >= 50
	}
	return fps > 0 && fps <= 31
}

// rankAtChosenSize returns the best variant among those whose CapDimension
// equals size, or nil when none does.
//
// Ruling R2, as amended by D-Y2: size first (the R1 rule resolved it), then
// the video family AV1 > HEVC > H.264 > absent, then the frame rate
// prefer60fps asks for (preferredFrameRate), then a SOURCE rendition over a
// transcode, then the higher bandwidth. Ties keep the earlier variant, so two
// truly indistinguishable renditions still resolve in playlist order.
//
// This replaces selectSourceVariant, which only ever looked at IsSource
// variants and ranked them by codec then pixel area. Two things moved: the
// codec now outranks the source flag (an enhanced AV1 rendition is the better
// archive even when Twitch does not flag it chunked), and pixel area is gone
// because the chosen size has already fixed the short edge.
//
// The frame-rate rung sits ABOVE the source flag, which is the point of it: a
// Twitch source is whatever the broadcaster sends, so a 1080p60 source beside
// a 1080p30 transcode is the common shape, and with prefer_60fps off the
// source flag used to win it every time — the setting was read by every
// YouTube selector and by none of these. It sits BELOW the codec: an AV1
// 30 fps rendition is still the better archive than an H.264 60 fps one.
func rankAtChosenSize(variants []TwitchHLSVariant, size int, prefer60fps bool) *TwitchHLSVariant {
	return rankWhere(variants, prefer60fps, func(v *TwitchHLSVariant) bool {
		return utils.CapDimension(v.Width, v.Height) == size
	})
}

// rankWhere is rankAtChosenSize's ranking over the variants keep accepts —
// codec, the frame rate prefer60fps asks for, the source flag, bandwidth, the
// earlier variant on a tie — or nil when keep accepts none. The pointer is
// into variants.
func rankWhere(variants []TwitchHLSVariant, prefer60fps bool, keep func(*TwitchHLSVariant) bool) *TwitchHLSVariant {
	best := -1
	for i := range variants {
		if !keep(&variants[i]) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		cur, cand := codecRank(variants[best].VideoCodec), codecRank(variants[i].VideoCodec)
		if cand != cur {
			if cand > cur {
				best = i
			}
			continue
		}
		curRate := preferredFrameRate(variants[best].FPS, prefer60fps)
		candRate := preferredFrameRate(variants[i].FPS, prefer60fps)
		if candRate != curRate {
			if candRate {
				best = i
			}
			continue
		}
		if variants[i].IsSource != variants[best].IsSource {
			if variants[i].IsSource {
				best = i
			}
			continue
		}
		if variants[i].Bandwidth > variants[best].Bandwidth {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &variants[best]
}

// SelectBestVariant selects the best HLS variant based on preferences.
//
// maxResolution and prefer60fps are the downloader settings
// max_video_resolution and prefer_60fps. prefer60fps decides only among the
// renditions of one size, by rankAtChosenSize's ranking: the size a height in
// qualityPref names (an fps suffix there wins over it), or failing that the
// next lower size, or failing both the size the cap chose.
//
// The returned pointer may point into either the caller-owned `variants`
// slice OR an internal filtered slice (audio_only-stripped, then optionally
// resolution-capped). Callers MUST treat the return value as read-only:
// mutating it has undefined effect on the caller's `variants`. Go's escape
// analysis keeps the underlying array live for the pointer's lifetime, so
// the read path is safe. Audit-finding #26.
func SelectBestVariant(variants []TwitchHLSVariant, qualityPref string, maxResolution int, prefer60fps bool) *TwitchHLSVariant {
	if len(variants) == 0 {
		return nil
	}

	// Pre-compute lowered names to avoid repeated ToLower calls
	loweredNames := make([]string, len(variants))
	for i := range variants {
		loweredNames[i] = strings.ToLower(variants[i].Name)
	}

	// Audio-only request
	if qualityPref == "audio_only" {
		for i := range variants {
			if strings.Contains(loweredNames[i], "audio_only") {
				return &variants[i]
			}
		}
		// Fallback to last variant (usually lowest quality)
		return &variants[len(variants)-1]
	}

	// Build filtered list (exclude audio_only)
	var filtered []TwitchHLSVariant
	for i, v := range variants {
		if strings.Contains(loweredNames[i], "audio_only") {
			continue
		}
		filtered = append(filtered, v)
	}

	if len(filtered) == 0 {
		// All variants were audio-only and the caller did NOT request
		// audio_only. Returning variants[0] degrades gracefully to an
		// audio-only stream rather than failing the download outright —
		// the user gets at least audio. Audit-finding #25.
		return &variants[0]
	}

	// Apply the resolution cap. Ruling R1: the cap compares the SHORTER frame
	// dimension, so a 720x1280 portrait stream counts as 720p and a 3840x2160
	// source counts as 2160p — the label the operator typed. The cap resolves
	// to ONE size (the largest at or below it, else the closest above it, with
	// 0 meaning unbounded) and everything at or below that size is kept, so the
	// quality preference below still has a ladder to search. It can never empty
	// the list: the pre-arc code silently fell through to the UNFILTERED
	// variants when it did, which made the cap mean nothing at all in exactly
	// the case it mattered.
	cands := make([]utils.Cand, len(filtered))
	for i := range filtered {
		cands[i] = utils.Cand{Width: filtered[i].Width, Height: filtered[i].Height}
	}
	capSize, haveCapSize := utils.SelectByCap(maxResolution, cands)
	if haveCapSize {
		var withinCap []TwitchHLSVariant
		for _, v := range filtered {
			if utils.CapDimension(v.Width, v.Height) <= capSize {
				withinCap = append(withinCap, v)
			}
		}
		filtered = withinCap
	}

	// Specific quality preference — match by height and optionally FPS
	if qualityPref != "" && qualityPref != "best" {
		targetHeight, targetFPS := parseQualityPref(qualityPref)
		if targetHeight > 0 {
			// The size the preference names, by the short edge
			if match := selectVariantByHeight(filtered, targetHeight, targetFPS, prefer60fps); match != nil {
				return match
			}
			// Descend to the next lower size
			if match := selectNextLowerVariant(filtered, targetHeight, targetFPS, prefer60fps); match != nil {
				return match
			}
			// No lower size — fall through to source/best
		} else {
			// Non-height pref (e.g. named quality) — substring match on name
			for i := range filtered {
				if strings.Contains(filtered[i].Name, qualityPref) {
					return &filtered[i]
				}
			}
		}
	}

	// Rank the chosen size: codec, then frame rate, then source, then
	// bandwidth (ruling R2, amended by D-Y2).
	if haveCapSize {
		if best := rankAtChosenSize(filtered, capSize, prefer60fps); best != nil {
			return best
		}
	}

	// Highest bandwidth. Defensive, and unreachable while rankAtChosenSize
	// answers from the same list SelectByCap resolved capSize from: some
	// element always has that CapDimension, and a playlist with no RESOLUTION
	// at all resolves to size 0, which every variant matches. Kept so the
	// function stays total if either rule is ever changed on its own.
	best := &filtered[0]
	for i := 1; i < len(filtered); i++ {
		if filtered[i].Bandwidth > best.Bandwidth {
			best = &filtered[i]
		}
	}
	return best
}

// selectVariantByHeight returns the variant a height preference picks at
// size targetHeight, or nil when no variant has that size. A variant's size is
// its SHORT edge (utils.CapDimension), the measure the cap and
// rankAtChosenSize use, so a portrait stream's 720p is its 720x1280
// rendition. An fps suffix ("720p60", targetFPS > 0) keeps the renditions of
// that size at targetFPS-1 and up when there are any; the rest is
// rankAtChosenSize's ranking, prefer60fps included — codec, frame rate,
// source, bandwidth.
//
// It used to match the raw height and take the highest bandwidth, so a
// portrait stream's 720p matched nothing (its 720x1280 transcode is 1280
// high) and the preference fell to whatever lay below it, and a suffix-less
// "720p" ignored prefer_60fps and the codec, which the size the cap chooses
// has been ranked by since D-Y2.
func selectVariantByHeight(variants []TwitchHLSVariant, targetHeight, targetFPS int, prefer60fps bool) *TwitchHLSVariant {
	if targetFPS > 0 {
		if match := rankWhere(variants, prefer60fps, func(v *TwitchHLSVariant) bool {
			return utils.CapDimension(v.Width, v.Height) == targetHeight && v.FPS >= float64(targetFPS)-1
		}); match != nil {
			return match
		}
	}
	return rankAtChosenSize(variants, targetHeight, prefer60fps)
}

// selectNextLowerVariant picks at the largest size below targetHeight, by the
// short edge and by selectVariantByHeight's rule (so an fps suffix still
// counts there). Returns nil if no smaller size exists. It descended by the
// raw height, which took a portrait stream's 480x854 rendition below a 900p
// preference while its 720x1280 one was there, and ranked by bandwidth alone.
func selectNextLowerVariant(variants []TwitchHLSVariant, targetHeight, targetFPS int, prefer60fps bool) *TwitchHLSVariant {
	lower := 0
	for i := range variants {
		if size := utils.CapDimension(variants[i].Width, variants[i].Height); size < targetHeight && size > lower {
			lower = size
		}
	}
	if lower == 0 {
		return nil
	}
	return selectVariantByHeight(variants, lower, targetFPS, prefer60fps)
}

// parseQualityPref parses a quality preference string like "1080p60" into height and fps.
// "1080p60" → (1080, 60), "720p" → (720, 0), "best" → (0, 0).
func parseQualityPref(pref string) (height int, fps int) {
	// Try "NNNpNN" format first
	n, _ := fmt.Sscanf(pref, "%dp%d", &height, &fps)
	if n >= 1 {
		return
	}
	return 0, 0
}

// isRestrictedEntitlementBody reports whether an usher error body carries a
// subscriber-only restriction code. yt-dlp twitch.py parity: the body is a
// JSON array whose first object's error_code is vod_manifest_restricted or
// unauthorized_entitlements — "You must be logged into an account that has
// access to this subscriber-only content". Anything else (geoblock, offline,
// malformed) is not a restriction match.
func isRestrictedEntitlementBody(body []byte) bool {
	var entries []struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
		return false
	}
	return entries[0].ErrorCode == "vod_manifest_restricted" ||
		entries[0].ErrorCode == "unauthorized_entitlements"
}

// withoutQuery strips the query string from the URL a *url.Error carries.
// An usher URL's query is the playback token document (user ID, client IP,
// entitlements) and its signature, and a transport failure's Error() quotes
// the whole URL: the job's error column, the dashboard, the TUI and the log
// all show it. The type survives, so a caller classifying transport errors
// still can.
func withoutQuery(err error) error {
	uerr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}
	u, _, _ := strings.Cut(uerr.URL, "?")
	return &url.Error{Op: uerr.Op, URL: u, Err: uerr.Err}
}

// FetchHLSMasterPlaylist fetches and parses an HLS master playlist from a URL.
func FetchHLSMasterPlaylist(ctx context.Context, playlistURL string) ([]TwitchHLSVariant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return nil, withoutQuery(err)
	}
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Client-ID", constants.TwitchGQLClientID)

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch hls playlist: %w", withoutQuery(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Usher returns 404 for "channel offline" vs "unauthorized" vs
		// "geoblocked" — each with a distinct body. Previously we
		// discarded the body and returned "hls playlist http 404", which
		// left users guessing why. Include a bounded prefix of the body
		// in the error (yt-dlp does the same — see references/yt-dlp
		// twitch.py). Drain the rest for connection reuse.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10)) // 4 KB prefix
		io.Copy(io.Discard, resp.Body)
		// Subscriber-only restriction gets its sentinel so the worker can
		// route the job to COOKIES? with an actionable message instead of
		// parking it in Error with a raw JSON body.
		if isRestrictedEntitlementBody(body) {
			return nil, fmt.Errorf("hls playlist http %d: %w", resp.StatusCode, ErrSubscriberOnly)
		}
		bodyStr := strings.TrimSpace(string(body))
		if bodyStr != "" {
			return nil, fmt.Errorf("hls playlist http %d: %s", resp.StatusCode, bodyStr)
		}
		return nil, fmt.Errorf("hls playlist http %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return nil, err
	}

	return ParseHLSMasterPlaylist(string(data)), nil
}
