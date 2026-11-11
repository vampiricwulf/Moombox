package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// tailAt is a stream that stopped with its last written segment `short`
// below head, as the engine reports it: behind head exactly when the next
// segment to fetch (last+1) is still below the head.
func tailAt(head, short int) streamTail {
	last := head - short
	return streamTail{present: true, behindHead: last+1 < head, lastSeq: last, head: head, headCurrent: true}
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
// rows turn complete); without the head > 0 check (the unknown-head row turns
// complete); without the current-head check (the stale-head rows turn
// complete).
func TestIncompleteTailEndOfStreamRule(t *testing.T) {
	const head = 11019
	over := liveEnd{confirmedOver: true}
	interrupted := func(s streamTail) streamTail { s.interrupted = true; return s }
	stale := func(s streamTail) streamTail { s.headCurrent = false; return s }
	// A stream whose head was never learned: the engine cannot latch it
	// behind (head > 0 is part of that), so only its sibling is.
	noHead := streamTail{present: true, lastSeq: head - 2, head: -1, headCurrent: true}

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
		{"head-2, the video head not read since its last segment", tailFacts{video: stale(s2), audio: s2, end: over}, true},
		{"head-2, the audio head not read since its last segment", tailFacts{video: s2, audio: stale(s2), end: over}, true},
		{"video head-2, the audio head never learned", tailFacts{video: s2, audio: noHead, end: over}, true},

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
// streams at one segment under one head, exactly 2 short of it. One short is
// not read so: that row's downloader stood at its head, which the old rule
// never flagged, so interruption evidence flagged it and its staging was kept
// for a resume.
//
// Mutants: the margin at 1 or 3; dropping a == c, b == d, the behind-head
// floor (the gap-1 and gap-0 rows are read), the IsVod, Finished or flag
// checks; an unanchored pattern.
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
		{"gap 1: at the head, flagged by an interruption", row("(V: 11018/11019 A: 11018/11019 C: 3)"), false},
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

// ownerTailLine is the progress line the owner's five rows carry.
const ownerTailLine = "(V: 11017/11019 A: 11017/11019 C: 17062)"

// tailMedia is a flagged capture's staging root.
var tailMedia = []string{"video_stream", "audio_stream", "video_stream.resume.json"}

// flaggedTailRow adds a Finished, flagged YouTube live row with the given
// progress line, its archive on disk and tailMedia in its staging, and
// returns the staging dir.
func flaggedTailRow(t *testing.T, db *database.Database, stagingBase, id, progress string) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "x.mp4")
	writeFixtureFile(t, archive, "archive")
	staging := filepath.Join(stagingBase, id)
	for _, n := range tailMedia {
		writeFixtureFile(t, filepath.Join(staging, n), "\x00\x00\x00\x18ftypdash")
	}
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "youtube", Status: database.StatusFinished,
		OutputFile: archive, DownloadStartedAt: downloadStarted}); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields(id, map[string]any{"incomplete_tail": true, "progress": progress, "percent": 99.98})
	return staging
}

// tailRowUntouched fails the test unless the row is still flagged with its
// line and every tailMedia file is still in its staging.
func tailRowUntouched(t *testing.T, db *database.Database, id, staging, progress string) {
	t.Helper()
	row, err := db.GetJob(id)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !row.IncompleteTail || row.Progress != progress || row.Status != database.StatusFinished {
		t.Errorf("the row changed: status=%s incompleteTail=%v progress=%q", row.Status, row.IncompleteTail, row.Progress)
	}
	for _, n := range tailMedia {
		if !fileExists(filepath.Join(staging, n)) {
			t.Errorf("%s was deleted from the staging", n)
		}
	}
}

// TestBootClearsToleratedTails pins the boot pass over real rows and real
// staging directories: a matching row loses its flag, reads as a complete
// job, and has its staging put through the post-mux cleanup — removed unless
// another keep holds. A row that does not match, an active row, a row whose
// archive is not on disk and a job whose staging another operation holds are
// not touched at all. One Info line per cleared job; a second boot does
// nothing.
//
// Mutants: an unconditional RemoveAll (the aside, unmuxed-part and chat rows
// lose what they keep); skipping the claim (the held row is cleared under the
// operation holding it); skipping the output-claim check (the row a finalize
// holds is cleared); leaving the flag set (the second pass logs again);
// clearing a VOD row; the margin at 1 (the owner's row keeps its flag) or 3
// (the gap-3 row is cleared); clearing the flag of a row whose archive is
// missing (it loses its flag and its Resume, its staging an orphan); dropping
// the recovered-aside keep (that row's staging is removed with the only copy
// of the aside in it).
func TestBootClearsToleratedTails(t *testing.T) {
	const owner = ownerTailLine
	type fixture struct {
		progress    string
		status      database.JobStatus
		isVod       bool
		chat        database.ChatStatus
		noArchive   bool
		noStaging   bool
		root        []string // files in the staging root
		seg1        bool     // an unrecorded part 1 with media
		lostSibling bool     // a set-aside recording marked recovered to a file that is gone
		held        bool     // another operation holds the staging claim
		finalizing  bool     // a finalize holds an output claim on the staging dir
	}
	media := tailMedia
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
		{"staging already gone", fixture{progress: owner, noStaging: true}, true, true, nil, "none on disk"},
		{"a set-aside recording keeps the staging", fixture{progress: owner, root: plus("video_stream.restart-1700000000")},
			true, false, plus("video_stream.restart-1700000000"), "kept: a set-aside recording was never merged into the archive"},
		{"a recovered aside whose sibling is gone keeps the staging", fixture{progress: owner, root: media, lostSibling: true},
			true, false, plus("video_stream.restart-1700000009"), "kept: a set-aside recording marked recovered is still in it"},
		{"an unmuxed part keeps the staging", fixture{progress: owner, root: media, seg1: true},
			true, false, media, "kept: a captured part is still unmuxed"},
		{"pending chat prunes to the chat capture", fixture{progress: owner, chat: database.ChatIncomplete, root: plus("chat.json", "chat.json.resume.json")},
			true, false, []string{"chat.json", "chat.json.resume.json"}, "pruned to the chat capture: chat ended incomplete"},

		{"the archive is not on disk", fixture{progress: owner, noArchive: true, root: media}, false, false, media, ""},
		{"gap 1", fixture{progress: "(V: 11018/11019 A: 11018/11019)", root: media}, false, false, media, ""},
		{"a != c", fixture{progress: "(V: 11017/11019 A: 11016/11019 C: 1)", root: media}, false, false, media, ""},
		{"b != d", fixture{progress: "(V: 11017/11019 A: 11017/11018 C: 1)", root: media}, false, false, media, ""},
		{"gap 0", fixture{progress: "(V: 11019/11019 A: 11019/11019 C: 1)", root: media}, false, false, media, ""},
		{"gap 3", fixture{progress: "(V: 11016/11019 A: 11016/11019 C: 1)", root: media}, false, false, media, ""},
		{"malformed", fixture{progress: "V: 11017/11019 A: 11017/11019", root: media}, false, false, media, ""},
		{"a VOD row", fixture{progress: owner, isVod: true, root: media}, false, false, media, ""},
		{"an Error row", fixture{progress: owner, status: database.StatusError, root: media}, false, false, media, ""},
		{"a Muxing row", fixture{progress: owner, status: database.StatusMuxing, root: media}, false, false, media, ""},
		{"another operation holds the staging", fixture{progress: owner, root: media, held: true}, false, false, media, ""},
		{"a finalize holds the staging", fixture{progress: owner, root: media, finalizing: true}, false, false, media, ""},
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
				if tc.f.lostSibling {
					aside := filepath.Join(staging, "video_stream.restart-1700000009")
					writeFixtureFile(t, aside, "set aside")
					writeFixtureFile(t, aside+asideRecoveredMarker, filepath.Join(t.TempDir(), "gone.mp4"))
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
			if tc.f.finalizing {
				defer claimOutputStem("j-other", staging)()
				if outputClaimOwner(staging) == "" {
					t.Fatal("fixture: the output claim does not cover the staging dir")
				}
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

// TestBootTailPassIsOneShot: the pass lists its rows once. A row flagged
// afterwards carries the same progress line when the rule itself kept it
// incomplete two short of its head (an interruption, a wait for a resume, an
// unconfirmed end), and a later boot must leave it, its staging and its
// resume state alone.
//
// Mutants: listing on every boot (the later row is cleared and its staging
// deleted); not writing the record when no row matched (likewise).
func TestBootTailPassIsOneShot(t *testing.T) {
	w, db, log, stagingBase := bootSweepWorker(t)
	old := flaggedTailRow(t, db, stagingBase, "j-old", ownerTailLine)

	w.clearToleratedTails() // the first boot of a version that has the rule

	if row, _ := db.GetJob("j-old"); row.IncompleteTail {
		t.Fatal("the row from before the rule kept its flag")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the row from before the rule kept its staging")
	}
	doc, listed, err := readTailPass(w.tailPassPath())
	if err != nil || !listed || len(doc.Pending) != 0 {
		t.Fatalf("the pass's record: listed=%v pending=%v err=%v, want it written with nothing pending", listed, doc.Pending, err)
	}

	// A capture the rule flags, interrupted two short of its head.
	kept := flaggedTailRow(t, db, stagingBase, "j-new", ownerTailLine)
	w.clearToleratedTails() // a later boot
	tailRowUntouched(t, db, "j-new", kept, ownerTailLine)
	if n, _ := tailClearLines(log, "j-new"); n != 0 {
		t.Error("a row flagged under the rule was logged as cleared")
	}

	// And with no matching row on the first boot, the record is still written.
	w2, db2, _, base2 := bootSweepWorker(t)
	w2.clearToleratedTails()
	kept2 := flaggedTailRow(t, db2, base2, "j-new", ownerTailLine)
	w2.clearToleratedTails()
	tailRowUntouched(t, db2, "j-new", kept2, ownerTailLine)
}

// TestBootTailPassFinishesAnInterruptedPass: rows a pass listed and did not
// reach before its process died are still pending in the record, and the
// next boot clears them — but only a row still as it was listed. One written
// since (resumed and flagged again, say) is left alone, and so is every row
// when the record cannot be read.
//
// Mutants: ignoring the pending rows of an existing record (the unreached
// row keeps its flag); dropping the updated_at comparison (the changed row
// is cleared); treating an unreadable record as none (the rows are listed
// again and cleared).
func TestBootTailPassFinishesAnInterruptedPass(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	unreached := flaggedTailRow(t, db, stagingBase, "j-unreached", ownerTailLine)
	changed := flaggedTailRow(t, db, stagingBase, "j-changed", ownerTailLine)
	unlisted := flaggedTailRow(t, db, stagingBase, "j-unlisted", ownerTailLine)
	row, _ := db.GetJob("j-unreached")
	if err := writeTailPass(w.tailPassPath(), tailPassDoc{Pending: map[string]string{
		"j-unreached": row.UpdatedAt,
		"j-changed":   "2026-11-01T00:00:00Z", // listed before the row's last write
		"j-deleted":   "2026-11-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}

	w.clearToleratedTails()

	if row, _ := db.GetJob("j-unreached"); row.IncompleteTail {
		t.Error("a listed row the earlier pass never reached kept its flag")
	}
	if _, err := os.Stat(unreached); !os.IsNotExist(err) {
		t.Error("a listed row the earlier pass never reached kept its staging")
	}
	tailRowUntouched(t, db, "j-changed", changed, ownerTailLine)
	tailRowUntouched(t, db, "j-unlisted", unlisted, ownerTailLine)
	if doc, listed, err := readTailPass(w.tailPassPath()); err != nil || !listed || len(doc.Pending) != 0 {
		t.Errorf("the record after the pass: listed=%v pending=%v err=%v, want nothing pending", listed, doc.Pending, err)
	}

	// A record that cannot be read: nothing is listed, nothing is cleared.
	w2, db2, log2, base2 := bootSweepWorker(t)
	kept := flaggedTailRow(t, db2, base2, "j-tail", ownerTailLine)
	writeFixtureFile(t, w2.tailPassPath(), "{not json")
	w2.clearToleratedTails()
	tailRowUntouched(t, db2, "j-tail", kept, ownerTailLine)
	if got := readFileOr(t, w2.tailPassPath()); got != "{not json" {
		t.Errorf("the unreadable record was rewritten: %q", got)
	}
	warned := false
	log2.mu.Lock()
	for _, line := range log2.lines {
		if msg, _ := line[0].(string); strings.Contains(msg, "record could not be read") {
			warned = true
		}
	}
	log2.mu.Unlock()
	if !warned {
		t.Error("an unreadable record was not logged")
	}
}

// TestBootTailClearRereadsTheRowUnderTheClaim: what the pass decides, it
// decides on the row as it reads under the staging claim, and it deletes
// only after its conditional write has applied.
//
// Mutants: drop the re-check under the claim (the row that stopped matching
// is cleared and its staging deleted); ignore UpdateJobFieldsIf's answer (the
// row revived just before the write loses its staging to the cleanup).
func TestBootTailClearRereadsTheRowUnderTheClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seam   *func(string)
		change map[string]any
	}{
		// Still Finished and flagged, but no longer the end-of-stream shape.
		{"the line changed before the claim", &bootSweepClaimed, map[string]any{"progress": "(V: 11000/11019 A: 11000/11019)"}},
		// Revived between the read under the claim and the write.
		{"the status changed before the write", &tailClearChecked, map[string]any{"status": database.StatusDownloading}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db, log, stagingBase := bootSweepWorker(t)
			staging := flaggedTailRow(t, db, stagingBase, "j-tail", ownerTailLine)
			fired := false
			*tc.seam = func(id string) {
				fired = true
				db.UpdateJobFields(id, tc.change)
			}
			t.Cleanup(func() { *tc.seam = nil })

			w.clearToleratedTails()

			if !fired {
				t.Fatal("the seam did not fire")
			}
			row, _ := db.GetJob("j-tail")
			if !row.IncompleteTail {
				t.Error("the flag was cleared on a row that changed before the pass acted")
			}
			for _, n := range tailMedia {
				if !fileExists(filepath.Join(staging, n)) {
					t.Errorf("%s was deleted from the staging of a row that changed before the pass acted", n)
				}
			}
			if n, _ := tailClearLines(log, "j-tail"); n != 0 {
				t.Error("the row was logged as cleared")
			}
		})
	}
}

// TestBootTailClearLogsToTheJobsOwnLog: the line that says a flag was cleared
// and what became of the staging is in the job's own log, the one both UIs
// show, and the routing is handed back afterwards.
//
// Mutant: no TrackJobForLogs after the cleanup (cleanupStagingAfterMux's own
// bracket untracks the terminal row, and the line reaches the global log only).
func TestBootTailClearLogsToTheJobsOwnLog(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	w.logger = routingLogger{db: db}
	w.orchestrator.logger = routingLogger{db: db}
	flaggedTailRow(t, db, stagingBase, "j-tail", ownerTailLine)

	w.clearToleratedTails()

	got := strings.Join(db.GetJobLogs("j-tail"), "\n")
	if !strings.Contains(got, "cleared an incomplete-tail flag") || !strings.Contains(got, "removed") {
		t.Errorf("the job's own log does not carry the cleared line; it holds:\n%s", got)
	}
	before := len(db.GetJobLogs("j-tail"))
	db.RouteLogToJobs("after the pass, job j-tail")
	if len(db.GetJobLogs("j-tail")) != before {
		t.Error("the job is still tracked for log routing after the pass")
	}
}

// TestResumeWaitingOnTheTailClearIsNotApplied: a Resume accepted on a
// Finished, flagged row waits while the boot pass holds the job's staging
// claim (the TUI's and the monitor's auto-resume are not refused). The pass
// clears the flag and deletes the staging without changing the status, so
// the resume must read its gate again before it writes: applied, it would
// re-capture a complete, cleaned job from nothing.
//
// Mutant: ResumeJob without the second gate read (the row goes Downloading
// and is queued).
func TestResumeWaitingOnTheTailClearIsNotApplied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db, _, stagingBase := bootSweepWorker(t)
		flaggedTailRow(t, db, stagingBase, "j-tail", ownerTailLine)
		release, err := w.claimJobOperation("j-tail", opTailClear)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.ResumeJob("j-tail"); err != nil {
			t.Fatalf("ResumeJob = %v, want it accepted and waiting", err)
		}
		synctest.Wait()
		if row, _ := db.GetJob("j-tail"); row.Status != database.StatusFinished {
			t.Fatalf("status = %s while the pass holds the claim, want Finished", row.Status)
		}
		// What the pass does under its claim.
		if !db.UpdateJobFieldsIf("j-tail", database.StatusFinished, map[string]any{"incomplete_tail": false, "progress": "", "percent": 100.0}) {
			t.Fatal("fixture: the clear did not apply")
		}
		release()
		w.wg.Wait()

		row, _ := db.GetJob("j-tail")
		if row.Status != database.StatusFinished {
			t.Errorf("status = %s, want the complete job left Finished", row.Status)
		}
		if why := w.queuedOrRunning("j-tail"); why != "" {
			t.Errorf("the job was queued for a run: %s", why)
		}
	})

	// The gate read again must not refuse an ordinary waiting resume.
	synctest.Test(t, func(t *testing.T) {
		w, db, _, stagingBase := bootSweepWorker(t)
		flaggedTailRow(t, db, stagingBase, "j-tail", ownerTailLine)
		release, err := w.claimJobOperation("j-tail", opTailClear)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.ResumeJob("j-tail"); err != nil {
			t.Fatalf("ResumeJob = %v", err)
		}
		synctest.Wait()
		release() // the pass left this row flagged
		w.wg.Wait()
		if row, _ := db.GetJob("j-tail"); row.Status != database.StatusDownloading {
			t.Errorf("status = %s, want a still-flagged row resumed (Downloading)", row.Status)
		}
	})
}

// TestBootTailClearRunsFromStart: Start runs the pass on its boot goroutine,
// which Stop waits for.
func TestBootTailClearRunsFromStart(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	staging := flaggedTailRow(t, db, stagingBase, "j-start", ownerTailLine)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Start(ctx) }()
	cancel()
	<-done   // Start has launched the boot goroutine and returned
	w.Stop() // waits for that goroutine

	if row, _ := db.GetJob("j-start"); row == nil || row.IncompleteTail {
		t.Error("Start did not clear the flag")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("Start did not remove the staging: %v", err)
	}
}

// TestConfirmsStreamOver pins R-T1's first condition: only a post-live or VOD
// answer on a response YouTube did not refuse is its word that the stream is
// over. not_a_stream is what a refused response with no video details reads
// as — a stream that turned members-only mid-broadcast, the bot wall — and
// the stream behind it may still be live.
//
// Mutants: accept not_a_stream; drop the playability check.
func TestConfirmsStreamOver(t *testing.T) {
	for _, tc := range []struct {
		status youtube.StreamStatus
		play   youtube.PlayabilityError
		want   bool
	}{
		{youtube.StreamPostLive, youtube.PlayabilityOK, true},
		{youtube.StreamVOD, youtube.PlayabilityOK, true},
		{youtube.StreamNotAStream, youtube.PlayabilityOK, false},
		{youtube.StreamNotAStream, youtube.PlayabilityMembersOnly, false},
		{youtube.StreamNotAStream, youtube.PlayabilityLoginRequired, false},
		{youtube.StreamPostLive, youtube.PlayabilityMembersOnly, false},
		{youtube.StreamVOD, youtube.PlayabilityLoginRequired, false},
		{youtube.StreamPostLive, "", false}, // no verdict read at all
		{youtube.StreamLive, youtube.PlayabilityOK, false},
		{youtube.StreamUpcoming, youtube.PlayabilityOK, false},
	} {
		if got := confirmsStreamOver(&youtube.VideoInfo{StreamStatus: tc.status, PlayabilityError: tc.play}); got != tc.want {
			t.Errorf("confirmsStreamOver(%s, %q) = %v, want %v", tc.status, tc.play, got, tc.want)
		}
	}
	if confirmsStreamOver(nil) {
		t.Error("confirmsStreamOver(nil) = true")
	}
}

// liveEndHarness is what the live-loop tests below share: a worker, a pinned
// job (no quality monitor) and its context, with the loop's player-response
// fetch answered by info and its downloader run returning at once.
func liveEndHarness(t *testing.T, info func() (*youtube.VideoInfo, error)) (*DownloadOrchestrator, *database.Database, *JobContext) {
	t.Helper()
	w, db := testWorkerSetup(t)
	pin := 299
	job := &database.Job{ID: "end1", VideoID: "end1", URL: "u", Platform: "youtube", Status: database.StatusDownloading, SelectedVideoItag: &pin}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	o := w.orchestrator
	o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
		StagingDir: t.TempDir(), OutputDir: t.TempDir(), Filename: "end1", Logger: &discardLogger{},
		Interruption: &interruptionSignal{}, Run: &runRecord{}}
	origInfo, origRun := liveVideoInfo, runLiveDownloaders
	t.Cleanup(func() { liveVideoInfo, runLiveDownloaders = origInfo, origRun })
	liveVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) { return info() }
	runLiveDownloaders = func(*DownloadOrchestrator, context.Context, *DownloadResult) error { return nil }
	return o, db, jobCtx
}

// TestLiveLoopConfirmsTheEndOnlyOnYouTubesWord drives the live loop
// (runLiveStreamDownload) to its end with real downloaders that stopped two
// short of their head, the owner's shape, and checks both the liveEnd it
// returns and the flag that end persists. Post-live or a VOD, unrefused, is
// the end confirmed and the recording complete. An end the loop reads off a
// refusal — a stream turned members-only mid-broadcast answers not_a_stream,
// and refuses its segments for the whole timeout the same way — or off a
// status it did not expect is not, and the recording stays incomplete with
// its staging and resume state.
//
// Mutants: confirmedOver = true for every status in the ended case (the
// refused rows read complete); the line deleted (the post-live row reads
// incomplete: the fix is off); confirmedOver = true in the default branch
// (the unexpected-status row reads complete).
func TestLiveLoopConfirmsTheEndOnlyOnYouTubesWord(t *testing.T) {
	const head = 8
	for _, tc := range []struct {
		name          string
		info          youtube.VideoInfo
		wantConfirmed bool
	}{
		{"post-live", youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayabilityError: youtube.PlayabilityOK}, true},
		{"a VOD", youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayabilityError: youtube.PlayabilityOK}, true},
		{"not_a_stream, members-only: a refusal", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityMembersOnly}, false},
		{"not_a_stream, login required: a refusal", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityLoginRequired}, false},
		{"not_a_stream, unrefused", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityOK}, false},
		{"post-live on a refused response", youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayabilityError: youtube.PlayabilityLoginRequired}, false},
		{"an unexpected status", youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, PlayabilityError: youtube.PlayabilityOK}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, db, jobCtx := liveEndHarness(t, func() (*youtube.VideoInfo, error) { cp := tc.info; return &cp, nil })
			result := &DownloadResult{
				VideoDownloader: newEndedTestDownloader(t, head, head-2),
				AudioDownloader: newEndedTestDownloader(t, head, head-2),
				HasVideo:        true, HasAudio: true, VideoItag: 299, AudioItag: 140,
			}
			tracker := NewProgressTracker(db, jobCtx.Job.ID, discardLogger{}, 0)
			var lastSeg atomicTimeValue
			got, end, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, false, &tc.info, result, tracker,
				func() bool { return false }, &lastSeg)
			tracker.Close()
			if err != nil {
				t.Fatalf("runLiveStreamDownload = %v", err)
			}
			if end.confirmedOver != tc.wantConfirmed || end.waitedForResume {
				t.Errorf("liveEnd = %+v, want confirmedOver=%v and no wait", end, tc.wantConfirmed)
			}
			incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobCtx.Job.ID, got, end)
			row, _ := db.GetJob(jobCtx.Job.ID)
			if incomplete != !tc.wantConfirmed || row.IncompleteTail != !tc.wantConfirmed {
				t.Errorf("incomplete_tail = %v (persisted %v), want %v", incomplete, row.IncompleteTail, !tc.wantConfirmed)
			}
		})
	}
}

// TestLiveLoopForcedEndsAreNotConfirmed: the two ends the loop forces after
// its budget of looks — YouTube still calling the stream live with no
// segments, and a status that stayed unreadable — are the loop giving up,
// not YouTube's word.
//
// Mutants: confirmedOver = true at "forcing end", at the unreadable-status
// break, or initialised true.
func TestLiveLoopForcedEndsAreNotConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		info func() (*youtube.VideoInfo, error)
	}{
		{"still live with no segments", func() (*youtube.VideoInfo, error) {
			return &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayabilityError: youtube.PlayabilityOK}, nil
		}},
		{"the status stayed unreadable", func() (*youtube.VideoInfo, error) { return nil, errors.New("HTTP 503") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				looks := 0
				o, db, jobCtx := liveEndHarness(t, func() (*youtube.VideoInfo, error) { looks++; return tc.info() })
				stopped := func(name string) *engine.SegmentDownloader {
					return engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(jobCtx.StagingDir, name),
						BaseURL: "https://r1.googlevideo.com/videoplayback?itag=299", StartSeq: 42, ForceStartSeq: true})
				}
				result := &DownloadResult{VideoDownloader: stopped("video_stream"), AudioDownloader: stopped("audio_stream"),
					HasVideo: true, HasAudio: true, VideoItag: 299, AudioItag: 140}
				live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayabilityError: youtube.PlayabilityOK}
				tracker := NewProgressTracker(db, jobCtx.Job.ID, discardLogger{}, 0)
				var lastSeg atomicTimeValue
				_, end, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, false, live, result, tracker,
					func() bool { return false }, &lastSeg)
				tracker.Close()
				time.Sleep(2 * activityUpdateInterval)
				if err != nil {
					t.Fatalf("runLiveStreamDownload = %v", err)
				}
				if looks < maxConsecutiveLiveChecks {
					t.Fatalf("the loop ended after %d looks, want it to spend its budget of %d: this is not the forced end", looks, maxConsecutiveLiveChecks)
				}
				if end.confirmedOver {
					t.Error("a forced end read as YouTube's confirmation that the stream is over")
				}
			})
		})
	}
}
