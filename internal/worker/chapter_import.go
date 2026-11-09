package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/childproc"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The worker's half of the chapter import (owner decision CI-1): the per-job
// claim an import holds, and the verified stream-copy re-mux that embeds the
// imported list in a single-file archive. What to import, the sidecar it is
// saved to and the original it keeps are the REST layer's
// (internal/web/routes/chapter_import.go), which the TUI calls in process.

// opChapterImport is the chapter import's name in the per-job claim
// (claimJobOperation), so the Mux action or a set-aside recovery refused while
// it runs says what is in the way.
const opChapterImport jobOp = "chapter import"

// ErrArchiveJobActive: the job is downloading or muxing, so its archive is not
// finished and cannot be rewritten (owner decision CI-1: only a job that is not
// active can be imported). The REST layer maps it to 409.
var ErrArchiveJobActive = errors.New("job is active; its archive cannot be rewritten while it records or muxes")

// ErrArchiveRemuxNoSpace: the archive's volume has no room for the re-mux's
// temporary copy (owner decision PR-I4), so the archive is left as it is and
// the chapter list stays a sidecar only. The REST layer reports it on the
// save's view (ChapterImportView.NoSpace) and in its embed error, which both
// UIs show.
var ErrArchiveRemuxNoSpace = errors.New("not enough free space to re-mux the archive")

// remuxHeadroom is what the archive's volume must keep free beyond the
// re-mux's full temporary copy of the archive: the FFMETADATA file, the
// copy's own index growing past the original's, and room for whatever else
// writes to the volume (a live capture) while the copy is written.
const remuxHeadroom = 256 << 20

// ClaimChapterImport takes the job's per-job claim for a chapter import and
// returns its release, to be called once the import is done. The claim is
// the one MuxJob and RecoverAsides take (claimJobOperation), so neither runs
// while an import rewrites the archive and an import is refused while either
// runs; and the trim service asks for it before every trim (TrimService
// archiveRewriting), so a trim never reads an archive mid-replace and an
// import never replaces one a trim is reading. Refused, too, while the job's
// last run is still registered (MuxJob's reason: a Cancelled row whose run
// has not stopped is about to be written again) and while the row is active.
//
// The order against a trim is what makes the pair exclusive without one lock
// around both: the import takes its claim and then looks for a trim, and a
// trim takes its slot and then looks for the claim — so whichever looks
// second sees the other, and at worst both refuse.
func (w *DownloadWorker) ClaimChapterImport(jobID string) (release func(), err error) {
	if w.queue.IsProcessing(jobID) {
		return nil, fmt.Errorf("%w (the job's last run is still stopping)", ErrStagingBusy)
	}
	release, err = w.claimJobOperation(jobID, opChapterImport)
	if err != nil {
		return nil, err
	}
	if ts := w.orchestrator.trims; ts != nil && ts.trimming(jobID) {
		release()
		return nil, fmt.Errorf("%w (a trim in progress)", ErrStagingBusy)
	}
	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil {
		release()
		if err == nil {
			err = fmt.Errorf("job %s not found", jobID)
		}
		return nil, err
	}
	if IsActiveJobStatus(job.Status) {
		release()
		return nil, fmt.Errorf("%w (status %s)", ErrArchiveJobActive, job.Status)
	}
	return release, nil
}

// chapterImportRunning reports whether a chapter import holds jobID's claim —
// the trim service's question before it starts a trim.
func (w *DownloadWorker) chapterImportRunning(jobID string) bool {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	return w.stagingClaims[jobID] == opChapterImport
}

// ChapterImportRunning reports whether a chapter import holds jobID's claim:
// the REST layer's question before Resume, Retry and Reinitialize, which it
// refuses with 409 while one does (owner decision PR-M5). Each of them starts
// what the import must not race — a run whose finalize writes the archive
// the re-mux is about to rename its copy over, or a reset that deletes the
// parts and staging beside it.
func (w *DownloadWorker) ChapterImportRunning(jobID string) bool {
	return w.chapterImportRunning(jobID)
}

// waitChapterImport returns once no chapter import holds jobID's claim, or
// reports false when ctx ends first. What waits (what) is logged once, when
// there is something to wait for. The finalize of a run (muxAndFinalize,
// through the orchestrator's waitArchiveRewrite) and a deferred Resume or
// Reinitialize (afterJobExit) wait here (owner decision PR-M5): the import
// replaces the archive in place, and a finalize that wrote it meanwhile was
// renamed over by the import's copy of the OLD archive.
//
// Only the import: the finalize runs inside a Mux that holds the claim
// itself. afterJobExit waits for any claim (awaitStagingClaim).
func (w *DownloadWorker) waitChapterImport(ctx context.Context, jobID, what string) bool {
	_, ok := w.awaitStagingClaim(ctx, jobID, what, isChapterImport, false)
	return ok
}

// ArchiveRemuxResult is what RemuxArchiveChapters measured on the archive it
// put in place.
type ArchiveRemuxResult struct {
	SizeBytes   int64
	DurationSec float64
}

// remuxProbeTimeout bounds each ffprobe of the archive and of its re-mux:
// both read the container header and its index, so a healthy probe takes
// milliseconds, and the bound is for a file on a stalled network share.
const remuxProbeTimeout = 30 * time.Second

// RemuxArchiveChapters embeds chapters in the single-file archive at path
// (owner decision CI-1: "re-mux a single-file archive by stream copy with the
// chapters embedded, so VLC, mpv and Plex see them"); an empty list removes
// the archive's chapters (a restore to an archive that had none). The caller
// holds ClaimChapterImport.
//
// The copy is engine.Muxer.RemuxChapters: every stream, the global metadata
// and the yt:stretch aspect kept, into a temporary file beside the archive
// that replaces it only once it passed remuxCheck — the archive copy's own
// output checks (streams and duration; muxedOutputIsShort, verifiedCopy),
// with the original archive as the input it is measured against, plus the
// chapter count the list asked for. Any failure leaves the archive exactly as
// it was. The row's file_size and length_seconds follow the replaced file,
// written only while the row still holds expect, the status the caller read
// (UpdateJobFieldsIf), so an operator's action that landed in between is not
// written over.
//
// Not the caller's context: the copy runs under the orchestrator's mux root,
// which Stop cancels, and a closed browser tab must not kill a re-mux half
// way. The row being deleted stops it — the archive it would put back is no
// longer anyone's — as MuxJob stops on a delete. The archive's stem is
// claimed (claimOutputStem) while FFmpeg writes, so the orphan sweep never
// offers the temporary file, or the archive, as unowned.
func (w *DownloadWorker) RemuxArchiveChapters(jobID string, expect database.JobStatus, path string, chapters []utils.Chapter) (*ArchiveRemuxResult, error) {
	ctx, cancel := context.WithCancel(w.orchestrator.muxRoot())
	defer cancel()
	unsubscribe := w.db.OnJobDeleted(func(deleted *database.JobDeleted) {
		if deleted.JobID == jobID {
			cancel()
		}
	})
	defer unsubscribe()

	if err := w.remuxRoom(path); err != nil {
		return nil, err
	}

	muxer := w.orchestrator.mux()
	ffprobePath := muxer.FFprobePath()
	orig, err := probeArchiveShape(ctx, ffprobePath, path)
	if err != nil {
		return nil, fmt.Errorf("read the archive before the re-mux: %w", err)
	}

	metaPath := ""
	if len(chapters) > 0 {
		f, err := os.CreateTemp("", "moombox-chapters-*.ffmeta")
		if err != nil {
			return nil, fmt.Errorf("write the chapter list: %w", err)
		}
		metaPath = f.Name()
		defer os.Remove(metaPath)
		_, werr := f.WriteString(utils.FFMetadataChapters(chapters))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, fmt.Errorf("write the chapter list: %w", werr)
		}
	}

	defer claimOutputStem(jobID, strings.TrimSuffix(path, filepath.Ext(path)))()
	var out *archiveShape
	check := func(ctx context.Context, tmp string) error {
		got, err := probeArchiveShape(ctx, ffprobePath, tmp)
		if err != nil {
			return fmt.Errorf("read the re-mux: %w", err)
		}
		out = got
		if err := remuxCheck(orig, got, len(chapters)); err != nil {
			return err
		}
		// Asked last, immediately before the rename, so a row deleted while
		// FFmpeg wrote, or while the copy was being probed, puts nothing back
		// in its archive's place — and nor does a row that moved on (owner
		// decision PR-M5). The copy is of the archive as it was when the
		// import read it: a row no longer in the status the caller read, or
		// with a run in flight, may have a new archive at this name by now,
		// or one about to be written, and the rename would put the old one
		// back over it.
		if err := ctx.Err(); err != nil {
			return err
		}
		return w.archiveStillAsRead(jobID, expect)
	}
	if err := muxer.RemuxChapters(ctx, path, path, metaPath, check); err != nil {
		return nil, err
	}

	res := &ArchiveRemuxResult{DurationSec: out.durationSec}
	fields := map[string]any{}
	if info, err := os.Stat(path); err == nil {
		res.SizeBytes = info.Size()
		fields["file_size"] = info.Size()
	}
	if out.durationSec > 0 {
		fields["length_seconds"] = int(out.durationSec)
	}
	if len(fields) > 0 {
		if !w.db.UpdateJobFieldsIf(jobID, expect, fields) {
			w.logger.Warn("chapter import: the archive was re-muxed, but the row's status moved on, so its size and length were not written",
				"jobID", jobID, "expected", expect)
		}
	}
	return res, nil
}

// remuxRoom is the re-mux's disk check, made before it writes anything (owner
// decision PR-I4): the re-mux writes a full temporary copy of the archive
// beside it, so the archive's volume needs the archive's size plus
// remuxHeadroom free, and the copy must not take the volume to the
// disk_critical threshold — the reading that closes backlog admission and
// alerts the operator. Gap-fill (diskCritical) and the mux repair
// (repairDiskSpace) guard their copies the same way; the import wrote one
// with no check at all, which on a shared, nearly full volume starved a live
// capture of the space it was writing into. A volume that cannot be read is
// not refused: the copy's own write fails cleanly, leaving the archive.
func (w *DownloadWorker) remuxRoom(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // the probe after this reports the archive's own error
	}
	query := w.diskSpace
	if query == nil {
		query = disk.GetDiskSpace
	}
	ds, err := query(filepath.Dir(path))
	if err != nil || ds == nil {
		return nil
	}
	need := uint64(info.Size()) + remuxHeadroom
	if ds.Free < need {
		return fmt.Errorf("%w: the archive's re-mux needs %s free on its volume, which has %s; the file was not re-muxed",
			ErrArchiveRemuxNoSpace, utils.FormatFileSize(int64(need)), utils.FormatFileSize(int64(ds.Free)))
	}
	var thresholds config.DiskConfig
	w.readConfig(func(c *config.MoomboxConfig) { thresholds = c.Disk })
	after := ds.UsedPct
	if ds.Total > 0 {
		after += float64(need) / float64(ds.Total) * 100
	}
	if thresholds.AtCritical(after) {
		return fmt.Errorf("%w: the archive's re-mux would take its volume to %.1f%% used, past the disk_critical threshold (%d%%); the file was not re-muxed",
			ErrArchiveRemuxNoSpace, after, thresholds.CriticalPercent)
	}
	return nil
}

// archiveStillAsRead is the re-mux's last question before its rename: is the
// row still in expect, the status the import read under its claim, with no
// run of the job in flight? An error says what moved, and the archive is
// left as it is.
func (w *DownloadWorker) archiveStillAsRead(jobID string, expect database.JobStatus) error {
	if w.queue.IsProcessing(jobID) {
		return fmt.Errorf("a run of the job started while the archive was re-muxed; the archive is left as it was")
	}
	fresh, err := w.db.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("re-read the job before replacing the archive: %w", err)
	}
	if fresh == nil {
		return fmt.Errorf("the job was deleted while the archive was re-muxed; the archive is left as it was")
	}
	if fresh.Status != expect {
		return fmt.Errorf("the job's status moved from %s to %s while the archive was re-muxed; the archive is left as it was", expect, fresh.Status)
	}
	return nil
}

// archiveShape is what the re-mux check compares: the duration ffprobe gives
// each media stream and the file, and how many chapters the file carries.
// Data streams are not counted: an MP4's chapter track is one
// (engine.Muxer.RemuxChapters drops the old one and writes its own), and the
// archive holds no other.
type archiveShape struct {
	durationSec float64
	streams     map[string][]float64 // codec_type → each stream's duration
	chapters    int
}

// probeArchiveShape reads a file's archiveShape with one ffprobe run.
func probeArchiveShape(ctx context.Context, ffprobePath, path string) (*archiveShape, error) {
	ctx, cancel := context.WithTimeout(ctx, remuxProbeTimeout)
	defer cancel()
	raw, err := childproc.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json",
		"-show_streams", "-show_format", "-show_chapters", engine.FFmpegPathArg(path)).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Chapters []json.RawMessage `json:"chapters"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}
	shape := &archiveShape{streams: map[string][]float64{}, chapters: len(probe.Chapters)}
	shape.durationSec, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	for _, s := range probe.Streams {
		if s.CodecType == "data" {
			continue
		}
		d, _ := strconv.ParseFloat(s.Duration, 64)
		shape.streams[s.CodecType] = append(shape.streams[s.CodecType], d)
	}
	return shape, nil
}

// remuxCheck is the verdict on a re-mux, verifyArchiveRemux in production; a
// variable so a test can refuse a copy FFmpeg made well.
var remuxCheck = verifyArchiveRemux

// Re-mux shortfall tolerance (owner decision PR-M7): how much shorter than
// the archive it copies a re-mux's stream, or the file, may be. The larger of
// remuxShortfallMinSec and remuxShortfallFraction of the original's length,
// with no floor below which nothing is measured.
//
// Deliberately NOT the live capture's muxedOutputIsShort (a 20 s shortfall
// allowed, nothing under 60 s judged): that tolerance is for a mux of raw
// fragments whose container metadata is unreliable, and this is a stream copy
// of one finished file into a copy that REPLACES it, the only copy there is.
// A whole-file copy has no reason to lose anything, so a second (container
// rounding of the last frame or audio packet) or half a percent (an edit list
// or a stretched timebase measured a little differently, on a long archive)
// is all it is allowed; a clip under a minute is now measured too.
const (
	remuxShortfallMinSec   = 1.0
	remuxShortfallFraction = 0.005
)

// remuxIsShort reports whether a re-mux's duration, outSec, falls short of
// the original's, origSec, by more than the re-mux tolerance. An original the
// probe could not read (0) cannot be judged; an output LONGER than its
// original never counts as short.
func remuxIsShort(origSec, outSec float64) bool {
	if origSec <= 0 {
		return false
	}
	return origSec-outSec > max(remuxShortfallMinSec, remuxShortfallFraction*origSec)
}

// verifyArchiveRemux holds a re-mux to the archive it copies: the same media
// streams, each kind as many as before, none of them — and not the file —
// shorter than its original by more than the re-mux's own tolerance
// (remuxIsShort: about a second or 0.5%, measured however short the archive
// is), and exactly the chapters the list asked for. A stream copy of a whole
// file has no reason to differ from it in anything but its chapters, so any
// difference means the copy went wrong and the archive must stay as it is.
func verifyArchiveRemux(orig, out *archiveShape, wantChapters int) error {
	for kind, durs := range orig.streams {
		got := out.streams[kind]
		if len(got) != len(durs) {
			return fmt.Errorf("the re-mux has %d %s stream(s), the archive %d; the archive is left as it was", len(got), kind, len(durs))
		}
		for i := range durs {
			if remuxIsShort(durs[i], got[i]) {
				return fmt.Errorf("the re-mux's %s stream is %.1fs, the archive's %.1fs; the archive is left as it was", kind, got[i], durs[i])
			}
		}
	}
	for kind := range out.streams {
		if _, ok := orig.streams[kind]; !ok {
			return fmt.Errorf("the re-mux has a %s stream the archive does not; the archive is left as it was", kind)
		}
	}
	if remuxIsShort(orig.durationSec, out.durationSec) {
		return fmt.Errorf("the re-mux is %.1fs, the archive %.1fs; the archive is left as it was", out.durationSec, orig.durationSec)
	}
	if out.chapters != wantChapters {
		return fmt.Errorf("the re-mux carries %d chapter(s), %d were asked for; the archive is left as it was", out.chapters, wantChapters)
	}
	return nil
}

// dropImportedArchiveChapters removes the chapter list an import saved beside
// a single-file archive (owner decision CI-1, `<name>.chapters.json` with its
// "original"), once a finalize of a NEW recording has written an archive at
// that name, and when a Reinitialize resets the row (reinitializeNow). The
// list was fitted to the recording it was imported into, and GET
// /api/jobs/{id}/chapters answers an import's list ahead of the file's own —
// so a list left behind would go on overriding the new archive's chapters,
// and Restore would put the old file's original onto it. The new archive
// carries the chapters its own finalize chose.
//
// Which finalizes drop it (owner decisions CJ-2, CK-1): a Reinitialize's
// re-download, a new recording, and a first download of a fresh row — an
// archive name can be reused (a deleted row's files kept, a template that
// resolves alike), and a list beside it cannot be told from the job's own
// there. A complete VOD that superseded the parts is a new recording too,
// and drops the parts' list on its own (supersedePartsWithVod). A
// re-finalize of the recording the archive already holds — MuxJob's
// (JobContext.Refinalize: the Mux action over an imported Error or
// Cancelled row, the automatic mux of an ended Twitch broadcast, a boot
// re-mux), a download run that continues it (a Resume, a retry:
// runJobContext, continuesRecording) and a Twitch gap-fill
// (refitSplitArchiveChapters) — keeps the import instead (keptImportFor),
// read only from beside the archive the row names.
// A split archive's list has its own lifecycle (removeSplitArchiveChapters,
// writeSplitArchiveChapters). Best effort: a removal that fails is logged.
func dropImportedArchiveChapters(outputFile string, lg logger) {
	path := ArchiveSidecarPath(outputFile, utils.ChaptersSidecarSuffix)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		lg.Warn("could not remove the archive's imported chapter list", "path", path, "err", err)
	}
}

// keptImport is a chapter import a re-finalize carries onto the archive it
// writes (owner decision CJ-2): the imported list and the original it kept,
// both on the player's global timeline, as the import saved them. fit
// refits both to the new archive and writes them.
type keptImport struct {
	from     string // the sidecar it was read from
	chapters []utils.Chapter
	original []utils.Chapter
}

// keptImportFor is the import a finalize keeps, nil for none: only a
// re-finalize of the same recording (jobCtx.Refinalize: MuxJob's, CJ-2, or
// a Resume's or retry's, CK-1) keeps one, read from
// the first of paths — the sidecars it could sit beside — that an import
// wrote. A sidecar that cannot be read is no import to keep: the finalize
// then writes its own chapters, as before CJ-2, and says so.
func keptImportFor(jobCtx *JobContext, lg logger, paths ...string) *keptImport {
	if !jobCtx.Refinalize {
		return nil
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		imp, imported, err := utils.ReadImportedChaptersSidecar(p)
		if err != nil {
			lg.Warn("re-finalize: the archive's imported chapter list is unreadable; the archive gets its own chapters", "path", p, "err", err)
			continue
		}
		if imported {
			return &keptImport{
				from:     p,
				chapters: utils.ChaptersFromSidecar(imp.Chapters),
				original: utils.ChaptersFromSidecar(imp.Original),
			}
		}
	}
	return nil
}

// refit fits the import and its original to a timeline durationMs long
// (utils.RefitImportedChapters) and returns the imported list as fitted.
func (k *keptImport) refit(durationMs int64) []utils.Chapter {
	k.chapters = utils.RefitImportedChapters(k.chapters, durationMs)
	k.original = utils.RefitImportedChapters(k.original, durationMs)
	return k.chapters
}

// write saves the kept import at path — "original" and all, so Restore
// still offers it — and removes the sidecar it was read from when that was
// another (an archive the re-finalize moved to a fresh template name).
func (k *keptImport) write(path string, lg logger) {
	if path == "" {
		return
	}
	if err := utils.WriteImportedChaptersSidecar(path, k.chapters, utils.SidecarFromChapters(k.original)); err != nil {
		lg.Warn("re-finalize: could not keep the archive's imported chapter list", "path", path, "err", err)
		return
	}
	if !sameFilePath(k.from, path) {
		if err := os.Remove(k.from); err != nil && !os.IsNotExist(err) {
			lg.Warn("re-finalize: could not remove the imported chapter list under the archive's old name", "path", k.from, "err", err)
		}
	}
}

// sameFilePath reports whether a and b name the same file: the same path once
// normalised (normalizePath, which folds case on Windows, as output_claims.go
// compares stems), or two names the filesystem resolves to one file
// (os.SameFile: a case-only difference on NTFS or a case-insensitive APFS
// volume, a symlinked directory, a hard link).
//
// The question a re-finalize asks before it removes the sidecar it read from
// once it has written the one beside the archive (owner decision PR-M8). A raw
// string compare called "X.chapters.json" and "x.chapters.json" two files, and
// on NTFS removing the first deletes the second, the file just written: the
// import and the original it kept were gone, on Windows only.
func sameFilePath(a, b string) bool {
	if normalizePath(a) == normalizePath(b) {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}
