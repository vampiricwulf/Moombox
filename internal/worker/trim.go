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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

const defaultTrimCRF = 18

// TrimRefusedError is a trim request refused for a reason the requester can
// act on: the job's state, the time range, a duplicate, a trim already
// running. Reason is written for the user and is shown as-is by the API;
// every other error CreateTrim returns is an internal failure whose detail
// stays in the log.
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

// TrimService handles creating and deleting trim records.
type TrimService struct {
	muxer     *engine.Muxer
	muxerMu   sync.RWMutex // guards muxer across SetFfmpegPath hot-reload vs. in-flight trims
	db        *database.Database
	notifier  notifications.Sender
	activeMu  sync.Mutex
	activeOps map[string]bool // tracks in-flight trim operations per job
	logger    interface {
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
	return &TrimService{
		muxer:     engine.NewMuxer(ffmpegPath, logger),
		db:        db,
		activeOps: make(map[string]bool),
		logger:    logger,
	}
}

// SetNotifier sets the notification manager for trim notifications.
func (ts *TrimService) SetNotifier(nm notifications.Sender) {
	ts.notifier = nm
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

// CreateTrim creates a trimmed version of a finished download. progressFn is
// called with 0-100 as FFmpeg encoding progresses; pass nil when progress
// reporting is not needed (audit reports/worker.md F58 — previously split
// into CreateTrim + CreateTrimWithProgress; the two thin wrappers added no
// value over a single method with an optional callback).
func (ts *TrimService) CreateTrim(ctx context.Context, job *database.Job, startTime, endTime float64, progressFn func(float64)) (*database.TrimRecord, error) {
	// Prevent concurrent trim operations on the same job (matching TS activeTrimOps)
	ts.activeMu.Lock()
	if ts.activeOps[job.ID] {
		ts.activeMu.Unlock()
		return nil, conflictTrim("another trim operation is already in progress for this job")
	}
	ts.activeOps[job.ID] = true
	ts.activeMu.Unlock()
	defer func() {
		ts.activeMu.Lock()
		delete(ts.activeOps, job.ID)
		ts.activeMu.Unlock()
	}()

	// Validate
	if job.Status != database.StatusFinished {
		return nil, refuseTrim("job must be finished to trim")
	}

	// Multi-segment path: if the job has segments, dispatch to segment-aware trim
	if len(job.Segments) > 0 {
		return ts.createMultiSegmentTrimInternal(ctx, job, startTime, endTime, progressFn)
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
	// Validate end time doesn't exceed video duration
	if job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		maxDuration := float64(*job.LengthSeconds)
		if endTime > maxDuration {
			return nil, refuseTrim("end time (%.0fs) exceeds video duration (%.0fs)", endTime, maxDuration)
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
	trimPath := filepath.Join(trimDir, trimBasename)
	// No trim row names the file until the encode is done, which for a long
	// range is a long time to be an unreferenced file in trim/.
	defer claimOutputStem(job.ID, strings.TrimSuffix(trimPath, filepath.Ext(trimPath)))()

	// Store relative path including parent directory from source job's filename
	// (matches TS: path.join(path.dirname(sourceJob.filename), "trim", trimFilename))
	sourceParentDir := filepath.Dir(job.Filename)
	trimRelativePath := filepath.Join(sourceParentDir, "trim", trimBasename)

	ts.logger.Info("creating trim", "jobID", job.ID, "start", startTime, "end", endTime)

	// Probe audio bitrate to match source quality (matches TS probeAudioBitrate)
	m := ts.mux()
	audioBitrate := probeAudioBitrate(ctx, m.FFprobePath(), job.OutputFile)

	// Run FFmpeg with progress if callback provided
	opts := &engine.TrimOptions{
		TrimStartOffset: startTime,
		TrimDuration:    duration,
		CRF:             defaultTrimCRF,
		AudioBitrate:    audioBitrate,
		UsePreciseTrim:  true,
		ProgressFn:      progressFn,
	}
	if err := m.Mux(ctx, job.OutputFile, "", trimPath, opts); err != nil {
		return nil, fmt.Errorf("ffmpeg trim: %w", err)
	}

	// Get file size
	info, _ := os.Stat(trimPath)
	var fileSize *int64
	if info != nil {
		sz := info.Size()
		fileSize = &sz
	}

	// Create record
	trimID := fmt.Sprintf("trim_%s_%d", job.ID, time.Now().UnixMilli())
	record := &database.TrimRecord{
		ID:        trimID,
		JobID:     job.ID,
		StartTime: startTime,
		EndTime:   endTime,
		Filename:  trimRelativePath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Duration:  duration,
		FileSize:  fileSize,
	}

	if err := ts.db.AddTrim(record); err != nil {
		return nil, fmt.Errorf("save trim record: %w", err)
	}

	// Send "Trim Created" notification
	if ts.notifier != nil {
		ts.notifier.Send(notifications.TrimCreated(NotifyFacts(job), notifications.TrimFacts{
			TimeRange: fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime)),
			Duration:  time.Duration(duration) * time.Second,
			Size:      fileSize,
		}))
	}

	ts.logger.Info("trim created", "trimID", trimID, "path", trimPath)
	return record, nil
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

// createMultiSegmentTrimInternal handles trimming for multi-segment quality-split jobs.
// It maps global start/end times to segment-local times, trims each involved
// segment, and concatenates them (with quality normalization if needed).
func (ts *TrimService) createMultiSegmentTrimInternal(ctx context.Context, job *database.Job, startTime, endTime float64, progressFn func(float64)) (*database.TrimRecord, error) {
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

	totalDuration := cumulative
	if endTime > totalDuration {
		return nil, refuseTrim("end time (%.0fs) exceeds total duration (%.0fs)", endTime, totalDuration)
	}

	trimDuration := endTime - startTime
	if trimDuration < 1 {
		return nil, refuseTrim("trim duration must be at least 1 second")
	}

	// Check for duplicate trims. Same rule as CreateTrim: a read failure is
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
	type segTrimInfo struct {
		Segment    *database.Segment
		LocalStart float64
		LocalEnd   float64
	}
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
	// needs; nothing has added a trim for this job in between (activeOps
	// serialises trims per job).
	trimBasename := uniqueTrimBasename(existing, job.VideoID, startTime, endTime)
	trimPath := filepath.Join(trimDir, trimBasename)
	defer claimOutputStem(job.ID, strings.TrimSuffix(trimPath, filepath.Ext(trimPath)))() // see CreateTrim

	// Relative path for DB storage
	sourceParentDir := filepath.Dir(job.Filename)
	trimRelativePath := filepath.Join(sourceParentDir, "trim", trimBasename)

	ts.logger.Info("creating multi-segment trim", "jobID", job.ID,
		"start", startTime, "end", endTime, "segments", len(involved))

	// Single-segment fast path
	if len(involved) == 1 {
		m := ts.mux()
		seg := involved[0]
		audioBitrate := probeAudioBitrate(ctx, m.FFprobePath(), seg.Segment.FilePath)
		duration := seg.LocalEnd - seg.LocalStart
		opts := &engine.TrimOptions{
			TrimStartOffset: seg.LocalStart,
			TrimDuration:    duration,
			CRF:             defaultTrimCRF,
			AudioBitrate:    audioBitrate,
			UsePreciseTrim:  true,
			ProgressFn:      progressFn,
		}
		if err := m.Mux(ctx, seg.Segment.FilePath, "", trimPath, opts); err != nil {
			return nil, fmt.Errorf("ffmpeg trim: %w", err)
		}
	} else {
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

		if progressFn != nil {
			if err := m.TrimAndConcatWithProgress(ctx, inputs, trimPath, targetW, targetH, targetFPS, defaultTrimCRF, audioBitrate, progressFn); err != nil {
				return nil, fmt.Errorf("multi-segment trim: %w", err)
			}
		} else {
			if err := m.TrimAndConcat(ctx, inputs, trimPath, targetW, targetH, targetFPS, defaultTrimCRF, audioBitrate); err != nil {
				return nil, fmt.Errorf("multi-segment trim: %w", err)
			}
		}
	}

	// Get file size
	info, _ := os.Stat(trimPath)
	var fileSize *int64
	if info != nil {
		sz := info.Size()
		fileSize = &sz
	}

	// Create record
	trimID := fmt.Sprintf("trim_%s_%d", job.ID, time.Now().UnixMilli())
	record := &database.TrimRecord{
		ID:        trimID,
		JobID:     job.ID,
		StartTime: startTime,
		EndTime:   endTime,
		Filename:  trimRelativePath,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Duration:  trimDuration,
		FileSize:  fileSize,
	}

	if err := ts.db.AddTrim(record); err != nil {
		return nil, fmt.Errorf("save trim record: %w", err)
	}

	// Send notification
	if ts.notifier != nil {
		ts.notifier.Send(notifications.TrimCreated(NotifyFacts(job), notifications.TrimFacts{
			TimeRange: fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime)),
			Duration:  time.Duration(trimDuration) * time.Second,
			Size:      fileSize,
			Parts:     len(involved),
		}))
	}

	ts.logger.Info("multi-segment trim created", "trimID", trimID, "path", trimPath,
		"segments", len(involved))
	return record, nil
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
