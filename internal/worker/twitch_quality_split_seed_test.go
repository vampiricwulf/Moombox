package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// A quality split used to seed its successor with -1, which the engine
// resolves to the playlist's MEDIA-SEQUENCE — the oldest segment in the
// window — so the new part re-downloaded, and repeated on screen, the closed
// part's last seconds. The successor now starts at the first sequence the
// closed part does not hold, like the gap and init-change splits.
//
// The 720p variant's playlist starts 404ing after minSegmentDuration (so the
// closed part is kept, not discarded as short) while the broadcast is still
// live; the engine reports the quality lost and the refresh moves to 1080p.
// Both variants serve the same sliding window of sequence numbers. The
// minimum is a second here rather than production's ten: the rule is the
// same, and the wall clock this run waits on is the minimum's. The broadcast
// ends once the successor has fetched a segment — the first moment there is
// something to check — rather than at a set time, which a split that came
// late on a slow machine could miss.
//
// The same split records the variant it moved to on the row, as the job's
// twitch_quality, which the Twitch preference reads back when the job is
// retried or resumed.
//
// Mutants: the quality split seeding its successor with -1 again; the
// recordVariant call removed from the quality-split branch (the row still
// names 720p30).
func TestTwitchQualitySplitSuccessorStartsWhereThePartEnded(t *testing.T) {
	prevMin := minSegmentDuration
	minSegmentDuration = time.Second
	t.Cleanup(func() { minSegmentDuration = prevMin })
	ts := oneSecondTS(t)
	start := time.Now()
	switchAt := minSegmentDuration + time.Second
	// giveUp ends a broadcast whose successor never fetches, so a broken
	// split fails here rather than at the context's deadline.
	const giveUp = 20 * time.Second

	var mu sync.Mutex
	got := map[string][]int{}
	ended := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got["1080"]) > 0 || time.Since(start) > giveUp
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			name = strings.TrimSuffix(name, ".m3u8")
			el := time.Since(start)
			if name == "720" && el > switchAt {
				http.NotFound(w, r)
				return
			}
			head := int(el.Seconds())
			lo := max(0, head-5)
			var b strings.Builder
			fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", lo)
			for s := lo; s <= head; s++ {
				fmt.Fprintf(&b, "#EXTINF:1.000,\n/%s/%d.ts\n", name, s)
			}
			if ended() {
				b.WriteString("#EXT-X-ENDLIST\n")
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(rest, ".ts"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		got[name] = append(got[name], n)
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)

	h := newEndVerdictHarness(t, "tw_qsplit_seed")
	low := twitch.TwitchHLSVariant{URL: srv.URL + "/720.m3u8", Name: "720p30", Width: 1280, Height: 720, FPS: 30}
	high := twitch.TwitchHLSVariant{URL: srv.URL + "/1080.m3u8", Name: "1080p60", Width: 1920, Height: 1080, FPS: 60}
	h.variant.URL, h.variant.Name, h.variant.Width, h.variant.Height, h.variant.FPS = low.URL, low.Name, low.Width, low.Height, low.FPS
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return !ended(), nil }
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		if time.Since(start) > switchAt {
			return []twitch.TwitchHLSVariant{high}, nil
		}
		return []twitch.TwitchHLSVariant{low}, nil
	}
	h.db.UpdateJobFields(h.job.ID, map[string]any{"twitch_quality": low.Name})
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "qsplit"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil); err != nil {
		t.Logf("ExecuteTwitch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	closed, successor := got["720"], got["1080"]
	if len(closed) == 0 || len(successor) == 0 {
		t.Fatalf("both variants must be fetched: 720p %v, 1080p %v", closed, successor)
	}
	if last, first := slices.Max(closed), slices.Min(successor); first != last+1 {
		t.Errorf("the closed part ends at seq %d but its successor starts at %d, want %d (720p %v, 1080p %v)",
			last, first, last+1, closed, successor)
	}
	segs, err := h.db.GetSegments(h.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Errorf("want the 720p part kept beside its 1080p successor, got %d parts", len(segs))
	}
	j, err := h.db.GetJob(h.job.ID)
	if err != nil || j == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.TwitchQuality != high.Name {
		t.Errorf("after the quality split twitch_quality = %q, want %q — the variant being recorded", j.TwitchQuality, high.Name)
	}
}
