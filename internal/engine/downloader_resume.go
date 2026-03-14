package engine

import (
	"encoding/json"
	"os"
	"time"
)

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
