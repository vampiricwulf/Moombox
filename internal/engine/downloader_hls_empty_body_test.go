package engine

import (
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

// runEmptyBodyHls runs the live sequential HLS path (StopOnGap, as a Twitch
// live capture runs) against mux and returns the file, the gaps and Start's
// error.
func runEmptyBodyHls(t *testing.T, mux *http.ServeMux) (string, []DownloadGap, error) {
	t.Helper()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/vod.m3u8",
		OutputFile: out,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})
	d.delays = fastDelays()
	// The stuck budget is a count; the pause between rounds decides nothing.
	d.delays.hlsStuckRetry = 10 * time.Millisecond
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) { gaps = append(gaps, g) }
	startErr := d.Start(t.Context())
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), gaps, startErr
}

// A 2xx with no body is a failed fetch of that sequence in the live HLS loop,
// as it is on every other write path: the segment is asked for again after a
// playlist refresh. It used to be written as a zero-byte segment — the
// sequence advanced with nothing in the file, no gap row and no part split.
//
// Mutant: the empty-body test after fetchSegment removed — the file is
// "[seg0][seg2][seg3]" with no gap recorded.
func TestHlsLiveEmptyBodyIsRetriedNotWritten(t *testing.T) {
	var seg1Requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, vodPlaylist) })
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		if seg1Requests.Add(1) == 1 {
			w.WriteHeader(http.StatusOK) // 200, no body
			return
		}
		echoSegment(w, r)
	})
	mux.HandleFunc("/", echoSegment)

	data, gaps, err := runEmptyBodyHls(t, mux)
	checkWholeVod(t, data, gaps, err)
	if n := seg1Requests.Load(); n != 2 {
		t.Errorf("seg1 requested %d times, want 2 (the empty answer, then the retry)", n)
	}
}

// A segment that only ever answers with an empty body exhausts the stuck
// budget like one that only ever fails: under StopOnGap, with media already in
// the file, the part ends at it and the position does not move past it.
//
// Mutant: as above — Start returns nil and the file is "[seg0][seg2][seg3]".
func TestHlsLiveEmptyBodyStuckEndsThePart(t *testing.T) {
	var seg1Requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, vodPlaylist) })
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, _ *http.Request) {
		seg1Requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", echoSegment)

	data, gaps, err := runEmptyBodyHls(t, mux)
	if !errors.Is(err, ErrGapDetected) {
		t.Errorf("Start() = %v, want ErrGapDetected", err)
	}
	if data != "[seg0]" {
		t.Errorf("file holds %q, want only the segment before the empty one", data)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps %v, want none: the successor part records the gap", gaps)
	}
	if n := seg1Requests.Load(); n != MaxSegmentRetries {
		t.Errorf("seg1 requested %d times, want %d", n, MaxSegmentRetries)
	}
}

// An empty body never counts as a media fetch on fMP4: the init segment is
// fetched only once real media is in hand, so a part whose first segment only
// answers empty stays an empty file and takes the skip-and-record-gap branch.
//
// Mutant: the empty-body test placed after ensureHlsInit — the init is fetched
// while the first segment has been asked for once.
func TestHlsLiveEmptyBodyDoesNotFetchTheInit(t *testing.T) {
	var deadRequests, deadAtInit atomic.Int32
	deadAtInit.Store(-1)
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n"+
			`#EXT-X-MAP:URI="init0.mp4"`+"\n"+
			"#EXTINF:1.000,live\ndead0.mp4\n#EXTINF:1.000,live\nseg1.mp4\n#EXTINF:1.000,live\nseg2.mp4\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case path == "init0.mp4":
			deadAtInit.CompareAndSwap(-1, deadRequests.Load())
			fmt.Fprint(w, "(I)")
		case strings.HasPrefix(path, "dead"):
			deadRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			fmt.Fprintf(w, "[%s]", strings.TrimSuffix(path, ".mp4"))
		}
	})

	data, gaps, err := runEmptyBodyHls(t, mux)
	if err != nil {
		t.Fatalf("Start() = %v, want the rest of the playlist downloaded", err)
	}
	if data != "(I)[seg1][seg2]" {
		t.Errorf("file holds %q, want the init once and then the media after the gap", data)
	}
	if len(gaps) != 1 || gaps[0].From != 0 || gaps[0].To != 0 {
		t.Errorf("gaps = %+v, want exactly one {From:0, To:0}", gaps)
	}
	if n := deadAtInit.Load(); n != MaxSegmentRetries {
		t.Errorf("init fetched after %d requests of the empty segment, want %d (only once it was given up)", n, MaxSegmentRetries)
	}
}
