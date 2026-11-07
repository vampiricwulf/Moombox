package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// downloaderAt is a downloader whose CurrentSeq is seq — what the live loop
// reads off a stopped one — stopped well behind its head, where a refused
// segment sits.
func downloaderAt(t *testing.T, seq int) *engine.SegmentDownloader {
	t.Helper()
	return downloaderAtHead(t, seq, seq+100)
}

// downloaderAtHead is downloaderAt with the head it learned: primed the way
// a failing download learns it, from the X-Head-Seqnum of one refused
// response (a probe, which writes nothing).
func downloaderAtHead(t *testing.T, seq, head int) *engine.SegmentDownloader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	d := engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=refused.1&itag=299",
		OutputFile: t.TempDir() + "/stream",
		StartSeq:   seq,
	})
	if _, _, err := d.ProbeSegmentAvailable(context.Background(), seq); err != nil {
		t.Fatalf("priming probe: %v", err)
	}
	if got := d.HeadSeq(); got != head {
		t.Fatalf("HeadSeq priming: got %d, want %d", got, head)
	}
	return d
}

// TestSameSeqRebuildCapIsThree pins the production value of the cap: owner
// decision D8 of the 2026-10-31 audit, "cap same-seq rebuilds at 3".
func TestSameSeqRebuildCapIsThree(t *testing.T) {
	if sameSeqRebuildCap != 3 {
		t.Errorf("sameSeqRebuildCap = %d, want 3", sameSeqRebuildCap)
	}
}

// TestSameSeqRebuildsCountsTheStuckStream pins what the cap counts: the
// ErrQualityLost rebuilds in a row at which ONE stream stopped where it
// stopped before. Its sibling keeps moving — the errgroup cancels it
// wherever it got to — so a count over the pair of sequences would never
// leave 1 while video was stuck; and a stream that moves starts its count
// again.
//
// Mutants:
//   - counting the pair (both sequences unchanged) instead of each stream: the
//     count stays at 1 while audio moves.
//   - seqRun.note never resetting on a new sequence: the count after video
//     moves is 4, not 1.
func TestSameSeqRebuildsCountsTheStuckStream(t *testing.T) {
	var r sameSeqRebuilds
	for i, step := range []struct{ video, audio, want int }{
		{50, 60, 1},
		{50, 75, 2},
		{50, 90, 3},
		{50, 104, 4},
		{51, 118, 1}, // video moved on
		{51, 118, 2}, // both stuck: still one count
	} {
		res := &DownloadResult{VideoDownloader: downloaderAt(t, step.video), AudioDownloader: downloaderAt(t, step.audio)}
		if got := r.note(res); got != step.want {
			t.Errorf("step %d (video %d, audio %d): count = %d, want %d", i, step.video, step.audio, got, step.want)
		}
	}

	// An audio-only capture is counted on its one stream.
	var audioOnly sameSeqRebuilds
	for i := range 3 {
		if got := audioOnly.note(&DownloadResult{AudioDownloader: downloaderAt(t, 7)}); got != i+1 {
			t.Errorf("audio-only rebuild %d: count = %d, want %d", i, got, i+1)
		}
	}
}

// TestSameSeqRebuildsCountsOnlyAStallTheRuleCanTake pins what the cap leaves
// uncounted, so it never sends these to the verify branch's half-hour budget:
//   - an HLS capture: the HLS loop has no refused-segment rule and never
//     reads the arming;
//   - a DASH stream stalled within two of its head — the live edge of a
//     broadcast interrupted while YouTube still calls it live, answering 403
//     or 410 there until it resumes — which the rule can never take.
//
// A stall that moves from the edge to well behind it starts counting from 1.
//
// Mutants: note without its IsHls check, or seqRun counting whatever the
// head: the matching row reaches the cap.
func TestSameSeqRebuildsCountsOnlyAStallTheRuleCanTake(t *testing.T) {
	t.Run("HLS", func(t *testing.T) {
		var r sameSeqRebuilds
		for i := range sameSeqRebuildCap + 2 {
			res := &DownloadResult{IsHls: true, VideoDownloader: downloaderAt(t, 50)}
			if got := r.note(res); got != 0 {
				t.Fatalf("rebuild %d: count = %d, want 0", i+1, got)
			}
		}
	})
	t.Run("at the live edge", func(t *testing.T) {
		var r sameSeqRebuilds
		for i, head := range []int{50, 51, 52, 51} { // cur 50: the edge, head-1, head-2
			res := &DownloadResult{VideoDownloader: downloaderAtHead(t, 50, head), AudioDownloader: downloaderAtHead(t, 50, head)}
			if got := r.note(res); got != 0 {
				t.Fatalf("rebuild %d (head %d): count = %d, want 0", i+1, head, got)
			}
		}
		res := &DownloadResult{VideoDownloader: downloaderAtHead(t, 50, 53)}
		if got := r.note(res); got != 1 {
			t.Errorf("first rebuild well behind the head: count = %d, want 1", got)
		}
	})
}

// TestSameSeqRebuildsArmsTheCappedRebuild pins which downloaders the cap
// arms: the rebuild that brings a stream's count to sameSeqRebuildCap — the
// third at the sequence — and every one built for it after, at the stuck
// sequence and on the stuck stream only. The first two rebuilds are plain
// retries; the third is the one whose downloader takes the segment
// (engine.SegmentDownloader.NoteRebuildsExhausted) where it would hand it
// back a fourth time.
//
// Mutants:
//   - arm with `>` for `>=`: the third rebuild is not armed.
//   - arm without its count check: the first rebuild is armed.
//   - arm naming the wrong stream: the audio downloader is armed.
//   - seqRun.note restarting its clock on every rebuild at the sequence: the
//     armed since is the latest rebuild's, not the first's.
func TestSameSeqRebuildsArmsTheCappedRebuild(t *testing.T) {
	type pair struct{ stopped, next *DownloadResult }
	var rebuilds []pair
	for rebuild := 1; rebuild <= sameSeqRebuildCap+1; rebuild++ {
		rebuilds = append(rebuilds, pair{
			stopped: &DownloadResult{VideoDownloader: downloaderAt(t, 50), AudioDownloader: downloaderAt(t, 60+rebuild)},
			next:    &DownloadResult{VideoDownloader: downloaderAt(t, 50), AudioDownloader: downloaderAt(t, 60+rebuild)},
		})
	}

	// In a bubble, so each rebuild lands a minute after the last on a clock
	// that moves only when told to.
	synctest.Test(t, func(t *testing.T) {
		var r sameSeqRebuilds
		var first time.Time
		for i, p := range rebuilds {
			rebuild := i + 1
			if rebuild > 1 {
				time.Sleep(time.Minute)
			}
			r.note(p.stopped)
			if rebuild == 1 {
				first = time.Now()
			}
			r.arm(p.next)
			want := -1
			if rebuild >= sameSeqRebuildCap {
				want = 50
			}
			if got := p.next.VideoDownloader.RebuildsExhaustedSeq(); got != want {
				t.Errorf("rebuild %d: video armed at %d, want %d", rebuild, got, want)
			}
			if want >= 0 {
				// The persistence a 5xx must outlast runs from the run's first
				// rebuild, not from this one.
				if got := p.next.VideoDownloader.RebuildsExhaustedSince(); !got.Equal(first) {
					t.Errorf("rebuild %d: armed since %v, want the first rebuild's %v", rebuild, got, first)
				}
			}
			if got := p.next.AudioDownloader.RebuildsExhaustedSeq(); got != -1 {
				t.Errorf("rebuild %d: audio armed at %d, want unarmed (it kept moving)", rebuild, got)
			}
		}
	})
}

// TestPastRefusedSegment pins where the part after a refused segment starts
// each stream. A stream stopped AT the hole steps one past it — the refusing
// stream, and a sibling stopped on the same sequence, which is almost always
// the same hole; started there, its new part (a file already holding its
// init, so with the first-segment hunt off) would refuse it again and split a
// second time. A stream stopped anywhere else keeps its own position, as a
// quality split keeps it.
//
// Mutant: moving only the stream that is behind, or both whatever their
// position: the matching row fails.
func TestPastRefusedSegment(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		video, audio           int
		wantV, wantA           int
		wantVMoved, wantAMoved bool
	}{
		{name: "video refused, audio ahead", video: 50, audio: 57, wantV: 51, wantA: 57, wantVMoved: true},
		{name: "audio refused, video behind", video: 44, audio: 50, wantV: 44, wantA: 51, wantAMoved: true},
		{name: "video refused, audio behind", video: 50, audio: 44, wantV: 51, wantA: 44, wantVMoved: true},
		{name: "both stopped at the hole", video: 50, audio: 50, wantV: 51, wantA: 51, wantVMoved: true, wantAMoved: true},
		{name: "no video stream", video: 0, audio: 50, wantV: 0, wantA: 51, wantAMoved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, a, vm, am := pastRefusedSegment(50, tc.video, tc.audio)
			if v != tc.wantV || a != tc.wantA || vm != tc.wantVMoved || am != tc.wantAMoved {
				t.Errorf("pastRefusedSegment(50, %d, %d) = (%d, %d, %v, %v), want (%d, %d, %v, %v)",
					tc.video, tc.audio, v, a, vm, am, tc.wantV, tc.wantA, tc.wantVMoved, tc.wantAMoved)
			}
		})
	}
}

// TestNoteRefusedHoleRecordsGapRows pins the gap rows of a split at a refused
// segment: one per stream that steps over it, through the progress tracker's
// OnGap — the rows the job's gap list and its persisted gaps show — and none
// for a stream that keeps its position.
//
// Mutant: noteRefusedHole ignoring its moved flags: the audio row appears.
func TestNoteRefusedHoleRecordsGapRows(t *testing.T) {
	tracker := NewProgressTracker(nil, "job-hole", &discardLogger{}, 0)
	res := &DownloadResult{VideoDownloader: downloaderAt(t, 50), AudioDownloader: downloaderAt(t, 57)}
	tracker.AttachVideoDownloader(res.VideoDownloader)
	tracker.AttachAudioDownloader(res.AudioDownloader)

	noteRefusedHole(res, 50, true, false)

	tracker.mu.Lock()
	got := slices.Clone(tracker.gaps)
	tracker.mu.Unlock()
	want := []database.Gap{{JobID: "job-hole", From: 50, To: 50, Stream: "video"}}
	if !slices.Equal(got, want) {
		t.Errorf("gap rows = %+v, want %+v", got, want)
	}
}

// TestLiveLoopSplitsAtARefusedSegment pins the live loop's wiring of the
// refused-segment split and the rebuild cap (owner decision D8), on
// runLiveStreamDownload's source as the stall split is
// (TestStillLiveRefreshSplitsOnAChangedQuality): the branch sits behind real
// player-response fetches and the live strategies' manifest setup, which no
// offline test can drive. The engine side — a segment refused forever while
// later ones are served ends the download with a *SegmentRefusedError, or is
// skipped in place with a gap row — is driven against an in-memory
// googlevideo by the engine's downloader_dash_refused_test.go.
//
// Mutants (each fails one row):
//   - attachProgress not arming SplitOnRefusedSegment, or not calling
//     rebuilds.arm: the engine never splits for this loop.
//   - the ErrQualityLost branch not entered on ErrSegmentRefused.
//   - the rebuild cap not counted, or not keeping a capped rebuild out of the
//     branch.
//   - the refused split not moving the streams past the hole, not recording
//     the gap rows, or adopting the look-refresh instead of splitting.
//   - splitPart sending the quality-split notification for a hole split.
func TestLiveLoopSplitsAtARefusedSegment(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *DownloadOrchestrator) runLiveStreamDownload(")
	end := strings.Index(body, "\nfunc unreadableStatusEndsCapture(")
	if start < 0 || end < start {
		t.Fatal("runLiveStreamDownload not found")
	}
	fn := body[start:end]
	in := func(from, to string) string {
		i := strings.Index(fn, from)
		if i < 0 {
			t.Fatalf("runLiveStreamDownload lacks %q", from)
		}
		j := strings.Index(fn[i:], to)
		if j < 0 {
			t.Fatalf("runLiveStreamDownload lacks %q after %q", to, from)
		}
		return fn[i : i+j]
	}

	attach := in("attachProgress := func(res *DownloadResult) {", "\n\tattachProgress(result)")
	for _, want := range []string{"d.SplitOnRefusedSegment = true", "rebuilds.arm(res)"} {
		if !strings.Contains(attach, want) {
			t.Errorf("attachProgress lacks %q", want)
		}
	}
	for _, want := range []string{
		"isRefused := errors.As(downloadErr, &refused)",
		"if isQualityLost && !qualityChanged {\n\t\t\tif n := rebuilds.note(result); n > sameSeqRebuildCap {\n\t\t\t\trebuildCapped = true",
		"if (qualityChanged || isQualityLost || isRefused) && !rebuildCapped {",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("runLiveStreamDownload lacks %q", want)
		}
	}
	notify := in("splitPart := func(", "// Mux the old segment in the background")
	if !strings.Contains(notify, "if hole < 0 || newQuality.Changed(currentQuality) {\n\t\t\to.logger.Info(\"quality split\"") ||
		!strings.Contains(notify, "o.sendQualitySplitNotification(") {
		t.Error("splitPart no longer keeps the quality-split notification to quality changes — a refused-segment split would announce one")
	}
	split := in("\t\t\tif isRefused {\n\t\t\t\thole := refused.Seq", "\n\t\t\tif !newQuality.Changed(currentQuality) && !streamIdentityChanged(result, refreshResult) {")
	for _, want := range []string{
		"oldVideoSeq, oldAudioSeq, videoMoved, audioMoved = pastRefusedSegment(hole, oldVideoSeq, oldAudioSeq)",
		"noteRefusedHole(result, hole, videoMoved, audioMoved)",
		"splitPart(newQuality, freshInfo, oldVideoSeq, oldAudioSeq, false, segmentEndTime, hole)",
		"continue",
	} {
		if !strings.Contains(split, want) {
			t.Errorf("the refused-segment split lacks %q", want)
		}
	}
	if strings.Contains(split, "result = refreshResult") {
		t.Error("the refused-segment split adopts the look-refresh — the part would continue across the hole")
	}
}

// fakeStopped is a stopped stream for noteStopped, standing in the engine's
// readings directly.
type fakeStopped struct {
	behind, whole bool
	seq           int
}

func (f fakeStopped) StoppedWellBehindHead() bool { return f.behind }
func (f fakeStopped) WholeStreamRefused() bool    { return f.whole }
func (f fakeStopped) CurrentSeq() int             { return f.seq }

// TestWholeStreamRefusalNeverReachesTheCap: a rebuild whose downloader's
// refused-segment rule declined the hole — nothing past it served, or the
// head served segments report within two of it — ends the count instead of
// adding to it (owner decision PR-M2). Every segment refused (a GVS, PO-token
// or bot-detection outage) and a stall at the edge the stream has served
// both raise the tracked head through refusals' X-Head-Seqnum and the head
// probes, so StoppedWellBehindHead reads them as a refused segment; counted,
// the fourth such rebuild passed the cap and the stream-end verify then
// finalized a capture YouTube still called live after six still-live checks.
// Reset, the loop rebuilds for as long as the stream is live, and arms the
// rule again every third rebuild. A segment refused while later ones are
// served still reaches the cap.
//
// Mutant: noteStopped not asking WholeStreamRefused — the fourth rebuild of
// the outage counts 4, past the cap.
func TestWholeStreamRefusalNeverReachesTheCap(t *testing.T) {
	var r seqRun
	for i := 1; i <= sameSeqRebuildCap; i++ {
		if got := r.noteStopped(fakeStopped{behind: true, seq: 50}); got != i {
			t.Fatalf("rebuild %d at a stuck segment: count = %d, want %d", i, got, i)
		}
	}
	// The armed downloader probed past the hole: nothing served.
	if got := r.noteStopped(fakeStopped{behind: true, whole: true, seq: 50}); got != 0 {
		t.Fatalf("a declined hole counted %d, want the run ended (0)", got)
	}
	// The outage goes on: the count starts again and re-arms at the cap.
	for i := 1; i <= sameSeqRebuildCap; i++ {
		if got := r.noteStopped(fakeStopped{behind: true, seq: 50}); got != i {
			t.Fatalf("rebuild %d after a decline: count = %d, want %d", i, got, i)
		}
	}
	if got := r.noteStopped(fakeStopped{behind: true, whole: true, seq: 50}); got > sameSeqRebuildCap {
		t.Errorf("the outage's rebuild counted %d, past the cap", got)
	}
}
