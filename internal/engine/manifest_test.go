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

func TestParseDash_EmptyPeriods(t *testing.T) {
	mpd := `<?xml version="1.0"?><MPD></MPD>`
	streams, err := ParseDash(mpd, "")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}
	if streams != nil {
		t.Errorf("expected nil streams for empty MPD, got %d", len(streams))
	}
}

func TestParseDash_InvalidXML(t *testing.T) {
	_, err := ParseDash("not xml", "")
	if err == nil {
		t.Error("expected error for invalid XML")
	}
}

func TestParseDash_SegmentTemplate(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate media="seg_$Number$.m4s" initialization="init.m4s" startNumber="1" timescale="90000">
        <SegmentTimeline>
          <S d="180000" r="4"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="1" bandwidth="2000000" width="1280" height="720">
        <BaseURL>https://cdn.example.com/stream/</BaseURL>
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

	s := streams[0]
	if s.StartNumber != 1 {
		t.Errorf("startNumber: got %d, want 1", s.StartNumber)
	}
	if s.Timescale != 90000 {
		t.Errorf("timescale: got %d, want 90000", s.Timescale)
	}
	if len(s.Segments) != 1 {
		t.Fatalf("expected 1 segment entry (with repeats), got %d", len(s.Segments))
	}
	if s.Segments[0].R != 4 {
		t.Errorf("repeat: got %d, want 4", s.Segments[0].R)
	}
	count := TotalSegmentCount(&s)
	if count != 5 {
		t.Errorf("total segments: got %d, want 5", count)
	}
	dur := TotalDurationSec(&s)
	if dur != 10.0 { // 5 segments * 180000/90000 = 10s
		t.Errorf("total duration: got %f, want 10.0", dur)
	}
}

func TestParseHls_Discontinuity(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-DISCONTINUITY-SEQUENCE:5
#EXTINF:4.0,
seg0.ts
#EXT-X-DISCONTINUITY
#EXTINF:4.0,
seg1.ts`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil || result.Playlist == nil {
		t.Fatal("expected playlist")
	}
	pl := result.Playlist
	if pl.DiscontinuitySequence != 5 {
		t.Errorf("discontinuity sequence: got %d, want 5", pl.DiscontinuitySequence)
	}
	if len(pl.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(pl.Segments))
	}
	if pl.Segments[0].Discontinuity != 0 {
		t.Errorf("seg0 discontinuity: got %d, want 0", pl.Segments[0].Discontinuity)
	}
	if pl.Segments[1].Discontinuity != 1 {
		t.Errorf("seg1 discontinuity: got %d, want 1", pl.Segments[1].Discontinuity)
	}
}

func TestParseHls_MasterFrameRate(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=59.94
https://example.com/1080p60.m3u8`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil || len(result.Variants) != 1 {
		t.Fatal("expected 1 variant")
	}
	if result.Variants[0].FPS != 60 { // 59.94 rounds to 60
		t.Errorf("FPS: got %d, want 60", result.Variants[0].FPS)
	}
}

func TestParseFrameRate(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"30", 30},
		{"60", 60},
		{"30/1", 30},
		{"30000/1001", 30},
		{"24000/1001", 24},
		{"59.94", 60},
		{"invalid", 0},
	}
	for _, tt := range tests {
		got := parseFrameRate(tt.input)
		if got != tt.want {
			t.Errorf("parseFrameRate(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestCalculateSegmentRange_NoSegments(t *testing.T) {
	stream := &DashStream{
		StartNumber: 0,
		Timescale:   1000,
	}
	result := CalculateSegmentRange(stream, 5.0, 15.0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Should use estimated 2s segments
	if result.StartSegment != 2 { // 5.0 / 2.0 = segment 2 (0-indexed)
		t.Errorf("StartSegment: got %d, want 2", result.StartSegment)
	}
}

func TestCalculateSegmentRange_WithTimeline(t *testing.T) {
	stream := &DashStream{
		StartNumber: 100,
		Timescale:   1000,
		Segments: []DashSegment{
			{D: 2000, R: 9}, // 10 segments of 2s each = 20s total
		},
	}
	result := CalculateSegmentRange(stream, 5.0, 15.0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Start at 5s: segment at index 2 (5.0/2.0), absolute = 100 + 2 = 102
	if result.StartSegment != 102 {
		t.Errorf("StartSegment: got %d, want 102", result.StartSegment)
	}
	if result.TrimDuration != 10.0 {
		t.Errorf("TrimDuration: got %f, want 10.0", result.TrimDuration)
	}
}

func TestSegmentDurationSec_Default(t *testing.T) {
	stream := &DashStream{Timescale: 1000}
	dur := SegmentDurationSec(stream)
	if dur != defaultSegmentDuration {
		t.Errorf("expected default %f, got %f", defaultSegmentDuration, dur)
	}
}

func TestEstimateSegmentCount(t *testing.T) {
	count := EstimateSegmentCount(10.0)
	if count != 5 { // ceil(10 / 2) = 5
		t.Errorf("expected 5, got %d", count)
	}
	count = EstimateSegmentCount(0)
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
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
