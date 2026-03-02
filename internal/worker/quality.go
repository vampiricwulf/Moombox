package worker

import (
	"fmt"
	"strconv"
	"strings"
)

// QualityInfo describes the resolution and framerate of a download stream.
type QualityInfo struct {
	Width  int
	Height int
	FPS    int
	Label  string // e.g. "1080p60", "720p"
}

// Changed returns true if the quality differs in resolution.
// FPS-only changes are ignored — splitting for framerate alone adds file
// overhead without saving storage since the total data volume is the same.
func (q QualityInfo) Changed(other QualityInfo) bool {
	return q.Width != other.Width || q.Height != other.Height
}

// FormatQualityLabel builds a label like "1080p60" or "720p".
// FPS is omitted when <= 30.
func FormatQualityLabel(height, fps int) string {
	if height <= 0 {
		return "unknown"
	}
	if fps > 30 {
		return fmt.Sprintf("%dp%d", height, fps)
	}
	return fmt.Sprintf("%dp", height)
}

// ParseQualityPreference parses a quality preference string into target height and fps.
//
//	"1080p60" → (1080, 60)
//	"720p"    → (720, 0)
//	"best"    → (0, 0)
//	"audio_only" → (-1, 0)
func ParseQualityPreference(pref string) (height int, fps int) {
	if pref == "" || pref == "best" {
		return 0, 0
	}
	if pref == "audio_only" {
		return -1, 0
	}

	// Try "NNNpNN" format
	s := strings.TrimSuffix(pref, "p")
	if idx := strings.Index(pref, "p"); idx > 0 {
		heightStr := pref[:idx]
		fpsStr := pref[idx+1:]
		h, err := strconv.Atoi(heightStr)
		if err == nil && h > 0 {
			height = h
			if fpsStr != "" {
				f, err := strconv.Atoi(fpsStr)
				if err == nil {
					fps = f
				}
			}
			return
		}
	}

	// Fallback: try plain number
	if h, err := strconv.Atoi(s); err == nil && h > 0 {
		return h, 0
	}
	return 0, 0
}
