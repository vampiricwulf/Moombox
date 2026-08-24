package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseHls_ExtXMapPerSegment pins per-segment EXT-X-MAP attribution: the
// tag applies to every following segment until the next EXT-X-MAP, and URIs
// resolve against the playlist URL. TS playlists (no tag) must yield empty
// MapURI so the downloader's fMP4 handling stays a no-op for them.
func TestParseHls_ExtXMapPerSegment(t *testing.T) {
	playlist := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-TARGETDURATION:2",
		"#EXT-X-MEDIA-SEQUENCE:5",
		`#EXT-X-MAP:URI="init1.mp4"`,
		"#EXTINF:2.0,",
		"seg5.mp4",
		"#EXTINF:2.0,",
		"seg6.mp4",
		`#EXT-X-MAP:URI="https://other.example/init2.mp4"`,
		"#EXTINF:2.0,",
		"seg7.mp4",
	}, "\n")

	result := ParseHls(playlist, "https://cdn.example/path/playlist.m3u8")
	if result == nil || result.Playlist == nil {
		t.Fatal("ParseHls returned nil")
	}
	segs := result.Playlist.Segments
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(segs))
	}
	if want := "https://cdn.example/path/init1.mp4"; segs[0].MapURI != want {
		t.Errorf("seg5 MapURI = %q, want %q", segs[0].MapURI, want)
	}
	if want := "https://cdn.example/path/init1.mp4"; segs[1].MapURI != want {
		t.Errorf("seg6 MapURI = %q, want %q", segs[1].MapURI, want)
	}
	if want := "https://other.example/init2.mp4"; segs[2].MapURI != want {
		t.Errorf("seg7 MapURI = %q, want %q", segs[2].MapURI, want)
	}

	tsPlaylist := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\nseg0.ts\n"
	tsResult := ParseHls(tsPlaylist, "https://cdn.example/playlist.m3u8")
	if tsResult == nil || tsResult.Playlist == nil || len(tsResult.Playlist.Segments) != 1 {
		t.Fatal("TS playlist parse failed")
	}
	if got := tsResult.Playlist.Segments[0].MapURI; got != "" {
		t.Errorf("TS segment MapURI = %q, want empty", got)
	}
}

// fmp4TestServer serves an HLS playlist whose body is produced by playlistFn
// (called with the 1-based refresh count), init segments by literal body from
// the inits map, and media segments as "[name]". Fetch counts per init path
// are tracked.
func fmp4TestServer(t *testing.T, playlistFn func(refresh int32) string, inits map[string]string) (*httptest.Server, *atomic.Int32, func(path string) int32) {
	t.Helper()
	var refresh atomic.Int32
	var initFetches map[string]*atomic.Int32 = make(map[string]*atomic.Int32)
	for p := range inits {
		initFetches[p] = &atomic.Int32{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, playlistFn(refresh.Add(1)))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if body, ok := inits[path]; ok {
			initFetches[path].Add(1)
			fmt.Fprint(w, body)
			return
		}
		name := strings.TrimSuffix(path, ".mp4")
		fmt.Fprintf(w, "[%s]", name)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &refresh, func(path string) int32 {
		c, ok := initFetches[path]
		if !ok {
			return 0
		}
		return c.Load()
	}
}

// TestHlsLive_FMP4InitWrittenFirst drives the live sequential path (StopOnGap,
// the Twitch configuration) through an fMP4 playlist: the EXT-X-MAP init
// segment must be fetched once and written BEFORE the first media segment, or
// the output is headerless fragments ffmpeg cannot demux.
func TestHlsLive_FMP4InitWrittenFirst(t *testing.T) {
	srv, _, initCount := fmp4TestServer(t, func(int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
		b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
		b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}, map[string]string{"init0.mp4": "(INIT)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(INIT)[seg0][seg1]"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if n := initCount("init0.mp4"); n != 1 {
		t.Errorf("init fetched %d times, want 1", n)
	}
}

// TestHlsLive_FMP4InitChangeSplitsPart pins the part-split contract: when the
// playlist's EXT-X-MAP changes to an init with DIFFERENT content mid-part
// (transcode restart), the loop must return ErrInitSegmentChanged without
// writing anything from the new init's era — appending fragments that
// reference a different moov corrupts the file. CurrentSeq must sit at the
// first unwritten segment so the successor part starts exactly there.
func TestHlsLive_FMP4InitChangeSplitsPart(t *testing.T) {
	srv, _, _ := fmp4TestServer(t, func(refresh int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n")
		if refresh == 1 {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
			b.WriteString(`#EXT-X-MAP:URI="init-a.mp4"` + "\n")
			b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		} else {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:2\n")
			b.WriteString(`#EXT-X-MAP:URI="init-b.mp4"` + "\n")
			b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg3.mp4\n")
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		return b.String()
	}, map[string]string{"init-a.mp4": "(A)", "init-b.mp4": "(B)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if !errors.Is(err, ErrInitSegmentChanged) {
		t.Fatalf("Start = %v, want ErrInitSegmentChanged", err)
	}

	data, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(data), "(A)[seg0][seg1]"; got != want {
		t.Errorf("output = %q, want %q (nothing from the new init's era may be appended)", got, want)
	}
	if got := d.CurrentSeq(); got != 2 {
		t.Errorf("CurrentSeq() = %d, want 2 (successor part must start at the first unwritten segment)", got)
	}
}

// TestHlsLive_FMP4InitURLRotatedSameContent pins the false-split guard: Twitch
// URLs rotate tokens, so a CHANGED map URI whose fetched content is byte-
// identical to the init already written must NOT split — the download adopts
// the new URI and continues appending.
func TestHlsLive_FMP4InitURLRotatedSameContent(t *testing.T) {
	srv, _, _ := fmp4TestServer(t, func(refresh int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n")
		if refresh == 1 {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
			b.WriteString(`#EXT-X-MAP:URI="init-a.mp4"` + "\n")
			b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		} else {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:2\n")
			b.WriteString(`#EXT-X-MAP:URI="init-b.mp4"` + "\n")
			b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg3.mp4\n")
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		return b.String()
	}, map[string]string{"init-a.mp4": "(SAME)", "init-b.mp4": "(SAME)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v (a rotated same-content init URL must not split)", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(SAME)[seg0][seg1][seg2][seg3]"; got != want {
		t.Errorf("output = %q, want %q (init written exactly once)", got, want)
	}
}

// TestHlsVod_FMP4InitWrittenFirst drives the VOD parallel path (ENDLIST, no
// StopOnGap): the init must land at the head of the file before the in-order
// segment flush.
func TestHlsVod_FMP4InitWrittenFirst(t *testing.T) {
	srv, _, initCount := fmp4TestServer(t, func(int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
		for i := 0; i < 4; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.000,\nseg%d.mp4\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}, map[string]string{"init0.mp4": "(VODINIT)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(VODINIT)[seg0][seg1][seg2][seg3]"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if n := initCount("init0.mp4"); n != 1 {
		t.Errorf("init fetched %d times, want 1", n)
	}
}

// TestHlsLive_FMP4StuckSegmentSkipsNotSplitCycle pins the empty-file gap
// rule on fMP4: a successor part whose FIRST segment is permanently failing
// must not have an init written before any media lands — the file must stay
// genuinely empty so stuck-segment exhaustion takes the skip-and-record-gap
// branch (one OnGap, recording continues) instead of returning ErrGapDetected
// and split-cycling init-only junk parts.
func TestHlsLive_FMP4StuckSegmentSkipsNotSplitCycle(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
		b.WriteString("#EXTINF:1.000,live\ndead0.mp4\n")
		b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	var initFetches atomic.Int32
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case path == "init0.mp4":
			initFetches.Add(1)
			fmt.Fprint(w, "(I)")
		case strings.HasPrefix(path, "dead"):
			http.Error(w, "gone", http.StatusNotFound)
		default:
			fmt.Fprintf(w, "[%s]", strings.TrimSuffix(path, ".mp4"))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) { gaps = append(gaps, g) }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v (ErrGapDetected here means the init write defeated the empty-file skip rule)", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(I)[seg1][seg2]"; got != want {
		t.Errorf("output = %q, want %q (init once, then post-gap media)", got, want)
	}
	if len(gaps) != 1 || gaps[0].From != 0 || gaps[0].To != 0 {
		t.Errorf("gaps = %+v, want exactly one {From:0, To:0}", gaps)
	}
	if n := initFetches.Load(); n != 1 {
		t.Errorf("init fetched %d times, want 1", n)
	}
}

// TestHlsLive_FMP4RevertsToTSSplitsPart pins the reversion guard: a playlist
// that stops carrying EXT-X-MAP while the file already starts with an init
// segment (Twitch rolling its fMP4 experiment back mid-broadcast) must
// part-split under StopOnGap — appending raw TS after ISOBMFF fragments
// silently corrupts the part tail.
func TestHlsLive_FMP4RevertsToTSSplitsPart(t *testing.T) {
	srv, _, _ := fmp4TestServer(t, func(refresh int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n")
		if refresh == 1 {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
			b.WriteString(`#EXT-X-MAP:URI="init-a.mp4"` + "\n")
			b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		} else {
			// Reverted to TS: no EXT-X-MAP at all.
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:2\n")
			b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		return b.String()
	}, map[string]string{"init-a.mp4": "(A)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if !errors.Is(err, ErrInitSegmentChanged) {
		t.Fatalf("Start = %v, want ErrInitSegmentChanged (TS after fMP4 must split, not append)", err)
	}
	data, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(data), "(A)[seg0][seg1]"; got != want {
		t.Errorf("output = %q, want %q (no TS bytes may follow the fMP4 part)", got, want)
	}
	if got := d.CurrentSeq(); got != 2 {
		t.Errorf("CurrentSeq() = %d, want 2", got)
	}
}

// TestHls_FMP4LegacySidecarSplitsInsteadOfAppending is the update-path
// compliance pin: a staged file recorded by a PRE-fMP4 binary (sidecar has no
// init fields, file content is whatever the old binary wrote) resumed by this
// binary against an fMP4 playlist must split — the file's init state is
// unknown, so appending fragments blind is never safe.
func TestHls_FMP4LegacySidecarSplitsInsteadOfAppending(t *testing.T) {
	srv, _, _ := fmp4TestServer(t, func(int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:2\n")
		b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
		b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}, map[string]string{"init0.mp4": "(I)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	legacy := []byte("OLDBINARYDATA")
	if err := os.WriteFile(outFile, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	// Legacy sidecar shape: no init fields.
	sidecar := fmt.Sprintf(`{"lastSeq":1,"bytesWritten":%d,"timestamp":%d,"baseUrl":%q,"streamId":"12345"}`,
		len(legacy), time.Now().Unix(), srv.URL+"/playlist.m3u8")
	if err := os.WriteFile(outFile+".resume.json", []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
		StreamID:   "12345",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if !errors.Is(err, ErrInitSegmentChanged) {
		t.Fatalf("Start = %v, want ErrInitSegmentChanged (unknown init state must split)", err)
	}
	data, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(legacy) {
		t.Errorf("staged file was modified: %q (nothing may be appended to a legacy part)", data)
	}
}

// TestHlsLive_FMP4AdBreakOwnInitNoSplit pins the ordering that keeps a
// stitched-ad break's OWN EXT-X-MAP inert: ad segments are skipped before the
// init logic runs, so the ad's init is never fetched, never written, and never
// read as an init change when content resumes.
func TestHlsLive_FMP4AdBreakOwnInitNoSplit(t *testing.T) {
	srv, _, initCount := fmp4TestServer(t, func(int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-DATERANGE:ID="stitched-ad-1",CLASS="twitch-stitched-ad",START-DATE="2026-07-01T00:00:02.000Z",DURATION=4.000` + "\n")
		b.WriteString(`#EXT-X-MAP:URI="content-init.mp4"` + "\n")
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:00.000Z\n#EXTINF:2.000,live\nseg0.mp4\n")
		b.WriteString(`#EXT-X-MAP:URI="ad-init.mp4"` + "\n")
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:02.000Z\n#EXTINF:2.000,Amazon\nad1.mp4\n")
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:04.000Z\n#EXTINF:2.000,Amazon\nad2.mp4\n")
		b.WriteString(`#EXT-X-MAP:URI="content-init.mp4"` + "\n")
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:06.000Z\n#EXTINF:2.000,live\nseg3.mp4\n")
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}, map[string]string{"content-init.mp4": "(C)", "ad-init.mp4": "(AD)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v (an ad break's own init must not read as an init change)", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(C)[seg0][seg3]"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if n := initCount("ad-init.mp4"); n != 0 {
		t.Errorf("ad init fetched %d times, want 0", n)
	}
	if n := initCount("content-init.mp4"); n != 1 {
		t.Errorf("content init fetched %d times, want 1", n)
	}
}

// TestHls_FMP4ResumeDoesNotRewriteInit pins the resume contract: a fresh
// downloader continuing the same staged file (same-quality recovery / daemon
// restart, resume sidecar present) must know the init is already at the head
// of the file — re-fetching or re-writing it would splice a second ftyp/moov
// mid-file.
func TestHls_FMP4ResumeDoesNotRewriteInit(t *testing.T) {
	var phase atomic.Int32 // 1 = pre-interrupt window, 2 = post-resume window
	phase.Store(1)
	srv, _, initCount := fmp4TestServer(t, func(int32) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
		b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
		b.WriteString("#EXTINF:1.000,live\nseg1.mp4\n")
		if phase.Load() == 2 {
			b.WriteString("#EXTINF:1.000,live\nseg2.mp4\n")
			b.WriteString("#EXTINF:1.000,live\nseg3.mp4\n")
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		return b.String()
	}, map[string]string{"init0.mp4": "(I)"})

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	opts := DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
		StreamID:   "12345",
	}

	// First session: record seg0+seg1, then get interrupted.
	d1 := NewSegmentDownloader(opts)
	d1.OnProgress = func(p DownloadProgress) {
		if p.Seq >= 1 {
			d1.Cancel()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d1.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Start = %v, want context.Canceled", err)
	}

	// Second session: same staging, resume sidecar present, stream continues.
	phase.Store(2)
	d2 := NewSegmentDownloader(opts)
	if err := d2.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "(I)[seg0][seg1][seg2][seg3]"; got != want {
		t.Errorf("output = %q, want %q (init must not be re-written on resume)", got, want)
	}
	if n := initCount("init0.mp4"); n != 1 {
		t.Errorf("init fetched %d times across resume, want 1 (unchanged URI needs no re-fetch)", n)
	}
}
