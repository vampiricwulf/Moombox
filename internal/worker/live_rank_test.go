package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// liveFormat is one live adaptive format of a real itag (no contentLength, a
// targetDurationSec: what partitionManifestlessFormats pools).
func liveFormat(itag int, mime string, w, h, fps, bitrate int, mod ...func(*youtube.Format)) youtube.Format {
	f := youtube.Format{Itag: itag, URL: fmt.Sprintf("https://x/%d", itag), MimeType: mime, Bitrate: bitrate, TargetDurationSec: 2}
	if w > 0 {
		f.Width, f.Height, f.Fps = &w, &h, &fps
	}
	for _, m := range mod {
		m(&f)
	}
	return f
}

const (
	liveAvc1  = `video/mp4; codecs="avc1.64002a"`
	liveVp9   = `video/webm; codecs="vp9"`
	liveVp92  = `video/webm; codecs="vp09.02.51.10.01.09.16.09.00"`
	liveAv1   = `video/mp4; codecs="av01.0.09M.08"`
	liveOpus  = `audio/webm; codecs="opus"`
	liveAac   = `audio/mp4; codecs="mp4a.40.2"`
	liveMedia = "AUDIO_QUALITY_MEDIUM"
)

// TestManifestlessSelectionRanksLikeVOD is owner decision D14 on the
// manifest-free live and post-live path (G37): at a size and frame rate, the
// shared rank — dynamic range under prefer_hdr, Premium, codec — decides
// before bandwidth, as it does for a VOD. The selector ranked bandwidth
// alone, so at 1080p60 the avc1 299 beat the vp9 303 by its peak bitrate. The
// frame rate still comes first (the YouTube order), and audio ranks by
// quality before codec.
//
// Mutants (each run): compareRenditions without the VideoRank rung (299 wins,
// 335 loses); formatToDashStreamInfo leaving Codecs or Premium empty (299
// wins; 399 beats 356); the rank moved ahead of the fps rung (the 30 fps
// vp9.2 beats the 60 fps avc1); the audio branch back to bandwidth alone (250
// … its higher stated bitrate wins).
func TestManifestlessSelectionRanksLikeVOD(t *testing.T) {
	premium := func(f *youtube.Format) { f.QualityLabel = "1080p Premium" }
	quality := func(q string) func(*youtube.Format) { return func(f *youtube.Format) { f.AudioQuality = q } }
	for _, tc := range []struct {
		name      string
		formats   []youtube.Format
		preferHDR bool
		video     int
		audio     int
	}{
		{"vp9 303 over avc1 299 at 1080p60", []youtube.Format{
			liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
			liveFormat(303, liveVp9, 1920, 1080, 60, 4_400_000),
		}, true, 303, 0},
		{"vp9.2 HDR over avc1 and vp9", []youtube.Format{
			liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
			liveFormat(303, liveVp9, 1920, 1080, 60, 4_400_000),
			liveFormat(335, liveVp92, 1920, 1080, 60, 4_000_000),
		}, true, 335, 0},
		{"prefer_hdr off keeps the SDR vp9", []youtube.Format{
			liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
			liveFormat(303, liveVp9, 1920, 1080, 60, 4_400_000),
			liveFormat(335, liveVp92, 1920, 1080, 60, 4_000_000),
		}, false, 303, 0},
		{"the frame rate before the rank", []youtube.Format{
			liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
			liveFormat(334, liveVp92, 1920, 1080, 30, 4_000_000),
		}, true, 299, 0},
		{"Premium 356 over AV1 399", []youtube.Format{
			liveFormat(399, liveAv1, 1920, 1080, 30, 2_000_000),
			liveFormat(356, liveVp9, 1920, 1080, 30, 3_000_000, premium),
		}, true, 356, 0},
		{"140 over 250", []youtube.Format{
			liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
			liveFormat(250, liveOpus, 0, 0, 0, 200_000, quality("AUDIO_QUALITY_LOW")),
			liveFormat(140, liveAac, 0, 0, 0, 130_000, quality(liveMedia)),
		}, true, 299, 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			video, audio := partitionManifestlessFormats(tc.formats)
			if got := SelectBestDashStream(video, 0, 2160, true, "", true, tc.preferHDR); got == nil || got.Itag != tc.video {
				t.Errorf("video = %+v, want %d", got, tc.video)
			}
			if tc.audio != 0 {
				if got := SelectBestDashStream(audio, 0, 0, false, "", false, false); got == nil || got.Itag != tc.audio {
					t.Errorf("audio = %+v, want %d", got, tc.audio)
				}
			}
		})
	}
}

// TestAQualityPreferenceRanksLikeVOD: a "1080p" preference matched at its
// size, and the descent below a size that is missing, rank there by the same
// rule (selectAtHeightIdx / selectNextLowerIdx carry the rankRule), DASH and
// HLS alike.
//
// Mutant: selectAtHeightIdx ranking under a rule with the rank removed (the
// avc1 299 / the avc1 variant wins).
func TestAQualityPreferenceRanksLikeVOD(t *testing.T) {
	video, _ := partitionManifestlessFormats([]youtube.Format{
		liveFormat(299, liveAvc1, 1920, 1080, 60, 6_000_000),
		liveFormat(303, liveVp9, 1920, 1080, 60, 4_400_000),
		liveFormat(401, liveAv1, 3840, 2160, 60, 20_000_000),
	})
	for _, pref := range []string{"1080p", "1440p"} {
		if got := SelectBestDashStream(video, 0, 2160, true, pref, true, true); got == nil || got.Itag != 303 {
			t.Errorf("DASH pref %s: %+v, want 303", pref, got)
		}
	}
	variants := []engine.HlsVariant{
		{Width: 1920, Height: 1080, FPS: 60, Bandwidth: 6_000_000, Codecs: "avc1.64002a,mp4a.40.2", URL: "https://x/itag/301/a"},
		{Width: 1920, Height: 1080, FPS: 60, Bandwidth: 4_000_000, Codecs: "vp09.00.41.08,mp4a.40.2", URL: "https://x/itag/9999/b"},
	}
	if got := selectHlsVariant(variants, "1080p", 2160, true, true, youtube.AudioLanguages{}); got == nil || got.URL != variants[1].URL {
		t.Errorf("HLS pref 1080p: %+v, want the vp9 variant", got)
	}
}

// liveRankMPD is a DASH manifest of a dubbed live-to-VOD stream: avc1 299
// and vp9 303 at 1080p60, and one audio itag per language, the English dub
// at the higher bandwidth.
const liveRankMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="299" bandwidth="6000000" width="1920" height="1080" frameRate="60" codecs="avc1.64002a">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="video/webm">
      <Representation id="303" bandwidth="4400000" width="1920" height="1080" frameRate="60" codecs="vp9">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/303/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="en">
      <Representation id="141" bandwidth="256000" codecs="mp4a.40.2">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/141/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="ja">
      <Representation id="140" bandwidth="128000" codecs="mp4a.40.2">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/140/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// TestDownloadDashRanksTheManifestLikeVOD drives the DASH-manifest strategy
// end to end: the conversion carries each Representation's codecs and lang
// (manifestStreamInfos), so the vp9 303 beats the avc1 299 (D14), and the
// original-language audio — Japanese, by the extraction's language map —
// beats the English dub that out-bandwidths it (G41's manifest half). The
// conversion used to drop both, and the highest bandwidth took each.
//
// Mutants (each run): manifestStreamInfos dropping Codecs (299 wins) or the
// track score (141 wins); DownloadDash passing an empty language map (141).
func TestDownloadDashRanksTheManifestLikeVOD(t *testing.T) {
	srv := newManifestServer(t, liveRankMPD)
	job, _ := vodPotJob(t)
	job.Config.PreferHDR = true
	info := &youtube.VideoInfo{
		StreamStatus:    youtube.StreamPostLive,
		DashManifestURL: srv.URL + "/api/manifest/dash/id/abc",
		AudioLanguages:  youtube.AudioLanguages{Original: "ja", Default: "en"},
	}
	res, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadDash: %v", err)
	}
	if res.VideoItag != 303 || res.AudioItag != 140 {
		t.Errorf("selected video %d audio %d, want 303 and the original-language 140", res.VideoItag, res.AudioItag)
	}
}

// liveRankDubMaster is a SABR-forced dubbed VOD's HLS master as web_safari
// serves it: one pre-merged variant per audio track at each size, each
// naming its track. The English dub lists first and out-bandwidths the
// Japanese original.
const liveRankDubMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=4200000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="en.3"
https://manifest.googlevideo.com/api/manifest/hls_playlist/id/abc/itag/96/sgoap/lang%3Den/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="ja.4"
https://manifest.googlevideo.com/api/manifest/hls_playlist/id/abc/itag/96/sgoap/lang%3Dja/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="en.3"
https://manifest.googlevideo.com/api/manifest/hls_playlist/id/abc/itag/95/sgoap/lang%3Den/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1900000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="ja.4"
https://manifest.googlevideo.com/api/manifest/hls_playlist/id/abc/itag/95/sgoap/lang%3Dja/index.m3u8
`

// TestDownloadHlsKeepsTheOriginalLanguage is G41 end to end: a dubbed VOD
// whose HLS master pre-merges each audio track into its own variant records
// the original language's variant, ranked by YT-EXT-AUDIO-CONTENT-ID against
// the extraction's language map, ahead of bandwidth; with no map (or a
// variant naming no track) bandwidth decides as before. It used to archive
// whichever variant out-bandwidthed the rest — here the English dub.
//
// Mutants (each run): hlsFields leaving the track at 0 (the dub wins);
// DownloadHls passing an empty language map (the dub wins); the track rung
// ahead of the rank (unchanged here — pinned by
// TestHlsVariantRanksCodecBeforeLanguage).
func TestDownloadHlsKeepsTheOriginalLanguage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		langs  youtube.AudioLanguages
		pref   string
		height int
		track  string
	}{
		{"best", youtube.AudioLanguages{Original: "ja", Default: "en"}, "", 1080, "ja.4"},
		{"720p", youtube.AudioLanguages{Original: "ja", Default: "en"}, "720p", 720, "ja.4"},
		{"no map: bandwidth", youtube.AudioLanguages{}, "", 1080, "en.3"},
		{"default only", youtube.AudioLanguages{Default: "en"}, "", 1080, "en.3"},
		{"a default over an unranked original", youtube.AudioLanguages{Default: "ja"}, "", 1080, "ja.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newManifestServer(t, liveRankDubMaster)
			job, logs := vodPotJob(t)
			job.Job.QualityPreference = tc.pref
			info := &youtube.VideoInfo{
				StreamStatus:      youtube.StreamVOD,
				HlsManifestURL:    srv.URL + "/api/manifest/hls_variant/id/abc/file/index.m3u8",
				HlsManifestSource: "tv_public",
				AudioLanguages:    tc.langs,
			}
			if _, err := DownloadHls(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil); err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			lines := potLines(logs, "selected HLS variant")
			if len(lines) != 1 {
				t.Fatalf("selected-variant lines = %v, want one", lines)
			}
			if lines[0]["audioTrack"] != tc.track || lines[0]["height"] != tc.height {
				t.Errorf("selected %v, want the %dp variant of track %s", lines[0], tc.height, tc.track)
			}
		})
	}
}

// TestHlsVariantRanksCodecBeforeLanguage: the track is yt-dlp's `lang` rung,
// after the codec (sort `…, vcodec, channels, acodec, lang`), so a vp9 dub
// still beats an avc1 original at one size — and the Premium itag 616 beats
// a standard vp9 (_video.py:3666-3668).
//
// Mutants: the track compared ahead of the VideoRank (the avc1 original
// wins); hlsPremiumItag always false (the higher-bandwidth 9999 wins).
func TestHlsVariantRanksCodecBeforeLanguage(t *testing.T) {
	langs := youtube.AudioLanguages{Original: "ja"}
	variants := []engine.HlsVariant{
		{Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4_000_000, Codecs: "avc1.640028,mp4a.40.2", AudioContentID: "ja.4", URL: "https://x/itag/96/ja"},
		{Width: 1920, Height: 1080, FPS: 30, Bandwidth: 3_000_000, Codecs: "vp09.00.40.08,mp4a.40.2", AudioContentID: "en.3", URL: "https://x/itag/9998/en"},
	}
	if got := selectHlsVariant(variants, "", 2160, true, true, langs); got == nil || got.URL != variants[1].URL {
		t.Errorf("got %+v, want the vp9 variant", got)
	}
	premium := []engine.HlsVariant{
		{Width: 1920, Height: 1080, FPS: 30, Bandwidth: 5_000_000, Codecs: "vp09.00.40.08,mp4a.40.2", URL: "https://x/itag/9999/index.m3u8"},
		{Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4_500_000, Codecs: "vp09.00.40.08,mp4a.40.2", URL: "https://x/itag/616/index.m3u8"},
	}
	if got := selectHlsVariant(premium, "", 2160, true, true, youtube.AudioLanguages{}); got == nil || got.URL != premium[1].URL {
		t.Errorf("got %+v, want the Premium 616", got)
	}
}

// TestHlsAudioOnlyKeepsTheOriginalLanguage is G41 under the audio_only
// preference: YouTube HLS has no audio-only variant, so the cheapest variant
// is taken — but among the variants of the original track, never a dub's
// that happens to be cheaper. With no language map it is the plain lowest
// bandwidth it always was (TestSelectHlsVariantCap's audio_only row).
//
// Mutant (run): the audio_only branch comparing bandwidth alone again (the
// 300k English dub wins).
func TestHlsAudioOnlyKeepsTheOriginalLanguage(t *testing.T) {
	variants := []engine.HlsVariant{
		{Width: 256, Height: 144, FPS: 30, Bandwidth: 300_000, Codecs: "avc1.4d400c,mp4a.40.5", AudioContentID: "en.3", URL: "https://x/itag/269/en"},
		{Width: 256, Height: 144, FPS: 30, Bandwidth: 310_000, Codecs: "avc1.4d400c,mp4a.40.5", AudioContentID: "ja.4", URL: "https://x/itag/269/ja"},
		{Width: 1280, Height: 720, FPS: 30, Bandwidth: 1_900_000, Codecs: "avc1.4d401f,mp4a.40.2", AudioContentID: "ja.4", URL: "https://x/itag/95/ja"},
	}
	if got := selectHlsVariant(variants, "audio_only", 2160, true, true, youtube.AudioLanguages{Original: "ja", Default: "en"}); got == nil || got.URL != variants[1].URL {
		t.Errorf("got %+v, want the cheapest original-language variant", got)
	}
	if got := selectHlsVariant(variants, "audio_only", 2160, true, true, youtube.AudioLanguages{}); got == nil || got.URL != variants[0].URL {
		t.Errorf("no map: got %+v, want the lowest bandwidth", got)
	}
}

// TestManifestAudioTakesItsItagsQualityAndChannels: a DASH manifest states
// no audioQuality or channel count, so manifestStreamInfos fills both from
// the extraction's copy of the same itag — the quality as yt-dlp's
// itag_qualities fill (_video.py:3670), the channels beyond upstream so this
// path ranks like VOD (D14, D15). Without the fill the codec rung decided
// and stereo Opus 251 beat the E-AC-3 5.1 328 that SelectBestFormats picks
// from the same two (and that the old bandwidth-only rule picked); with no
// copy of an itag the rungs tie and the codec decides, as upstream's do.
//
// Mutants (each run): the fill dropped (251 wins the first row); the
// channels fill dropped (251 wins); the quality fill dropped (the LOW 5.1
// row's 328 beats the MEDIUM stereo 251 on channels).
func TestManifestAudioTakesItsItagsQualityAndChannels(t *testing.T) {
	streams := []engine.DashStream{
		{Itag: 251, MimeType: "audio/webm", Codecs: "opus", Bandwidth: 160_000, BaseURL: "https://x/251"},
		{Itag: 328, MimeType: "audio/mp4", Codecs: "ec-3", Bandwidth: 384_000, BaseURL: "https://x/328"},
	}
	audioFormat := func(itag int, mime, quality string, channels int) youtube.Format {
		return youtube.Format{Itag: itag, URL: fmt.Sprintf("https://x/%d", itag), MimeType: mime, AudioQuality: quality, AudioChannels: channels}
	}
	for _, tc := range []struct {
		name    string
		formats []youtube.Format
		want    int
	}{
		{"5.1 over stereo at one quality", []youtube.Format{
			audioFormat(251, `audio/webm; codecs="opus"`, "AUDIO_QUALITY_MEDIUM", 2),
			audioFormat(328, `audio/mp4; codecs="ec-3"`, "AUDIO_QUALITY_MEDIUM", 6),
		}, 328},
		{"quality before channels", []youtube.Format{
			audioFormat(251, `audio/webm; codecs="opus"`, "AUDIO_QUALITY_MEDIUM", 2),
			audioFormat(328, `audio/mp4; codecs="ec-3"`, "AUDIO_QUALITY_LOW", 6),
		}, 251},
		{"no copy of the itags: the codec decides", nil, 251},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectBestDashStream(manifestStreamInfos(streams, youtube.AudioLanguages{}, tc.formats), 0, 1080, false, "", true, true)
			if got == nil || got.Itag != tc.want {
				t.Errorf("audio = %+v, want %d", got, tc.want)
			}
			if tc.formats != nil {
				vod := youtube.SelectBestFormats(tc.formats, 1080, true, true)
				if vod.Audio == nil || vod.Audio.Itag != tc.want {
					t.Errorf("the VOD selector picks %+v from the same itags, want %d", vod.Audio, tc.want)
				}
			}
		})
	}
}
