package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// The chapter import (owner decisions CI-1 to CI-5): a viewer's timestamp
// comment, pasted in the Web player's dialog or the TUI's prompt, parsed by
// the one parser (utils.ParseChapterComment), shifted by an offset, fitted to
// the archive, and saved as the job's chapters. Both UIs reach it through
// ChapterService — the Web UI over the three POST routes below, the TUI in
// process — so a preview, a save and a restore mean one thing everywhere.

// ChapterImporter is the worker's half of the import: the per-job claim that
// keeps the re-mux off the Mux action, a set-aside recovery and a trim, and the
// verified re-mux itself. *worker.DownloadWorker in production.
type ChapterImporter interface {
	ClaimChapterImport(jobID string) (release func(), err error)
	RemuxArchiveChapters(jobID string, expect database.JobStatus, path string, chapters []utils.Chapter) (*worker.ArchiveRemuxResult, error)
}

// ChapterImportRefusal is an import, preview or restore refused, with the
// HTTP status the route answers and a reason both UIs show as it stands.
type ChapterImportRefusal struct {
	Status int
	Reason string
}

func (e *ChapterImportRefusal) Error() string { return e.Reason }

func refuseChapters(status int, format string, args ...any) error {
	return &ChapterImportRefusal{Status: status, Reason: fmt.Sprintf(format, args...)}
}

// ChapterImportView is what a preview, a save and a restore answer: the list
// (parsed and fitted for a preview, as saved otherwise), the job's chapters
// as the player shows them now, whether an original is kept to restore, and
// the numbers the dialog shows beside them. All times are seconds on the
// player's global timeline.
type ChapterImportView struct {
	Chapters []Chapter `json:"chapters"`
	Current  []Chapter `json:"current"`
	// HasOriginal: an import kept the archive's chapters from before it
	// (owner decision CI-3), so "Restore original" is offered.
	HasOriginal bool `json:"hasOriginal"`
	// OffsetSeconds is the offset the list was shifted by.
	OffsetSeconds float64 `json:"offsetSeconds"`
	// DefaultOffsetSeconds is what the offset field starts at (owner
	// decision CI-5): the capture's start in the video's own timeline,
	// negated, where Moombox knows it. See defaultChapterOffset.
	DefaultOffsetSeconds float64 `json:"defaultOffsetSeconds"`
	// DefaultOffsetText is DefaultOffsetSeconds as the field shows it
	// pre-filled, a signed clock ("-1:23:45", "-3:28.033"; owner decision
	// CK-2), and "" when the start is unknown and the field is left empty
	// for the operator. Both UIs pre-fill from it, so the clock is written
	// in one place (utils.FormatChapterOffset).
	DefaultOffsetText string `json:"defaultOffsetText"`
	// DefaultOffsetNote says where the offset came from, always (owner
	// decisions CJ-3, CK-2): "joined 1:23:45 in", "recorded from the
	// start", "VOD", or "start unknown — enter it by hand". Both UIs show
	// it under the field, after "Pre-filled: " when DefaultOffsetText is
	// set.
	DefaultOffsetNote string  `json:"defaultOffsetNote"`
	DurationSeconds   float64 `json:"durationSeconds"`
	// Split: the archive is in parts, so the list is saved beside them only
	// (owner decision CI-1: recorded parts are never rewritten).
	Split bool `json:"split"`
	// Error is a preview's reason for an empty list ("No timestamps
	// found"); a save refuses with it instead.
	Error string `json:"error,omitempty"`
	// Embedded: a save or restore re-muxed the single-file archive with the
	// list. EmbedError is why it did not, when it tried — the list is saved
	// beside the archive all the same, and the archive is as it was.
	Embedded   bool   `json:"embedded"`
	EmbedError string `json:"embedError,omitempty"`
}

// ChapterService is the import behind both UIs.
type ChapterService struct {
	deps  *ChapterRouteDeps
	cache *chapterCache
}

// chapterTarget is where a job's imported chapters go: its single-file
// archive (re-muxed) or none for a split one, the sidecar either way, and the
// length of the player's timeline the list is fitted to.
type chapterTarget struct {
	archive    string
	sidecar    string
	durationMs int64
	split      bool
}

// importableStatus is the rows an import may rewrite (owner decision CI-1): a
// job that is not active and has an archive — Finished, or an Error or
// Cancelled row whose archive file exists (chapterTargetFor refuses one
// without).
func importableStatus(s database.JobStatus) bool {
	switch s {
	case database.StatusFinished, database.StatusError, database.StatusCancelled:
		return true
	}
	return false
}

// chapterTargetFor resolves job's archive and sidecar under the containment
// guard the player's own routes apply, and measures its timeline: a
// single-file archive's length as ffprobe reads it (the row's
// length_seconds when it cannot), a split one's parts laid end to end
// (SegmentPlayer.init's layout, which the chapters route reads in).
func (s *ChapterService) chapterTargetFor(ctx context.Context, job *database.Job) (*chapterTarget, error) {
	if worker.IsActiveJobStatus(job.Status) {
		return nil, refuseChapters(http.StatusConflict, "The job is %s; chapters can be imported once it has finished", job.Status)
	}
	if !importableStatus(job.Status) {
		return nil, refuseChapters(http.StatusConflict, "The job has no finished archive to import chapters into")
	}
	segments, err := s.deps.DB.GetSegments(job.ID)
	if err != nil {
		return nil, refuseChapters(http.StatusInternalServerError, "Could not read the job's parts")
	}
	if len(segments) > 0 {
		sidecar, ok := splitSidecarPath(job, segments, s.deps)
		if !ok {
			return nil, refuseChapters(http.StatusNotFound, "The job's parts are missing")
		}
		var total float64
		for _, seg := range segments {
			total += seg.DurationSeconds
		}
		return &chapterTarget{sidecar: sidecar, durationMs: int64(math.Round(total * 1000)), split: true}, nil
	}
	path, msg, code := jobVideoPath(job, s.deps.Store)
	if code != 0 {
		return nil, refuseChapters(code, "The job's archive cannot be read: %s", msg)
	}
	durationMs := int64(math.Round(ffprobeDurationSeconds(ctx, s.ffprobePath(), path) * 1000))
	if durationMs <= 0 && job.LengthSeconds != nil {
		durationMs = int64(*job.LengthSeconds) * 1000
	}
	return &chapterTarget{archive: path, sidecar: worker.ArchiveSidecarPath(path, utils.ChaptersSidecarSuffix), durationMs: durationMs}, nil
}

func (s *ChapterService) ffprobePath() string {
	var ffmpegPath string
	s.deps.Store.Read(func(c *config.MoomboxConfig) { ffmpegPath = c.Paths.FfmpegPath })
	return engine.NewMuxer(ffmpegPath, s.deps.Logger).FFprobePath()
}

// HasOriginal reports whether jobID's chapters were imported, so an original
// is kept to restore (the TUI's A U is offered only then). It reads the
// sidecar and nothing else — no probe — because the TUI asks it per job
// while it builds a chord's job list.
func (s *ChapterService) HasOriginal(jobID string) bool {
	job, err := s.load(jobID)
	if err != nil || !importableStatus(job.Status) {
		return false
	}
	var sidecar string
	segments, err := s.deps.DB.GetSegments(job.ID)
	switch {
	case err != nil:
		return false
	case len(segments) > 0:
		p, ok := splitSidecarPath(job, segments, s.deps)
		if !ok {
			return false
		}
		sidecar = p
	default:
		path, _, code := jobVideoPath(job, s.deps.Store)
		if code != 0 {
			return false
		}
		sidecar = worker.ArchiveSidecarPath(path, utils.ChaptersSidecarSuffix)
	}
	_, imported, err := utils.ReadImportedChaptersSidecar(sidecar)
	return err == nil && imported
}

// The notes defaultChapterOffset gives beside the pre-filled offset (owner
// decision CK-2) when it is not a late join's "joined H:MM:SS in".
const (
	offsetNoteFromStart = "recorded from the start"
	offsetNoteVod       = "VOD"
	offsetNoteUnknown   = "start unknown — enter it by hand"
)

// defaultChapterOffset is the offset the import's field starts at (owner
// decision CI-5: the capture's known start in the video's timeline, negated,
// and 0 otherwise), the field's pre-filled text (a signed clock, owner
// decision CK-2; "" when the start is unknown) and the note both UIs show
// beside it to say where it came from — always one (CK-2):
//
//   - a VOD is the video's own timeline: 0, "VOD";
//   - a live YouTube capture's join point is on its row since CJ-3
//     (join_offset_seconds, written by the finalize from the staged
//     recording — a split archive's from part 1's since CK-3): a capture
//     recorded from sq=0 has 0, "recorded from the start"; one that joined
//     late its join point, so a viewer's "1:30:00" lands 1:30:00 minus the
//     join into the archive, "joined 1:23:45 in";
//   - a row whose join point is unknown (NULL: a Twitch capture, a row from
//     before the column, a start never recorded) is 0 with the field left
//     empty, "start unknown — enter it by hand", for the operator to set;
//   - a trim's start would be negated too, and added to the join point when
//     both apply — but a trim is a file of its own beside the archive
//     (TrimService), never the archive the import rewrites, so no imported
//     archive has a trim start, and that term is always 0.
func defaultChapterOffset(job *database.Job) (seconds float64, text, note string) {
	switch {
	case job != nil && job.IsVod:
		return 0, utils.FormatChapterOffset(0), offsetNoteVod
	case job == nil || job.JoinOffsetSeconds == nil:
		return 0, "", offsetNoteUnknown
	case *job.JoinOffsetSeconds <= 0:
		return 0, utils.FormatChapterOffset(0), offsetNoteFromStart
	}
	joinMs := int64(math.Round(*job.JoinOffsetSeconds * 1000))
	join := float64(joinMs) / 1000
	return -join, utils.FormatChapterOffset(-joinMs), "joined " + clockText(join) + " in"
}

// clockText is a non-negative number of seconds as H:MM:SS, or M:SS under
// an hour — the chapter tables' own format (formatTimestamp, chapterClock).
func clockText(sec float64) string {
	s := int64(max(sec, 0))
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// chapterImportBody is the three POST routes' body: the pasted comment and
// the offset, as a number of seconds or as the field's own text ("-1:30",
// "-90"), which utils.ParseChapterOffset reads — the one offset parser, as
// ParseChapterComment is the one comment parser.
type chapterImportBody struct {
	Text          string          `json:"text"`
	OffsetSeconds json.RawMessage `json:"offsetSeconds"`
}

// offsetText is the body's offset as ParseChapterOffset's input.
func (b chapterImportBody) offsetText() string {
	raw := strings.TrimSpace(string(b.OffsetSeconds))
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(b.OffsetSeconds, &s) == nil {
		return s
	}
	return raw
}

// toChapters is a fitted list in the route's shape, seconds.
func toChapters(list []utils.Chapter) []Chapter {
	out := make([]Chapter, 0, len(list))
	for _, c := range list {
		out = append(out, Chapter{Start: float64(c.StartMs) / 1000, End: float64(c.EndMs) / 1000, Title: c.Title})
	}
	return out
}

// view starts a ChapterImportView for job: its chapters as the player shows
// them now and whether an original is kept.
func (s *ChapterService) view(ctx context.Context, job *database.Job, t *chapterTarget) *ChapterImportView {
	defaultOffset, defaultText, defaultNote := defaultChapterOffset(job)
	v := &ChapterImportView{
		Current:              jobChapters(ctx, s.deps, s.cache, job),
		DefaultOffsetSeconds: defaultOffset,
		DefaultOffsetText:    defaultText,
		DefaultOffsetNote:    defaultNote,
		DurationSeconds:      float64(t.durationMs) / 1000,
		Split:                t.split,
		Chapters:             []Chapter{},
	}
	if v.Current == nil {
		v.Current = []Chapter{}
	}
	if _, imported, err := utils.ReadImportedChaptersSidecar(t.sidecar); err == nil && imported {
		v.HasOriginal = true
	}
	return v
}

func (s *ChapterService) load(jobID string) (*database.Job, error) {
	job, err := s.deps.DB.GetJob(jobID)
	if err != nil {
		return nil, refuseChapters(http.StatusInternalServerError, "Could not read the job")
	}
	if job == nil {
		return nil, refuseChapters(http.StatusNotFound, "Job not found")
	}
	return job, nil
}

// fit parses text and fits it to t with the offset; the error is the
// parser's, or the offset's, refusal.
func fitImport(text, offset string, t *chapterTarget) ([]utils.Chapter, int64, error) {
	offsetMs, err := utils.ParseChapterOffset(offset)
	if err != nil {
		return nil, 0, err
	}
	parsed, err := utils.ParseChapterComment(text)
	if err != nil {
		return nil, offsetMs, err
	}
	list, err := utils.FitImportedChapters(parsed, offsetMs, t.durationMs)
	return list, offsetMs, err
}

// Preview parses text against job's archive without saving anything (owner
// decision CI-4: the dialog's live preview, and the TUI's): the list as it
// would be saved, offset applied, beside the job's current chapters. A paste
// that yields no chapter answers an empty list with the reason in Error, so
// the dialog shows it in place of the table.
func (s *ChapterService) Preview(ctx context.Context, jobID, text, offset string) (*ChapterImportView, error) {
	job, err := s.load(jobID)
	if err != nil {
		return nil, err
	}
	t, err := s.chapterTargetFor(ctx, job)
	if err != nil {
		return nil, err
	}
	v := s.view(ctx, job, t)
	list, offsetMs, err := fitImport(text, offset, t)
	v.OffsetSeconds = float64(offsetMs) / 1000
	if err != nil {
		var reason *utils.ChapterImportError
		if !errors.As(err, &reason) {
			return nil, err
		}
		v.Error = reason.Reason
		return v, nil
	}
	v.Chapters = toChapters(list)
	return v, nil
}

// Save imports text as job's chapters (owner decisions CI-1, CI-3): the
// fitted list replaces the job's chapters. A paste the parser or the offset
// refuses is a 400 with its reason.
func (s *ChapterService) Save(ctx context.Context, jobID, text, offset string) (*ChapterImportView, error) {
	job, err := s.load(jobID)
	if err != nil {
		return nil, err
	}
	t, err := s.chapterTargetFor(ctx, job)
	if err != nil {
		return nil, err
	}
	list, offsetMs, err := fitImport(text, offset, t)
	if err != nil {
		var reason *utils.ChapterImportError
		if errors.As(err, &reason) {
			return nil, refuseChapters(http.StatusBadRequest, "%s", reason.Reason)
		}
		return nil, err
	}
	v, err := s.apply(ctx, jobID, list)
	if v != nil {
		v.OffsetSeconds = float64(offsetMs) / 1000
	}
	return v, err
}

// Restore brings back the chapters job had before its first import (owner
// decision CI-3), by the same save path — the sidecar, and the re-mux of a
// single-file archive — with the original kept, so a later import still
// restores to it.
func (s *ChapterService) Restore(ctx context.Context, jobID string) (*ChapterImportView, error) {
	job, err := s.load(jobID)
	if err != nil {
		return nil, err
	}
	t, err := s.chapterTargetFor(ctx, job)
	if err != nil {
		return nil, err
	}
	imp, imported, err := utils.ReadImportedChaptersSidecar(t.sidecar)
	if err != nil {
		return nil, refuseChapters(http.StatusInternalServerError, "The saved chapter list cannot be read")
	}
	if !imported {
		return nil, refuseChapters(http.StatusConflict, "There are no original chapters to restore")
	}
	list := make([]utils.Chapter, 0, len(imp.Original))
	for _, c := range imp.Original {
		list = append(list, utils.Chapter{StartMs: int64(math.Round(c.Start * 1000)), EndMs: int64(math.Round(c.End * 1000)), Title: c.Title})
	}
	return s.apply(ctx, jobID, list)
}

// apply saves list as the job's chapters under the import's claim: the
// sidecar first, with the original — the chapters the player showed before
// the first import, embedded or a finalize's sidecar, possibly none — kept
// from the first import on; then, for a single-file archive, the verified
// re-mux. A re-mux that fails leaves the archive as it was and the sidecar
// saved, and says why in EmbedError; the player reads the sidecar first, so
// it shows the list either way.
func (s *ChapterService) apply(ctx context.Context, jobID string, list []utils.Chapter) (*ChapterImportView, error) {
	if s.deps.Importer == nil {
		return nil, refuseChapters(http.StatusServiceUnavailable, "Chapter import is not available")
	}
	release, err := s.deps.Importer.ClaimChapterImport(jobID)
	if err != nil {
		if errors.Is(err, worker.ErrStagingBusy) || errors.Is(err, worker.ErrArchiveJobActive) {
			return nil, refuseChapters(http.StatusConflict, "%s", err.Error())
		}
		return nil, err
	}
	defer release()
	// Read again under the claim: the status and the archive the import
	// writes are the ones no Mux action can now change, and the status read
	// here is the one the row's size write is conditioned on.
	job, err := s.load(jobID)
	if err != nil {
		return nil, err
	}
	t, err := s.chapterTargetFor(ctx, job)
	if err != nil {
		return nil, err
	}

	var original []utils.SidecarChapter
	if imp, imported, rerr := utils.ReadImportedChaptersSidecar(t.sidecar); rerr == nil && imported {
		original = imp.Original
	} else {
		// The first import: what the archive carries now becomes the
		// original, read strictly — a probe that failed (ffprobe's exit, its
		// timeout) must refuse the save, not record "none" and then re-mux
		// the archive's own chapters away for good. Not under the request's
		// context: the re-mux below runs on the mux root and outlives the
		// request, so a closed tab must not be what cut this read short and
		// turned the archive's chapters into an empty original.
		current, cerr := jobChaptersStrict(context.WithoutCancel(ctx), s.deps, s.cache, job)
		if cerr != nil {
			s.deps.Logger.Warn("Chapter import: could not read the archive's chapters to keep as the original; nothing was changed",
				"job", jobID, "error", cerr)
			return nil, refuseChapters(http.StatusServiceUnavailable, "The archive's current chapters could not be read, so they could not be kept to restore; nothing was changed")
		}
		for _, c := range current {
			original = append(original, utils.SidecarChapter{Start: c.Start, End: c.End, Title: c.Title})
		}
	}
	if err := utils.WriteImportedChaptersSidecar(t.sidecar, list, original); err != nil {
		s.deps.Logger.Warn("Chapter import: could not write the chapter list", "job", jobID, "file", filepath.Base(t.sidecar), "error", err)
		return nil, refuseChapters(http.StatusInternalServerError, "Could not save the chapter list")
	}
	s.deps.Logger.Info("Chapter import: saved the chapter list", "job", jobID, "chapters", len(list), "file", filepath.Base(t.sidecar))

	v := s.view(ctx, job, t)
	v.Chapters = toChapters(list)
	if t.split {
		return v, nil
	}
	if _, rerr := s.deps.Importer.RemuxArchiveChapters(jobID, job.Status, t.archive, list); rerr != nil {
		s.deps.Logger.Warn("Chapter import: the archive was not re-muxed with the chapters; it is left as it was, and the list is saved beside it",
			"job", jobID, "error", rerr)
		v.EmbedError = rerr.Error()
		return v, nil
	}
	v.Embedded = true
	return v, nil
}

// chapterImportRoutes registers the import's three routes beside GET
// /api/jobs/{id}/chapters (owner decision CI-4). Mutating, so the server's
// global CSRF and auth middleware cover them, as every other POST.
//
//   - POST /api/jobs/{id}/chapters/preview {text, offsetSeconds}: parse only;
//     nothing is written.
//   - POST /api/jobs/{id}/chapters {text, offsetSeconds}: save.
//   - POST /api/jobs/{id}/chapters/restore: bring back the original.
func chapterImportRoutes(r chi.Router, svc *ChapterService) {
	answer := func(rw http.ResponseWriter, v *ChapterImportView, err error) {
		if err != nil {
			var refusal *ChapterImportRefusal
			if errors.As(err, &refusal) {
				jsonError(rw, refusal.Reason, refusal.Status)
				return
			}
			jsonError(rw, "chapter import failed", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, v)
	}
	decode := func(rw http.ResponseWriter, req *http.Request) (chapterImportBody, bool) {
		var body chapterImportBody
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return body, false
		}
		return body, true
	}
	r.Post("/api/jobs/{id}/chapters/preview", func(rw http.ResponseWriter, req *http.Request) {
		body, ok := decode(rw, req)
		if !ok {
			return
		}
		v, err := svc.Preview(req.Context(), chi.URLParam(req, "id"), body.Text, body.offsetText())
		answer(rw, v, err)
	})
	r.Post("/api/jobs/{id}/chapters", func(rw http.ResponseWriter, req *http.Request) {
		body, ok := decode(rw, req)
		if !ok {
			return
		}
		v, err := svc.Save(req.Context(), chi.URLParam(req, "id"), body.Text, body.offsetText())
		answer(rw, v, err)
	})
	r.Post("/api/jobs/{id}/chapters/restore", func(rw http.ResponseWriter, req *http.Request) {
		v, err := svc.Restore(req.Context(), chi.URLParam(req, "id"))
		answer(rw, v, err)
	})
}

// splitSidecarPath is a split archive's chapter list path beside its parts
// (worker.ArchiveSidecarPath of the first part), resolved through the first
// part's file under the containment guard the part routes apply; false when
// that part is missing or outside the output directory.
func splitSidecarPath(job *database.Job, segments []database.Segment, deps *ChapterRouteDeps) (string, bool) {
	if len(segments) == 0 || segments[0].FilePath == "" {
		return "", false
	}
	abs, err := filepath.Abs(segments[0].FilePath)
	if err != nil {
		return "", false
	}
	part, ok := validatePathTraversal(abs, resolveOutputDir(job, deps.Store))
	if !ok {
		return "", false
	}
	return worker.ArchiveSidecarPath(part, utils.ChaptersSidecarSuffix), true
}
