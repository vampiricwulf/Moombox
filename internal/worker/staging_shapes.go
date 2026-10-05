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
// group as one recording (groupStagedAsides keys on the stamp), and a stamp no
// existing aside in dir already uses: two set-asides inside one second would
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
	}
	return asides, nil
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
