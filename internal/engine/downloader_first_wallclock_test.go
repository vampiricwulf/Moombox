package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// TestSegmentIngestWalltime reads the Ingestion-Walltime-Us a live YouTube
// segment leads with. The first row is capture B's sq=0 as measured
// (2026-11-27, owner ruling RV-19): ingested 23:29:04.764160. A segment with
// no such key, a value that is not a positive whole number, or one past the
// scanned head reads as the zero time.
//
// Mutants: the value read from First-Frame-Time-Us (row 1 reads 23:29:10);
// the scan unbounded (the far row reads a time); a zero or negative value
// accepted.
func TestSegmentIngestWalltime(t *testing.T) {
	meta := func(lines string) []byte {
		return append(mp4box("emsg", []byte(lines)), gvsFrag(0)...)
	}
	b := meta("Sequence-Number: 0\r\nIngestion-Walltime-Us: 1795822144764160\r\nFirst-Frame-Time-Us: 1795822150413000\r\n")
	if got, want := segmentIngestWalltime(b), time.Date(2026, 11, 27, 23, 29, 4, 764160000, time.UTC); !got.Equal(want) {
		t.Errorf("capture B's sq=0 = %v, want %v", got, want)
	}
	withInit := append(gvsInit(), meta("Ingestion-Walltime-Us: 1795820210408302\n")...)
	if got, want := segmentIngestWalltime(withInit), time.UnixMicro(1795820210408302).UTC(); !got.Equal(want) {
		t.Errorf("after an inline init = %v, want %v", got, want)
	}
	far := append(bytes.Repeat([]byte{0}, ingestWalltimeScan), []byte("Ingestion-Walltime-Us: 1795820210408302\n")...)
	for name, data := range map[string][]byte{
		"none":        gvsFrag(3),
		"empty":       nil,
		"no digits":   meta("Ingestion-Walltime-Us: soon\n"),
		"zero":        meta("Ingestion-Walltime-Us: 0\n"),
		"negative":    meta("Ingestion-Walltime-Us: -5\n"),
		"overflow":    meta("Ingestion-Walltime-Us: 99999999999999999999999\n"),
		"past scan":   far,
		"first-frame": meta("First-Frame-Time-Us: 1795822150413000\n"),
	} {
		if got := segmentIngestWalltime(data); !got.IsZero() {
			t.Errorf("%s: read %v, want the zero time", name, got)
		}
	}
}

// gvsWalltime is the ingest wall clock gvsTimedMedia gives sequence seq: five
// seconds a segment from capture B's sq=0.
func gvsWalltime(seq int) time.Time {
	return time.UnixMicro(1795822144764160 + int64(seq)*5_000_000).UTC()
}

// gvsTimedMedia is gvsMedia with the leading metadata a live YouTube segment
// carries: sq=0's init, then the metadata box, then the fragment.
func gvsTimedMedia(seq int) []byte {
	m := mp4box("emsg", fmt.Appendf(nil, "Sequence-Number: %d\r\nIngestion-Walltime-Us: %d\r\n", seq, gvsWalltime(seq).UnixMicro()))
	if seq == 0 {
		return append(append(gvsInit(), m...), gvsFrag(0)...)
	}
	return append(m, gvsFrag(seq)...)
}

// TestDashLiveReportsTheFirstWrittenWallclock: beside OnFirstMedia's
// sequence, the live DASH loop reports, once, the ingest wall clock of the
// first segment it writes into a file it opened empty — the archive's time
// zero (owner ruling RV-19): sq=0's on a start from the head, the segment the
// first-segment hunt landed on past an evicted head, and the catch-up
// writer's first when the downloader starts far behind. A resume reports
// nothing (its file began earlier), and a stream whose segments carry no
// metadata reports nothing.
//
// Mutants: the wall clock read from every written segment (more than one
// report); the sequential write passing no data (the first two rows report
// nothing); the catch-up write passing none (the far-behind row reports
// nothing); the report made on a resume.
func TestDashLiveReportsTheFirstWrittenWallclock(t *testing.T) {
	for _, tc := range []struct {
		name              string
		head              int
		initInlineAtStart bool
		refused           func(seq int) bool
		resumeAt          int
		untimed           bool
		want              []time.Time
	}{
		{name: "start at sq=0", head: 4, initInlineAtStart: true, want: []time.Time{gvsWalltime(0)}},
		{name: "hunt past an evicted head", head: 8, refused: func(seq int) bool { return seq <= 2 }, want: []time.Time{gvsWalltime(3)}},
		{name: "far behind: the catch-up writes first", head: 60, initInlineAtStart: true, want: []time.Time{gvsWalltime(0)}},
		{name: "a resume reports nothing", head: 6, initInlineAtStart: true, resumeAt: 3},
		{name: "segments without metadata report nothing", head: 4, initInlineAtStart: true, untimed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := serveGVS(t, constHead(tc.head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case tc.refused != nil && tc.refused(seq):
					return gvsGone
				case seq <= tc.head:
					return gvsServe
				default:
					return gvsForbidden
				}
			})
			if !tc.untimed {
				g.body = gvsTimedMedia
			}
			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "audio_stream")
				base := gvsBase("wall")
				if tc.resumeAt > 0 {
					var held []byte
					for seq := 0; seq < tc.resumeAt; seq++ {
						held = append(held, gvsTimedMedia(seq)...)
					}
					if err := os.WriteFile(out, held, 0o644); err != nil {
						t.Fatal(err)
					}
					state, _ := json.Marshal(ResumeState{LastSeq: tc.resumeAt - 1, BytesWritten: int64(len(held)), Timestamp: time.Now().Unix(), BaseURL: base})
					if err := os.WriteFile(out+resumeFileSuffix, state, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				var mu sync.Mutex
				var got []time.Time
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:           base,
					OutputFile:        out,
					InitInlineAtStart: tc.initInlineAtStart,
					CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
				})
				d.OnFirstMediaWallclock = func(wall time.Time) {
					mu.Lock()
					got = append(got, wall)
					mu.Unlock()
				}
				if err := d.Start(t.Context()); err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("OnFirstMediaWallclock reports = %v, want %v", got, tc.want)
				}
			})
		})
	}
}

// TestHlsReportsTheFirstWrittenWallclock: the HLS paths report the first
// written segment's #EXT-X-PROGRAM-DATE-TIME as its wall clock — the live
// loop's first content segment after the stitched ads (12:00:04), and the
// VOD-parallel writer's first (12:00:00 at sq=7) — and nothing for a
// playlist with no date-time or for an append.
//
// Mutants: the live loop passing a zero date-time (its row reports nothing);
// the VOD-parallel writer passing a zero one (its row reports nothing).
func TestHlsReportsTheFirstWrittenWallclock(t *testing.T) {
	run := func(base, out string, startSeq int, force, stopOnGap bool) []time.Time {
		var mu sync.Mutex
		var got []time.Time
		d := NewSegmentDownloader(DownloaderOptions{BaseURL: base, OutputFile: out, StartSeq: startSeq, ForceStartSeq: force, IsHls: true, StopOnGap: stopOnGap})
		d.OnFirstMediaWallclock = func(wall time.Time) {
			mu.Lock()
			got = append(got, wall)
			mu.Unlock()
		}
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
	dir := t.TempDir()
	live := firstSegmentPlaylist(t, true).URL + "/playlist.m3u8"
	if got, want := run(live, filepath.Join(dir, "live"), -1, false, true), []time.Time{time.Date(2026, 10, 8, 12, 0, 4, 0, time.UTC)}; !reflect.DeepEqual(got, want) {
		t.Errorf("live loop reports %v, want %v", got, want)
	}
	if got := run(firstSegmentPlaylist(t, false).URL+"/playlist.m3u8", filepath.Join(dir, "undated"), -1, false, true); len(got) != 0 {
		t.Errorf("a playlist without date-times reports %v, want nothing", got)
	}
	appendTo := filepath.Join(dir, "append")
	if err := os.WriteFile(appendTo, []byte("[seg2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(live, appendTo, 3, true, true); len(got) != 0 {
		t.Errorf("an append reports %v, want nothing", got)
	}

	vod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Ext(r.URL.Path) == ".m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:7\n"+
				"#EXT-X-PROGRAM-DATE-TIME:2026-11-27T22:56:50.408Z\n#EXTINF:2.000,\nseg7.ts\n#EXTINF:2.000,\nseg8.ts\n#EXT-X-ENDLIST\n")
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	t.Cleanup(vod.Close)
	if got, want := run(vod.URL+"/v.m3u8", filepath.Join(dir, "video.ts"), -1, false, false), []time.Time{time.Date(2026, 11, 27, 22, 56, 50, 408_000_000, time.UTC)}; !reflect.DeepEqual(got, want) {
		t.Errorf("VOD-parallel writer reports %v, want %v", got, want)
	}
}
