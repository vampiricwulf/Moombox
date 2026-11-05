package youtube

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

var codecRegex = regexp.MustCompile(`codecs="?([^",]+)`)

// Video codec priority patterns (higher score = more preferred). Mirrors
// yt-dlp's default FormatSort vcodec order (utils/_utils.py): av01 > vp9.2 >
// vp9 > h265/hevc > h264/avc > vp8. Matching yt-dlp means AV1 — including the
// Premium enhanced-bitrate itags like 721 — is chosen over vp9 at the same
// resolution/fps. Patterns are tested in order, so the more-specific vp9.2
// pattern must precede vp9. Codec strings are the extracted `codecs=` value
// (e.g. "av01.0.09M.08", "vp9", "vp9.2", "avc1.640028").
var videoCodecPatterns = []struct {
	pattern *regexp.Regexp
	score   int
}{
	{regexp.MustCompile(`^av0?1`), 6},
	{regexp.MustCompile(`^vp0?9\.0?2`), 5},
	{regexp.MustCompile(`^vp0?9`), 4},
	{regexp.MustCompile(`^([hx]265|he?vc?)`), 3},
	{regexp.MustCompile(`^([hx]264|avc)`), 2},
	{regexp.MustCompile(`^vp0?8`), 1},
}

// Audio codec priority patterns. The two AAC profiles YouTube serves share a
// score, as they share yt-dlp's "mp4a" rank, so bitrate decides between them:
// HE-AAC (mp4a.40.5, itag 139) scored above AAC-LC (mp4a.40.2, itag 140) and,
// codec outranking bitrate, a pool with no Opus took the 48 kbps track over
// the 128 kbps one.
//
// Below AAC, yt-dlp's acodec order goes on `ac-?4`, `e-?a?c-?3`, `ac-?3`
// (utils/_utils.py:5375-5376): the surround codecs of the 5.1 renditions
// (328 E-AC-3, 380 AC-3). They scored 0 here, last; they still rank below
// Opus and AAC on this rung, and win only through the channels rung ahead of
// it (AudioRank) — a 5.1 track over a stereo one, as upstream ranks. Both
// stream-copy into the MP4 archive beside H.264, VP9 and AV1 video
// (owner decision D15's check: TestSurroundAudioStreamCopiesIntoMP4,
// internal/engine).
var audioCodecPatterns = []struct {
	pattern *regexp.Regexp
	score   int
}{
	{regexp.MustCompile(`^opus`), 6},
	{regexp.MustCompile(`^mp4a\.40\.[25]`), 5},
	{regexp.MustCompile(`^mp4a`), 4},
	{regexp.MustCompile(`^ac-?4`), 3},
	{regexp.MustCompile(`^e-?a?c-?3`), 2},
	{regexp.MustCompile(`^ac-?3`), 1},
}

// SelectedFormats contains the best video and audio formats.
type SelectedFormats struct {
	Video *Format
	Audio *Format
}

// SelectBestFormats selects the best video and audio formats based on configuration.
// Use SelectBestFormatsWithLogger to get debug logging of format selection.
// preferHDR is downloader.prefer_hdr (rangeScore).
func SelectBestFormats(formats []Format, maxResolution int, prefer60fps, preferHDR bool) SelectedFormats {
	return selectBestFormatsImpl(formats, maxResolution, prefer60fps, preferHDR, nil)
}

// SelectBestFormatsWithLogger selects formats with debug logging (matching TS formatSelector).
func SelectBestFormatsWithLogger(formats []Format, maxResolution int, prefer60fps, preferHDR bool, logger interface {
	Debug(msg string, args ...any)
}) SelectedFormats {
	return selectBestFormatsImpl(formats, maxResolution, prefer60fps, preferHDR, logger)
}

// CompareVideoFormats orders two video renditions as the selector ranks
// them once the cap has chosen a size: the long edge, then the frame rate
// prefer60fps asks for (higher with it, lower without; an unstated rate reads
// as 30), then the shared VideoRank (dynamic range under preferHDR, Premium,
// codec), then the higher bitrate, then the lower auth level. Positive when a
// ranks above b. The format picker's [Best …] badges rank with it too, so a
// badge names the rendition automatic selection would take of its kind.
func CompareVideoFormats(a, b *Format, prefer60fps, preferHDR bool) int {
	if c := cmp.Compare(a.MaxDimension(), b.MaxDimension()); c != 0 {
		return c
	}
	aFps, bFps := 30, 30
	if a.Fps != nil {
		aFps = *a.Fps
	}
	if b.Fps != nil {
		bFps = *b.Fps
	}
	if aFps != bFps {
		if prefer60fps {
			return cmp.Compare(aFps, bFps)
		}
		return cmp.Compare(bFps, aFps)
	}
	if c := CompareVideoRank(a.videoRank(), b.videoRank(), preferHDR); c != 0 {
		return c
	}
	// Higher bitrate wins: yt-dlp's size/br sort, which takes the
	// higher-quality stream at the same res/fps/range/codec (e.g. AV1 Premium
	// 721 over the base AV1 399 where both are labelled alike).
	if c := cmp.Compare(a.Bitrate, b.Bitrate); c != 0 {
		return c
	}
	return cmp.Compare(authLevelOf(b), authLevelOf(a))
}

// CompareAudioFormats orders two audio renditions as the selector ranks
// them: the shared AudioRank (track, audioQuality, channels, codec), then
// the higher bitrate, then the lower auth level. Positive when a ranks above
// b.
func CompareAudioFormats(a, b *Format) int {
	if c := CompareAudioRank(AudioRankOf(a), AudioRankOf(b)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Bitrate, b.Bitrate); c != 0 {
		return c
	}
	return cmp.Compare(authLevelOf(b), authLevelOf(a))
}

// isSelectableVideo reports whether a format is a video rendition the selector
// can actually download. It is the single definition the cap pre-pass and the
// selection loop below both use: two copies of this test could drift, and the
// symptom would be a cap resolved over a different set than the one the gate
// filters — silent, and wrong only for some playlists.
func isSelectableVideo(f *Format) bool {
	return strings.Contains(f.MimeType, "video") && f.URL != ""
}

// isSelectableAudio is isSelectableVideo's audio half, classified as the
// selection loop classifies: a muxed format's mimeType names video, so it is
// video.
func isSelectableAudio(f *Format) bool {
	return !strings.Contains(f.MimeType, "video") && strings.Contains(f.MimeType, "audio") && f.URL != ""
}

// intactRenditions reports whether formats holds a selectable video and a
// selectable audio rendition that is not Damaged — whether the selector's
// first rung has anything to put ahead of a damaged one.
func intactRenditions(formats []Format) (video, audio bool) {
	for i := range formats {
		f := &formats[i]
		if f.Damaged {
			continue
		}
		video = video || isSelectableVideo(f)
		audio = audio || isSelectableAudio(f)
	}
	return video, audio
}

// audioTrackScore ranks one audio format's TRACK against the others. Ported
// from yt-dlp's get_language_code_and_preference (_video.py:3276-3291 at
// b77c9304e), whose constants are ORIGINAL_LANG_VALUE = 10 and
// DEFAULT_LANG_VALUE = 5, with 'descriptive' at -10 and everything else at
// -1.
//
// Where it ranks is Moombox's own: the track is the FIRST audio rung here
// (AudioRank), ahead of quality, channels and codec, because archiving the
// wrong language is worse than archiving a lesser encode. yt-dlp's YouTube
// sort puts `lang` much later — `quality, res, fps, hdr:12, source, vcodec,
// channels, acodec, lang` (_video.py:4204-4207) — so upstream can take a
// dub's HIGH rendition over the original's MEDIUM one; this port keeps the
// track first by decision (the deliberate divergence the audit kept).
//
// The DRC penalty is upstream's, but it sits on a different rung there:
// yt-dlp folds it into `quality` (`q(quality) - bool(isDrc) / 2`,
// _video.py:3466), the first rung of that sort. Here it is a TIE-BREAK inside
// a track — doubled rungs and a -1 — never across tracks: a
// loudness-normalised rendition is a processed copy of the same audio, so
// given both we archive the untouched one. The dedup's collapse removes a DRC
// twin wherever the clean rendition of its itag exists, so in practice the
// two encodings agree.
func audioTrackScore(f *Format) int {
	name := strings.ToLower(f.AudioTrackName)
	score := -1
	switch {
	case strings.Contains(name, "descriptive"):
		score = -10
	case strings.Contains(name, "original"):
		score = 10
	case f.AudioIsDefault:
		score = 5
	}
	// ×2 so the DRC penalty can never move a format across a track boundary.
	score *= 2
	if f.IsDrc {
		score--
	}
	return score
}

func selectBestFormatsImpl(formats []Format, maxResolution int, prefer60fps, preferHDR bool, logger interface {
	Debug(msg string, args ...any)
}) SelectedFormats {
	var bestVideo, bestAudio *Format

	// The first rung, ahead of every other — the cap included: a Damaged
	// rendition (markDamagedFormats) is taken only when no intact one of its
	// kind exists. It is yt-dlp's `'preference': -10 if is_damaged`
	// (_video.py:3474, 0ad92dfb1, #2823). ie_pref, the sort field that reads
	// a format's preference, is a PRIORITY field, ranked ahead of everything
	// a user's format sort asks for, resolution included (utils/_utils.py:5392,
	// 5532-5536) — and the cap is such a sort: `-S res:N`, the closest size
	// above when nothing is at or below. So a short top AV1 loses to the
	// intact VP9 beside it, and a pool whose only rendition at the chosen size
	// is short resolves the cap over the intact ones. Passing over it, rather
	// than ranking it, leaves the winner among the intact formats exactly as
	// before. Without it a Finished archive's video track stopped partway,
	// and the post-mux check, which compares the output against the staged
	// inputs (verifyAgainstStaging), had nothing longer to hold it to. The
	// dedup's per-itag collapse ahead of this (collapseToPreferredRendition)
	// ranks an intact track first for the same reason, so the intact dub of a
	// video whose every original-language copy is short is still here to win.
	intactVideo, intactAudio := intactRenditions(formats)

	// Log available video formats (matching TS FormatSelector debug output)
	if logger != nil {
		var available, passedOver []string
		for i := range formats {
			f := &formats[i]
			if strings.Contains(f.MimeType, "video") && f.URL != "" {
				w, h, fps := 0, 0, 0
				if f.Width != nil {
					w = *f.Width
				}
				if f.Height != nil {
					h = *f.Height
				}
				if f.Fps != nil {
					fps = *f.Fps
				}
				available = append(available, fmt.Sprintf("itag %d (%dx%d@%dfps, %dbps)", f.Itag, w, h, fps, f.Bitrate))
			}
			if f.Damaged && (isSelectableVideo(f) && intactVideo || isSelectableAudio(f) && intactAudio) {
				passedOver = append(passedOver, fmt.Sprintf("itag %d", f.Itag))
			}
		}
		if len(available) > 0 {
			logger.Debug(fmt.Sprintf("[FormatSelector] Available video formats with URLs: %s", strings.Join(available, ", ")))
		}
		if len(passedOver) > 0 {
			logger.Debug(fmt.Sprintf("[FormatSelector] Passing over damaged renditions (under half the video's length): %s", strings.Join(passedOver, ", ")))
		}
	}

	// Resolve the resolution cap ONCE, over the video formats that are
	// actually downloadable (ruling R1). The cap compares the SHORT edge, the
	// largest size at or below it wins, and when nothing is at or below it the
	// closest size ABOVE it is chosen — the cap is a preference among the
	// qualities YouTube offered, never a filter that can leave a job with no
	// video at all. `maxResolution == 0` is unbounded.
	videoCands := make([]utils.Cand, 0, len(formats))
	for i := range formats {
		f := &formats[i]
		if !isSelectableVideo(f) || f.Damaged && intactVideo {
			continue
		}
		w, h := f.dims()
		videoCands = append(videoCands, utils.Cand{Width: w, Height: h})
	}
	capSize, haveCapSize := utils.SelectByCap(maxResolution, videoCands)

	for i := range formats {
		f := &formats[i]
		mimeType := f.MimeType

		if strings.Contains(mimeType, "video") {
			if !isSelectableVideo(f) || f.Damaged && intactVideo || !haveCapSize || f.CapDimension() != capSize {
				continue
			}

			// Within the chosen size: long edge, frame rate, dynamic range,
			// Premium, codec, bitrate, auth level (CompareVideoFormats).
			if bestVideo == nil || CompareVideoFormats(f, bestVideo, prefer60fps, preferHDR) > 0 {
				bestVideo = f
			}
		} else if strings.Contains(mimeType, "audio") {
			if f.URL == "" || f.Damaged && intactAudio {
				continue
			}
			// Track identity first: archiving the wrong LANGUAGE is worse than
			// archiving a lesser encoder, so this outranks every other rung.
			// Then yt-dlp's audio order: the audioQuality ladder, channels,
			// codec, bitrate (CompareAudioFormats). Before it codec outranked
			// quality and channels, so a 5.1 track lost to stereo Opus,
			// Premium 141 (HIGH) lost to 251 (MEDIUM), and with 251 gone 250
			// (Opus, LOW, ~70 kbps) beat 140 (AAC, MEDIUM, 128 kbps).
			if bestAudio == nil || CompareAudioFormats(f, bestAudio) > 0 {
				bestAudio = f
			}
		}
	}

	if logger != nil {
		if bestVideo != nil {
			w, h := 0, 0
			if bestVideo.Width != nil {
				w = *bestVideo.Width
			}
			if bestVideo.Height != nil {
				h = *bestVideo.Height
			}
			rank := bestVideo.videoRank()
			logger.Debug(fmt.Sprintf("[FormatSelector] best video: itag=%d %s %s%s %dx%d %dbps (source=%s)",
				bestVideo.Itag, extractCodec(bestVideo.MimeType), rank.Range, premiumTag(rank.Premium),
				w, h,
				bestVideo.Bitrate, bestVideo.Source))
		} else {
			logger.Debug("[FormatSelector] no suitable video format found")
		}
		if bestAudio != nil {
			logger.Debug(fmt.Sprintf("[FormatSelector] best audio: itag=%d %s %s %dch %dbps (source=%s)",
				bestAudio.Itag, extractCodec(bestAudio.MimeType), bestAudio.AudioQuality, bestAudio.AudioChannels,
				bestAudio.Bitrate, bestAudio.Source))
		} else {
			logger.Debug("[FormatSelector] no suitable audio format found")
		}
	}

	return SelectedFormats{Video: bestVideo, Audio: bestAudio}
}

// SelectWithOptions selects formats with optional manual itag override.
func SelectWithOptions(formats []Format, maxResolution int, prefer60fps, preferHDR bool, videoItag, audioItag *int) SelectedFormats {
	var video, audio *Format

	// Manual video selection
	if videoItag != nil && *videoItag != -1 {
		for i := range formats {
			if formats[i].Itag == *videoItag && strings.Contains(formats[i].MimeType, "video") && formats[i].URL != "" {
				video = &formats[i]
				break
			}
		}
	}

	// Manual audio selection
	if audioItag != nil && *audioItag != -1 {
		for i := range formats {
			if formats[i].Itag == *audioItag && strings.Contains(formats[i].MimeType, "audio") && formats[i].URL != "" {
				audio = &formats[i]
				break
			}
		}
	}

	// Explicit skip
	skipVideo := videoItag != nil && *videoItag == -1
	skipAudio := audioItag != nil && *audioItag == -1

	// Auto-select for unresolved
	auto := SelectBestFormats(formats, maxResolution, prefer60fps, preferHDR)
	if video == nil && !skipVideo {
		video = auto.Video
	}
	if audio == nil && !skipAudio {
		audio = auto.Audio
	}

	return SelectedFormats{Video: video, Audio: audio}
}

func extractCodec(mimeType string) string {
	m := codecRegex.FindStringSubmatch(mimeType)
	if m == nil {
		return ""
	}
	return m[1]
}

func scoreVideoCodec(codec string) int {
	for _, p := range videoCodecPatterns {
		if p.pattern.MatchString(codec) {
			return p.score
		}
	}
	return 0
}

func scoreAudioCodec(codec string) int {
	for _, p := range audioCodecPatterns {
		if p.pattern.MatchString(codec) {
			return p.score
		}
	}
	return 0
}

// premiumTag is the selector log's mark of a Premium rendition.
func premiumTag(premium bool) string {
	if premium {
		return " Premium"
	}
	return ""
}

func authLevelOf(f *Format) int {
	if f.AuthLevel != nil {
		return *f.AuthLevel
	}
	return 999
}
