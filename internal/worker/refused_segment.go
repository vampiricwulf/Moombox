package worker

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// sameSeqRebuildCap is how many times the live loop rebuilds a YouTube DASH
// capture at the same sequence before it stops: owner decision D8 of the
// 2026-10-31 audit ("cap same-seq rebuilds at 3 either way", G15).
//
// Each rebuild answers an ErrQualityLost — a segment below the head that
// will not come while YouTube calls the stream live — with a full
// GetVideoInfo, a PO-token mint, a cipher re-resolve and new downloaders at
// the same CurrentSeq. When the quality came back unchanged the loop simply
// continued, with nothing counting, so one segment YouTube refuses for good
// drove that cycle every ten to twenty seconds for the rest of the broadcast
// and recorded nothing after it. yt-dlp has no such cycle to bound: its
// fragment downloader skips a non-first fragment once fragment_retries are
// spent (downloader/fragment.py, skip_unavailable_fragments).
//
// The rebuild that reaches the cap arms its downloaders
// (engine.SegmentDownloader.NoteRebuildsExhausted): where they would hand the
// sequence back once more they apply the engine's refused-segment rule to it
// instead — the rebuilds were the fresh credentials that rule waits for —
// and the loop closes the part there (ErrSegmentRefused). A sequence the rule
// cannot take, because the head is not more than two past it, comes back as
// ErrQualityLost once more; past the cap the loop stops rebuilding and
// verifies the stream on the still-live branch's five-minute cadence and
// budget instead.
const sameSeqRebuildCap = 3

// seqRun counts consecutive rebuilds of one stream at one sequence, and
// when the first of them was.
type seqRun struct {
	seq, count int
	since      time.Time
}

// note records a rebuild at seq and returns how many in a row were at it.
func (r *seqRun) note(seq int) int {
	if r.count > 0 && r.seq == seq {
		r.count++
	} else {
		r.seq, r.count, r.since = seq, 1, time.Now()
	}
	return r.count
}

// sameSeqRebuilds counts the live loop's ErrQualityLost rebuilds per stream:
// a stream whose downloader stopped where the previous rebuild started it
// made no progress across that rebuild. Per stream, because the sibling of a
// stuck stream keeps moving (the errgroup cancels it wherever it is), so the
// pair of sequences changes on every rebuild even while one of them is stuck.
type sameSeqRebuilds struct{ video, audio seqRun }

// note records an ErrQualityLost rebuild of res's downloaders at the
// sequences they stopped at, and returns the larger of the two streams'
// counts.
func (s *sameSeqRebuilds) note(res *DownloadResult) int {
	n := 0
	if res.VideoDownloader != nil {
		n = max(n, s.video.note(res.VideoDownloader.CurrentSeq()))
	}
	if res.AudioDownloader != nil {
		n = max(n, s.audio.note(res.AudioDownloader.CurrentSeq()))
	}
	return n
}

// arm tells res's downloaders which sequence the loop has rebuilt at as many
// times as it will. A downloader starting elsewhere — a part a split opened
// past it, a stream that has moved on — never meets the sequence, so arming
// it is harmless.
func (s *sameSeqRebuilds) arm(res *DownloadResult) {
	if s.video.count >= sameSeqRebuildCap && res.VideoDownloader != nil {
		res.VideoDownloader.NoteRebuildsExhausted(s.video.seq, s.video.since)
	}
	if s.audio.count >= sameSeqRebuildCap && res.AudioDownloader != nil {
		res.AudioDownloader.NoteRebuildsExhausted(s.audio.seq, s.audio.since)
	}
}

// pastRefusedSegment returns where the part that follows a refused segment
// starts each stream, given the sequences the closed part's downloaders
// stopped at. A stream stopped AT the refused sequence starts one past it:
// the stream that refused it, and its sibling when it stopped on the same
// sequence — almost always the same hole, refused on both streams, and
// started there it would refuse again in a part whose file already holds its
// init and so cannot hunt past it, splitting a second time. A stream stopped
// anywhere else keeps its own position, exactly as a quality split keeps it;
// the mux aligns the two from their first timestamps (alignStagedInputs).
// videoMoved and audioMoved report which streams step over the hole, so the
// caller records their gap rows.
func pastRefusedSegment(seq, videoSeq, audioSeq int) (v, a int, videoMoved, audioMoved bool) {
	v, a = videoSeq, audioSeq
	if videoSeq == seq {
		v, videoMoved = seq+1, true
	}
	if audioSeq == seq {
		a, audioMoved = seq+1, true
	}
	return v, a, videoMoved, audioMoved
}

// noteRefusedHole records the gap row of each stream that steps over the
// refused sequence, through the progress tracker's OnGap, the same row a
// catch-up gap or an in-place skip records.
func noteRefusedHole(res *DownloadResult, seq int, videoMoved, audioMoved bool) {
	gap := engine.DownloadGap{From: seq, To: seq}
	if videoMoved && res.VideoDownloader != nil && res.VideoDownloader.OnGap != nil {
		res.VideoDownloader.OnGap(gap)
	}
	if audioMoved && res.AudioDownloader != nil && res.AudioDownloader.OnGap != nil {
		res.AudioDownloader.OnGap(gap)
	}
}
