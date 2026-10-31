package main

import (
	"encoding/json"
	"testing"

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
