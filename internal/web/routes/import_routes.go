package routes

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// truncateUTF8 caps s at maxBytes without splitting a multi-byte rune —
// decoded titles are UTF-8, and a blind byte slice would persist an invalid
// trailing sequence into the job title.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// decodeImportHeader percent-decodes an import metadata header value,
// falling back to the raw value when it isn't valid percent-encoding.
// Uses PathUnescape (not QueryUnescape) so a literal '+' is preserved rather
// than turned into a space — the header is not form-urlencoded, and the
// frontend sends encodeURIComponent output (spaces as %20, '+' as %2B).
func decodeImportHeader(v string) string {
	if v == "" || !strings.Contains(v, "%") {
		return v
	}
	if decoded, err := url.PathUnescape(v); err == nil {
		return decoded
	}
	return v
}

// ImportRoutes registers import-related API routes.
// Uses its own 5/min rate limiter per the spec — the global API
// limiter passed via routes_wiring is intentionally NOT applied here
// (audit Q-21/U-2); imports are rare, large, and need a tighter cap.
// Returns a cleanup function that stops the rate limiter's background goroutine.
func ImportRoutes(r chi.Router, db *database.Database, store *config.Store) func() {
	importRL := web.NewRateLimiter(5, time.Minute)
	// Key the buckets by the effective client IP so a trusted reverse proxy
	// doesn't collapse every remote client into a single 5/min bucket.
	importRL.ClientIP = func(r *http.Request) string { return web.EffectiveClientIP(store, r) }
	r.With(importRL.Middleware).Post("/api/import", func(rw http.ResponseWriter, req *http.Request) {
		// Max 500MB upload
		req.Body = http.MaxBytesReader(rw, req.Body, 500*1024*1024)

		contentType := req.Header.Get("Content-Type")
		if !strings.Contains(contentType, "application/octet-stream") &&
			!strings.Contains(contentType, "application/zip") {
			jsonError(rw, "expected application/octet-stream or application/zip", http.StatusBadRequest)
			return
		}

		// The frontend percent-encodes these headers (HTTP headers are
		// Latin-1; raw CJK titles would throw in the browser before the
		// request is even sent). Decode here; a value without % sequences
		// (e.g. from curl) passes through unchanged, and malformed encoding
		// falls back to the raw header.
		titleHeader := truncateUTF8(decodeImportHeader(req.Header.Get("X-Import-Title")), 500)
		channelHeader := truncateUTF8(decodeImportHeader(req.Header.Get("X-Import-Channel")), 500)

		// Audit Q-13: peek the first 4 bytes for the ZIP local-file-header
		// magic ("PK\x03\x04") before allocating a temp file. Otherwise a
		// caller can stream up to 500MB of arbitrary garbage to disk before
		// `zip.OpenReader` rejects it, which is a cheap DoS on disk-tight
		// installs. Empty zips also start with the same magic, so the worst
		// false negative is wasting a few KB on an empty archive.
		peek := make([]byte, 4)
		n, _ := io.ReadFull(req.Body, peek)
		if n < 4 || peek[0] != 'P' || peek[1] != 'K' || peek[2] != 0x03 || peek[3] != 0x04 {
			jsonError(rw, "invalid zip file (bad signature)", http.StatusBadRequest)
			return
		}

		// Read the uploaded file to a temp location
		tmpFile, err := os.CreateTemp("", "moombox-import-*.zip")
		if err != nil {
			jsonError(rw, "failed to create temp file", http.StatusInternalServerError)
			return
		}
		tmpPath := tmpFile.Name()
		defer os.Remove(tmpPath)

		// Write the already-consumed signature bytes back into the temp file
		// so the subsequent zip.OpenReader sees a complete archive.
		if _, err := tmpFile.Write(peek[:n]); err != nil {
			tmpFile.Close()
			jsonError(rw, "failed to write upload", http.StatusInternalServerError)
			return
		}

		_, err = copyWithLimit(tmpFile, req.Body, 500*1024*1024-int64(n))
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
		var outputDir string
		store.Read(func(c *config.MoomboxConfig) {
			outputDir = c.Paths.OutputDirectory
		})
		if outputDir == "" {
			outputDir = "./output"
		}
		importsDir := filepath.Join(outputDir, "imports")
		os.MkdirAll(importsDir, 0o755)

		baseFilename := fmt.Sprintf("%s [%s]", utils.SanitizeForFilename(title), videoID)
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

		// Content-Type must be set before the explicit WriteHeader — headers
		// set afterwards are silently dropped for non-gzip clients.
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		jsonResponse(rw, job)
	})

	return func() { importRL.Close() }
}

// extractZipEntry extracts a single zip entry to a destination path. On any
// failure the partially-written destination is removed — the caller returns
// an error to the client without creating a job, so a leftover truncated
// file would be a silent disk leak under output/imports/.
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

	// Limit to declared size + 1 byte to detect zip bombs that lie about UncompressedSize64
	limit := int64(f.UncompressedSize64) + 1
	n, err := io.Copy(out, io.LimitReader(rc, limit))
	if n >= limit {
		err = fmt.Errorf("zip entry %q exceeds declared size (zip bomb protection)", f.Name)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(destPath)
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
