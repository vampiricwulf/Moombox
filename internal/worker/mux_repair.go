package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// A staged fMP4 recording with one damaged fragment mid-file — zeroed, torn,
// an HTML error body served with 200 — used to finish as a Finished archive
// over the part before the damage, with staging, the only copy of everything
// after it, deleted on the way out (W30-A11). FFmpeg's `-c copy` stops at the
// damaged fragment and exits 0; ENGINE-9's check measured the INPUT with
// ffprobe, whose mov demuxer stops at the same fragment, so the two always
// agreed; and with a video_stream and an audio_stream it compared format
// durations, which the intact track carried to full length.
//
// Every copy of staged media into an archive, a part or an aside's sibling now
// goes through copyStaged (owner decision: repair and finish):
//
//   - each input is walked with engine.WalkFMP4, which reads the boxes itself;
//   - a damaged one is muxed from a repaired copy written beside it in
//     staging — the stream without the unreadable bytes, every intact
//     fragment after them kept — and the job finishes with a short gap there;
//   - the copy is checked track by track against the fragments' own
//     timeline (verifyAgainstStaging), so a copy that still stops short is
//     rejected whatever the demuxer says;
//   - and the staged original is kept, by a record beside it
//     (muxGapRecordSuffix) the staging cleanup reads as a shield, whenever
//     anything was left out.
//
// An input the walk has no business with (MPEG-TS, WebM, a progressive MP4)
// is measured as it always was. One that is shaped like a fragmented MP4 but
// cannot be walked, or whose timeline is no measure of it, is never repaired
// and is measured as it always was too — but nothing independent of FFmpeg
// vouches for that copy, so its staging is kept in the same way.

// stagedRepairSuffix names the repaired copy of a staged recording, written
// beside it: "<file>.moombox-repair". Not a name discoverStagingMedia knows,
// nor a set-aside one (engine.IsStagedRestartPath wants digits after the
// stamp), so no scan mistakes it for a recording. It is removed once the copy
// that read it is done; one left behind — the process died mid-mux, or the
// removal failed — is removed by the next repair of the same file before it
// counts the free space, by the job's staging cleanup, and by the boot sweep
// (removeStaleRepairedCopies), so it never sits in shielded staging.
const stagedRepairSuffix = ".moombox-repair"

// muxGapRecordSuffix names the record a mux leaves beside a staged recording
// it could not carry whole: "<file>.mux-gaps.json". Its presence — for a
// recording still there at the size the mux read — is the staging-cleanup
// shield (stagedMuxGaps).
const muxGapRecordSuffix = ".mux-gaps.json"

// repairHeadroom is what the staging volume must have free beyond the
// repaired copy itself before one is written.
const repairHeadroom = 64 << 20

// repairDiskSpace is disk.GetDiskSpace, a variable so a test can stand in a
// full staging volume.
var repairDiskSpace = disk.GetDiskSpace

// stagedInput is one staged recording as a copy reads it.
type stagedInput struct {
	path string // the recording as the engine staged it
	read string // what FFmpeg reads: path, or the repaired copy beside it
	// walk is nil for a file that is not a fragmented MP4, or one the walk
	// could not read.
	walk *engine.FMP4Walk
	// unmeasured names why nothing independent of FFmpeg can measure a copy
	// of this file — a fragmented MP4 the walk could not read, or whose
	// timeline is no measure of it — and is "" when something can.
	unmeasured string
	// lead is, for an input whose first fragment(s) the repair leaves out
	// (lostStart) and that a copy places against the other half of its
	// capture (alignStagedInputs), how far into the capture its repaired
	// copy begins: the gap the archive keeps at its start. 0 otherwise.
	lead float64
}

// lostStart reports whether a drop came before the input's first readable
// fragment in any of its video or audio tracks: its repaired copy begins
// later on the capture's timeline than the recording did.
func (in *stagedInput) lostStart() bool {
	if in.walk == nil {
		return false
	}
	for _, d := range in.walk.Drops {
		for _, g := range d.Gaps {
			if !g.HasFrom && g.HasTo && codecTypeOf(g.Handler) != "" {
				return true
			}
		}
	}
	return false
}

// carriesOnly reports whether every track the walk found in the input is of
// one codec type: a video_stream or an audio_stream of a DASH capture.
func (in *stagedInput) carriesOnly(codecType string) bool {
	if in.walk == nil || len(in.walk.Tracks) == 0 {
		return false
	}
	for _, t := range in.walk.Tracks {
		if codecTypeOf(t.Handler) != codecType {
			return false
		}
	}
	return true
}

// measured reports whether the input's own fragments measure a copy of it.
func (in *stagedInput) measured() bool { return in.walk != nil && in.walk.Unreliable == "" }

// keepsStaging reports whether a copy of this input leaves its staging kept:
// something was left out of it, or nothing could vouch for it.
func (in *stagedInput) keepsStaging() bool {
	return in.unmeasured != "" || (in.walk != nil && in.walk.Damaged())
}

// stagedInputs is a copy's inputs: the video half and the audio half, either
// of which may be nil.
type stagedInputs struct {
	video, audio *stagedInput
	// offsets delay each input on the copy's timeline (alignStagedInputs);
	// aligned is set when they were worked out, which only a copy of two
	// inputs one of which lost its start needs.
	offsets engine.InputOffsets
	aligned bool
}

func (s *stagedInputs) list() []*stagedInput {
	if s == nil {
		return nil
	}
	var out []*stagedInput
	for _, in := range []*stagedInput{s.video, s.audio} {
		if in != nil {
			out = append(out, in)
		}
	}
	return out
}

// readPaths are the files FFmpeg reads, "" for a half there is none of.
func (s *stagedInputs) readPaths() (video, audio string) {
	if s.video != nil {
		video = s.video.read
	}
	if s.audio != nil {
		audio = s.audio.read
	}
	return video, audio
}

// keepsStaging reports whether any input's copy leaves its staging kept.
func (s *stagedInputs) keepsStaging() bool {
	for _, in := range s.list() {
		if in.keepsStaging() {
			return true
		}
	}
	return false
}

// copyStaged is the one stream copy of staged media into an output file —
// the archive (muxAndFinalize), a part (muxSegment), a set-aside recording's
// sibling (muxStagedAsides). It walks and, where it has to, repairs the
// inputs (prepareStagedInputs), copies with ENGINE-9's check run on the copy
// under FFmpeg's temporary name (verifiedCopy), and removes the repaired
// copies again. The verdict carries the inputs, so a caller knows whether its
// staging must be kept (stagedInputs.keepsStaging) and what to report.
func (o *DownloadOrchestrator) copyStaged(ctx context.Context, jobID, videoPath, audioPath, outputPath string) (muxVerdict, error) {
	var v muxVerdict
	ins, err := o.prepareStagedInputs(ctx, jobID, outputPath, videoPath, audioPath)
	if err != nil {
		return v, err
	}
	defer o.removeRepairedCopies(jobID, ins)
	v.inputs = ins
	video, audio := ins.readPaths()
	if err := o.mux().MuxCopyAt(ctx, video, audio, outputPath, ins.offsets, o.verifiedCopy(jobID, &v, ins)); err != nil {
		return v, err
	}
	o.reportStagingRepairs(jobID, ins, outputPath)
	return v, nil
}

// prepareStagedInputs walks each staged input, places the two against each
// other when a repair cuts the start of either (alignStagedInputs), records
// beside each whether its copy will leave staging kept (writeMuxGapRecord —
// before anything is muxed, so a record that cannot be written stops the
// copy rather than leave its staging unshielded), and writes a repaired copy
// of each damaged one.
func (o *DownloadOrchestrator) prepareStagedInputs(ctx context.Context, jobID, outputPath, videoPath, audioPath string) (*stagedInputs, error) {
	ins := &stagedInputs{}
	slots := []struct {
		path string
		slot **stagedInput
	}{{videoPath, &ins.video}, {audioPath, &ins.audio}}
	for _, s := range slots {
		if s.path == "" {
			continue
		}
		in, err := o.walkStagedInput(ctx, jobID, s.path)
		if err != nil {
			return nil, err
		}
		*s.slot = in
	}
	if err := o.alignStagedInputs(ctx, jobID, ins); err != nil {
		return nil, err
	}
	for _, in := range ins.list() {
		if err := o.recordStagedInput(in, outputPath); err != nil {
			o.removeRepairedCopies(jobID, ins)
			return nil, err
		}
		if in.walk != nil && in.walk.Damaged() {
			if err := o.writeRepairedCopy(ctx, jobID, in); err != nil {
				o.removeRepairedCopies(jobID, ins)
				return nil, err
			}
		}
	}
	return ins, nil
}

// muxStartTolerance is how far a track of a copy that alignStagedInputs
// placed may start from where its staged fragments put it: an edit list's
// AAC priming, a video's composition offset — a few tens of milliseconds —
// and well short of the one-second fragment a desync would be.
const muxStartTolerance = 0.5

// alignStagedInputs places the two inputs of a copy on the timeline they
// share when a repair cuts the start of either. FFmpeg starts every input
// file at its own first timestamp, so a repaired audio_stream whose first
// fragment was the damaged one — beginning at the second fragment's decode
// time — came out level with a video_stream that still begins at the first:
// the archive played that audio from 0 s against the picture from 0 s, ahead
// of it by the dropped stretch for the whole archive, and the per-track
// check, measuring each track from its own first readable fragment, passed
// it. The two halves of a DASH capture (a live or manifestless video_stream
// and audio_stream, a restart's pair of asides, a whole-file VOD download's
// pair) are cut from one broadcast's media timeline, so where each one's
// first readable fragment sits on it places them against each other: each
// input is delayed (engine.InputOffsets, FFmpeg's -itsoffset) by how far
// after the earlier one it begins, and the archive keeps a gap at the start
// of the cut track instead. An input the walk cannot read is placed by
// ffprobe's start time of it; one nothing can place refuses the copy, with
// staging kept, rather than write an archive out of step.
//
// A copy whose inputs kept their starts is left as FFmpeg has always muxed
// it, and so is a copy of one input, whose tracks keep their places in it.
func (o *DownloadOrchestrator) alignStagedInputs(ctx context.Context, jobID string, ins *stagedInputs) error {
	if ins.video == nil || ins.audio == nil || (!ins.video.lostStart() && !ins.audio.lostStart()) {
		return nil
	}
	halves := []*stagedInput{ins.video, ins.audio}
	var starts [2]float64
	for i, in := range halves {
		s, ok := o.stagedStart(ctx, in)
		if !ok {
			cut := halves[1-i]
			return fmt.Errorf("the staged %s lost its first fragment(s) to damage, and where the staged %s begins cannot be read to place it against, so the two would play out of step — staging is preserved; mux them by hand",
				filepath.Base(cut.path), filepath.Base(in.path))
		}
		starts[i] = s
	}
	origin := min(starts[0], starts[1])
	ins.offsets = engine.InputOffsets{Video: starts[0] - origin, Audio: starts[1] - origin}
	ins.aligned = true
	for i, in := range halves {
		off := starts[i] - origin
		if !in.lostStart() || off <= 0 {
			continue
		}
		in.lead = off
		o.logger.Warn("a staged recording lost its first fragment(s) to damage; its repaired copy is placed where its first readable fragment sits on the capture's timeline, so the archive keeps a gap at its start rather than play out of step",
			"staged", in.path, "startGap", fmt.Sprintf("0:00:00.0 to %s (%.1fs)", engine.FormatTimelineSeconds(off), off), "jobID", jobID)
	}
	return nil
}

// stagedStart is where a staged input begins on its capture's timeline: its
// first readable fragment's decode time, or for a file the walk cannot read,
// ffprobe's start time of it. ok is false when neither can be had.
func (o *DownloadOrchestrator) stagedStart(ctx context.Context, in *stagedInput) (float64, bool) {
	if in.walk != nil {
		if s, ok := in.walk.StartSeconds(); ok {
			return s, true
		}
	}
	if p := o.runFFprobe(ctx, in.read); p != nil {
		return p.StartSec, true
	}
	return 0, false
}

// walkStagedInput walks one staged recording.
func (o *DownloadOrchestrator) walkStagedInput(ctx context.Context, jobID, path string) (*stagedInput, error) {
	in := &stagedInput{path: path, read: path}
	walk, err := engine.WalkFMP4(ctx, path)
	switch {
	case err == nil:
		in.walk = walk
		if walk.Unreliable != "" {
			in.unmeasured = "its fragments' timeline is no measure of it: " + walk.Unreliable
		}
	case errors.Is(err, engine.ErrNotFMP4):
		// TS, WebM, a progressive MP4: measured by its probe, as always.
	case errors.Is(err, engine.ErrUnreadableFMP4):
		in.unmeasured = err.Error()
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		return nil, fmt.Errorf("read the staged recording %s: %w", filepath.Base(path), err)
	}
	if in.unmeasured != "" {
		o.logger.Warn("a staged recording cannot be checked against its own fragments; it is muxed as it stands and staging will be kept",
			"staged", path, "why", in.unmeasured, "jobID", jobID)
	}
	return in, nil
}

// writeRepairedCopy writes the damaged input's stream without its drops
// beside it, and points the copy at it. It refuses — the mux then fails with
// staging kept — when the staging volume has no room for it.
func (o *DownloadOrchestrator) writeRepairedCopy(ctx context.Context, jobID string, in *stagedInput) error {
	dst := in.path + stagedRepairSuffix
	need := in.walk.RepairedSize()
	// A repaired copy of this file an earlier mux left — the process died
	// before it removed it, or the removal failed — is the file this one
	// truncates: its bytes are no claim on the volume. Counted as used, it
	// refused every retry on a volume with room for one copy, pointing at
	// nothing. Removed first, or credited when it will not go.
	var leftover uint64
	if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() {
		switch err := os.Remove(dst); {
		case err == nil:
			o.logger.Info("removed a repaired copy an earlier mux left beside a staged recording", "path", dst, "jobID", jobID)
		case !os.IsNotExist(err):
			leftover = uint64(info.Size())
		}
	}
	if ds, err := repairDiskSpace(filepath.Dir(dst)); err == nil && ds.Free+leftover < uint64(need)+repairHeadroom {
		return fmt.Errorf("the staged %s has %d damaged stretch(es) to leave out, and the repaired copy it is muxed from needs %s free on the staging volume, which has %s — staging is preserved; free some space and re-run the Mux action",
			filepath.Base(in.path), len(in.walk.Drops), utils.FormatFileSize(need+repairHeadroom), utils.FormatFileSize(int64(ds.Free)))
	}
	for _, d := range in.walk.Drops {
		o.logger.Warn("a staged recording has a damaged stretch FFmpeg cannot read past; the archive is muxed from a repaired copy without it, and keeps a gap there",
			"staged", in.path, "dropped", engine.FormatFMP4Drop(d), "found", d.Reason, "jobID", jobID)
	}
	if _, err := engine.WriteFMP4Repair(ctx, in.path, in.walk, dst); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("write the repaired copy of the staged %s: %w", filepath.Base(in.path), err)
	}
	in.read = dst
	return nil
}

// removeRepairedCopies removes the repaired copies a copy read. A removal
// that fails leaves one behind in staging, for the job's staging cleanup and
// the boot sweep to try again (removeStaleRepairedCopies).
func (o *DownloadOrchestrator) removeRepairedCopies(jobID string, ins *stagedInputs) {
	for _, in := range ins.list() {
		if in.read == in.path {
			continue
		}
		if err := os.Remove(in.read); err != nil && !os.IsNotExist(err) {
			o.logger.Warn("could not remove the repaired copy of a staged recording; it stays in staging",
				"path", in.read, "err", err, "jobID", jobID)
		}
	}
}

// removeStaleRepairedCopies removes every repaired copy (stagedRepairSuffix)
// in a job's staging tree — the root and each seg_N dir, tombstoned ones
// included. A repaired copy is derived from the staged recording beside it,
// which a repair never touches, so removing one loses nothing; the caller
// makes sure no copy is reading it — the boot sweep under the job's staging
// claim, the staging cleanup once the job's own copies are done. Without
// this one a mux killed mid-copy (a restart, a self-update) left a copy the
// size of the recording in staging its mux-gap record keeps for good.
func (w *DownloadWorker) removeStaleRepairedCopies(jobID, stagingDir string) {
	if stagingDir == "" {
		return
	}
	dirs := []string{stagingDir}
	for _, sd := range segDirsOf(stagingDir, true) {
		dirs = append(dirs, sd.dir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), stagedRepairSuffix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				w.logger.Warn("could not remove a leftover repaired copy of a staged recording; it is tried again at the next start",
					"path", p, "err", err, "jobID", jobID)
				continue
			}
			w.logger.Info("removed a leftover repaired copy of a staged recording",
				"path", p, "jobID", jobID,
				"reason", "a mux that read it did not remove it (the process stopped mid-mux, or the removal failed); it is derived from the staged recording beside it, which is kept")
		}
	}
}

// reportStagingRepairs is the job-log note a finished copy leaves when it
// was made from a repaired input or could not be vouched for — logged before
// the caller's Finished write, which ends the job's log routing (CORE-12).
func (o *DownloadOrchestrator) reportStagingRepairs(jobID string, ins *stagedInputs, outputPath string) {
	for _, in := range ins.list() {
		switch {
		case in.walk != nil && in.walk.Damaged():
			var gaps []string
			for _, d := range in.walk.Drops {
				gaps = append(gaps, engine.FormatFMP4Drop(d))
			}
			args := []any{"output", outputPath, "staged", in.path, "gaps", strings.Join(gaps, " | "),
				"droppedBytes", in.walk.DroppedBytes()}
			if in.lead > 0 {
				args = append(args, "startGap", fmt.Sprintf("%s 0:00:00.0 to %s (%.1fs) of the output", engine.HandlerName(in.walk.Tracks[0].Handler), engine.FormatTimelineSeconds(in.lead), in.lead))
			}
			o.logger.Warn("muxed with a gap: damaged bytes of a staged recording were left out of the output, and every intact fragment after them was kept; staging is kept with the original bytes",
				append(args, "jobID", jobID)...)
		case in.unmeasured != "":
			o.logger.Warn("muxed without a check against the staged fragments; staging is kept",
				"output", outputPath, "staged", in.path, "why", in.unmeasured, "jobID", jobID)
		}
	}
}

// keepRepairedAside marks every file of a set-aside group recovered to out —
// so no later finalize or recovery muxes it again — and removes none of them:
// the copy was made from a repaired input, or nothing could vouch for it, so
// the aside holds bytes its sibling does not. A half of the restart whose own
// copy was whole gets a record holding it with the other half (HeldWith), and
// the pair stays a pair for an operator who wants to rework it by hand.
func (o *DownloadOrchestrator) keepRepairedAside(jobID string, g asideGroup, out string) {
	var damaged string
	for _, p := range g.files {
		if hasMuxGapRecord(p) {
			damaged = p
			break
		}
	}
	for _, p := range g.files {
		if !hasMuxGapRecord(p) {
			if info, err := os.Stat(p); err != nil {
				o.logger.Warn("could not hold a set-aside recording with the other half of its restart", "aside", p, "err", err, "jobID", jobID)
			} else if err := writeMuxGapRecord(p, muxGapRecord{Size: info.Size(), Output: out, HeldWith: damaged, At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				o.logger.Warn("could not hold a set-aside recording with the other half of its restart", "aside", p, "err", err, "jobID", jobID)
			}
		}
		if err := os.WriteFile(p+asideRecoveredMarker, []byte(out), 0o644); err != nil {
			o.logger.Warn("could not mark a set-aside recording recovered; the next pass muxes it again", "aside", p, "err", err, "jobID", jobID)
		}
	}
	o.logger.Warn("the set-aside recording stays in staging: its sibling was muxed from a repaired copy, or could not be checked, and the aside keeps what the sibling lacks",
		"output", out, "aside", strings.Join(g.files, " | "), "jobID", jobID)
}

// codecTypeOf is the ffprobe codec_type a track's handler type is copied as.
func codecTypeOf(handler string) string {
	switch handler {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	}
	return ""
}

// verifyAgainstStaging is ENGINE-9's check of a finished copy. Every input
// whose fragments the walk read is measured by them, TRACK BY TRACK: each
// video and audio track's span — its first readable fragment to the end of
// its last, a repaired gap included, since the fragments after it keep their
// decode times — against the duration of the copy's stream of that type, on
// muxedOutputIsShort's tolerance and floor. A copy missing a track the
// fragments carry is short too. The rest — inputs the walk has no business
// with, or cannot measure — keep the probe-based check verifyMuxedDuration
// has always made, against the copy's format duration.
//
// Measured by the probe, a damaged fMP4 input and the copy that stopped at
// its damage always agreed, and a copy whose video stopped at a third passed
// beside an intact audio track, because the format duration is the longest
// stream's.
func (o *DownloadOrchestrator) verifyAgainstStaging(ctx context.Context, jobID string, out *ffprobeData, ins *stagedInputs) error {
	expected := map[string]float64{}
	var fallback []string
	for _, in := range ins.list() {
		if !in.measured() {
			fallback = append(fallback, in.read)
			continue
		}
		for _, t := range in.walk.Tracks {
			if typ := codecTypeOf(t.Handler); typ != "" {
				expected[typ] = max(expected[typ], t.SpanSeconds())
			}
		}
	}
	for _, typ := range []string{"video", "audio"} {
		want, ok := expected[typ]
		if !ok || want < muxDurationCheckFloor.Seconds() {
			continue
		}
		got, present := out.streamSeconds(typ)
		if present && !muxedOutputIsShort(want, got) {
			continue
		}
		o.logger.Error("muxed output's "+typ+" is shorter than the staged fragments — the copy stopped at a bad fragment",
			"jobID", jobID, "fragmentSeconds", want, "outputSeconds", got)
		return fmt.Errorf("mux produced %.0fs of %s from %.0fs of staged fragments (%.0fs missing) — the copy stopped at a bad fragment; the short output was removed and staging is preserved, re-run the Mux action",
			got, typ, want, want-got)
	}
	if err := o.verifyAlignedStarts(jobID, out, ins); err != nil {
		return err
	}
	if len(fallback) > 0 {
		return o.verifyMuxedDuration(ctx, jobID, out.DurationSec, fallback...)
	}
	return nil
}

// verifyAlignedStarts checks a copy alignStagedInputs placed: each half's
// track must start in the copy where the offset put it, within
// muxStartTolerance. A copy whose tracks came out level anyway is the desync
// the offsets exist to prevent. A half the walk did not read, or that holds
// more than its one kind of track, is not judged.
func (o *DownloadOrchestrator) verifyAlignedStarts(jobID string, out *ffprobeData, ins *stagedInputs) error {
	if !ins.aligned {
		return nil
	}
	for _, h := range []struct {
		in   *stagedInput
		typ  string
		want float64
	}{{ins.video, "video", ins.offsets.Video}, {ins.audio, "audio", ins.offsets.Audio}} {
		if !h.in.carriesOnly(h.typ) {
			continue
		}
		got, ok := out.streamStart(h.typ)
		if !ok || math.Abs(got-h.want) <= muxStartTolerance {
			continue
		}
		o.logger.Error("muxed output's "+h.typ+" does not start where its staged fragments put it — the copy is out of step",
			"jobID", jobID, "wantStartSeconds", h.want, "outputStartSeconds", got)
		return fmt.Errorf("mux started the %s at %.1fs where its staged fragments put it at %.1fs — the copy is out of step; the output was removed and staging is preserved, re-run the Mux action",
			h.typ, got, h.want)
	}
	return nil
}

// muxGapRecord is what a copy that could not carry a staged recording whole
// leaves beside it (muxGapRecordSuffix).
type muxGapRecord struct {
	// Size is the recording's size when the copy read it. A record whose
	// recording has since changed size — a resume appended to it — or is
	// gone describes bytes that are not there, and shields nothing.
	Size int64 `json:"size"`
	// Output is the file the copy wrote.
	Output string `json:"output,omitempty"`
	// Dropped is each stretch the repair left out, as engine.FormatFMP4Drop
	// renders it.
	Dropped []string `json:"dropped,omitempty"`
	// DroppedBytes is how many bytes they cover.
	DroppedBytes int64 `json:"droppedBytes,omitempty"`
	// GapSeconds is the longest hole each drop leaves in any track, summed.
	GapSeconds float64 `json:"gapSeconds,omitempty"`
	// Unmeasured is why nothing independent of FFmpeg vouched for the copy.
	Unmeasured string `json:"unmeasured,omitempty"`
	// HeldWith is, for a set-aside recording kept only because the other
	// half of its restart was, that other half.
	HeldWith string `json:"heldWith,omitempty"`
	// At is when the copy was made, RFC 3339 UTC.
	At string `json:"at"`
}

// reason is the shield's one-line account of a record, for the Warn that
// keeps a staging dir and the docs that name it.
func (r muxGapRecord) reason() string {
	switch {
	case len(r.Dropped) > 0:
		return fmt.Sprintf("%d damaged stretch(es) (%d bytes, %.1fs) were left out of %s: %s",
			len(r.Dropped), r.DroppedBytes, r.GapSeconds, r.Output, strings.Join(r.Dropped, " | "))
	case r.Unmeasured != "":
		return "the copy into " + r.Output + " could not be checked against its fragments: " + r.Unmeasured
	case r.HeldWith != "":
		return "kept with the other half of its restart, " + r.HeldWith
	}
	return "recorded by an earlier mux"
}

// recordStagedInput writes the input's record when its copy will leave
// staging kept, and removes a stale one when it will not.
func (o *DownloadOrchestrator) recordStagedInput(in *stagedInput, outputPath string) error {
	if !in.keepsStaging() {
		if err := os.Remove(in.path + muxGapRecordSuffix); err != nil && !os.IsNotExist(err) {
			o.logger.Warn("could not remove a stale mux-gap record", "path", in.path+muxGapRecordSuffix, "err", err)
		}
		return nil
	}
	info, err := os.Stat(in.path)
	if err != nil {
		return fmt.Errorf("record the staged %s's mux gaps: %w", filepath.Base(in.path), err)
	}
	rec := muxGapRecord{Size: info.Size(), Output: outputPath, Unmeasured: in.unmeasured, At: time.Now().UTC().Format(time.RFC3339)}
	if in.walk != nil {
		for _, d := range in.walk.Drops {
			rec.Dropped = append(rec.Dropped, engine.FormatFMP4Drop(d))
			var widest float64
			for _, g := range d.Gaps {
				switch {
				case g.HasFrom && g.HasTo:
					widest = max(widest, g.To-g.From)
				case g.HasTo:
					// Before the first readable fragment: as long as the
					// lead the copy restores, when it restores one.
					widest = max(widest, in.lead)
				}
			}
			rec.GapSeconds += widest
		}
		rec.DroppedBytes = in.walk.DroppedBytes()
	}
	if err := writeMuxGapRecord(in.path, rec); err != nil {
		return fmt.Errorf("record the staged %s's mux gaps, without which its staging would not be kept: %w", filepath.Base(in.path), err)
	}
	return nil
}

// writeMuxGapRecord writes rec beside the staged recording at path.
func writeMuxGapRecord(path string, rec muxGapRecord) error {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return utils.WriteFileAtomic(path+muxGapRecordSuffix, raw, 0o644)
}

// readMuxGapRecord reads the record beside the staged recording at path, and
// reports false when there is none that holds: no record, an unreadable one,
// or a recording gone or no longer the size the record describes.
func readMuxGapRecord(path string) (muxGapRecord, bool) {
	var rec muxGapRecord
	raw, err := os.ReadFile(path + muxGapRecordSuffix)
	if err != nil || json.Unmarshal(raw, &rec) != nil {
		return rec, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != rec.Size {
		return rec, false
	}
	return rec, true
}

// hasMuxGapRecord reports whether a record that holds sits beside path.
func hasMuxGapRecord(path string) bool {
	_, ok := readMuxGapRecord(path)
	return ok
}

// muxGapNote is one staged recording a mux could not carry whole.
type muxGapNote struct {
	path string
	rec  muxGapRecord
}

// stagedMuxGaps returns every staged recording under a job's staging tree —
// the root and each seg_N dir, tombstoned ones included, as for the asides —
// that a record beside it says a mux could not carry whole. This is the
// staging-cleanup shield: what the record describes is bytes of the capture
// that are in no archive, kept until the job is deleted or an operator takes
// the dir.
func stagedMuxGaps(stagingDir string) []muxGapNote {
	if stagingDir == "" {
		return nil
	}
	dirs := []string{stagingDir}
	for _, sd := range segDirsOf(stagingDir, true) {
		dirs = append(dirs, sd.dir)
	}
	var out []muxGapNote
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), muxGapRecordSuffix)
			if !ok || e.IsDir() {
				continue
			}
			p := filepath.Join(dir, name)
			if rec, ok := readMuxGapRecord(p); ok {
				out = append(out, muxGapNote{path: p, rec: rec})
			}
		}
	}
	return out
}

// muxGapSummary is the Download Finished embed's line for a job whose staging
// holds mux-gap records, "" for one whose does not; dropped reports that the
// records describe bytes left out of an output rather than only a copy
// nothing could check.
func muxGapSummary(stagingDir string) (summary string, dropped bool) {
	notes := stagedMuxGaps(stagingDir)
	if len(notes) == 0 {
		return "", false
	}
	var stretches int
	var seconds float64
	unmeasured := false
	for _, n := range notes {
		stretches += len(n.rec.Dropped)
		seconds += n.rec.GapSeconds
		unmeasured = unmeasured || n.rec.Unmeasured != ""
	}
	var parts []string
	if stretches > 0 {
		parts = append(parts, fmt.Sprintf("%d damaged stretch(es) left out (%.1fs)", stretches, seconds))
	}
	if unmeasured {
		parts = append(parts, "not checked against the staged fragments")
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "; ") + " — staging kept", stretches > 0
}
