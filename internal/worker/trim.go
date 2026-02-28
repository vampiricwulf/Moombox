package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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

// CreateTrim creates a trimmed version of a finished download.
func (ts *TrimService) CreateTrim(ctx context.Context, job *database.Job, startTime, endTime float64) (*database.TrimRecord, error) {
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

	// Run FFmpeg
	if err := ts.muxer.TrimWithAudio(ctx, job.OutputFile, trimPath, startTime, endTime, defaultTrimCRF, audioBitrate); err != nil {
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
				URL:   job.URL,
				Event: "trim_deleted",
			},
		)
	}

	ts.logger.Info("trim deleted", "trimID", trimID)
	return nil
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

	// bit_rate is in bps, convert to kbps
	var bps float64
	if err := json.Unmarshal([]byte(stream.BitRate), &bps); err != nil {
		// Try parsing as string number
		fmt.Sscanf(stream.BitRate, "%f", &bps)
	}
	if bps > 0 {
		kbps := int(math.Round(bps / 1000))
		return kbps
	}

	return defaultAudioBitrate
}
