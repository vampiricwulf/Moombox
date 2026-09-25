package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
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
//  2. Resolution cap (maxRes) — ruling R1: the cap compares the SHORTER frame
//     dimension and resolves to one size, the largest at or below it, or the
//     closest above it when nothing is at or below. `0` is unbounded. It never
//     empties the candidate list.
//  3. Quality preference (qualityPref) — targets a specific resolution/FPS like
//     "1080p60" among everything at or below the chosen size, so a per-job
//     preference lower than the cap is still honoured
//  4. Highest bandwidth among the streams AT the chosen size (source/best)
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

	// Build the type-filtered candidate list (indices into streams).
	typed := make([]int, 0, len(streams))
	for i := range streams {
		s := &streams[i]
		if isVideo && !strings.Contains(s.MimeType, "video") {
			continue
		}
		if !isVideo && !strings.Contains(s.MimeType, "audio") {
			continue
		}
		typed = append(typed, i)
	}

	if len(typed) == 0 {
		return nil
	}

	// Resolve the resolution cap to a size, then keep everything at or below
	// it. Audio streams carry no frame size and are never capped.
	candidates := typed
	capSize, haveCapSize := 0, false
	if isVideo {
		cands := make([]utils.Cand, len(typed))
		for i, idx := range typed {
			cands[i] = utils.Cand{Width: streams[idx].Width, Height: streams[idx].Height}
		}
		capSize, haveCapSize = utils.SelectByCap(maxRes, cands)
		if haveCapSize {
			capped := make([]int, 0, len(typed))
			for _, idx := range typed {
				if utils.CapDimension(streams[idx].Width, streams[idx].Height) <= capSize {
					capped = append(capped, idx)
				}
			}
			candidates = capped
		}
	}

	// Quality preference targeting for video streams, over everything at or
	// below the chosen size — a job that asked for 720p under a 2160 cap gets
	// 720p, exactly as it did before this arc.
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

	// Default: highest bandwidth AT the chosen size (source/best). Restricting
	// to the chosen size is what makes "the largest rendition at or below the
	// cap" true even when a smaller rendition happens to carry more bits.
	atSize := candidates
	if isVideo && haveCapSize {
		atSize = make([]int, 0, len(candidates))
		for _, idx := range candidates {
			if utils.CapDimension(streams[idx].Width, streams[idx].Height) == capSize {
				atSize = append(atSize, idx)
			}
		}
		// Defensive, as above — capSize is always attained by some candidate.
		if len(atSize) == 0 {
			atSize = candidates
		}
	}
	best := atSize[0]
	for _, idx := range atSize[1:] {
		if streams[idx].Bandwidth > streams[best].Bandwidth {
			best = idx
		}
	}
	return &streams[best]
}

// selectAtHeightIdx returns the index into items of the best-bandwidth entry
// at targetHeight, preferring entries whose FPS meets targetFPS-1 when
// targetFPS > 0. Returns -1 when no entry matches the target height.
// Accessor extracts (height, fps, bandwidth) from each item.
//
// Shared between DASH and HLS variant selection — the algorithm is identical
// modulo the underlying stream type (audit reports/worker.md F35).
func selectAtHeightIdx[T any](items []T, accessor func(T) (height, fps, bandwidth int), targetHeight, targetFPS int) int {
	best := -1
	var bestBw int
	foundAtFPS := false
	for i, item := range items {
		h, f, bw := accessor(item)
		if h != targetHeight {
			continue
		}
		isFPSMatch := targetFPS > 0 && f >= targetFPS-1
		if !isFPSMatch && foundAtFPS {
			// Already have FPS matches; ignore non-FPS entries.
			continue
		}
		if isFPSMatch && !foundAtFPS {
			// First FPS match — reset best to favour this entry over any
			// earlier non-FPS ones.
			best = i
			bestBw = bw
			foundAtFPS = true
			continue
		}
		if best < 0 || bw > bestBw {
			best = i
			bestBw = bw
		}
	}
	return best
}

// selectNextLowerIdx returns the index into items of the best-bandwidth entry
// whose height is strictly below targetHeight, or -1 if nothing is below.
// Shared with HLS (audit reports/worker.md F36).
func selectNextLowerIdx[T any](items []T, accessor func(T) (height, bandwidth int), targetHeight int) int {
	bestHeight := 0
	for _, item := range items {
		h, _ := accessor(item)
		if h < targetHeight && h > bestHeight {
			bestHeight = h
		}
	}
	if bestHeight == 0 {
		return -1
	}
	best := -1
	var bestBw int
	for i, item := range items {
		h, bw := accessor(item)
		if h == bestHeight {
			if best < 0 || bw > bestBw {
				best = i
				bestBw = bw
			}
		}
	}
	return best
}

func dashFieldAccessor(s DashStreamInfo) (int, int, int) {
	return s.Height, s.FPS, s.Bandwidth
}

func dashHeightBandwidth(s DashStreamInfo) (int, int) {
	return s.Height, s.Bandwidth
}

// selectByHeightPref finds a DASH stream matching the target height, optionally with FPS.
// Returns highest bandwidth at that height, preferring FPS match if targetFPS > 0.
func selectByHeightPref(streams []DashStreamInfo, candidates []int, targetHeight, targetFPS int) *DashStreamInfo {
	// Build a filtered view so the generic helper operates on exactly the
	// caller's candidate set; map the returned index back to the original.
	filtered := make([]DashStreamInfo, len(candidates))
	for i, idx := range candidates {
		filtered[i] = streams[idx]
	}
	idx := selectAtHeightIdx(filtered, dashFieldAccessor, targetHeight, targetFPS)
	if idx < 0 {
		return nil
	}
	return &streams[candidates[idx]]
}

// selectNextLowerHeight finds the best DASH stream below the target height,
// descending through available heights. Returns nil if no lower heights exist.
func selectNextLowerHeight(streams []DashStreamInfo, candidates []int, targetHeight int) *DashStreamInfo {
	filtered := make([]DashStreamInfo, len(candidates))
	for i, idx := range candidates {
		filtered[i] = streams[idx]
	}
	idx := selectNextLowerIdx(filtered, dashHeightBandwidth, targetHeight)
	if idx < 0 {
		return nil
	}
	return &streams[candidates[idx]]
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
