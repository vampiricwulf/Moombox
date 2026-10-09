package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const defaultTrimCRF = 18

// TrimRefusedError is a trim request refused for a reason the requester can
// act on: the job's state, the time range, a duplicate, a trim already
// running. Reason is written for the user and is shown as-is by the API;
// every other error CreateTrim or StartTrim returns is an internal failure
// whose detail stays in the log (and the trim_error embed).
type TrimRefusedError struct {
	Reason string
	// Conflict marks a request that clashes with current state (a duplicate,
	// a trim already in progress) rather than one that is malformed.
	Conflict bool
}

func (e *TrimRefusedError) Error() string { return e.Reason }

func refuseTrim(format string, args ...any) error {
	return &TrimRefusedError{Reason: fmt.Sprintf(format, args...)}
}

func conflictTrim(reason string) error {
	return &TrimRefusedError{Reason: reason, Conflict: true}
}

// ErrTrimNotFound is returned (possibly wrapped) by DeleteTrim when the trim,
// or the job it belongs to, does not exist.
var ErrTrimNotFound = errors.New("trim not found")

// TrimTask is a trim the service is encoding: the id its record will carry,
// the job and range it is for, and how far FFmpeg has got (0-100, the
// percentage runFFmpegWithProgress reads off FFmpeg's time= lines).
type TrimTask struct {
	ID        string  `json:"id"`
	JobID     string  `json:"jobId"`
	StartTime float64 `json:"startTime"`
	EndTime   float64 `json:"endTime"`
	Progress  float64 `json:"progress"`
}

// Trim task states, as TrimEvent.State spells them.
const (
	TrimStateRunning  = "running"
	TrimStateFinished = "finished"
	TrimStateFailed   = "failed"
)

// TrimEvent is one change to a trim the service runs: "running" as it
// starts, then exactly one of "finished" (with the record it stored) or
// "failed" (with a reason written for the user). The service hands each to
// the hook SetOnEvent installs; the dashboard receives them as the
// trim_status WebSocket frame.
type TrimEvent struct {
	TrimTask
	State string               `json:"state"`
	Trim  *database.TrimRecord `json:"trim,omitempty"`
	Error string               `json:"error,omitempty"`
}

// trimFailedReason is what a trim that broke tells the user; its detail
// (paths, FFmpeg's stderr) stays in the log and the trim_error embed. The
// TUI's trimFailureText says the same.
const trimFailedReason = "Could not create the trim; the log has the reason"

// trimInterruptedReason is a trim cut short by its caller or by Stop: the
// operator stopped Moombox, nothing failed, so no trim_error is sent.
const trimInterruptedReason = "Moombox stopped before the trim finished"

// trimProgressInterval is the least time between two "running" events of
// one trim. FFmpeg reports progress about twice a second and the dashboard's
// bar needs no more; RunningTrims always has the latest figure.
const trimProgressInterval = 250 * time.Millisecond

// trimStopWait bounds how long Stop waits for the trims it cancelled to
// remove their partial files. A cancelled FFmpeg is killed, so the wait is
// normally milliseconds; the bound only keeps a wedged one from holding the
// shutdown, and stays inside the force-exit margin the shutdown leaves
// beyond the worker's own budget (cmd/moombox forceExitAfter).
const trimStopWait = 2 * time.Second

// TrimService handles creating and deleting trim records.
type TrimService struct {
	muxer    *engine.Muxer
	muxerMu  sync.RWMutex // guards muxer across SetFfmpegPath hot-reload vs. in-flight trims
	db       *database.Database
	notifier notifications.Sender
	// ctx bounds every trim the service runs, whoever asked for it: a
	// dashboard trim runs detached from its request (StartTrim), and the
	// TUI's in-process one under context.Background(), so the service's
	// own lifetime is what reaches their FFmpeg. Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// wg counts the trims between prepare and the end of run, so Stop can
	// wait for each to remove its partial file.
	wg       sync.WaitGroup
	activeMu sync.Mutex
	// activeOps holds the trim in flight per job — one at a time, so a
	// second request for a job (the same range or another) is refused while
	// the first encodes — with the progress RunningTrims reports. A
	// reservation whose request is still being checked has no ID yet.
	activeOps map[string]*TrimTask
	stopped   bool // set by Stop, under activeMu: no trim starts after it
	onEvent   func(TrimEvent)
	// progressInterval is trimProgressInterval; a field so a test can take
	// every report, or none.
	progressInterval time.Duration

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewTrimService creates a new trim service.
func NewTrimService(db *database.Database, ffmpegPath string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *TrimService {
	ctx, cancel := context.WithCancel(context.Background())
	return &TrimService{
		muxer:     engine.NewMuxer(ffmpegPath, logger),
		db:        db,
		ctx:       ctx,
		cancel:    cancel,
		activeOps: make(map[string]*TrimTask),
		logger:    logger,

		progressInterval: trimProgressInterval,
	}
}

// SetNotifier sets the notification manager for trim notifications.
func (ts *TrimService) SetNotifier(nm notifications.Sender) {
	ts.notifier = nm
}

// SetOnEvent installs the hook every TrimEvent is handed to (the WebSocket
// broadcast, cmd/moombox). It runs on the trim's own goroutine, outside the
// service's locks.
func (ts *TrimService) SetOnEvent(fn func(TrimEvent)) {
	ts.activeMu.Lock()
	ts.onEvent = fn
	ts.activeMu.Unlock()
}

// SetFfmpegPath rebuilds the muxer for a new ffmpeg path (config hot-reload).
// In-flight trims keep the muxer they captured; new trims see the new path.
func (ts *TrimService) SetFfmpegPath(path string) {
	m := engine.NewMuxer(path, ts.logger)
	ts.muxerMu.Lock()
	ts.muxer = m
	ts.muxerMu.Unlock()
}

// mux returns the current muxer under the read lock.
func (ts *TrimService) mux() *engine.Muxer {
	ts.muxerMu.RLock()
	defer ts.muxerMu.RUnlock()
	return ts.muxer
}

// FFprobePath reports the ffprobe path of the current muxer (observability
// for the hot-reload path; trims themselves go through mux()).
func (ts *TrimService) FFprobePath() string { return ts.mux().FFprobePath() }

// Stop cancels every trim the service is running — their FFmpeg is killed
// and each removes its partial file — waits (up to trimStopWait) for them to
// finish doing so, and refuses any trim asked for after it.
func (ts *TrimService) Stop() {
	ts.activeMu.Lock()
	ts.stopped = true
	ts.activeMu.Unlock()
	ts.cancel()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ts.logger.Error("panic waiting for trims to stop", "panic", fmt.Sprint(r))
			}
		}()
		ts.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(trimStopWait):
		ts.logger.Warn("trims still running after stop", "waited", trimStopWait)
	}
}

// CreateTrim creates a trimmed version of a finished download and returns
// once it is done. progressFn is called with 0-100 as FFmpeg encoding
// progresses; pass nil when progress reporting is not needed (audit
// reports/worker.md F58 — previously split into CreateTrim +
// CreateTrimWithProgress; the two thin wrappers added no value over a single
// method with an optional callback). The trim runs under ctx and the
// service's own lifetime both.
func (ts *TrimService) CreateTrim(ctx context.Context, job *database.Job, startTime, endTime float64, progressFn func(float64)) (*database.TrimRecord, error) {
	plan, err := ts.prepare(job, startTime, endTime)
	if err != nil {
		return nil, err
	}
	return ts.run(ctx, plan, progressFn)
}

// StartTrim checks a trim request and, when it is one to run, starts the
// encode as a task of the service's own and returns it at once. A refusal
// (*TrimRefusedError) or a failure to set the trim up comes back here; the
// encode's outcome reaches the TrimEvent hook, the log, and — for a trim
// that broke — the trim_error notification. The dashboard's trim route runs
// trims this way: bounded by its request, a trim died with the page that
// asked for it, and a reload or a closed tab left its partial file behind.
func (ts *TrimService) StartTrim(job *database.Job, startTime, endTime float64) (TrimTask, error) {
	plan, err := ts.prepare(job, startTime, endTime)
	if err != nil {
		return TrimTask{}, err
	}
	task := ts.snapshot(plan)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ts.logger.Error("panic in detached trim", "jobID", job.ID, "panic", fmt.Sprint(r))
			}
		}()
		if _, err := ts.run(ts.ctx, plan, nil); err != nil {
			if ts.ctx.Err() != nil {
				// Stop cut it short: Moombox is stopping, nothing failed.
				ts.logger.Info("trim interrupted by shutdown", "jobID", job.ID, "trimID", task.ID)
				return
			}
			ts.logger.Error("Failed to create trim", "jobID", job.ID, "trimID", task.ID, "error", err.Error())
		}
	}()
	return task, nil
}

// segTrimInfo is one segment a quality-split trim reads: the part of it, in
// the segment's own time, that falls inside the trim.
type segTrimInfo struct {
	Segment    *database.Segment
	LocalStart float64
	LocalEnd   float64
}

// trimPlan is a trim request that passed every check, with its job's slot
// in activeOps held: what the encode reads and where it writes.
type trimPlan struct {
	task     *TrimTask
	job      *database.Job
	start    float64
	end      float64
	duration float64
	trimPath string // the finished file
	relPath  string // what the record stores
	// involved is set for a quality-split job: the segments the range
	// covers, in order.
	involved []segTrimInfo
	// lastEmit is when run last sent this trim's "running" event. Only the
	// trim's own goroutine touches it: FFmpeg's progress is reported from
	// the stderr loop run's encode runs in.
	lastEmit time.Time
}

// prepare holds the job's trim slot and checks the request, returning the
// plan run executes. A refusal, or a failure setting the trim up, gives the
// slot back; a failure that is not a refusal also sends trim_error, as a
// failed encode does.
func (ts *TrimService) prepare(job *database.Job, startTime, endTime float64) (*trimPlan, error) {
	// Prevent concurrent trim operations on the same job (matching TS activeTrimOps)
	ts.activeMu.Lock()
	if ts.stopped {
		ts.activeMu.Unlock()
		return nil, conflictTrim("Moombox is stopping")
	}
	if ts.activeOps[job.ID] != nil {
		ts.activeMu.Unlock()
		return nil, conflictTrim("another trim operation is already in progress for this job")
	}
	task := &TrimTask{}
	ts.activeOps[job.ID] = task
	ts.wg.Add(1)
	ts.activeMu.Unlock()

	var plan *trimPlan
	var err error
	if len(job.Segments) > 0 {
		plan, err = ts.planSegments(job, startTime, endTime)
	} else {
		plan, err = ts.planSingle(job, startTime, endTime)
	}
	if err != nil {
		ts.release(job.ID)
		if _, refused := errors.AsType[*TrimRefusedError](err); !refused {
			ts.sendTrimFailed(job, err)
		}
		return nil, err
	}

	ts.activeMu.Lock()
	task.ID = fmt.Sprintf("trim_%s_%d", job.ID, time.Now().UnixMilli())
	task.JobID = job.ID
	task.StartTime = startTime
	task.EndTime = endTime
	ts.activeMu.Unlock()
	plan.task = task
	return plan, nil
}

// release gives back the job's trim slot and the Stop wait it counts in.
func (ts *TrimService) release(jobID string) {
	ts.activeMu.Lock()
	delete(ts.activeOps, jobID)
	ts.activeMu.Unlock()
	ts.wg.Done()
}

// snapshot copies the plan's task under the lock its progress is written
// under.
func (ts *TrimService) snapshot(plan *trimPlan) TrimTask {
	ts.activeMu.Lock()
	defer ts.activeMu.Unlock()
	return *plan.task
}

// emit hands ev to the hook SetOnEvent installed, if any.
func (ts *TrimService) emit(ev TrimEvent) {
	ts.activeMu.Lock()
	fn := ts.onEvent
	ts.activeMu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// planSingle checks a trim of a single-file recording.
func (ts *TrimService) planSingle(job *database.Job, startTime, endTime float64) (*trimPlan, error) {
	// Validate
	if job.Status != database.StatusFinished {
		return nil, refuseTrim("job must be finished to trim")
	}
	if job.OutputFile == "" {
		return nil, refuseTrim("no output file for job")
	}
	if _, err := os.Stat(job.OutputFile); err != nil {
		ts.logger.Warn("trim: output file unreadable", "jobID", job.ID, "err", err)
		return nil, refuseTrim("output file not found")
	}
	if startTime < 0 {
		return nil, refuseTrim("start time cannot be negative")
	}
	if startTime >= endTime {
		return nil, refuseTrim("start time must be before end time")
	}
	// Validate end time doesn't exceed video duration. length_seconds is the
	// probed duration with its fraction dropped (muxAndFinalize stores
	// int(ffprobe's duration)), so the file runs on for up to a second past
	// it — and the dashboard's end marker is the player's own fractional
	// duration: "to the end" of a 3600.48 s archive posts 3600.48 against a
	// row reading 3600, and was refused as past the end. The end is past the
	// file only from the next whole second on; FFmpeg stops at the file's end
	// whatever lies between.
	if job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		maxDuration := float64(*job.LengthSeconds)
		if endTime >= maxDuration+1 {
			return nil, refuseTrim("end time (%s) exceeds video duration (%s)", trimSeconds(endTime), trimSeconds(maxDuration))
		}
	}
	duration := endTime - startTime
	if duration < 1 {
		return nil, refuseTrim("trim duration must be at least 1 second")
	}

	// Generate output filename
	trimDir := filepath.Join(filepath.Dir(job.OutputFile), "trim")
	if err := os.MkdirAll(trimDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trim dir: %w", err)
	}

	// Check for duplicates (exact same range) BEFORE building the filename —
	// the name also needs the existing list for collision disambiguation.
	// A read failure is an error, not an empty list: with the list empty
	// the duplicate check passes and uniqueTrimBasename picks the base name,
	// so ffmpeg (-y) overwrites the trim already on disk and a second
	// record is stored for the same path.
	existing, err := ts.db.GetTrimsForJob(job.ID)
	if err != nil {
		return nil, fmt.Errorf("list existing trims: %w", err)
	}
	for _, t := range existing {
		if t.StartTime == startTime && t.EndTime == endTime {
			return nil, conflictTrim("trim already exists")
		}
	}

	trimBasename := uniqueTrimBasename(existing, job.VideoID, startTime, endTime)
	// Store relative path including parent directory from source job's filename
	// (matches TS: path.join(path.dirname(sourceJob.filename), "trim", trimFilename))
	return &trimPlan{
		job:      job,
		start:    startTime,
		end:      endTime,
		duration: duration,
		trimPath: filepath.Join(trimDir, trimBasename),
		relPath:  filepath.Join(filepath.Dir(job.Filename), "trim", trimBasename),
	}, nil
}

// partialTrimPath is where a trim encodes before it is complete: beside the
// finished name in trim/, so the move into place is a rename on the same
// volume, and still .mp4, which is how FFmpeg picks the output format. A
// trim that stops early — FFmpeg failed, Moombox stopped, or (before trims
// ran detached) the page that asked was closed — left a truncated file
// under the finished name; now nothing carries that name until the encode
// is whole, and a partial file left by a crash says what it is.
func partialTrimPath(trimPath string) string {
	return strings.TrimSuffix(trimPath, filepath.Ext(trimPath)) + ".partial" + filepath.Ext(trimPath)
}

// run encodes a prepared trim into its partial file, moves it into place,
// stores its record and announces it; or, when anything fails, removes what
// it wrote, says so, and gives the slot back. Every outcome — a panic
// included — ends in exactly one "finished" or "failed" TrimEvent, sent
// after the slot is free.
func (ts *TrimService) run(ctx context.Context, plan *trimPlan, progressFn func(float64)) (rec *database.TrimRecord, err error) {
	job := plan.job
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(ts.ctx, cancel)()

	// No trim row names the file until the encode is done, which for a long
	// range is a long time to be an unreferenced file in trim/. The claim is
	// on the stem, so it covers the partial file too. Deferred ahead of the
	// outcome below, so it is released after it: once the partial file is gone.
	defer claimOutputStem(job.ID, strings.TrimSuffix(plan.trimPath, filepath.Ext(plan.trimPath)))()

	partial := partialTrimPath(plan.trimPath)
	placed := false // the file under the finished name is this trim's
	defer func() {
		if r := recover(); r != nil {
			ts.logger.Error("panic in trim", "jobID", job.ID, "panic", fmt.Sprint(r))
			rec, err = nil, fmt.Errorf("trim panicked: %v", r)
		}
		os.Remove(partial)
		if err != nil && placed {
			os.Remove(plan.trimPath)
		}
		ev := TrimEvent{TrimTask: ts.snapshot(plan)}
		switch {
		case err == nil:
			ev.State, ev.Trim = TrimStateFinished, rec
		case ctx.Err() != nil:
			ev.State, ev.Error = TrimStateFailed, trimInterruptedReason
		default:
			ev.State, ev.Error = TrimStateFailed, trimFailedReason
			ts.sendTrimFailed(job, err)
		}
		ts.release(job.ID)
		ts.emit(ev)
	}()

	plan.lastEmit = time.Now()
	ts.emit(TrimEvent{TrimTask: ts.snapshot(plan), State: TrimStateRunning})
	progress := func(pct float64) {
		if progressFn != nil {
			progressFn(pct)
		}
		ts.noteProgress(plan, pct)
	}

	if plan.involved == nil {
		ts.logger.Info("creating trim", "jobID", job.ID, "start", plan.start, "end", plan.end)
		err = ts.encodeSingle(ctx, plan, partial, progress)
	} else {
		ts.logger.Info("creating multi-segment trim", "jobID", job.ID,
			"start", plan.start, "end", plan.end, "segments", len(plan.involved))
		err = ts.encodeSegments(ctx, plan, partial, progress)
	}
	if err != nil {
		return nil, err
	}
	// utils.ReplaceFile, not os.Rename: the file was written a moment ago,
	// and on Windows a scanner or indexer still holding it turns the move
	// into a spurious failure (sweep-2 TOOL-2).
	if err := utils.ReplaceFile(partial, plan.trimPath); err != nil {
		return nil, fmt.Errorf("move trim into place: %w", err)
	}
	placed = true

	// Get file size
	info, _ := os.Stat(plan.trimPath)
	var fileSize *int64
	if info != nil {
		sz := info.Size()
		fileSize = &sz
	}

	// Create record
	record := &database.TrimRecord{
		ID:        plan.task.ID, // set in prepare, read-only since
		JobID:     job.ID,
		StartTime: plan.start,
		EndTime:   plan.end,
		Filename:  plan.relPath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Duration:  plan.duration,
		FileSize:  fileSize,
	}

	if err := ts.db.AddTrim(record); err != nil {
		return nil, fmt.Errorf("save trim record: %w", err)
	}

	// Send "Trim Created" notification
	if ts.notifier != nil {
		parts := 0
		if plan.involved != nil {
			parts = len(plan.involved)
		}
		ts.notifier.Send(notifications.TrimCreated(NotifyFacts(job), notifications.TrimFacts{
			TimeRange: fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(plan.start), FormatSecondsToTimestamp(plan.end)),
			Duration:  time.Duration(plan.duration) * time.Second,
			Size:      fileSize,
			Parts:     parts,
		}))
	}

	ts.logger.Info("trim created", "trimID", record.ID, "path", plan.trimPath)
	return record, nil
}

// noteProgress records a trim's FFmpeg percentage — what RunningTrims gives a
// page that connects mid-trim — and hands it on as a "running" event, at most
// one per progressInterval: the same figure the TUI's dialog reads off its
// own callback, for the dashboard's bar.
func (ts *TrimService) noteProgress(plan *trimPlan, pct float64) {
	ts.activeMu.Lock()
	plan.task.Progress = pct
	snap := *plan.task
	interval := ts.progressInterval
	ts.activeMu.Unlock()
	now := time.Now()
	if now.Sub(plan.lastEmit) < interval {
		return
	}
	plan.lastEmit = now
	ts.emit(TrimEvent{TrimTask: snap, State: TrimStateRunning})
}

// RunningTrims returns the trims the service is encoding, with their latest
// progress: what a dashboard is seeded with when it connects (initial_state),
// so a page reloaded mid-trim still shows the trim running. A reservation
// whose request is still being checked has no id yet and is not listed.
func (ts *TrimService) RunningTrims() []TrimTask {
	ts.activeMu.Lock()
	defer ts.activeMu.Unlock()
	out := make([]TrimTask, 0, len(ts.activeOps))
	for _, t := range ts.activeOps {
		if t.ID != "" {
			out = append(out, *t)
		}
	}
	slices.SortFunc(out, func(a, b TrimTask) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// encodeSingle encodes a single-file recording's trim into out.
func (ts *TrimService) encodeSingle(ctx context.Context, plan *trimPlan, out string, progressFn func(float64)) error {
	// Probe audio bitrate to match source quality (matches TS probeAudioBitrate)
	m := ts.mux()
	audioBitrate := probeAudioBitrate(ctx, m.FFprobePath(), plan.job.OutputFile)
	opts := &engine.TrimOptions{
		TrimStartOffset: plan.start,
		TrimDuration:    plan.duration,
		CRF:             defaultTrimCRF,
		AudioBitrate:    audioBitrate,
		UsePreciseTrim:  true,
		ProgressFn:      progressFn,
	}
	if err := m.Mux(ctx, plan.job.OutputFile, "", out, opts); err != nil {
		return fmt.Errorf("ffmpeg trim: %w", err)
	}
	return nil
}

// sendTrimFailed sends the "Trim Failed" embed for a trim that broke. Not for
// a refusal, which its caller answers to whoever asked, nor for one cut short
// by a stop, which is no failure; the post-download trim, which has nobody
// to answer to, sends those itself (DownloadOrchestrator.sendTrimFailed).
func (ts *TrimService) sendTrimFailed(job *database.Job, err error) {
	sendTrimFailed(ts.notifier, job, err)
}

// DeleteTrim deletes a trim record and its file.
func (ts *TrimService) DeleteTrim(jobID, trimID string) error {
	job, err := ts.db.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job not found: %s: %w", jobID, ErrTrimNotFound)
	}

	trims, err := ts.db.GetTrimsForJob(jobID)
	if err != nil {
		return fmt.Errorf("get trims: %w", err)
	}

	var target *database.TrimRecord
	for _, t := range trims {
		if t.ID == trimID {
			target = &t
			break
		}
	}
	if target == nil {
		return ErrTrimNotFound
	}

	// Delete DB record only — file stays on disk for orphaned files cleanup
	if err := ts.db.DeleteTrim(trimID); err != nil {
		return fmt.Errorf("delete trim record: %w", err)
	}

	// Send "Trim Deleted" notification (matches TS fields: Source Video, Time Range, Duration)
	if ts.notifier != nil {
		trimDuration := target.EndTime - target.StartTime
		timeRange := fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(target.StartTime), FormatSecondsToTimestamp(target.EndTime))
		durStr := formatDurationHuman(time.Duration(trimDuration) * time.Second)
		// The one row->facts mapper (notify_facts.go), as every other job send
		// in this package uses it. `trim_deleted` is never a lifecycle event —
		// it stays a separate post — but the footer's platform, the author
		// line and the dashboard deep link are the job's either way, and the
		// deep link needs JobID and Author together.
		// The title is job-supplied text: escaped here as every builder and
		// the worker's own hand-built sends escape it, so a "*" or "_" in a
		// stream title cannot italicise the rest of the embed.
		f := NotifyFacts(job)
		title := notifications.EscapeMarkdown(job.Title)
		ts.notifier.Send("Trim Deleted",
			fmt.Sprintf("Trim deleted for: %s", title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Source Video", Value: title, Inline: false},
				{Name: "Time Range", Value: timeRange, Inline: true},
				{Name: "Duration", Value: durStr, Inline: true},
			},
			notifications.SendOptions{
				URL:       f.URL,
				Thumbnail: f.ThumbnailURL,
				Event:     "trim_deleted",
				Author:    notifyAuthor(f),
				Platform:  f.Platform,
				JobID:     f.ID,
			},
		)
	}

	ts.logger.Info("trim deleted", "trimID", trimID)
	return nil
}

// planSegments checks a trim of a multi-segment quality-split recording. It
// maps the global start/end times to segment-local ones; the encode trims
// each involved segment and concatenates them (with quality normalization
// if needed).
func (ts *TrimService) planSegments(job *database.Job, startTime, endTime float64) (*trimPlan, error) {
	if job.Status != database.StatusFinished {
		return nil, refuseTrim("job must be finished to trim")
	}
	if startTime < 0 {
		return nil, refuseTrim("start time cannot be negative")
	}
	if startTime >= endTime {
		return nil, refuseTrim("start time must be before end time")
	}

	// Calculate cumulative time offsets for each segment
	type segmentRange struct {
		Segment     *database.Segment
		GlobalStart float64 // cumulative start in global timeline
		GlobalEnd   float64 // cumulative end in global timeline
	}

	var ranges []segmentRange
	var cumulative float64
	for i := range job.Segments {
		seg := &job.Segments[i]
		r := segmentRange{
			Segment:     seg,
			GlobalStart: cumulative,
			GlobalEnd:   cumulative + seg.DurationSeconds,
		}
		ranges = append(ranges, r)
		cumulative += seg.DurationSeconds
	}

	// The segments' durations are the probed ones, fraction and all, so the
	// bound is exact here; only the message rounds, and it rounds the way the
	// single-file one does.
	totalDuration := cumulative
	if endTime > totalDuration {
		return nil, refuseTrim("end time (%s) exceeds total duration (%s)", trimSeconds(endTime), trimSeconds(totalDuration))
	}

	trimDuration := endTime - startTime
	if trimDuration < 1 {
		return nil, refuseTrim("trim duration must be at least 1 second")
	}

	// Check for duplicate trims. Same rule as planSingle: a read failure is
	// an error, since an empty list here would let the base name — and the
	// overwrite — through.
	existing, err := ts.db.GetTrimsForJob(job.ID)
	if err != nil {
		return nil, fmt.Errorf("list existing trims: %w", err)
	}
	for _, t := range existing {
		if t.StartTime == startTime && t.EndTime == endTime {
			return nil, conflictTrim("trim already exists")
		}
	}

	// Find which segments overlap with [startTime, endTime)
	var involved []segTrimInfo
	for _, r := range ranges {
		// Skip segments entirely before or after the trim range
		if r.GlobalEnd <= startTime || r.GlobalStart >= endTime {
			continue
		}
		localStart := 0.0
		if startTime > r.GlobalStart {
			localStart = startTime - r.GlobalStart
		}
		localEnd := r.Segment.DurationSeconds
		if endTime < r.GlobalEnd {
			localEnd = endTime - r.GlobalStart
		}
		involved = append(involved, segTrimInfo{
			Segment:    r.Segment,
			LocalStart: localStart,
			LocalEnd:   localEnd,
		})
	}

	if len(involved) == 0 {
		return nil, refuseTrim("no segments found for trim range")
	}

	// Verify all segment files exist
	for _, s := range involved {
		if s.Segment.FilePath == "" {
			return nil, fmt.Errorf("segment %d has no file path", s.Segment.SegmentIndex)
		}
		if _, err := os.Stat(s.Segment.FilePath); err != nil {
			return nil, fmt.Errorf("segment %d file not found: %w", s.Segment.SegmentIndex, err)
		}
	}

	// Generate output filename and paths
	outputDir := filepath.Dir(involved[0].Segment.FilePath)
	trimDir := filepath.Join(outputDir, "trim")
	if err := os.MkdirAll(trimDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trim dir: %w", err)
	}

	// The list read for the duplicate check above is the one the name
	// needs; nothing adds a trim for this job before the encode is done
	// (activeOps holds one trim per job).
	trimBasename := uniqueTrimBasename(existing, job.VideoID, startTime, endTime)
	return &trimPlan{
		job:      job,
		start:    startTime,
		end:      endTime,
		duration: trimDuration,
		trimPath: filepath.Join(trimDir, trimBasename),
		// Relative path for DB storage
		relPath:  filepath.Join(filepath.Dir(job.Filename), "trim", trimBasename),
		involved: involved,
	}, nil
}

// encodeSegments encodes a quality-split recording's trim into out.
func (ts *TrimService) encodeSegments(ctx context.Context, plan *trimPlan, out string, progressFn func(float64)) error {
	involved := plan.involved
	// Single-segment fast path
	if len(involved) == 1 {
		m := ts.mux()
		seg := involved[0]
		audioBitrate := probeAudioBitrate(ctx, m.FFprobePath(), seg.Segment.FilePath)
		opts := &engine.TrimOptions{
			TrimStartOffset: seg.LocalStart,
			TrimDuration:    seg.LocalEnd - seg.LocalStart,
			CRF:             defaultTrimCRF,
			AudioBitrate:    audioBitrate,
			UsePreciseTrim:  true,
			ProgressFn:      progressFn,
		}
		if err := m.Mux(ctx, seg.Segment.FilePath, "", out, opts); err != nil {
			return fmt.Errorf("ffmpeg trim: %w", err)
		}
		return nil
	}

	// Multi-segment: find target quality (lowest resolution + FPS among involved)
	targetW, targetH, targetFPS := 0, 0, 0
	for _, s := range involved {
		w, h, fps := 0, 0, 0
		if s.Segment.VideoWidth != nil {
			w = *s.Segment.VideoWidth
		}
		if s.Segment.VideoHeight != nil {
			h = *s.Segment.VideoHeight
		}
		if s.Segment.VideoFps != nil {
			fps = *s.Segment.VideoFps
		}
		if targetH == 0 || h < targetH {
			targetW = w
			targetH = h
		}
		if targetFPS == 0 || fps < targetFPS {
			targetFPS = fps
		}
	}
	if targetH == 0 {
		targetH = 720
		targetW = 1280
	}
	if targetFPS == 0 {
		targetFPS = 30
	}

	// Probe audio bitrate from the first involved segment
	m := ts.mux()
	audioBitrate := probeAudioBitrate(ctx, m.FFprobePath(), involved[0].Segment.FilePath)

	// Build TrimSegmentInput slice
	var inputs []engine.TrimSegmentInput
	for _, s := range involved {
		w, h, fps := 0, 0, 0
		if s.Segment.VideoWidth != nil {
			w = *s.Segment.VideoWidth
		}
		if s.Segment.VideoHeight != nil {
			h = *s.Segment.VideoHeight
		}
		if s.Segment.VideoFps != nil {
			fps = *s.Segment.VideoFps
		}
		needScale := w != targetW || h != targetH || fps != targetFPS
		inputs = append(inputs, engine.TrimSegmentInput{
			InputPath: s.Segment.FilePath,
			StartTime: s.LocalStart,
			Duration:  s.LocalEnd - s.LocalStart,
			NeedScale: needScale,
		})
	}

	if err := m.TrimAndConcatWithProgress(ctx, inputs, out, targetW, targetH, targetFPS, defaultTrimCRF, audioBitrate, progressFn); err != nil {
		return fmt.Errorf("multi-segment trim: %w", err)
	}
	return nil
}

// sendTrimFailed is the "Trim Failed" embed: the one builder both senders
// share — TrimService for a trim that broke, and the post-download trim for
// the failures the service leaves to its caller.
//
// `trim_error` is never a lifecycle event (it stays its own post, and it is
// one of the events that can still ping an edit-mode target), but the
// footer's platform, the author line and the dashboard deep link are the
// job's either way, and the deep link needs JobID and Author together.
func sendTrimFailed(n notifications.Sender, job *database.Job, trimErr error) {
	if n == nil || job == nil {
		return
	}
	f := NotifyFacts(job)
	// Title and channel are job-supplied text, escaped as every builder in
	// internal/notifications and the worker's own Job Failed send escape
	// them; the Error field below already was.
	n.Send("Trim Failed",
		fmt.Sprintf("Failed to create trim for \"%s\"", notifications.EscapeMarkdown(job.Title)),
		notifications.TypeError,
		[]notifications.Field{
			{Name: "Channel", Value: notifications.EscapeMarkdown(job.ChannelName), Inline: true},
			{Name: notifications.IDLabel(job.Platform), Value: job.VideoID, Inline: true},
			{Name: "Error", Value: notifications.EscapeMarkdown(trimErr.Error())},
		},
		notifications.SendOptions{
			URL:       f.URL,
			Thumbnail: f.ThumbnailURL,
			Event:     "trim_error",
			Author:    notifyAuthor(f),
			Platform:  f.Platform,
			JobID:     f.ID,
		},
	)
}

// trimSeconds spells a trim bound for a refusal: to the millisecond, without
// trailing zeros. Rounded to whole seconds, "%.0f" told someone whose end ran
// 0.48 s past a 3600 s row that "end time (3600s) exceeds video duration
// (3600s)".
func trimSeconds(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64) + "s"
}

// uniqueTrimBasename returns a trim filename that doesn't collide with any
// existing trim's file for the same job. Names use whole-second-rounded
// bounds, so two distinct sub-second ranges (1.2s and 1.8s) can round to the
// same name — without disambiguation the second FFmpeg run would overwrite
// the first trim's file while both DB records point at the same bytes.
func uniqueTrimBasename(existing []database.TrimRecord, videoID string, startTime, endTime float64) string {
	taken := func(name string) bool {
		for _, t := range existing {
			if filepath.Base(t.Filename) == name {
				return true
			}
		}
		return false
	}
	name := fmt.Sprintf("%s [%.0fs-%.0fs].mp4", videoID, startTime, endTime)
	for n := 2; taken(name); n++ {
		name = fmt.Sprintf("%s [%.0fs-%.0fs] (%d).mp4", videoID, startTime, endTime, n)
	}
	return name
}

// probeAudioBitrate probes the audio bitrate of a source file in kbps.
// Falls back to 128 kbps if the stream has no bitrate metadata (matches TS probeAudioBitrate).
func probeAudioBitrate(ctx context.Context, ffprobePath, filePath string) int {
	const defaultAudioBitrate = 128

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-select_streams", "a:0",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil || len(output) == 0 {
		return defaultAudioBitrate
	}

	var data struct {
		Streams []struct {
			BitRate   string `json:"bit_rate"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(output, &data); err != nil {
		return defaultAudioBitrate
	}

	if len(data.Streams) == 0 {
		return defaultAudioBitrate
	}

	stream := data.Streams[0]
	if stream.BitRate == "" {
		return defaultAudioBitrate
	}

	// bit_rate is in bps (ffprobe returns it as a string), convert to kbps
	bps, parseErr := strconv.ParseFloat(stream.BitRate, 64)
	if parseErr != nil {
		return defaultAudioBitrate
	}
	if bps > 0 {
		kbps := int(math.Round(bps / 1000))
		return kbps
	}

	return defaultAudioBitrate
}
