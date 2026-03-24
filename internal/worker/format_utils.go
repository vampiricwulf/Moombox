package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// IsProgressiveFormat returns true if the format contains both video and audio
// (pre-muxed), meaning no separate audio download is needed.
func IsProgressiveFormat(f *youtube.Format) bool {
	return f != nil && f.AudioQuality != "" && f.Width != nil && f.Height != nil
}

// SelectBestDashStream selects the best stream from DASH representations.
//
// Selection priority:
//  1. Manual itag override (preferItag > 0) — exact match, bypasses all filters
//  2. Quality preference (qualityPref) — targets a specific resolution/FPS like "1080p60"
//  3. maxRes hard cap — filters out streams exceeding the pixel limit
//  4. Highest bandwidth among remaining candidates
//
// For audio streams (isVideo=false), qualityPref and maxRes are ignored.
func SelectBestDashStream(streams []DashStreamInfo, preferItag int, maxRes int, isVideo bool, qualityPref string) *DashStreamInfo {
	// Manual itag selection — bypasses all other logic
	if preferItag > 0 {
		for i := range streams {
			if streams[i].Itag == preferItag {
				return &streams[i]
			}
		}
	}

	// Build filtered candidate list
	candidates := make([]int, 0, len(streams)) // indices into streams
	for i := range streams {
		s := &streams[i]

		// Filter by type
		if isVideo && !strings.Contains(s.MimeType, "video") {
			continue
		}
		if !isVideo && !strings.Contains(s.MimeType, "audio") {
			continue
		}

		// Resolution cap for video
		if isVideo && maxRes > 0 {
			maxDim := max(s.Height, s.Width)
			if maxDim > maxRes {
				continue
			}
		}

		candidates = append(candidates, i)
	}

	if len(candidates) == 0 {
		return nil
	}

	// Quality preference targeting for video streams
	if isVideo && qualityPref != "" && qualityPref != "best" {
		targetHeight, targetFPS := ParseQualityPreference(qualityPref)
		if targetHeight > 0 {
			if match := selectByHeightPref(streams, candidates, targetHeight, targetFPS); match != nil {
				return match
			}
			// Target height not found — descend through lower heights
			if match := selectNextLowerHeight(streams, candidates, targetHeight); match != nil {
				return match
			}
			// No lower heights either — fall through to source/best
		}
	}

	// Default: highest bandwidth among candidates (source/best)
	best := candidates[0]
	for _, idx := range candidates[1:] {
		if streams[idx].Bandwidth > streams[best].Bandwidth {
			best = idx
		}
	}
	return &streams[best]
}

// selectByHeightPref finds a DASH stream matching the target height, optionally with FPS.
// Returns highest bandwidth at that height, preferring FPS match if targetFPS > 0.
func selectByHeightPref(streams []DashStreamInfo, candidates []int, targetHeight, targetFPS int) *DashStreamInfo {
	var heightMatches []int
	for _, idx := range candidates {
		if streams[idx].Height == targetHeight {
			heightMatches = append(heightMatches, idx)
		}
	}
	if len(heightMatches) == 0 {
		return nil
	}
	// If FPS-specific (e.g. "1080p60"), prefer highest bandwidth among FPS matches
	if targetFPS > 0 {
		bestFPS := -1
		for _, idx := range heightMatches {
			if streams[idx].FPS >= targetFPS-1 {
				if bestFPS == -1 || streams[idx].Bandwidth > streams[bestFPS].Bandwidth {
					bestFPS = idx
				}
			}
		}
		if bestFPS >= 0 {
			return &streams[bestFPS]
		}
	}
	// Return highest bandwidth at target height
	best := heightMatches[0]
	for _, idx := range heightMatches[1:] {
		if streams[idx].Bandwidth > streams[best].Bandwidth {
			best = idx
		}
	}
	return &streams[best]
}

// selectNextLowerHeight finds the best DASH stream below the target height,
// descending through available heights. Returns nil if no lower heights exist.
func selectNextLowerHeight(streams []DashStreamInfo, candidates []int, targetHeight int) *DashStreamInfo {
	// Find the highest available height below the target
	bestHeight := 0
	for _, idx := range candidates {
		h := streams[idx].Height
		if h < targetHeight && h > bestHeight {
			bestHeight = h
		}
	}
	if bestHeight == 0 {
		return nil
	}
	// Among streams at that height, pick highest bandwidth
	var best *DashStreamInfo
	for _, idx := range candidates {
		if streams[idx].Height == bestHeight {
			if best == nil || streams[idx].Bandwidth > best.Bandwidth {
				best = &streams[idx]
			}
		}
	}
	return best
}

// DashStreamInfo holds basic info about a DASH stream for selection.
type DashStreamInfo struct {
	Itag           int
	MimeType       string
	Codecs         string
	Width          int
	Height         int
	FPS            int // From DASH frameRate attribute (0 if not present)
	Bandwidth      int
	BaseURL        string
	Initialization string // Init segment URL
}
