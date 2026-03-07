package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// normalizePath returns a cleaned absolute path suitable for map-key comparison.
// On Windows (case-insensitive filesystem), paths are lowercased.
func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// OrphanedEntry represents an orphaned file or directory found during scanning.
type OrphanedEntry struct {
	Path      string `json:"path"`                // Absolute path (for deletion)
	RelPath   string `json:"relPath"`             // Relative path (for display)
	Type      string `json:"type"`                // "staging", "output", or "trim"
	Size      int64  `json:"size"`                // Total size in bytes
	Modified  string `json:"modified"`            // ISO 8601 timestamp
	JobID     string `json:"jobId,omitempty"`     // Associated job ID (if found)
	JobTitle  string `json:"jobTitle,omitempty"`  // Job title (if found)
	JobStatus string `json:"jobStatus,omitempty"` // Job status (if found)
}

// ScanOrphanedFiles scans staging and output directories for orphaned files.
func ScanOrphanedFiles(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	var entries []OrphanedEntry

	// Scan staging directories
	stagingEntries, err := scanStagingOrphans(db, cfg)
	if err == nil {
		entries = append(entries, stagingEntries...)
	}

	// Scan output files
	outputEntries, err := scanOutputOrphans(db, cfg)
	if err == nil {
		entries = append(entries, outputEntries...)
	}

	// Scan trim files
	trimEntries, err := scanTrimOrphans(db, cfg)
	if err == nil {
		entries = append(entries, trimEntries...)
	}

	return entries, nil
}

// DeleteOrphanedFile safely deletes a file or directory if it's under the configured directories.
func DeleteOrphanedFile(path string, cfg *config.MoomboxConfig) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	// Verify path is under staging or output directory
	if !isUnderDirectory(absPath, resolveStagingDir(cfg)) && !isUnderDirectory(absPath, resolveOutputDir(cfg)) {
		return fmt.Errorf("path is not under staging or output directory")
	}

	// Check for symlink escape
	realPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		if !isUnderDirectory(realPath, resolveStagingDir(cfg)) && !isUnderDirectory(realPath, resolveOutputDir(cfg)) {
			return fmt.Errorf("path escapes configured directories via symlink")
		}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("path not found: %w", err)
	}

	if info.IsDir() {
		return os.RemoveAll(absPath)
	}
	return os.Remove(absPath)
}

func resolveStagingDir(cfg *config.MoomboxConfig) string {
	dir := cfg.Paths.StagingDirectory
	if dir == "" {
		dir = "./staging"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func resolveOutputDir(cfg *config.MoomboxConfig) string {
	dir := cfg.Paths.OutputDirectory
	if dir == "" {
		dir = "./output"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func isUnderDirectory(path, dir string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	// Normalize case on Windows for case-insensitive filesystem comparison
	if runtime.GOOS == "windows" {
		absPath = strings.ToLower(absPath)
		absDir = strings.ToLower(absDir)
	}
	// Normalize and ensure the path is strictly under the directory
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	// Reject paths that escape via ".."
	return !strings.HasPrefix(rel, "..") && rel != "."
}

// scanStagingOrphans scans the staging directory for orphaned subdirectories.
func scanStagingOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	stagingDir := cfg.Paths.StagingDirectory
	if stagingDir == "" {
		stagingDir = "./staging"
	}

	dirEntries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Active statuses — staging is in use for these
	activeStatuses := map[database.JobStatus]bool{
		database.StatusDownloading: true,
		database.StatusLive:        true,
		database.StatusUpcoming:    true,
		database.StatusMuxing:      true,
	}

	var entries []OrphanedEntry
	absStagingDir, _ := filepath.Abs(stagingDir)

	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}

		jobID := de.Name()
		absPath := filepath.Join(absStagingDir, jobID)

		// Check if job exists and its status
		job, err := db.GetJob(jobID)
		if err == nil && job != nil && activeStatuses[job.Status] {
			// Active job — skip, staging is in use
			continue
		}

		// Compute directory size and modified time
		size, modified := dirSizeAndModified(absPath)

		relPath, _ := filepath.Rel(absStagingDir, absPath)
		if relPath == "" {
			relPath = jobID
		}

		entry := OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "staging",
			Size:     size,
			Modified: modified.UTC().Format(time.RFC3339),
		}

		if job != nil {
			entry.JobID = job.ID
			entry.JobTitle = job.Title
			entry.JobStatus = string(job.Status)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// scanOutputOrphans scans the output directory for files not referenced by any job.
func scanOutputOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	outputDir := cfg.Paths.OutputDirectory
	if outputDir == "" {
		outputDir = "./output"
	}

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(absOutputDir); os.IsNotExist(err) {
		return nil, nil
	}

	// Collect all known output and chat file paths from DB.
	// Uses normalizePath for case-insensitive comparison on Windows.
	jobs, err := db.GetAllJobs()
	if err != nil {
		return nil, err
	}

	knownFiles := make(map[string]bool)
	for _, job := range jobs {
		if job.OutputFile != "" {
			knownFiles[normalizePath(job.OutputFile)] = true
		}
		if job.ChatFile != "" {
			knownFiles[normalizePath(job.ChatFile)] = true
		}
		if job.ThumbnailFile != "" {
			knownFiles[normalizePath(job.ThumbnailFile)] = true
		}
		if job.DescriptionFile != "" {
			knownFiles[normalizePath(job.DescriptionFile)] = true
		}
	}

	var entries []OrphanedEntry

	err = filepath.Walk(absOutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			// Skip trim directories — handled separately
			if info.Name() == "trim" {
				return filepath.SkipDir
			}
			return nil
		}

		// Only check media, chat, thumbnail, and description files
		ext := strings.ToLower(filepath.Ext(path))
		isMedia := ext == ".mp4" || ext == ".mkv" || ext == ".webm" || ext == ".ts"
		isThumbnail := ext == ".jpg" || ext == ".webp" || ext == ".png"
		isChat := strings.HasSuffix(strings.ToLower(path), ".chat.json")
		isDescription := ext == ".description"
		if !isMedia && !isThumbnail && !isChat && !isDescription {
			return nil
		}

		absPath, _ := filepath.Abs(path)
		if knownFiles[normalizePath(absPath)] {
			return nil // Referenced by a job
		}

		relPath, _ := filepath.Rel(absOutputDir, absPath)

		entries = append(entries, OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "output",
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})

		return nil
	})
	if err != nil {
		return entries, err
	}

	return entries, nil
}

// scanTrimOrphans scans for trim files not referenced by any DB trim record.
func scanTrimOrphans(db *database.Database, cfg *config.MoomboxConfig) ([]OrphanedEntry, error) {
	outputDir := cfg.Paths.OutputDirectory
	if outputDir == "" {
		outputDir = "./output"
	}

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(absOutputDir); os.IsNotExist(err) {
		return nil, nil
	}

	// Collect all known trim filenames from DB
	jobs, err := db.GetAllJobs()
	if err != nil {
		return nil, err
	}

	knownTrimFiles := make(map[string]bool)
	for _, job := range jobs {
		trims, err := db.GetTrimsForJob(job.ID)
		if err != nil {
			continue
		}
		for _, tr := range trims {
			// Resolve trim path: relative to output dir
			trimAbs := tr.Filename
			if !filepath.IsAbs(trimAbs) {
				trimAbs = filepath.Join(absOutputDir, trimAbs)
			}
			knownTrimFiles[normalizePath(trimAbs)] = true
		}
	}

	var entries []OrphanedEntry

	// Walk looking for */trim/*.mp4 patterns
	err = filepath.Walk(absOutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		// Only consider files inside "trim" directories
		dir := filepath.Dir(path)
		if filepath.Base(dir) != "trim" {
			return nil
		}

		absPath, _ := filepath.Abs(path)
		if knownTrimFiles[normalizePath(absPath)] {
			return nil // Referenced by a trim record
		}

		relPath, _ := filepath.Rel(absOutputDir, absPath)

		entries = append(entries, OrphanedEntry{
			Path:     absPath,
			RelPath:  relPath,
			Type:     "trim",
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})

		return nil
	})
	if err != nil {
		return entries, err
	}

	return entries, nil
}

// dirSizeAndModified computes total size and latest modification time for a directory.
func dirSizeAndModified(dir string) (int64, time.Time) {
	var totalSize int64
	var latestMod time.Time

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
		}
		return nil
	})

	if latestMod.IsZero() {
		latestMod = time.Now()
	}

	return totalSize, latestMod
}
