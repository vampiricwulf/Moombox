package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The end-of-stream tail rule (owner ruling R-T1).
//
// Once a YouTube broadcast is over, the head it reports (X-Head-Seqnum) can
// stand up to engine.EndOfStreamHeadMargin segments past the last segment it
// will ever serve. A live capture that recorded everything then stops with
// its last written segment at head-2, retries the two that do not exist for
// its whole timeout, and finalizes "behind head". Flagging that recording
// incomplete_tail kept its whole staging folder for nothing.
//
// Two pieces live here: the verdict a finished download gets (incompleteTail),
// and the one-time boot pass over rows flagged before the rule
// (clearToleratedTails).

// liveEnd is what the live loop alone knows about how its capture ended. The
// zero value is what the VOD branch passes: no wait, no confirmation.
type liveEnd struct {
	// waitedForResume: the loop gave up out of an unresolved wait for an
	// interrupted broadcast (see finalizeIncompleteTail).
	waitedForResume bool
	// confirmedOver: the loop's own end verification got YouTube's answer
	// that the stream is over (confirmsStreamOver: post-live or a VOD, on a
	// response that was not refused). A forced end leaves it false, and so
	// does an end read off a refusal.
	confirmedOver bool
}

// streamTail is where one downloader stopped.
type streamTail struct {
	present     bool // the job has this stream (audio-only and video-only jobs lack one)
	behindHead  bool // engine.SegmentDownloader.FinalizedBehindHead
	interrupted bool // engine.SegmentDownloader.FinalizedDuringInterruption
	lastSeq     int  // the last segment written
	head        int  // the head the downloader tracked (-1 when never learned)
	headCurrent bool // engine.SegmentDownloader.HeadReadSinceLastSegment
}

// tailFacts is everything the incomplete-tail verdict reads.
type tailFacts struct {
	video, audio streamTail
	end          liveEnd
}

func downloaderTail(d *engine.SegmentDownloader) streamTail {
	if d == nil {
		return streamTail{}
	}
	return streamTail{
		present:     true,
		behindHead:  d.FinalizedBehindHead(),
		interrupted: d.FinalizedDuringInterruption(),
		lastSeq:     d.LastSeq(),
		head:        d.HeadSeq(),
		headCurrent: d.HeadReadSinceLastSegment(),
	}
}

func resultTailFacts(result *DownloadResult, end liveEnd) tailFacts {
	return tailFacts{
		video: downloaderTail(result.VideoDownloader),
		audio: downloaderTail(result.AudioDownloader),
		end:   end,
	}
}

// incompleteTail is the one incomplete-tail rule, for the live branch, the
// split capture's final part (muxLiveFinalPart) and the VOD branch alike.
//
// A recording is incomplete when a stream finalized during an interruption
// or the live loop gave up out of a wait for one (Tier 2: the sidecar was
// kept for a resume), or when either stream finalized behind its head —
// video and audio track their heads separately, and a tail missing on one
// truncates the mux. One exception, R-T1: behind the head is complete when
// the stop has the shape of an ended broadcast (endOfStreamTail).
func incompleteTail(f tailFacts) bool {
	if f.video.interrupted || f.audio.interrupted || f.end.waitedForResume {
		return true
	}
	if !f.video.behindHead && !f.audio.behindHead {
		return false
	}
	return !endOfStreamTail(f)
}

// endOfStreamTail reports whether a behind-head stop is the end of the
// broadcast and not a missing tail. All of these must hold:
//
//   - YouTube confirmed the stream over, to the live loop itself. The VOD
//     branch never says so, and a downloader that stops near the head while
//     the loop is still deciding is judged as before.
//   - Each stream's last written segment is within
//     engine.EndOfStreamHeadMargin of the head it tracked, and that head is
//     current: YouTube reported it after the last segment landed. A head
//     from before a stall in which nothing answered says nothing about how
//     far the broadcast ran on.
//   - Video and audio stopped at the same segment. Two streams that end
//     apart are not an end of stream; one of them lost something.
//
// The caller has already ruled out interruption evidence, and the engine
// latches behind-head only after retrying for its whole timeout.
func endOfStreamTail(f tailFacts) bool {
	if !f.end.confirmedOver {
		return false
	}
	for _, s := range []streamTail{f.video, f.audio} {
		if s.present && (s.head <= 0 || !s.headCurrent || !engine.WithinEndOfStreamMargin(s.lastSeq, s.head)) {
			return false
		}
	}
	if f.video.present && f.audio.present && f.video.lastSeq != f.audio.lastSeq {
		return false
	}
	return true
}

// confirmsStreamOver reports whether a player response is YouTube's word that
// the stream is over: post-live or a VOD, on a response it did not refuse.
//
// not_a_stream is not that word, although the live loop ends on it too. It is
// how a refused response with no video details classifies (classifyStream):
// the bot wall, a rate limit, a stream that turned members-only or private
// mid-broadcast. A stream behind such a refusal may still be live, and its
// segments are refused the same way, which leaves the downloader a segment or
// two below the head for the whole timeout — the end-of-stream shape exactly.
// Nor is any refused response, whatever status it classifies to.
func confirmsStreamOver(info *youtube.VideoInfo) bool {
	if info == nil || info.PlayabilityError != youtube.PlayabilityOK {
		return false
	}
	return info.StreamStatus == youtube.StreamPostLive || info.StreamStatus == youtube.StreamVOD
}

// opTailClear holds a job's staging claim while the boot pass clears a
// tolerated tail flag and cleans the staging it kept.
const opTailClear jobOp = "end-of-stream tail clear"

// tailProgressLine is the line a live DASH capture's tracker leaves on the
// row (ProgressTracker.buildProgressString): last written segment over head,
// for video and audio, then the chat count.
var tailProgressLine = regexp.MustCompile(`^\(V: (\d+)/(\d+) A: (\d+)/(\d+)(?: C: \d+)?\)$`)

// toleratedTailRow reports whether a row flagged incomplete_tail before the
// rule existed has the end-of-stream shape, read off its saved progress line
// "(V: a/b A: c/d ...)": both streams at the same segment (a == c) under the
// same head (b == d), exactly the margin short of it. It returns the segment
// and the head.
//
// The ruling's text for these rows is 1 <= b-a <= 2. Only b-a == 2 is taken.
// a is the last segment WRITTEN, so a row one short of its head had
// currentSeq == head: the old rule (cur < head) never flagged that. Such a
// row was flagged by interruption evidence, which the same ruling says keeps
// a recording incomplete, and its staging was kept for a resume. The gap the
// old behind-head rule did flag inside the margin is 2, the owner's rows.
//
// Only a Finished YouTube live capture is read this way. A VOD download's
// incomplete line counts the NEXT segment (incompleteProgressString), not
// the last written, so the same numbers mean one segment more missing; those
// rows keep their flag. A row two short cannot say whether an interruption
// also flagged it: the ruling takes the line as the evidence.
func toleratedTailRow(job *database.Job) (lastSeq, head int, ok bool) {
	if job == nil || job.Status != database.StatusFinished || !job.IncompleteTail ||
		job.IsVod || job.Platform == "twitch" {
		return 0, 0, false
	}
	m := tailProgressLine.FindStringSubmatch(job.Progress)
	if m == nil {
		return 0, 0, false
	}
	var n [4]int
	for i := range n {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return 0, 0, false
		}
		n[i] = v
	}
	if n[0] != n[2] || n[1] != n[3] {
		return 0, 0, false
	}
	lastSeq, head = n[0], n[1]
	// Behind the head as the old rule read it (currentSeq, lastSeq+1, below
	// the head), and inside the margin.
	if lastSeq+1 >= head || !engine.WithinEndOfStreamMargin(lastSeq, head) {
		return 0, 0, false
	}
	return lastSeq, head, true
}

// tailPassFileName is the boot pass's record, kept beside the database (as
// open-alerts.json is, so a Docker volume or a moved install carries both).
const tailPassFileName = "end-of-stream-tails.json"

// tailPassDoc is that file. Its existence says the pass has listed its rows:
// the rows flagged before the rule existed. Pending holds the listed rows the
// pass has not reached yet (job ID to the row's updated_at when listed); it
// is non-empty on disk only while a pass runs, or after a process that died
// during one.
//
// The pass is one-shot because a saved row cannot say WHY it was flagged. The
// rule itself still flags a capture two short of its head when an
// interruption, a wait for a resume or an unconfirmed end says so, and that
// row ends with the same progress line. A pass that re-read the rows on every
// boot would clear exactly the rows the rule keeps, and delete the staging
// and resume state kept for them. So the rows are listed once, on the first
// boot of a version that has the rule, and a listed row is cleared only while
// it is still as it was listed (updated_at unchanged).
type tailPassDoc struct {
	Pending map[string]string `json:"pending"`
}

func (w *DownloadWorker) tailPassPath() string {
	return filepath.Join(filepath.Dir(w.db.File()), tailPassFileName)
}

// readTailPass reads the record. listed is false, with no error, when the
// file is not there: the pass has not listed yet.
func readTailPass(path string) (doc tailPassDoc, listed bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return tailPassDoc{}, false, nil
	}
	if err != nil {
		return tailPassDoc{}, false, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return tailPassDoc{}, false, err
	}
	return doc, true, nil
}

func writeTailPass(path string, doc tailPassDoc) error {
	if doc.Pending == nil {
		doc.Pending = map[string]string{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return utils.WriteFileAtomic(path, data, 0o644)
}

// clearToleratedTails is the boot pass for rows flagged before the rule:
// every Finished job toleratedTailRow accepts has its flag cleared, its
// progress set to a complete job's, and its staging put through the post-mux
// cleanup. It needs no schema version: it lists the rows once and records
// that beside the database (tailPassDoc), so a later boot does nothing.
// Nothing is touched unless that record is on disk first; a record that
// cannot be read or written leaves every row as it is.
//
// It walks the rows, not the staging directory: a flagged row whose staging
// is already gone still shows the badge and still offers Resume.
func (w *DownloadWorker) clearToleratedTails() {
	path := w.tailPassPath()
	doc, listed, err := readTailPass(path)
	if err != nil {
		w.logger.Warn("boot: the end-of-stream tail pass's record could not be read; no incomplete-tail flag was changed",
			"path", path, "err", err)
		return
	}
	if !listed {
		jobs, err := w.db.GetAllJobs()
		if err != nil {
			w.logger.Warn("boot: could not read the jobs to clear end-of-stream tail flags; nothing changed", "err", err)
			return
		}
		doc.Pending = map[string]string{}
		for _, job := range jobs {
			if _, _, ok := toleratedTailRow(job); ok {
				doc.Pending[job.ID] = job.UpdatedAt
			}
		}
		if err := writeTailPass(path, doc); err != nil {
			w.logger.Warn("boot: the end-of-stream tail pass could not write its record; no incomplete-tail flag was changed, the next start tries again",
				"path", path, "err", err)
			return
		}
	}
	if len(doc.Pending) == 0 {
		return
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	ids := make([]string, 0, len(doc.Pending))
	for id := range doc.Pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		w.clearToleratedTail(id, doc.Pending[id], filepath.Join(stagingBase, id))
		// Reached, whatever became of it: a row the pass left flagged is not
		// tried again, since by a later boot nothing says it is still a row
		// from before the rule. A record that cannot be rewritten leaves the
		// row pending, which is harmless: a cleared row no longer matches,
		// and one that changes no longer carries the updated_at it was
		// listed with.
		delete(doc.Pending, id)
		if err := writeTailPass(path, doc); err != nil {
			w.logger.Warn("boot: the end-of-stream tail pass could not update its record", "path", path, "err", err)
		}
	}
}

// clearToleratedTail is clearToleratedTails for one listed row. The staging
// claim is taken first and the row read again under it; it is cleared only if
// it is still the row that was listed (updated_at unchanged) and still
// matches. The write applies only while the row is Finished, and staging is
// touched only after the write has applied.
//
// A Resume accepted before the claim was taken waits on it and runs once it
// is released. It re-reads its own gate then (ResumeJob), so it does not
// resume the row this has just made complete.
//
// A row left flagged is logged with the reason: another operation holds its
// staging, it changed since it was listed, or its archive is not where the
// row says. That last one is checked before the write, so such a row keeps
// its flag, its progress line, its staging and its Resume together until the
// owner acts; clearing the flag alone would leave the staging an orphan
// beside a row that no longer offers Resume.
func (w *DownloadWorker) clearToleratedTail(jobID, listedAt, stagingDir string) {
	if owner := outputClaimOwner(stagingDir); owner != "" {
		w.logger.Warn("boot: an incomplete-tail flag from before the end-of-stream margin was left set: a finalize holds the job's staging. Resume the job or delete its staging by hand",
			"jobID", jobID, "heldBy", owner)
		return
	}
	release, err := w.claimJobOperation(jobID, opTailClear)
	if err != nil {
		w.logger.Warn("boot: an incomplete-tail flag from before the end-of-stream margin was left set: another operation holds the job's staging. Resume the job or delete its staging by hand",
			"jobID", jobID, "err", err)
		return
	}
	defer release()
	if bootSweepClaimed != nil {
		bootSweepClaimed(jobID)
	}

	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil {
		return
	}
	lastSeq, head, ok := toleratedTailRow(job)
	if !ok || job.UpdatedAt != listedAt {
		w.logger.Info("boot: an incomplete-tail flag was left as it is: the row changed since the end-of-stream tail pass listed it",
			"jobID", jobID, "status", job.Status)
		return
	}
	if missing := missingArchiveFile(w.db, job); missing != "" {
		w.logger.Info("boot: an incomplete-tail flag from before the end-of-stream margin was left set: the archive is not where the row says, so the staging may be the only copy",
			"jobID", jobID, "missing", missing)
		return
	}
	// The row is Finished, so its per-job log routing is off; put the lines
	// below in the job's own log (the bracket cleanupStagingAfterMux uses).
	w.db.TrackJobForLogs(jobID)
	defer w.restoreLogRouting(jobID)

	if tailClearChecked != nil {
		tailClearChecked(jobID)
	}
	if !w.db.UpdateJobFieldsIf(jobID, database.StatusFinished, map[string]any{
		"incomplete_tail": false,
		"progress":        "",
		"percent":         100.0,
	}) {
		return
	}
	staging := w.cleanClearedTailStaging(jobID, stagingDir)
	// cleanupStagingAfterMux has the same bracket inside it, and its restore
	// turned the routing off again on this terminal row.
	w.db.TrackJobForLogs(jobID)
	w.logger.Info("cleared an incomplete-tail flag: the recording stopped within the end-of-stream margin of the reported head, which is how an ended broadcast looks",
		"jobID", jobID, "lastSeq", lastSeq, "headSeq", head, "shortOfHead", head-lastSeq,
		"margin", engine.EndOfStreamHeadMargin, "staging", staging, "path", stagingDir)
}

// cleanClearedTailStaging gives the staging of a row clearToleratedTail has
// just cleared the cleanup a complete job gets after its mux
// (cleanupStagingAfterMux), and says what became of it. Caller holds the
// job's staging claim and has checked the archive is where the row says.
//
// One guard comes first, the boot sweep's (reclaimJobLeftovers): no
// set-aside recording marked recovered may be left that cannot be proved
// redundant. Such a directory is left for the orphan sweep and its
// confirm-before-delete.
func (w *DownloadWorker) cleanClearedTailStaging(jobID, stagingDir string) string {
	if _, err := os.Stat(stagingDir); err != nil {
		if os.IsNotExist(err) {
			return "none on disk"
		}
		return fmt.Sprintf("left alone, unreadable: %v", err)
	}
	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil {
		return "kept: the row could not be read again"
	}
	if w.removeVerifiedRecoveredAsides(jobID, stagingDir) {
		return "kept: a set-aside recording marked recovered is still in it"
	}
	keep := decideStagingCleanup(w.db, job, jobID, stagingDir).keep
	w.cleanupStagingAfterMux(jobID, stagingDir)
	switch keep {
	case removeStaging:
		if _, err := os.Stat(stagingDir); err == nil {
			return "removal failed; left in place"
		}
		return "removed"
	case keepStagingForAsides:
		return "kept: a set-aside recording was never merged into the archive"
	case keepStagingForUnmuxedPart:
		return "kept: a captured part is still unmuxed"
	case keepStagingForUnusedRoot:
		return "kept: the staging root holds a recording the finalize did not use"
	case keepStagingForMuxGaps:
		return "kept: a mux could not carry a staged recording whole"
	case keepStagingForTail:
		return "kept: the row is flagged incomplete again"
	case keepStagingChatOnly:
		return "pruned to the chat capture: chat ended incomplete"
	}
	return "kept"
}
