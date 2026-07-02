package engine

import (
	"strings"
	"testing"
)

// Regression: the /sq/N → /sq/$Number$ conversion must use the LITERAL
// replacement variant. ReplaceAllString interprets "$Number" as a
// (nonexistent) named-group reference and expands it to empty, yielding
// "/sq/$" — which 404'd every segment of any manifest whose Representation
// BaseURL carried a current-sequence component.
func TestParseDash_SqBaseURLBecomesNumberTemplate(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="299" bandwidth="9000000" width="1920" height="1080">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc123DEF45.1/itag/299/sq/1234</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	streams, err := ParseDash(mpd, "https://manifest.example/dash.mpd")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("streams: want 1, got %d", len(streams))
	}
	if base := streams[0].BaseURL; !strings.Contains(base, "/sq/$Number$") {
		t.Errorf("BaseURL missing the /sq/$Number$ template (the $-expansion bug yields \"/sq/$\"): %q", base)
	}
}

// Regression: ParseHls must reject content that is not an M3U8 document —
// CDNs serve HTML/JSON error pages with 200 OK, and parsing their non-#
// lines as segment URLs produced garbage downloads and bogus gap records.
func TestParseHls_RejectsNonPlaylistContent(t *testing.T) {
	if got := ParseHls("<html><body>502 Bad Gateway</body></html>", "https://x/"); got != nil {
		t.Errorf("HTML error page: want nil, got %+v", got)
	}
	if got := ParseHls(`{"error":"rate limited"}`, "https://x/"); got != nil {
		t.Errorf("JSON error body: want nil, got %+v", got)
	}
	// Leading blank lines before #EXTM3U are tolerated.
	got := ParseHls("\n\n#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nseg1.ts\n", "https://x/")
	if got == nil || got.Playlist == nil || len(got.Playlist.Segments) != 1 {
		t.Errorf("valid playlist with leading blanks: want 1 segment, got %+v", got)
	}
}

// Regression: a UTF-8 BOM must be stripped for the WHOLE document, not just
// the guard's first-line check — otherwise parseMediaPlaylist (which uses
// TrimSpace, which does not strip U+FEFF) reads "\ufeff#EXTM3U" as a segment
// URL, shifting every real segment's sequence by one.
func TestParseHls_StripsBOM(t *testing.T) {
	m3u8 := "\ufeff#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\nseg0.ts\n#EXTINF:2.0,\nseg1.ts\n"
	result := ParseHls(m3u8, "https://x/")
	if result == nil || result.Playlist == nil {
		t.Fatal("BOM-prefixed playlist: expected a parsed media playlist")
	}
	if len(result.Playlist.Segments) != 2 {
		t.Fatalf("expected 2 segments (no bogus #EXTM3U segment), got %d", len(result.Playlist.Segments))
	}
	if got := result.Playlist.Segments[0].URL; got != "https://x/seg0.ts" {
		t.Errorf("first segment URL = %q, want the real seg0.ts (not the #EXTM3U line)", got)
	}
}

// Regression: an absurdly large body (a proxy/error page served on the
// playlist endpoint) must be rejected before the per-newline Split allocates
// a giant slice-header array.
func TestParseHls_RejectsOversizedBody(t *testing.T) {
	huge := "#EXTM3U\n" + strings.Repeat("\n", (16<<20)+1)
	if got := ParseHls(huge, "https://x/"); got != nil {
		t.Errorf("oversized body: want nil, got %+v", got)
	}
}

// Regression: a relative top-level MPD <BaseURL> must be resolved against the
// manifest URL, like every lower level — using it verbatim left every segment
// URL relative and 404-ing.
func TestParseDash_RelativeTopLevelBaseURL(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD>
  <BaseURL>dash/</BaseURL>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="137" bandwidth="4000000" width="1920" height="1080">
        <BaseURL>seg/</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	streams, err := ParseDash(mpd, "https://cdn.example.com/live/manifest.mpd")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	// dash/ resolves against the manifest dir, then seg/ against that.
	want := "https://cdn.example.com/live/dash/seg/"
	if streams[0].BaseURL != want {
		t.Errorf("BaseURL = %q, want %q (relative top-level BaseURL resolved against manifest URL)", streams[0].BaseURL, want)
	}
}

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
	totalSegs := totalSegmentCount(&video)
	if totalSegs != 10 {
		t.Errorf("expected 10 segments, got %d", totalSegs)
	}
	totalDur := totalDurationSec(&video)
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

func TestParseHls_ZeroDurationFallback(t *testing.T) {
	// Malformed #EXTINF with zero duration should fall back to TargetDuration.
	m3u8 := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:0,
seg0.ts
#EXTINF:,
seg1.ts
#EXTINF:4.0,
seg2.ts`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil || result.Playlist == nil {
		t.Fatal("expected playlist")
	}
	pl := result.Playlist
	if len(pl.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(pl.Segments))
	}
	// seg0: EXTINF:0 -> falls back to TargetDuration (6)
	if pl.Segments[0].Duration != 6.0 {
		t.Errorf("seg0 Duration: got %f, want 6.0 (TargetDuration fallback)", pl.Segments[0].Duration)
	}
	// seg1: malformed EXTINF -> falls back too
	if pl.Segments[1].Duration != 6.0 {
		t.Errorf("seg1 Duration: got %f, want 6.0 (TargetDuration fallback)", pl.Segments[1].Duration)
	}
	// seg2: valid EXTINF stays as-is
	if pl.Segments[2].Duration != 4.0 {
		t.Errorf("seg2 Duration: got %f, want 4.0", pl.Segments[2].Duration)
	}
}

func TestParseHls_ZeroDurationNoTargetDuration(t *testing.T) {
	// Zero-duration segment + no TargetDuration -> falls back to
	// defaultSegmentDuration (2.0) so progress math never sees zero.
	m3u8 := `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:0,
seg0.ts`

	result := ParseHls(m3u8, "https://example.com/")
	if result == nil || result.Playlist == nil {
		t.Fatal("expected playlist")
	}
	if len(result.Playlist.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(result.Playlist.Segments))
	}
	if result.Playlist.Segments[0].Duration != defaultSegmentDuration {
		t.Errorf("Duration: got %f, want %f (default fallback)",
			result.Playlist.Segments[0].Duration, defaultSegmentDuration)
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

func TestParseDash_NonNumericID_Itag(t *testing.T) {
	// Non-numeric representation IDs should leave Itag at the -1 sentinel
	// so they don't all collide on 0 and confuse manual-itag selection.
	mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="video-1080" bandwidth="4000000" width="1920" height="1080">
        <BaseURL>https://example.com/seg</BaseURL>
      </Representation>
      <Representation id="video-720" bandwidth="2000000" width="1280" height="720">
        <BaseURL>https://example.com/seg2</BaseURL>
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
	for i, s := range streams {
		if s.Itag != -1 {
			t.Errorf("stream %d: Itag = %d, want -1 (sentinel for non-numeric ID)", i, s.Itag)
		}
	}
}

func TestParseDash_MissingID_Itag(t *testing.T) {
	// Representations without an id attribute get the -1 sentinel.
	mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation bandwidth="4000000" width="1920" height="1080">
        <BaseURL>https://example.com/seg</BaseURL>
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
	if streams[0].Itag != -1 {
		t.Errorf("Itag = %d, want -1 (sentinel for missing ID)", streams[0].Itag)
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
	count := totalSegmentCount(&s)
	if count != 5 {
		t.Errorf("total segments: got %d, want 5", count)
	}
	dur := totalDurationSec(&s)
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

func TestCalculateSegmentRange_NegativeTimes(t *testing.T) {
	stream := &DashStream{StartNumber: 0, Timescale: 1000}
	// Negative start
	if r := CalculateSegmentRange(stream, -1.0, 10.0); r != nil {
		t.Errorf("expected nil for negative startTimeSec, got %+v", r)
	}
	// Negative end
	if r := CalculateSegmentRange(stream, 1.0, -5.0); r != nil {
		t.Errorf("expected nil for negative endTimeSec, got %+v", r)
	}
}

func TestCalculateSegmentRange_ReversedRange(t *testing.T) {
	stream := &DashStream{StartNumber: 0, Timescale: 1000}
	// End <= start with end > 0 should return nil
	if r := CalculateSegmentRange(stream, 10.0, 5.0); r != nil {
		t.Errorf("expected nil for reversed range, got %+v", r)
	}
	// Zero-length range (start == end)
	if r := CalculateSegmentRange(stream, 10.0, 10.0); r != nil {
		t.Errorf("expected nil for zero-length range, got %+v", r)
	}
}

func TestCalculateSegmentRange_EndZero_IsUnbounded(t *testing.T) {
	stream := &DashStream{StartNumber: 0, Timescale: 1000}
	// endTimeSec == 0 means "no end bound" -- valid even with startTimeSec > 0
	r := CalculateSegmentRange(stream, 5.0, 0)
	if r == nil {
		t.Fatal("expected non-nil result for endTimeSec == 0")
	}
	if r.EndSegment != -1 {
		t.Errorf("EndSegment: got %d, want -1 (unbounded)", r.EndSegment)
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
	dur := segmentDurationSec(stream)
	if dur != defaultSegmentDuration {
		t.Errorf("expected default %f, got %f", defaultSegmentDuration, dur)
	}
}

func TestEstimateSegmentCount(t *testing.T) {
	count := estimateSegmentCount(10.0)
	if count != 5 { // ceil(10 / 2) = 5
		t.Errorf("expected 5, got %d", count)
	}
	count = estimateSegmentCount(0)
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
