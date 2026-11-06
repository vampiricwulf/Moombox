package worker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestSplitLiveCaptureGetsItsArchiveVodChapters is owner decision FE-15's
// "the same chapter sources" for a split live Twitch capture. ExecuteTwitch
// staged the chapters — the archive VOD's moments, looked up at finalize —
// only for a single-part archive, so a split capture's finalize found none
// staged and wrote the observed categories alone beside its parts. It now
// stages for both, and the finalize writes the VOD's chapters, moved onto the
// parts' global timeline.
//
// The behaviour half drives what the capture now does (stageTwitchChapters
// into the staging root, then the multi-segment finalize); the source half
// pins the call in ExecuteTwitch, which a unit test cannot reach without a
// real capture, outside any if.
//
// Mutants: the stage call put back under `if segmentIndex == 0 && ...` (the
// source half fails); archiveChapters not reading the staged list (the
// observed "Just Chatting" is written instead of the VOD's titles).
func TestSplitLiveCaptureGetsItsArchiveVodChapters(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	job := &database.Job{ID: "tw_split_vod", VideoID: "tw_split_vod", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusDownloading, StreamStartTime: start.Format(time.RFC3339)}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	root, staging := t.TempDir(), t.TempDir()
	noteTwitchRecordingStart(staging, start)
	noteTwitchCategory(staging, "Just Chatting", start.Add(2*time.Second))
	var segments []database.Segment
	for i, sp := range []struct{ at, dur int64 }{{0, 300}, {600, 300}} {
		name := "Show - part" + strconv.Itoa(i+1) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, database.Segment{SegmentIndex: i, Filename: name, FilePath: p,
			UnixStart: start.Unix() + sp.at, DurationSeconds: float64(sp.dur)})
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging,
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
	variant := &TwitchVariantInfo{ArchiveChaptersFn: func(context.Context) []utils.Chapter {
		return []utils.Chapter{{StartMs: 0, EndMs: 700_000, Title: "Intro"}, {StartMs: 700_000, EndMs: 900_000, Title: "Ranked"}}
	}}
	o.stageTwitchChapters(context.Background(), jobCtx, variant, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
		t.Fatalf("finalizeMultiSegmentJob: %v", err)
	}
	got, err := utils.ReadChaptersSidecar(filepath.Join(root, "Show"+utils.ChaptersSidecarSuffix))
	if err != nil {
		t.Fatal(err)
	}
	// 700 s of the capture is 100 s into part 2, which starts at 300 s of
	// the global timeline.
	want := []utils.SidecarChapter{{Start: 0, End: 400, Title: "Intro"}, {Start: 400, End: 600, Title: "Ranked"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chapters beside the parts = %+v, want the archive VOD's %+v", got, want)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "orchestrator_twitch.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ExecuteTwitch" {
			continue
		}
		var stack []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "stageTwitchChapters" {
				found = true
				for _, a := range stack {
					if _, isIf := a.(*ast.IfStmt); isIf {
						t.Errorf("ExecuteTwitch stages the chapters under an if at %s: a split capture's finalize finds none", fset.Position(a.Pos()))
					}
				}
			}
			return true
		})
	}
	if !found {
		t.Error("ExecuteTwitch no longer calls stageTwitchChapters")
	}
}
