package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// liveShapeStagingNames are the staged media names every capture EXCEPT the
// whole-file VOD download writes: the DASH family's video_stream and
// audio_stream (live, post-live and manifest-driven DASH alike) and the HLS
// strategy's video.ts. The whole-file pair, video.mp4 and audio.m4a, is
// DownloadVod's alone.
//
// discoverStagingMedia ranks these above the whole-file pair, which is right
// for every dir that only ever held one capture and wrong for the one that
// held two: see setAsideLiveShapesForVod.
var liveShapeStagingNames = []string{"video_stream", "audio_stream", "video.ts"}

// setAsideStagedMedia sets aside every file named in names under dir the way
// the engine's no-truncate guard sets a recording aside: renamed to
// <name>.restart-<unix ts> (engine.StagedRestartSuffix), its resume sidecar
// travelling with it as engine.StagedRestartSidecar. The asides are then what
// they are for any other restart — muxed into their own file beside the
// archive at finalize (muxStagedAsides), shielded in staging until they are,
// and recoverable on demand (A S, RecoverAsides).
//
// ONE stamp for the whole call, so a DASH capture's video and audio halves
// group as one recording (groupStagedAsides keys on the stamp — and on the
// recording a name belongs to, so a video.ts set aside in the same call is
// still a recording, and a sibling, of its own), and a stamp no existing
// aside in dir already uses: two set-asides inside one second would
// otherwise rename onto each other, which on POSIX silently replaces the
// first.
//
// An empty file is removed rather than set aside — there is nothing in it to
// recover, and an aside FFmpeg cannot open is shielded in staging forever.
//
// A rename that fails is NOT downgraded to a delete, and the call stops at the
// first one: nothing is destroyed implicitly, and the caller decides what a
// staging dir it could not clear means. Returns the asides it made, in names
// order.
func setAsideStagedMedia(dir string, names []string) ([]string, error) {
	var present []string
	for _, name := range names {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() == 0 {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove empty staged %s: %w", name, err)
			}
			if err := os.Remove(engine.StagedRestartSidecar(p)); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove the resume state of empty staged %s: %w", name, err)
			}
			continue
		}
		present = append(present, p)
	}
	if len(present) == 0 {
		return nil, nil
	}

	stamp := time.Now().Unix()
	for taken := true; taken; {
		taken = false
		for _, p := range present {
			aside := p + engine.StagedRestartSuffix + strconv.FormatInt(stamp, 10)
			if _, err := os.Lstat(aside); err == nil {
				taken = true
				stamp++
				break
			}
		}
	}

	var asides []string
	for _, p := range present {
		aside := p + engine.StagedRestartSuffix + strconv.FormatInt(stamp, 10)
		if err := utils.ReplaceFile(p, aside); err != nil {
			return asides, fmt.Errorf("set aside staged %s: %w", filepath.Base(p), err)
		}
		asides = append(asides, aside)
		// The sidecar describes the recording that just moved; left beside the
		// fresh file it is a resume offset map for bytes that are not there.
		// Moved when it can be, removed when it cannot — the engine's
		// moveResumeStateAside rule.
		sidecar := engine.StagedRestartSidecar(p)
		if err := utils.ReplaceFile(sidecar, engine.StagedRestartSidecar(aside)); err != nil && !os.IsNotExist(err) {
			if rmErr := os.Remove(sidecar); rmErr != nil && !os.IsNotExist(rmErr) {
				return asides, fmt.Errorf("set aside %s but its resume state could not be moved or removed: %w", filepath.Base(p), rmErr)
			}
		}
		// So does the record of a mux that could not carry it whole, which
		// keepRepairedAside and the aside removals read beside the aside. One
		// that cannot follow is no trap: beside the fresh file it describes
		// a size that file will not have, and the aside's own mux walks it
		// and records it again.
		if err := utils.ReplaceFile(p+muxGapRecordSuffix, aside+muxGapRecordSuffix); err != nil && !os.IsNotExist(err) {
			os.Remove(p + muxGapRecordSuffix)
		}
	}
	return asides, nil
}

// setAsideShortSpan is what a quality split does with the span it closes too
// short to be a part of its own (under minSegmentDuration): sets the span's
// media in dir aside (setAsideStagedMedia), so the next part may reuse the
// dir and index without the engine resume-appending the new quality onto
// the old one, and the span is still muxed into its own file beside the
// archive at finalize (muxStagedAsides) and recoverable by A S like any
// other aside. Reports whether dir is clear of live-shape media, which is
// what lets the caller reuse it.
//
// The span used to be os.Remove'd (owner decision PR-R9: "a quality split
// never deletes the short stretch before it; it is set aside like any closed
// part"). A span is short in WALL-CLOCK time, so a from-start capture
// catching up through a backlog could gather far more than ten seconds of
// footage in it, and the closed span can be the only copy of what it holds.
// A span in the job's root (the first part, which the next part does not
// share a dir with) was left in place, and the staging cleanup then deleted
// it as "the short span the split skipped": set aside, it is recovered too.
//
// A set-aside that fails reports false (nothing is ever downgraded to a
// delete), and the caller then closes the span as a regular part instead,
// muxed in the background like any other.
func (o *DownloadOrchestrator) setAsideShortSpan(dir, jobID string) bool {
	asides, err := setAsideStagedMedia(dir, liveShapeStagingNames)
	if err != nil {
		o.logger.Error("could not set the short span before a quality split aside; it is kept as a part of its own",
			"dir", dir, "err", err, "jobID", jobID)
		return false
	}
	if len(asides) > 0 {
		o.logger.Info("the short span before a quality split was set aside; it is muxed into its own file beside the archive at finalize",
			"asides", asides, "jobID", jobID)
	}
	return true
}

// setAsideLiveShapesForVod clears a whole-file VOD download's staging dir of
// any live-shape capture an earlier run of the same job left there, before
// the download writes video.mp4 / audio.m4a beside it.
//
// The two shapes cannot share a dir. discoverStagingMedia ranks video_stream
// and video.ts above video.mp4, so a restart mux, the Mux action and every
// predicate built on that discovery (muxOnRestart, HasSegmentFiles) chose the
// earlier capture — typically a post-live attempt that stalled, which is WHY
// the job came back as a VOD — and finished the job "clean" from it while the
// complete download was deleted with the rest of staging. Ranking by mtime
// was the alternative, and it answers the wrong question: the newest file is
// not the complete one (a whole-file download interrupted an hour in is newer
// than the post-live capture it replaced), and neither name says which of the
// two finished. Setting the earlier shape aside leaves exactly one current
// recording in the dir, so every discovery agrees with the run that is about
// to write it, and keeps the earlier one recoverable — it is muxed into its
// own sibling at finalize like any other aside, never into the archive.
//
// Called from DownloadVod once the formats are resolved, so a run that fails
// before it could download anything has moved nothing. A dir that cannot be
// cleared fails the run: downloading beside a capture discovery will prefer
// is the bug this exists to prevent.
func setAsideLiveShapesForVod(job *JobContext) error {
	asides, err := setAsideStagedMedia(job.StagingDir, liveShapeStagingNames)
	lg := newScopedLogger(job.Logger, "jobID", job.Job.ID)
	if len(asides) > 0 {
		lg.Warn("an earlier capture of this job was set aside before the whole-file VOD download; it is muxed to its own file beside the archive, never into it",
			"asides", strings.Join(asides, " | "))
	}
	if err != nil {
		lg.Error("could not set an earlier capture aside before the whole-file VOD download; refusing to download beside it",
			"dir", job.StagingDir, "err", err)
		return fmt.Errorf("VOD: staging holds an earlier capture that could not be set aside: %w", err)
	}
	return nil
}

// setAsideOtherLiveShape clears a capture's part dir (job.StagingDir) of the
// staged media of the OTHER live shape, once a setup has built the
// downloaders in result and before they write: the DASH family's
// video_stream and audio_stream when result records HLS (video.ts), the HLS
// video.ts when it records anything else. setAsideLiveShapesForVod's rule —
// two captures cannot share a dir — for the two live shapes; a whole-file
// download has cleared both already (DownloadVod), so for it this finds
// nothing.
//
// A job resumed after a restart records into the part it left unmuxed
// (discoverResumeSegment), normally in the shape it had. The live HLS
// fallback (setupWithFallback) changes the shape inside the part, for either
// of its causes: a live job resumed while the player cannot be solved (audit
// G10), or while its web-family streams need a GVS token none can be minted
// (missing_pot, audit G16), records the live HLS playlist into the part that
// holds its earlier from-start DASH capture, and once solving or minting
// works again a job resumed from that fallback — live, or post-live after
// the broadcast ended — records DASH beside its video.ts. The finalize muxes the one recording result names, the restart
// mux and the Mux action the one discoverStagingMedia ranks first
// (video_stream over video.ts), and the cleanup then deleted staging — no
// part dirs, no whole-file pair, nothing it shields (decideStagingCleanup) —
// with the other recording unmuxed in it. Set aside, that one is muxed into
// its own file beside the archive at finalize (muxStagedAsides) and shielded
// in staging until it is, like any restart's aside.
//
// A dir that cannot be cleared fails the setup, as it fails the whole-file
// download: recording beside a capture the finalize will not take is the
// loss this exists to prevent.
func setAsideOtherLiveShape(job *JobContext, result *DownloadResult) error {
	if result == nil {
		return nil
	}
	other, shape := []string{"video.ts"}, "DASH"
	if result.IsHls {
		other, shape = []string{"video_stream", "audio_stream"}, "HLS"
	}
	asides, err := setAsideStagedMedia(job.StagingDir, other)
	lg := newScopedLogger(job.Logger, "jobID", job.Job.ID)
	if len(asides) > 0 {
		lg.Warn("an earlier capture of this job in another shape was set aside before the "+shape+" capture records into its part; it is muxed to its own file beside the archive, never into it",
			"asides", strings.Join(asides, " | "))
	}
	if err != nil {
		lg.Error("could not set an earlier capture in another shape aside; refusing to record the "+shape+" capture beside it",
			"dir", job.StagingDir, "err", err)
		return fmt.Errorf("staging holds an earlier capture in another shape that could not be set aside: %w", err)
	}
	return nil
}
