package worker

import (
	"os"
	"path/filepath"
)

// segmentFileNames contains the known filenames produced by download strategies.
var segmentFileNames = map[string]bool{
	"video_stream": true, // DASH video
	"audio_stream": true, // DASH audio
	"video.ts":     true, // HLS
	"video.mp4":    true, // VOD video
	"audio.m4a":    true, // VOD audio
}

// HasStagingFiles returns true if the staging directory for a job exists and is non-empty.
func HasStagingFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// HasSegmentFiles returns true if the staging directory contains recognized segment files.
func HasSegmentFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && segmentFileNames[e.Name()] {
			return true
		}
	}
	return false
}
