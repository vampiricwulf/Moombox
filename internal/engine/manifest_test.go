package engine

import (
	"testing"
)

func TestParseDash(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="137" bandwidth="4000000" width="1920" height="1080" codecs="avc1.640028">
        <BaseURL>https://example.com/sq/0</BaseURL>
        <SegmentList startNumber="0" timescale="1000">
          <SegmentTimeline>
            <S d="2000" r="9"/>
          </SegmentTimeline>
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="140" bandwidth="128000" codecs="mp4a.40.2">
        <BaseURL>https://example.com/sq/0</BaseURL>
        <SegmentList startNumber="0" timescale="1000">
          <SegmentTimeline>
            <S d="2000" r="9"/>
          </SegmentTimeline>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	streams, err := ParseDash(mpd, "")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}

	if len(streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(streams))
	}

	video := streams[0]
	if video.Itag != 137 {
		t.Errorf("expected itag 137, got %d", video.Itag)
	}
	if video.Width != 1920 || video.Height != 1080 {
		t.Errorf("expected 1920x1080, got %dx%d", video.Width, video.Height)
	}
	if video.Codecs != "avc1.640028" {
		t.Errorf("expected avc1.640028, got %q", video.Codecs)
	}
	if len(video.Segments) != 1 {
		t.Fatalf("expected 1 segment entry, got %d", len(video.Segments))
	}
	// r=9 means 10 occurrences, each 2000ms = 20s total
	totalSegs := TotalSegmentCount(&video)
	if totalSegs != 10 {
		t.Errorf("expected 10 segments, got %d", totalSegs)
	}
	totalDur := TotalDurationSec(&video)
	if totalDur != 20.0 {
		t.Errorf("expected 20.0s duration, got %f", totalDur)
	}
}

func TestParseDash_YouTubeLiveTemplate(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="137" bandwidth="4000000" width="1920" height="1080">
        <BaseURL>https://rr1---sn-abc.googlevideo.com/videoplayback?sq/123&amp;itag=137</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	streams, err := ParseDash(mpd, "")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}

	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}

	// Should have /sq/$Number$ in the URL
	if streams[0].BaseURL == "" {
		t.Fatal("expected non-empty BaseURL")
	}
}

func TestParseHls_Master(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",VIDEO="chunked"
https://example.com/1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"
https://example.com/720p.m3u8`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsMaster {
		t.Error("expected master playlist")
	}
	if len(result.Variants) != 2 {
		t.Fatalf("expected 2 variants, got %d", len(result.Variants))
	}

	v1 := result.Variants[0]
	if v1.Bandwidth != 6000000 {
		t.Errorf("expected bandwidth 6000000, got %d", v1.Bandwidth)
	}
	if v1.Width != 1920 || v1.Height != 1080 {
		t.Errorf("expected 1920x1080, got %dx%d", v1.Width, v1.Height)
	}
	if !v1.IsSource {
		t.Error("expected chunked variant to be source")
	}
}

func TestParseHls_Media(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:4.0,
segment100.ts
#EXTINF:4.0,
segment101.ts
#EXTINF:4.0,
segment102.ts`

	result := ParseHls(m3u8, "https://example.com/stream/")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.IsMaster {
		t.Error("expected media playlist")
	}

	pl := result.Playlist
	if pl.TargetDuration != 4.0 {
		t.Errorf("expected target duration 4.0, got %f", pl.TargetDuration)
	}
	if pl.MediaSequence != 100 {
		t.Errorf("expected media sequence 100, got %d", pl.MediaSequence)
	}
	if len(pl.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(pl.Segments))
	}
	if pl.EndList {
		t.Error("expected endList=false for live")
	}

	seg := pl.Segments[0]
	if seg.Duration != 4.0 {
		t.Errorf("expected segment duration 4.0, got %f", seg.Duration)
	}
	if seg.URL != "https://example.com/stream/segment100.ts" {
		t.Errorf("unexpected segment URL: %s", seg.URL)
	}
}

func TestParseHls_VOD(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.0,
seg0.ts
#EXTINF:3.5,
seg1.ts
#EXT-X-ENDLIST`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil || result.Playlist == nil {
		t.Fatal("expected playlist")
	}
	if !result.Playlist.EndList {
		t.Error("expected endList=true for VOD")
	}
	if len(result.Playlist.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(result.Playlist.Segments))
	}
}

func TestSegmentURL(t *testing.T) {
	tmpl := "https://example.com/sq/$Number$"
	url := SegmentURL(tmpl, 42)
	if url != "https://example.com/sq/42" {
		t.Errorf("expected .../sq/42, got %s", url)
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		base     string
		ref      string
		expected string
	}{
		{"https://example.com/path/", "seg.ts", "https://example.com/path/seg.ts"},
		{"https://example.com/path/", "https://other.com/seg.ts", "https://other.com/seg.ts"},
		{"", "seg.ts", "seg.ts"},
	}
	for _, tt := range tests {
		got := resolveURL(tt.base, tt.ref)
		if got != tt.expected {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.base, tt.ref, got, tt.expected)
		}
	}
}
