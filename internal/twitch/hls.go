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
)

var (
	hlsBandwidthRe  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	hlsResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	hlsFrameRateRe  = regexp.MustCompile(`FRAME-RATE=([\d.]+)`)
	hlsVideoGroupRe = regexp.MustCompile(`VIDEO="([^"]+)"`)
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

	// Apply max resolution cap (only if non-empty result).
	// We compare the larger dimension (max(Height, Width)) against the cap
	// so vertical/portrait streams (e.g. 720x1280) are filtered against
	// their *long edge* — i.e. a 720x1280 portrait stream counts as 1280p
	// for cap purposes, matching how a typical 16:9 1920x1080 stream is
	// treated as 1920 wide. See audit-finding #23.
	if maxResolution > 0 {
		var withinCap []TwitchHLSVariant
		for _, v := range filtered {
			maxDim := max(v.Height, v.Width)
			if maxDim <= maxResolution {
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

	// Prefer source quality
	for i := range filtered {
		if filtered[i].IsSource {
			return &filtered[i]
		}
	}

	// Highest bandwidth
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
