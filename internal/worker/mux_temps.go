package worker

import (
	"path/filepath"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// RemoveStaleMuxTemps is the start-up sweep for what a crash or a kill
// mid-mux leaves: the temporary file FFmpeg was writing beside the archive
// (engine.Muxer.writeOutput), wearing the run token of another process. It
// runs once per start, beside the trim and import temp sweeps, and removes
// only those whose writer is gone — whose writer's lock it can take
// (engine.RemoveStaleMuxTemps). This run's own temporary files — a boot
// re-mux already under way — carry this run's token and are never touched,
// and a file another live process is writing (a second Moombox sharing the
// output tree) holds its lock and is left to it.
//
// The places it looks are every place a job can have written an archive:
// the output directory as configured now and each job's own
// output_directory, walked whole (the template puts archives in a
// subdirectory), and — one level each — the directories every recorded file
// is in, which outlive a change of output_directory: a split job's later
// parts are written beside its first, wherever that went. A row that cannot
// be read costs only its own directories.
//
// Every removal is logged with its path.
func RemoveStaleMuxTemps(db *database.Database, outputDir string, log logger) int {
	trees, dirs := muxTempRoots(db, outputDir)
	removed, live, err := engine.RemoveStaleMuxTemps(trees, dirs)
	for _, p := range removed {
		log.Info("boot cleanup: removed a temporary file an interrupted FFmpeg run left",
			"path", p, "reason", "a crash or a kill stopped the mux that was writing it; the archive it was becoming is muxed again from staging")
	}
	for _, p := range live {
		log.Debug("boot cleanup: left a temporary file another running Moombox process is writing", "path", p)
	}
	if err != nil {
		log.Debug("boot cleanup: could not read a directory for interrupted FFmpeg runs' temporary files", "err", err)
	}
	return len(removed)
}

// muxTempRoots returns the trees and the single directories RemoveStaleMuxTemps
// reads: outputDir ("./output" when unset, as buildJobContext defaults it) and
// every job's output_directory, and the directory of every output_file and
// part file_path not already inside one of those trees.
func muxTempRoots(db *database.Database, outputDir string) (trees, dirs []string) {
	if outputDir == "" {
		outputDir = "./output"
	}
	trees = []string{filepath.Clean(outputDir)}
	var files []string
	if db != nil {
		if jobs, err := db.GetAllJobs(); err == nil {
			for _, j := range jobs {
				if j.OutputDirectory != "" {
					trees = append(trees, filepath.Clean(j.OutputDirectory))
				}
				if j.OutputFile != "" {
					files = append(files, j.OutputFile)
				}
				for _, s := range j.Segments {
					if s.FilePath != "" {
						files = append(files, s.FilePath)
					}
				}
			}
		}
	}
	trees = outermostDirs(trees)
	for _, f := range files {
		if d := filepath.Dir(f); !withinAny(d, trees) && !withinAny(d, dirs) {
			dirs = append(dirs, d)
		}
	}
	return trees, dirs
}

// outermostDirs drops duplicates and every directory inside another of dirs,
// so each tree is walked once.
func outermostDirs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if withinAny(d, out) {
			continue
		}
		inside := false
		for _, t := range dirs {
			inside = inside || isUnderDirectory(d, t)
		}
		if !inside {
			out = append(out, d)
		}
	}
	return out
}

// withinAny reports whether dir is one of trees or below one, compared as
// normalizePath spells them.
func withinAny(dir string, trees []string) bool {
	for _, t := range trees {
		if normalizePath(dir) == normalizePath(t) || isUnderDirectory(dir, t) {
			return true
		}
	}
	return false
}
