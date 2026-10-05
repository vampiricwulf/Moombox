package routes

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"golang.org/x/text/encoding/charmap"

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
		req.Body = http.MaxBytesReader(rw, req.Body, maxImportUpload)

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
		tmpFile, err := os.CreateTemp("", importTempPrefix+"*.zip")
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

		// One byte past the cap is read on purpose: an upload that stops
		// exactly AT the cap was copied silently truncated before, and then
		// failed as "invalid zip file" — a 400 that blamed the archive for
		// what was its size. MaxBytesReader answers the byte past the cap
		// with *http.MaxBytesError; either sign is a 413.
		_, err = copyWithLimit(tmpFile, req.Body, maxImportUpload-int64(n))
		tmpFile.Close()
		if err != nil {
			if importUploadTooLarge(err) {
				jsonError(rw, "upload too large (max 500 MB)", http.StatusRequestEntityTooLarge)
				return
			}
			jsonError(rw, "failed to read upload", http.StatusBadRequest)
			return
		}

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

		// Validate paths for traversal. Defence in depth — no entry name
		// ever reaches a destination path (the output name is built from the
		// sanitised title and a validated id below) — so the rule is a ".."
		// path COMPONENT, not the substring: Moombox's own naming writes
		// "Wait... what_ [id].mp4", which the substring test refused to
		// re-import.
		for _, f := range zipReader.File {
			name := filepath.ToSlash(filepath.Clean(f.Name))
			if slices.Contains(strings.Split(name, "/"), "..") || filepath.IsAbs(f.Name) || strings.HasPrefix(name, "/") {
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
		videoFilename := filepath.Base(zipEntryName(videoFile))
		videoExt := filepath.Ext(videoFilename)
		videoBasename := strings.TrimSuffix(videoFilename, videoExt)

		// Try to extract video ID from [XXXXXXXXXXX] pattern. The title a
		// filename yields is the name WITHOUT it: the output name appends
		// " [id]" itself, and keeping it doubled the id in both the title and
		// the file ("video [id] [id].mp4").
		idMatch := bracketIDRe.FindStringSubmatch(videoBasename)
		videoID := ""
		nameTitle := videoBasename
		if idMatch != nil {
			videoID = idMatch[1]
			nameTitle = strings.Join(strings.Fields(strings.Replace(videoBasename, idMatch[0], "", 1)), " ")
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

		// Use chat metadata videoId if the filename carried no [bracket] id.
		//
		// utils.IsVideoID, not a bare non-empty check (WEB-1 / O-AA): this
		// value comes out of the UPLOADED zip and is interpolated into the
		// output filename below. filepath.Join CLEANS what it joins, so an id
		// beginning "/.." promotes the following ".." elements to real path
		// segments and walks out of imports/ — and os.Create truncates
		// whatever it lands on. The bracket-regex path (bracketIDRe) is
		// already constrained to the same shape; this is the path that was
		// not. An id that fails the check falls back to the generated one
		// rather than failing the import: O-AA chose "imports everything"
		// over "surfaces bad archives".
		if videoID == "" && utils.IsVideoID(meta.VideoID) {
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
			title = nameTitle
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

		// Belt and braces on top of the id validation above: every WRITE now
		// goes through the same canonical containment check the read routes
		// use, so a future change to how baseFilename is built cannot re-open
		// WEB-1. CanonicalPath resolves the nearest existing ancestor, so this
		// is valid on a path that does not exist yet.
		if _, ok := validatePathTraversal(videoOutPath, outputDir); !ok {
			jsonError(rw, "invalid output path", http.StatusBadRequest)
			return
		}

		// Extract video file
		if err := extractZipEntry(videoFile, videoOutPath); err != nil {
			jsonError(rw, "failed to extract video", http.StatusInternalServerError)
			return
		}

		// Extract chat file if present
		chatOutName, chatOutPath := "", ""
		if chatFile != nil {
			chatOutName = filepath.Join("imports", baseFilename+".chat.json")
			chatOutPath = filepath.Join(outputDir, chatOutName)
			if _, ok := validatePathTraversal(chatOutPath, outputDir); !ok {
				chatOutName, chatOutPath = "", ""
			} else if err := extractZipEntry(chatFile, chatOutPath); err != nil {
				// Non-fatal, just skip chat
				chatOutName, chatOutPath = "", ""
			}
		}

		// The row a recording gets, so an import is not a second-class job:
		// absolute output/chat paths, the pinned output directory the
		// relative names resolve against, and the size (Stats' recorded
		// total and the details dialog's size line both read it).
		absVideo, _ := filepath.Abs(videoOutPath)
		absChat := ""
		if chatOutPath != "" {
			absChat, _ = filepath.Abs(chatOutPath)
		}
		var fileSize *int64
		if info, err := os.Stat(videoOutPath); err == nil {
			size := info.Size()
			fileSize = &size
		}

		// Create job
		job := &database.Job{
			ID:              videoID,
			VideoID:         videoID,
			URL:             "https://www.youtube.com/watch?v=" + videoID,
			Title:           title,
			ChannelName:     channel,
			ThumbnailURL:    "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg",
			Platform:        "youtube",
			Status:          database.StatusFinished,
			Progress:        "Imported",
			Percent:         100,
			Filename:        videoOutName,
			ChatFilename:    chatOutName,
			OutputFile:      absVideo,
			ChatFile:        absChat,
			OutputDirectory: outputDir,
			FileSize:        fileSize,
			ManuallyAdded:   true,
			CreatedAt:       time.Now().UTC().Format(time.RFC3339),
			UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
		}

		added, err := db.AddJob(job)
		if err != nil {
			// No row will ever name what was just extracted: take it back
			// out rather than leave it for the Files tab to find as orphans.
			os.Remove(videoOutPath)
			if chatOutPath != "" {
				os.Remove(chatOutPath)
			}
			jsonError(rw, "failed to create job", http.StatusInternalServerError)
			return
		}
		if !added {
			// Another request inserted this id between JobExists and here.
			// Its row may well name these very files, so they stay.
			jsonError(rw, "job already exists for video ID: "+videoID, http.StatusConflict)
			return
		}
		// Answer with the row as stored, not the struct built above: a field
		// the insert does not write would otherwise be reported as saved.
		if stored, err := db.GetJob(videoID); err == nil && stored != nil {
			job = stored
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

// maxImportUpload caps an import upload (the whole request body).
const maxImportUpload = 500 * 1024 * 1024

// errImportTooLarge is copyWithLimit's answer to a body longer than its limit.
var errImportTooLarge = errors.New("upload exceeds the import size limit")

// copyWithLimit copies at most limit bytes and reports errImportTooLarge when
// src holds more — read one byte past the limit to tell the two apart.
func copyWithLimit(dst *os.File, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err == nil && n > limit {
		return n, errImportTooLarge
	}
	return n, err
}

// importUploadTooLarge reports whether a spool error means the body ran past
// the cap — copyWithLimit's own verdict, or MaxBytesReader's.
func importUploadTooLarge(err error) bool {
	_, overMax := errors.AsType[*http.MaxBytesError](err)
	return overMax || errors.Is(err, errImportTooLarge)
}

// zipEntryName returns f's name as UTF-8. A name that is not valid UTF-8 is
// CP437 — the zip format's encoding when the UTF-8 flag is clear, and what
// older Windows zippers write — and is decoded as such; a name that IS valid
// UTF-8 is used as-is even with the flag clear, because many tools write UTF-8
// without setting it. Raw CP437 bytes otherwise became an invalid-UTF-8 title
// and filename, which JSON then rendered as U+FFFD.
func zipEntryName(f *zip.File) string {
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	if s, err := charmap.CodePage437.NewDecoder().String(f.Name); err == nil {
		return s
	}
	return strings.ToValidUTF8(f.Name, "\uFFFD")
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// importTempPrefix names the temp file an upload is spooled to before it is
// opened as a zip (see the import route above).
const importTempPrefix = "moombox-import-"

// CleanupOldImportTemp removes import spool files older than 24h. The route
// removes its own with a deferred os.Remove, but a hard abort mid-import (OS
// kill, power loss) leaves up to 500 MB behind in the temp directory. No
// import runs for a day, so the age floor never touches one in flight.
func CleanupOldImportTemp() (removed int, err error) {
	return utils.RemoveStaleTempEntries(24*time.Hour, importTempPrefix)
}
