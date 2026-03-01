// Package routes provides HTTP route handlers for the Moombox API.
package routes

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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
	StreamID        string
	Title           string
	ChannelName     string
	ThumbnailURL    string
	AvatarURL       string
	StartedAt       string
	GameCategory    string
	IsLive          bool
}

// filterJobsByAge splits jobs into active vs archived based on the
// hide_finished_age_days config setting. Non-finished jobs are always active.
// Finished jobs are archived when their age exceeds the threshold.
// If hideAgeDays is 0, all finished jobs are immediately archived.
func filterJobsByAge(jobs []*database.Job, archived bool, cfg *config.MoomboxConfig) []*database.Job {
	hideAgeDays := cfg.Monitors.HideFinishedAgeDays.Value

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
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
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

// JobRoutes registers job-related API routes.
func JobRoutes(r chi.Router, db *database.Database, cfg *config.MoomboxConfig, w *worker.DownloadWorker, rl *web.RateLimiter, twitchFetcher TwitchMetadataFetcher, ytFetcher YouTubeMetadataFetcher, notifier *notifications.Manager) {
	// GET /api/v1/jobs
	r.Get("/api/v1/jobs", func(rw http.ResponseWriter, req *http.Request) {
		jobs, err := db.GetAllJobs(false)
		if err != nil {
			jsonError(rw, "failed to get jobs", http.StatusInternalServerError)
			return
		}

		// Filter out finished jobs older than hide_finished_age_days
		filtered := filterJobsByAge(jobs, false, cfg)
		sendPaginated(rw, req, filtered)
	})

	// GET /api/v1/jobs/archived — finished jobs older than hide_finished_age_days
	r.Get("/api/v1/jobs/archived", func(rw http.ResponseWriter, req *http.Request) {
		jobs, err := db.GetAllJobs(true)
		if err != nil {
			jsonError(rw, "failed to get jobs", http.StatusInternalServerError)
			return
		}

		// Get only finished jobs that are older than the age threshold
		archived := filterJobsByAge(jobs, true, cfg)

		// Sort by updatedAt descending (most recent first)
		sort.Slice(archived, func(i, j int) bool {
			return archived[i].UpdatedAt > archived[j].UpdatedAt
		})

		sendPaginated(rw, req, archived)
	})

	// GET /api/v1/jobs/:id
	r.Get("/api/v1/jobs/{id}", func(rw http.ResponseWriter, req *http.Request) {
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

		jsonResponse(rw, job)
	})

	// GET /api/v1/jobs/:id/video — range-request video streaming
	r.Get("/api/v1/jobs/{id}/video", func(rw http.ResponseWriter, req *http.Request) {
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
		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = cfg.Paths.OutputDirectory
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

		// Path traversal guard: resolved path must be within output directory
		resolvedOutputDir, err := filepath.Abs(outputDir)
		if err != nil {
			jsonError(rw, "invalid output directory", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(filePath, resolvedOutputDir+string(filepath.Separator)) {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}

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

	// GET /api/v1/jobs/:id/chat
	r.Get("/api/v1/jobs/{id}/chat", func(rw http.ResponseWriter, req *http.Request) {
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
		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = cfg.Paths.OutputDirectory
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

		// Path traversal guard
		resolvedOutputDir, err := filepath.Abs(outputDir)
		if err != nil {
			jsonError(rw, "invalid output directory", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(chatPath, resolvedOutputDir+string(filepath.Separator)) {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}

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

	// GET /api/v1/jobs/:id/trims
	r.Get("/api/v1/jobs/{id}/trims", func(rw http.ResponseWriter, req *http.Request) {
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

	// GET /api/v1/jobs/:id/logs — per-job log buffer
	r.Get("/api/v1/jobs/{id}/logs", func(rw http.ResponseWriter, req *http.Request) {
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

	// POST /api/v1/jobs — create a new job
	r.With(rl.Middleware).Post("/api/v1/jobs", func(rw http.ResponseWriter, req *http.Request) {
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
					jobID = fmt.Sprintf("tw_manual_%s_%d", login, time.Now().UnixMilli())
				}
			}

			// Check for duplicate
			if active, _ := db.HasActiveJob(jobID); active {
				jsonError(rw, "job already exists", http.StatusConflict)
				return
			}

			job := &database.Job{
				ID:               jobID,
				VideoID:          jobID,
				URL:              url,
				Title:            title,
				ChannelName:      channelName,
				Platform:         "twitch",
				Status:           database.StatusUpcoming,
				ThumbnailURL:     thumbnailURL,
				ChannelAvatarURL: avatarURL,
				StreamStartTime:  streamStartTime,
				TwitchCategory:   twitchCategory,
				TwitchQuality:    body.QualityPreference,
				IsVod:            isVod,
				ManuallyAdded:    true,
				OutputDirectory:  body.OutputDirectory,
				CreatedAt:        now,
				UpdatedAt:        now,
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

	// POST /api/v1/jobs/:id/cancel
	r.Post("/api/v1/jobs/{id}/cancel", func(rw http.ResponseWriter, req *http.Request) {
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

	// POST /api/v1/jobs/:id/retry
	r.Post("/api/v1/jobs/{id}/retry", func(rw http.ResponseWriter, req *http.Request) {
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

		db.UpdateJobFields(jobID, map[string]any{
			"status":   database.StatusUpcoming,
			"error":    "",
			"progress": "",
			"percent":  0,
		})

		if w != nil {
			w.EnqueueJob(jobID)
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/v1/jobs/:id/open-folder — loopback only (match TS: resolve via outputDir + filename)
	r.With(web.LoopbackOnly).Post("/api/v1/jobs/{id}/open-folder", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "Job not found", http.StatusNotFound)
			return
		}

		var dir string
		if job.Filename != "" {
			// Resolve path: join output directory + filename, then get parent dir (match TS)
			outputDir := job.OutputDirectory
			if outputDir == "" {
				outputDir = cfg.Paths.OutputDirectory
			}
			filePath, err := filepath.Abs(filepath.Join(outputDir, job.Filename))
			if err != nil {
				jsonError(rw, "invalid file path", http.StatusBadRequest)
				return
			}

			// Path traversal guard (match TS)
			resolvedOutputDir, err := filepath.Abs(outputDir)
			if err != nil {
				jsonError(rw, "invalid output directory", http.StatusInternalServerError)
				return
			}
			if !strings.HasPrefix(filePath, resolvedOutputDir+string(filepath.Separator)) {
				jsonError(rw, "Access denied", http.StatusForbidden)
				return
			}

			dir = filepath.Dir(filePath)
		} else {
			// Fall back to staging directory for active jobs
			stagingBase := cfg.Paths.StagingDirectory
			if stagingBase == "" {
				stagingBase = "./staging"
			}
			stagingDir, err := filepath.Abs(filepath.Join(stagingBase, job.ID))
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

		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "windows":
			cmd = exec.Command("explorer", dir)
		case "darwin":
			cmd = exec.Command("open", dir)
		default:
			cmd = exec.Command("xdg-open", dir)
		}
		cmd.Start()

		jsonResponse(rw, map[string]bool{"success": true})
	})

	// DELETE /api/v1/jobs/:id
	r.Delete("/api/v1/jobs/{id}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		// Only allow deletion of terminal states + COOKIES (matching TypeScript)
		switch job.Status {
		case database.StatusFinished, database.StatusError, database.StatusCancelled, database.StatusCookies:
			// Allowed
		default:
			jsonError(rw, "can only delete finished, error, cancelled, or cookies jobs", http.StatusBadRequest)
			return
		}

		if err := db.DeleteJob(jobID); err != nil {
			jsonError(rw, "failed to delete job", http.StatusInternalServerError)
			return
		}

		// Clean up per-job logs (match TS: ctx.jobLogs.delete(job.id))
		db.ClearJobLogs(jobID)

		jsonResponse(rw, map[string]any{"success": true})
	})

	// Note: /api/ → /api/v1/ aliasing handled by APIAliasMiddleware
}

var (
	youtubeURLRe  = regexp.MustCompile(`(?:youtube\.com/watch\?v=|youtu\.be/|youtube\.com/live/)([a-zA-Z0-9_-]{11})`)
	twitchURLRe   = regexp.MustCompile(`(?:twitch\.tv/)([a-zA-Z0-9_]+)`)
	bracketIDRe   = regexp.MustCompile(`\[([a-zA-Z0-9_-]{11})\]`)
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
	// GET /api/v1/formats/:videoId
	r.Get("/api/v1/formats/{videoId}", func(rw http.ResponseWriter, req *http.Request) {
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
			jsonError(rw, "failed to get formats: "+err.Error(), http.StatusInternalServerError)
			return
		}

		jsonResponse(rw, result)
	})
}

// Helper functions

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
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
	r.Get("/api/v1/status", func(rw http.ResponseWriter, req *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		resp := map[string]any{
			"status":    "running",
			"uptime":    time.Since(deps.StartTime).Seconds(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"memory": map[string]any{
				"rss":       mem.Sys / 1048576,
				"heapUsed":  mem.HeapAlloc / 1048576,
				"heapTotal": mem.HeapSys / 1048576,
				"external":  mem.MSpanSys / 1048576,
				"goroutines": runtime.NumGoroutine(),
			},
			"version": deps.Version,
		}

		// Update status from shared atomic
		if ui := SharedUpdateInfo.Load(); ui != nil {
			resp["updateAvailable"] = map[string]any{
				"version":      ui.Version,
				"tagName":      ui.TagName,
				"releaseNotes": ui.ReleaseNotes,
				"publishedAt":  ui.PublishedAt,
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

// ConfigRoutesCallbacks contains optional callbacks invoked when config changes require hot-reload.
type ConfigRoutesCallbacks struct {
	// OnLogLevelChange is called when the log_level config field changes.
	OnLogLevelChange func(level string)
	// OnMaxParallelChange is called when num_parallel_downloads changes.
	OnMaxParallelChange func(n int)
	// OnHideFinishedAgeChanged is called when hide_finished_age_days changes,
	// so callers can re-broadcast the job list with updated archive thresholds.
	OnHideFinishedAgeChanged func()
	// OnChannelChange is called when channels are added, updated, or removed,
	// so monitors can re-evaluate their channel lists immediately.
	OnChannelChange func()
}

// isSafePath validates that a path doesn't contain traversal or absolute paths.
// Matches TypeScript safePathSchema.
func isSafePath(p string) bool {
	if p == "" {
		return true
	}
	if strings.Contains(p, "..") {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	// Check Windows drive letter (e.g., C:)
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		return false
	}
	return true
}

// validateConfigUpdates validates the config update map against TypeScript Zod
// schema constraints. Returns a map of field→error messages (empty if valid).
// Matches TypeScript updateConfigSchema constraints exactly.
func validateConfigUpdates(updates map[string]any) map[string]string {
	errs := make(map[string]string)

	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			if v < 1 || v > 65535 {
				errs["network.port"] = "port must be between 1 and 65535"
			}
		}
		if v, ok := net["network_access"].(string); ok {
			switch v {
			case "localhost", "lan", "external":
			default:
				errs["network.network_access"] = "network_access must be localhost, lan, or external"
			}
		}
		if v, ok := net["tls_cert_path"].(string); ok {
			if v != "" && !isSafePath(v) {
				errs["network.tls_cert_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := net["tls_key_path"].(string); ok {
			if v != "" && !isSafePath(v) {
				errs["network.tls_key_path"] = "Path cannot contain .. or be absolute"
			}
		}
	}

	// Paths sub-fields
	if paths, ok := updates["paths"].(map[string]any); ok {
		if v, ok := paths["log_file_path"].(string); ok {
			if !isSafePath(v) {
				errs["paths.log_file_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["database_path"].(string); ok {
			if !isSafePath(v) {
				errs["paths.database_path"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["output_directory"].(string); ok {
			if !isSafePath(v) {
				errs["paths.output_directory"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := paths["staging_directory"].(string); ok {
			if !isSafePath(v) {
				errs["paths.staging_directory"] = "Path cannot contain .. or be absolute"
			}
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			switch strings.ToUpper(v) {
			case "DEBUG", "INFO", "WARN", "ERROR":
			default:
				errs["logs.log_level"] = "log_level must be DEBUG, INFO, WARN, or ERROR"
			}
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			if v < 1024 || v > 1073741824 {
				errs["logs.log_max_file_size"] = "log_max_file_size must be between 1024 and 1073741824"
			}
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			if v < 1 || v > 100 {
				errs["logs.log_max_files"] = "log_max_files must be between 1 and 100"
			}
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["max_feed_items"].(float64); ok {
			if v < 1 || v > 100 {
				errs["monitors.max_feed_items"] = "max_feed_items must be between 1 and 100"
			}
		}
		if v, ok := mon["decapi_check_interval"].(float64); ok {
			if v < 15 || v > 3600 {
				errs["monitors.decapi_check_interval"] = "decapi_check_interval must be between 15 and 3600"
			}
		}
		if v, ok := mon["twitch_check_interval"].(float64); ok {
			if v < 5 || v > 3600 {
				errs["monitors.twitch_check_interval"] = "twitch_check_interval must be between 5 and 3600"
			}
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["output_template"].(string); ok {
			if len(v) > 500 {
				errs["downloader.output_template"] = "output_template must be at most 500 characters"
			}
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			if v < 1 || v > 10 {
				errs["downloader.num_parallel_downloads"] = "num_parallel_downloads must be between 1 and 10"
			}
		}
		if v, ok := dl["max_video_resolution"].(float64); ok {
			if v < 1 {
				errs["downloader.max_video_resolution"] = "max_video_resolution must be at least 1"
			}
		}
	}

	// Disk sub-fields
	if dk, ok := updates["disk"].(map[string]any); ok {
		if v, ok := dk["disk_warn_percent"].(float64); ok {
			if v < 1 || v > 100 {
				errs["disk.disk_warn_percent"] = "disk_warn_percent must be between 1 and 100"
			}
		}
		if v, ok := dk["disk_critical_percent"].(float64); ok {
			if v < 1 || v > 100 {
				errs["disk.disk_critical_percent"] = "disk_critical_percent must be between 1 and 100"
			}
		}
	}

	// Cookies sub-fields
	if ck, ok := updates["cookies"].(map[string]any); ok {
		if v, ok := ck["cookie_file"].(string); ok {
			if !isSafePath(v) {
				errs["cookies.cookie_file"] = "Path cannot contain .. or be absolute"
			}
		}
		if v, ok := ck["browser_profile_dir"].(string); ok {
			if !isSafePath(v) {
				errs["cookies.browser_profile_dir"] = "Path cannot contain .. or be absolute"
			}
		}
	}

	return errs
}

// applyConfigUpdates applies allowlisted config fields from a snake_case map
// to the config struct. Used by both PUT /config and POST /setup/complete.
// Matches TypeScript updateConfigSchema field names exactly.
func applyConfigUpdates(cfg *config.MoomboxConfig, updates map[string]any) {
	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			cfg.Network.Port = int(v)
		}
		if v, ok := net["network_access"].(string); ok {
			cfg.Network.NetworkAccess = v
		}
		if v, ok := net["https_enabled"].(bool); ok {
			cfg.Network.HTTPSEnabled = v
		}
		if v, ok := net["tls_cert_path"].(string); ok {
			cfg.Network.TLSCertPath = v
		}
		if v, ok := net["tls_key_path"].(string); ok {
			cfg.Network.TLSKeyPath = v
		}
	}

	// Paths sub-fields
	if paths, ok := updates["paths"].(map[string]any); ok {
		if v, ok := paths["log_file_path"].(string); ok {
			cfg.Paths.LogFilePath = v
		}
		if v, ok := paths["database_path"].(string); ok {
			cfg.Paths.DatabasePath = v
		}
		if v, ok := paths["output_directory"].(string); ok {
			cfg.Paths.OutputDirectory = v
		}
		if v, ok := paths["staging_directory"].(string); ok {
			cfg.Paths.StagingDirectory = v
		}
		if v, ok := paths["ffmpeg_path"].(string); ok {
			cfg.Paths.FfmpegPath = v
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			cfg.Logs.LogLevel = v
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			cfg.Logs.LogMaxFileSize = int(v)
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			cfg.Logs.LogMaxFiles = int(v)
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["max_feed_items"].(float64); ok {
			cfg.Monitors.MaxFeedItems = int(v)
		}
		if v, ok := mon["feed_check_interval"].(float64); ok {
			cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: v}
		} else if vs, ok := mon["feed_check_interval"].(string); ok {
			cfg.Monitors.FeedCheckInterval = config.ParseFlexDuration(vs, "minutes", cfg.Monitors.FeedCheckInterval.Value)
		}
		if v, ok := mon["decapi_check_interval"].(float64); ok {
			n := int(v)
			cfg.Monitors.DecapiCheckInterval = &n
		}
		if v, ok := mon["twitch_check_interval"].(float64); ok {
			n := int(v)
			cfg.Monitors.TwitchCheckInterval = &n
		}
		if v, ok := mon["hide_finished_age_days"].(float64); ok {
			cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: v}
		} else if vs, ok := mon["hide_finished_age_days"].(string); ok {
			cfg.Monitors.HideFinishedAgeDays = config.ParseFlexDuration(vs, "days", cfg.Monitors.HideFinishedAgeDays.Value)
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["max_video_resolution"].(float64); ok {
			cfg.Downloader.MaxVideoResolution = int(v)
		}
		if v, ok := dl["output_template"].(string); ok {
			cfg.Downloader.OutputTemplate = v
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			cfg.Downloader.NumParallelDownloads = int(v)
		}
		if v, ok := dl["download_chat"].(bool); ok {
			cfg.Downloader.DownloadChat = v
		}
		if v, ok := dl["prefer_60fps"].(bool); ok {
			cfg.Downloader.Prefer60fps = v
		}
		if v, ok := dl["segment_retry_delay_cap"].(float64); ok {
			cfg.Downloader.SegmentRetryDelayCap = int(v)
		}
		if v, ok := dl["segment_live_check_retries"].(float64); ok {
			cfg.Downloader.SegmentLiveCheckRetries = int(v)
		}
		if v, ok := dl["po_token"].(string); ok {
			cfg.Downloader.PoToken = v
		}
		if v, ok := dl["visitor_data"].(string); ok {
			cfg.Downloader.VisitorData = v
		}
		if v, ok := dl["pot_provider_url"].(string); ok {
			cfg.Downloader.PotProviderURL = v
		}
	}

	// Cookies
	if ck, ok := updates["cookies"].(map[string]any); ok {
		if v, ok := ck["cookie_file"].(string); ok {
			cfg.Cookies.CookieFile = v
		}
		if v, ok := ck["auto_enabled"].(bool); ok {
			cfg.Cookies.AutoEnabled = v
		}
		if v, ok := ck["browser_profile_dir"].(string); ok {
			cfg.Cookies.BrowserProfileDir = v
		}
		if v, ok := ck["platforms"].([]any); ok {
			var platforms []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					platforms = append(platforms, s)
				}
			}
			cfg.Cookies.Platforms = platforms
		}
		if v, ok := ck["active_platforms"].([]any); ok {
			var activePlatforms []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					activePlatforms = append(activePlatforms, s)
				}
			}
			cfg.Cookies.ActivePlatforms = activePlatforms
		}
		if v, ok := ck["refresh_interval"].(float64); ok {
			cfg.Cookies.RefreshInterval = config.FlexDuration{Value: v}
		} else if vs, ok := ck["refresh_interval"].(string); ok {
			cfg.Cookies.RefreshInterval = config.ParseFlexDuration(vs, "minutes", cfg.Cookies.RefreshInterval.Value)
		}
	}

	// Disk
	if dk, ok := updates["disk"].(map[string]any); ok {
		if v, ok := dk["disk_warn_percent"].(float64); ok {
			cfg.Disk.WarnPercent = int(v)
		}
		if v, ok := dk["disk_critical_percent"].(float64); ok {
			cfg.Disk.CriticalPercent = int(v)
		}
	}

	// Updates
	if upd, ok := updates["updates"].(map[string]any); ok {
		if v, ok := upd["auto_check_updates"].(bool); ok {
			cfg.Updates.AutoCheckUpdates = v
		}
	}

	// Notifications
	if notifs, ok := updates["notifications"].([]any); ok {
		var configs []config.NotificationConfig
		for _, n := range notifs {
			if nm, ok := n.(map[string]any); ok {
				nc := config.NotificationConfig{}
				if v, ok := nm["url"].(string); ok {
					nc.URL = v
				}
				if v, ok := nm["tags"].([]any); ok {
					for _, t := range v {
						if s, ok := t.(string); ok {
							nc.Tags = append(nc.Tags, s)
						}
					}
				}
				if v, ok := nm["events"].([]any); ok {
					for _, e := range v {
						if s, ok := e.(string); ok {
							nc.Events = append(nc.Events, s)
						}
					}
				}
				configs = append(configs, nc)
			}
		}
		cfg.Notifications = configs
	}

	// Channels
	if chs, ok := updates["channels"].([]any); ok {
		data, _ := json.Marshal(chs)
		var channels []config.ChannelConfig
		if json.Unmarshal(data, &channels) == nil {
			cfg.Channels = channels
		}
	}
}

// ConfigRoutes registers config-related API routes.
func ConfigRoutes(r chi.Router, cfg *config.MoomboxConfig, saveConfig func(*config.MoomboxConfig) error, callbacks *ConfigRoutesCallbacks) {
	// GET /api/v1/config
	r.Get("/api/v1/config", func(rw http.ResponseWriter, req *http.Request) {
		// Clone config and return with hasPassword injected.
		// PasswordHash has json:"-" tag so it's already excluded from marshaling.
		jsonResponse(rw, struct {
			*config.MoomboxConfig
			HasPassword bool `json:"hasPassword"`
		}{
			MoomboxConfig: cfg,
			HasPassword:   cfg.Network.PasswordHash != "",
		})
	})

	// PUT /api/v1/config
	r.Put("/api/v1/config", func(rw http.ResponseWriter, req *http.Request) {
		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Validate with Zod-equivalent schema constraints (match TS updateConfigSchema)
		if validationErrs := validateConfigUpdates(updates); len(validationErrs) > 0 {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(rw).Encode(map[string]any{
				"error":   "Validation failed",
				"details": validationErrs,
			})
			return
		}

		// Prevent enabling external access without a password
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfg.Network.PasswordHash == "" {
					jsonError(rw, "A password must be set before enabling external access. Go to Settings \u2192 Security.", http.StatusBadRequest)
					return
				}
			}
		}

		// Snapshot values that need hot-reload comparison after save.
		oldLogLevel := cfg.Logs.LogLevel
		oldNumParallel := cfg.Downloader.NumParallelDownloads
		oldHideAge := cfg.Monitors.HideFinishedAgeDays.Value

		// Apply allowlisted updates — matches TypeScript updateConfigSchema (snake_case)
		applyConfigUpdates(cfg, updates)

		// Persist to disk
		if saveConfig != nil {
			if err := saveConfig(cfg); err != nil {
				jsonError(rw, "failed to save config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Hot-reload runtime-reloadable settings
		if callbacks != nil {
			if cfg.Logs.LogLevel != oldLogLevel && callbacks.OnLogLevelChange != nil {
				callbacks.OnLogLevelChange(cfg.Logs.LogLevel)
			}
			if cfg.Downloader.NumParallelDownloads != oldNumParallel && callbacks.OnMaxParallelChange != nil {
				callbacks.OnMaxParallelChange(cfg.Downloader.NumParallelDownloads)
			}
			if cfg.Monitors.HideFinishedAgeDays.Value != oldHideAge && callbacks.OnHideFinishedAgeChanged != nil {
				callbacks.OnHideFinishedAgeChanged()
			}
			if _, hasChannels := updates["channels"]; hasChannels && callbacks.OnChannelChange != nil {
				callbacks.OnChannelChange()
			}
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}

// ChannelRoutes registers channel-related API routes.
func ChannelRoutes(r chi.Router, cfg *config.MoomboxConfig, saveConfig func(*config.MoomboxConfig) error, onChannelChange func()) {
	// POST /api/v1/config/channels
	r.Post("/api/v1/config/channels", func(rw http.ResponseWriter, req *http.Request) {
		var channel config.ChannelConfig
		if err := json.NewDecoder(req.Body).Decode(&channel); err != nil {
			jsonError(rw, "invalid channel config", http.StatusBadRequest)
			return
		}

		if channel.ID == "" {
			jsonError(rw, "channel ID required", http.StatusBadRequest)
			return
		}

		// Upsert
		found := false
		for i, ch := range cfg.Channels {
			if ch.ID == channel.ID {
				cfg.Channels[i] = channel
				found = true
				break
			}
		}
		if !found {
			cfg.Channels = append(cfg.Channels, channel)
		}

		// Persist to disk
		if saveConfig != nil {
			if err := saveConfig(cfg); err != nil {
				jsonError(rw, "failed to save config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		if onChannelChange != nil {
			onChannelChange()
		}

		jsonResponse(rw, map[string]any{"success": true, "channel": channel})
	})

	// DELETE /api/v1/config/channels/:id
	r.Delete("/api/v1/config/channels/{id}", func(rw http.ResponseWriter, req *http.Request) {
		channelID := chi.URLParam(req, "id")

		for i, ch := range cfg.Channels {
			if ch.ID == channelID {
				cfg.Channels = append(cfg.Channels[:i], cfg.Channels[i+1:]...)

				// Persist to disk
				if saveConfig != nil {
					if err := saveConfig(cfg); err != nil {
						jsonError(rw, "failed to save config: "+err.Error(), http.StatusInternalServerError)
						return
					}
				}

				if onChannelChange != nil {
					onChannelChange()
				}

				jsonResponse(rw, map[string]any{"success": true})
				return
			}
		}

		jsonError(rw, "channel not found", http.StatusNotFound)
	})
}

// TrimRoutes registers trim-related API routes.
func TrimRoutes(r chi.Router, db *database.Database, trimSvc *worker.TrimService) {
	// POST /api/v1/jobs/:id/trims
	r.Post("/api/v1/jobs/{id}/trims", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		var body struct {
			StartTime *float64 `json:"startTime"`
			EndTime   *float64 `json:"endTime"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Validate trim parameters (matching TypeScript createTrimSchema)
		if body.StartTime == nil || body.EndTime == nil {
			jsonError(rw, "Validation failed: startTime and endTime are required", http.StatusBadRequest)
			return
		}
		if math.IsNaN(*body.StartTime) || math.IsInf(*body.StartTime, 0) || math.IsNaN(*body.EndTime) || math.IsInf(*body.EndTime, 0) {
			jsonError(rw, "Validation failed: startTime and endTime must be finite numbers", http.StatusBadRequest)
			return
		}
		if *body.StartTime < 0 {
			jsonError(rw, "Validation failed: Start time cannot be negative", http.StatusBadRequest)
			return
		}
		if *body.EndTime <= *body.StartTime {
			jsonError(rw, "Validation failed: End time must be after start time", http.StatusBadRequest)
			return
		}
		if *body.EndTime-*body.StartTime < 1 {
			jsonError(rw, "Validation failed: Trim must be at least 1 second", http.StatusBadRequest)
			return
		}

		record, err := trimSvc.CreateTrim(req.Context(), job, *body.StartTime, *body.EndTime)
		if err != nil {
			// Match TS: don't expose internal error details
			jsonError(rw, "Failed to create trim", http.StatusBadRequest)
			return
		}

		jsonResponse(rw, map[string]any{"trim": record})
	})

	// DELETE /api/v1/jobs/:id/trims/:trimId
	r.Delete("/api/v1/jobs/{id}/trims/{trimId}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		trimID := chi.URLParam(req, "trimId")

		if err := trimSvc.DeleteTrim(jobID, trimID); err != nil {
			jsonError(rw, err.Error(), http.StatusBadRequest)
			return
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}

// PotSessionData represents a PO token response.
type PotSessionData struct {
	PoToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
}

// PotProviderInterface defines the PO token provider methods needed by routes.
type PotProviderInterface interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
	GeneratePoTokenSession(ctx context.Context, contentBinding string, bypassCache bool) (poToken string, actualBinding string, err error)
	InvalidateCaches()
	InvalidateIntegrityTokens()
	GetMinterCacheKeys() []string
}

// PotRoutesDeps holds dependencies for POT routes.
type PotRoutesDeps struct {
	PotProvider PotProviderInterface
	StartTime   time.Time
	RateLimit   *web.RateLimiter
	Logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// PotRoutes registers POT provider endpoints (root-mounted).
func PotRoutes(r chi.Router, deps *PotRoutesDeps) {
	potRL := deps.RateLimit

	r.With(web.LoopbackOnly, potRL.Middleware).Post("/get_pot", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			ContentBinding string `json:"content_binding"`
			BypassCache    bool   `json:"bypass_cache"`
			DataSyncID     string `json:"data_sync_id"`
			VisitorData    string `json:"visitor_data"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Reject deprecated fields (match TS: separate checks with warn logs)
		if body.DataSyncID != "" {
			if deps.Logger != nil {
				deps.Logger.Warn("[PotProvider] data_sync_id is deprecated, use content_binding instead")
			}
			jsonError(rw, "data_sync_id is deprecated; use content_binding", http.StatusBadRequest)
			return
		}
		if body.VisitorData != "" {
			if deps.Logger != nil {
				deps.Logger.Warn("[PotProvider] visitor_data is deprecated, use content_binding instead")
			}
			jsonError(rw, "visitor_data is deprecated; use content_binding", http.StatusBadRequest)
			return
		}

		if deps.PotProvider == nil {
			jsonError(rw, "PO token provider not available", http.StatusServiceUnavailable)
			return
		}

		cbLabel := ""
		if body.ContentBinding != "" {
			cbLabel = " (binding=" + body.ContentBinding + ")"
		}
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] POT requested" + cbLabel)
		}

		poToken, actualBinding, err := deps.PotProvider.GeneratePoTokenSession(req.Context(), body.ContentBinding, body.BypassCache)
		if err != nil {
			jsonError(rw, "failed to generate PO token: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if deps.Logger != nil && len(poToken) > 30 {
			deps.Logger.Info("[PotProvider] POT generated: " + poToken[:30] + "...")
		}

		jsonResponse(rw, &PotSessionData{
			PoToken:        poToken,
			ContentBinding: actualBinding,
		})
	})

	r.With(web.LoopbackOnly).Post("/invalidate_caches", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] Cache invalidation requested")
		}
		if deps.PotProvider != nil {
			deps.PotProvider.InvalidateCaches()
		}
		rw.WriteHeader(http.StatusNoContent)
	})

	r.With(web.LoopbackOnly).Post("/invalidate_it", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Info("[PotProvider] Integrity token invalidation requested")
		}
		if deps.PotProvider != nil {
			deps.PotProvider.InvalidateIntegrityTokens()
		}
		rw.WriteHeader(http.StatusNoContent)
	})

	r.Get("/ping", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Debug("[PotProvider] Ping received")
		}
		jsonResponse(rw, map[string]any{
			"server_uptime": time.Since(deps.StartTime).Seconds(),
			"version":       "1.0.0",
		})
	})

	r.Get("/minter_cache", func(rw http.ResponseWriter, req *http.Request) {
		if deps.Logger != nil {
			deps.Logger.Debug("[PotProvider] Minter cache requested")
		}
		if deps.PotProvider == nil {
			jsonResponse(rw, []string{})
			return
		}
		keys := deps.PotProvider.GetMinterCacheKeys()
		if keys == nil {
			keys = []string{}
		}
		jsonResponse(rw, keys)
	})
}

// SetupDeps holds dependencies for setup wizard routes.
type SetupDeps struct {
	Cfg             *config.MoomboxConfig
	Auth            *web.AuthService
	SaveConfig      func(*config.MoomboxConfig) error
	OnChannelChange func()
	OnRestart       func()
}

// SetupRoutes registers setup wizard endpoints.
func SetupRoutes(r chi.Router, deps *SetupDeps) {
	r.Get("/api/v1/setup/status", func(rw http.ResponseWriter, req *http.Request) {
		// isFirstRun matches TypeScript: !configManager.hasConfig()
		resp := map[string]any{
			"isFirstRun": !deps.Cfg.ConfigLoaded,
		}
		// Include FFmpeg status when config exists (post-setup check)
		if deps.Cfg.ConfigLoaded {
			path := deps.Cfg.Paths.FfmpegPath
			if path == "" {
				path = "ffmpeg"
			}
			valid, _ := CheckFFmpegCached(path)
			resp["ffmpegValid"] = valid
		}
		jsonResponse(rw, resp)
	})

	// POST /api/v1/setup/complete — uses same updateConfigSchema as PUT /config (snake_case, nested)
	// plus an additional "password" field for first-run password setup.
	r.Post("/api/v1/setup/complete", func(rw http.ResponseWriter, req *http.Request) {
		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		cfg := deps.Cfg

		// Extract password before validation (not part of updateConfigSchema)
		password, _ := updates["password"].(string)
		delete(updates, "password")

		// Validate with Zod-equivalent schema constraints (match TS updateConfigSchema)
		if validationErrs := validateConfigUpdates(updates); len(validationErrs) > 0 {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(rw).Encode(map[string]any{
				"error":   "Validation failed",
				"details": validationErrs,
			})
			return
		}

		// Hash password if provided (needed before external access check)
		if password != "" {
			if len(password) < 8 {
				jsonError(rw, "password must be at least 8 characters", http.StatusBadRequest)
				return
			}
			if deps.Auth != nil {
				hash, err := deps.Auth.HashPassword(password)
				if err != nil {
					jsonError(rw, "failed to hash password", http.StatusInternalServerError)
					return
				}
				cfg.Network.PasswordHash = hash
			}
		}

		// Validate external access requires password (match TS)
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfg.Network.PasswordHash == "" {
					jsonError(rw, "A password (min 8 characters) is required for external access.", http.StatusBadRequest)
					return
				}
			}
		}

		// Apply config updates (same schema as PUT /config)
		applyConfigUpdates(cfg, updates)

		// Create directories if specified
		if cfg.Paths.OutputDirectory != "" {
			os.MkdirAll(cfg.Paths.OutputDirectory, 0o755)
		}
		if cfg.Paths.StagingDirectory != "" {
			os.MkdirAll(cfg.Paths.StagingDirectory, 0o755)
		}

		// Save config
		if deps.SaveConfig != nil {
			if err := deps.SaveConfig(cfg); err != nil {
				jsonError(rw, "failed to save config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Kick monitors to pick up any channels added during setup
		if deps.OnChannelChange != nil {
			deps.OnChannelChange()
		}

		jsonResponse(rw, map[string]any{"success": true})

		// Trigger a restart so all services re-initialize with new config
		if deps.OnRestart != nil {
			go func() {
				time.Sleep(500 * time.Millisecond)
				deps.OnRestart()
			}()
		}
	})
}

// LogRoutes registers log-related API routes.
func LogRoutes(r chi.Router, getRecentLogs func() []string) {
	r.Get("/api/v1/logs", func(rw http.ResponseWriter, req *http.Request) {
		logs := getRecentLogs()
		jsonResponse(rw, logs)
	})
}

// ImportRoutes registers import-related API routes.
// Uses its own 5/min rate limiter per the spec.
func ImportRoutes(r chi.Router, db *database.Database, cfg *config.MoomboxConfig, rl *web.RateLimiter) {
	importRL := web.NewRateLimiter(5, time.Minute)
	r.With(importRL.Middleware).Post("/api/v1/import", func(rw http.ResponseWriter, req *http.Request) {
		// Max 500MB upload
		req.Body = http.MaxBytesReader(rw, req.Body, 500*1024*1024)

		contentType := req.Header.Get("Content-Type")
		if !strings.Contains(contentType, "application/octet-stream") &&
			!strings.Contains(contentType, "application/zip") {
			jsonError(rw, "expected application/octet-stream or application/zip", http.StatusBadRequest)
			return
		}

		titleHeader := req.Header.Get("X-Import-Title")
		channelHeader := req.Header.Get("X-Import-Channel")
		if len(titleHeader) > 500 {
			titleHeader = titleHeader[:500]
		}
		if len(channelHeader) > 500 {
			channelHeader = channelHeader[:500]
		}

		// Read the uploaded file to a temp location
		tmpFile, err := os.CreateTemp("", "moombox-import-*.zip")
		if err != nil {
			jsonError(rw, "failed to create temp file", http.StatusInternalServerError)
			return
		}
		tmpPath := tmpFile.Name()
		defer os.Remove(tmpPath)

		_, err = copyWithLimit(tmpFile, req.Body, 500*1024*1024)
		if err != nil {
			tmpFile.Close()
			jsonError(rw, "failed to read upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		tmpFile.Close()

		// Try to open as ZIP
		zipReader, zipErr := zip.OpenReader(tmpPath)
		if zipErr != nil {
			jsonError(rw, "invalid zip file", http.StatusBadRequest)
			return
		}
		defer zipReader.Close()

		// Zip bomb protection
		const maxUncompressed = 2 * 1024 * 1024 * 1024 // 2GB
		const maxFiles = 1000
		const maxCompressionRatio = 100

		if len(zipReader.File) > maxFiles {
			jsonError(rw, "too many files in zip", http.StatusBadRequest)
			return
		}

		var totalUncompressed uint64
		for _, f := range zipReader.File {
			totalUncompressed += f.UncompressedSize64
			if totalUncompressed > maxUncompressed {
				jsonError(rw, "zip file too large (uncompressed, max 2GB)", http.StatusBadRequest)
				return
			}
		}

		// Check compression ratio
		stat, _ := os.Stat(tmpPath)
		if stat != nil && stat.Size() > 0 && totalUncompressed/uint64(stat.Size()) > maxCompressionRatio {
			jsonError(rw, "suspicious compression ratio (possible zip bomb)", http.StatusBadRequest)
			return
		}

		// Validate paths for traversal
		for _, f := range zipReader.File {
			name := filepath.Clean(f.Name)
			if strings.Contains(name, "..") || filepath.IsAbs(name) {
				jsonError(rw, "invalid zip entry path", http.StatusBadRequest)
				return
			}
		}

		// Scan for video and chat files
		videoExts := map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".ts": true}
		var videoFile, chatFile *zip.File

		for _, f := range zipReader.File {
			if f.FileInfo().IsDir() {
				continue
			}
			name := strings.ToLower(f.Name)
			ext := filepath.Ext(name)

			if videoFile == nil && videoExts[ext] {
				videoFile = f
			}
			if chatFile == nil && strings.HasSuffix(name, ".chat.json") {
				chatFile = f
			}
		}

		// Fallback: look for any .json with messages array
		if chatFile == nil {
			for _, f := range zipReader.File {
				if f.FileInfo().IsDir() {
					continue
				}
				name := strings.ToLower(f.Name)
				if !strings.HasSuffix(name, ".json") {
					continue
				}
				if f.UncompressedSize64 > 10*1024*1024 {
					continue // Skip large JSON files
				}
				rc, err := f.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					continue
				}
				var parsed struct {
					Messages []struct {
						OffsetMs json.Number `json:"offsetMs"`
					} `json:"messages"`
				}
				if json.Unmarshal(data, &parsed) == nil && len(parsed.Messages) > 0 {
					chatFile = f
					break
				}
			}
		}

		if videoFile == nil {
			jsonError(rw, "no video file found in zip (.mp4, .mkv, .webm, .ts)", http.StatusBadRequest)
			return
		}

		// Derive metadata
		videoFilename := filepath.Base(videoFile.Name)
		videoExt := filepath.Ext(videoFilename)
		videoBasename := strings.TrimSuffix(videoFilename, videoExt)

		// Try to extract video ID from [XXXXXXXXXXX] pattern
		idMatch := bracketIDRe.FindStringSubmatch(videoBasename)
		videoID := ""
		if idMatch != nil {
			videoID = idMatch[1]
		}

		// Read optional chat metadata for videoId/title/channel
		type chatMeta struct {
			VideoID     string `json:"videoId"`
			VideoTitle  string `json:"videoTitle"`
			ChannelName string `json:"channelName"`
		}
		var meta chatMeta
		if chatFile != nil {
			if rc, err := chatFile.Open(); err == nil {
				data, readErr := io.ReadAll(rc)
				rc.Close()
				if readErr == nil {
					json.Unmarshal(data, &meta)
				}
			}
		}

		// Use chat metadata videoId if we generated a random one
		if videoID == "" && meta.VideoID != "" {
			videoID = meta.VideoID
		}
		if videoID == "" {
			videoID = fmt.Sprintf("imp_%s", randomHex(4))
		}

		title := titleHeader
		if title == "" {
			title = meta.VideoTitle
		}
		if title == "" {
			title = videoBasename
		}
		if title == "" {
			title = "Import"
		}
		channel := channelHeader
		if channel == "" {
			channel = meta.ChannelName
		}
		if channel == "" {
			channel = "Import"
		}

		// Check for duplicate (use JobExists to match TS - checks ALL jobs, not just active)
		if db.JobExists(videoID) {
			jsonError(rw, "job already exists for video ID: "+videoID, http.StatusConflict)
			return
		}

		// Output paths
		outputDir := cfg.Paths.OutputDirectory
		if outputDir == "" {
			outputDir = "./output"
		}
		importsDir := filepath.Join(outputDir, "imports")
		os.MkdirAll(importsDir, 0o755)

		baseFilename := fmt.Sprintf("%s [%s]", sanitizeForFilename(title), videoID)
		videoOutName := filepath.Join("imports", baseFilename+videoExt)
		videoOutPath := filepath.Join(outputDir, videoOutName)

		// Extract video file
		if err := extractZipEntry(videoFile, videoOutPath); err != nil {
			jsonError(rw, "failed to extract video: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Extract chat file if present
		chatOutName := ""
		if chatFile != nil {
			chatOutName = filepath.Join("imports", baseFilename+".chat.json")
			chatOutPath := filepath.Join(outputDir, chatOutName)
			if err := extractZipEntry(chatFile, chatOutPath); err != nil {
				// Non-fatal, just skip chat
				chatOutName = ""
			}
		}

		// Create job
		job := &database.Job{
			ID:            videoID,
			VideoID:       videoID,
			URL:           "https://www.youtube.com/watch?v=" + videoID,
			Title:         title,
			ChannelName:   channel,
			ThumbnailURL:  "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg",
			Platform:      "youtube",
			Status:        database.StatusFinished,
			Progress:      "Imported",
			Percent:       100,
			Filename:      videoOutName,
			ChatFilename:  chatOutName,
			ManuallyAdded: true,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		}

		if _, err := db.AddJob(job); err != nil {
			jsonError(rw, "failed to create job: "+err.Error(), http.StatusInternalServerError)
			return
		}

		rw.WriteHeader(http.StatusCreated)
		jsonResponse(rw, job)
	})
}

// extractZipEntry extracts a single zip entry to a destination path.
func extractZipEntry(f *zip.File, destPath string) error {
	os.MkdirAll(filepath.Dir(destPath), 0o755)
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}

func copyWithLimit(dst *os.File, src io.Reader, limit int64) (int64, error) {
	return io.Copy(dst, io.LimitReader(src, limit))
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sanitizeForFilename removes or replaces characters not safe for filenames.
var unsafeFilenameRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitizeForFilename(s string) string {
	s = unsafeFilenameRe.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

// RestartRoute registers the restart endpoint (loopback only).
// The onRestart callback is invoked after the HTTP response is sent.
// It should spawn a new process and then trigger graceful shutdown.
func RestartRoute(r chi.Router, onRestart func()) {
	r.With(web.LoopbackOnly).Post("/api/v1/restart", func(rw http.ResponseWriter, req *http.Request) {
		jsonResponse(rw, map[string]any{"success": true, "message": "Restarting..."})

		go func() {
			time.Sleep(500 * time.Millisecond)
			if onRestart != nil {
				onRestart()
			}
		}()
	})
}
