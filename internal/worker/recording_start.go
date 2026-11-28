package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
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

// recordingStartMu serialises the read-modify-write of recordingStartFile and
// mediaStartFile: a job's video and audio downloaders report their first
// segment from two goroutines at once, into the same dir.
var recordingStartMu sync.Mutex

// mediaStartFile records, in a staging dir, the wall clock of each staged
// recording's first segment (engine.SegmentDownloader.OnFirstMediaWallclock:
// a live YouTube DASH segment's Ingestion-Walltime-Us, an HLS segment's
// #EXT-X-PROGRAM-DATE-TIME), as RFC 3339 with sub-second digits, keyed by the
// file's base name as recordingStartFile is. It follows recordingStartFile's
// rules exactly: written once per file a downloader starts, left alone by a
// resume or an append, dropped when a downloader opens the file empty. A
// sibling file and not a field of recordingStartFile, so a staging dir an
// older binary left still reads.
//
// FFmpeg's mux moves each input's first timestamp to 0, so the entry is
// the wall clock of the archive's time zero: the job's media_start_time,
// which the dashboard player lines chat up with (owner ruling RV-19).
const mediaStartFile = "media-start.json"

// readRecordingStarts returns dir's recordingStartFile entries, or nil when
// it has none or cannot be read.
func readRecordingStarts(dir string) map[string]int {
	return readStagingRecord[int](dir, recordingStartFile)
}

// noteRecordingStart records that the staged file at path began at seq.
// Best effort: a write that fails leaves the file's start unknown, which
// costs it only the chapters a known start at sq=0 would have earned.
func noteRecordingStart(path string, seq int) {
	noteStagingRecord(path, recordingStartFile, seq)
}

// forgetRecordingStart drops the staged file at path's entry, so the file
// reads as of unknown start until its first media write records one. Best
// effort, like noteRecordingStart: an entry that cannot be dropped is
// overwritten by that write all the same.
func forgetRecordingStart(path string) {
	forgetStagingRecord[int](path, recordingStartFile)
}

// readStagingRecord returns the entries of dir's per-file record name
// (recordingStartFile, mediaStartFile), or nil when it has none or cannot
// be read.
func readStagingRecord[T any](dir, name string) map[string]T {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	var entries map[string]T
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	return entries
}

// noteStagingRecord sets the staged file at path's entry in its dir's
// record name to v, keeping every other file's. Best effort.
func noteStagingRecord[T any](path, name string, v T) {
	if path == "" {
		return
	}
	recordingStartMu.Lock()
	defer recordingStartMu.Unlock()
	dir := filepath.Dir(path)
	entries := readStagingRecord[T](dir, name)
	if entries == nil {
		entries = map[string]T{}
	}
	entries[filepath.Base(path)] = v
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = utils.WriteFileAtomic(filepath.Join(dir, name), data, 0o644)
}

// forgetStagingRecord drops the staged file at path's entry from its dir's
// record name. Best effort.
func forgetStagingRecord[T any](path, name string) {
	if path == "" {
		return
	}
	recordingStartMu.Lock()
	defer recordingStartMu.Unlock()
	dir := filepath.Dir(path)
	entries := readStagingRecord[T](dir, name)
	if _, ok := entries[filepath.Base(path)]; !ok {
		return
	}
	delete(entries, filepath.Base(path))
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = utils.WriteFileAtomic(filepath.Join(dir, name), data, 0o644)
}

// recordStartsOf has each of result's downloaders record the sequence its
// file begins at (noteRecordingStart) and that first segment's wall clock
// (noteMediaStart), each unless something else already listens for it.
// Installed by runDownloaders ahead of Start, so every capture it runs —
// live, post-live and VOD alike — leaves the records.
//
// An entry describes the recording that was in the file when it was
// written, and a file of the same name is started over — set aside by the
// engine's no-truncate guard or setAsideStagedMedia, a discarded partial —
// with that entry still in the record. So a downloader that opens its file
// empty (OnStart with resuming false) first drops the file's entries, and
// the fresh recording is of unknown start until its own first media write
// records one: a fresh file whose writer reports no start, or whose first
// write never comes, is never read as the earlier recording's. A listener a
// caller installed on OnStart still runs, after the drop.
func recordStartsOf(result *DownloadResult) {
	for _, pair := range []struct {
		d    *engine.SegmentDownloader
		path string
	}{{result.VideoDownloader, result.VideoPath}, {result.AudioDownloader, result.AudioPath}} {
		if pair.d == nil || pair.path == "" {
			continue
		}
		ownStart, ownWall := pair.d.OnFirstMedia == nil, pair.d.OnFirstMediaWallclock == nil
		if !ownStart && !ownWall {
			continue
		}
		path := pair.path
		prev := pair.d.OnStart
		pair.d.OnStart = func(seq int, resuming bool) {
			if !resuming {
				if ownStart {
					forgetRecordingStart(path)
				}
				if ownWall {
					forgetStagingRecord[string](path, mediaStartFile)
				}
			}
			if prev != nil {
				prev(seq, resuming)
			}
		}
		if ownStart {
			pair.d.OnFirstMedia = func(seq int) { noteRecordingStart(path, seq) }
		}
		if ownWall {
			pair.d.OnFirstMediaWallclock = func(wall time.Time) { noteMediaStart(path, wall) }
		}
	}
}

// noteMediaStart records that the staged file at path's first segment has
// the wall clock wall (mediaStartFile). Best effort: a write that fails
// leaves the archive unanchored, and the player falls back to the join
// offset.
func noteMediaStart(path string, wall time.Time) {
	noteStagingRecord(path, mediaStartFile, wall.UTC().Format(time.RFC3339Nano))
}

// mediaStartTime is the wall clock of a single-file mux's time zero, for the
// job's media_start_time (owner ruling RV-19): the video input's
// mediaStartFile entry, which is what the picture starts at. A video that
// carries no wall clock (a WebM segment has no readable metadata) takes the
// audio's entry only when the two are known to have begun at the same
// sequence (recordingStartFile), or when there is no video input; otherwise
// it is unknown — never a guess. ok is false when it is not known.
func mediaStartTime(videoPath, audioPath string) (string, bool) {
	entry := func(p string) (string, bool) {
		if p == "" {
			return "", false
		}
		v, ok := readStagingRecord[string](filepath.Dir(p), mediaStartFile)[filepath.Base(p)]
		if !ok {
			return "", false
		}
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			return "", false
		}
		return v, true
	}
	if v, ok := entry(videoPath); ok {
		return v, true
	}
	if videoPath != "" {
		vs, vok := readRecordingStarts(filepath.Dir(videoPath))[filepath.Base(videoPath)]
		as, aok := readRecordingStarts(filepath.Dir(audioPath))[filepath.Base(audioPath)]
		if audioPath == "" || !vok || !aok || vs != as {
			return "", false
		}
	}
	return entry(audioPath)
}

// splitMediaStartTime is mediaStartTime for a split archive: part 1's, read
// from its own staging dir as splitJoinOffsetSeconds reads its join point.
// The split archive's chat runs on one timeline from part 1's start, so a
// later part, a resume or a quality split never moves it.
func splitMediaStartTime(jobCtx *JobContext, segments []database.Segment) (string, bool) {
	media := partOneStagingMedia(jobCtx, segments)
	if media == nil {
		return "", false
	}
	return mediaStartTime(media.VideoPath, media.AudioPath)
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
// join_offset_seconds, which the chapter import's offset pre-fills from;
// a split archive's is part 1's, splitJoinOffsetSeconds, CK-3); ok is
// false when it is not known. Asked by the finalize for a live YouTube
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
//     b77c9304e) — a sequence N began N segments into the broadcast. The
//     live HLS path fetches no player-response format, so its result
//     carries the extraction's cadence as SegmentDurationSec instead
//     (advertisedSegmentDuration); a Mux action from staging has neither;
//   - an input with neither is unknown, and the next late input is asked;
//     none answering is unknown: never a guess, as recordedFromStart never
//     guesses.
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
	}
	return 0, false
}

// splitJoinOffsetSeconds is joinOffsetSeconds for a split archive (owner
// decision CK-3): where in the broadcast part 1 — segments[0], the first
// part the archive keeps — began, read from that part's own staged recording
// and its recordingStartFile entry by the single-file rules: 0 when it was
// recorded from sq=0, the fragmented MP4's start_time or its first sequence
// times the advertised segment duration for a late join, unknown otherwise.
// Part 1 is staged in the job's staging root (part 0, unless a seg_0 dir
// exists: rootIsPartZero) or in seg_<its index> — a part merge folds a run's
// later parts into its first and keeps the first's dir, and the dirs stay
// until the finalize's staging cleanup. result is the finalize's download
// result, whose advertised cadence is the broadcast's (one live broadcast's
// formats share it, as advertisedSegmentDuration relies on); nil from the
// Mux action's staging, which leaves the fragments' own timing and sq=0.
//
// The chapter import lays the parts end to end from this point. Whatever
// fell between two parts — an outage, the seconds a quality split spent
// re-opening — is in the broadcast's timeline but in no part, so a comment's
// timestamps past a gap land later in the archive than they should by the
// gap's length: not compensated, as the import's single offset cannot.
func (o *DownloadOrchestrator) splitJoinOffsetSeconds(ctx context.Context, jobCtx *JobContext, segments []database.Segment, result *DownloadResult) (float64, bool) {
	media := partOneStagingMedia(jobCtx, segments)
	if media == nil {
		return 0, false
	}
	return o.joinOffsetSeconds(ctx, result, media.VideoPath, media.AudioPath)
}

// partOneStagingMedia is the staged recording of a split archive's part 1 —
// segments[0], the first part the archive keeps — in the job's staging root
// (part 0, unless a seg_0 dir exists: rootIsPartZero) or in seg_<its index>;
// nil when there is none.
func partOneStagingMedia(jobCtx *JobContext, segments []database.Segment) *DownloadResult {
	if len(segments) == 0 || jobCtx.StagingDir == "" {
		return nil
	}
	dir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segments[0].SegmentIndex))
	if segments[0].SegmentIndex == 0 && rootIsPartZero(jobCtx.StagingDir) {
		dir = jobCtx.StagingDir
	}
	return discoverStagingMedia(dir)
}

// formatTargetDuration is the advertised per-segment duration of the format
// result's video (or audio) downloader fetched, else the result's own
// SegmentDurationSec (the live HLS path's); 0 when it is not known.
func formatTargetDuration(result *DownloadResult, video bool) int {
	if result == nil {
		return 0
	}
	f := result.AudioFormat
	if video {
		f = result.VideoFormat
	}
	if f == nil || f.TargetDurationSec <= 0 {
		return result.SegmentDurationSec
	}
	return f.TargetDurationSec
}

// advertisedSegmentDuration is the per-segment duration a live extraction's
// adaptive formats advertise (targetDurationSec), from the first of infos
// that carries one: a video format's first, else any format's; 0 when none
// does. The formats of one live broadcast share the cadence its sequence
// numbers count in, as selectedVideoTargetDuration relies on for the
// eviction math, so any one is the HLS variant's too.
func advertisedSegmentDuration(infos ...*youtube.VideoInfo) int {
	for _, info := range infos {
		if info == nil {
			continue
		}
		for i := range info.Formats {
			if f := &info.Formats[i]; f.IsVideo() && f.TargetDurationSec > 0 {
				return f.TargetDurationSec
			}
		}
		for i := range info.Formats {
			if f := &info.Formats[i]; f.TargetDurationSec > 0 {
				return f.TargetDurationSec
			}
		}
	}
	return 0
}
