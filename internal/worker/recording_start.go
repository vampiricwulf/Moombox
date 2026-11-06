package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// recordingStartFile records, in a staging dir, the sequence each staged
// recording there began at: the media sequence of the first segment its
// downloader wrote into it when it opened the file empty
// (engine.SegmentDownloader.OnFirstMedia), keyed by the file's base name
// ("video_stream", "audio_stream", "video.ts"). An entry is written once per
// file a downloader starts and left alone by every resume and append, which
// continue a file that began where the entry says; a downloader that starts
// the file over (a set-aside, a discarded partial) writes its own.
//
// It is what tells a finalize that a live YouTube capture recorded from
// sq=0 — the broadcast's own start, which shares the VOD's timeline, so the
// video's description chapters line up with it (owner decision FE-18) —
// from one that joined late (the live edge, a split, a restart past the
// head), whose chapters would sit at the wrong times. A recording with no
// entry — one started before this record existed, or whose entry could not
// be written — is of unknown start, and is treated as late.
const recordingStartFile = "recording-start.json"

// recordingStartMu serialises the read-modify-write of recordingStartFile:
// a job's video and audio downloaders report their first segment from two
// goroutines at once, into the same dir.
var recordingStartMu sync.Mutex

// readRecordingStarts returns dir's recordingStartFile entries, or nil when
// it has none or cannot be read.
func readRecordingStarts(dir string) map[string]int {
	data, err := os.ReadFile(filepath.Join(dir, recordingStartFile))
	if err != nil {
		return nil
	}
	var starts map[string]int
	if json.Unmarshal(data, &starts) != nil {
		return nil
	}
	return starts
}

// noteRecordingStart records that the staged file at path began at seq.
// Best effort: a write that fails leaves the file's start unknown, which
// costs it only the chapters a known start at sq=0 would have earned.
func noteRecordingStart(path string, seq int) {
	if path == "" {
		return
	}
	recordingStartMu.Lock()
	defer recordingStartMu.Unlock()
	dir := filepath.Dir(path)
	starts := readRecordingStarts(dir)
	if starts == nil {
		starts = map[string]int{}
	}
	starts[filepath.Base(path)] = seq
	data, err := json.Marshal(starts)
	if err != nil {
		return
	}
	_ = utils.WriteFileAtomic(filepath.Join(dir, recordingStartFile), data, 0o644)
}

// recordStartsOf has each of result's downloaders record the sequence its
// file begins at (noteRecordingStart), unless something else already
// listens. Installed by runDownloaders ahead of Start, so every capture it
// runs — live, post-live and VOD alike — leaves the record.
func recordStartsOf(result *DownloadResult) {
	for _, pair := range []struct {
		d    *engine.SegmentDownloader
		path string
	}{{result.VideoDownloader, result.VideoPath}, {result.AudioDownloader, result.AudioPath}} {
		if pair.d == nil || pair.path == "" || pair.d.OnFirstMedia != nil {
			continue
		}
		path := pair.path
		pair.d.OnFirstMedia = func(seq int) { noteRecordingStart(path, seq) }
	}
}

// recordedFromStart reports whether every staged input of a single-file mux
// is known to have begun at sequence 0 (recordingStartFile): at least one
// input, and each with an entry of 0 in its own dir's record. An input with
// no entry is of unknown start and answers false — never a guess.
func recordedFromStart(paths ...string) bool {
	found := false
	for _, p := range paths {
		if p == "" {
			continue
		}
		seq, ok := readRecordingStarts(filepath.Dir(p))[filepath.Base(p)]
		if !ok || seq != 0 {
			return false
		}
		found = true
	}
	return found
}
