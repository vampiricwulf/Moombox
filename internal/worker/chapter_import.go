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
	"github.com/vampiricwulf/Moombox/internal/database"
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
const opChapterImport = "chapter import"

// ErrArchiveJobActive: the job is downloading or muxing, so its archive is not
// finished and cannot be rewritten (owner decision CI-1: only a job that is not
// active can be imported). The REST layer maps it to 409.
var ErrArchiveJobActive = errors.New("job is active; its archive cannot be rewritten while it records or muxes")

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
		// Asked last, so a row deleted while FFmpeg wrote, or while the
		// copy was being probed, puts nothing back in its archive's place.
		return ctx.Err()
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

// verifyArchiveRemux holds a re-mux to the archive it copies: the same media
// streams, each kind as many as before, none of them — and not the file —
// shorter than its original by more than the archive copy's own tolerance
// (muxedOutputIsShort, ENGINE-9's check, which judges only an input long
// enough to be measured), and exactly the chapters the list asked for. A
// stream copy of a whole file has no reason to differ from it in anything
// but its chapters, so any difference means the copy went wrong and the
// archive must stay as it is.
func verifyArchiveRemux(orig, out *archiveShape, wantChapters int) error {
	for kind, durs := range orig.streams {
		got := out.streams[kind]
		if len(got) != len(durs) {
			return fmt.Errorf("the re-mux has %d %s stream(s), the archive %d; the archive is left as it was", len(got), kind, len(durs))
		}
		for i := range durs {
			if muxedOutputIsShort(durs[i], got[i]) {
				return fmt.Errorf("the re-mux's %s stream is %.0fs, the archive's %.0fs; the archive is left as it was", kind, got[i], durs[i])
			}
		}
	}
	for kind := range out.streams {
		if _, ok := orig.streams[kind]; !ok {
			return fmt.Errorf("the re-mux has a %s stream the archive does not; the archive is left as it was", kind)
		}
	}
	if muxedOutputIsShort(orig.durationSec, out.durationSec) {
		return fmt.Errorf("the re-mux is %.0fs, the archive %.0fs; the archive is left as it was", out.durationSec, orig.durationSec)
	}
	if out.chapters != wantChapters {
		return fmt.Errorf("the re-mux carries %d chapter(s), %d were asked for; the archive is left as it was", out.chapters, wantChapters)
	}
	return nil
}
