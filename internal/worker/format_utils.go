package worker

import (
	"fmt"
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
//  4. Among the streams AT the chosen size (source/best): the frame rate
//     prefer60fps asks for, then the highest bandwidth
//
// Sizes are the frame's SHORTER edge (utils.CapDimension) throughout — the
// cap's own measure — so a portrait stream's "1080p" is its 1080x1920
// rendition, not whichever rendition happens to be 1080 tall. Frame rate is
// ranked by fpsPreference: an explicit "…p60" asks for 60, and otherwise
// prefer60fps decides, as it does on the whole-file VOD path.
//
// For audio streams (isVideo=false), qualityPref, maxRes and prefer60fps are
// ignored.
func SelectBestDashStream(streams []DashStreamInfo, preferItag int, maxRes int, isVideo bool, qualityPref string, prefer60fps bool) *DashStreamInfo {
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
			prefer := fpsPreference(targetFPS, prefer60fps)
			if match := selectByHeightPref(streams, candidates, targetHeight, prefer); match != nil {
				return match
			}
			// Target height not found — descend to the next lower size,
			// ranked there by the same frame-rate rule
			if match := selectNextLowerHeight(streams, candidates, targetHeight, prefer); match != nil {
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
	if !isVideo {
		best := atSize[0]
		for _, idx := range atSize[1:] {
			if streams[idx].Bandwidth > streams[best].Bandwidth {
				best = idx
			}
		}
		return &streams[best]
	}
	filtered := make([]DashStreamInfo, len(atSize))
	for i, idx := range atSize {
		filtered[i] = streams[idx]
	}
	return &streams[atSize[rankByFPSThenBandwidth(filtered, dashFieldAccessor, fpsPreference(0, prefer60fps))]]
}

// fpsPreference is the frame-rate test a rendition is ranked by among those of
// one size: an explicit "…p60" (targetFPS > 0) asks for at least targetFPS-1;
// otherwise prefer60fps decides — 50 fps and up when set, a known rate of 31
// and below when not. The VOD path's selector honours prefer_60fps the same
// way; the live paths used to ignore it, so a 30 fps recording of a stream
// that also offered 60 could not be had short of pinning an itag.
func fpsPreference(targetFPS int, prefer60fps bool) func(fps int) bool {
	switch {
	case targetFPS > 0:
		return func(fps int) bool { return fps >= targetFPS-1 }
	case prefer60fps:
		return func(fps int) bool { return fps >= 50 }
	default:
		return func(fps int) bool { return fps > 0 && fps <= 31 }
	}
}

// rankByFPSThenBandwidth returns the index of the best item: the highest
// bandwidth among those whose frame rate prefer accepts, or among all of them
// when it accepts none. items must not be empty.
func rankByFPSThenBandwidth[T any](items []T, accessor func(T) (size, fps, bandwidth int), prefer func(int) bool) int {
	best, bestPreferred := -1, false
	var bestBw int
	for i, item := range items {
		_, f, bw := accessor(item)
		preferred := prefer(f)
		switch {
		case best < 0, preferred && !bestPreferred:
		case preferred == bestPreferred && bw > bestBw:
		default:
			continue
		}
		best, bestPreferred, bestBw = i, preferred, bw
	}
	return best
}

// selectAtHeightIdx returns the index into items of the best entry at
// targetHeight — ranked by rankByFPSThenBandwidth under prefer — or -1 when no
// entry matches the target height. Accessor extracts (size, fps, bandwidth)
// from each item, size being the frame's shorter edge.
//
// Shared between DASH and HLS variant selection — the algorithm is identical
// modulo the underlying stream type (audit reports/worker.md F35).
func selectAtHeightIdx[T any](items []T, accessor func(T) (size, fps, bandwidth int), targetHeight int, prefer func(int) bool) int {
	var at []int
	for i, item := range items {
		if h, _, _ := accessor(item); h == targetHeight {
			at = append(at, i)
		}
	}
	if len(at) == 0 {
		return -1
	}
	sub := make([]T, len(at))
	for i, idx := range at {
		sub[i] = items[idx]
	}
	return at[rankByFPSThenBandwidth(sub, accessor, prefer)]
}

// selectNextLowerIdx returns the index into items of the entry a preference
// picks at the largest size strictly below targetHeight — ranked there by
// selectAtHeightIdx under prefer, exactly as the size the preference named
// would have been — or -1 if nothing is below. Shared with HLS (audit
// reports/worker.md F36).
//
// It ranked the lower size by bandwidth alone, so a "1440p" on a stream
// whose top size was 1080 recorded the 1080p60 rendition with prefer_60fps
// off — the 1080p30 one that "1080p" itself took — and a "1440p60" could
// land on a 30 fps rendition that out-bit the 60 beside it. Twitch's descent
// (selectNextLowerVariant) has applied its size's ranking, fps suffix
// included, since D-Y2.
func selectNextLowerIdx[T any](items []T, accessor func(T) (size, fps, bandwidth int), targetHeight int, prefer func(int) bool) int {
	lower := 0
	for _, item := range items {
		if h, _, _ := accessor(item); h < targetHeight && h > lower {
			lower = h
		}
	}
	if lower == 0 {
		return -1
	}
	return selectAtHeightIdx(items, accessor, lower, prefer)
}

// dashFieldAccessor measures a stream by its frame's SHORTER edge, the cap's
// own measure (ruling R1): a preference compared against the raw Height sent a
// portrait stream's "1080p" (1080x1920) down the next-lower-height descent to
// its 480x854 rendition.
func dashFieldAccessor(s DashStreamInfo) (int, int, int) {
	return utils.CapDimension(s.Width, s.Height), s.FPS, s.Bandwidth
}

// selectByHeightPref finds a DASH stream matching the target height, ranked by
// frame rate (prefer) and then bandwidth.
func selectByHeightPref(streams []DashStreamInfo, candidates []int, targetHeight int, prefer func(int) bool) *DashStreamInfo {
	// Build a filtered view so the generic helper operates on exactly the
	// caller's candidate set; map the returned index back to the original.
	filtered := make([]DashStreamInfo, len(candidates))
	for i, idx := range candidates {
		filtered[i] = streams[idx]
	}
	idx := selectAtHeightIdx(filtered, dashFieldAccessor, targetHeight, prefer)
	if idx < 0 {
		return nil
	}
	return &streams[candidates[idx]]
}

// selectNextLowerHeight finds the DASH stream a preference picks at the next
// lower size below the target height, ranked by frame rate (prefer) and then
// bandwidth. Returns nil if no lower size exists.
func selectNextLowerHeight(streams []DashStreamInfo, candidates []int, targetHeight int, prefer func(int) bool) *DashStreamInfo {
	filtered := make([]DashStreamInfo, len(candidates))
	for i, idx := range candidates {
		filtered[i] = streams[idx]
	}
	idx := selectNextLowerIdx(filtered, dashFieldAccessor, targetHeight, prefer)
	if idx < 0 {
		return nil
	}
	return &streams[candidates[idx]]
}

// warnUnhonouredPins logs a manual itag the live selection could not honour.
// SelectBestDashStream falls back to automatic selection when a pinned itag
// is not in the pool, and the live strategies used to say nothing about it —
// only "selected" lines naming another itag, while the whole-file VOD path
// warns ("Manual video itag N not found, falling back to auto"). A pin also
// switches the quality monitor off, so this line is the one place the
// operator learns the pin did not take.
func warnUnhonouredPins(job *JobContext, videoItag int, video *DashStreamInfo, audioItag int, audio *DashStreamInfo) {
	if videoItag > 0 && video != nil && video.Itag != videoItag {
		job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d not offered for this stream; recording itag %d instead", videoItag, video.Itag))
	}
	if audioItag > 0 && audio != nil && audio.Itag != audioItag {
		job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual audio itag %d not offered for this stream; recording itag %d instead", audioItag, audio.Itag))
	}
}

// DashStreamInfo holds basic info about a DASH stream for selection.
type DashStreamInfo struct {
	Itag           int
	MimeType       string
	Width          int
	Height         int
	FPS            int // From DASH frameRate attribute (0 if not present)
	Bandwidth      int
	BaseURL        string
	Initialization string // Init segment URL
}
