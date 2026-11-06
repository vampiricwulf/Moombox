package engine

import (
	"context"
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

// TestCheckStaleEndAnswersOnlyTheStaleWindow pins CheckStaleEnd's scope: it
// replaces CheckStreamStatus at the stale-playlist site and nowhere else. A
// Twitch VOD that is no longer recording answers it "ended", and the
// playlist below — twelve segments, no ENDLIST, seg3-seg5 dead — is that
// VOD. The stuck-skip exit asks CheckStreamStatus ("not ended", a VOD has no
// live end), so the three dead segments are skipped as gaps and seg6-seg11,
// which are still fetchable, are written; only then, the playlist having
// stopped growing, does the stale window ask CheckStaleEnd and end the
// download cleanly.
//
// Mutants: the stale site ignoring CheckStaleEnd (CheckStreamStatus's "not
// ended" never ends the run: the stale-loop guard below cancels it and the
// test fails); the stuck-skip exit asking CheckStaleEnd as well (the run
// stops at "[seg0][seg1][seg2]", dropping the fetchable tail — the W18
// review's probe).
func TestCheckStaleEndAnswersOnlyTheStaleWindow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := range 12 {
			fmt.Fprintf(&b, "#EXTINF:1.0,\nseg%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	for _, dead := range []string{"/seg3.ts", "/seg4.ts", "/seg5.ts"} {
		mux.HandleFunc(dead, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	}
	mux.HandleFunc("/", echoSegment)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var statusAsks, staleAsks atomic.Int32
	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/vod.m3u8",
		OutputFile: out,
		StartSeq:   -1,
		IsHls:      true,
		CheckStreamStatus: func(context.Context) (bool, error) {
			// The stuck-skip exit asks once per three skips; more than a
			// handful of asks is the stale site looping on "not ended".
			if statusAsks.Add(1) > 10 {
				cancel()
			}
			return false, nil
		},
		CheckStaleEnd: func(context.Context) (bool, error) {
			staleAsks.Add(1)
			return true, nil
		},
	})
	d.delays = fastDelays()
	d.delays.hlsStuckRetry = 10 * time.Millisecond
	err := d.Start(ctx)
	if err != nil {
		t.Fatalf("Start() = %v, want the stale window to end the VOD cleanly", err)
	}
	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	want := "[seg0][seg1][seg2][seg6][seg7][seg8][seg9][seg10][seg11]"
	if string(data) != want {
		t.Errorf("file holds %q, want %q — the fetchable tail past the dead run must be written", data, want)
	}
	if n := staleAsks.Load(); n != 1 {
		t.Errorf("CheckStaleEnd asked %d times, want once (the one stale window)", n)
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not latched by the stale-window end")
	}
}
