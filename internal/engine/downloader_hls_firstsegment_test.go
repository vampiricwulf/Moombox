package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseHlsProgramDateTimePerSegment: every segment carries its own
// wall-clock time — its tag when it has one, else the last tag advanced by
// the durations in between — and none when the playlist has no PDT at all.
//
// Mutants: ProgramDateTime left unset in parseMediaPlaylist (every segment is
// zero); the PDT taken AFTER the segment's own duration is added (each segment
// reads as the next one's start).
func TestParseHlsProgramDateTimePerSegment(t *testing.T) {
	const pl = "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:10\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-10-08T12:00:00.250Z\n#EXTINF:2.000,live\na.ts\n" +
		"#EXTINF:2.000,live\nb.ts\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-10-08T12:00:09.000Z\n#EXTINF:2.000,live\nc.ts\n"
	res := ParseHls(pl, "https://example.com/x.m3u8")
	if res == nil || res.Playlist == nil || len(res.Playlist.Segments) != 3 {
		t.Fatalf("parsed %+v, want three segments", res)
	}
	base := time.Date(2026, 10, 8, 12, 0, 0, 250_000_000, time.UTC)
	for i, want := range []time.Time{base, base.Add(2 * time.Second), time.Date(2026, 10, 8, 12, 0, 9, 0, time.UTC)} {
		if got := res.Playlist.Segments[i].ProgramDateTime; !got.Equal(want) {
			t.Errorf("segment %d ProgramDateTime = %v, want %v", i, got, want)
		}
	}

	bare := ParseHls("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.000,\na.ts\n", "https://example.com/x.m3u8")
	if got := bare.Playlist.Segments[0].ProgramDateTime; !got.IsZero() {
		t.Errorf("a playlist without PDT tags dated its segment %v, want zero", got)
	}
}

// firstSegmentPlaylist serves one window: two stitched-ad segments, then three
// content segments, all dated by ONE leading PDT tag, then ENDLIST.
func firstSegmentPlaylist(t *testing.T, withPDT bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		if withPDT {
			b.WriteString(`#EXT-X-DATERANGE:ID="stitched-ad-1",CLASS="twitch-stitched-ad",START-DATE="2026-10-08T12:00:00.000Z",DURATION=4.000` + "\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-10-08T12:00:00.000Z\n")
		}
		for i := range 5 {
			fmt.Fprintf(&b, "#EXTINF:2.000,live\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runFirstSegment drives one live-configured downloader to the playlist's end
// and returns every OnFirstSegment report it made.
func runFirstSegment(t *testing.T, srv *httptest.Server, outFile string, startSeq int, force bool) []time.Time {
	t.Helper()
	var mu sync.Mutex
	var got []time.Time
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:       srv.URL + "/playlist.m3u8",
		OutputFile:    outFile,
		StartSeq:      startSeq,
		ForceStartSeq: force,
		IsHls:         true,
		StopOnGap:     true, // the Twitch live configuration
		OnFirstSegment: func(pdt time.Time) {
			mu.Lock()
			got = append(got, pdt)
			mu.Unlock()
		},
	})
	d.delays = fastDelays()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return got
}

// TestHlsLiveReportsTheFirstWrittenSegmentsTime is the engine half of D-T8: a
// fresh file reports, once, the wall-clock time of the first segment WRITTEN
// into it — the first frame of the file. The two stitched-ad segments at the
// head of the window are skipped, not written, so the report is the first
// content segment's time (12:00:04), dated from a single leading tag; a
// playlist without PDT reports the zero time, which is the caller's cue to
// keep its own clock.
//
// Mutants: the report made for every written segment (three reports); made
// before the ad skip, i.e. for the window's first listed segment (12:00:00);
// reportFirstSegment armed unconditionally (the append below reports).
func TestHlsLiveReportsTheFirstWrittenSegmentsTime(t *testing.T) {
	got := runFirstSegment(t, firstSegmentPlaylist(t, true), filepath.Join(t.TempDir(), "video_stream"), -1, false)
	want := time.Date(2026, 10, 8, 12, 0, 4, 0, time.UTC)
	if len(got) != 1 || !got[0].Equal(want) {
		t.Errorf("OnFirstSegment reports = %v, want exactly one, %v — the first content segment written", got, want)
	}

	got = runFirstSegment(t, firstSegmentPlaylist(t, false), filepath.Join(t.TempDir(), "video_stream"), -1, false)
	if len(got) != 1 || !got[0].IsZero() {
		t.Errorf("without PDT tags OnFirstSegment reports = %v, want exactly one zero time", got)
	}
}

// TestHlsLiveAppendReportsNoFirstSegment: a downloader appending to a file
// that already holds media — the same-quality recovery's ForceStartSeq, a
// resume — did not start that file, so it has no first frame to report. One
// seeded at a position with NO file yet (a split successor) starts its file
// and does report.
//
// Mutant: reportFirstSegment armed unconditionally in Start.
func TestHlsLiveAppendReportsNoFirstSegment(t *testing.T) {
	srv := firstSegmentPlaylist(t, true)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "video_stream")
	if err := os.WriteFile(outFile, []byte("[seg2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runFirstSegment(t, srv, outFile, 3, true); len(got) != 0 {
		t.Errorf("an append reported a first segment: %v", got)
	}

	fresh := filepath.Join(dir, "fresh_stream")
	got := runFirstSegment(t, srv, fresh, 3, true)
	if want := time.Date(2026, 10, 8, 12, 0, 6, 0, time.UTC); len(got) != 1 || !got[0].Equal(want) {
		t.Errorf("a successor seeded at seq 3 with no file reported %v, want exactly %v", got, want)
	}
}
