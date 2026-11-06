package youtube

import (
	"cmp"
	"regexp"
	"strings"
)

// This file is the one ranking every YouTube selector shares below size and
// frame rate: the whole-file VOD selector (selectBestFormatsImpl), the live,
// post-live and DASH-manifest selector (the worker's SelectBestDashStream),
// the HLS variant selector (the worker's selectHlsVariant) and the format
// picker's [Best …] badges (cmd/moombox's formatsPayload). Owner decision D14:
// "rank like VOD" — before it the live paths ranked bandwidth alone, so at
// 1080p60 avc1 299 usually beat vp9 303 because its peak bitrate was higher,
// and the VOD and live paths of one video picked different codecs.
//
// The order is yt-dlp's YouTube sort, `quality, res, fps, hdr:12, source,
// vcodec, channels, acodec, lang` then the default `size, br`
// (extractor/youtube/_video.py:4204-4207 at b77c9304e, prepended to
// FormatSorter.default, utils/_utils.py:5363-5365). Size and frame rate are
// each caller's own (the cap, prefer_60fps, a quality preference); what
// follows them is here:
//
//   - video: dynamic range (VideoRank.Range, parse_codecs' rules), then the
//     Premium label (the `source` rung), then the codec, and the caller's
//     bitrate after that;
//   - audio: the TRACK first — Moombox's deliberate divergence, kept: yt-dlp
//     ranks `lang` after acodec, Moombox puts the original language ahead of
//     everything because archiving a dub is worse than archiving a lesser
//     encode — then the audioQuality ladder (`quality`), channels, codec, and
//     the caller's bitrate.

// DynamicRange is a video rendition's dynamic range as yt-dlp's parse_codecs
// derives it from the codecs string (utils/_utils.py:3048-3088): HDR10 for
// vp9 profile 2 and for AV1 whose fourth part (bit depth) is 10, DV for the
// Dolby Vision fourccs, and SDR for every other video (YoutubeDL.py fills
// `dynamic_range: SDR` in for a video format parse_codecs left unlabelled).
type DynamicRange int

const (
	RangeSDR DynamicRange = iota
	RangeHDR10
	RangeDV
)

func (r DynamicRange) String() string {
	switch r {
	case RangeHDR10:
		return "HDR10"
	case RangeDV:
		return "DV"
	default:
		return "SDR"
	}
}

// rangeScore orders the dynamic ranges, higher preferred. yt-dlp's hdr order
// is dv > hdr12 > hdr10+ > hdr10 > hlg > sdr, but YouTube's sort field is
// `hdr:12`, whose limit ranks anything above HDR12 — Dolby Vision — below
// every value the limit admits (utils/_utils.py:5378-5379, the limit applied
// in FormatSorter._calculate_field_preference): HDR10 > SDR > DV. Dolby
// Vision stays last either way; it is the one range most players cannot
// show.
//
// preferHDR is the `downloader.prefer_hdr` setting (owner decision D13: on by
// default, as upstream ranks; off for an operator whose players handle SDR
// only). Off, SDR ranks above HDR10. It moves only this rung: size and frame
// rate still come first, so an HDR rendition at a larger size or the
// preferred frame rate is still taken.
func rangeScore(r DynamicRange, preferHDR bool) int {
	switch r {
	case RangeDV:
		return 0
	case RangeHDR10:
		if preferHDR {
			return 2
		}
		return 1
	default:
		if preferHDR {
			return 1
		}
		return 2
	}
}

// codecParts is parse_codecs' normalisation of one codec: the fourcc
// lower-cased, every run of zeros that precedes a digit dropped
// (`re.sub(r'0+(?=\d)', ...)`), then split on dots — so
// "av01.0.12M.10" becomes [av1 0 12M 10] and "vp09.02.51.10" [vp9 2 51 10].
func codecParts(codec string) []string {
	codec = strings.TrimSpace(codec)
	if fourcc, rest, ok := strings.Cut(codec, "."); ok {
		codec = strings.ToLower(fourcc) + "." + rest
	} else {
		codec = strings.ToLower(codec)
	}
	var b strings.Builder
	for i := 0; i < len(codec); i++ {
		if codec[i] == '0' {
			j := i
			for j < len(codec) && codec[j] == '0' {
				j++
			}
			if j < len(codec) && codec[j] >= '0' && codec[j] <= '9' {
				i = j - 1
				continue
			}
		}
		b.WriteByte(codec[i])
	}
	return strings.Split(b.String(), ".")
}

// videoFourccs are parse_codecs' video fourccs (utils/_utils.py:3058-3059).
var videoFourccs = map[string]bool{
	"avc1": true, "avc2": true, "avc3": true, "avc4": true, "vp9": true, "vp8": true,
	"hev1": true, "hev2": true, "h263": true, "h264": true, "mp4v": true, "hvc1": true,
	"av1": true, "theora": true, "dvh1": true, "dvhe": true,
}

// VideoCodecOf is the first video codec of a codecs list — a mimeType's
// `codecs="…"` value, an MPD Representation's codecs attribute, or an HLS
// variant's CODECS, which on a pre-merged variant also names its audio
// ("avc1.4d401f,mp4a.40.2") — as parse_codecs takes its vcodec: the first
// entry whose fourcc is a video one, else "".
func VideoCodecOf(codecs string) string {
	for c := range strings.SplitSeq(codecs, ",") {
		c = strings.TrimSpace(c)
		if c != "" && videoFourccs[codecParts(c)[0]] {
			return c
		}
	}
	return ""
}

// dynamicRangeOf is parse_codecs' hdr verdict for one video codec
// (utils/_utils.py:3062-3068).
func dynamicRangeOf(codec string) DynamicRange {
	parts := codecParts(codec)
	switch {
	case parts[0] == "dvh1" || parts[0] == "dvhe":
		return RangeDV
	case parts[0] == "av1" && len(parts) > 3 && parts[3] == "10":
		return RangeHDR10
	case parts[0] == "vp9" && len(parts) > 1 && parts[1] == "2":
		return RangeHDR10
	}
	return RangeSDR
}

// mimeCodecsRegex reads a mimeType's whole codecs list.
var mimeCodecsRegex = regexp.MustCompile(`codecs="([^"]+)"`)

// CodecsOf is a mimeType's codecs list — `video/mp4; codecs="avc1.42001E,
// mp4a.40.2"` gives "avc1.42001E, mp4a.40.2" — or "" when it names none. The
// worker fills a manifest-free stream's DashStreamInfo.Codecs with it.
func CodecsOf(mimeType string) string {
	if m := mimeCodecsRegex.FindStringSubmatch(mimeType); m != nil {
		return m[1]
	}
	return extractCodec(mimeType)
}

// VideoRank is the part of a video rendition's rank that every YouTube
// selector applies after size and frame rate, and ahead of bitrate.
type VideoRank struct {
	// Range is the rendition's dynamic range (dynamicRangeOf).
	Range DynamicRange
	// Premium marks an enhanced-bitrate rendition: yt-dlp's
	// `source_preference` +100 for a qualityLabel naming "Premium"
	// (_video.py:3444) and for the HLS itags 616 and 235 (3666-3668).
	// `source` sorts after hdr and before vcodec, so a Premium VP9 1080p
	// (356) beats a standard AV1 1080p (399).
	Premium bool
	// Codec is the videoCodecPatterns score of the rendition's codec.
	Codec int
}

// VideoRankOf ranks a rendition from its codecs list (VideoCodecOf takes the
// video one) and whether it is a Premium rendition.
func VideoRankOf(codecs string, premium bool) VideoRank {
	codec := VideoCodecOf(codecs)
	if codec == "" {
		// A codecs value parse_codecs does not know, kept as the first entry:
		// it scores whatever the patterns give it, and is SDR.
		codec, _, _ = strings.Cut(codecs, ",")
		codec = strings.TrimSpace(codec)
		return VideoRank{Codec: scoreVideoCodec(codec)}
	}
	return VideoRank{Range: dynamicRangeOf(codec), Premium: premium, Codec: scoreVideoCodec(codec)}
}

// CompareVideoRank orders two ranks: positive when a ranks above b, negative
// when below, 0 when they tie (and the caller's bitrate decides). Dynamic
// range, then Premium, then codec.
func CompareVideoRank(a, b VideoRank, preferHDR bool) int {
	if c := cmp.Compare(rangeScore(a.Range, preferHDR), rangeScore(b.Range, preferHDR)); c != 0 {
		return c
	}
	if a.Premium != b.Premium {
		if a.Premium {
			return 1
		}
		return -1
	}
	return cmp.Compare(a.Codec, b.Codec)
}

// IsPremium reports whether the format is a Premium enhanced-bitrate
// rendition: its qualityLabel names "Premium", the one test yt-dlp makes on
// an https format (`100 if 'Premium' in name`, _video.py:3444). YouTube
// serves such a format's URL only to an account entitled to it.
func (f *Format) IsPremium() bool {
	return strings.Contains(f.QualityLabel, "Premium")
}

// videoRank is the format's VideoRank.
func (f *Format) videoRank() VideoRank {
	return VideoRankOf(extractCodec(f.MimeType), f.IsPremium())
}

// audioQualityLadder is yt-dlp's `quality` ladder for audio
// (_video.py:3238-3243): its qualities() list puts tiny, then
// audio_quality_ultralow, low, medium and high, and an audio format's
// quality is its audioQuality when its own quality is tiny or absent
// (3401-3403). The index is the rung: a format that states no audioQuality
// is "tiny", 0, below every one that does — as upstream ranks the tv
// client's audio formats, which state none (the dedup fills one in from
// another client's copy of the same stream, fillStreamFacts, as upstream's
// itag_qualities does for manifest formats).
var audioQualityLadder = map[string]int{
	"AUDIO_QUALITY_ULTRALOW": 1,
	"AUDIO_QUALITY_LOW":      2,
	"AUDIO_QUALITY_MEDIUM":   3,
	"AUDIO_QUALITY_HIGH":     4,
}

// AudioQualityRung is audioQuality's place on audioQualityLadder.
func AudioQualityRung(audioQuality string) int {
	return audioQualityLadder[strings.ToUpper(audioQuality)]
}

// AudioRank is an audio rendition's rank ahead of bitrate, rung by rung:
// track, quality, channels, codec.
type AudioRank struct {
	// Track is the track's identity score (audioTrackScore, or
	// LanguageTrackScore for a manifest rendition known only by language).
	Track int
	// Quality is the audioQuality rung (AudioQualityRung), 0 unstated.
	Quality int
	// Channels is audioChannels, 0 unstated (yt-dlp's `channels`, 3464).
	Channels int
	// Codec is the audioCodecPatterns score.
	Codec int
}

// CompareAudioRank orders two ranks: positive when a ranks above b, 0 on a
// tie (bitrate decides).
func CompareAudioRank(a, b AudioRank) int {
	if c := cmp.Compare(a.Track, b.Track); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Quality, b.Quality); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Channels, b.Channels); c != 0 {
		return c
	}
	return cmp.Compare(a.Codec, b.Codec)
}

// AudioRankOf is the format's AudioRank.
func AudioRankOf(f *Format) AudioRank {
	return AudioRank{
		Track:    audioTrackScore(f),
		Quality:  AudioQualityRung(f.AudioQuality),
		Channels: f.AudioChannels,
		Codec:    ScoreAudioCodec(extractCodec(f.MimeType)),
	}
}

// ScoreAudioCodec is the audioCodecPatterns score of a codecs list's first
// entry.
func ScoreAudioCodec(codecs string) int {
	codec, _, _ := strings.Cut(codecs, ",")
	return scoreAudioCodec(strings.TrimSpace(codec))
}

// AudioLanguages is one extraction's language_map (_video.py:3230-3234): the
// language of the track YouTube labels original, and of the one it marks
// default for this viewer — each the first an entry of the extraction
// stated. Recorded from every format entry, the ones with no URL included,
// as yt-dlp does since #14883 (_video.py:3528-3531): a client YouTube has
// forced onto SABR lists its formats with their URLs stripped, and they are
// then the only record of which track is the original that a manifest's
// renditions — known only by language — can be ranked against.
type AudioLanguages struct {
	Original string `json:"original,omitempty"`
	Default  string `json:"default,omitempty"`
}

// audioTrackLanguage is get_language_code_and_preference's language code:
// audioTrack.id up to its first dot ("en.4" → "en").
func audioTrackLanguage(trackID string) string {
	lang, _, _ := strings.Cut(trackID, ".")
	return lang
}

// note records one entry's track into the map, first statement winning, as
// get_language_code_and_preference fills language_map (_video.py:3283-3290):
// a descriptive track is never the original or the default.
func (l *AudioLanguages) note(trackID, displayName string, isDefault bool) {
	lang := audioTrackLanguage(trackID)
	name := strings.ToLower(displayName)
	switch {
	case lang == "" || strings.Contains(name, "descriptive"):
	case strings.Contains(name, "original"):
		if l.Original == "" {
			l.Original = lang
		}
	case isDefault:
		if l.Default == "" {
			l.Default = lang
		}
	}
}

// merge fills each language l has not stated from o.
func (l *AudioLanguages) merge(o AudioLanguages) {
	if l.Original == "" {
		l.Original = o.Original
	}
	if l.Default == "" {
		l.Default = o.Default
	}
}

// LanguageTrackScore ranks a rendition known only by its language — an HLS
// variant's YT-EXT-AUDIO-CONTENT-ID, a DASH manifest Representation's lang —
// as process_manifest_format does (_video.py:3655-3663): the original
// language at ORIGINAL_LANG_VALUE, the default at DEFAULT_LANG_VALUE, every
// other at upstream's -1, and a rendition that states no language at -1 too.
// On audioTrackScore's doubled scale, so the two compare.
func LanguageTrackScore(lang string, langs AudioLanguages) int {
	lang = audioTrackLanguage(lang)
	switch {
	case lang != "" && lang == langs.Original:
		return 20
	case lang != "" && lang == langs.Default:
		return 10
	}
	return -2
}
