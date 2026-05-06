// Package routes provides HTTP route handlers for the Moombox API.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// jobWithStaging wraps a Job with computed staging/segment file presence.
type jobWithStaging struct {
	*database.Job
	HasStaging  bool `json:"hasStaging"`
	HasSegments bool `json:"hasSegments"`
}

// enrichJob adds computed staging fields to a job response.
func enrichJob(job *database.Job, stagingBase string) jobWithStaging {
	return jobWithStaging{
		Job:         job,
		HasStaging:  worker.HasStagingFiles(stagingBase, job.ID),
		HasSegments: worker.HasSegmentFiles(stagingBase, job.ID),
	}
}

// YouTubeMetadataFetcher provides YouTube metadata for job creation.
type YouTubeMetadataFetcher interface {
	// FetchMetadata returns basic metadata (title, channel, thumbnail) for a video ID.
	FetchMetadata(ctx context.Context, videoID string) (*YouTubeJobMetadata, error)
}

// YouTubeJobMetadata holds metadata needed for creating a YouTube job.
type YouTubeJobMetadata struct {
	Title        string
	ChannelName  string
	ChannelID    string
	ThumbnailURL string
}

// TwitchMetadataFetcher provides Twitch stream/VOD metadata for job creation.
type TwitchMetadataFetcher interface {
	// FetchStreamMetadata returns metadata for a live Twitch channel.
	// Returns nil if channel is offline or not found.
	FetchStreamMetadata(ctx context.Context, login string) (*TwitchJobMetadata, error)
	// FetchVodMetadata returns metadata for a Twitch VOD.
	FetchVodMetadata(ctx context.Context, vodID string) (*TwitchJobMetadata, error)
}

// TwitchJobMetadata holds metadata needed for creating a Twitch job.
type TwitchJobMetadata struct {
	StreamID     string
	Title        string
	ChannelName  string
	ThumbnailURL string
	AvatarURL    string
	StartedAt    string
	GameCategory string
	IsLive       bool
}

// filterJobsByAge splits jobs into active vs archived based on the
// hide_finished_age_days config setting. Non-finished jobs are always active.
// Finished jobs are archived when their age exceeds the threshold.
// If hideAgeDays is 0, all finished jobs are immediately archived.
func filterJobsByAge(jobs []*database.Job, archived bool, store *config.Store) []*database.Job {
	var hideAgeDays float64
	store.Read(func(c *config.MoomboxConfig) {
		hideAgeDays = c.Monitors.HideFinishedAgeDays.Value
	})

	// Negative means never hide finished jobs — they all stay in active view
	if hideAgeDays < 0 {
		if archived {
			return nil
		}
		return jobs
	}

	hideAge := time.Duration(hideAgeDays*24) * time.Hour
	now := time.Now()

	var result []*database.Job
	for _, j := range jobs {
		if j.Status != database.StatusFinished {
			// Non-finished jobs are always in the active list
			if !archived {
				result = append(result, j)
			}
			continue
		}
		// Finished job: check age
		updatedAt, err := time.Parse(time.RFC3339, j.UpdatedAt)
		if err != nil {
			// If we can't parse the date, treat as active
			if !archived {
				result = append(result, j)
			}
			continue
		}
		age := now.Sub(updatedAt)
		if archived {
			if age > hideAge {
				result = append(result, j)
			}
		} else {
			if age <= hideAge {
				result = append(result, j)
			}
		}
	}
	return result
}

// sendPaginated applies optional offset/limit pagination and writes the response.
// Matches TypeScript validateOffsetPagination: returns 400 for invalid values.
func sendPaginated(rw http.ResponseWriter, req *http.Request, items []*database.Job) {
	offsetStr := req.URL.Query().Get("offset")
	limitStr := req.URL.Query().Get("limit")
	hasPagination := offsetStr != "" || limitStr != ""

	offset := 0
	limit := len(items)
	if offsetStr != "" {
		n, err := strconv.Atoi(offsetStr)
		if err != nil || n < 0 {
			jsonError(rw, fmt.Sprintf("Invalid offset parameter (must be >= 0): %s", offsetStr), http.StatusBadRequest)
			return
		}
		offset = n
	}
	if limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n < 1 || n > 1000 {
			jsonError(rw, fmt.Sprintf("Invalid limit parameter (must be 1-1000): %s", limitStr), http.StatusBadRequest)
			return
		}
		limit = n
	}

	total := len(items)

	if offset > len(items) {
		offset = len(items)
	}
	end := min(offset+limit, len(items))
	paged := items[offset:end]

	// Ensure non-nil slice so JSON serializes as [] not null
	if paged == nil {
		paged = []*database.Job{}
	}

	if hasPagination {
		jsonResponse(rw, map[string]any{
			"data": paged,
			"pagination": map[string]any{
				"total":   total,
				"offset":  offset,
				"limit":   limit,
				"hasMore": end < total,
			},
		})
	} else {
		jsonResponse(rw, paged)
	}
}

// JobRoutes registers job-related API routes. The Store carries the cfg
// pointer + lock; all read sites here go through store.Read so the
// migration drops the per-route cfgMu plumbing.
func JobRoutes(r chi.Router, db *database.Database, store *config.Store, w *worker.DownloadWorker, rl *web.RateLimiter, twitchFetcher TwitchMetadataFetcher, ytFetcher YouTubeMetadataFetcher, notifier *notifications.Manager, wsHub *web.WebSocketHub) {
	// GET /api/jobs
	r.Get("/api/jobs", func(rw http.ResponseWriter, req *http.Request) {
		jobs, err := db.GetAllJobs()
		if err != nil {
			jsonError(rw, "failed to get jobs", http.StatusInternalServerError)
			return
		}

		// Filter out finished jobs older than hide_finished_age_days
		filtered := filterJobsByAge(jobs, false, store)
		sendPaginated(rw, req, filtered)
	})

	// GET /api/jobs/archived — finished jobs older than hide_finished_age_days
	r.Get("/api/jobs/archived", func(rw http.ResponseWriter, req *http.Request) {
		jobs, err := db.GetAllJobs()
		if err != nil {
			jsonError(rw, "failed to get jobs", http.StatusInternalServerError)
			return
		}

		// Get only finished jobs that are older than the age threshold
		archived := filterJobsByAge(jobs, true, store)

		// Sort by updatedAt descending (most recent first)
		slices.SortFunc(archived, func(a, b *database.Job) int {
			if a.UpdatedAt > b.UpdatedAt {
				return -1
			}
			if a.UpdatedAt < b.UpdatedAt {
				return 1
			}
			return 0
		})

		sendPaginated(rw, req, archived)
	})

	// GET /api/jobs/:id
	r.Get("/api/jobs/{id}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		// Cache headers: only truly finished jobs get long-lived cache.
		// Error/Cancelled may be retried, so they need no-cache.
		if job.Status == database.StatusFinished {
			rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			rw.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}

		var stagingBase string
		store.Read(func(c *config.MoomboxConfig) {
			stagingBase = c.Paths.EffectiveStagingDir()
		})

		jsonResponse(rw, enrichJob(job, stagingBase))
	})

	// GET /api/jobs/:id/video — range-request video streaming
	r.Get("/api/jobs/{id}/video", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		if job.Filename == "" && job.OutputFile == "" {
			jsonError(rw, "no output file", http.StatusNotFound)
			return
		}

		// Build file path from output directory + relative filename
		var cfgOutputDir string
		store.Read(func(c *config.MoomboxConfig) {
			cfgOutputDir = c.Paths.OutputDirectory
		})

		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = cfgOutputDir
		}
		if outputDir == "" {
			outputDir = "./output"
		}

		filename := job.Filename
		if filename == "" {
			filename = job.OutputFile
		}

		filePath, err := filepath.Abs(filepath.Join(outputDir, filename))
		if err != nil {
			jsonError(rw, "invalid path", http.StatusBadRequest)
			return
		}

		// Fallback: if file not found at outputDir+filename, try outputFile
		// directly (it stores the full relative path from CWD).
		if _, err := os.Stat(filePath); os.IsNotExist(err) && job.OutputFile != "" {
			if alt, err2 := filepath.Abs(job.OutputFile); err2 == nil {
				if _, err3 := os.Stat(alt); err3 == nil {
					filePath = alt
				}
			}
		}

		// Path traversal guard: resolve symlinks before prefix check
		resolvedPath, ok := validatePathTraversal(filePath, outputDir)
		if !ok {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}
		filePath = resolvedPath

		// Check file existence before serving (match TS: 404 if file not present)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			jsonError(rw, "video file not found", http.StatusNotFound)
			return
		}

		ext := strings.ToLower(filepath.Ext(filePath))
		switch ext {
		case ".mp4":
			rw.Header().Set("Content-Type", "video/mp4")
		case ".webm":
			rw.Header().Set("Content-Type", "video/webm")
		case ".ts":
			rw.Header().Set("Content-Type", "video/mp2t")
		case ".mkv":
			rw.Header().Set("Content-Type", "video/x-matroska")
		default:
			rw.Header().Set("Content-Type", "application/octet-stream")
		}

		// Cache headers (match TS: immutable for finished, no-cache for others)
		if job.Status == database.StatusFinished {
			rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			rw.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}

		http.ServeFile(rw, req, filePath)
	})

	// GET /api/jobs/:id/thumbnail — serves the locally-stored thumbnail.
	// Returns 404 when the job has no thumbnail_file (the frontend falls
	// back to the remote thumbnailUrl in that case via the img onerror
	// handler in app.js). Audit reports/web.md — owner request: prefer
	// local-stored thumbnails so the dashboard doesn't keep hitting
	// i.ytimg.com / static-cdn.jtvnw.net for assets we already downloaded.
	r.Get("/api/jobs/{id}/thumbnail", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		if job.ThumbnailFile == "" {
			jsonError(rw, "no thumbnail file", http.StatusNotFound)
			return
		}

		// Resolve path. ThumbnailFile is stored as filepath.Join(outputDir,
		// filenameBase+ext) — usually that's already an absolute path
		// (outputDir is absolute on Windows), but we handle relative
		// inputs by joining with outputDir.
		var thumbCfgOutputDir string
		store.Read(func(c *config.MoomboxConfig) {
			thumbCfgOutputDir = c.Paths.OutputDirectory
		})
		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = thumbCfgOutputDir
		}
		if outputDir == "" {
			outputDir = "./output"
		}

		// ThumbnailFile is stored as filepath.Join(outputDir, basename+ext)
		// at mux time. That can be either absolute (typical when
		// cfg.Paths.OutputDirectory is absolute) or relative (when the
		// config used "./output" or similar). filepath.Abs handles both
		// correctly: absolute paths pass through untouched, relative
		// paths get joined to CWD. The previous IsAbs+Join attempt was
		// double-counting outputDir for the relative case (resulting in
		// `outputDir/outputDir/file.jpg`) and producing 404s that the
		// frontend's onerror handler then misclassified as "fall back to
		// the channel-avatar URL" -- which is what owner saw render as
		// 1:1 letterboxed.
		filePath, err := filepath.Abs(job.ThumbnailFile)
		if err != nil {
			jsonError(rw, "invalid path", http.StatusBadRequest)
			return
		}

		// Path traversal guard against the resolved output dir.
		resolvedPath, ok := validatePathTraversal(filePath, outputDir)
		if !ok {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}
		filePath = resolvedPath

		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			jsonError(rw, "thumbnail not found", http.StatusNotFound)
			return
		}

		switch strings.ToLower(filepath.Ext(filePath)) {
		case ".jpg", ".jpeg":
			rw.Header().Set("Content-Type", "image/jpeg")
		case ".png":
			rw.Header().Set("Content-Type", "image/png")
		case ".webp":
			rw.Header().Set("Content-Type", "image/webp")
		default:
			rw.Header().Set("Content-Type", "application/octet-stream")
		}
		// Day-long cache; the file is content-addressable through the
		// job ID — if the thumbnail changes, it's a new file at the
		// same path, but the file mtime change isn't observed here so
		// keep this conservative.
		rw.Header().Set("Cache-Control", "public, max-age=86400")

		http.ServeFile(rw, req, filePath)
	})

	// GET /api/jobs/:id/segments — returns segments for multi-segment jobs
	r.Get("/api/jobs/{id}/segments", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		segments, err := db.GetSegments(jobID)
		if err != nil {
			jsonError(rw, "failed to get segments", http.StatusInternalServerError)
			return
		}
		if len(segments) == 0 {
			jsonError(rw, "no segments", http.StatusNotFound)
			return
		}

		if job.Status == database.StatusFinished {
			rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			rw.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		jsonResponse(rw, segments)
	})

	// GET /api/jobs/:id/segments/:index/video — serves individual segment video file
	r.Get("/api/jobs/{id}/segments/{index}/video", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		indexStr := chi.URLParam(req, "index")

		segIndex, err := strconv.Atoi(indexStr)
		if err != nil {
			jsonError(rw, "invalid segment index", http.StatusBadRequest)
			return
		}

		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		segments, err := db.GetSegments(jobID)
		if err != nil {
			jsonError(rw, "failed to get segments", http.StatusInternalServerError)
			return
		}

		// Find the segment by index
		var seg *database.Segment
		for i := range segments {
			if segments[i].SegmentIndex == segIndex {
				seg = &segments[i]
				break
			}
		}
		if seg == nil {
			jsonError(rw, "segment not found", http.StatusNotFound)
			return
		}

		if seg.FilePath == "" {
			jsonError(rw, "segment file not available", http.StatusNotFound)
			return
		}

		filePath, err := filepath.Abs(seg.FilePath)
		if err != nil {
			jsonError(rw, "invalid path", http.StatusBadRequest)
			return
		}

		// Path traversal guard: resolve symlinks before prefix check
		var segCfgOutputDir string
		store.Read(func(c *config.MoomboxConfig) {
			segCfgOutputDir = c.Paths.OutputDirectory
		})

		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = segCfgOutputDir
		}
		if outputDir == "" {
			outputDir = "./output"
		}
		resolvedPath, ok := validatePathTraversal(filePath, outputDir)
		if !ok {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}
		filePath = resolvedPath

		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			jsonError(rw, "segment file not found", http.StatusNotFound)
			return
		}

		rw.Header().Set("Content-Type", "video/mp4")
		if job.Status == database.StatusFinished {
			rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			rw.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		http.ServeFile(rw, req, filePath)
	})

	// GET /api/jobs/:id/chat
	r.Get("/api/jobs/{id}/chat", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		if job.ChatFilename == "" {
			jsonError(rw, "no chat file", http.StatusNotFound)
			return
		}

		// Resolve chat file path relative to output directory
		var chatCfgOutputDir string
		store.Read(func(c *config.MoomboxConfig) {
			chatCfgOutputDir = c.Paths.OutputDirectory
		})

		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = chatCfgOutputDir
		}
		if outputDir == "" {
			outputDir = "./output"
		}

		chatPath, err := filepath.Abs(filepath.Join(outputDir, job.ChatFilename))
		if err != nil {
			jsonError(rw, "invalid path", http.StatusBadRequest)
			return
		}

		// Fallback: if chat not found at outputDir+chatFilename, try deriving
		// from outputFile (which stores the full relative path from CWD).
		if _, err := os.Stat(chatPath); os.IsNotExist(err) && job.OutputFile != "" {
			// Derive chat path from outputFile by replacing the video extension
			base := strings.TrimSuffix(job.OutputFile, filepath.Ext(job.OutputFile))
			alt := base + ".chat.json"
			if absAlt, err2 := filepath.Abs(alt); err2 == nil {
				if _, err3 := os.Stat(absAlt); err3 == nil {
					chatPath = absAlt
				}
			}
		}

		// Path traversal guard: resolve symlinks before prefix check
		resolvedChat, ok := validatePathTraversal(chatPath, outputDir)
		if !ok {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}
		chatPath = resolvedChat

		// Check file existence before serving (match TS: 404 for missing file)
		if _, err := os.Stat(chatPath); os.IsNotExist(err) {
			jsonError(rw, "chat file not found", http.StatusNotFound)
			return
		}

		// Read and validate JSON (match TS: 422 for corrupt/unreadable)
		data, err := os.ReadFile(chatPath)
		if err != nil {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		if !json.Valid(data) {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}

		rw.Header().Set("Content-Type", "application/json")
		rw.Write(data)
	})

	// GET /api/jobs/:id/trims
	r.Get("/api/jobs/{id}/trims", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")

		// Verify job exists (match TS: 404 if not found)
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		trims, err := db.GetTrimsForJob(jobID)
		if err != nil {
			jsonError(rw, "failed to get trims", http.StatusInternalServerError)
			return
		}
		if trims == nil {
			trims = []database.TrimRecord{}
		}
		jsonResponse(rw, map[string]any{"trims": trims})
	})

	// GET /api/jobs/:id/logs — per-job log buffer
	r.Get("/api/jobs/{id}/logs", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		// Verify job exists
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		// Per-job logs will be populated by the worker during download.
		// For now return empty array; the worker sets these via db.GetJobLogs().
		logs := db.GetJobLogs(jobID)
		if logs == nil {
			logs = []string{}
		}
		jsonResponse(rw, logs)
	})

	// POST /api/jobs — create a new job
	r.With(rl.Middleware).Post("/api/jobs", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			URL               string   `json:"url"`
			VideoID           string   `json:"videoId"`
			Platform          string   `json:"platform,omitempty"`
			OutputDirectory   string   `json:"outputDirectory,omitempty"`
			AllowNonStream    bool     `json:"allowNonStream,omitempty"`
			SelectedVideoItag *int     `json:"selectedVideoItag,omitempty"`
			SelectedAudioItag *int     `json:"selectedAudioItag,omitempty"`
			StartTime         *float64 `json:"startTime,omitempty"`
			EndTime           *float64 `json:"endTime,omitempty"`
			TwitchType        string   `json:"twitchType,omitempty"`
			QualityPreference string   `json:"quality_preference,omitempty"`
		}

		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		videoID := body.VideoID
		if videoID == "" && body.URL != "" {
			videoID = extractVideoIDFromURL(body.URL)
		}

		if videoID == "" {
			jsonError(rw, "video ID or URL required", http.StatusBadRequest)
			return
		}

		// Cross-field validation (matches TS Zod refinements)
		if body.SelectedVideoItag != nil && body.SelectedAudioItag != nil &&
			*body.SelectedVideoItag == -1 && *body.SelectedAudioItag == -1 {
			jsonError(rw, "cannot skip both video and audio", http.StatusBadRequest)
			return
		}
		if body.StartTime != nil && body.EndTime != nil && *body.EndTime <= *body.StartTime {
			jsonError(rw, "end time must be greater than start time", http.StatusBadRequest)
			return
		}
		if body.OutputDirectory != "" && strings.Contains(body.OutputDirectory, "..") {
			jsonError(rw, "output directory must not contain path traversal", http.StatusBadRequest)
			return
		}

		// Detect platform from URL or explicit body field
		platform := body.Platform
		if platform == "" {
			platform = "youtube"
			if body.URL != "" && strings.Contains(body.URL, "twitch.tv") {
				platform = "twitch"
			}
		}

		now := time.Now().UTC().Format(time.RFC3339)

		if platform == "twitch" && twitchFetcher != nil {
			// Twitch job creation — fetch metadata from Twitch API
			login := strings.ToLower(videoID)
			isVod := body.TwitchType == "vod"

			var (
				jobID           string
				url             string
				title           = login + " — Manual Add"
				channelName     = login
				thumbnailURL    string
				avatarURL       string
				streamStartTime string
				twitchCategory  string
			)

			if isVod {
				jobID = "tw_v" + login
				url = "https://www.twitch.tv/videos/" + login
				meta, err := twitchFetcher.FetchVodMetadata(req.Context(), login)
				if err == nil && meta != nil {
					title = meta.ChannelName + " — " + meta.Title
					channelName = meta.ChannelName
					thumbnailURL = meta.ThumbnailURL
					streamStartTime = meta.StartedAt
					twitchCategory = meta.GameCategory
				}
			} else {
				url = "https://www.twitch.tv/" + login
				meta, err := twitchFetcher.FetchStreamMetadata(req.Context(), login)
				if err == nil && meta != nil && meta.IsLive {
					jobID = "tw_" + meta.StreamID
					title = meta.ChannelName + " — " + meta.Title
					if meta.Title == "" {
						title = meta.ChannelName + " — Live"
					}
					channelName = meta.ChannelName
					thumbnailURL = meta.ThumbnailURL
					avatarURL = meta.AvatarURL
					streamStartTime = meta.StartedAt
					twitchCategory = meta.GameCategory
				} else {
					// UnixNano instead of UnixMilli: two rapid manual adds
					// within the same millisecond (double-click, script)
					// would otherwise collide on the generated jobID and
					// trip the duplicate-check path.
					jobID = fmt.Sprintf("tw_manual_%s_%d", login, time.Now().UnixNano())
				}
			}

			// Check for duplicate
			if active, _ := db.HasActiveJob(jobID); active {
				jsonError(rw, "job already exists", http.StatusConflict)
				return
			}

			job := &database.Job{
				ID:                jobID,
				VideoID:           jobID,
				URL:               url,
				Title:             title,
				ChannelName:       channelName,
				Platform:          "twitch",
				Status:            database.StatusUpcoming,
				ThumbnailURL:      thumbnailURL,
				ChannelAvatarURL:  avatarURL,
				StreamStartTime:   streamStartTime,
				TwitchCategory:    twitchCategory,
				TwitchQuality:     body.QualityPreference,
				QualityPreference: body.QualityPreference,
				IsVod:             isVod,
				ManuallyAdded:     true,
				OutputDirectory:   body.OutputDirectory,
				CreatedAt:         now,
				UpdatedAt:         now,
			}

			added, err := db.AddJob(job)
			if err != nil {
				jsonError(rw, "failed to create job", http.StatusInternalServerError)
				return
			}
			if !added {
				jsonError(rw, "job already exists", http.StatusConflict)
				return
			}
			if w != nil {
				w.EnqueueJob(job.ID)
			}
			if notifier != nil {
				notifier.Send(
					"Twitch Video Added",
					fmt.Sprintf("Manually added: %s", job.Title),
					notifications.TypeInfo,
					[]notifications.Field{
						{Name: "Channel", Value: job.ChannelName, Inline: true},
						{Name: "Stream ID", Value: job.ID, Inline: true},
					},
					notifications.SendOptions{
						URL:       job.URL,
						Thumbnail: job.ThumbnailURL,
						Event:     "added",
					},
				)
			}
			rw.WriteHeader(http.StatusCreated)
			jsonResponse(rw, job)
			return
		}

		// YouTube job creation
		url := body.URL
		if url == "" {
			url = "https://www.youtube.com/watch?v=" + videoID
		}

		// Check for duplicate
		if active, _ := db.HasActiveJob(videoID); active {
			jsonError(rw, "job already exists", http.StatusConflict)
			return
		}

		// Fetch metadata from YouTube (title, channel name) with fallback defaults (match TS)
		title := "Manual Add"
		channelName := "Manual"
		var thumbnailURL string
		if ytFetcher != nil {
			meta, err := ytFetcher.FetchMetadata(req.Context(), videoID)
			if err == nil && meta != nil {
				title = meta.Title
				channelName = meta.ChannelName
				thumbnailURL = meta.ThumbnailURL
			}
		}
		if thumbnailURL == "" {
			thumbnailURL = "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg"
		}

		job := &database.Job{
			ID:                videoID,
			VideoID:           videoID,
			URL:               url,
			Title:             title,
			ChannelName:       channelName,
			ThumbnailURL:      thumbnailURL,
			Status:            database.StatusUpcoming,
			Platform:          platform,
			ManuallyAdded:     true,
			AllowNonStream:    body.AllowNonStream,
			OutputDirectory:   body.OutputDirectory,
			SelectedVideoItag: body.SelectedVideoItag,
			SelectedAudioItag: body.SelectedAudioItag,
			QualityPreference: body.QualityPreference,
			StartTime:         body.StartTime,
			EndTime:           body.EndTime,
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		added, err := db.AddJob(job)
		if err != nil {
			jsonError(rw, "failed to create job", http.StatusInternalServerError)
			return
		}
		if !added {
			jsonError(rw, "job already exists", http.StatusConflict)
			return
		}

		if w != nil {
			w.EnqueueJob(job.ID)
		}

		if notifier != nil {
			fields := []notifications.Field{
				{Name: "Channel", Value: job.ChannelName, Inline: true},
				{Name: "Video ID", Value: videoID, Inline: true},
			}
			if body.SelectedVideoItag != nil {
				label := fmt.Sprintf("itag %d", *body.SelectedVideoItag)
				if *body.SelectedVideoItag == -1 {
					label = "None (audio only)"
				}
				fields = append(fields, notifications.Field{Name: "Video Format", Value: label, Inline: true})
			}
			if body.SelectedAudioItag != nil {
				label := fmt.Sprintf("itag %d", *body.SelectedAudioItag)
				if *body.SelectedAudioItag == -1 {
					label = "None (video only)"
				}
				fields = append(fields, notifications.Field{Name: "Audio Format", Value: label, Inline: true})
			}
			if body.StartTime != nil || body.EndTime != nil {
				startSec := 0.0
				if body.StartTime != nil {
					startSec = *body.StartTime
				}
				startMin := int(startSec) / 60
				startS := int(startSec) % 60
				startStr := fmt.Sprintf("%d:%02d", startMin, startS)
				endStr := "end"
				if body.EndTime != nil {
					endMin := int(*body.EndTime) / 60
					endS := int(*body.EndTime) % 60
					endStr = fmt.Sprintf("%d:%02d", endMin, endS)
				}
				rangeValue := startStr + " - " + endStr
				if body.EndTime != nil {
					dur := *body.EndTime - startSec
					durMin := int(dur) / 60
					durS := int(dur) % 60
					rangeValue = fmt.Sprintf("%s (%d:%02d)", rangeValue, durMin, durS)
				}
				fields = append(fields, notifications.Field{Name: "Time Range", Value: rangeValue})
			}
			notifier.Send(
				"Video Added",
				fmt.Sprintf("Manually added: %s", job.Title),
				notifications.TypeInfo,
				fields,
				notifications.SendOptions{
					URL:       job.URL,
					Thumbnail: job.ThumbnailURL,
					Event:     "added",
				},
			)
		}

		rw.WriteHeader(http.StatusCreated)
		jsonResponse(rw, job)
	})

	// POST /api/jobs/:id/cancel
	r.Post("/api/jobs/{id}/cancel", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		// Only allow cancellation from specific active states (match TypeScript)
		switch job.Status {
		case database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing, database.StatusCookies:
			// OK to cancel
		default:
			jsonError(rw, "Job cannot be cancelled in current state", http.StatusBadRequest)
			return
		}

		db.UpdateJobFields(jobID, map[string]any{
			"status": database.StatusCancelled,
		})

		if w != nil {
			w.CancelJob(jobID)
		}

		if notifier != nil {
			idLabel := "Video ID"
			if job.Platform == "twitch" {
				idLabel = "Stream ID"
			}
			notifyURL := job.URL
			if notifyURL == "" {
				notifyURL = "https://www.youtube.com/watch?v=" + job.VideoID
			}
			notifier.Send(
				"Job Cancelled",
				fmt.Sprintf("Cancelled: %s", job.Title),
				notifications.TypeCancelled,
				[]notifications.Field{
					{Name: "Channel", Value: job.ChannelName, Inline: true},
					{Name: idLabel, Value: job.VideoID, Inline: true},
				},
				notifications.SendOptions{
					URL:       notifyURL,
					Thumbnail: job.ThumbnailURL,
					Event:     "cancelled",
				},
			)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/jobs/:id/retry — backward compat, delegates to ReinitializeJob
	r.Post("/api/jobs/{id}/retry", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		// Only allow retry from terminal/cookies states
		switch job.Status {
		case database.StatusError, database.StatusCancelled, database.StatusCookies:
			// OK to retry
		default:
			jsonError(rw, "Job cannot be retried in current state", http.StatusBadRequest)
			return
		}

		if w != nil {
			w.ReinitializeJob(jobID)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/jobs/:id/resume — resume a YouTube job preserving staging files
	r.Post("/api/jobs/{id}/resume", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		switch job.Status {
		case database.StatusError, database.StatusCancelled, database.StatusCookies:
			// OK
		default:
			jsonError(rw, "Job cannot be resumed in current state", http.StatusBadRequest)
			return
		}

		if job.Platform != "youtube" {
			jsonError(rw, "Resume is only supported for YouTube jobs", http.StatusBadRequest)
			return
		}

		var stagingBase string
		store.Read(func(c *config.MoomboxConfig) {
			stagingBase = c.Paths.EffectiveStagingDir()
		})

		if !worker.HasStagingFiles(stagingBase, jobID) {
			jsonError(rw, "No staging files found — use Reinitialize instead", http.StatusBadRequest)
			return
		}

		if w != nil {
			w.ResumeJob(jobID)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/jobs/:id/reinitialize — reset job to fresh state and re-enqueue
	r.Post("/api/jobs/{id}/reinitialize", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		switch job.Status {
		case database.StatusError, database.StatusCancelled, database.StatusCookies:
			// OK
		default:
			jsonError(rw, "Job cannot be reinitialized in current state", http.StatusBadRequest)
			return
		}

		if w != nil {
			w.ReinitializeJob(jobID)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/jobs/:id/mux — force-mux from staging files
	r.Post("/api/jobs/{id}/mux", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		switch job.Status {
		case database.StatusError, database.StatusCancelled:
			// OK
		default:
			jsonError(rw, "Job cannot be muxed in current state", http.StatusBadRequest)
			return
		}

		var stagingBase string
		store.Read(func(c *config.MoomboxConfig) {
			stagingBase = c.Paths.EffectiveStagingDir()
		})

		if !worker.HasSegmentFiles(stagingBase, jobID) {
			jsonError(rw, "No segment files found in staging", http.StatusBadRequest)
			return
		}

		if w != nil {
			if err := w.MuxJob(jobID); err != nil {
				jsonError(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/jobs/:id/open-folder — loopback only (match TS: resolve via outputDir + filename)
	r.With(web.LoopbackOnly).Post("/api/jobs/{id}/open-folder", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "Job not found", http.StatusNotFound)
			return
		}

		var openCfgOutputDir, openCfgStagingDir string
		store.Read(func(c *config.MoomboxConfig) {
			openCfgOutputDir = c.Paths.OutputDirectory
			openCfgStagingDir = c.Paths.EffectiveStagingDir()
		})

		var dir string
		if job.Filename != "" {
			// Resolve path: join output directory + filename, then get parent dir (match TS)
			outputDir := job.OutputDirectory
			if outputDir == "" {
				outputDir = openCfgOutputDir
			}
			filePath, err := filepath.Abs(filepath.Join(outputDir, job.Filename))
			if err != nil {
				jsonError(rw, "invalid file path", http.StatusBadRequest)
				return
			}

			// Path traversal guard: resolve symlinks before prefix check
			resolvedPath, ok := validatePathTraversal(filePath, outputDir)
			if !ok {
				jsonError(rw, "access denied", http.StatusForbidden)
				return
			}

			dir = filepath.Dir(resolvedPath)
			if _, err := os.Stat(dir); err != nil {
				jsonError(rw, "Output folder not found", http.StatusNotFound)
				return
			}
		} else {
			// Fall back to staging directory for active jobs
			stagingDir, err := filepath.Abs(filepath.Join(openCfgStagingDir, job.ID))
			if err != nil {
				jsonError(rw, "invalid staging path", http.StatusBadRequest)
				return
			}
			if _, err := os.Stat(stagingDir); err != nil {
				jsonError(rw, "Staging folder not found", http.StatusNotFound)
				return
			}
			dir = stagingDir
		}

		cmd := exec.Command("explorer", dir)
		if err := cmd.Start(); err != nil {
			jsonError(rw, "failed to open folder", http.StatusInternalServerError)
			return
		}
		// Release the OS process handle immediately — we don't call
		// cmd.Wait() (explorer.exe detaches and runs independently),
		// and without Release() the handle leaks until the Moombox
		// process exits, accumulating for every open-folder request.
		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}

		jsonResponse(rw, map[string]bool{"success": true})
	})

	// DELETE /api/jobs/:id
	r.Delete("/api/jobs/{id}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		// If the job is in an active state, auto-cancel and wait for the worker
		// to drain before removing the row. Cancel-then-delete is a single
		// user-facing action so the caller does not need to issue two requests.
		switch job.Status {
		case database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing:
			if w != nil {
				w.CancelJob(jobID)
				if !w.WaitForJobExit(jobID, 5*time.Second) {
					slog.Warn("delete: job did not exit within timeout; removing row anyway", "jobID", jobID)
				}
			}
		}

		if err := db.DeleteJob(jobID); err != nil {
			jsonError(rw, "failed to delete job", http.StatusInternalServerError)
			return
		}

		// Clean up per-job logs (match TS: ctx.jobLogs.delete(job.id))
		db.ClearJobLogs(jobID)

		// Clean up WebSocket throttle state for deleted job
		if wsHub != nil {
			wsHub.CleanupJob(jobID)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

}

var (
	youtubeURLRe = regexp.MustCompile(`(?:youtube\.com/watch\?v=|youtu\.be/|youtube\.com/live/)([a-zA-Z0-9_-]{11})`)
	twitchURLRe  = regexp.MustCompile(`(?:twitch\.tv/)([a-zA-Z0-9_]+)`)
	bracketIDRe  = regexp.MustCompile(`\[([a-zA-Z0-9_-]{11})\]`)
)

func extractVideoIDFromURL(url string) string {
	for _, re := range []*regexp.Regexp{youtubeURLRe, twitchURLRe} {
		if m := re.FindStringSubmatch(url); m != nil {
			return m[1]
		}
	}
	return ""
}

// FormatRoutesDeps holds dependencies for format routes.
type FormatRoutesDeps struct {
	DB  *database.Database
	Cfg *config.MoomboxConfig
	YT  interface {
		GetFormats(ctx context.Context, videoID string) (map[string]any, error)
	}
}

// FormatRoutes registers format-related API routes.
func FormatRoutes(r chi.Router, deps *FormatRoutesDeps) {
	// GET /api/formats/:videoId
	r.Get("/api/formats/{videoId}", func(rw http.ResponseWriter, req *http.Request) {
		videoID := chi.URLParam(req, "videoId")
		if !utils.IsVideoID(videoID) {
			jsonError(rw, "invalid video ID", http.StatusBadRequest)
			return
		}

		if deps.YT == nil {
			jsonError(rw, "YouTube service not available", http.StatusServiceUnavailable)
			return
		}

		result, err := deps.YT.GetFormats(req.Context(), videoID)
		if err != nil {
			jsonError(rw, "failed to get formats", http.StatusInternalServerError)
			return
		}

		jsonResponse(rw, result)
	})
}

// Helper functions

// validatePathTraversal resolves symlinks on both filePath and outputDir, then
// checks that filePath is inside outputDir. Returns the resolved file path on
// success, or an empty string and false if the check fails.
func validatePathTraversal(filePath, outputDir string) (string, bool) {
	resolvedOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(resolvedOutputDir); err == nil {
		resolvedOutputDir = real
	}
	if real, err := filepath.EvalSymlinks(filePath); err == nil {
		filePath = real
	}
	if !strings.HasPrefix(filePath, resolvedOutputDir+string(filepath.Separator)) {
		return "", false
	}
	return filePath, true
}

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// StatusRouteDeps holds dependencies for the status route.
type StatusRouteDeps struct {
	Cfg                        *config.MoomboxConfig
	Version                    string
	StartTime                  time.Time
	GetCookieStatus            func() map[string]any
	GetTwitchAuthStatus        func() map[string]any
	GetAutoCookieReloginNeeded func() any
	GetActivePlatforms         func() map[string]bool
	GetNextFeedCheck           func() int64
	GetNextDecapiCheck         func() int64
	GetNextTwitchCheck         func() int64
}

// StatusRoute returns the server status.
func StatusRoute(r chi.Router, deps *StatusRouteDeps) {
	r.Get("/api/status", func(rw http.ResponseWriter, req *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		resp := map[string]any{
			"status":    "running",
			"uptime":    time.Since(deps.StartTime).Seconds(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"memory": map[string]any{
				"rss":        mem.Sys / 1048576,
				"heapUsed":   mem.HeapAlloc / 1048576,
				"heapTotal":  mem.HeapSys / 1048576,
				"external":   mem.MSpanSys / 1048576,
				"goroutines": runtime.NumGoroutine(),
			},
			"version": deps.Version,
		}

		// Update status from shared atomic
		if ui := SharedUpdateInfo.Load(); ui != nil {
			resp["updateAvailable"] = map[string]any{
				"version":          ui.Version,
				"tagName":          ui.TagName,
				"releaseNotes":     ui.ReleaseNotes,
				"releaseNotesHtml": ui.ReleaseNotesHtml,
				"publishedAt":      ui.PublishedAt,
			}
		}

		// Disk status from shared atomic
		if ds := SharedDiskStatus.Load(); ds != nil {
			resp["disk"] = map[string]any{
				"free":      ds.Free,
				"total":     ds.Total,
				"usedPct":   ds.UsedPct,
				"warnLevel": ds.WarnLevel,
			}
		}

		if deps.GetActivePlatforms != nil {
			resp["activePlatforms"] = deps.GetActivePlatforms()
		}
		if deps.GetCookieStatus != nil {
			resp["cookieStatus"] = deps.GetCookieStatus()
		}
		if deps.GetTwitchAuthStatus != nil {
			resp["twitchAuthStatus"] = deps.GetTwitchAuthStatus()
		}
		if deps.GetAutoCookieReloginNeeded != nil {
			resp["autoCookieReloginRequired"] = deps.GetAutoCookieReloginNeeded()
		}
		if deps.GetNextFeedCheck != nil {
			resp["nextFeedCheck"] = deps.GetNextFeedCheck()
		}
		if deps.GetNextDecapiCheck != nil {
			resp["nextDecapiCheck"] = deps.GetNextDecapiCheck()
		}
		if deps.GetNextTwitchCheck != nil {
			resp["nextTwitchCheck"] = deps.GetNextTwitchCheck()
		}

		jsonResponse(rw, resp)
	})
}

// LogRoutes registers log-related API routes.
func LogRoutes(r chi.Router, getRecentLogs func() []string) {
	// logRouteMaxLines caps the number of log lines any single call to
	// GET /api/logs may return. The backing ring buffer can be larger
	// (or a different implementation might not cap at all), but emitting
	// thousands of lines in one JSON response to a low-bandwidth client
	// would stall the response pipeline. 500 comfortably covers the
	// recent-events UX the frontend needs.
	const logRouteMaxLines = 500
	r.Get("/api/logs", func(rw http.ResponseWriter, req *http.Request) {
		logs := getRecentLogs()
		if len(logs) > logRouteMaxLines {
			logs = logs[len(logs)-logRouteMaxLines:]
		}
		jsonResponse(rw, logs)
	})
}

// RestartRoute registers the restart endpoint (loopback only).
// The onRestart callback is invoked after the HTTP response is sent.
// It should spawn a new process and then trigger graceful shutdown.
func RestartRoute(r chi.Router, onRestart func()) {
	r.With(web.LoopbackOnly).Post("/api/restart", func(rw http.ResponseWriter, req *http.Request) {
		jsonResponse(rw, map[string]any{"success": true, "message": "Restarting..."})

		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic("restart handler", r)
				}
			}()
			time.Sleep(500 * time.Millisecond)
			if onRestart != nil {
				onRestart()
			}
		}()
	})
}
