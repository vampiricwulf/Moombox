package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// adBreakBroadcast serves an ended broadcast's last window: one-second
// segments from pdt0, the third and fourth a stitched-ad break, ENDLIST.
func adBreakBroadcast(t *testing.T, pdt0 time.Time) *httptest.Server {
	t.Helper()
	ts := oneSecondTS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(ts)
			return
		}
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprintf(&b, "#EXT-X-DATERANGE:ID=\"stitched-ad-1\",CLASS=\"twitch-stitched-ad\",START-DATE=\"%s\",DURATION=2.000\n",
			pdt0.Add(2*time.Second).Format("2006-01-02T15:04:05.000Z07:00"))
		for i := range 6 {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:1.000,live\n/%d.ts\n",
				pdt0.Add(time.Duration(i)*time.Second).Format("2006-01-02T15:04:05.000Z07:00"), i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTwitchCaptureSplitsAtAdBreaksAndKeepsAGapFillPlan is owner decision D17
// through the real ExecuteTwitch: on a channel that fills its ad breaks, the
// capture closes its part where a stitched-ad break ends and continues in the
// next one on the same variant; each part records the wall-clock span its
// file holds; and the finished job is left a gap-fill plan with the opening
// it joined late for and the ad break between its parts.
//
// Mutants: SplitOnAdBreak not passed to the engine (one part, no ad-break
// span); the ErrAdBreakEnded branch removed (the split is read as a failure
// and the job lands in Error, no plan); the part closed without its ad-break
// mark (the hole reads as a lost gap); the plan not kept after the finalize.
func TestTwitchCaptureSplitsAtAdBreaksAndKeepsAGapFillPlan(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_gapfill_capture")
	pdt0 := time.Date(2026, 9, 1, 20, 5, 0, 0, time.UTC)
	srv := adBreakBroadcast(t, pdt0)
	h.job.StreamStartTime = pdt0.Add(-5 * time.Minute).Format(time.RFC3339)
	h.db.UpdateJobFields(h.job.ID, map[string]any{"stream_start_time": h.job.StreamStartTime})
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "streamer - capture"
	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.ChannelLogin = "streamer"
	h.variant.GapFillAdBreaks = true
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return false, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil); err != nil {
		t.Fatalf("ExecuteTwitch = %v", err)
	}

	root := readPartCoverage(h.jobCtx.StagingDir)
	next := readPartCoverage(filepath.Join(h.jobCtx.StagingDir, "seg_1"))
	if root.StartMs != pdt0.UnixMilli() || root.EndMs != pdt0.Add(2*time.Second).UnixMilli() || !root.EndedAtAdBreak {
		t.Errorf("part 1 coverage %+v, want 0-2 s, closed at the ad break", root)
	}
	if next.StartMs != pdt0.Add(4*time.Second).UnixMilli() || next.EndMs != pdt0.Add(6*time.Second).UnixMilli() {
		t.Errorf("part 2 coverage %+v, want 4-6 s", next)
	}
	job, _ := h.db.GetJob(h.job.ID)
	if job.Status != database.StatusFinished {
		t.Fatalf("job %s, want Finished", job.Status)
	}
	g, err := h.db.GetGapFill(h.job.ID)
	if err != nil || g == nil {
		t.Fatalf("no gap-fill plan kept (%v)", err)
	}
	var plan twitchGapFillPlan
	if err := json.Unmarshal([]byte(g.Plan), &plan); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, sp := range plan.Spans {
		kinds = append(kinds, sp.Kind)
	}
	if strings.Join(kinds, ",") != gapKindLateStart+","+gapKindAdBreak || plan.Login != "streamer" || plan.VariantName != "720p60" {
		t.Errorf("plan %+v (kinds %v), want the late start and the ad break of streamer's 720p60 capture", plan, kinds)
	}
}

// TestTwitchGapFillHooksAreWired pins the wiring no harness drives: processJob
// handing a live capture its channel login and the channel's ad-break
// setting, and Start running the sweep over the production filler.
//
// Mutant: any one of those lines removed.
func TestTwitchGapFillHooksAreWired(t *testing.T) {
	src, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"variant.ChannelLogin = login",
		"variant.GapFillAdBreaks = twitchChannelGapFillAdBreaks(c.Channels, login)",
		"go w.runTwitchGapFills(ctx, newTwitchGapFiller(w.db, w.tw, w.orchestrator, w.logger))",
	} {
		if !strings.Contains(string(src), n) {
			t.Errorf("worker.go no longer carries %q", n)
		}
	}
}
