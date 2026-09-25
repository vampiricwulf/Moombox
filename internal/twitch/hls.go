package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// codecRank orders the video families an enhanced broadcast can offer.
//
// This is what makes the whole feature byte-compatible: a pre-enhanced playlist
// lists only H.264 renditions, so every source ties at "avc1" and
// selectSourceVariant's `> codecRank("avc1")` guard keeps playlist order — the
// incumbent, the first source in playlist order, wins exactly as before. An
// ABSENT family ranks 0, below avc1's 1, so a CODECS-less playlist (an older
// capture, a fixture) ties the same way one rank lower.
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

// rankAtChosenSize returns the best variant among those whose CapDimension
// equals size, or nil when none does.
//
// Ruling R2: size first (the R1 rule resolved it), then the video family
// AV1 > HEVC > H.264 > absent, then a SOURCE rendition over a transcode, then
// the higher bandwidth. Ties keep the earlier variant, so two truly
// indistinguishable renditions still resolve in playlist order.
//
// This replaces selectSourceVariant, which only ever looked at IsSource
// variants and ranked them by codec then pixel area. Two things moved: the
// codec now outranks the source flag (an enhanced AV1 rendition is the better
// archive even when Twitch does not flag it chunked), and pixel area is gone
// because the chosen size has already fixed the short edge.
func rankAtChosenSize(variants []TwitchHLSVariant, size int) *TwitchHLSVariant {
	best := -1
	for i := range variants {
		if utils.CapDimension(variants[i].Width, variants[i].Height) != size {
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
// The returned pointer may point into either the caller-owned `variants`
// slice OR an internal filtered slice (audio_only-stripped, then optionally
// resolution-capped). Callers MUST treat the return value as read-only:
// mutating it has undefined effect on the caller's `variants`. Go's escape
// analysis keeps the underlying array live for the pointer's lifetime, so
// the read path is safe. Audit-finding #26.
func SelectBestVariant(variants []TwitchHLSVariant, qualityPref string, maxResolution int) *TwitchHLSVariant {
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
		if len(withinCap) > 0 {
			filtered = withinCap
		}
	}

	// Specific quality preference — match by height and optionally FPS
	if qualityPref != "" && qualityPref != "best" {
		targetHeight, targetFPS := parseQualityPref(qualityPref)
		if targetHeight > 0 {
			// Try exact height match
			if match := selectVariantByHeight(filtered, targetHeight, targetFPS); match != nil {
				return match
			}
			// Descend through lower heights
			if match := selectNextLowerVariant(filtered, targetHeight); match != nil {
				return match
			}
			// No lower heights — fall through to source/best
		} else {
			// Non-height pref (e.g. named quality) — substring match on name
			for i := range filtered {
				if strings.Contains(filtered[i].Name, qualityPref) {
					return &filtered[i]
				}
			}
		}
	}

	// Rank the chosen size: codec, then source, then bandwidth (ruling R2).
	if haveCapSize {
		if best := rankAtChosenSize(filtered, capSize); best != nil {
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

// selectVariantByHeight finds a variant matching the target height, optionally with FPS.
func selectVariantByHeight(variants []TwitchHLSVariant, targetHeight, targetFPS int) *TwitchHLSVariant {
	var heightMatches []int
	for i := range variants {
		if variants[i].Height == targetHeight {
			heightMatches = append(heightMatches, i)
		}
	}
	if len(heightMatches) == 0 {
		return nil
	}
	// If FPS-specific, prefer highest bandwidth among FPS matches
	if targetFPS > 0 {
		bestFPS := -1
		for _, idx := range heightMatches {
			if variants[idx].FPS >= float64(targetFPS)-1 {
				if bestFPS == -1 || variants[idx].Bandwidth > variants[bestFPS].Bandwidth {
					bestFPS = idx
				}
			}
		}
		if bestFPS >= 0 {
			return &variants[bestFPS]
		}
	}
	// Return highest bandwidth at target height
	best := heightMatches[0]
	for _, idx := range heightMatches[1:] {
		if variants[idx].Bandwidth > variants[best].Bandwidth {
			best = idx
		}
	}
	return &variants[best]
}

// selectNextLowerVariant finds the best variant below the target height,
// descending through available heights. Returns nil if no lower heights exist.
func selectNextLowerVariant(variants []TwitchHLSVariant, targetHeight int) *TwitchHLSVariant {
	bestHeight := 0
	for i := range variants {
		h := variants[i].Height
		if h < targetHeight && h > bestHeight {
			bestHeight = h
		}
	}
	if bestHeight == 0 {
		return nil
	}
	var best *TwitchHLSVariant
	for i := range variants {
		if variants[i].Height == bestHeight {
			if best == nil || variants[i].Bandwidth > best.Bandwidth {
				best = &variants[i]
			}
		}
	}
	return best
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

// FetchHLSMasterPlaylist fetches and parses an HLS master playlist from a URL.
func FetchHLSMasterPlaylist(ctx context.Context, url string) ([]TwitchHLSVariant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Client-ID", constants.TwitchGQLClientID)

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch hls playlist: %w", err)
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
