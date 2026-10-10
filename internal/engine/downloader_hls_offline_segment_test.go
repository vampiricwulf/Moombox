package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// vodPlaylist is a four-segment VOD playlist; seg names echo into the file.
const vodPlaylist = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-PLAYLIST-TYPE:VOD\n" +
	"#EXTINF:1.0,\nseg0.ts\n#EXTINF:1.0,\nseg1.ts\n#EXTINF:1.0,\nseg2.ts\n#EXTINF:1.0,\nseg3.ts\n#EXT-X-ENDLIST\n"

// runOutageVod downloads vodPlaylist from srv. sequential takes the loop's
// one-segment-at-a-time path (StopOnGap, as a Twitch live capture runs);
// otherwise the ENDLIST playlist goes to runHlsVodParallel, with maxRetries
// fetch attempts per segment.
func runOutageVod(t *testing.T, srv *httptest.Server, offline *atomic.Bool, sequential bool, maxRetries int) (string, []DownloadGap, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/vod.m3u8",
		OutputFile: out,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  sequential,
		MaxRetries: maxRetries,
		IsOnline:   func() bool { return !offline.Load() },
	})
	d.delays = fastDelays()
	var mu sync.Mutex
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) {
		mu.Lock()
		defer mu.Unlock()
		gaps = append(gaps, g)
	}
	startErr := d.Start(t.Context())
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return string(data), gaps, startErr
}

func checkWholeVod(t *testing.T, data string, gaps []DownloadGap, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("Start() = %v, want the VOD downloaded", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps %v — a segment was skipped over an outage", gaps)
	}
	if data != "[seg0][seg1][seg2][seg3]" {
		t.Errorf("file holds %q, want every segment once", data)
	}
}

func echoSegment(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
}

// seg1FailsOffline serves vodPlaylist with seg1's first request opening a
// two-second outage, during which seg1 fails. The playlist stays reachable,
// so only a segment-failure branch can notice the outage.
func seg1FailsOffline(t *testing.T, offline *atomic.Bool) *httptest.Server {
	var tripped atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vodPlaylist)
	})
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		if tripped.CompareAndSwap(false, true) {
			offline.Store(true)
			// Long enough for the old code to spend the whole stuck budget
			// (MaxSegmentRetries rounds of about a tenth of a second each).
			time.AfterFunc(2*time.Second, func() { offline.Store(false) })
		}
		if offline.Load() {
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		echoSegment(w, r)
	})
	mux.HandleFunc("/", echoSegment)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A segment that fails while the device is offline waits for connectivity
// and is fetched again then. It used to be charged to the stuck-segment
// count like any other failure, so an outage that outlasted a few retries
// ended the part (StopOnGap) or skipped the segment as a gap.
//
// Mutant: the IsOnline check in the segment-failure branch removed — the
// part ends with ErrGapDetected at seg1.
func TestAnOfflineSegmentFailureWaitsForConnectivity(t *testing.T) {
	var offline atomic.Bool
	srv := seg1FailsOffline(t, &offline)
	data, gaps, err := runOutageVod(t, srv, &offline, true, 0)
	checkWholeVod(t, data, gaps, err)
}

// The VOD path proper — runHlsVodParallel through fetchSegmentWithRetry —
// gave a segment five attempts over about fifty seconds and then wrote a gap
// in its place, so an outage longer than that turned every segment in flight
// into a hole in the archive. An attempt that fails offline is not charged
// now; one attempt per segment proves it.
//
// Mutant: the IsOnline wait in fetchSegmentWithRetry removed — seg1 is a gap.
func TestAVodSegmentFailingOfflineIsNotAGap(t *testing.T) {
	var offline atomic.Bool
	srv := seg1FailsOffline(t, &offline)
	data, gaps, err := runOutageVod(t, srv, &offline, false, 1)
	checkWholeVod(t, data, gaps, err)
}

// Failures before an outage do not carry over past it (sequential path). The
// outage here is
// caught by the playlist path (a 404 while offline), and the segment that was
// failing before it fails once more right after; the old count crossed
// MaxSegmentRetries on that one failure and ended the part at seg1.
//
// Mutant: the stuck count kept across hlsOutages — ErrGapDetected at seg1.
func TestTheStuckSegmentCountRestartsAfterAnOutage(t *testing.T) {
	var offline atomic.Bool
	var seg1Requests atomic.Int32
	var outageDone atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		if seg1Requests.Load() == MaxSegmentRetries-1 && outageDone.CompareAndSwap(false, true) {
			offline.Store(true)
			time.AfterFunc(300*time.Millisecond, func() { offline.Store(false) })
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, vodPlaylist)
	})
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		// Fails MaxSegmentRetries times in all — enough to skip it if the
		// failures before the outage still counted — then serves.
		if seg1Requests.Add(1) <= MaxSegmentRetries {
			http.Error(w, "cdn hiccup", http.StatusServiceUnavailable)
			return
		}
		echoSegment(w, r)
	})
	mux.HandleFunc("/", echoSegment)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data, gaps, err := runOutageVod(t, srv, &offline, true, 0)
	if !outageDone.Load() {
		t.Fatal("the outage never happened — the test did not exercise the restart")
	}
	checkWholeVod(t, data, gaps, err)
}
