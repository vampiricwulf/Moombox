package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// tailAt is a stream that stopped with its last written segment `short`
// below head, as the engine reports it: behind head exactly when the next
// segment to fetch (last+1) is still below the head.
func tailAt(head, short int) streamTail {
	last := head - short
	return streamTail{present: true, behindHead: last+1 < head, lastSeq: last, head: head}
}

// TestIncompleteTailEndOfStreamRule is owner ruling R-T1 as a table. The
// owner's captures: last written 11017 under head 11019 on both streams,
// YouTube confirmed over, nothing interrupted — complete.
//
// Mutants: engine.EndOfStreamHeadMargin at 1 (the head-2 row turns
// incomplete) or 3 (the head-3 row turns complete); endOfStreamTail without
// the confirmedOver check (the unconfirmed and VOD rows turn complete);
// without the same-segment check (the split rows turn complete); incompleteTail
// consulting the margin before the interruption evidence (the interrupted
// rows turn complete).
func TestIncompleteTailEndOfStreamRule(t *testing.T) {
	const head = 11019
	over := liveEnd{confirmedOver: true}
	interrupted := func(s streamTail) streamTail { s.interrupted = true; return s }

	s0, s1, s2, s3 := tailAt(head, 0), tailAt(head, 1), tailAt(head, 2), tailAt(head, 3)
	for _, tc := range []struct {
		name string
		f    tailFacts
		want bool // incomplete
	}{
		{"last == head", tailFacts{video: s0, audio: s0, end: over}, false},
		{"last == head-1", tailFacts{video: s1, audio: s1, end: over}, false},
		{"last == head-2: the end-of-stream margin", tailFacts{video: s2, audio: s2, end: over}, false},
		{"last == head-3: past the margin", tailFacts{video: s3, audio: s3, end: over}, true},

		{"head-2, the stream not confirmed over (and the VOD branch's zero liveEnd)", tailFacts{video: s2, audio: s2}, true},
		{"head-2, the worker waited for a resume", tailFacts{video: s2, audio: s2, end: liveEnd{confirmedOver: true, waitedForResume: true}}, true},
		{"head-2, the video downloader interrupted", tailFacts{video: interrupted(s2), audio: s2, end: over}, true},
		{"head-2, the audio downloader interrupted", tailFacts{video: s2, audio: interrupted(s2), end: over}, true},
		{"video head-2, audio head-1: different segments", tailFacts{video: s2, audio: s1, end: over}, true},
		{"video head-1, audio head-2: different segments", tailFacts{video: s1, audio: s2, end: over}, true},
		{"video head-2, audio head-3", tailFacts{video: s2, audio: s3, end: over}, true},
		{"head-2, the same segment under heads one apart", tailFacts{video: s2, audio: tailAt(head-1, 1), end: over}, false},
		{"video only, head-2", tailFacts{video: s2, end: over}, false},
		{"video only, head-3", tailFacts{video: s3, end: over}, true},
		{"audio only, head-2", tailFacts{audio: s2, end: over}, false},

		{"at the head, interrupted", tailFacts{video: interrupted(s0), audio: s0, end: over}, true},
		{"no downloaders, the worker waited", tailFacts{end: liveEnd{waitedForResume: true}}, true},
		{"no downloaders, nothing held", tailFacts{}, false},
	} {
		if got := incompleteTail(tc.f); got != tc.want {
			t.Errorf("%s: incompleteTail = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// newEndedTestDownloader runs a real SegmentDownloader to its finalize
// against a stream that is over: the head reads `head`, segments up to
// lastServed are served, the rest answer 403 for the whole timeout, and the
// status check says ended.
func newEndedTestDownloader(t *testing.T, head, lastServed int) *engine.SegmentDownloader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		sqStr := r.URL.Query().Get("sq")
		if sqStr == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		sq, err := strconv.Atoi(sqStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if sq <= lastServed {
			fmt.Fprintf(w, "seg-%05d;", sq)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	d := engine.NewSegmentDownloader(engine.DownloaderOptions{
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
	if got := d.LastSeq(); got != lastServed {
		t.Fatalf("test setup invalid: LastSeq = %d, want %d", got, lastServed)
	}
	if got := d.HeadSeq(); got != head {
		t.Fatalf("test setup invalid: HeadSeq = %d, want %d", got, head)
	}
	return d
}

// TestFinalizeIncompleteTailAtTheEngineSeam drives real downloaders to the
// stops the table above describes and checks the flag finalizeIncompleteTail
// persists.
//
// Mutants: as TestIncompleteTailEndOfStreamRule, on the real latches — and
// resultTailFacts reading CurrentSeq for the last written segment (the
// head-3 capture reads as head-2, inside the margin).
func TestFinalizeIncompleteTailAtTheEngineSeam(t *testing.T) {
	db := openTestDB(t)
	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}
	const head = 8
	over := liveEnd{confirmedOver: true}
	for i, tc := range []struct {
		name           string
		short          int // the last written segment is head-short
		end            liveEnd
		wantBehind     bool
		wantIncomplete bool
	}{
		{"head-1, confirmed over", 1, over, false, false},
		{"head-2, confirmed over", 2, over, true, false},
		{"head-2, not confirmed (the VOD branch)", 2, liveEnd{}, true, true},
		{"head-2, the worker waited for a resume", 2, liveEnd{confirmedOver: true, waitedForResume: true}, true, true},
		{"head-3, confirmed over", 3, over, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "yt_seam_" + strconv.Itoa(i)
			if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, Status: database.StatusDownloading}); err != nil {
				t.Fatal(err)
			}
			// The flag starts set, so a complete verdict has to clear it.
			db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": true})
			result := &DownloadResult{
				VideoDownloader: newEndedTestDownloader(t, head, head-tc.short),
				AudioDownloader: newEndedTestDownloader(t, head, head-tc.short),
			}
			if got := result.VideoDownloader.FinalizedBehindHead(); got != tc.wantBehind {
				t.Fatalf("FinalizedBehindHead = %v, want %v: the engine latches every stop below the head, whatever the margin", got, tc.wantBehind)
			}

			if got := resultIncompleteTail(result, tc.end); got != tc.wantIncomplete {
				t.Errorf("resultIncompleteTail = %v, want %v", got, tc.wantIncomplete)
			}
			incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, result, tc.end)
			if incomplete != tc.wantIncomplete {
				t.Errorf("finalizeIncompleteTail = %v, want %v", incomplete, tc.wantIncomplete)
			}
			row, err := db.GetJob(jobID)
			if err != nil || row == nil {
				t.Fatalf("GetJob: %v", err)
			}
			if row.IncompleteTail != tc.wantIncomplete {
				t.Errorf("persisted incomplete_tail = %v, want %v", row.IncompleteTail, tc.wantIncomplete)
			}
		})
	}
}

// TestInterruptedCaptureNearTheHeadStaysIncomplete: a downloader that
// finalized during an interruption is incomplete even with the stream
// confirmed over, however near the head it stopped.
func TestInterruptedCaptureNearTheHeadStaysIncomplete(t *testing.T) {
	d := newInterruptedTestDownloader(t)
	if d.HeadSeq()-d.LastSeq() > engine.EndOfStreamHeadMargin {
		t.Fatalf("test setup invalid: stopped %d below the head, want it inside the margin", d.HeadSeq()-d.LastSeq())
	}
	if !resultIncompleteTail(&DownloadResult{VideoDownloader: d}, liveEnd{confirmedOver: true}) {
		t.Error("an interrupted capture inside the end-of-stream margin read as complete")
	}
}

// TestToleratedTailRow pins which saved rows the boot pass reads as an ended
// broadcast: a Finished, flagged YouTube live capture whose line shows both
// streams at one segment under one head, 1 or 2 short of it.
//
// Mutants: the margin at 1 or 3; dropping a == c, b == d, the gap >= 1
// floor, the IsVod, Finished or flag checks; an unanchored pattern.
func TestToleratedTailRow(t *testing.T) {
	row := func(progress string) *database.Job {
		return &database.Job{Status: database.StatusFinished, IncompleteTail: true, Platform: "youtube", Progress: progress}
	}
	with := func(j *database.Job, edit func(*database.Job)) *database.Job { edit(j); return j }
	const owner = "(V: 11017/11019 A: 11017/11019 C: 17062)"
	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"the owner's rows", row(owner), true},
		{"no chat count", row("(V: 11017/11019 A: 11017/11019)"), true},
		{"gap 1", row("(V: 11018/11019 A: 11018/11019 C: 3)"), true},
		{"gap 0", row("(V: 11019/11019 A: 11019/11019 C: 3)"), false},
		{"gap 3", row("(V: 11016/11019 A: 11016/11019 C: 3)"), false},
		{"past the head", row("(V: 11020/11019 A: 11020/11019)"), false},
		{"a != c", row("(V: 11017/11019 A: 11016/11019 C: 3)"), false},
		{"b != d", row("(V: 11017/11019 A: 11017/11018 C: 3)"), false},
		{"no heads", row("(V: 11017 A: 11017 C: 3)"), false},
		{"a wait activity after the counters", row(owner + " retrying 4m"), false},
		{"a VOD percentage line", row("V:100% A:100% C: 3"), false},
		{"an HLS line", row("Seq: 11017 C: 3"), false},
		{"empty", row(""), false},
		{"numbers too large to read", row("(V: 99999999999999999997/99999999999999999999 A: 99999999999999999997/99999999999999999999)"), false},
		{"nil", nil, false},
		{"a VOD row", with(row("(V: 11017/11019 A: 11017/11019)"), func(j *database.Job) { j.IsVod = true }), false},
		{"a Twitch row", with(row(owner), func(j *database.Job) { j.Platform = "twitch" }), false},
		{"not flagged", with(row(owner), func(j *database.Job) { j.IncompleteTail = false }), false},
		{"Error", with(row(owner), func(j *database.Job) { j.Status = database.StatusError }), false},
		{"Muxing", with(row(owner), func(j *database.Job) { j.Status = database.StatusMuxing }), false},
		{"Downloading", with(row(owner), func(j *database.Job) { j.Status = database.StatusDownloading }), false},
	} {
		if _, _, ok := toleratedTailRow(tc.job); ok != tc.want {
			t.Errorf("%s: toleratedTailRow ok = %v, want %v", tc.name, ok, tc.want)
		}
	}
	if last, head, _ := toleratedTailRow(row(owner)); last != 11017 || head != 11019 {
		t.Errorf("the owner's row read as last=%d head=%d, want 11017 and 11019", last, head)
	}
}

// tailClearLines counts the pass's Info lines for jobID, and returns the
// "staging" value of the last one.
func tailClearLines(log *lockedLogger, jobID string) (n int, staging string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, line := range log.lines {
		if msg, _ := line[0].(string); !strings.HasPrefix(msg, "cleared an incomplete-tail flag") {
			continue
		}
		var id, st string
		for i := 1; i+1 < len(line); i += 2 {
			switch line[i] {
			case "jobID":
				id, _ = line[i+1].(string)
			case "staging":
				st, _ = line[i+1].(string)
			}
		}
		if id == jobID {
			n++
			staging = st
		}
	}
	return n, staging
}

// TestBootClearsToleratedTails pins the boot pass over real rows and real
// staging directories: a matching row loses its flag, reads as a complete
// job, and has its staging put through the post-mux cleanup — removed unless
// another keep holds. A row that does not match, an active row and a job
// whose staging another operation holds are not touched at all. One Info
// line per cleared job; a second boot does nothing.
//
// Mutants: an unconditional RemoveAll (the aside, unmuxed-part, chat and
// missing-archive rows lose what they keep); skipping the claim (the held row
// is cleared under the operation holding it); leaving the flag set (the
// second pass logs again); clearing a VOD row; the margin at 1 (the owner's
// row keeps its flag) or 3 (the gap-3 row is cleared).
func TestBootClearsToleratedTails(t *testing.T) {
	const owner = "(V: 11017/11019 A: 11017/11019 C: 17062)"
	type fixture struct {
		progress  string
		status    database.JobStatus
		isVod     bool
		chat      database.ChatStatus
		noArchive bool
		noStaging bool
		root      []string // files in the staging root
		seg1      bool     // an unrecorded part 1 with media
		held      bool     // another operation holds the staging claim
	}
	media := []string{"video_stream", "audio_stream", "video_stream.resume.json"}
	plus := func(names ...string) []string { return append(names, media...) }
	for _, tc := range []struct {
		name        string
		f           fixture
		wantCleared bool
		wantGone    bool     // the staging dir is removed
		wantKept    []string // staging files that must survive
		wantStaging string   // the Info line's "staging" value ("" = not checked)
	}{
		{"the owner's row", fixture{progress: owner, root: media}, true, true, nil, "removed"},
		{"gap 1", fixture{progress: "(V: 11018/11019 A: 11018/11019)", root: media}, true, true, nil, "removed"},
		{"staging already gone", fixture{progress: owner, noStaging: true}, true, true, nil, "none on disk"},
		{"a set-aside recording keeps the staging", fixture{progress: owner, root: plus("video_stream.restart-1700000000")},
			true, false, plus("video_stream.restart-1700000000"), "kept: a set-aside recording was never merged into the archive"},
		{"an unmuxed part keeps the staging", fixture{progress: owner, root: media, seg1: true},
			true, false, media, "kept: a captured part is still unmuxed"},
		{"pending chat prunes to the chat capture", fixture{progress: owner, chat: database.ChatIncomplete, root: plus("chat.json", "chat.json.resume.json")},
			true, false, []string{"chat.json", "chat.json.resume.json"}, "pruned to the chat capture: chat ended incomplete"},
		{"the archive is not on disk", fixture{progress: owner, noArchive: true, root: media}, true, false, media, ""},

		{"a != c", fixture{progress: "(V: 11017/11019 A: 11016/11019 C: 1)", root: media}, false, false, media, ""},
		{"b != d", fixture{progress: "(V: 11017/11019 A: 11017/11018 C: 1)", root: media}, false, false, media, ""},
		{"gap 0", fixture{progress: "(V: 11019/11019 A: 11019/11019 C: 1)", root: media}, false, false, media, ""},
		{"gap 3", fixture{progress: "(V: 11016/11019 A: 11016/11019 C: 1)", root: media}, false, false, media, ""},
		{"malformed", fixture{progress: "V: 11017/11019 A: 11017/11019", root: media}, false, false, media, ""},
		{"a VOD row", fixture{progress: "(V: 11018/11019 A: 11018/11019)", isVod: true, root: media}, false, false, media, ""},
		{"an Error row", fixture{progress: owner, status: database.StatusError, root: media}, false, false, media, ""},
		{"a Muxing row", fixture{progress: owner, status: database.StatusMuxing, root: media}, false, false, media, ""},
		{"another operation holds the staging", fixture{progress: owner, root: media, held: true}, false, false, media, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db, log, stagingBase := bootSweepWorker(t)
			const id = "j-tail"
			archive := filepath.Join(t.TempDir(), "x.mp4")
			if !tc.f.noArchive {
				writeFixtureFile(t, archive, "archive")
			}
			staging := filepath.Join(stagingBase, id)
			if !tc.f.noStaging {
				for _, n := range tc.f.root {
					writeFixtureFile(t, filepath.Join(staging, n), "\x00\x00\x00\x18ftypdash")
				}
				if tc.f.seg1 {
					writeFixtureFile(t, filepath.Join(staging, "seg_1", "video_stream"), "\x00\x00\x00\x18ftypdash")
				}
			}
			status := tc.f.status
			if status == "" {
				status = database.StatusFinished
			}
			if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "youtube", Status: status, IsVod: tc.f.isVod,
				ChatStatus: tc.f.chat, OutputFile: archive, DownloadStartedAt: downloadStarted}); err != nil {
				t.Fatal(err)
			}
			db.UpdateJobFields(id, map[string]any{"incomplete_tail": true, "progress": tc.f.progress, "percent": 99.98})
			before, _ := db.GetJob(id)
			if before.IsVod != tc.f.isVod || !before.IncompleteTail || before.ChatStatus != tc.f.chat && tc.f.chat != "" {
				t.Fatalf("fixture row: isVod=%v incompleteTail=%v chat=%q", before.IsVod, before.IncompleteTail, before.ChatStatus)
			}
			if tc.f.held {
				release, err := w.claimJobOperation(id, opMux)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}

			w.clearToleratedTails()

			row, err := db.GetJob(id)
			if err != nil || row == nil {
				t.Fatalf("GetJob: %v", err)
			}
			if cleared := !row.IncompleteTail; cleared != tc.wantCleared {
				t.Fatalf("flag cleared = %v, want %v", cleared, tc.wantCleared)
			}
			lines, stagingSaid := tailClearLines(log, id)
			if tc.wantCleared {
				if row.Progress != "" || row.Percent != 100 {
					t.Errorf("row reads progress=%q percent=%v, want a complete job's (\"\" and 100)", row.Progress, row.Percent)
				}
				if row.Status != database.StatusFinished {
					t.Errorf("status = %s, want it left Finished", row.Status)
				}
				if lines != 1 {
					t.Errorf("%d Info lines for the cleared job, want 1: %v", lines, log.lines)
				}
				if tc.wantStaging != "" && stagingSaid != tc.wantStaging {
					t.Errorf("the line says staging %q, want %q", stagingSaid, tc.wantStaging)
				}
			} else {
				if row.Progress != tc.f.progress || row.Percent != 99.98 || row.Status != status {
					t.Errorf("an untouched row changed: progress=%q percent=%v status=%s", row.Progress, row.Percent, row.Status)
				}
				if lines != 0 {
					t.Errorf("a row left alone was logged as cleared: %v", log.lines)
				}
			}
			_, statErr := os.Stat(staging)
			if gone := os.IsNotExist(statErr); gone != tc.wantGone {
				t.Errorf("staging removed = %v, want %v (the line said %q)", gone, tc.wantGone, stagingSaid)
			}
			for _, n := range tc.wantKept {
				if !fileExists(filepath.Join(staging, n)) {
					t.Errorf("%s was deleted from staging that had to keep it", n)
				}
			}
			if tc.f.chat == database.ChatIncomplete && fileExists(filepath.Join(staging, "video_stream")) {
				t.Error("the muxed media was kept beside the pending chat capture; a complete job's cleanup prunes it")
			}

			// A second boot: nothing more happens.
			w.clearToleratedTails()
			again, _ := db.GetJob(id)
			if again.IncompleteTail != row.IncompleteTail || again.Progress != row.Progress || again.UpdatedAt != row.UpdatedAt {
				t.Errorf("the second pass changed the row: %+v -> %+v", row, again)
			}
			if n, _ := tailClearLines(log, id); n != lines {
				t.Errorf("the second pass logged again: %d lines, was %d", n, lines)
			}
			for _, n := range tc.wantKept {
				if !fileExists(filepath.Join(staging, n)) {
					t.Errorf("the second pass deleted %s", n)
				}
			}
		})
	}
}

// TestBootTailClearRunsFromStart: Start runs the pass on its boot goroutine.
func TestBootTailClearRunsFromStart(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	archive := filepath.Join(t.TempDir(), "x.mp4")
	writeFixtureFile(t, archive, "archive")
	staging := filepath.Join(stagingBase, "j-start")
	writeFixtureFile(t, filepath.Join(staging, "video_stream"), "\x00\x00\x00\x18ftypdash")
	if _, err := db.AddJob(&database.Job{ID: "j-start", VideoID: "j-start", Platform: "youtube", Status: database.StatusFinished,
		OutputFile: archive, DownloadStartedAt: downloadStarted}); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-start", map[string]any{"incomplete_tail": true, "progress": "(V: 11017/11019 A: 11017/11019 C: 17062)"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Start(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, _ := db.GetJob("j-start")
		_, err := os.Stat(staging)
		if row != nil && !row.IncompleteTail && os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Start did not clear the flag and remove the staging: flagged=%v statErr=%v", row != nil && row.IncompleteTail, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	<-done
}
