package twitch

import (
	"context"
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
		return &variants[0]
	}

	// Apply max resolution cap (only if non-empty result)
	if maxResolution > 0 {
		var withinCap []TwitchHLSVariant
		for _, v := range filtered {
			maxDim := v.Width
			if v.Height > maxDim {
				maxDim = v.Height
			}
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

// FetchHLSMasterPlaylist fetches and parses an HLS master playlist from a URL.
func FetchHLSMasterPlaylist(ctx context.Context, url string) ([]TwitchHLSVariant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Client-ID", constants.TwitchGQLClientID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch hls playlist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // drain for connection reuse
		return nil, fmt.Errorf("hls playlist http %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return nil, err
	}

	return ParseHLSMasterPlaylist(string(data)), nil
}
