package engine

import "testing"

// TestParseHlsReadsTheYouTubeAudioContentID: YouTube's master playlist for a
// dubbed video lists one pre-merged variant per audio track, each naming its
// track in YT-EXT-AUDIO-CONTENT-ID, which yt-dlp reads into the format's
// language (extractor/common.py:2390-2392). The parse ignored it, so the
// variant selector had no way to tell the original from a dub (G41).
//
// Mutant: the YT-EXT-AUDIO-CONTENT-ID case removed from parseMasterPlaylist.
func TestParseHlsReadsTheYouTubeAudioContentID(t *testing.T) {
	master := "#EXTM3U\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="en.3",YT-EXT-XTAGS="CggKA2xhbmcSAWU"` + "\n" +
		"https://example.com/itag/96/en/index.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=3900000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,YT-EXT-AUDIO-CONTENT-ID="ja.4"` + "\n" +
		"https://example.com/itag/96/ja/index.m3u8\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720` + "\n" +
		"https://example.com/itag/95/index.m3u8\n"
	parsed := ParseHls(master, "https://example.com/master.m3u8")
	if parsed == nil || len(parsed.Variants) != 3 {
		t.Fatalf("parsed %+v, want three variants", parsed)
	}
	for i, want := range []string{"en.3", "ja.4", ""} {
		if got := parsed.Variants[i].AudioContentID; got != want {
			t.Errorf("variant %d: AudioContentID = %q, want %q", i, got, want)
		}
	}
	if got := parsed.Variants[0].Codecs; got != "avc1.640028,mp4a.40.2" {
		t.Errorf("variant 0: Codecs = %q, want the quoted list whole", got)
	}
}

// TestParseDashReadsLang: an MPD audio Representation's lang — its own, else
// its AdaptationSet's — reaches DashStream.Lang, as yt-dlp's
// _parse_mpd_formats reads it, so the DASH-manifest audio selection can rank
// the original language first.
//
// Mutants: the representation/adaptation-set lang fallback dropped (the set's
// "ja" is lost); the Representation's own lang ignored (the "en" override
// reads "ja").
func TestParseDashReadsLang(t *testing.T) {
	mpd := `<?xml version="1.0"?><MPD><Period>` +
		`<AdaptationSet mimeType="audio/mp4" lang="ja">` +
		`<Representation id="140" bandwidth="130000" codecs="mp4a.40.2"><BaseURL>https://example.com/140</BaseURL></Representation>` +
		`<Representation id="139" bandwidth="50000" codecs="mp4a.40.5" lang="en"><BaseURL>https://example.com/139</BaseURL></Representation>` +
		`</AdaptationSet>` +
		`<AdaptationSet mimeType="video/mp4"><Representation id="137" bandwidth="4000000" codecs="avc1.640028" width="1920" height="1080"><BaseURL>https://example.com/137</BaseURL></Representation></AdaptationSet>` +
		`</Period></MPD>`
	streams, err := ParseDash(mpd, "")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}
	want := map[int]string{140: "ja", 139: "en", 137: ""}
	for _, s := range streams {
		if s.Lang != want[s.Itag] {
			t.Errorf("itag %d: Lang = %q, want %q", s.Itag, s.Lang, want[s.Itag])
		}
	}
	if len(streams) != 3 {
		t.Errorf("parsed %d streams, want 3", len(streams))
	}
}
