package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
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
// and the boot pass over rows flagged before the rule (clearToleratedTails).

// liveEnd is what the live loop alone knows about how its capture ended. The
// zero value is what the VOD branch passes: no wait, no confirmation.
type liveEnd struct {
	// waitedForResume: the loop gave up out of an unresolved wait for an
	// interrupted broadcast (see finalizeIncompleteTail).
	waitedForResume bool
	// confirmedOver: the loop's own end verification got YouTube's answer
	// that the stream is over (post-live, a VOD, or no longer a stream). A
	// forced end leaves it false.
	confirmedOver bool
}

// streamTail is where one downloader stopped.
type streamTail struct {
	present     bool // the job has this stream (audio-only and video-only jobs lack one)
	behindHead  bool // engine.SegmentDownloader.FinalizedBehindHead
	interrupted bool // engine.SegmentDownloader.FinalizedDuringInterruption
	lastSeq     int  // the last segment written
	head        int  // the head the downloader tracked (-1 when never learned)
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
//     engine.EndOfStreamHeadMargin of the head it tracked.
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
		if s.present && (s.head <= 0 || !engine.WithinEndOfStreamMargin(s.lastSeq, s.head)) {
			return false
		}
	}
	if f.video.present && f.audio.present && f.video.lastSeq != f.audio.lastSeq {
		return false
	}
	return true
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
// same head (b == d), short of it by at least one segment and by no more than
// the margin. It returns the segment and the head.
//
// Only a Finished YouTube live capture is read this way. A VOD download's
// incomplete line counts the NEXT segment (incompleteProgressString), not
// the last written, so the same numbers mean one segment more missing; those
// rows keep their flag. The row cannot say whether an interruption also
// flagged it: the ruling takes the line as the evidence.
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
	if head-lastSeq < 1 || !engine.WithinEndOfStreamMargin(lastSeq, head) {
		return 0, 0, false
	}
	return lastSeq, head, true
}

// clearToleratedTails is the boot pass for rows flagged before the rule:
// every Finished job toleratedTailRow accepts has its flag cleared, its
// progress set to a complete job's, and its staging put through the post-mux
// cleanup. It needs no schema version: a cleared row no longer matches, so a
// second boot does nothing.
//
// It walks the rows, not the staging directory: a flagged row whose staging
// is already gone still shows the badge and still offers Resume.
func (w *DownloadWorker) clearToleratedTails() {
	jobs, err := w.db.GetAllJobs()
	if err != nil {
		w.logger.Warn("boot: could not read the jobs to clear end-of-stream tail flags; nothing changed", "err", err)
		return
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	for _, job := range jobs {
		if _, _, ok := toleratedTailRow(job); ok {
			w.clearToleratedTail(job.ID, filepath.Join(stagingBase, job.ID))
		}
	}
}

// clearToleratedTail is clearToleratedTails for one row. The staging claim is
// taken first and the row read again under it: Resume and the other reset
// verbs wait on the claim, so the row cannot be revived between that read and
// the write, and the write still applies only while the row is Finished.
// Staging is touched only after the write has applied. A job whose staging
// another operation holds is left for the next boot.
func (w *DownloadWorker) clearToleratedTail(jobID, stagingDir string) {
	if outputClaimOwner(stagingDir) != "" {
		return
	}
	release, err := w.claimJobOperation(jobID, opTailClear)
	if err != nil {
		return
	}
	defer release()

	job, err := w.db.GetJob(jobID)
	if err != nil {
		return
	}
	lastSeq, head, ok := toleratedTailRow(job)
	if !ok {
		return
	}
	// The row is Finished, so its per-job log routing is off; put the lines
	// below in the job's own log (the bracket cleanupStagingAfterMux uses).
	w.db.TrackJobForLogs(jobID)
	defer w.restoreLogRouting(jobID)

	if !w.db.UpdateJobFieldsIf(jobID, database.StatusFinished, map[string]any{
		"incomplete_tail": false,
		"progress":        "",
		"percent":         100.0,
	}) {
		return
	}
	staging := w.cleanClearedTailStaging(jobID, stagingDir)
	w.logger.Info("cleared an incomplete-tail flag: the recording stopped within the end-of-stream margin of the reported head, which is how an ended broadcast looks",
		"jobID", jobID, "lastSeq", lastSeq, "headSeq", head, "shortOfHead", head-lastSeq,
		"margin", engine.EndOfStreamHeadMargin, "staging", staging, "path", stagingDir)
}

// cleanClearedTailStaging gives the staging of a row clearToleratedTail has
// just cleared the cleanup a complete job gets after its mux
// (cleanupStagingAfterMux), and says what became of it. Caller holds the
// job's staging claim.
//
// Two guards come first, both the boot sweep's (reclaimJobLeftovers): the
// archive must be where the row says, and no set-aside recording marked
// recovered may be left that cannot be proved redundant. Either way the
// directory is left for the orphan sweep and its confirm-before-delete.
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
	if missing := missingArchiveFile(w.db, job); missing != "" {
		return "kept: the archive is not where the row says (" + missing + ")"
	}
	if w.removeVerifiedRecoveredAsides(jobID, stagingDir) {
		return "kept: a set-aside recording marked recovered is still in it"
	}
	keep := decideStagingCleanup(w.db, job, jobID, stagingDir).keep
	w.cleanupStagingAfterMux(jobID, stagingDir)
	switch keep {
	case removeStaging:
		if _, err := os.Stat(stagingDir); err == nil {
			return "removal failed; left for the next boot"
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
