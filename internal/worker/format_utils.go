package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// IsProgressiveFormat returns true if the format contains both video and audio
// (pre-muxed), meaning no separate audio download is needed.
func IsProgressiveFormat(f *youtube.Format) bool {
	return f.AudioQuality != "" && f.Width != nil && f.Height != nil
}

// SelectBestDashStream selects the best stream from DASH representations by itag or bandwidth.
func SelectBestDashStream(streams []DashStreamInfo, preferItag int, maxRes int, isVideo bool) *DashStreamInfo {
	// Manual itag selection
	if preferItag > 0 {
		for i := range streams {
			if streams[i].Itag == preferItag {
				return &streams[i]
			}
		}
	}

	var best *DashStreamInfo
	for i := range streams {
		s := &streams[i]

		// Filter by type
		if isVideo && !strings.Contains(s.MimeType, "video") {
			continue
		}
		if !isVideo && !strings.Contains(s.MimeType, "audio") {
			continue
		}

		// Resolution filter for video
		if isVideo && maxRes > 0 {
			maxDim := s.Width
			if s.Height > maxDim {
				maxDim = s.Height
			}
			if maxDim > maxRes {
				continue
			}
		}

		if best == nil || s.Bandwidth > best.Bandwidth {
			best = s
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
	Bandwidth      int
	BaseURL        string
	Initialization string // Init segment URL
}
