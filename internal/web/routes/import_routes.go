package routes

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"golang.org/x/text/encoding/charmap"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
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
// logger names what an import could not write: the client gets the step that
// failed, the log gets the error.
func ImportRoutes(r chi.Router, db *database.Database, store *config.Store, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) func() {
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

		// The recording the zip holds: one video, or the parts of one split
		// recording; its chats paired by name. Anything else is refused,
		// naming the videos — the import used to take the first video and,
		// separately, the first chat, so a second recording was dropped
		// without a word and the first could be imported under another
		// video's id, title and chat (W25-04).
		rec, err := scanImportRecording(zipReader.File)
		if err != nil {
			jsonError(rw, err.Error(), http.StatusBadRequest)
			return
		}

		// The id the file name carries, and the title it yields: the name
		// WITHOUT that id, since the output name appends " [id]" itself and
		// keeping it doubled the id in both the title and the file
		// ("video [id] [id].mp4"). A split recording's parts share one name,
		// less their " - partN".
		videoID, nameTitle := importNameID(filepath.Base(rec.stem))

		// Metadata — id, title, channel, platform — comes only from a chat
		// paired with this recording by name.
		var meta importChatMeta
		if metaChat := rec.metaChat(); metaChat != nil {
			if rc, err := metaChat.Open(); err == nil {
				meta = readImportChatMeta(rc)
				rc.Close()
			}
		}

		// Use chat metadata videoId if the filename carried no [bracket] id.
		//
		// utils.IsVideoID, not a bare non-empty check (WEB-1 / O-AA): this
		// value comes out of the UPLOADED zip and is interpolated into the
		// output filename below. filepath.Join CLEANS what it joins, so an id
		// beginning "/.." promotes the following ".." elements to real path
		// segments and walks out of imports/ — and os.Create truncates
		// whatever it lands on. The file-name path (importNameIDRe) only
		// takes path-safe id shapes; this is the path that was not. An id
		// that fails the check falls back to the generated one rather than
		// failing the import: O-AA chose "imports everything" over
		// "surfaces bad archives".
		//
		// A Moombox Twitch archive says what it is twice: its chat header
		// (twitch.TwitchChatData, platform "twitch") and the "tw_" job id its
		// file is named with. It imported as a YouTube row — channel
		// "Import", a watch URL for a video that does not exist, and the
		// "[tw_…]" id left in its title (W25-03).
		twitchArchive := meta.Platform == "twitch" || strings.HasPrefix(videoID, "tw_")
		if videoID == "" && !twitchArchive && utils.IsVideoID(meta.VideoID) {
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
		if channel == "" && twitchArchive {
			channel = cmp.Or(meta.ChannelDisplayName, meta.ChannelLogin)
		}
		if channel == "" {
			channel = "Import"
		}

		// Where the row points. A YouTube id is a watch page and a
		// thumbnail; of Twitch's ids only a VOD's names a page
		// (twitch.tv/videos/<id>) — a live capture's stream id names none,
		// so that row carries no URL rather than a wrong one.
		platform := "youtube"
		videoURL := "https://www.youtube.com/watch?v=" + videoID
		thumbnailURL := "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg"
		isVod := false
		if twitchArchive {
			platform, videoURL, thumbnailURL = "twitch", "", ""
			if m := importTwitchVODRe.FindStringSubmatch(videoID); m != nil {
				videoURL, isVod = "https://www.twitch.tv/videos/"+m[1], true
			}
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
		if err := os.MkdirAll(importsDir, 0o755); err != nil {
			logger.Error("import: could not create the imports directory", "dir", importsDir, "err", err)
			jsonError(rw, "failed to create the imports directory", http.StatusInternalServerError)
			return
		}

		// Every entry is extracted to a temporary name inside imports/ and
		// moved to its own name only once the row exists. Nothing this
		// request did not create is ever truncated or removed: a row's Delete
		// leaves its files in imports/, so a re-import of the same archive
		// writes exactly the names that are already there, and extracting
		// straight onto them truncated the only good copy — removed it
		// outright when the entry turned out damaged or the insert failed,
		// and replaced it without a word when a different file came in under
		// the same name (W25-01).
		//
		// A chat that cannot be written fails the import like a video: the
		// zip carried it, and a 201 without it said the archive was imported
		// when its chat was gone (W25-02).
		files := rec.files()
		// What this request extracted and did not place goes on the way out,
		// on every failure path and through a panic the recovery middleware
		// catches. Only those temporary files: never a placed or adopted one.
		defer func() {
			for _, f := range files {
				if f.tmp != "" {
					os.Remove(f.tmp)
				}
			}
		}()
		for _, f := range files {
			if err := f.extract(importsDir); err != nil {
				logger.Error("import: could not extract the "+f.kind, "entry", zipEntryName(f.entry), "err", err)
				jsonError(rw, "failed to extract "+f.kind, http.StatusInternalServerError)
				return
			}
		}

		// The names: "<title> [<id>]", or the first " (n)" of it that is
		// free or already holds exactly these bytes — a file left by a
		// deleted row of this very archive is re-adopted, not copied again;
		// a different one keeps its name and this import takes the next.
		baseStem := importStem(title, videoID)
		stem, err := chooseImportNames(importsDir, outputDir, baseStem, files)
		switch {
		case errors.Is(err, errImportInvalidPath):
			jsonError(rw, "invalid output path", http.StatusBadRequest)
			return
		case errors.Is(err, errImportNameTaken):
			logger.Warn("import: no free name in imports/", "stem", baseStem, "err", err)
			jsonError(rw, "no free name in imports/ for "+baseStem, http.StatusConflict)
			return
		case err != nil:
			logger.Error("import: could not check imports/ for the archive's name", "stem", baseStem, "err", err)
			jsonError(rw, "failed to check imports/ for the archive's name", http.StatusInternalServerError)
			return
		}
		// The row a recording gets, so an import is not a second-class job:
		// absolute output/chat paths, the pinned output directory the
		// relative names resolve against, and the size (Stats' recorded
		// total and the details dialog's size line both read it). A split
		// recording is stored as the finalize stores one
		// (finalizeMultiSegmentJob): filename the parts' shared name with no
		// extension, output_file the first part, the parts' total size, and
		// a segment row per part.
		videos := importVideoFiles(files)
		absVideo, _ := filepath.Abs(videos[0].dest)
		videoOutName := filepath.Join("imports", filepath.Base(videos[0].dest))
		var totalSize int64
		for _, v := range videos {
			totalSize += v.size
		}
		var segments []*database.Segment
		if len(videos) > 1 {
			videoOutName = filepath.Join("imports", stem)
			var ffmpegPath string
			store.Read(func(c *config.MoomboxConfig) { ffmpegPath = c.Paths.FfmpegPath })
			ffprobePath := engine.NewMuxer(ffmpegPath, logger).FFprobePath()
			for _, v := range videos {
				seg := importSegment(videoID, v, files)
				// The player lays the parts on one timeline by their
				// durations; a part it cannot read plays, but not seekably
				// across the others.
				if seg.DurationSeconds = importProbeDuration(req.Context(), ffprobePath, v.tmp); seg.DurationSeconds <= 0 {
					logger.Warn("import: could not read a part's duration", "part", seg.Filename)
				}
				segments = append(segments, seg)
			}
		}
		chatOutName, absChat := "", ""
		if c := importJobChat(files); c != nil {
			chatOutName = filepath.Join("imports", filepath.Base(c.dest))
			absChat, _ = filepath.Abs(c.dest)
		}

		// Create job
		job := &database.Job{
			ID:              videoID,
			VideoID:         videoID,
			URL:             videoURL,
			Title:           title,
			ChannelName:     channel,
			ThumbnailURL:    thumbnailURL,
			Platform:        platform,
			IsVod:           isVod,
			Status:          database.StatusFinished,
			Progress:        "Imported",
			Percent:         100,
			Filename:        videoOutName,
			ChatFilename:    chatOutName,
			OutputFile:      absVideo,
			ChatFile:        absChat,
			OutputDirectory: outputDir,
			FileSize:        &totalSize,
			ManuallyAdded:   true,
			CreatedAt:       time.Now().UTC().Format(time.RFC3339),
			UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
		}
		var totalSeconds float64
		for _, s := range segments {
			totalSeconds += s.DurationSeconds
		}
		if totalSeconds > 0 {
			length := int(totalSeconds)
			job.LengthSeconds = &length
		}

		added, err := db.AddJob(job)
		if err != nil {
			// No row will ever name what was extracted: the deferred cleanup
			// takes the temporary files back out, and nothing was placed.
			logger.Error("import: could not create the job", "id", videoID, "err", err)
			jsonError(rw, "failed to create job", http.StatusInternalServerError)
			return
		}
		if !added {
			// Another request inserted this id between JobExists and here.
			// Its row may well name files in imports/; this request placed
			// none, and its temporary files go with the deferred cleanup.
			jsonError(rw, "job already exists for video ID: "+videoID, http.StatusConflict)
			return
		}

		// A split recording's parts, on rows of their own beside the job's.
		for _, s := range segments {
			if err := db.AddSegment(s); err != nil {
				logger.Error("import: could not record a part", "id", videoID, "part", s.Filename, "err", err)
				db.DeleteJob(videoID) // its segment rows go with it (ON DELETE CASCADE)
				jsonError(rw, "failed to create job", http.StatusInternalServerError)
				return
			}
		}

		// Into place, now that a row names them.
		for _, f := range files {
			if err := f.place(); err != nil {
				logger.Error("import: could not move the "+f.kind+" into place", "path", f.dest, "err", err)
				// The row goes, and so does every file this request placed —
				// each was a name nothing held. An adopted file stays: it was
				// there before this request.
				db.DeleteJob(videoID)
				for _, p := range files {
					if p.placed {
						os.Remove(p.dest)
					}
				}
				jsonError(rw, "failed to move the "+f.kind+" into imports/", http.StatusInternalServerError)
				return
			}
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
		jsonResponse(rw, importResponse{Job: job, Import: newImportOutcome(baseStem, files, rec.unpairedChats)})
	})

	return func() { importRL.Close() }
}

// importResponse is an import's 201 body: the row as stored, and under
// "import" what the import did about names already taken in imports/.
type importResponse struct {
	*database.Job
	Import importOutcome `json:"import"`
}

// importOutcome tells the dashboard and the TUI what became of the names the
// import found taken, and of a chat it found no video for. Every field is
// empty for an import that took free names and every chat.
type importOutcome struct {
	// Readopted names the files that were already in imports/ holding
	// exactly the imported bytes: the row names them, and they were not
	// written over.
	Readopted []string `json:"readopted,omitempty"`
	// Renamed lists the files whose name held a different file: that file
	// was left alone, and the import took the first free " (n)" name.
	Renamed []importRename `json:"renamed,omitempty"`
	// UnpairedChats names the zip's chat archives whose name matches none of
	// its videos: a chat is paired by name only, so these were not imported.
	UnpairedChats []string `json:"unpairedChats,omitempty"`
	// Note is the outcome in one line, the words both clients show.
	Note string `json:"note,omitempty"`
}

// importRename is one file placed under a " (n)" name: From is the name a
// different file already held, To the name the import took.
type importRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// newImportOutcome reports where each file landed against the name it was
// meant to have, baseStem plus its suffix, and the chats left out.
func newImportOutcome(baseStem string, files []*importFile, unpairedChats []string) importOutcome {
	out := importOutcome{UnpairedChats: unpairedChats}
	var notes []string
	if len(unpairedChats) > 0 {
		notes = append(notes, "left out "+strings.Join(unpairedChats, ", ")+": a chat is imported only beside the video its name matches")
	}
	for _, f := range files {
		name := filepath.Base(f.dest)
		switch {
		case f.adopted:
			out.Readopted = append(out.Readopted, name)
		case name != baseStem+f.suffix:
			out.Renamed = append(out.Renamed, importRename{From: baseStem + f.suffix, To: name})
			notes = append(notes, fmt.Sprintf("imports/ already held a different %q; imported as %q", baseStem+f.suffix, name))
		}
	}
	if len(out.Readopted) > 0 {
		notes = append(notes, "kept the identical copy already in imports/: "+strings.Join(out.Readopted, ", "))
	}
	out.Note = strings.Join(notes, "; ")
	return out
}

// importVideoExts are the extensions an import takes for a video.
var importVideoExts = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".ts": true}

// importPartRe is a split recording's part, its name less the extension: the
// finalize names each "<name> - part<N>" with N from 1 (muxSegment,
// internal/worker/orchestrator_mux.go).
var importPartRe = regexp.MustCompile(`^(.+) - part([1-9][0-9]*)$`)

// importPart is one video of the recording a zip holds.
type importPart struct {
	video *zip.File
	stem  string    // its entry name less the extension
	ext   string    // its extension, as the entry spells it
	num   int       // the part number its name carries; 0 for a recording in one file
	chat  *zip.File // "<stem>.chat.json": this part's own chat (a split Twitch capture's)
}

// importRecording is what a zip holds: one recording, in one file or in the
// parts the finalize splits one into, and the chats paired with it by name.
type importRecording struct {
	stem  string       // the entry name its files share, less any " - partN" and the extension
	parts []importPart // in part order
	chat  *zip.File    // "<stem>.chat.json": the recording's own chat
	// unpairedChats are the zip's .chat.json entries named after no video.
	unpairedChats []string
}

// scanImportRecording finds the recording in a zip's entries. A zip with
// more than one video is accepted only when they are the parts of one split
// recording, named as the finalize names them; any other is refused with the
// videos named. A chat belongs to the video whose name it carries —
// "<name>.chat.json", or a "<name>.json" holding a messages array — and to
// nothing else: never to a video just because it is the only chat there.
func scanImportRecording(entries []*zip.File) (*importRecording, error) {
	var videos []importPart
	chats := map[string]*zip.File{}
	jsons := map[string]*zip.File{}
	var chatOrder []string
	for _, f := range entries {
		if f.FileInfo().IsDir() {
			continue
		}
		name := zipEntryName(f)
		if importMacMetadata(f, name) {
			continue
		}
		lower := strings.ToLower(name)
		// The extension is cut from the name as written: lower-casing can
		// change a rune's length (KELVIN SIGN is "k"), so an offset taken
		// from the lowered name can split the original.
		switch ext := filepath.Ext(name); {
		case importVideoExts[strings.ToLower(ext)]:
			videos = append(videos, importPart{video: f, stem: strings.TrimSuffix(name, ext), ext: ext})
		case strings.HasSuffix(lower, ".chat.json"):
			if stem := name[:len(name)-len(".chat.json")]; chats[stem] == nil {
				chats[stem] = f
				chatOrder = append(chatOrder, stem)
			}
		case strings.ToLower(ext) == ".json":
			if stem := name[:len(name)-len(".json")]; jsons[stem] == nil {
				jsons[stem] = f
			}
		}
	}
	if len(videos) == 0 {
		return nil, errors.New("no video file found in zip (.mp4, .mkv, .webm, .ts)")
	}

	rec := &importRecording{stem: videos[0].stem, parts: videos}
	if len(videos) > 1 {
		seen := map[int]bool{}
		for i, v := range videos {
			m := importPartRe.FindStringSubmatch(v.stem)
			n := 0
			if m != nil {
				n, _ = strconv.Atoi(m[2])
			}
			if m == nil || n == 0 || seen[n] || (i > 0 && m[1] != rec.stem) {
				return nil, importMultipleRecordingsError(videos)
			}
			rec.stem, videos[i].num, seen[n] = m[1], n, true
		}
		slices.SortFunc(rec.parts, func(a, b importPart) int { return a.num - b.num })
	}

	paired := map[string]bool{}
	pair := func(stem string) *zip.File {
		if c := chats[stem]; c != nil {
			paired[stem] = true
			return c
		}
		if j := jsons[stem]; j != nil && importJSONIsChat(j) {
			return j
		}
		return nil
	}
	rec.chat = pair(rec.stem)
	if len(rec.parts) > 1 {
		for i := range rec.parts {
			rec.parts[i].chat = pair(rec.parts[i].stem)
		}
	}
	for _, stem := range chatOrder {
		if !paired[stem] {
			rec.unpairedChats = append(rec.unpairedChats, filepath.Base(stem)+".chat.json")
		}
	}
	return rec, nil
}

// importAppleDoubleMagic opens every AppleDouble file: the extended
// attributes and resource fork macOS keeps for a file on a volume that
// cannot hold them, as "._<name>" beside it.
var importAppleDoubleMagic = []byte{0x00, 0x05, 0x16, 0x07}

// importMacMetadata reports whether a zip entry is macOS's metadata about a
// file rather than a file of the archive. Finder's Compress writes each
// file's extended attributes (every download carries com.apple.quarantine)
// as "__MACOSX/<dir>/._<name>", and other Mac tools write the "._<name>"
// beside the file — named with that file's own extension, so it was
// counted as a second video, and a zip of one recording was refused as two.
// Everything under __MACOSX is metadata; a "._" name elsewhere is metadata
// when it holds AppleDouble's magic, so a video whose own title begins "._"
// still imports.
func importMacMetadata(f *zip.File, name string) bool {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return false
	}
	if slices.Contains(parts[:len(parts)-1], "__MACOSX") {
		return true
	}
	if !strings.HasPrefix(parts[len(parts)-1], "._") {
		return false
	}
	rc, err := f.Open()
	if err != nil {
		return false
	}
	defer rc.Close()
	head := make([]byte, len(importAppleDoubleMagic))
	_, err = io.ReadFull(rc, head)
	return err == nil && bytes.Equal(head, importAppleDoubleMagic)
}

// importMultipleRecordingsError refuses a zip whose videos are not the parts
// of one recording, naming them (the first ten).
func importMultipleRecordingsError(videos []importPart) error {
	var names []string
	for _, v := range videos {
		names = append(names, filepath.Base(v.stem)+v.ext)
	}
	if len(names) > 10 {
		names = append(names[:10], fmt.Sprintf("and %d more", len(names)-10))
	}
	return fmt.Errorf("the zip holds more than one recording: %s — import one recording per zip "+
		"(a split recording's parts are named \"<name> - part1\", \"<name> - part2\"…)", strings.Join(names, ", "))
}

// importJSONIsChat reports whether a ".json" entry is a chat archive: a
// messages array with at least one message. Anything past 10 MB is not read.
func importJSONIsChat(f *zip.File) bool {
	if f.UncompressedSize64 > 10*1024*1024 {
		return false
	}
	rc, err := f.Open()
	if err != nil {
		return false
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return false
	}
	var parsed struct {
		Messages []struct {
			OffsetMs json.Number `json:"offsetMs"`
		} `json:"messages"`
	}
	return json.Unmarshal(data, &parsed) == nil && len(parsed.Messages) > 0
}

// metaChat is the chat an import reads the recording's id, title, channel
// and platform from: its own, else its first part's.
func (r *importRecording) metaChat() *zip.File {
	if r.chat != nil {
		return r.chat
	}
	for _, p := range r.parts {
		if p.chat != nil {
			return p.chat
		}
	}
	return nil
}

// files lists what the import writes, videos first in part order: each part
// keeps its " - partN" in its suffix, and each chat follows its video's name.
func (r *importRecording) files() []*importFile {
	var files, chats []*importFile
	for _, p := range r.parts {
		partSuffix := p.stem[len(r.stem):] // " - partN", or "" for a recording in one file
		files = append(files, &importFile{entry: p.video, kind: "video", suffix: partSuffix + p.ext, part: p.num})
		if p.chat != nil {
			chats = append(chats, &importFile{entry: p.chat, kind: "chat", suffix: partSuffix + ".chat.json", part: p.num})
		}
	}
	if r.chat != nil {
		chats = append([]*importFile{{entry: r.chat, kind: "chat", suffix: ".chat.json"}}, chats...)
	}
	return append(files, chats...)
}

// importVideoFiles is the videos of files, in part order.
func importVideoFiles(files []*importFile) []*importFile {
	var videos []*importFile
	for _, f := range files {
		if f.kind == "video" {
			videos = append(videos, f)
		}
	}
	return videos
}

// importJobChat is the chat the job row names: the recording's own, else its
// first part's — the finalize's rule for a split recording whose parts carry
// their own chats.
func importJobChat(files []*importFile) *importFile {
	var first *importFile
	for _, f := range files {
		if f.kind != "chat" {
			continue
		}
		if f.part == 0 {
			return f
		}
		if first == nil {
			first = f
		}
	}
	return first
}

// importSegment is a split recording's part as the finalize records one: its
// index (the part number less one), file, size and own chat. The duration is
// the caller's to probe.
func importSegment(jobID string, video *importFile, files []*importFile) *database.Segment {
	abs, _ := filepath.Abs(video.dest)
	size := video.size
	seg := &database.Segment{
		JobID:        jobID,
		SegmentIndex: video.part - 1,
		Filename:     filepath.Base(video.dest),
		FilePath:     abs,
		FileSize:     &size,
	}
	for _, f := range files {
		if f.kind == "chat" && f.part == video.part {
			seg.ChatFile, _ = filepath.Abs(f.dest)
		}
	}
	return seg
}

// importProbeDuration reads a file's duration in seconds with ffprobe; 0 when
// it cannot (no ffprobe, or a file it cannot read). A variable so a test can
// stand one in.
var importProbeDuration = func(ctx context.Context, ffprobePath, file string) float64 {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json", "-show_format", file).Output()
	if err != nil {
		return 0
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	return d
}

// importFile is one file an import writes into imports/: a zip entry,
// extracted to a temporary name and then placed under the chosen stem plus
// its suffix — or adopted, when that name already holds the same bytes.
type importFile struct {
	entry  *zip.File
	kind   string // "video" or "chat": what a failure names
	suffix string // what follows the stem: ".mp4", ".chat.json", " - part2.mp4"
	part   int    // the split recording's part it belongs to; 0 for the whole

	tmp  string // this request's extraction; "" once placed or removed
	size int64
	sum  [sha256.Size]byte

	dest    string // the destination, once chooseImportStem picks it
	adopted bool   // dest already held these bytes; nothing is written there
	placed  bool   // this request created dest
}

// importPartialExt ends the temporary name an import extracts an entry to.
// The orphan sweep does not list it (it is not media, chat, thumbnail or
// description), and CleanupOldImportTemp removes one a hard abort left.
const importPartialExt = ".partial"

// extract writes the entry to a new temporary file in dir, hashing it on the
// way. On any failure the temporary file is removed.
func (f *importFile) extract(dir string) error {
	rc, err := f.entry.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.CreateTemp(dir, importTempPrefix+"*"+importPartialExt)
	if err != nil {
		return err
	}
	// CreateTemp's 0600 would follow the file into place; an archive is
	// 0644, as every other file the output directory holds.
	err = out.Chmod(0o644)

	// Limit to declared size + 1 byte to detect zip bombs that lie about UncompressedSize64
	h := sha256.New()
	limit := int64(f.entry.UncompressedSize64) + 1
	var n int64
	if err == nil {
		n, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, limit))
		if n >= limit {
			err = fmt.Errorf("zip entry %q exceeds declared size (zip bomb protection)", f.entry.Name)
		}
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(out.Name())
		return err
	}
	f.tmp, f.size = out.Name(), n
	h.Sum(f.sum[:0])
	return nil
}

// errImportNameTaken says a destination holds something other than the
// imported bytes.
var errImportNameTaken = errors.New("name taken")

// errImportInvalidPath is a destination outside the output directory.
var errImportInvalidPath = errors.New("invalid output path")

// claim checks dest for f: free, or already holding exactly f's bytes (same
// size and SHA-256) — an adopted file. Anything else there takes the name.
func (f *importFile) claim(dest string) (adopt bool, err error) {
	info, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != f.size {
		return false, errImportNameTaken
	}
	existing, err := os.Open(dest)
	if err != nil {
		return false, err
	}
	defer existing.Close()
	h := sha256.New()
	if _, err := io.Copy(h, existing); err != nil {
		return false, err
	}
	var sum [sha256.Size]byte
	if h.Sum(sum[:0]); sum != f.sum {
		return false, errImportNameTaken
	}
	return true, nil
}

// place moves f's temporary file to its destination. An adopted destination
// keeps its own file (the temporary one is dropped) — unless it went missing
// since it was checked, when the temporary file takes its place after all.
func (f *importFile) place() error {
	if f.adopted {
		if _, err := os.Lstat(f.dest); err == nil {
			os.Remove(f.tmp)
			f.tmp = ""
			return nil
		}
		f.adopted = false
	}
	if err := placeNoReplace(f.tmp, f.dest); err != nil {
		return err
	}
	f.tmp, f.placed = "", true
	return nil
}

// placeNoReplace moves tmp to dest unless dest exists. A hard link refuses an
// existing name where a rename would replace it; on a filesystem without hard
// links the rename runs after a check for dest instead.
func placeNoReplace(tmp, dest string) error {
	err := os.Link(tmp, dest)
	if err == nil {
		os.Remove(tmp)
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if _, statErr := os.Lstat(dest); !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", dest, errImportNameTaken)
	}
	return os.Rename(tmp, dest)
}

// importMaxDisambiguation is the last " (n)" chooseImportStem tries.
const importMaxDisambiguation = 999

// chooseImportNames picks every file's destination and returns the video's
// stem. The video files choose together — a recording's parts share one stem,
// as "<stem> - partN" — and each other file (a chat) on its own: a chat is
// re-adopted beside a re-adopted video, or beside one that took a " (n)"
// name, whenever its own name holds the same bytes, rather than written
// again because its sibling's name was taken.
func chooseImportNames(importsDir, outputDir, base string, files []*importFile) (string, error) {
	var videos []*importFile
	for _, f := range files {
		if f.kind == "video" {
			videos = append(videos, f)
		}
	}
	stem, err := chooseImportStem(importsDir, outputDir, base, videos)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.kind != "video" {
			if _, err := chooseImportStem(importsDir, outputDir, base, []*importFile{f}); err != nil {
				return "", err
			}
		}
	}
	return stem, nil
}

// chooseImportStem picks the stem a group of files is placed under: base,
// else "base (2)", "base (3)"… — the way uniqueTrimBasename names a second
// trim of the same range — the first whose every file is free or already
// holds the imported bytes. It sets each file's dest and adopted.
func chooseImportStem(importsDir, outputDir, base string, files []*importFile) (string, error) {
	for n := 1; n <= importMaxDisambiguation; n++ {
		stem := base
		if n > 1 {
			stem = fmt.Sprintf("%s (%d)", base, n)
		}
		usable := true
		for _, f := range files {
			dest := filepath.Join(importsDir, stem+f.suffix)
			// Belt and braces on top of the id validation: every WRITE goes
			// through the same canonical containment check the read routes
			// use, so a future change to how the stem is built cannot
			// re-open WEB-1. CanonicalPath resolves the nearest existing
			// ancestor, so this is valid on a path that does not exist yet.
			if _, ok := validatePathTraversal(dest, outputDir); !ok {
				return "", errImportInvalidPath
			}
			adopt, err := f.claim(dest)
			if errors.Is(err, errImportNameTaken) {
				usable = false
				break
			}
			if err != nil {
				return "", err
			}
			f.dest, f.adopted = dest, adopt
		}
		if usable {
			return stem, nil
		}
	}
	return "", fmt.Errorf("every name up to %q: %w", fmt.Sprintf("%s (%d)", base, importMaxDisambiguation), errImportNameTaken)
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

// importNameReserve is what an import's file name can carry past its
// "<title> [<id>]" stem: a " (999)" disambiguation, a " - part999" part
// suffix and the longest sibling suffix, ".chat.json".
const importNameReserve = len(" (999)") + len(" - part999") + len(".chat.json")

// importStem is the file stem an import writes: the sanitized title and
// " [<id>]". The title is cut by BYTES on a rune boundary, to the budget a
// recording's template gives it (config.TemplateTitleMaxBytes) and to what a
// 255-byte file name leaves once the id and importNameReserve are in. The
// sanitizer's cap counts runes, and a CJK title is three bytes a rune: a
// 90-character Japanese title made "<title> [<id>].mp4" too long to create,
// and at 78 characters the video fitted while its ".chat.json" did not.
func importStem(title, id string) string {
	budget := min(config.TemplateTitleMaxBytes, importNameMaxBytes-len(" ["+id+"]")-importNameReserve)
	safe := utils.SanitizeForFilename(title)
	if len(safe) > budget {
		// Sanitized again once cut, for what the cut can expose: trailing
		// dots or spaces, or a bare Windows device name. Its "_" guard is a
		// byte, which the -1 leaves room for.
		safe = utils.SanitizeForFilename(truncateUTF8(safe, budget-1))
	}
	return fmt.Sprintf("%s [%s]", safe, id)
}

// importNameMaxBytes is a file name's limit on Linux filesystems (NAME_MAX).
const importNameMaxBytes = 255

// importNameIDRe is a bracketed id in an archive's file name: a YouTube video
// id; a Moombox Twitch job's id, which its archives are named with
// (worker.go names a Twitch recording by job.ID: "tw_v<vod id>",
// "tw_<stream id>", "tw_manual_<login>_<n>"); or the "imp_" placeholder an
// earlier import minted (randomHex(4), the shape the dashboard's
// isImportPlaceholderId and the TUI's isImportPlaceholderID read), so
// re-importing an imported archive keeps its id. Every shape is path-safe —
// the id is interpolated into the output name.
var importNameIDRe = regexp.MustCompile(`\[(tw_[a-zA-Z0-9_]{1,64}|imp_[0-9a-f]{8}|[a-zA-Z0-9_-]{11})\]`)

// importTwitchVODRe is a Twitch VOD job's id, the one Twitch id shape that
// names a page of its own: twitch.tv/videos/<digits>.
var importTwitchVODRe = regexp.MustCompile(`^tw_v([0-9]+)$`)

// importNameID returns the id a file name's stem carries and the stem without
// it. The id is the LAST bracketed one: Moombox ("${title} [${id}]") and
// yt-dlp ("%(title)s [%(id)s]") both append it, and a title can carry a
// bracketed tag of the same shape ("[Holo-Live3D] Concert [dQw4w9WgXcQ]") —
// the first match took the tag for the id.
func importNameID(stem string) (id, rest string) {
	all := importNameIDRe.FindAllStringSubmatchIndex(stem, -1)
	if len(all) == 0 {
		return "", stem
	}
	m := all[len(all)-1]
	return stem[m[2]:m[3]], strings.Join(strings.Fields(stem[:m[0]]+" "+stem[m[1]:]), " ")
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// importTempPrefix names the temp file an upload is spooled to before it is
// opened as a zip, and the temporary file each entry is extracted to inside
// imports/ before it is placed (see the import route above).
const importTempPrefix = "moombox-import-"

// importPartialRe is the whole name of an entry's temporary extraction:
// os.CreateTemp's random part is decimal. No name an import places can take
// this shape — every one carries " [<id>]" — so the sweep below can never
// match an archive, whatever its title.
var importPartialRe = regexp.MustCompile(`^` + regexp.QuoteMeta(importTempPrefix) + `[0-9]+` + regexp.QuoteMeta(importPartialExt) + `$`)

// importTempMaxAge is how old a leftover must be before it is swept. No
// import runs for a day, so the age floor never touches one in flight.
const importTempMaxAge = 24 * time.Hour

// CleanupOldImportTemp removes import leftovers older than importTempMaxAge:
// spool files in the temp directory, and entries extracted into
// <outputDir>/imports but never placed. The route removes its own with a
// deferred os.Remove, but a hard abort mid-import (OS kill, power loss)
// leaves up to 500 MB behind in the one and 2 GB in the other.
func CleanupOldImportTemp(outputDir string) (removed int, err error) {
	removed, err = utils.RemoveStaleTempEntries(importTempMaxAge, importTempPrefix)
	if outputDir == "" {
		outputDir = "./output"
	}
	importsDir := filepath.Join(outputDir, "imports")
	entries, readErr := os.ReadDir(importsDir)
	if readErr != nil {
		if !errors.Is(readErr, fs.ErrNotExist) && err == nil {
			err = readErr
		}
		return removed, err
	}
	cutoff := time.Now().Add(-importTempMaxAge)
	for _, ent := range entries {
		if !ent.Type().IsRegular() || !importPartialRe.MatchString(ent.Name()) {
			continue
		}
		if info, infoErr := ent.Info(); infoErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(importsDir, ent.Name())) == nil {
			removed++
		}
	}
	return removed, err
}

// importChatMeta is what an import reads out of a chat archive's header: a
// YouTube archive's (chat.ChatData) videoId, videoTitle and channelName, or a
// Twitch archive's (twitch.TwitchChatData) platform, channelLogin and
// channelDisplayName.
type importChatMeta struct {
	VideoID     string
	VideoTitle  string
	ChannelName string

	Platform           string
	ChannelLogin       string
	ChannelDisplayName string
}

// importChatHeaders are the header sets readImportChatMeta stops at: either
// writer's three strings, all of which it always writes ahead of the
// messages.
var importChatHeaders = [][]string{
	{"videoId", "videoTitle", "channelName"},
	{"platform", "channelLogin", "channelDisplayName"},
}

// readImportChatMeta reads the top-level header strings of a chat archive as
// a stream. The whole file used to be read into memory and unmarshalled for
// three strings, and a long stream's chat runs to hundreds of MB. Moombox
// writes them ahead of the messages, so the read normally stops — once
// either writer's set is complete (importChatHeaders) — before reaching
// them; any value in between is skipped token by token, never held. A file
// that is not a JSON object, or ends early, yields whatever was found before
// that.
func readImportChatMeta(r io.Reader) importChatMeta {
	var meta importChatMeta
	dec := json.NewDecoder(r)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return meta
	}
	fields := map[string]*string{
		"videoId":            &meta.VideoID,
		"videoTitle":         &meta.VideoTitle,
		"channelName":        &meta.ChannelName,
		"platform":           &meta.Platform,
		"channelLogin":       &meta.ChannelLogin,
		"channelDisplayName": &meta.ChannelDisplayName,
	}
	found := map[string]bool{}
	complete := func() bool {
		for _, set := range importChatHeaders {
			if found[set[0]] && found[set[1]] && found[set[2]] {
				return true
			}
		}
		return false
	}
	for !complete() && dec.More() {
		t, err := dec.Token()
		if err != nil {
			return meta
		}
		key, _ := t.(string)
		if dst, ok := fields[key]; ok {
			if v, err := dec.Token(); err == nil {
				if str, isStr := v.(string); isStr {
					*dst = str
					found[key] = true
					continue
				}
				if d, isDelim := v.(json.Delim); isDelim && !skipJSONContainer(dec, d) {
					return meta
				}
				continue
			}
			return meta
		}
		if !skipJSONValue(dec) {
			return meta
		}
	}
	return meta
}

// skipJSONValue consumes the next value, however deeply nested, one token at
// a time. It reports false when the stream ends or breaks.
func skipJSONValue(dec *json.Decoder) bool {
	t, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := t.(json.Delim); ok {
		return skipJSONContainer(dec, d)
	}
	return true
}

// skipJSONContainer consumes the rest of the object or array whose opening
// delimiter open was just read.
func skipJSONContainer(dec *json.Decoder, open json.Delim) bool {
	if open != '{' && open != '[' {
		return true
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return true
}
