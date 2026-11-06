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
	"testing"
	"time"
)

// adBreakPlaylistServer serves five 2 s segments from 2026-07-01T00:00:00Z,
// the third and fourth a stitched-ad break, ENDLIST at the end.
func adBreakPlaylistServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-DATERANGE:ID="stitched-ad-9",CLASS="twitch-stitched-ad",START-DATE="2026-07-01T00:00:04.000Z",DURATION=4.000` + "\n")
		for i := range 5 {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:%02d.000Z\n", i*2)
			name := fmt.Sprintf("seg%d", i)
			if i == 2 || i == 3 {
				name = fmt.Sprintf("ad%d", i)
			}
			fmt.Fprintf(&b, "#EXTINF:2.000,live\n%s.ts\n", name)
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

// TestHlsLive_SplitOnAdBreak is owner decision D17 follow-up 3's capture
// side: with SplitOnAdBreak the live loop ends the part at the first content
// segment after a stitched-ad break, that segment unwritten and the position
// left on it, so a successor seeded there starts the next part with it; and
// OnSegmentWritten reports each written segment's program date-time and
// duration, the span the gap-fill maps onto the archive VOD.
//
// Mutants: the split not taken (ErrAdBreakEnded never returned, the file
// runs on to seg4); the position advanced past the segment (CurrentSeq 5);
// OnSegmentWritten not called (no spans reported).
func TestHlsLive_SplitOnAdBreak(t *testing.T) {
	srv := adBreakPlaylistServer(t)
	tmp := t.TempDir()
	type span struct {
		at  time.Time
		dur float64
	}
	var spans []span
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:          srv.URL + "/playlist.m3u8",
		OutputFile:       filepath.Join(tmp, "a"),
		StartSeq:         -1,
		IsHls:            true,
		StopOnGap:        true,
		SplitOnAdBreak:   true,
		OnSegmentWritten: func(at time.Time, dur float64) { spans = append(spans, span{at, dur}) },
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); !errors.Is(err, ErrAdBreakEnded) {
		t.Fatalf("Start = %v, want ErrAdBreakEnded", err)
	}
	if data, _ := os.ReadFile(filepath.Join(tmp, "a")); string(data) != "[seg0][seg1]" {
		t.Errorf("first part = %q, want [seg0][seg1]", data)
	}
	if got := d.CurrentSeq(); got != 4 {
		t.Errorf("CurrentSeq = %d, want 4 (on the first content segment after the break)", got)
	}
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if len(spans) != 2 || !spans[0].at.Equal(t0) || !spans[1].at.Equal(t0.Add(2*time.Second)) || spans[1].dur != 2 {
		t.Errorf("OnSegmentWritten reported %+v, want 00:00 and 00:02, 2 s each", spans)
	}

	// The successor, seeded at that position, starts the next part with it.
	next := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/playlist.m3u8", OutputFile: filepath.Join(tmp, "b"),
		StartSeq: d.CurrentSeq(), ForceStartSeq: true, IsHls: true, StopOnGap: true, SplitOnAdBreak: true,
	})
	if err := next.Start(ctx); err != nil {
		t.Fatalf("successor Start = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(tmp, "b")); string(data) != "[seg4]" {
		t.Errorf("second part = %q, want [seg4]", data)
	}
}
