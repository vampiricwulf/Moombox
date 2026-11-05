package worker

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestMarathonLiveCaptureRecordsTheLiveEdge pins audit G11's setup half and
// owner decision 10: a live capture of a broadcast live longer than ~118 h
// (marathonLiveEdge) recorded nothing — it always began at sq 0 or the DB
// seq, which YouTube's ~120 h window has dropped, and spent half an hour of
// still-live checks before ending with zero bytes. It now records the live
// HLS playlist, as yt-dlp records a live stream by default, and the job log
// says why from-start was dropped. Younger streams, VODs and post-live
// runs, and a part that already holds a DASH capture to append to, keep the
// from-start setup; an HLS setup that fails leaves it to run.
//
// Mutants: the marathon arm dropped from setupWithFallback (the 130 h rows
// run the from-start strategy); the age compared against 120 h of the wrong
// unit or not at all (the 100 h row records HLS); the DASH-manifest kind
// left out (its row); the isVod guard dropped (the post-live row records
// HLS); the staged-capture exemption dropped (the resumed row records HLS);
// the from-start retry after a failed HLS setup dropped (the HLS-fails row
// returns the HLS error); the Warn dropped (every HLS row).
func TestMarathonLiveCaptureRecordsTheLiveEdge(t *testing.T) {
	const warnMsg = "[Live] the stream has been live longer than YouTube keeps its segments (~120 h) — recording from the start cannot run; recording the live HLS playlist instead, from the live edge"
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		kind     string
		isVod    bool
		start    string
		staged   string // a DASH file the part already holds
		hlsErr   error
		wantHls  bool // HLS's result is the setup's
		wantFrom bool // the from-start strategy ran
	}{
		{name: "manifest-free, live 130 h", kind: "manifestless_dash", start: ago(130 * time.Hour), wantHls: true},
		{name: "DASH manifest, live 130 h", kind: "dash", start: ago(130 * time.Hour), wantHls: true},
		{name: "live 100 h", kind: "manifestless_dash", start: ago(100 * time.Hour), wantFrom: true},
		{name: "no start time", kind: "manifestless_dash", wantFrom: true},
		{name: "a start that does not parse", kind: "manifestless_dash", start: "2026-01-01", wantFrom: true},
		{name: "post-live, 130 h", kind: "manifestless_dash", isVod: true, start: ago(130 * time.Hour), wantFrom: true},
		{name: "resumed into a part holding its DASH capture", kind: "manifestless_dash", start: ago(130 * time.Hour), staged: "video_stream", wantFrom: true},
		{name: "the live HLS setup fails", kind: "manifestless_dash", start: ago(130 * time.Hour), hlsErr: errors.New("no HLS manifest URL available"), wantFrom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls", answers: []error{tc.hlsErr}}
			swapHls(t, hls)
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			job.Job.StreamStartTime = tc.start
			if tc.staged != "" {
				if err := os.WriteFile(filepath.Join(job.StagingDir, tc.staged), []byte("moof"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := &fakeStrategy{kind: tc.kind}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8"}
			res, err := setupWithFallback(context.Background(), job, s, tc.isVod, info, &StrategyDeps{})
			if err != nil || res == nil {
				t.Fatalf("setup = %+v, %v; want a result", res, err)
			}
			if res.IsHls != tc.wantHls {
				t.Errorf("result records HLS = %v, want %v", res.IsHls, tc.wantHls)
			}
			if ran := len(s.seen) > 0; ran != tc.wantFrom {
				t.Errorf("from-start strategy ran = %v, want %v", ran, tc.wantFrom)
			}
			triedHls := tc.wantHls || tc.hlsErr != nil
			if (len(hls.seen) == 1) != triedHls || len(hls.seen) > 1 {
				t.Errorf("HLS tried %d time(s), want %v", len(hls.seen), triedHls)
			}
			if rec.warned(warnMsg) != triedHls {
				t.Errorf("the from-start-dropped Warn logged = %v, want %v: %v", rec.warned(warnMsg), triedHls, logs.lines())
			}
			if triedHls {
				if line := logLine(logs, warnMsg); line == nil || line["liveFor"] == "" || line["streamStartTime"] != tc.start {
					t.Errorf("Warn = %v, want the age and the start it was read from", line)
				}
			}
			if tc.hlsErr != nil && !rec.warned("[Live] the live HLS setup failed — trying the from-start capture anyway") {
				t.Errorf("no Warn saying the HLS setup failed: %v", logs.lines())
			}
		})
	}
}

// TestMarathonLiveEdgeAge pins the threshold owner decision 10 set: two hours
// short of yt-dlp's MAX_DURATION, 432000 s.
func TestMarathonLiveEdgeAge(t *testing.T) {
	if marathonLiveEdgeAge != 118*time.Hour {
		t.Errorf("marathonLiveEdgeAge = %v, want 118h", marathonLiveEdgeAge)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	job, _ := reextractJob(t)
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{118 * time.Hour, false}, {118*time.Hour + time.Second, true}, {-time.Hour, false}} {
		job.Job.StreamStartTime = now.Add(-tc.age).Format(time.RFC3339)
		if _, got := marathonLiveEdge(job, &fakeStrategy{kind: "manifestless_dash"}, false, now); got != tc.want {
			t.Errorf("live for %v: marathonLiveEdge = %v, want %v", tc.age, got, tc.want)
		}
	}
}

// TestEvictedLiveStartSwitchesToTheLiveEdge pins audit G11's live-loop half:
// a still-live capture whose downloaders stopped having written nothing
// from a start the CDN no longer serves spent the six still-live checks
// re-seeding the same evicted position, then ended with zero bytes. Once
// diagnoseEvictedStart confirms the eviction, evictedStartToLiveEdge builds
// the live HLS capture and hands it back, the job log saying why. A start
// still served, an HLS capture already, and bytes written leave the checks
// to run; so does an HLS setup that fails.
//
// Mutants: evictedStartToLiveEdge switching without the diagnosis (the
// "sq 0 still served" row switches); the IsHls guard dropped (the HLS row
// runs a bisection and switches); the HLS failure returned as a switch (the
// failing row hands back a nil result); the Warn dropped (the switch row).
func TestEvictedLiveStartSwitchesToTheLiveEdge(t *testing.T) {
	const head = 30_000
	const itag = 137
	for _, tc := range []struct {
		name       string
		front      int // the oldest sequence the fake CDN serves
		isHls      bool
		hlsErr     error
		wantSwitch bool
		wantHls    bool // hls was asked
	}{
		{name: "evicted start", front: 25_000, wantSwitch: true, wantHls: true},
		{name: "sq 0 still served", front: 0},
		{name: "already recording HLS", front: 25_000, isHls: true},
		{name: "the HLS setup fails", front: 25_000, hlsErr: errors.New("no HLS manifest URL available"), wantHls: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
				if seq, _ := strconv.Atoi(r.URL.Query().Get("sq")); seq < tc.front {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Write([]byte{0, 0, 0, 8, 'f', 't', 'y', 'p'})
			}))
			defer srv.Close()
			videoDl := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: srv.URL + "/videoplayback?id=live-evict&itag=137"})
			if _, _, err := videoDl.ProbeSegmentAvailable(context.Background(), 0); err != nil {
				t.Fatalf("priming probe failed: %v", err)
			}
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			job.Job.SelectedVideoItag = new(itag)
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{
				{Itag: itag, Width: new(1920), Height: new(1080), TargetDurationSec: 5},
			}}
			o := &DownloadOrchestrator{logger: &discardLogger{}}
			want := &DownloadResult{IsHls: true}
			asked := 0
			got, ok := o.evictedStartToLiveEdge(context.Background(), job, info, &DownloadResult{VideoDownloader: videoDl, IsHls: tc.isHls},
				func() (*DownloadResult, error) {
					asked++
					if tc.hlsErr != nil {
						return nil, tc.hlsErr
					}
					return want, nil
				})
			if ok != tc.wantSwitch || (ok && got != want) || (!ok && got != nil) {
				t.Errorf("evictedStartToLiveEdge = %v, %v; want switch %v", got, ok, tc.wantSwitch)
			}
			if (asked == 1) != tc.wantHls || asked > 1 {
				t.Errorf("the HLS setup ran %d time(s), want %v", asked, tc.wantHls)
			}
			const warnMsg = "[Live] the capture's start is gone from YouTube's ~120 h window — recording from the start cannot run; recording the live HLS playlist instead, from the live edge"
			if rec.warned(warnMsg) != tc.wantHls {
				t.Errorf("the from-start-dropped Warn logged = %v, want %v", rec.warned(warnMsg), tc.wantHls)
			}
			if tc.wantHls {
				if line := logLine(logs, warnMsg); line == nil || line["err"] == nil {
					t.Errorf("Warn = %v, want the eviction diagnosis as its err", line)
				}
			}
			if tc.hlsErr != nil && !rec.warned("[Live] the live HLS setup failed — the still-live checks go on") {
				t.Errorf("no Warn saying the HLS setup failed: %v", logs.lines())
			}
		})
	}
}

// TestStillLiveBranchSwitchesAnEvictedStartFirst pins where the live loop
// asks evictedStartToLiveEdge: first in the still-live branch, ahead of the
// still-live check it would otherwise spend, with a switch adopting the HLS
// result into the loop (result, attachProgress) and going round again.
// Structural for the reason TestStillLiveRefreshSplitsOnAChangedQuality is:
// the branch sits behind the live strategies' real fetches.
//
// Mutants: the call dropped, or moved below `consecutiveLiveChecks.Add(1)`;
// the switch not adopted (`result = hlsResult` or attachProgress dropped);
// the HLS capture built in the root staging dir instead of curCtx.
func TestStillLiveBranchSwitchesAnEvictedStartFirst(t *testing.T) {
	const name = "orchestrator_youtube.go"
	fset := token.NewFileSet()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	var liveCase *ast.CaseClause
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runLiveStreamDownload" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok && len(cc.List) == 1 && text(cc.List[0]) == "youtube.StreamLive" {
				liveCase = cc
			}
			return liveCase == nil
		})
		return false
	})
	if liveCase == nil || len(liveCase.Body) == 0 {
		t.Fatal("no `case youtube.StreamLive:` in runLiveStreamDownload")
	}
	first, ok := liveCase.Body[0].(*ast.IfStmt)
	if !ok || !strings.Contains(text(first.Init), "o.evictedStartToLiveEdge(") {
		t.Fatal("the still-live branch does not ask evictedStartToLiveEdge first, ahead of the check it spends")
	}
	init := text(first.Init)
	if !strings.Contains(init, "o.refreshDownload(ctx, curCtx, freshInfo, true)") || !strings.Contains(init, "setAsideOtherLiveShape(curCtx, r)") {
		t.Errorf("the HLS capture is not built into the current part (curCtx), its other shape set aside: %s", init)
	}
	body := text(first.Body)
	for _, want := range []string{"result = hlsResult", "attachProgress(result)", "continue"} {
		if !strings.Contains(body, want) {
			t.Errorf("a switch does not adopt the HLS capture: missing %q", want)
		}
	}
}
