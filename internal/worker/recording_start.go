package worker

import (
	"context"
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
// the file over (a set-aside, a discarded partial) drops the entry when it
// opens the file empty and writes its own (recordStartsOf).
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

// forgetRecordingStart drops the staged file at path's entry, so the file
// reads as of unknown start until its first media write records one. Best
// effort, like noteRecordingStart: an entry that cannot be dropped is
// overwritten by that write all the same.
func forgetRecordingStart(path string) {
	if path == "" {
		return
	}
	recordingStartMu.Lock()
	defer recordingStartMu.Unlock()
	dir := filepath.Dir(path)
	starts := readRecordingStarts(dir)
	if _, ok := starts[filepath.Base(path)]; !ok {
		return
	}
	delete(starts, filepath.Base(path))
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
//
// An entry describes the recording that was in the file when it was
// written, and a file of the same name is started over — set aside by the
// engine's no-truncate guard or setAsideStagedMedia, a discarded partial —
// with that entry still in the record. So a downloader that opens its file
// empty (OnStart with resuming false) first drops the file's entry, and the
// fresh recording is of unknown start until its own first media write
// records one: a fresh file whose writer reports no start, or whose first
// write never comes, is never read as the earlier recording's sq=0. A
// listener a caller installed on OnStart still runs, after the drop.
func recordStartsOf(result *DownloadResult) {
	for _, pair := range []struct {
		d    *engine.SegmentDownloader
		path string
	}{{result.VideoDownloader, result.VideoPath}, {result.AudioDownloader, result.AudioPath}} {
		if pair.d == nil || pair.path == "" || pair.d.OnFirstMedia != nil {
			continue
		}
		path := pair.path
		prev := pair.d.OnStart
		pair.d.OnStart = func(seq int, resuming bool) {
			if !resuming {
				forgetRecordingStart(path)
			}
			if prev != nil {
				prev(seq, resuming)
			}
		}
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

// joinOffsetSeconds is where in the broadcast a live YouTube capture's
// single-file recording began, in seconds (owner decision CJ-3: the row's
// join_offset_seconds, which the chapter import's offset pre-fills from);
// ok is false when it is not known. Asked by the finalize for a live YouTube
// job only: a VOD's archive is the video's own timeline, which the import's
// 0 already is, and a Twitch capture's comments count in a VOD Moombox does
// not hold.
//
// The start comes from what the staged recording already records:
//
//   - every input recorded from sq=0 (recordedFromStart) is 0;
//   - else the first input whose recordingStartFile entry is past 0 joined
//     late, and where it joined is read from the segment's own timing when
//     the file carries it: a fragmented MP4 (YouTube's DASH) keeps each
//     fragment's decode time on the broadcast's timeline, so ffprobe's
//     start_time of the staged file IS the join point (the same reading
//     ffprobeData.spanSec corrects for) — exact even where segments ran
//     long or short;
//   - failing that (an MPEG-TS from the HLS path, whose start_time is a PTS
//     the broadcast's timeline does not share), the entry's sequence times
//     the stream's advertised segment duration (Format.TargetDurationSec,
//     the cadence yt-dlp's live downloader paces itself by, _video.py
//     b77c9304e) — a sequence N began N segments into the broadcast;
//   - an input with no entry, or no duration to multiply by, is unknown:
//     never a guess, as recordedFromStart never guesses.
func (o *DownloadOrchestrator) joinOffsetSeconds(ctx context.Context, result *DownloadResult, videoPath, audioPath string) (float64, bool) {
	if recordedFromStart(videoPath, audioPath) {
		return 0, true
	}
	for _, in := range []struct {
		path   string
		target int
	}{{videoPath, formatTargetDuration(result, true)}, {audioPath, formatTargetDuration(result, false)}} {
		if in.path == "" {
			continue
		}
		seq, ok := readRecordingStarts(filepath.Dir(in.path))[filepath.Base(in.path)]
		if !ok || seq <= 0 {
			continue
		}
		if p := o.runFFprobe(ctx, in.path); p != nil && isMovContainer(p.FormatName) && p.StartSec > 0 {
			return p.StartSec, true
		}
		if in.target > 0 {
			return float64(seq * in.target), true
		}
		return 0, false
	}
	return 0, false
}

// formatTargetDuration is the advertised per-segment duration of the format
// result's video (or audio) downloader fetched, 0 when it is not known.
func formatTargetDuration(result *DownloadResult, video bool) int {
	if result == nil {
		return 0
	}
	f := result.AudioFormat
	if video {
		f = result.VideoFormat
	}
	if f == nil {
		return 0
	}
	return f.TargetDurationSec
}
