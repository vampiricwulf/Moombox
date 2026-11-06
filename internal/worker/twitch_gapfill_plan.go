package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// Twitch archive-VOD gap-fill, owner decision D17 and its follow-ups. A live
// Twitch capture records the live edge only, so the footage it never had — the
// opening when it joined late, a span a restart or an outage longer than the
// playlist window cost (each a gap split), and, where the channel asks for it,
// each stitched-ad break it skipped — used to be written off as unrecoverable.
// The broadcast's archive VOD has all of it. yt-dlp's answer is to record the
// VOD instead of the live stream from the start (--live-from-start,
// TwitchStreamIE._real_extract, twitch.py b77c9304e, 00b1bec55); the owner
// chose the narrower one, beyond upstream: keep the live capture, and fill only
// the missing spans from the VOD, each as a part of its own in time order.
//
// This file is the capture's half: what each part's file holds, in wall-clock
// time (twitch_coverage.json, from the engine's OnSegmentWritten), and the
// plan built from it at finalize. twitch_gapfill.go is the half that runs the
// plan once the VOD is complete.

// twitchCoverageFile records, in each live Twitch part's staging dir, the
// wall-clock span the part's file holds.
const twitchCoverageFile = "twitch_coverage.json"

// partCoverage is twitchCoverageFile's content: the program date-time of the
// first segment the file holds and the end of the last one (epoch ms), and
// whether the part was closed at the end of a stitched-ad break
// (engine.ErrAdBreakEnded), which makes the hole after it an ad break's.
//
// StartUnknown marks a part whose file already held footage when its
// recorder was made and had no coverage file to say since when — a part a
// build without the gap-fill began, resumed by one with it (an upgrade
// mid-broadcast). Its StartMs is the first segment written after the resume,
// not the first the file holds, so the footage before it reads as lost when it
// is not; gapSpansFor makes no plan for a capture with such a part.
type partCoverage struct {
	StartMs        int64 `json:"startMs,omitempty"`
	EndMs          int64 `json:"endMs,omitempty"`
	EndedAtAdBreak bool  `json:"endedAtAdBreak,omitempty"`
	StartUnknown   bool  `json:"startUnknown,omitempty"`
}

// coverageSaveEvery is how far a part's recorded end may run ahead of the one
// on disk before it is written again. A crash loses at most this much of the
// end, which only widens the gap the fill then covers (an overlap, never a
// hole) — at the cost of one small atomic write per this much footage.
const coverageSaveEvery = 30 * time.Second

// coverageRecorder keeps one part's coverage, fed from the download goroutine
// (note) and closed from the capture's loop (close). A part resumed after a
// restart loads what its file already held.
type coverageRecorder struct {
	mu      sync.Mutex
	dir     string
	cov     partCoverage
	savedTo int64
}

func newCoverageRecorder(dir string) *coverageRecorder {
	r := &coverageRecorder{dir: dir}
	if _, err := os.Stat(filepath.Join(dir, twitchCoverageFile)); err != nil && partHoldsMedia(dir) {
		// Footage with no record of where it starts: say so on disk at
		// once, so a crash before the first note still leaves the mark.
		r.cov.StartUnknown = true
		r.mu.Lock()
		r.saveLocked()
		r.mu.Unlock()
		return r
	}
	r.cov = readPartCoverage(dir)
	r.savedTo = r.cov.EndMs
	return r
}

// partHoldsMedia reports whether a part dir holds a non-empty staged media
// file (discoverStagingMedia's), the footage a coverage file would describe.
func partHoldsMedia(dir string) bool {
	m := discoverStagingMedia(dir)
	if m == nil || m.VideoPath == "" {
		return false
	}
	fi, err := os.Stat(m.VideoPath)
	return err == nil && fi.Size() > 0
}

// readPartCoverage reads dir's coverage; a missing or unreadable file is none.
func readPartCoverage(dir string) partCoverage {
	var c partCoverage
	if data, err := os.ReadFile(filepath.Join(dir, twitchCoverageFile)); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

// note records a written segment: its program date-time and duration.
func (r *coverageRecorder) note(pdt time.Time, durSec float64) {
	if pdt.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	start := pdt.UnixMilli()
	end := pdt.Add(time.Duration(durSec * float64(time.Second))).UnixMilli()
	if r.cov.StartMs == 0 {
		r.cov.StartMs = start
	}
	r.cov.EndMs = max(r.cov.EndMs, end)
	// A fresh part's savedTo is 0, so its first segment is written at once.
	if r.cov.EndMs-r.savedTo >= coverageSaveEvery.Milliseconds() {
		r.saveLocked()
	}
}

// close writes the part's final coverage; adBreak marks a part the capture
// closed at the end of a stitched-ad break.
func (r *coverageRecorder) close(adBreak bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if adBreak {
		r.cov.EndedAtAdBreak = true
	}
	if r.cov.StartMs != 0 {
		r.saveLocked()
	}
}

// reset forgets a discarded part's coverage (the short-segment rule reuses its
// dir for the next part) and removes its file.
func (r *coverageRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cov, r.savedTo = partCoverage{}, 0
	_ = os.Remove(filepath.Join(r.dir, twitchCoverageFile))
}

func (r *coverageRecorder) saveLocked() {
	data, err := json.Marshal(r.cov)
	if err != nil {
		return
	}
	if utils.WriteFileAtomic(filepath.Join(r.dir, twitchCoverageFile), data, 0o644) == nil {
		r.savedTo = r.cov.EndMs
	}
}

// Gap kinds: what lost the footage a span covers.
const (
	gapKindLateStart = "late_start" // the capture began after the broadcast did
	gapKindLost      = "lost"       // a gap split: a restart, an outage, a window gap
	gapKindAdBreak   = "ad_break"   // a stitched-ad break the capture skipped
	gapKindTail      = "tail"       // the broadcast ended while the capture was cut off
)

// gapFillLateStartAfter is how far after the broadcast's start the capture's
// first frame must be before the opening counts as lost (owner decision D17
// follow-up 3: "late start more than ~1 min after stream_start_time"). A
// capture that joins on time still starts some seconds in: the monitor's poll
// and the live playlist's window.
const gapFillLateStartAfter = time.Minute

// gapFillMinGap is the smallest hole between two parts that counts as lost
// footage. A split that loses nothing — an init-segment change, a quality
// change, an ad-break split's own seam — leaves the next part starting where
// the last one ended, give or take program date-time rounding; one lost live
// segment is two seconds.
const gapFillMinGap = 3 * time.Second

// gapSpan is one span of the broadcast the capture lacks, in wall-clock epoch
// ms; ToMs 0 is open-ended (a tail, ended by the VOD's own end). AfterPart is
// the segment index of the part it follows, -1 for one before the first.
type gapSpan struct {
	FromMs    int64  `json:"fromMs"`
	ToMs      int64  `json:"toMs,omitempty"`
	Kind      string `json:"kind"`
	AfterPart int    `json:"afterPart"`
}

// stagedPartCoverage is one part's coverage with the segment index its file
// was muxed under. HasMedia reports that the part's dir held footage when the
// plan was made (partHoldsMedia), so a part with footage and no coverage is
// told from an empty one.
type stagedPartCoverage struct {
	Index    int          `json:"index"`
	Cov      partCoverage `json:"cov"`
	HasMedia bool         `json:"hasMedia,omitempty"`
}

// gapSpansFor computes the spans a capture lacks from its parts' coverage (in
// any order; empty parts without any are left out): the opening before the first
// part when it began more than gapFillLateStartAfter after streamStart, each
// hole between consecutive parts wider than gapFillMinGap, and every hole
// after a part the capture closed at an ad break when adBreaks is set — and,
// with tailOpen (the broadcast ended while
// the capture was cut off), the rest of the broadcast after the last part.
//
// A part that holds footage but cannot say which — no coverage file at all (a
// part a build without the gap-fill recorded and closed), or a start that is
// only the resume's (partCoverage.StartUnknown) — makes no spans at all: its
// footage cannot be placed, and every span computed around it could be
// footage it already holds, which the fill would download again and insert
// beside it as a duplicate part. Only the capture that straddles the upgrade
// to this build has such a part.
func gapSpansFor(streamStart time.Time, parts []stagedPartCoverage, adBreaks, tailOpen bool) []gapSpan {
	var have []stagedPartCoverage
	for _, p := range parts {
		placed := p.Cov.StartMs > 0 && p.Cov.EndMs > p.Cov.StartMs
		if p.Cov.StartUnknown || (p.HasMedia && !placed) {
			return nil
		}
		if placed {
			have = append(have, p)
		}
	}
	if len(have) == 0 {
		return nil
	}
	sort.Slice(have, func(i, j int) bool { return have[i].Cov.StartMs < have[j].Cov.StartMs })
	var out []gapSpan
	if !streamStart.IsZero() {
		if first := have[0].Cov.StartMs; first-streamStart.UnixMilli() > gapFillLateStartAfter.Milliseconds() {
			out = append(out, gapSpan{FromMs: streamStart.UnixMilli(), ToMs: first, Kind: gapKindLateStart, AfterPart: -1})
		}
	}
	for i := 1; i < len(have); i++ {
		prev, next := have[i-1], have[i]
		gap := next.Cov.StartMs - prev.Cov.EndMs
		kind := gapKindLost
		switch {
		case prev.Cov.EndedAtAdBreak:
			// The capture split there itself, so the whole hole is the
			// break's, however short.
			if !adBreaks || gap <= 0 {
				continue
			}
			kind = gapKindAdBreak
		case gap <= gapFillMinGap.Milliseconds():
			continue
		}
		out = append(out, gapSpan{FromMs: prev.Cov.EndMs, ToMs: next.Cov.StartMs, Kind: kind, AfterPart: prev.Index})
	}
	if tailOpen {
		last := have[len(have)-1]
		out = append(out, gapSpan{FromMs: last.Cov.EndMs, Kind: gapKindTail, AfterPart: last.Index})
	}
	return out
}

// stagedPartCoverages reads the coverage of every part of a live capture's
// staging: the root (part 0, unless a seg_0 dir exists — rootIsPartZero) and
// each seg_N.
func stagedPartCoverages(stagingDir string) []stagedPartCoverage {
	var out []stagedPartCoverage
	if rootIsPartZero(stagingDir) {
		out = append(out, stagedPartCoverage{Index: 0, Cov: readPartCoverage(stagingDir), HasMedia: partHoldsMedia(stagingDir)})
	}
	for _, s := range stagedSegDirs(stagingDir) {
		out = append(out, stagedPartCoverage{Index: s.idx, Cov: readPartCoverage(s.dir), HasMedia: partHoldsMedia(s.dir)})
	}
	return out
}

// twitchMuxGapFillPlan is the gap-fill plan for a live Twitch capture that did
// not finalize itself and is muxed from its staging instead (muxJob), nil for
// any other mux. Owner decision FE-25: ANY Mux that finalizes a live capture
// whose parts' coverage records survive in staging plans a fill, from an
// Error row and from a Cancelled one alike —
//
//   - one whose broadcast ended while Moombox was down (prior's error
//     twitchEndedOfflineErrMsg), muxed by the operator's Mux action;
//   - one that stopped with its broadcast's end unconfirmed (park_reason
//     twitch_end_unconfirmed), muxed by AutoMuxEndedBroadcast once the
//     monitor confirms the broadcast over (owner decision D-T4), or by the
//     operator's Mux before that;
//   - one that failed for any other reason, or that the operator cancelled,
//     muxed by the operator's Mux action. These made no plan before FE-25,
//     on the reading that the recorded footage was all that was wanted or
//     possible; the owner chose the fill, which only ever adds parts beside
//     the archive and never touches what was recorded (twitch_gapfill.go).
//
// Each loses what owner decision D17 says is always filled — the late start
// and every gap split, which their parts' coverage still records, and the
// tail after the last part where the capture was cut off (twitchMuxTailOpen)
// — never for a Cancelled row, whose Cancel closed it (owner decision FQ-2).
// A Muxing row (a boot re-mux of a capture that
// completed) is not this: its own finalize was the orchestrator's, whose plan
// knows whether the broadcast ended (ExecuteTwitch's outageFinalize).
// No coverage on disk, or none that can be placed, is no plan
// (gapSpansFor).
//
// prior is the row as muxJob found it, before its Muxing write cleared the
// marker; job is the row the mux runs on. The tail is open only where the
// capture stopped before its broadcast's end was seen (twitchMuxTailOpen):
// a row that reached ExecuteTwitch's finalize and then failed or was
// cancelled — its mux failed after a normal end, the operator cancelled it
// while Muxing — already holds the broadcast to its end, and a tail planned
// for it would cut the VOD's last segments a second time as a "recovered"
// duplicate part (vodSegmentRange keeps every segment the padded span
// touches). Its late start and gap splits are still filled. The selection the
// capture ran on is read back from the row (twitch_quality, the variant it
// last recorded, and quality_preference) and the job context's snapshot of
// the global selection settings, as newTwitchVariantInfo reads it.
func (w *DownloadWorker) twitchMuxGapFillPlan(prior, job *database.Job, jobCtx *JobContext) *twitchGapFillPlan {
	if prior == nil || job == nil || jobCtx == nil || prior.Platform != "twitch" || prior.IsVod {
		return nil
	}
	if prior.Status != database.StatusError && prior.Status != database.StatusCancelled {
		return nil
	}
	login := TwitchJobLogin(job)
	if login == "" {
		return nil
	}
	variant := &TwitchVariantInfo{
		Name:         job.TwitchQuality,
		QualityPref:  job.QualityPreference,
		ChannelLogin: login,
	}
	if jobCtx.Config != nil {
		variant.MaxResolution, variant.Prefer60fps = jobCtx.Config.MaxVideoResolution, jobCtx.Config.Prefer60fps
	}
	w.readConfig(func(c *config.MoomboxConfig) {
		variant.GapFillAdBreaks = twitchChannelGapFillAdBreaks(c.Channels, login)
	})
	return newTwitchGapFillPlan(job, variant, stagedPartCoverages(jobCtx.StagingDir), twitchMuxTailOpen(prior), time.Now())
}

// twitchMuxTailOpen reports whether a live Twitch row muxed from its staging
// stopped before its broadcast's end was seen, so the plan's tail stays open.
//
// A Cancelled row never is (owner decision FQ-2): the operator's Cancel says
// the recording ends where it stopped, so its Mux plans only the late start
// and the gap splits, whenever the Cancel landed — while Downloading, where
// stream_end_time is still empty and the rule below read the capture as cut
// off mid-broadcast and filled the rest of it from the VOD, and while
// Muxing alike. It is read off the row's status alone, which CancelJob
// writes (with nothing else that lasts), so it holds only while the row is
// Cancelled: a Retry or Reinitialize writes another status, and a row that
// then fails again is read by the rules below as any other. It is checked
// ahead of the markers, which a Cancel leaves on the row (it writes no
// error and no park_reason): a Mux of an ended-offline Error row cancelled
// while Muxing keeps twitchEndedOfflineErrMsg, and its tail is closed too.
//
// Otherwise the two markers say so outright: twitchEndedOfflineErrMsg (the broadcast
// ended while Moombox was down) and park_reason twitch_end_unconfirmed. Any
// other Error or Cancelled row is read by stream_end_time, which a live
// Twitch capture writes in ExecuteTwitch's finalize just before
// muxAndFinalize and nowhere earlier (a retry or re-initialisation clears
// it): set, the capture reached its end and only its mux failed or was
// cancelled, and the tail is closed; empty, the capture was cut off
// mid-broadcast and the tail is open.
//
// One row is closed here that ran on past its capture: an outage finalize
// (outageFinalize — the broadcast ended or changed while connectivity was
// down) whose own mux then failed. stream_end_time is written on that path
// too, and nothing on the row tells it apart from a normal end. Its tail is
// not filled on the Mux; before FE-25 that row made no plan at all.
func twitchMuxTailOpen(prior *database.Job) bool {
	if prior.Status == database.StatusCancelled {
		return false
	}
	if prior.Error == twitchEndedOfflineErrMsg || prior.ParkReason == database.ParkReasonTwitchEndUnconfirmed {
		return true
	}
	return prior.StreamEndTime == ""
}
