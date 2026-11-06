package youtube

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// rankVideo is one video rendition of a real itag for the ranking tables:
// mp4 for avc1/av01, webm for vp9, at w x h and fps, its qualityLabel naming
// "Premium" when premium is set.
func rankVideo(itag int, codecs string, w, h, fps, bitrate int, premium bool) Format {
	container := "mp4"
	if codecs[:2] == "vp" {
		container = "webm"
	}
	label := fmt.Sprintf("%dp%d", h, fps)
	if premium {
		label = fmt.Sprintf("%dp Premium", h)
	}
	return Format{
		Itag: itag, URL: fmt.Sprintf("https://x/%d", itag),
		MimeType: fmt.Sprintf(`video/%s; codecs="%s"`, container, codecs),
		Bitrate:  bitrate, Width: &w, Height: &h, Fps: &fps, QualityLabel: label,
	}
}

// rankAudio is one audio rendition of a real itag: its codecs, the
// audioQuality it is listed with ("" for none), channels and bitrate.
func rankAudio(itag int, codecs, quality string, channels, bitrate int) Format {
	container := "mp4"
	if codecs == "opus" {
		container = "webm"
	}
	return Format{
		Itag: itag, URL: fmt.Sprintf("https://x/%d", itag),
		MimeType:     fmt.Sprintf(`audio/%s; codecs="%s"`, container, codecs),
		AudioQuality: quality, AudioChannels: channels, Bitrate: bitrate,
	}
}

// TestDynamicRangeFollowsParseCodecs pins parse_codecs' hdr verdict
// (utils/_utils.py:3048-3088) on the codec strings YouTube serves: vp9
// profile 2 and 10-bit AV1 are HDR10, the Dolby Vision fourccs DV, and every
// other video SDR — 8-bit AV1 and vp9 profile 0 included.
//
// Mutants (each run): the zero-stripping removed (vp09.02 reads profile "02",
// SDR; av01's "10" part unaffected but "08" unchanged too — the vp9.2 rows
// fail); AV1 read by its third part (12M) instead of the fourth (the 10-bit
// row fails); dvh1/dvhe dropped (DV rows read SDR).
func TestDynamicRangeFollowsParseCodecs(t *testing.T) {
	for _, tc := range []struct {
		codecs string
		want   DynamicRange
	}{
		{"vp09.02.51.10.01.09.16.09.00", RangeHDR10}, // 330-337
		{"vp9.2", RangeHDR10},
		{"av01.0.13M.10.0.110.09.16.09.0", RangeHDR10}, // 694-702
		{"av01.0.09M.08", RangeSDR},                    // 394-401
		{"vp9", RangeSDR},
		{"vp09.00.51.08", RangeSDR},
		{"avc1.640028", RangeSDR},
		{"dvh1.05.06", RangeDV},
		{"dvhe.08.07", RangeDV},
		{"avc1.4d401f,mp4a.40.2", RangeSDR},
		{"mp4a.40.2,vp09.02.51.10", RangeHDR10}, // the first VIDEO codec decides
	} {
		if got := VideoRankOf(tc.codecs, false).Range; got != tc.want {
			t.Errorf("range of %q = %s, want %s", tc.codecs, got, tc.want)
		}
	}
}

// TestVideoRankingMatchesYtDlp is the video half of G37-G41's tables, real
// itag sets at one size and frame rate, with prefer_60fps on:
//
//   - 330-337 vs 394-401: VP9.2 HDR beside AV1 SDR. yt-dlp's `hdr:12` rung,
//     ahead of vcodec, takes the HDR; the codec rung alone took the AV1
//     (G39). With prefer_hdr off (owner decision D13) the SDR AV1 wins.
//   - AV1 HDR (701) out-peaked by AV1 SDR (399): bitrate alone took the SDR.
//   - Dolby Vision ranks below SDR, as `hdr:12`'s limit does.
//   - 356 vs 399: the Premium VP9 beats the standard AV1 — `source` sorts
//     before vcodec (G40) — and the codec decides again between two
//     non-Premium renditions.
//
// Mutants (each run): the range rung removed from CompareVideoRank (the HDR
// rows take the AV1 SDR); rangeScore ignoring preferHDR (the prefer_hdr=false
// rows take the HDR); DV scored as HDR10 (the DV row takes the dvh1); the
// Premium rung removed or placed after the codec (356 loses to 399).
func TestVideoRankingMatchesYtDlp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		formats   []Format
		preferHDR bool
		want      int
	}{
		{"VP9.2 HDR over AV1 SDR at 2160p60", []Format{
			rankVideo(401, "av01.0.17M.08", 3840, 2160, 60, 26_000_000, false),
			rankVideo(337, "vp09.02.51.10.01.09.16.09.00", 3840, 2160, 60, 30_000_000, false),
			rankVideo(315, "vp9", 3840, 2160, 60, 28_000_000, false),
		}, true, 337},
		{"VP9.2 HDR over AV1 SDR at 1080p60, a lower bitrate", []Format{
			rankVideo(335, "vp09.02.41.10.01.09.16.09.00", 1920, 1080, 60, 3_000_000, false),
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 60, 4_000_000, false),
		}, true, 335},
		{"prefer_hdr off keeps the AV1 SDR", []Format{
			rankVideo(335, "vp09.02.41.10.01.09.16.09.00", 1920, 1080, 60, 3_000_000, false),
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 60, 4_000_000, false),
		}, false, 399},
		{"AV1 HDR out-peaked by AV1 SDR", []Format{
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 60, 5_000_000, false),
			rankVideo(699, "av01.0.09M.10.0.110.09.16.09.0", 1920, 1080, 60, 4_000_000, false),
		}, true, 699},
		{"prefer_hdr off, AV1 HDR out-peaking AV1 SDR", []Format{
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 60, 4_000_000, false),
			rankVideo(699, "av01.0.09M.10.0.110.09.16.09.0", 1920, 1080, 60, 5_000_000, false),
		}, false, 399},
		{"Dolby Vision below SDR", []Format{
			rankVideo(9001, "dvh1.05.06", 1920, 1080, 60, 9_000_000, false),
			rankVideo(299, "avc1.64002a", 1920, 1080, 60, 4_000_000, false),
		}, true, 299},
		{"Premium VP9 356 over AV1 399", []Format{
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 30, 2_000_000, false),
			rankVideo(356, "vp9", 1920, 1080, 30, 3_000_000, true),
			rankVideo(248, "vp9", 1920, 1080, 30, 2_500_000, false),
		}, true, 356},
		{"without the Premium label the codec decides", []Format{
			rankVideo(399, "av01.0.09M.08", 1920, 1080, 30, 2_000_000, false),
			rankVideo(248, "vp9", 1920, 1080, 30, 2_500_000, false),
		}, true, 399},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectBestFormats(tc.formats, 2160, true, tc.preferHDR)
			if got.Video == nil || got.Video.Itag != tc.want {
				t.Errorf("video = %v, want %d", itagOf(got.Video), tc.want)
			}
		})
	}
}

// TestHDRNeverOutranksSizeOrFrameRate: the dynamic-range rung sits after
// size and frame rate (D13: "at equal size and fps"), so a larger SDR, or an
// SDR at the frame rate prefer_60fps asks for, still wins.
//
// Mutant: CompareVideoRank moved ahead of the fps rung in CompareVideoFormats
// (the 30 fps HDR beats the 60 fps SDR).
func TestHDRNeverOutranksSizeOrFrameRate(t *testing.T) {
	sdr60 := rankVideo(399, "av01.0.09M.08", 1920, 1080, 60, 4_000_000, false)
	hdr30 := rankVideo(335, "vp09.02.41.10.01.09.16.09.00", 1920, 1080, 30, 3_000_000, false)
	if got := SelectBestFormats([]Format{hdr30, sdr60}, 2160, true, true); got.Video == nil || got.Video.Itag != 399 {
		t.Errorf("video = %v, want the 60 fps SDR 399", itagOf(got.Video))
	}
	sdr4k := rankVideo(401, "av01.0.17M.08", 3840, 2160, 30, 20_000_000, false)
	if got := SelectBestFormats([]Format{hdr30, sdr4k}, 2160, true, true); got.Video == nil || got.Video.Itag != 401 {
		t.Errorf("video = %v, want the 2160p SDR 401", itagOf(got.Video))
	}
}

// TestAudioRankingMatchesYtDlp is G38's table: after the track, yt-dlp
// ranks audio by the audioQuality ladder, then channels, then codec
// (_video.py:3238-3243, 3464; sort 4204-4207). Moombox ranked codec first.
//
//   - 140 vs 250 (251 gone, as a re-selection without it leaves the pool):
//     AAC MEDIUM 128 kbps over Opus LOW ~70 kbps.
//   - 256/258/328/380 vs 251: each 5.1 rendition over stereo Opus at the same
//     quality — and, all four together, AAC 258 (the higher bitrate of the
//     two AAC 5.1) over E-AC-3 and AC-3, the codec rung below channels.
//   - 141 vs 251: the Premium HIGH AAC over MEDIUM Opus.
//   - 141 vs 256: yt-dlp's `quality` sorts ahead of `channels`, so stereo
//     HIGH beats 5.1 MEDIUM.
//   - The track still comes first (the deliberate divergence): a 5.1 dub
//     loses to the stereo original.
//
// Mutants (each run): the quality rung removed from CompareAudioRank (250
// beats 140, 251 beats 141); the quality and channels rungs swapped (256
// beats 141); the channels rung removed (251 beats every 5.1
// row); codec ahead of quality (250 beats 140); the ec-3/ac-3 patterns
// dropped (pinned by TestScoreAudioCodec instead); the track rung dropped
// (the dub wins).
func TestAudioRankingMatchesYtDlp(t *testing.T) {
	const medium, low, high = "AUDIO_QUALITY_MEDIUM", "AUDIO_QUALITY_LOW", "AUDIO_QUALITY_HIGH"
	opus251 := rankAudio(251, "opus", medium, 2, 160_000)
	for _, tc := range []struct {
		name    string
		formats []Format
		want    int
	}{
		{"140 over 250", []Format{rankAudio(250, "opus", low, 2, 70_000), rankAudio(140, "mp4a.40.2", medium, 2, 129_000)}, 140},
		{"256 over 251", []Format{opus251, rankAudio(256, "mp4a.40.5", medium, 6, 192_000)}, 256},
		{"258 over 251", []Format{opus251, rankAudio(258, "mp4a.40.2", medium, 6, 384_000)}, 258},
		{"328 over 251", []Format{opus251, rankAudio(328, "ec-3", medium, 6, 384_000)}, 328},
		{"380 over 251", []Format{opus251, rankAudio(380, "ac-3", medium, 6, 384_000)}, 380},
		{"the 5.1 set", []Format{
			opus251,
			rankAudio(380, "ac-3", medium, 6, 384_000),
			rankAudio(328, "ec-3", medium, 6, 384_000),
			rankAudio(256, "mp4a.40.5", medium, 6, 192_000),
			rankAudio(258, "mp4a.40.2", medium, 6, 384_000),
		}, 258},
		{"141 over 251", []Format{opus251, rankAudio(141, "mp4a.40.2", high, 2, 255_000)}, 141},
		{"quality before channels: 141 over 256", []Format{rankAudio(256, "mp4a.40.5", medium, 6, 192_000), rankAudio(141, "mp4a.40.2", high, 2, 255_000)}, 141},
		{"the stereo original over a 5.1 dub", func() []Format {
			orig := rankAudio(251, "opus", medium, 2, 160_000)
			orig.AudioTrackID, orig.AudioTrackName = "ja.4", "Japanese original"
			dub := rankAudio(258, "mp4a.40.2", high, 6, 384_000)
			dub.AudioTrackID, dub.AudioTrackName = "en.3", "English"
			return []Format{dub, orig}
		}(), 251},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectBestFormats(tc.formats, 1080, true, true)
			if got.Audio == nil || got.Audio.Itag != tc.want {
				t.Errorf("audio = %v, want %d", itagOf(got.Audio), tc.want)
			}
		})
	}
}

// TestParseFormatsReadsAudioChannels: audioChannels reaches the Format, the
// channels rung's input. It was never parsed.
//
// Mutant: the getInt(f, "audioChannels") line removed.
func TestParseFormatsReadsAudioChannels(t *testing.T) {
	sd := decodePlayerJSON(t, `{"adaptiveFormats": [
		{"itag": 258, "url": "https://x/258", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioQuality": "AUDIO_QUALITY_MEDIUM", "audioChannels": 6},
		{"itag": 251, "url": "https://x/251", "mimeType": "audio/webm; codecs=\"opus\"", "audioQuality": "AUDIO_QUALITY_MEDIUM", "audioChannels": 2}]}`)
	formats, _ := NewPlayerAPI(nil, noopLogger{}).parseFormats(context.Background(), sd)
	if len(formats) != 2 || formats[0].AudioChannels != 6 || formats[1].AudioChannels != 2 {
		t.Fatalf("channels = %+v, want 6 and 2", formats)
	}
	if got := SelectBestFormats(formats, 1080, true, true); got.Audio == nil || got.Audio.Itag != 258 {
		t.Errorf("audio = %v, want the 5.1 258", itagOf(got.Audio))
	}
}

// TestDedupFillsAStreamsQualityFromAnotherClient: a tv-family copy states
// no audioQuality; where it wins its stream's dedup, the quality ladder read
// it as "tiny" and ranked 251 below 250. The dedup now fills the winner from
// another client's copy of the same itag — modelled on yt-dlp's
// itag_qualities, which fills only manifest formats, so beyond upstream for
// the https copies — and never touches a video format, whose
// audioQuality marks a muxed file.
//
// Mutants: the fill call removed from deduplicateFormats (250 wins); note
// keeping a video copy's audioQuality too (the tv 137 takes the web copy's,
// and with it IsProgressiveFormat's mark).
func TestDedupFillsAStreamsQualityFromAnotherClient(t *testing.T) {
	tv, web := AuthLevelTVPublic, AuthLevelWeb
	tv251 := rankAudio(251, "opus", "", 0, 160_000)
	tv251.Source, tv251.AuthLevel = "tv", &tv
	web251 := rankAudio(251, "opus", "AUDIO_QUALITY_MEDIUM", 2, 160_000)
	web251.Source, web251.AuthLevel = "web", &web
	web250 := rankAudio(250, "opus", "AUDIO_QUALITY_LOW", 2, 70_000)
	web250.Source, web250.AuthLevel = "web", &web
	video := rankVideo(137, "avc1.640028", 1920, 1080, 30, 4_000_000, false)
	video.Source, video.AuthLevel = "tv", &tv
	webVideo := video
	webVideo.Source, webVideo.AuthLevel, webVideo.AudioQuality = "web", &web, "AUDIO_QUALITY_MEDIUM"

	pool := deduplicateFormats(context.Background(), []Format{tv251, web251, web250, video, webVideo})
	got := SelectBestFormats(pool, 1080, true, true)
	if got.Audio == nil || got.Audio.Itag != 251 || got.Audio.Source != "tv" {
		t.Fatalf("audio = %v (%+v), want the tv copy of 251", itagOf(got.Audio), got.Audio)
	}
	if got.Audio.AudioQuality != "AUDIO_QUALITY_MEDIUM" || got.Audio.AudioChannels != 2 {
		t.Errorf("251's quality/channels = %q/%d, want them filled from the web copy", got.Audio.AudioQuality, got.Audio.AudioChannels)
	}
	if got.Video == nil || got.Video.AudioQuality != "" {
		t.Errorf("video %+v: a video format must not take an audioQuality", got.Video)
	}
}

// TestAudioLanguagesComeFromURLLessFormatsToo is yt-dlp #14883
// (_video.py:3528-3531) for G41: the original and default track languages
// are recorded from every format entry, the URL-less ones a SABR-forced
// client lists included — and across the extraction's responses, first
// statement winning, stamped on the cascade's VideoInfo. parseFormats
// discarded a URL-less entry whole, so a SABR-forced dubbed VOD had no
// record of its original for the HLS variants to be ranked against.
//
// Mutants (each run): noteAudioLanguages skipping entries without a url (the
// SABR response states nothing); the descriptive guard dropped (the
// descriptive German track, listed first and marked default, becomes the
// default); finishExtraction not restamping (the winner's own, empty, map
// survives); merge letting a later response overwrite (the default reads
// "fr").
func TestAudioLanguagesComeFromURLLessFormatsToo(t *testing.T) {
	sabr := decodePlayerJSON(t, `{"videoDetails": {"videoId": "test1234567", "lengthSeconds": "600"},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {"serverAbrStreamingUrl": "https://x/sabr", "adaptiveFormats": [
			{"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "audioTrack": {"id": "de.5", "displayName": "German descriptive", "audioIsDefault": true}},
			{"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "audioTrack": {"id": "ja.3", "displayName": "Japanese original", "audioIsDefault": false}},
			{"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "audioTrack": {"id": "en.3", "displayName": "English", "audioIsDefault": true}}]}}`)
	later := decodePlayerJSON(t, `{"videoDetails": {"videoId": "test1234567", "lengthSeconds": "600"},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {"hlsManifestUrl": "https://x/master.m3u8", "adaptiveFormats": [
			{"itag": 140, "url": "https://x/140", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "fr.3", "displayName": "French", "audioIsDefault": true}}]}}`)
	bare := decodePlayerJSON(t, `{"videoDetails": {"videoId": "test1234567", "lengthSeconds": "600"},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {"hlsManifestUrl": "https://x/master.m3u8"}}`)

	p := NewPlayerAPI(nil, noopLogger{})
	ctx := withExtractionState(context.Background())
	first, err := p.parsePlayerResponse(ctx, sabr, "", nil, "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	want := AudioLanguages{Original: "ja", Default: "en"}
	if first.AudioLanguages != want {
		t.Errorf("the SABR response's own languages = %+v, want %+v", first.AudioLanguages, want)
	}
	if _, err := p.parsePlayerResponse(ctx, later, "", nil, "test1234567"); err != nil {
		t.Fatal(err)
	}
	winner, err := p.parsePlayerResponse(ctx, bare, "", nil, "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	info, err := p.finishExtraction(ctx, winner, nil, "test1234567", &mismatchTally{attempts: 3, survived: 3}, nil)
	if err != nil {
		t.Fatalf("finishExtraction: %v", err)
	}
	if info.AudioLanguages != want {
		t.Errorf("the extraction's languages = %+v, want %+v", info.AudioLanguages, want)
	}
}

// TestLanguageTrackScore: a rendition known only by its language ranks the
// original first, then the default, then any other, as
// process_manifest_format's language_preference does (10, 5, -1); a track
// id's part after the dot is not part of the language.
func TestLanguageTrackScore(t *testing.T) {
	langs := AudioLanguages{Original: "ja", Default: "en"}
	for _, tc := range []struct {
		lang string
		want int
	}{{"ja.4", 20}, {"ja", 20}, {"en.3", 10}, {"fr.2", -2}, {"", -2}} {
		if got := LanguageTrackScore(tc.lang, langs); got != tc.want {
			t.Errorf("LanguageTrackScore(%q) = %d, want %d", tc.lang, got, tc.want)
		}
	}
	if got := LanguageTrackScore("", AudioLanguages{}); got != -2 {
		t.Errorf("an unstated language against an empty map = %d, want -2 (no match on \"\")", got)
	}
}

// TestStretchedRatioFromKeywords is G44: `yt:stretch=W:H` in
// videoDetails.keywords is the display aspect an old anamorphic upload asks
// for, read as _real_extract reads it (_video.py:4087-4101) — the first
// keyword with the prefix whose W and H parse above 0, a malformed or
// zero one passed over — and the extraction's is the first response's that
// states keywords at all (get_first). Nothing read the keywords before.
//
// Mutants (each run): the w > 0 && h > 0 guard dropped (the 0:9 row reads
// 0 and stops, the 16:9 after it lost — and 16:0 divides to +Inf); the loop
// stopping at the first prefixed keyword (the malformed row loses its
// valid sibling); the prefix test loosened to a substring
// ("not:stretch=4:3" counts);
// finishExtraction not restamping (the winner's own "no keywords" 0
// survives); noteStretch letting a later response replace the first.
func TestStretchedRatioFromKeywords(t *testing.T) {
	for _, tc := range []struct {
		keywords string // the JSON list, "" for none stated
		want     float64
		stated   bool
	}{
		{"", 0, false},
		{`["music", "live"]`, 0, true},
		{`["yt:stretch=16:9"]`, 16.0 / 9, true},
		{`["yt:stretch=16 : 9"]`, 16.0 / 9, true},
		{`["yt:stretch=0:9", "yt:stretch=16:9"]`, 16.0 / 9, true},
		{`["yt:stretch=16:0"]`, 0, true},
		{`["yt:stretch=wide", "yt:stretch=4:3"]`, 4.0 / 3, true},
		{`["not:stretch=4:3"]`, 0, true},
	} {
		vd := `{"videoId": "test1234567"}`
		if tc.keywords != "" {
			vd = `{"videoId": "test1234567", "keywords": ` + tc.keywords + `}`
		}
		got, stated := stretchedRatio(decodePlayerJSON(t, vd))
		if got != tc.want || stated != tc.stated {
			t.Errorf("keywords %s: ratio %v stated %v, want %v %v", tc.keywords, got, stated, tc.want, tc.stated)
		}
	}

	response := func(keywords string) map[string]any {
		vd := `"videoId": "test1234567", "lengthSeconds": "600"`
		if keywords != "" {
			vd += `, "keywords": ` + keywords
		}
		return decodePlayerJSON(t, `{"videoDetails": {`+vd+`}, "playabilityStatus": {"status": "OK"},
			"streamingData": {"adaptiveFormats": [{"itag": 18, "url": "https://x/18", "mimeType": "video/mp4; codecs=\"avc1.42001E, mp4a.40.2\"", "width": 480, "height": 360}]}}`)
	}
	p := NewPlayerAPI(nil, noopLogger{})
	ctx := withExtractionState(context.Background())
	for _, kw := range []string{"", `["yt:stretch=16:9"]`, `["yt:stretch=4:3"]`} {
		if _, err := p.parsePlayerResponse(ctx, response(kw), "", nil, "test1234567"); err != nil {
			t.Fatal(err)
		}
	}
	winner, err := p.parsePlayerResponse(ctx, response(""), "", nil, "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	if winner.StretchedRatio != 0 {
		t.Fatalf("a response without keywords carries ratio %v", winner.StretchedRatio)
	}
	info, err := p.finishExtraction(ctx, winner, nil, "test1234567", &mismatchTally{attempts: 4, survived: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.StretchedRatio != 16.0/9 {
		t.Errorf("the extraction's ratio = %v, want the first stating response's 16/9", info.StretchedRatio)
	}
}

// TestCodecPartsFollowsParseCodecs: codecParts is parse_codecs'
// `re.sub(r'0+(?=\d)', ...)` exactly, backtracking included — a run of
// zeros not followed by a digit keeps its last zero. Each row's want is
// Python's output (re.sub on the lower-cased fourcc's codec, split on dots).
//
// Mutant (run): the not-followed-by-a-digit arm dropped, so the whole run is
// kept ("00", "00M", "100").
func TestCodecPartsFollowsParseCodecs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"vp09.00.51.08", "vp9 0 51 8"},
		{"av01.0.00M.10", "av1 0 0M 10"},
		{"avc1.100.40", "avc1 10 40"},
		{"av01.0.12M.10", "av1 0 12M 10"},
		{"vp09.02.51.10", "vp9 2 51 10"},
		{"mp4a.40.2", "mp4a 40 2"},
	} {
		if got := strings.Join(codecParts(tc.in), " "); got != tc.want {
			t.Errorf("codecParts(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPremiumHoldsWithoutAKnownCodec: Premium is yt-dlp's source_preference
// +100, read off the label or the HLS itag, never the codec
// (_video.py:3444, 3666-3668) — so an HLS variant with no CODECS attribute
// keeps it.
//
// Mutant (run): the unknown-codec branch of VideoRankOf dropping Premium.
func TestPremiumHoldsWithoutAKnownCodec(t *testing.T) {
	for _, codecs := range []string{"", "unknown.1"} {
		if r := VideoRankOf(codecs, true); !r.Premium {
			t.Errorf("VideoRankOf(%q, true) = %+v, want Premium", codecs, r)
		}
	}
}
