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

		// Parse attributes
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

	// Audio-only request
	if qualityPref == "audio_only" {
		for i := range variants {
			if strings.Contains(strings.ToLower(variants[i].Name), "audio_only") {
				return &variants[i]
			}
		}
		// Fallback to last variant (usually lowest quality)
		return &variants[len(variants)-1]
	}

	// Build filtered list (exclude audio_only)
	var filtered []TwitchHLSVariant
	for _, v := range variants {
		if strings.Contains(strings.ToLower(v.Name), "audio_only") {
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

	// Specific quality preference — match by height like TS
	if qualityPref != "" {
		// Try exact height match first (e.g. "720p" → height==720)
		var targetHeight int
		fmt.Sscanf(qualityPref, "%dp", &targetHeight)
		if targetHeight > 0 {
			for i := range filtered {
				if filtered[i].Height == targetHeight {
					return &filtered[i]
				}
			}
		}
		// Fallback to substring match on name
		for i := range filtered {
			if strings.Contains(filtered[i].Name, qualityPref) {
				return &filtered[i]
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
		return nil, fmt.Errorf("hls playlist http %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ParseHLSMasterPlaylist(string(data)), nil
}
