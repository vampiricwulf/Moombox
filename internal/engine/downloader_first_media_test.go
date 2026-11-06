package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// TestDashLiveReportsTheFirstWrittenSequence pins OnFirstMedia on the live
// DASH loop: a file the downloader opened empty reports, once, the sequence
// of the first media segment written into it — 0 for a manifest-free start
// at the broadcast's head, the segment the first-segment hunt landed on when
// the head below it is evicted, and the same from the catch-up writer when
// the downloader starts far behind the live edge. A resumed file, which began
// before this downloader, reports nothing. The worker records it beside the
// staged file to tell a from-start recording from a late one (FE-18).
//
// Mutants: the report made for every written segment (more than one report);
// firstMediaWritten dropped from the sequential write (the first two rows
// report nothing); dropped from the catch-up write (the far-behind row
// reports nothing); armed on a resume too (the resumed row reports).
func TestDashLiveReportsTheFirstWrittenSequence(t *testing.T) {
	for _, tc := range []struct {
		name              string
		head              int
		initInlineAtStart bool
		refused           func(seq int) bool
		resumeAt          int // >0: resume a file holding 0..resumeAt-1
		want              []int
	}{
		{name: "manifest-free start at sq=0", head: 4, initInlineAtStart: true, want: []int{0}},
		{name: "hunt past an evicted head", head: 8, refused: func(seq int) bool { return seq <= 2 }, want: []int{3}},
		{name: "far behind: the catch-up writes first", head: 60, initInlineAtStart: true, want: []int{0}},
		{name: "a resume reports nothing", head: 6, initInlineAtStart: true, resumeAt: 3, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveGVS(t, constHead(tc.head), func(r *http.Request, seq, attempt int) gvsAnswer {
				switch {
				case tc.refused != nil && tc.refused(seq):
					return gvsGone
				case seq <= tc.head:
					return gvsServe
				default:
					return gvsForbidden // past the end
				}
			})
			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video_stream")
				base := gvsBase("first")
				if tc.resumeAt > 0 {
					held := gvsRecording(0, tc.resumeAt-1)
					if err := os.WriteFile(out, held, 0o644); err != nil {
						t.Fatal(err)
					}
					state, _ := json.Marshal(ResumeState{LastSeq: tc.resumeAt - 1, BytesWritten: int64(len(held)), Timestamp: time.Now().Unix(), BaseURL: base})
					if err := os.WriteFile(out+resumeFileSuffix, state, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				var mu sync.Mutex
				var got []int
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:           base,
					OutputFile:        out,
					InitInlineAtStart: tc.initInlineAtStart,
					CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
				})
				d.OnFirstMedia = func(seq int) {
					mu.Lock()
					got = append(got, seq)
					mu.Unlock()
				}
				if err := d.Start(t.Context()); err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("OnFirstMedia reports = %v, want %v", got, tc.want)
				}
			})
		})
	}
}

// TestHlsLiveReportsTheFirstWrittenSequence: the live HLS loop reports the
// first written segment's media sequence beside its date-time — the first
// content segment after the stitched ads (2), or the seeded position of a
// successor with no file yet (3) — and nothing for an append.
//
// Mutant: the HLS loop disarming the report itself instead of through
// firstMediaWritten (the fresh rows report nothing).
func TestHlsLiveReportsTheFirstWrittenSequence(t *testing.T) {
	srv := firstSegmentPlaylist(t, true)
	run := func(outFile string, startSeq int, force bool) []int {
		var mu sync.Mutex
		var got []int
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:       srv.URL + "/playlist.m3u8",
			OutputFile:    outFile,
			StartSeq:      startSeq,
			ForceStartSeq: force,
			IsHls:         true,
			StopOnGap:     true,
		})
		d.OnFirstMedia = func(seq int) {
			mu.Lock()
			got = append(got, seq)
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
	if got := run(filepath.Join(dir, "fresh"), -1, false); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("fresh file reports %v, want [2] (the first content segment)", got)
	}
	if got := run(filepath.Join(dir, "successor"), 3, true); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("successor reports %v, want [3]", got)
	}
	appendTo := filepath.Join(dir, "append")
	if err := os.WriteFile(appendTo, []byte("[seg2]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(appendTo, 3, true); len(got) != 0 {
		t.Errorf("an append reports %v, want nothing", got)
	}
}
