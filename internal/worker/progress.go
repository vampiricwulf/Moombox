package worker

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	progressUpdateInterval  = 16 * time.Millisecond // ~60fps, matches TUI tick rate
	progressPersistInterval = 1 * time.Second
)

// ProgressTracker tracks download progress and updates the database.
type ProgressTracker struct {
	mu            sync.Mutex
	db            *database.Database
	logger        interface{ Warn(msg string, args ...any) }
	jobID         string
	videoSeq      int
	audioSeq      int
	videoTotal    int
	audioTotal    int
	chatCount     int
	bytesTotal       int64
	speedSmooth      *utils.SmoothValue
	lastUpdate       time.Time
	lastPersist      time.Time
	lastVideoBytes   int64 // last p.Bytes for video downloader delta accumulation
	lastAudioBytes   int64 // last p.Bytes for audio downloader delta accumulation
	speedLastBytes   int64 // last bytesTotal snapshot for speed calculation
	lastBytesTime    time.Time
	startTime        time.Time // B8: for ETA calculation
	vodPercent       float64   // VOD download progress percentage (from chunked download)
	vodTotalBytes    int64     // Total file size for VOD chunked download (0 if not VOD)
	gaps          []database.Gap
}

// NewProgressTracker creates a new progress tracker for a job.
func NewProgressTracker(db *database.Database, jobID string, logger interface{ Warn(msg string, args ...any) }) *ProgressTracker {
	now := time.Now()
	return &ProgressTracker{
		db:            db,
		logger:        logger,
		jobID:         jobID,
		speedSmooth:   utils.NewSmoothValue(0.7),
		lastUpdate:    now,
		lastPersist:   now,
		lastBytesTime: now,
		startTime:     now,
	}
}

// AttachVideoDownloader attaches progress callbacks to a video segment downloader.
func (pt *ProgressTracker) AttachVideoDownloader(dl *engine.SegmentDownloader) {
	dl.OnProgress = func(p engine.DownloadProgress) {
		pt.mu.Lock()
		pt.videoSeq = p.Seq
		if p.HeadSeq > 0 {
			pt.videoTotal = p.HeadSeq
		}
		if p.Total > 0 {
			pt.videoTotal = p.Total
		}
		// Total must never be smaller than the last downloaded segment
		if pt.videoTotal > 0 && pt.videoTotal < pt.videoSeq {
			pt.videoTotal = pt.videoSeq
		}
		// Track VOD chunked download progress
		if p.TotalBytes > 0 {
			pt.vodTotalBytes = p.TotalBytes
		}
		if p.Percent > 0 {
			pt.vodPercent = p.Percent
		}
		pt.bytesTotal += p.Bytes - pt.lastVideoBytes
		pt.lastVideoBytes = p.Bytes
		pt.mu.Unlock()
		pt.maybeUpdate()
	}

	dl.OnGap = func(g engine.DownloadGap) {
		pt.logger.Warn(fmt.Sprintf("[DownloadOrchestrator] video segment gap: seq %d–%d", g.From, g.To))
		pt.mu.Lock()
		pt.gaps = append(pt.gaps, database.Gap{
			JobID:  pt.jobID,
			From:   g.From,
			To:     g.To,
			Stream: "video",
		})
		pt.mu.Unlock()
	}
}

// AttachAudioDownloader attaches progress callbacks to an audio segment downloader.
func (pt *ProgressTracker) AttachAudioDownloader(dl *engine.SegmentDownloader) {
	dl.OnProgress = func(p engine.DownloadProgress) {
		pt.mu.Lock()
		pt.audioSeq = p.Seq
		if p.HeadSeq > 0 {
			pt.audioTotal = p.HeadSeq
		}
		if p.Total > 0 {
			pt.audioTotal = p.Total
		}
		// Total must never be smaller than the last downloaded segment
		if pt.audioTotal > 0 && pt.audioTotal < pt.audioSeq {
			pt.audioTotal = pt.audioSeq
		}
		pt.bytesTotal += p.Bytes - pt.lastAudioBytes
		pt.lastAudioBytes = p.Bytes
		pt.mu.Unlock()
		pt.maybeUpdate()
	}

	dl.OnGap = func(g engine.DownloadGap) {
		pt.logger.Warn(fmt.Sprintf("[DownloadOrchestrator] audio segment gap: seq %d–%d", g.From, g.To))
		pt.mu.Lock()
		pt.gaps = append(pt.gaps, database.Gap{
			JobID:  pt.jobID,
			From:   g.From,
			To:     g.To,
			Stream: "audio",
		})
		pt.mu.Unlock()
	}
}

// SetChatCount updates the chat message count.
func (pt *ProgressTracker) SetChatCount(count int) {
	pt.mu.Lock()
	pt.chatCount = count
	pt.mu.Unlock()
	pt.maybeUpdate()
}

func (pt *ProgressTracker) maybeUpdate() {
	pt.mu.Lock()

	now := time.Now()
	if now.Sub(pt.lastUpdate) < progressUpdateInterval {
		pt.mu.Unlock()
		return
	}
	pt.lastUpdate = now

	// Calculate instantaneous speed (bytes delta / time delta, matching TS)
	elapsed := now.Sub(pt.lastBytesTime).Seconds()
	if elapsed > 0 {
		bytesDelta := pt.bytesTotal - pt.speedLastBytes
		speed := float64(bytesDelta) / elapsed
		pt.speedSmooth.Update(speed)
		pt.speedLastBytes = pt.bytesTotal
		pt.lastBytesTime = now
	}

	// Build progress string (A3: includes chat count like "V:1234 A:1234 C:5678")
	progress := pt.buildProgressString()

	// Calculate percent
	percent := 0.0
	if pt.vodTotalBytes > 0 {
		percent = pt.vodPercent
	} else if pt.videoTotal > 0 {
		percent = float64(pt.videoSeq) / float64(pt.videoTotal) * 100
	}

	// B8: Calculate ETA
	eta := pt.calculateETA()

	// Snapshot values for DB update
	updates := map[string]any{
		"progress":            progress,
		"percent":             percent,
		"speed":               utils.FormatSpeed(pt.speedSmooth.Value()),
		"last_video_seq":      pt.videoSeq,
		"last_audio_seq":      pt.audioSeq,
		"total_video_seq":     pt.videoTotal,
		"total_audio_seq":     pt.audioTotal,
		"total_chat_messages": pt.chatCount,
	}
	if eta != "" {
		updates["eta"] = eta
	}

	// Snapshot gaps for persistence
	var gapsToSave []database.Gap
	shouldPersist := now.Sub(pt.lastPersist) >= progressPersistInterval
	if shouldPersist {
		pt.lastPersist = now
		gapsToSave = make([]database.Gap, len(pt.gaps))
		copy(gapsToSave, pt.gaps)
		pt.gaps = nil
	}

	pt.mu.Unlock()

	// DB operations outside the lock to reduce contention
	pt.db.UpdateJobFields(pt.jobID, updates)

	for _, gap := range gapsToSave {
		pt.db.AddGap(gap.JobID, gap.From, gap.To, gap.Stream)
	}
}

// buildProgressString builds the progress display string.
// Format matches TypeScript: "(A: X V: Y C: Z)" for DASH, "Seq: X" for HLS, "V:95.3%" for VOD.
func (pt *ProgressTracker) buildProgressString() string {
	// VOD chunked download: show percentage instead of segment counts
	if pt.vodTotalBytes > 0 {
		s := fmt.Sprintf("V:%.1f%%", pt.vodPercent)
		if pt.chatCount > 0 {
			s += fmt.Sprintf(" C: %d", pt.chatCount)
		}
		return s
	}

	if pt.audioTotal > 0 || pt.audioSeq > 0 {
		vPart := strconv.Itoa(pt.videoSeq)
		if pt.videoTotal > 0 {
			vPart = strconv.Itoa(pt.videoSeq) + "/" + strconv.Itoa(pt.videoTotal)
		}
		aPart := strconv.Itoa(pt.audioSeq)
		if pt.audioTotal > 0 {
			aPart = strconv.Itoa(pt.audioSeq) + "/" + strconv.Itoa(pt.audioTotal)
		}
		s := fmt.Sprintf("(A: %s V: %s", aPart, vPart)
		if pt.chatCount > 0 {
			s += fmt.Sprintf(" C: %d", pt.chatCount)
		}
		return s + ")"
	}
	if pt.chatCount > 0 {
		return fmt.Sprintf("Seq: %d C: %d", pt.videoSeq, pt.chatCount)
	}
	return fmt.Sprintf("Seq: %d", pt.videoSeq)
}

// calculateETA estimates time remaining based on segment or byte progress (B8).
func (pt *ProgressTracker) calculateETA() string {
	elapsed := time.Since(pt.startTime).Seconds()
	if elapsed < 5 {
		return "" // Too early for meaningful estimate
	}

	var remaining float64

	if pt.vodTotalBytes > 0 && pt.bytesTotal > 0 {
		// VOD chunked download: bytes-based ETA
		bytesPerSec := float64(pt.bytesTotal) / elapsed
		if bytesPerSec <= 0 {
			return ""
		}
		remaining = float64(pt.vodTotalBytes-pt.bytesTotal) / bytesPerSec
	} else if pt.videoTotal > 0 && pt.videoSeq > 0 {
		// Segment-based ETA
		segsPerSec := float64(pt.videoSeq) / elapsed
		if segsPerSec <= 0 {
			return ""
		}
		remaining = float64(pt.videoTotal-pt.videoSeq) / segsPerSec
	} else {
		return ""
	}

	if remaining <= 0 {
		return ""
	}

	d := time.Duration(remaining) * time.Second
	if d > 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d > time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d > time.Minute {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// Finalize saves any remaining state.
func (pt *ProgressTracker) Finalize() {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	for _, gap := range pt.gaps {
		pt.db.AddGap(gap.JobID, gap.From, gap.To, gap.Stream)
	}
	pt.gaps = nil
}
