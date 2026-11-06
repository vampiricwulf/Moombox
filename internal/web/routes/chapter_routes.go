package routes

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/childproc"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// chapterProbeTimeout bounds one ffprobe -show_chapters run. The chapter list
// sits in the container header (an MP4's moov, which the copy mux writes up
// front), so a healthy probe answers in milliseconds; the bound is for a file
// on a stalled network share, which must cost the player a missing chapter
// list and never a hung request.
const chapterProbeTimeout = 10 * time.Second

// chapterCacheSize bounds the per-file chapter cache. One entry is a few
// hundred bytes (a long Twitch VOD has some dozens of chapters), and a player
// session opens recordings one at a time, so this holds every file a
// dashboard plausibly reopens without letting a scripted walk over the whole
// archive grow the process without bound.
const chapterCacheSize = 256

// Chapter is one entry of GET /api/jobs/{id}/chapters: a span on the player's
// global timeline, in seconds, and its title.
type Chapter struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

// ChapterRouteDeps carries what GET /api/jobs/{id}/chapters needs: the jobs
// and their parts, the config for the output directory and the FFmpeg path
// (read per request, so a hot-reloaded FFmpeg path is the one probed, as the
// import's duration probe does), and the logger a failed probe is reported on.
type ChapterRouteDeps struct {
	DB     *database.Database
	Store  *config.Store
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// ChapterRoutes registers GET /api/jobs/{id}/chapters (owner decision CH-1,
// the follow-up to W22 / D23). W22 embeds chapters in a single-file archive at
// mux time (Twitch moments or categories, YouTube description chapters, as an
// FFMETADATA1 input), but a browser's <video> shows no MP4 chapter atoms, so
// the Web UI player asks for them here. Nothing is stored: the list is read
// from the archive file(s) with ffprobe on request and cached per file
// version (chapterCache), so a re-mux that rewrites the file is seen on the
// next open.
//
// A multi-part job is probed part by part, and each part's chapters are
// shifted by that part's start offset on the player's global timeline — the
// sum of the earlier parts' durationSeconds in segment_index order, which is
// how SegmentPlayer.init (web/public/modules/segments.js) lays the parts out.
// That is how a W14 gap-filled Twitch job shows the chapters its original
// file carries, at the right place among the recovered spans beside it.
//
// The answer is always 200 for a job that exists: an archive with no
// chapters, a missing or unreadable file and a failed ffprobe all answer an
// empty list, because the player treats chapters as decoration and must never
// error over them. The failure is logged (a probe failure once per file
// version, at Warn; the rest at Debug).
func ChapterRoutes(r chi.Router, deps *ChapterRouteDeps) {
	chapterRoutes(r, deps, newChapterCache(chapterCacheSize))
}

// chapterRoutes is ChapterRoutes over a given cache, so a test can count the
// probes behind it.
func chapterRoutes(r chi.Router, deps *ChapterRouteDeps, cache *chapterCache) {
	r.Get("/api/jobs/{id}/chapters", func(rw http.ResponseWriter, req *http.Request) {
		job, ok := loadJob(rw, deps.DB, chi.URLParam(req, "id"))
		if !ok {
			return
		}
		chapters := jobChapters(req.Context(), deps, cache, job)
		if chapters == nil {
			chapters = []Chapter{}
		}
		// Revalidate, never immutable: a re-mux or a part merge rewrites the
		// file(s) behind the same URL — the same reasoning as /video (D-6).
		rw.Header().Set("Cache-Control", "private, no-cache")
		jsonResponse(rw, map[string]any{"chapters": chapters})
	})
}

// jobChapters is the job's chapter list on the player's global timeline, in
// time order; nil when there is none or it cannot be read. Sorted here, for
// both shapes: ffprobe lists a file's chapters in the container's order,
// which nothing obliges to be chronological (an FFMETADATA1 input is written
// as given), and the player's strip, list and current-chapter highlight all
// read the list as sorted.
func jobChapters(ctx context.Context, deps *ChapterRouteDeps, cache *chapterCache, job *database.Job) []Chapter {
	all := jobChaptersUnsorted(ctx, deps, cache, job)
	slices.SortStableFunc(all, func(a, b Chapter) int {
		switch {
		case a.Start < b.Start:
			return -1
		case a.Start > b.Start:
			return 1
		}
		return 0
	})
	return all
}

func jobChaptersUnsorted(ctx context.Context, deps *ChapterRouteDeps, cache *chapterCache, job *database.Job) []Chapter {
	var ffmpegPath string
	deps.Store.Read(func(c *config.MoomboxConfig) { ffmpegPath = c.Paths.FfmpegPath })
	ffprobePath := engine.NewMuxer(ffmpegPath, deps.Logger).FFprobePath()

	segments, err := deps.DB.GetSegments(job.ID)
	if err != nil {
		deps.Logger.Debug("Chapters: could not read the job's parts", "job", job.ID, "error", err)
		return nil
	}
	// The same test the player makes (onPlayerJobSelect): any part row at
	// all, and the job plays as parts, so its chapters come from the parts.
	if len(segments) == 0 {
		path, msg, code := jobVideoPath(job, deps.Store)
		if code != 0 {
			deps.Logger.Debug("Chapters: no archive file to read", "job", job.ID, "reason", msg)
			return nil
		}
		return cache.chapters(ctx, ffprobePath, path, job.ID, deps.Logger)
	}

	outputDir := resolveOutputDir(job, deps.Store)
	var all []Chapter
	offset := 0.0
	for _, seg := range segments {
		start := offset
		// Accumulated before any skip, exactly as SegmentPlayer.init does
		// (`cumulative += seg.durationSeconds || 0`): a part whose file is
		// gone still holds its place on the player's timeline.
		offset += seg.DurationSeconds
		if seg.FilePath == "" {
			continue
		}
		// The containment guard GET /api/jobs/{id}/segments/{index}/video
		// applies: this reads the file the player would play, and no other.
		abs, err := filepath.Abs(seg.FilePath)
		if err != nil {
			continue
		}
		path, ok := validatePathTraversal(abs, outputDir)
		if !ok {
			deps.Logger.Debug("Chapters: part file missing or outside the output directory", "job", job.ID, "segment", seg.SegmentIndex)
			continue
		}
		for _, c := range cache.chapters(ctx, ffprobePath, path, job.ID, deps.Logger) {
			c.Start += start
			c.End += start
			// A chapter cannot run past its own part: the next part's span
			// begins where this one's duration ends on the global timeline,
			// and an end read from the file's own (slightly longer, or
			// re-muxed) timeline would overlap the next part's first chapter.
			// One that starts at or past the part's end is dropped, not kept
			// as a zero-length span: it would sit inside the next part, sort
			// after that part's first chapter, and the player's highlight
			// (the last chapter started) would name it through the next
			// part's opening. A W14 gap-fill's original-file row is the
			// VOD's whole-second length, up to a second short of the file,
			// so a chapter in that last second is the realistic case. A
			// part with no duration yet (a live part not yet measured) has
			// no end to clamp to and keeps its chapters as read.
			if seg.DurationSeconds > 0 {
				partEnd := start + seg.DurationSeconds
				if c.Start >= partEnd {
					continue
				}
				c.End = min(c.End, partEnd)
			}
			c.End = max(c.End, c.Start)
			all = append(all, c)
		}
	}
	return all
}

// chapterCache holds each archive file's chapter list, keyed by its path and
// checked against the file's size and modification time on every read, so a
// re-mux, a part merge or a gap-fill re-finalize that rewrites the file
// invalidates its entry by itself. Bounded LRU: the least recently read entry
// is dropped once more than max are held.
type chapterCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *chapterCacheEntry, most recently read at the front
	byKey map[string]*list.Element
	// probe reads one file's chapters; ffprobeChapters in production, a field
	// so a test can count the probes behind the cache.
	probe func(ctx context.Context, ffprobePath, file string) ([]Chapter, error)
}

type chapterCacheEntry struct {
	path     string
	size     int64
	mtime    time.Time
	chapters []Chapter
}

func newChapterCache(max int) *chapterCache {
	return &chapterCache{
		max:   max,
		order: list.New(),
		byKey: make(map[string]*list.Element),
		probe: ffprobeChapters,
	}
}

// chapters returns the file's chapters (file-relative seconds), probing it
// only when no entry for this version of the file is held.
//
// What is cached is the probe's verdict on this version of the file: its
// chapters, none, or ffprobe's refusal of it (an exit status or unparsable
// output) — the file will not read differently until it changes, and its key
// changes with it, so the refusal is logged once at Warn and not re-probed on
// every open. What is NOT cached is a failure that says nothing about the
// file: the request going away under the probe (CommandContext kills the
// child, so that arrives as an *exec.ExitError too — hence the ctx check
// ahead of the error's shape), ffprobe not starting at all (not installed
// yet, a path being fixed in Settings), or the probe's own timeout — the
// bound exists for a stalled network share, and a share that recovers keeps
// the file's size and mtime, so a cached timeout would leave the file
// chapterless until a restart. Those are retried on the next open; the
// player's fetch is never awaited, so a retried slow probe costs it nothing.
func (c *chapterCache) chapters(ctx context.Context, ffprobePath, path, jobID string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) []Chapter {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		logger.Debug("Chapters: archive file not readable", "job", jobID, "file", filepath.Base(path))
		return nil
	}
	if got, ok := c.get(path, info); ok {
		return got
	}
	got, err := c.probe(ctx, ffprobePath, path)
	if err != nil {
		var exitErr *exec.ExitError
		cacheable := ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && (errors.As(err, &exitErr) || errors.Is(err, errUnreadableChapters))
		if !cacheable {
			logger.Debug("Chapters: ffprobe did not answer; retried on the next open", "job", jobID, "file", filepath.Base(path), "error", err)
			return nil
		}
		logger.Warn("Chapters: ffprobe could not read the archive's chapters; the player shows none", "job", jobID, "file", filepath.Base(path), "error", err)
		got = nil
	}
	c.put(path, info, got)
	return got
}

func (c *chapterCache) get(path string, info os.FileInfo) ([]Chapter, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[path]
	if !ok {
		return nil, false
	}
	e := el.Value.(*chapterCacheEntry)
	if e.size != info.Size() || !e.mtime.Equal(info.ModTime()) {
		return nil, false
	}
	c.order.MoveToFront(el)
	return slices.Clone(e.chapters), true
}

func (c *chapterCache) put(path string, info os.FileInfo, chapters []Chapter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &chapterCacheEntry{path: path, size: info.Size(), mtime: info.ModTime(), chapters: slices.Clone(chapters)}
	if el, ok := c.byKey[path]; ok {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.byKey[path] = c.order.PushFront(e)
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(*chapterCacheEntry).path)
	}
}

// errUnreadableChapters is ffprobe answering with output that is not its JSON
// chapter list.
var errUnreadableChapters = errors.New("unreadable ffprobe chapter output")

// ffprobeChapters reads one file's chapter list with ffprobe
// (-show_chapters -print_format json), in the file's own seconds, in the
// order ffprobe lists them. The file goes through engine.FFmpegPathArg, the
// seam every ffprobe argv takes (TestEveryFFprobeCallAppliesThePathSeam).
func ffprobeChapters(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
	ctx, cancel := context.WithTimeout(ctx, chapterProbeTimeout)
	defer cancel()
	out, err := childproc.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json", "-show_chapters", engine.FFmpegPathArg(file)).Output()
	if err != nil {
		// Killed by the bound: say so, so the cache can tell its own
		// timeout (retried) from ffprobe's exit-status refusal (cached).
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
		return nil, err
	}
	return parseFFprobeChapters(out)
}

// parseFFprobeChapters reads ffprobe's -show_chapters JSON: each chapter's
// start and end in the file's seconds and its title tag, in the order listed.
func parseFFprobeChapters(out []byte) ([]Chapter, error) {
	var probe struct {
		Chapters []struct {
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, errors.Join(errUnreadableChapters, err)
	}
	chapters := make([]Chapter, 0, len(probe.Chapters))
	for _, ch := range probe.Chapters {
		start, err1 := strconv.ParseFloat(ch.StartTime, 64)
		end, err2 := strconv.ParseFloat(ch.EndTime, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		title := ch.Tags["title"]
		if title == "" {
			// Matroska and some muxers spell the tag in capitals.
			title = ch.Tags["TITLE"]
		}
		chapters = append(chapters, Chapter{Start: start, End: max(end, start), Title: title})
	}
	return chapters, nil
}
