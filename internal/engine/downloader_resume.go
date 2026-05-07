package engine

import (
	"encoding/json"
	"os"
	"regexp"
	"time"
)

// streamIdentityPathRe extracts (videoID, itag) from a path-style YouTube
// media URL — DASH manifest output and HLS variant playlists share the
// `/id/<videoID>.<streamNumber>/itag/<itag>/` shape.
var streamIdentityPathRe = regexp.MustCompile(`/id/([\w-]+)\.\d+/itag/(\d+)/`)

// streamIdentityQueryIDRe / streamIdentityQueryItagRe extract id and itag
// from a query-style YouTube media URL — manifestless DASH adaptiveFormat
// URLs ship as `videoplayback?expire=...&id=<videoID>.<n>&itag=<itag>&...`.
// Both parameters appear at arbitrary positions in the query, so we match
// each independently rather than constraining adjacency.
var streamIdentityQueryIDRe = regexp.MustCompile(`[?&]id=([\w-]+)\.\d+`)
var streamIdentityQueryItagRe = regexp.MustCompile(`[?&]itag=(\d+)`)

// streamIdentity returns the videoID + itag fingerprint for a YouTube media
// URL, or "" when neither URL shape matches. Two URLs with the same
// fingerprint refer to the same logical stream variant even when every
// other path or query component has rotated (expire, ei, ip, ns, n, sig,
// pot, mt, mh, …).
//
// Tries path-style first (the manifest-driven DASH and HLS variant case),
// then falls back to query-style (manifestless DASH adaptiveFormat URLs).
// The fallback exists because the format pool URLs straight out of
// streamingData.adaptiveFormats[] are query-style, while
// engine.ParseDash output for manifest-driven streams is path-style.
func streamIdentity(rawURL string) string {
	if m := streamIdentityPathRe.FindStringSubmatch(rawURL); m != nil {
		return m[1] + "/" + m[2]
	}
	idMatch := streamIdentityQueryIDRe.FindStringSubmatch(rawURL)
	itagMatch := streamIdentityQueryItagRe.FindStringSubmatch(rawURL)
	if idMatch != nil && itagMatch != nil {
		return idMatch[1] + "/" + itagMatch[1]
	}
	return ""
}

// maxResumeStateAge is the oldest resume state we'll trust. Beyond this, we
// treat the file as stale (the downloader likely changed URL/quality since
// then, or the segment numbering has rolled) and start fresh. Seven days is
// generous enough for weekend-long outages without letting ancient state
// linger for months.
const maxResumeStateAge = 7 * 24 * time.Hour

// ResumeState holds download progress for crash recovery.
type ResumeState struct {
	LastSeq      int    `json:"lastSeq"`
	BytesWritten int64  `json:"bytesWritten"`
	Timestamp    int64  `json:"timestamp"`
	BaseURL      string `json:"baseUrl"`
}

func (d *SegmentDownloader) loadResume() (*ResumeState, error) {
	data, err := os.ReadFile(d.opts.ResumeFile)
	if err != nil {
		return nil, err
	}
	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	// Guard against empty / corrupted resume files that round-trip a zero
	// LastSeq + zero BytesWritten. saveResume() skips writing until seq > 0
	// but a manually corrupted file could still have this shape. Treating
	// it as valid would cause the caller to advance currentSeq to
	// LastSeq+1 = 1 and skip segment 0 entirely, losing the first segment
	// for YouTube live DASH (StartNumber=0) on resume.
	if state.LastSeq < 0 || (state.LastSeq == 0 && state.BytesWritten == 0) {
		return nil, nil
	}
	// Stale resume files are rejected. Timestamp == 0 is permitted (legacy
	// state files saved before the field was used); only explicit future
	// timestamps and > maxResumeStateAge in the past are considered stale.
	if state.Timestamp > 0 {
		age := time.Since(time.Unix(state.Timestamp, 0))
		if age > maxResumeStateAge {
			d.logger.Info("[Downloader] Resume state too old, starting fresh",
				"age", age, "maxAge", maxResumeStateAge)
			return nil, nil
		}
	}
	return &state, nil
}

// saveResume writes the current download state to a temp file and atomically
// renames it over the resume file to avoid corruption from crashes.
func (d *SegmentDownloader) saveResume() {
	seq := int(d.currentSeq.Load())
	if seq <= 0 {
		return // Nothing downloaded yet — no useful state to persist.
	}
	state := ResumeState{
		LastSeq:      seq - 1,
		BytesWritten: d.bytesWritten.Load(),
		Timestamp:    time.Now().Unix(),
		BaseURL:      d.getBaseURL(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmpFile := d.opts.ResumeFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		d.logger.Warn("[Downloader] Failed to write resume file", "file", tmpFile, "error", err)
		// Clean up partial temp file on failure
		os.Remove(tmpFile)
		return
	}
	if err := os.Rename(tmpFile, d.opts.ResumeFile); err != nil {
		d.logger.Warn("[Downloader] Failed to rename resume file", "from", tmpFile, "to", d.opts.ResumeFile, "error", err)
		os.Remove(tmpFile)
	}
}

// ClearResume removes the resume state file.
func (d *SegmentDownloader) ClearResume() {
	os.Remove(d.opts.ResumeFile)
}
