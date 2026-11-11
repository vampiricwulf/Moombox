package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEndOfStreamMargin pins the margin and what it counts from: the LAST
// WRITTEN segment. The owner's captures stopped with segment 11017 written,
// currentSeq 11018 and head 11019 — two short, and inside the margin.
//
// Mutants: the constant at 1 (head-2 falls outside) or 3 (head-3 falls
// inside); comparing currentSeq in place of the last written segment.
func TestEndOfStreamMargin(t *testing.T) {
	if EndOfStreamHeadMargin != 2 {
		t.Fatalf("EndOfStreamHeadMargin = %d, want 2 (owner ruling R-T1)", EndOfStreamHeadMargin)
	}
	const head = 11019
	for _, tc := range []struct {
		last int
		want bool
	}{
		{head, true},
		{head - 1, true},
		{head - 2, true}, // the owner's captures: last written 11017
		{head - 3, false},
		{head - 100, false},
	} {
		if got := WithinEndOfStreamMargin(tc.last, head); got != tc.want {
			t.Errorf("WithinEndOfStreamMargin(last=%d, head=%d) = %v, want %v", tc.last, head, got, tc.want)
		}
	}
}

// TestFinalizeBehindHeadLatchesWhateverTheMargin pins the engine's half of
// the split: it decides nothing by the margin. Every stop below the head
// latches FinalizedBehindHead (so the worker has the evidence and the sidecar
// is kept), and only the line differs — the unfetched-tail Warn beyond the
// margin, an Info naming the margin inside it. A downloader at the head
// (currentSeq == head: the last written is head-1) is not behind, as before.
//
// Mutants: skip the latch inside the margin (the head-2 row reads not
// behind, and the engine has decided the recording complete on its own);
// Warn inside the margin; drop the Warn beyond it.
func TestFinalizeBehindHeadLatchesWhateverTheMargin(t *testing.T) {
	const head = 11019
	for _, tc := range []struct {
		name       string
		last       int
		wantBehind bool
		wantWarn   bool
	}{
		{"last == head", head, false, false},
		{"last == head-1", head - 1, false, false},
		{"last == head-2", head - 2, true, false},
		{"last == head-3", head - 3, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &secretLogger{}
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:    "http://127.0.0.1:1/videoplayback?itag=140",
				OutputFile: filepath.Join(t.TempDir(), "v"),
				Logger:     log,
			})
			d.currentSeq.Store(int64(tc.last + 1))
			d.headSeq.Store(head)
			if got := d.LastSeq(); got != tc.last {
				t.Fatalf("LastSeq = %d, want %d: currentSeq is the next segment to fetch", got, tc.last)
			}

			if got := d.finalizeBehindHead(); got != tc.wantBehind {
				t.Errorf("finalizeBehindHead = %v, want %v", got, tc.wantBehind)
			}
			if got := d.FinalizedBehindHead(); got != tc.wantBehind {
				t.Errorf("FinalizedBehindHead = %v, want %v", got, tc.wantBehind)
			}
			log.mu.Lock()
			lines := strings.Join(log.lines, "\n")
			log.mu.Unlock()
			if got := strings.Contains(lines, "WARN ") && strings.Contains(lines, "unfetched tail"); got != tc.wantWarn {
				t.Errorf("unfetched-tail Warn logged = %v, want %v; lines:\n%s", got, tc.wantWarn, lines)
			}
			wantInfo := tc.wantBehind && !tc.wantWarn
			if got := strings.Contains(lines, "INFO ") && strings.Contains(lines, "within the end-of-stream margin"); got != wantInfo {
				t.Errorf("end-of-stream margin Info logged = %v, want %v; lines:\n%s", got, wantInfo, lines)
			}
		})
	}
}

// TestHeadReadSinceLastSegment pins the freshness the worker's end-of-stream
// verdict asks of the head. A broadcast that is over keeps answering the
// retries past its last segment with X-Head-Seqnum, so its head is current. A
// stall in which nothing past the last segment answers with the header leaves
// the head at its value from before the stall, and it must not read as
// current: the broadcast may have run on.
//
// Mutants: drop the stamp in noteHeadSeqFromResponse (the ended row reads
// stale); stamp on lastSegTime's side, or compare the other way round (the
// stalled row reads current); report true when no head was ever read.
func TestHeadReadSinceLastSegment(t *testing.T) {
	const head, lastServed = 8, 6
	for _, tc := range []struct {
		name            string
		headerPastTail  bool
		wantHeadCurrent bool
	}{
		{"an ended broadcast: the retries still carry the head", true, true},
		{"a stall: nothing past the last segment carries the head", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sq, err := strconv.Atoi(r.URL.Query().Get("sq"))
				if err == nil && sq <= lastServed {
					w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
					fmt.Fprintf(w, "seg-%05d;", sq)
					return
				}
				if tc.headerPastTail {
					w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()

			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:           srv.URL + "/videoplayback?itag=140",
				OutputFile:        filepath.Join(t.TempDir(), "v"),
				MaxTimeout:        300 * time.Millisecond,
				CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
			})
			d.SetFastDelaysForTests(20)
			done := make(chan error, 1)
			go func() { done <- d.Start(context.Background()) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Start = %v, want nil", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the downloader did not finalize")
			}
			if d.LastSeq() != lastServed || d.HeadSeq() != head {
				t.Fatalf("test setup invalid: LastSeq = %d, HeadSeq = %d, want %d and %d", d.LastSeq(), d.HeadSeq(), lastServed, head)
			}
			if got := d.HeadReadSinceLastSegment(); got != tc.wantHeadCurrent {
				t.Errorf("HeadReadSinceLastSegment = %v, want %v", got, tc.wantHeadCurrent)
			}
		})
	}

	fresh := NewSegmentDownloader(DownloaderOptions{BaseURL: "http://127.0.0.1:1/videoplayback?itag=140", OutputFile: filepath.Join(t.TempDir(), "v")})
	if fresh.HeadReadSinceLastSegment() {
		t.Error("a downloader that never read a head reports it current")
	}
}
