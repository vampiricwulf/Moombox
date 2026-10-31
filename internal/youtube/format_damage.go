package youtube

import (
	"context"
)

// videoDurationSeconds is the length markDamagedFormats holds each format
// against: the response's videoDetails.lengthSeconds, else its microformat's
// — yt-dlp's `duration` chain (_video.py:4036-4038), whose
// `int_or_none(...) or ...` also passes a "0" on to the next source. Upstream
// takes the first response's value across the extraction (get_first); this is
// the parsed response's own, which is the one its formats were cut from. Zero
// when neither states one, as a live or upcoming broadcast's do not.
func videoDurationSeconds(videoDetails, microformat map[string]any) int {
	if n := broadcastLength(videoDetails); n > 0 {
		return n
	}
	return broadcastLength(microformat)
}

// markDamagedFormats stamps Format.Damaged on every format serving under half
// of durationSeconds, and returns their itags in pool order. Ported from
// yt-dlp's is_damaged (_video.py:3430-3438, the test on 3435):
// `format_duration < duration // 2`, a format's approxDurationMs in seconds
// against the video's length halved in WHOLE seconds — so a 601 s video's
// line is 300 s, not 300.5 s.
// The rule is upstream's second: 0ad92dfb1 (#2823) held each format against
// the longest one less 10 s, and a25a42432 moved it to half the duration,
// because small differences between renditions are ordinary (upstream names
// __2ABJjxzNo and ySuUZEjARPY) and the case it exists for, 2-nOtRESiUc, is a
// rendition encoded far short.
//
// A format that states no approxDurationMs is never damaged (upstream's
// try_call turns the None comparison into None), and neither is anything when
// the length is unknown — its half is 0, and no length is under that. A
// stated 0 is damaged, as upstream's 0.0 is.
func markDamagedFormats(formats []Format, durationSeconds int) []int {
	half := int64(durationSeconds/2) * 1000
	var itags []int
	for i := range formats {
		f := &formats[i]
		if f.ApproxDurationMs != nil && *f.ApproxDurationMs < half {
			f.Damaged = true
			itags = append(itags, f.Itag)
		}
	}
	return itags
}

// firstDamagedReport is firstDRMReport for the damaged-rendition line.
func (s *extractionState) firstDamagedReport() bool {
	return s == nil || s.damagedWarned.CompareAndSwap(false, true)
}

// reportDamagedFormats tells the operator a response carried damaged
// renditions. Upstream reports it with `report_warning(f'Some {client_name}
// client {proto} formats are possibly damaged. They will be deprioritized',
// only_once=True)` (_video.py:3436-3438), and a Warn is what it earns here:
// the archive is taken from another rendition than the best one listed, and
// the line is the only place that says why. Once per extraction, as the DRM
// report is (firstDRMReport), naming the client that first carried them — the
// encode is the video's, so every client serving it says the same. A probe
// records it at Debug, for the DRM report's reason: the probes run every 30 s
// for a fact the extraction already reported.
func (p *PlayerAPI) reportDamagedFormats(ctx context.Context, itags []int) {
	scope := playerCallScopeFrom(ctx)
	const note = "approxDurationMs under half the video's length — encoded short, so its track would stop partway (yt-dlp #2823)"
	switch {
	case scope.probeOnly:
		p.logger.Debug("[PlayerApi] some formats are possibly damaged (probe)",
			"client", scope.clientName(), "itags", itags, "note", note)
	case extractionStateFrom(ctx).firstDamagedReport():
		p.logger.Warn("[PlayerApi] some formats are possibly damaged; they will be deprioritized",
			"client", scope.clientName(), "itags", itags, "note", note)
	}
}
