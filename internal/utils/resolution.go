package utils

// Cand is one candidate rendition's frame size. Every caller of SelectByCap
// maps its own format/variant/stream type into a slice of these; the selector
// answers with a SIZE, so nothing about the caller's type leaks in here.
type Cand struct {
	Width  int
	Height int
}

// CapDimension returns the dimension downloader.max_video_resolution is
// compared against: the SHORTER of the two positive dimensions. A rendition
// that reports only one dimension is measured by that one; a rendition that
// reports neither measures 0.
//
// Owner ruling R1 (2026-09-19). Every selection site in the tree used to
// compare max(width, height), which made the shipped default of 2160 mean "at
// most a 1920-wide frame": a 3840x2160 source has a 3840 long edge, so 4K was
// excluded by the very key that names it, while a 720x1280 portrait stream
// counted as 1280p. The short edge is what a resolution label means everywhere
// else, and it reads the same in both orientations — 3840x2160 and 2160x3840
// are both 2160 here.
func CapDimension(width, height int) int {
	switch {
	case width <= 0 && height <= 0:
		return 0
	case width <= 0:
		return height
	case height <= 0:
		return width
	default:
		return min(width, height)
	}
}

// SelectByCap resolves a resolution cap against a candidate list and returns
// the CapDimension value the cap selects, plus whether anything was selected at
// all (false only for an empty list).
//
// The rule, in full (ruling R1):
//   - the LARGEST candidate size at or below capPx wins;
//   - if nothing is at or below it, the SMALLEST size above it wins — the cap
//     is a preference among the qualities on offer, never an exclusion that
//     leaves the job with nothing to download;
//   - capPx <= 0 means unbounded and always picks the largest size.
//
// It returns the chosen SIZE rather than an index on purpose. Several
// candidates routinely share a short edge — a 1920x1080 and a 2560x1080
// rendition, or an AV1 and an H.264 encode of the same frame — and R1 leaves
// that tie to the caller's own secondary ranking (fps, codec, bandwidth,
// source). Returning an index would have to break it here, arbitrarily and
// invisibly. Callers filter their own candidates to CapDimension == size (the
// "best available" path) or <= size (a path that must still honour a per-job
// quality preference) and rank what is left exactly as they did before.
func SelectByCap(capPx int, candidates []Cand) (int, bool) {
	bestBelow, haveBelow := 0, false
	bestAbove, haveAbove := 0, false
	for _, c := range candidates {
		d := CapDimension(c.Width, c.Height)
		if capPx <= 0 || d <= capPx {
			if !haveBelow || d > bestBelow {
				bestBelow, haveBelow = d, true
			}
			continue
		}
		if !haveAbove || d < bestAbove {
			bestAbove, haveAbove = d, true
		}
	}
	if haveBelow {
		return bestBelow, true
	}
	if haveAbove {
		return bestAbove, true
	}
	return 0, false
}
