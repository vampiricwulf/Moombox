package engine

import "testing"

// demuxedMasterForTest is the shape of the visionos master YouTube hands a
// live-edge capture (measured 2026-11-27 on three 24/7 streams): every
// variant names AUDIO group 233 or 234, each an #EXT-X-MEDIA TYPE=AUDIO
// rendition with a URI of its own, and CODECS names the audio codec although
// the variant's segments carry only h264. The rendition tags sit AFTER the
// first variant here on purpose: the parse must read them before any
// variant, as yt-dlp does.
const demuxedMasterForTest = "#EXTM3U\n" +
	"#EXT-X-INDEPENDENT-SEGMENTS\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=290000,CODECS="avc1.42C00B,mp4a.40.5",RESOLUTION=256x144,FRAME-RATE=15,AUDIO="233"` + "\n" +
	"https://example.com/itag/269/index.m3u8\n" +
	`#EXT-X-MEDIA:URI="https://example.com/itag/233/index.m3u8",TYPE=AUDIO,GROUP-ID="233",NAME="Default",DEFAULT=YES,AUTOSELECT=YES` + "\n" +
	`#EXT-X-MEDIA:URI="audio/itag/234/index.m3u8",TYPE=AUDIO,GROUP-ID="234",NAME="Default",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE="en"` + "\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=4500000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,AUDIO="234"` + "\n" +
	"https://example.com/itag/270/index.m3u8\n"

// TestParseHlsDemuxedMaster: a variant whose AUDIO group's first rendition
// has a URI is video-only, and the renditions are listed with their group,
// name, DEFAULT and resolved URI (yt-dlp, extractor/common.py
// _parse_m3u8_formats_and_subtitles).
//
// Mutants: the AUDIO case removed (no group, nothing video-only); the
// #EXT-X-MEDIA pre-pass removed (no renditions); VideoOnly set without the
// URI check (the muxed test below fails); URI left unresolved.
func TestParseHlsDemuxedMaster(t *testing.T) {
	p := ParseHls(demuxedMasterForTest, "https://example.com/master/index.m3u8")
	if p == nil || !p.IsMaster || len(p.Variants) != 2 {
		t.Fatalf("parsed %+v, want a master with two variants", p)
	}
	for i, want := range []string{"233", "234"} {
		v := p.Variants[i]
		if v.AudioGroup != want || !v.VideoOnly {
			t.Errorf("variant %d: AudioGroup %q VideoOnly %v, want %q and video-only", i, v.AudioGroup, v.VideoOnly, want)
		}
	}
	if len(p.Renditions) != 2 {
		t.Fatalf("renditions %+v, want two", p.Renditions)
	}
	r := p.AudioRenditions("234")
	if len(r) != 1 {
		t.Fatalf("AudioRenditions(234) = %+v, want one", r)
	}
	if r[0].URI != "https://example.com/master/audio/itag/234/index.m3u8" || r[0].Name != "Default" || !r[0].Default || r[0].Alternate || r[0].Language != "en" {
		t.Errorf("rendition 234 = %+v", *r[0])
	}
	if got := p.AudioRenditions("233"); len(got) != 1 || got[0].URI != "https://example.com/itag/233/index.m3u8" {
		t.Errorf("AudioRenditions(233) = %+v", got)
	}
}

// TestParseHlsAudioRenditionWithoutURIIsMuxed: a rendition that names the
// group but carries no URI means the audio is inside the variant — the
// variant is not video-only, and the group has no rendition to record.
// So is a variant naming a group without CODECS (yt-dlp takes it as a
// complete format), an audio-only variant, and one that names no group (the
// old muxed masters, itags 91-96).
func TestParseHlsAudioRenditionWithoutURIIsMuxed(t *testing.T) {
	master := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",DEFAULT=YES` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="sep",NAME="Main",URI="https://example.com/a.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,AUDIO="aac"` + "\n" +
		"https://example.com/720.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,AUDIO="sep"` + "\n" +
		"https://example.com/nocodecs.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=128000,CODECS="mp4a.40.2",AUDIO="sep"` + "\n" +
		"https://example.com/audio.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720` + "\n" +
		"https://example.com/itag/95/index.m3u8\n"
	p := ParseHls(master, "https://example.com/m.m3u8")
	if p == nil || len(p.Variants) != 4 {
		t.Fatalf("parsed %+v", p)
	}
	for i, v := range p.Variants {
		if v.VideoOnly {
			t.Errorf("variant %d (%s) marked video-only", i, v.URL)
		}
	}
	if got := p.AudioRenditions("aac"); len(got) != 0 {
		t.Errorf("AudioRenditions(aac) = %+v, want none: its rendition has no URI", got)
	}
}

// TestParseHlsRenditionNeedsTypeGroupAndName: yt-dlp's extract_media drops
// an #EXT-X-MEDIA without TYPE, GROUP-ID or NAME, so a group whose only tag
// lacks one is no group, and its variant is not video-only. DEFAULT=NO and
// AUTOSELECT=NO mark an alternate.
func TestParseHlsRenditionNeedsTypeGroupAndName(t *testing.T) {
	master := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="g",URI="https://example.com/a.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="h",NAME="Commentary",DEFAULT=NO,URI="https://example.com/c.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="h",NAME="Described",AUTOSELECT=NO,URI="https://example.com/d.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="g"` + "\n" +
		"https://example.com/v.m3u8\n"
	p := ParseHls(master, "https://example.com/m.m3u8")
	if p == nil || len(p.Variants) != 1 {
		t.Fatalf("parsed %+v", p)
	}
	if p.Variants[0].VideoOnly {
		t.Error("variant marked video-only by a rendition tag that has no NAME")
	}
	if len(p.Renditions) != 2 {
		t.Fatalf("renditions = %+v, want the two named ones", p.Renditions)
	}
	for _, r := range p.Renditions {
		if !r.Alternate || r.Default {
			t.Errorf("rendition %q: Alternate %v Default %v, want an alternate", r.Name, r.Alternate, r.Default)
		}
	}
}
