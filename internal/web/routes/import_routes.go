package routes

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// unsafeFilenameRe matches characters not safe for filenames.
var unsafeFilenameRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// ImportRoutes registers import-related API routes.
// Uses its own 5/min rate limiter per the spec.
// cfgMu protects concurrent reads/writes to the shared cfg struct.
// Returns a cleanup function that stops the rate limiter's background goroutine.
func ImportRoutes(r chi.Router, db *database.Database, cfg *config.MoomboxConfig, cfgMu *sync.RWMutex, rl *web.RateLimiter) func() {
	importRL := web.NewRateLimiter(5, time.Minute)
	r.With(importRL.Middleware).Post("/api/import", func(rw http.ResponseWriter, req *http.Request) {
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
			jsonError(rw, "failed to read upload", http.StatusBadRequest)
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
		cfgMu.RLock()
		outputDir := cfg.Paths.OutputDirectory
		cfgMu.RUnlock()
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
			jsonError(rw, "failed to extract video", http.StatusInternalServerError)
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
			jsonError(rw, "failed to create job", http.StatusInternalServerError)
			return
		}

		rw.WriteHeader(http.StatusCreated)
		jsonResponse(rw, job)
	})

	return func() { importRL.Close() }
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

	// Limit to declared size + 1 byte to detect zip bombs that lie about UncompressedSize64
	limit := int64(f.UncompressedSize64) + 1
	n, err := io.Copy(out, io.LimitReader(rc, limit))
	if n >= limit {
		return fmt.Errorf("zip entry %q exceeds declared size (zip bomb protection)", f.Name)
	}
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
