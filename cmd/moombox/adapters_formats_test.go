package main

import (
	"encoding/json"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// pickerFixture builds one format of a picker answer: video when w > 0,
// audio otherwise, with a URL (the picker lists nothing without one) and a
// contentLength unless mod takes it away.
func pickerFixture(itag, w, bitrate int, mime string, mod ...func(*youtube.Format)) youtube.Format {
	f := youtube.Format{Itag: itag, URL: "https://example.invalid/videoplayback", MimeType: mime, Bitrate: bitrate, ContentLength: "1048576"}
	if w > 0 {
		h := w * 9 / 16
		f.Width, f.Height = &w, &h
	}
	for _, m := range mod {
		m(&f)
	}
	return f
}

func damagedFormat(f *youtube.Format) { f.Damaged = true }

// otfFormat and liveAdaptiveFormat are the two kinds youtube.Format.Segmented
// names, each without the contentLength that would make it a file.
func otfFormat(f *youtube.Format)          { f.IsOTF, f.ContentLength = true, "" }
func liveAdaptiveFormat(f *youtube.Format) { f.TargetDurationSec, f.ContentLength = 2, "" }

const (
	pickerWebm = `video/webm; codecs="vp9"`
	pickerMp4  = `video/mp4; codecs="avc1.640028"`
	pickerOpus = `audio/webm; codecs="opus"`
	pickerAac  = `audio/mp4; codecs="mp4a.40.2"`
)

// pickerJSON is formatsPayload as the route sends it: marshalled, then read
// back the way a client reads it.
func pickerJSON(t *testing.T, info *youtube.VideoInfo) (raw []byte, decoded map[string]any) {
	t.Helper()
	raw, err := json.Marshal(formatsPayload("vid", info))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return raw, decoded
}

// pickerRowsByItag indexes one list of the decoded answer by itag.
func pickerRowsByItag(t *testing.T, decoded map[string]any, list string) map[int]map[string]any {
	t.Helper()
	rows, _ := decoded[list].([]any)
	out := make(map[int]map[string]any, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		itag, _ := row["itag"].(float64)
		out[int(itag)] = row
	}
	return out
}

// TestFormatsPayloadMarksDamagedAndSegmentedRenditions is the owner's W9 and
// W4 picker follow-up. The format picker's answer (GET /api/formats) knew
// nothing of the two marks the downloader acts on: it badged the top WEBM
// [Best WEBM] — and the dashboard preselected it — though that rendition was
// encoded short of half the video (automatic selection passes it over) or
// served only as fragments (a whole-file VOD cannot fetch it). Each row now
// says `damaged` and `segmented`, beside every field it already carried, and
// a badge goes to the best rendition of its kind that is neither. A kind
// whose every rendition is marked keeps its old pick, so the badge exists.
//
// Mutants (each run): the rows without their marks (the flag assertions
// fail); bestPickerItag ignoring Damaged (WEBM 313, OPUS 251) or Segmented
// (WEBM 271); Segmented read as IsOTF alone, or as IsOTF ||
// TargetDurationSec without the contentLength (264, an OTF file, is marked
// and loses the MP4 badge); the fallback dropped (AAC null).
func TestFormatsPayloadMarksDamagedAndSegmentedRenditions(t *testing.T) {
	info := &youtube.VideoInfo{Title: "t", Formats: []youtube.Format{
		pickerFixture(313, 3840, 20_000_000, pickerWebm, damagedFormat),
		pickerFixture(271, 2560, 9_000_000, pickerWebm, otfFormat),
		pickerFixture(248, 1920, 4_000_000, pickerWebm),
		// An OTF format that states its length is a file (Segmented's own
		// narrowing): it is badged, not marked.
		pickerFixture(264, 2560, 8_000_000, pickerMp4, func(f *youtube.Format) { f.IsOTF = true }),
		pickerFixture(137, 1920, 6_000_000, pickerMp4, liveAdaptiveFormat),
		pickerFixture(136, 1280, 2_000_000, pickerMp4),
		pickerFixture(251, 0, 160_000, pickerOpus, damagedFormat),
		pickerFixture(250, 0, 70_000, pickerOpus),
		pickerFixture(140, 0, 128_000, pickerAac, damagedFormat),
		pickerFixture(139, 0, 48_000, pickerAac, damagedFormat),
	}}
	_, decoded := pickerJSON(t, info)

	video := pickerRowsByItag(t, decoded, "videoFormats")
	audio := pickerRowsByItag(t, decoded, "audioFormats")
	for _, tc := range []struct {
		rows               map[int]map[string]any
		itag               int
		damaged, segmented bool
	}{
		{video, 313, true, false},
		{video, 271, false, true},
		{video, 248, false, false},
		{video, 264, false, false},
		{video, 137, false, true},
		{video, 136, false, false},
		{audio, 251, true, false},
		{audio, 250, false, false},
		{audio, 140, true, false},
		{audio, 139, true, false},
	} {
		row := tc.rows[tc.itag]
		if row == nil {
			t.Errorf("itag %d missing from the answer", tc.itag)
			continue
		}
		if row["damaged"] != tc.damaged || row["segmented"] != tc.segmented {
			t.Errorf("itag %d: damaged=%v segmented=%v, want %v and %v", tc.itag, row["damaged"], row["segmented"], tc.damaged, tc.segmented)
		}
		// Additive: the format's own fields are still there.
		if row["mimeType"] == nil || row["bitrate"] == nil || row["url"] == nil {
			t.Errorf("itag %d lost its own fields: %v", tc.itag, row)
		}
	}

	best, _ := decoded["bestItags"].(map[string]any)
	for key, want := range map[string]float64{
		"bestWebmVideo": 248, // past the damaged 313 and the OTF 271
		"bestMp4Video":  264, // the OTF file; the live adaptive 137 is past it anyway
		"bestOpusAudio": 250, // past the damaged 251
		"bestAacAudio":  140, // every AAC is damaged: the old pick, the first
	} {
		if best[key] != want {
			t.Errorf("bestItags.%s = %v, want %v", key, best[key], want)
		}
	}
}

// TestFormatsPayloadKeepsABadgeWhenEveryRenditionIsMarked: on a live or
// post-live stream every format is live adaptive, so every row is segmented.
// The badges then keep the pick they always made — the first of each kind —
// rather than vanish, and a kind with no rendition at all is still null.
//
// Mutant: bestPickerItag without its fallback (every badge is null).
func TestFormatsPayloadKeepsABadgeWhenEveryRenditionIsMarked(t *testing.T) {
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{
		pickerFixture(299, 1920, 6_000_000, pickerMp4, liveAdaptiveFormat),
		pickerFixture(298, 1280, 3_000_000, pickerMp4, liveAdaptiveFormat),
		pickerFixture(140, 0, 128_000, pickerAac, liveAdaptiveFormat),
	}}
	_, decoded := pickerJSON(t, info)
	best, _ := decoded["bestItags"].(map[string]any)
	if best["bestMp4Video"] != float64(299) || best["bestAacAudio"] != float64(140) {
		t.Errorf("bestItags = %v, want the first MP4 (299) and AAC (140) though every one is segmented", best)
	}
	for _, key := range []string{"bestWebmVideo", "bestOpusAudio"} {
		if v, ok := best[key]; !ok || v != nil {
			t.Errorf("bestItags.%s = %v (present %v), want null: no rendition of that kind", key, v, ok)
		}
	}
}

// pickerMuxed is itag 18's mimeType: video and audio in one file.
const pickerMuxed = `video/mp4; codecs="avc1.42001E, mp4a.40.2"`

func muxedFormat(f *youtube.Format) { f.AudioQuality = "AUDIO_QUALITY_LOW" }

// TestFormatsPayloadBadgesNoMuxedFileOverASegmentedSibling: a muxed
// progressive file (itag 18) is a whole file wherever its adaptive siblings
// are segmented, so a badge that skipped the marks alone gave it [Best MP4]
// on an OTF VOD, and the dashboard — which puts an intact badge first —
// preselected the 640x360 file. Such a VOD comes with a manifest and runs
// on DashStrategy, whose manifest has no itag 18: the pin fell back to
// automatic selection where the 1080p WEBM preselected before was honoured.
// The badges go to adaptive renditions, falling back to the first adaptive
// one of a kind, marked; a muxed row is badged only when its kind has no
// adaptive row at all.
//
// Mutants (each run): muxed() always false — the marks-only skip (18 is
// [Best MP4] on the OTF VOD and the post-live stream); the muxed fallback
// ahead of the marked one (18 wins wherever it is listed); the muxed
// fallback dropped (a kind of muxed rows only has no badge).
func TestFormatsPayloadBadgesNoMuxedFileOverASegmentedSibling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		info     *youtube.VideoInfo
		wantMp4  float64
		wantWebm any
	}{
		{
			// The worker's own fixture of this stream: OTF adaptive formats
			// beside a whole muxed 18 (TestDownloadVodLeavesSegmentedFormatsOut).
			name: "an OTF VOD with a whole muxed 18",
			info: &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, DashManifestURL: "https://example.invalid/mpd", Formats: []youtube.Format{
				pickerFixture(248, 1920, 4_000_000, pickerWebm, otfFormat),
				pickerFixture(137, 1920, 6_000_000, pickerMp4, otfFormat),
				pickerFixture(136, 1280, 2_000_000, pickerMp4, otfFormat),
				pickerFixture(18, 640, 500_000, pickerMuxed, muxedFormat),
				pickerFixture(251, 0, 160_000, pickerOpus, otfFormat),
				pickerFixture(140, 0, 128_000, pickerAac, otfFormat),
			}},
			wantMp4: 137, wantWebm: float64(248),
		},
		{
			// A post-live response can list 18 without a contentLength
			// (youtube.Format.IsVideoOnly); it is no live adaptive format,
			// so it is not segmented either.
			name: "a post-live stream with a contentLength-less muxed 18",
			info: &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, Formats: []youtube.Format{
				pickerFixture(299, 1920, 6_000_000, pickerMp4, liveAdaptiveFormat),
				pickerFixture(18, 640, 500_000, pickerMuxed, muxedFormat, func(f *youtube.Format) { f.ContentLength = "" }),
				pickerFixture(140, 0, 128_000, pickerAac, liveAdaptiveFormat),
			}},
			wantMp4: 299, wantWebm: nil,
		},
		{
			// Same size and frame rate, so the lower bitrate sorts first: the
			// muxed file is the first MP4 row, ahead of the segmented 134.
			name: "a muxed 18 listed ahead of every adaptive MP4",
			info: &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, DashManifestURL: "https://example.invalid/mpd", Formats: []youtube.Format{
				pickerFixture(243, 640, 400_000, pickerWebm, otfFormat),
				pickerFixture(18, 640, 300_000, pickerMuxed, muxedFormat),
				pickerFixture(134, 640, 400_000, pickerMp4, otfFormat),
				pickerFixture(140, 0, 128_000, pickerAac, otfFormat),
			}},
			wantMp4: 134, wantWebm: float64(243),
		},
		{
			name: "a kind of muxed rows only",
			info: &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, Formats: []youtube.Format{
				pickerFixture(248, 1920, 4_000_000, pickerWebm),
				pickerFixture(18, 640, 500_000, pickerMuxed, muxedFormat),
				pickerFixture(251, 0, 160_000, pickerOpus),
			}},
			wantMp4: 18, wantWebm: float64(248),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, decoded := pickerJSON(t, tc.info)
			best, _ := decoded["bestItags"].(map[string]any)
			if best["bestMp4Video"] != tc.wantMp4 || best["bestWebmVideo"] != tc.wantWebm {
				t.Errorf("bestItags = %v, want bestMp4Video %v and bestWebmVideo %v", best, tc.wantMp4, tc.wantWebm)
			}
			if row := pickerRowsByItag(t, decoded, "videoFormats")[18]; row == nil || row["damaged"] != false || row["segmented"] != false {
				t.Errorf("itag 18 = %v, want listed, unmarked: a whole file is neither", row)
			}
		})
	}
}

// TestFormatsPayloadDecodesIntoTheTUIPicker holds the TUI's decode to the
// route's shape. The TUI read the four Best picks as flat fields of
// FormatsData, where the answer nests them in bestItags, so the decode
// filled none and the TUI picker never showed a Best note; it reads the
// nested object now, and the two marks beside each row.
//
// Mutants: FormatsData's picks flat again (BestItags zero); the TUI's
// VideoFormat or AudioFormat without Damaged/Segmented (the marks lost).
func TestFormatsPayloadDecodesIntoTheTUIPicker(t *testing.T) {
	info := &youtube.VideoInfo{Formats: []youtube.Format{
		pickerFixture(313, 3840, 20_000_000, pickerWebm, damagedFormat),
		pickerFixture(248, 1920, 4_000_000, pickerWebm),
		pickerFixture(137, 1920, 6_000_000, pickerMp4, otfFormat),
		pickerFixture(136, 1280, 2_000_000, pickerMp4),
		pickerFixture(251, 0, 160_000, pickerOpus, otfFormat),
		pickerFixture(250, 0, 70_000, pickerOpus),
		pickerFixture(140, 0, 128_000, pickerAac, damagedFormat),
	}}
	raw, _ := pickerJSON(t, info)
	var data tui.FormatsData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode into tui.FormatsData: %v", err)
	}
	if want := (tui.BestItags{BestWebmVideo: 248, BestMp4Video: 136, BestOpusAudio: 250, BestAacAudio: 140}); data.BestItags != want {
		t.Errorf("BestItags = %+v, want %+v", data.BestItags, want)
	}
	marks := map[int][2]bool{}
	for _, f := range data.VideoFormats {
		marks[f.Itag] = [2]bool{f.Damaged, f.Segmented}
	}
	for _, f := range data.AudioFormats {
		marks[f.Itag] = [2]bool{f.Damaged, f.Segmented}
	}
	for itag, want := range map[int][2]bool{313: {true, false}, 248: {}, 137: {false, true}, 136: {}, 251: {false, true}, 250: {}, 140: {true, false}} {
		if marks[itag] != want {
			t.Errorf("itag %d: [damaged segmented] = %v, want %v", itag, marks[itag], want)
		}
	}
}
