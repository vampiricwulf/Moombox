package engine

import (
	"encoding/json"
	"os"
	"time"
)

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
		return // Nothing downloaded yet (matching TS guard)
	}
	state := ResumeState{
		LastSeq:      seq - 1,
		BytesWritten: d.bytesWritten.Load(),
		Timestamp:    time.Now().Unix(),
		BaseURL:      d.opts.BaseURL,
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
