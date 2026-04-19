package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

const defaultTrimCRF = 18

// TrimService handles creating and deleting trim records.
type TrimService struct {
	muxer      *engine.Muxer
	db         *database.Database
	notifier   *notifications.Manager
	activeMu   sync.Mutex
	activeOps  map[string]bool // tracks in-flight trim operations per job
	logger     interface {
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
func (ts *TrimService) SetNotifier(nm *notifications.Manager) {
	ts.notifier = nm
}

// CreateTrimWithProgress creates a trimmed version with FFmpeg progress reporting.
// progressFn is called with 0-100 as encoding progresses.
func (ts *TrimService) CreateTrimWithProgress(ctx context.Context, job *database.Job, startTime, endTime float64, progressFn func(float64)) (*database.TrimRecord, error) {
	return ts.createTrimInternal(ctx, job, startTime, endTime, progressFn)
}

// CreateTrim creates a trimmed version of a finished download.
func (ts *TrimService) CreateTrim(ctx context.Context, job *database.Job, startTime, endTime float64) (*database.TrimRecord, error) {
	return ts.createTrimInternal(ctx, job, startTime, endTime, nil)
}

func (ts *TrimService) createTrimInternal(ctx context.Context, job *database.Job, startTime, endTime float64, progressFn func(float64)) (*database.TrimRecord, error) {
	// Prevent concurrent trim operations on the same job (matching TS activeTrimOps)
	ts.activeMu.Lock()
	if ts.activeOps[job.ID] {
		ts.activeMu.Unlock()
		return nil, fmt.Errorf("another trim operation is already in progress for this job")
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
		return nil, fmt.Errorf("job must be finished to trim")
	}

	// Multi-segment path: if the job has segments, dispatch to segment-aware trim
	if len(job.Segments) > 0 {
		return ts.createMultiSegmentTrimInternal(ctx, job, startTime, endTime, progressFn)
	}

	if job.OutputFile == "" {
		return nil, fmt.Errorf("no output file for job")
	}
	if _, err := os.Stat(job.OutputFile); err != nil {
		return nil, fmt.Errorf("output file not found: %w", err)
	}
	if startTime < 0 {
		return nil, fmt.Errorf("start time cannot be negative")
	}
	if startTime >= endTime {
		return nil, fmt.Errorf("start time must be before end time")
	}
	// Validate end time doesn't exceed video duration
	if job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		maxDuration := float64(*job.LengthSeconds)
		if endTime > maxDuration {
			return nil, fmt.Errorf("end time (%.0fs) exceeds video duration (%.0fs)", endTime, maxDuration)
		}
	}
	duration := endTime - startTime
	if duration < 1 {
		return nil, fmt.Errorf("trim duration must be at least 1 second")
	}

	// Generate output filename
	trimDir := filepath.Join(filepath.Dir(job.OutputFile), "trim")
	if err := os.MkdirAll(trimDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trim dir: %w", err)
	}

	trimBasename := fmt.Sprintf("%s [%.0fs-%.0fs].mp4", job.VideoID, startTime, endTime)
	trimPath := filepath.Join(trimDir, trimBasename)

	// Store relative path including parent directory from source job's filename
	// (matches TS: path.join(path.dirname(sourceJob.filename), "trim", trimFilename))
	sourceParentDir := filepath.Dir(job.Filename)
	trimRelativePath := filepath.Join(sourceParentDir, "trim", trimBasename)

	// Check for duplicates
	existing, _ := ts.db.GetTrimsForJob(job.ID)
	for _, t := range existing {
		if t.StartTime == startTime && t.EndTime == endTime {
			return nil, fmt.Errorf("trim already exists")
		}
	}

	ts.logger.Info("creating trim", "jobID", job.ID, "start", startTime, "end", endTime)

	// Probe audio bitrate to match source quality (matches TS probeAudioBitrate)
	audioBitrate := probeAudioBitrate(ctx, ts.muxer.FFprobePath(), job.OutputFile)

	// Run FFmpeg with progress if callback provided
	opts := &engine.TrimOptions{
		TrimStartOffset: startTime,
		TrimDuration:    duration,
		CRF:             defaultTrimCRF,
		AudioBitrate:    audioBitrate,
		UsePreciseTrim:  true,
		ProgressFn:      progressFn,
	}
	if err := ts.muxer.Mux(ctx, job.OutputFile, "", trimPath, opts); err != nil {
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

	// Send "Trim Created" notification (matches TS NotificationType.INFO + format)
	if ts.notifier != nil {
		timeRange := fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime))
		durStr := formatDurationHuman(time.Duration(duration) * time.Second)
		fields := []notifications.Field{
			{Name: "Source Video", Value: job.Title, Inline: false},
			{Name: "Time Range", Value: timeRange, Inline: true},
			{Name: "Duration", Value: durStr, Inline: true},
		}
		if fileSize != nil {
			fields = append(fields, notifications.Field{
				Name: "File Size", Value: formatFileSize(*fileSize), Inline: true,
			})
		}
		ts.notifier.Send("Trim Created",
			fmt.Sprintf("Created %s trim from \"%s\"", durStr, job.Title),
			notifications.TypeInfo,
			fields,
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "trim_created",
			},
		)
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
		return fmt.Errorf("job not found: %s", jobID)
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
		return fmt.Errorf("trim not found")
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
		ts.notifier.Send("Trim Deleted",
			fmt.Sprintf("Trim deleted for: %s", job.Title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Source Video", Value: job.Title, Inline: false},
				{Name: "Time Range", Value: timeRange, Inline: true},
				{Name: "Duration", Value: durStr, Inline: true},
			},
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "trim_deleted",
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
		return nil, fmt.Errorf("start time cannot be negative")
	}
	if startTime >= endTime {
		return nil, fmt.Errorf("start time must be before end time")
	}

	// Calculate cumulative time offsets for each segment
	type segmentRange struct {
		Segment    *database.Segment
		GlobalStart float64 // cumulative start in global timeline
		GlobalEnd   float64 // cumulative end in global timeline
	}

	var ranges []segmentRange
	var cumulative float64
	for i := range job.Segments {
		seg := &job.Segments[i]
		r := segmentRange{
			Segment:    seg,
			GlobalStart: cumulative,
			GlobalEnd:   cumulative + seg.DurationSeconds,
		}
		ranges = append(ranges, r)
		cumulative += seg.DurationSeconds
	}

	totalDuration := cumulative
	if endTime > totalDuration {
		return nil, fmt.Errorf("end time (%.0fs) exceeds total duration (%.0fs)", endTime, totalDuration)
	}

	trimDuration := endTime - startTime
	if trimDuration < 1 {
		return nil, fmt.Errorf("trim duration must be at least 1 second")
	}

	// Check for duplicate trims
	existing, _ := ts.db.GetTrimsForJob(job.ID)
	for _, t := range existing {
		if t.StartTime == startTime && t.EndTime == endTime {
			return nil, fmt.Errorf("trim already exists")
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
		return nil, fmt.Errorf("no segments found for trim range")
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

	trimBasename := fmt.Sprintf("%s [%.0fs-%.0fs].mp4", job.VideoID, startTime, endTime)
	trimPath := filepath.Join(trimDir, trimBasename)

	// Relative path for DB storage
	sourceParentDir := filepath.Dir(job.Filename)
	trimRelativePath := filepath.Join(sourceParentDir, "trim", trimBasename)

	ts.logger.Info("creating multi-segment trim", "jobID", job.ID,
		"start", startTime, "end", endTime, "segments", len(involved))

	// Single-segment fast path
	if len(involved) == 1 {
		seg := involved[0]
		audioBitrate := probeAudioBitrate(ctx, ts.muxer.FFprobePath(), seg.Segment.FilePath)
		duration := seg.LocalEnd - seg.LocalStart
		opts := &engine.TrimOptions{
			TrimStartOffset: seg.LocalStart,
			TrimDuration:    duration,
			CRF:             defaultTrimCRF,
			AudioBitrate:    audioBitrate,
			UsePreciseTrim:  true,
			ProgressFn:      progressFn,
		}
		if err := ts.muxer.Mux(ctx, seg.Segment.FilePath, "", trimPath, opts); err != nil {
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
		audioBitrate := probeAudioBitrate(ctx, ts.muxer.FFprobePath(), involved[0].Segment.FilePath)

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
			if err := ts.muxer.TrimAndConcatWithProgress(ctx, inputs, trimPath, targetW, targetH, targetFPS, defaultTrimCRF, audioBitrate, progressFn); err != nil {
				return nil, fmt.Errorf("multi-segment trim: %w", err)
			}
		} else {
			if err := ts.muxer.TrimAndConcat(ctx, inputs, trimPath, targetW, targetH, targetFPS, defaultTrimCRF, audioBitrate); err != nil {
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
		timeRange := fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime))
		durStr := formatDurationHuman(time.Duration(trimDuration) * time.Second)
		fields := []notifications.Field{
			{Name: "Source Video", Value: job.Title, Inline: false},
			{Name: "Time Range", Value: timeRange, Inline: true},
			{Name: "Duration", Value: durStr, Inline: true},
		}
		if len(involved) > 1 {
			fields = append(fields, notifications.Field{
				Name: "Segments", Value: fmt.Sprintf("%d segments", len(involved)), Inline: true,
			})
		}
		if fileSize != nil {
			fields = append(fields, notifications.Field{
				Name: "File Size", Value: formatFileSize(*fileSize), Inline: true,
			})
		}
		ts.notifier.Send("Trim Created",
			fmt.Sprintf("Created %s trim from \"%s\"", durStr, job.Title),
			notifications.TypeInfo,
			fields,
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "trim_created",
			},
		)
	}

	ts.logger.Info("multi-segment trim created", "trimID", trimID, "path", trimPath,
		"segments", len(involved))
	return record, nil
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
