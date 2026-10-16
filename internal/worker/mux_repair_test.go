package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// W30-A11: a live DASH capture is staged as one fMP4 stream — the init, then
// every fragment appended as it arrived — and one damaged fragment mid-file
// made `-c copy` stop there with exit 0. The input probe stopped at the same
// fragment, the shortfall check compared the copy with itself, the row went
// Finished over the part before the damage, and staging, the only copy of
// everything after it, was deleted. These tests stage the shape the engine
// writes, from a real FFmpeg fragmented MP4 split into its init and its
// fragments, and drive it through the real mux paths.

// fragmentedSource is one generated recording, split the way the engine
// stages it: the init segment and each fragment's bytes.
type fragmentedSource struct {
	init  []byte
	frags [][]byte
}

var (
	fragmentedSourcesMu sync.Mutex
	fragmentedSources   = map[string]fragmentedSource{}
)

// fragmentedFixture renders seconds of "video" (5 fps, a keyframe and so a
// fragment every 2 s), "audio" (AAC, a fragment every 2 s) or "muxed" (both
// in one file, every moof carrying a video traf and an audio traf — the
// Twitch fMP4 shape, an EXT-X-MAP init then a muxed variant's fragments) as a
// fragmented MP4 — -movflags frag_keyframe+empty_moov+default_base_moof, the
// shape of a DASH representation — and splits it into its init (ftyp+moov)
// and its moof+mdat fragments. FFmpeg's closing mfra index is left off: the
// engine never stages one. Cached per test binary: every test here wants the
// same few recordings.
func fragmentedFixture(t *testing.T, ffmpegPath, kind string, seconds int) fragmentedSource {
	t.Helper()
	key := fmt.Sprint(kind, seconds)
	fragmentedSourcesMu.Lock()
	defer fragmentedSourcesMu.Unlock()
	if src, ok := fragmentedSources[key]; ok {
		return src
	}
	path := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-nostdin", "-y"}
	video := []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=5:duration=%d", seconds)}
	audio := []string{"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds)}
	h264 := []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "10", "-keyint_min", "10", "-sc_threshold", "0"}
	switch kind {
	case "audio":
		args = append(append(args, audio...), "-c:a", "aac", "-frag_duration", "2000000",
			"-movflags", "+empty_moov+default_base_moof")
	case "muxed":
		args = append(append(append(append(args, video...), audio...), h264...), "-c:a", "aac",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof")
	default:
		args = append(append(append(args, video...), h264...), "-movflags", "+frag_keyframe+empty_moov+default_base_moof")
	}
	args = append(args, "-f", "mp4", path)
	if out, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate the fragmented fixture: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var src fragmentedSource
	var starts []int
	end := len(data)
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if size < 8 {
			t.Fatalf("fixture box %q at %d has size %d", typ, off, size)
		}
		switch typ {
		case "moof":
			starts = append(starts, off)
		case "mfra":
			end = off
		}
		off += size
	}
	if len(starts) < 10 {
		t.Fatalf("fixture has %d fragments, want a fragment every 2 s", len(starts))
	}
	src.init = data[:starts[0]]
	for i, s := range starts {
		e := end
		if i+1 < len(starts) {
			e = starts[i+1]
		}
		src.frags = append(src.frags, data[s:e])
	}
	fragmentedSources[key] = src
	return src
}

// damage is how stageFragmented spoils one fragment.
type damage int

const (
	damageNone damage = iota
	damageZeroed
	damageTorn // half its bytes, then the next fragment appended
	damageHTML // an error page served with 200 in its place
	// damageTornMoof cuts the fragment 4 bytes short of its moof's end —
	// inside the trun's sample table, the moof's last box — and appends the
	// next fragment: the moof still parses, its table reading on into the
	// next fragment's bytes.
	damageTornMoof
)

// stageFragmented writes src to path as the engine stages it — the init, then
// each fragment appended — with fragment bad spoiled by how.
func stageFragmented(t *testing.T, path string, src fragmentedSource, bad int, how damage) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(src.init)
	for i, f := range src.frags {
		if i == bad {
			switch how {
			case damageZeroed:
				f = make([]byte, len(f))
			case damageTorn:
				f = f[:len(f)/2]
			case damageHTML:
				f = []byte("<html><head><title>Error</title></head><body>Service unavailable</body></html>\n")
			case damageTornMoof:
				f = f[:binary.BigEndian.Uint32(f)-4]
			}
		}
		buf.Write(f)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// probedStreams is one stream of an output as the assertions read it.
type probedStreams map[string]struct {
	seconds float64
	frames  int
}

// probeStreams reads an output's streams with ffprobe directly — the
// frame count is what shows the fragments after the damage made it across.
func probeStreams(t *testing.T, ffprobePath, path string) probedStreams {
	t.Helper()
	out, err := exec.Command(ffprobePath, "-v", "quiet", "-print_format", "json", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Duration  string `json:"duration"`
			NbFrames  string `json:"nb_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	streams := probedStreams{}
	for _, s := range p.Streams {
		d, _ := strconv.ParseFloat(s.Duration, 64)
		n, _ := strconv.Atoi(s.NbFrames)
		streams[s.CodecType] = struct {
			seconds float64
			frames  int
		}{d, n}
	}
	return streams
}

// routedRepairJob is muxFixtureJob with both loggers routed into the job's
// own log, tracked as cmd/moombox tracks it — so a test can read what the
// operator reads in the job dialog.
func routedRepairJob(t *testing.T, w *DownloadWorker, db *database.Database, jobID string) (stagingDir, outputDir string) {
	t.Helper()
	w.logger = routingLogger{db: db}
	w.orchestrator.logger = routingLogger{db: db}
	untrackOnTerminal(t, db)
	stagingDir, outputDir = muxFixtureJob(t, w, db, jobID)
	db.TrackJobForLogs(jobID)
	return stagingDir, outputDir
}

// TestDamagedDashFragmentFinishesWithAGap is W30-A11 end to end through the
// Mux action (MuxJob → muxAndFinalize, the path the boot re-mux takes too):
// a 300 s video_stream with fragment 50 (100-102 s) zeroed, torn (in its mdat
// or inside its moof) or replaced by an HTML body. The job must finish at the full length with every intact
// fragment after the damage in the archive — 1490 of 1500 frames, the ten
// lost being the damaged fragment's own — the gap noted in the job's log
// with its time and byte range, and staging kept with its record, without
// the repaired copy the mux read.
//
// Mutants:
//   - writeRepairedCopy not pointing the copy at the repaired file (FFmpeg
//     stops at the damage; the per-track check rejects the 100 s copy and the
//     job ends in Error).
//   - walkStagedInput ignoring the walk — the probe-based check alone, the
//     staged file muxed as it stands (the row reads Finished at 100 s and
//     staging is deleted).
//   - the mux-gap shield dropped from decideStagingCleanup (staging deleted).
//   - reportStagingRepairs not called (the job's log has no gap line).
//   - cleanupStagingAfterMux keeping the dir without saying why (the job's
//     log does not end with the reason).
//   - removeRepairedCopies not called (the repaired copy is left in staging).
//   - the walk resyncing from a torn moof's claimed end rather than from
//     inside it (the torn-moof row loses intact fragment 51 too: 1480
//     frames).
func TestDamagedDashFragmentFinishesWithAGap(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	src := fragmentedFixture(t, ffmpegPath, "video", 300)
	for _, tc := range []struct {
		name string
		how  damage
	}{
		{"zeroed", damageZeroed},
		{"torn", damageTorn},
		{"torn inside its moof", damageTornMoof},
		{"html body", damageHTML},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			const id = "j-gap"
			staging, _ := routedRepairJob(t, w, db, id)
			media := filepath.Join(staging, "video_stream")
			stageFragmented(t, media, src, 50, tc.how)

			if err := w.MuxJob(id); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.Stop()

			row, _ := db.GetJob(id)
			if row == nil || row.Status != database.StatusFinished {
				t.Fatalf("job = %v (error %q), want Finished — the damage is repairable", statusOf(row), errorOf(row))
			}
			if row.LengthSeconds == nil || *row.LengthSeconds < 298 || *row.LengthSeconds > 301 {
				t.Errorf("length = %v, want ~300 s — the gap stays in the timeline", row.LengthSeconds)
			}
			v := probeStreams(t, ffprobePath, row.OutputFile)["video"]
			if v.frames != 1490 || v.seconds < 298 {
				t.Errorf("archive video = %d frames over %.1fs, want 1490 over ~300 s — every intact fragment after the damage", v.frames, v.seconds)
			}
			log := strings.Join(db.GetJobLogs(id), "\n")
			for _, want := range []string{"muxed with a gap", "video 0:01:40.0 to 0:01:42.0", "bytes ",
				"preserving staging dir: the archive was muxed without all of a staged recording"} {
				if !strings.Contains(log, want) {
					t.Errorf("the job's log does not say %q:\n%s", want, log)
				}
			}
			if _, err := os.Stat(media); err != nil {
				t.Errorf("staging was not kept: %v", err)
			}
			if !hasMuxGapRecord(media) {
				t.Error("the staged recording has no mux-gap record — nothing shields its staging")
			}
			if _, err := os.Stat(media + stagedRepairSuffix); !os.IsNotExist(err) {
				t.Errorf("the repaired copy was left in staging (stat err %v)", err)
			}
			if v := decideStagingCleanup(db, row, id, staging); v.keep != keepStagingForMuxGaps {
				t.Errorf("decideStagingCleanup = %v, want keepStagingForMuxGaps", v.keep)
			}
		})
	}
}

// TestDamagedDashTwoFilesChecksEachTrack is the realistic YouTube shape — a
// video_stream and an audio_stream — with the damage in one half and then the
// other. The format duration is the longest stream's, so the old check passed
// a copy whose damaged track stopped at a third beside the whole one; now
// each track is repaired and measured on its own.
//
// Mutant: walkStagedInput ignoring the walk — the probe check's rule, the
// format duration against the longest input, with the staged files muxed as
// they stand: the copy passes with one track at 100 s.
func TestDamagedDashTwoFilesChecksEachTrack(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	video := fragmentedFixture(t, ffmpegPath, "video", 300)
	audio := fragmentedFixture(t, ffmpegPath, "audio", 300)
	for _, tc := range []struct {
		name               string
		videoBad, audioBad int
		damaged            string
	}{
		{"damage in video", 50, -1, "video"},
		{"damage in audio", -1, 50, "audio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			const id = "j-gap-av"
			staging, _ := routedRepairJob(t, w, db, id)
			stageFragmented(t, filepath.Join(staging, "video_stream"), video, tc.videoBad, damageZeroed)
			stageFragmented(t, filepath.Join(staging, "audio_stream"), audio, tc.audioBad, damageZeroed)

			if err := w.MuxJob(id); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.Stop()

			row, _ := db.GetJob(id)
			if row == nil || row.Status != database.StatusFinished {
				t.Fatalf("job = %v (error %q), want Finished", statusOf(row), errorOf(row))
			}
			streams := probeStreams(t, ffprobePath, row.OutputFile)
			for _, typ := range []string{"video", "audio"} {
				if s := streams[typ]; s.seconds < 298 {
					t.Errorf("archive %s = %.1fs, want ~300 s", typ, s.seconds)
				}
			}
			if v := streams["video"]; (tc.damaged == "video") != (v.frames == 1490) {
				t.Errorf("archive video = %d frames, want %s", v.frames, map[bool]string{true: "1490 (one fragment left out)", false: "1500"}[tc.damaged == "video"])
			}
			damaged := filepath.Join(staging, tc.damaged+"_stream")
			if !hasMuxGapRecord(damaged) {
				t.Errorf("the damaged %s has no mux-gap record", tc.damaged)
			}
			whole := filepath.Join(staging, map[string]string{"video": "audio", "audio": "video"}[tc.damaged]+"_stream")
			if hasMuxGapRecord(whole) {
				t.Errorf("the whole %s was recorded as damaged", whole)
			}
			if _, err := os.Stat(damaged); err != nil {
				t.Errorf("staging was not kept: %v", err)
			}
		})
	}
}

// probeStarts reads each output stream's start_time.
func probeStarts(t *testing.T, ffprobePath, path string) map[string]float64 {
	t.Helper()
	out, err := exec.Command(ffprobePath, "-v", "quiet", "-print_format", "json", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			StartTime string `json:"start_time"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	starts := map[string]float64{}
	for _, s := range p.Streams {
		starts[s.CodecType], _ = strconv.ParseFloat(s.StartTime, 64)
	}
	return starts
}

// TestDamagedDashStartKeepsItsLead is the two-file capture with the damage
// in the very first fragment after the init — of the video, then of the
// audio. The repaired copy of the damaged half begins at its second
// fragment's decode time, and FFmpeg starts each input file at its own first
// timestamp: muxed as it was, that half's content from 2 s played at 0 s
// against the other's from 0 s, out of step for the whole archive, and the
// per-track check passed it. The damaged track must start about the dropped
// fragment's length into the archive, the whole one at 0, and the gap at the
// start must be named in the job's log and counted in the record.
//
// Mutants:
//   - alignStagedInputs returning before it works anything out (the damaged
//     track starts at 0: out of step).
//   - copyStaged muxing with no offsets (MuxCopy): verifyAlignedStarts
//     rejects the copy and the job ends in Error; with that check gone too,
//     the damaged track starts at 0.
//   - lostStart ignoring a drop ahead of the first readable fragment (as
//     the first mutant).
//   - recordStagedInput not counting the lead (gapSeconds 0).
func TestDamagedDashStartKeepsItsLead(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	video := fragmentedFixture(t, ffmpegPath, "video", 300)
	audio := fragmentedFixture(t, ffmpegPath, "audio", 300)
	for _, tc := range []struct {
		name               string
		videoBad, audioBad int
		damaged, whole     string
	}{
		{"first video fragment", 0, -1, "video", "audio"},
		{"first audio fragment", -1, 0, "audio", "video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			const id = "j-gap-start"
			staging, _ := routedRepairJob(t, w, db, id)
			stageFragmented(t, filepath.Join(staging, "video_stream"), video, tc.videoBad, damageZeroed)
			stageFragmented(t, filepath.Join(staging, "audio_stream"), audio, tc.audioBad, damageZeroed)

			if err := w.MuxJob(id); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.Stop()

			row, _ := db.GetJob(id)
			if row == nil || row.Status != database.StatusFinished {
				t.Fatalf("job = %v (error %q), want Finished", statusOf(row), errorOf(row))
			}
			starts := probeStarts(t, ffprobePath, row.OutputFile)
			if got := starts[tc.damaged]; got < 1.9 || got > 2.1 {
				t.Errorf("archive %s starts at %.3fs, want ~2 s — where its first readable fragment sits beside the %s; at 0 it plays out of step", tc.damaged, got, tc.whole)
			}
			if got := starts[tc.whole]; got < -0.1 || got > 0.1 {
				t.Errorf("archive %s starts at %.3fs, want 0", tc.whole, got)
			}
			if s := probeStreams(t, ffprobePath, row.OutputFile)[tc.damaged]; s.seconds < 297 {
				t.Errorf("archive %s = %.1fs, want ~298 s — every fragment after the damaged first one", tc.damaged, s.seconds)
			}
			log := strings.Join(db.GetJobLogs(id), "\n")
			if !strings.Contains(log, "0:00:00.0 to 0:00:02.0") {
				t.Errorf("the job's log does not name the gap at the start:\n%s", log)
			}
			rec, ok := readMuxGapRecord(filepath.Join(staging, tc.damaged+"_stream"))
			if !ok || rec.GapSeconds < 1.9 || rec.GapSeconds > 2.1 {
				t.Errorf("record = %+v (holds %v), want gapSeconds ~2", rec, ok)
			}
		})
	}
}

// TestDamagedStartRefusedWhenItCannotBePlaced: a half whose first fragment
// was damaged, beside a half whose start nothing can read — not a fragmented
// MP4 the walk reads, and nothing ffprobe opens — cannot be placed against
// it, so the copy is refused, with nothing recorded or repaired, rather than
// written out of step.
//
// Mutant: stagedStart answering 0 for a file it cannot read (the copy goes
// ahead with a made-up start).
func TestDamagedStartRefusedWhenItCannotBePlaced(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	videoPath, audioPath := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")
	stageFragmented(t, videoPath, fragmentedFixture(t, ffmpegPath, "video", 300), 0, damageZeroed)
	if err := os.WriteFile(audioPath, bytes.Repeat([]byte("not media "), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
	ins, err := o.prepareStagedInputs(context.Background(), "j1", filepath.Join(dir, "out.mp4"), videoPath, audioPath)
	if err == nil || !strings.Contains(err.Error(), "out of step") {
		t.Fatalf("prepareStagedInputs = %+v, %v; want a refusal naming the desync", ins, err)
	}
	for _, p := range []string{videoPath + muxGapRecordSuffix, videoPath + stagedRepairSuffix} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was written by a refused copy (stat err %v)", filepath.Base(p), err)
		}
	}
}

// TestDamagedTwitchFMP4FinishesWithAGap is the Twitch fMP4 capture: one
// video_stream holding a muxed variant — the EXT-X-MAP init, then fragments
// whose every moof carries a video traf and an audio traf — on a Twitch row.
// A damaged fragment leaves a gap in both tracks, every fragment after it is
// kept, and staging is kept with its record.
//
// Mutant: walkStagedInput ignoring the walk (both tracks stop at 100 s).
func TestDamagedTwitchFMP4FinishesWithAGap(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	const id = "j-gap-twitch"
	staging, _ := routedRepairJob(t, w, db, id)
	db.UpdateJobFields(id, map[string]any{"platform": "twitch"})
	media := filepath.Join(staging, "video_stream")
	src := fragmentedFixture(t, ffmpegPath, "muxed", 300)
	stageFragmented(t, media, src, len(src.frags)/3, damageZeroed)

	if err := w.MuxJob(id); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	row, _ := db.GetJob(id)
	if row == nil || row.Status != database.StatusFinished {
		t.Fatalf("job = %v (error %q), want Finished", statusOf(row), errorOf(row))
	}
	streams := probeStreams(t, ffprobePath, row.OutputFile)
	for _, typ := range []string{"video", "audio"} {
		if s := streams[typ]; s.seconds < 298 {
			t.Errorf("archive %s = %.1fs, want ~300 s", typ, s.seconds)
		}
	}
	if v := streams["video"]; v.frames != 1490 {
		t.Errorf("archive video = %d frames, want 1490 — one fragment left out, every one after it kept", v.frames)
	}
	if !hasMuxGapRecord(media) {
		t.Error("the staged recording has no mux-gap record")
	}
	log := strings.Join(db.GetJobLogs(id), "\n")
	if !strings.Contains(log, "muxed with a gap") || !strings.Contains(log, "audio ") {
		t.Errorf("the job's log does not name the gap in both tracks:\n%s", log)
	}
}

// TestCleanDashMuxFinishesAndClearsStaging is the control: the same two-file
// capture with nothing wrong in it muxes the staged files as they stand — no
// repaired copy, no record — finishes, and its staging is cleaned up as
// before.
//
// Mutant: stagedInput.keepsStaging true for every walked input — staging is
// kept for a capture with nothing wrong in it.
func TestCleanDashMuxFinishesAndClearsStaging(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	video := fragmentedFixture(t, ffmpegPath, "video", 300)
	audio := fragmentedFixture(t, ffmpegPath, "audio", 300)
	w, db := testWorkerSetup(t)
	const id = "j-clean-av"
	staging, _ := routedRepairJob(t, w, db, id)
	stageFragmented(t, filepath.Join(staging, "video_stream"), video, -1, damageNone)
	stageFragmented(t, filepath.Join(staging, "audio_stream"), audio, -1, damageNone)

	if err := w.MuxJob(id); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	row, _ := db.GetJob(id)
	if row == nil || row.Status != database.StatusFinished {
		t.Fatalf("job = %v (error %q), want Finished", statusOf(row), errorOf(row))
	}
	if v := probeStreams(t, ffprobePath, row.OutputFile)["video"]; v.frames != 1500 {
		t.Errorf("archive video = %d frames, want 1500", v.frames)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("a clean capture's staging survived its mux (stat err %v)", err)
	}
	if strings.Contains(strings.Join(db.GetJobLogs(id), "\n"), "gap") {
		t.Errorf("a clean capture's log mentions a gap:\n%s", strings.Join(db.GetJobLogs(id), "\n"))
	}
}

// TestVerifyAgainstStagingJudgesEachTrack pins the check itself against a
// real walk of a 300 s video_stream and audio_stream: a copy whose format
// duration is whole while its video stopped at 100 s is short, a copy missing
// a track the fragments carry is short, and container jitter within the
// tolerance is not.
//
// Mutants: judging each track by the format duration (the first and third
// rows pass); dropping the missing-track rule (the third row passes).
func TestVerifyAgainstStagingJudgesEachTrack(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	videoPath, audioPath := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")
	stageFragmented(t, videoPath, fragmentedFixture(t, ffmpegPath, "video", 300), -1, damageNone)
	stageFragmented(t, audioPath, fragmentedFixture(t, ffmpegPath, "audio", 300), -1, damageNone)
	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
	ctx := context.Background()
	ins, err := o.prepareStagedInputs(ctx, "j1", filepath.Join(dir, "out.mp4"), videoPath, audioPath)
	if err != nil {
		t.Fatal(err)
	}
	stream := func(typ string, sec float64) probedStream { return probedStream{CodecType: typ, DurationSec: sec} }
	for _, tc := range []struct {
		name    string
		out     *ffprobeData
		short   bool
		mention string
	}{
		{"video cut at 100 s beside whole audio", &ffprobeData{DurationSec: 300, Streams: []probedStream{stream("video", 100), stream("audio", 300)}}, true, "100s of video"},
		{"whole copy", &ffprobeData{DurationSec: 300, Streams: []probedStream{stream("video", 300), stream("audio", 300)}}, false, ""},
		{"video track missing", &ffprobeData{DurationSec: 300, Streams: []probedStream{stream("audio", 300)}}, true, "video"},
		{"jitter within tolerance", &ffprobeData{DurationSec: 300, Streams: []probedStream{stream("video", 290), stream("audio", 299)}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := o.verifyAgainstStaging(ctx, "j1", tc.out, ins)
			if (err != nil) != tc.short {
				t.Fatalf("verifyAgainstStaging = %v, want short=%v", err, tc.short)
			}
			if err != nil && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not name %q", err, tc.mention)
			}
		})
	}
}

// TestDamagedPartMuxesWithAGap is the part path (muxSegment — a quality or
// gap split, and the finalize's unrecorded-part recovery): a damaged seg_1
// recording is recorded as a part of its full length, and its seg dir keeps
// the record that shields staging even though the part is recorded.
//
// Mutant: muxSegment's copy made from the staged file as it stands, with
// the probe check (the part is recorded at 100 s, with no record beside it).
func TestDamagedPartMuxesWithAGap(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	const id = "j-gap-part"
	staging, _ := muxFixtureJob(t, w, db, id)
	segDir := filepath.Join(staging, "seg_1")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(segDir, "video_stream")
	stageFragmented(t, media, fragmentedFixture(t, ffmpegPath, "video", 300), 50, damageZeroed)

	job, _ := db.GetJob(id)
	seg, err := w.orchestrator.muxSegment(context.Background(), w.buildJobContext(job), 1, 0, 1700000000,
		QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media})
	if err != nil {
		t.Fatalf("muxSegment on a repairable part = %v, want the part recorded", err)
	}
	if seg.DurationSeconds < 298 {
		t.Errorf("part duration = %.1fs, want ~300 s", seg.DurationSeconds)
	}
	if !hasMuxGapRecord(media) {
		t.Fatal("the part's staged recording has no mux-gap record")
	}
	row, _ := db.GetJob(id)
	if v := decideStagingCleanup(db, row, id, staging); v.keep != keepStagingForMuxGaps {
		t.Errorf("decideStagingCleanup = %v, want keepStagingForMuxGaps — the part is recorded, its damaged bytes are not", v.keep)
	}
}

// TestDamagedAsideIsRecoveredAndKept is the set-aside recovery (the finalize's
// muxStagedAsides, and the A S action through recoverAsides): a DASH restart's
// pair, its video half damaged. The sibling is muxed whole from a repaired
// copy; neither half is removed — the video half holds bytes the sibling
// lacks, the audio half is held with it — and nothing removes them later:
// not the next pass's removeRecoveredAsides, not the boot sweep, not the
// staging cleanup. Nor is the pair muxed again to "-2".
//
// Mutants:
//   - muxStagedAsides removing the group after a repaired copy (the aside,
//     the only copy of the damaged bytes, is gone).
//   - removeRecoveredAsides without its mux-gap guard (the second pass
//     removes it).
//   - removeVerifiedRecoveredAside without its guard (the boot sweep does).
//   - keepRepairedAside not marking the group recovered (the second pass
//     muxes it again to "-2").
func TestDamagedAsideIsRecoveredAndKept(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	const id = "j-gap-aside"
	staging, outputDir := muxFixtureJob(t, w, db, id)
	stamp := engine.StagedRestartSuffix + "1700000000"
	videoAside := filepath.Join(staging, "video_stream"+stamp)
	audioAside := filepath.Join(staging, "audio_stream"+stamp)
	stageFragmented(t, videoAside, fragmentedFixture(t, ffmpegPath, "video", 300), 50, damageZeroed)
	stageFragmented(t, audioAside, fragmentedFixture(t, ffmpegPath, "audio", 300), -1, damageNone)

	job, _ := db.GetJob(id)
	jobCtx := w.buildJobContext(job)
	recovered := w.orchestrator.muxStagedAsides(context.Background(), jobCtx, outputDir, "base")
	if len(recovered) != 1 {
		t.Fatalf("recovered = %v, want one sibling", recovered)
	}
	if v := probeStreams(t, ffprobePath, recovered[0])["video"]; v.frames != 1490 {
		t.Errorf("sibling video = %d frames, want 1490 — every intact fragment after the damage", v.frames)
	}
	for _, p := range []string{videoAside, audioAside} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the aside %s was removed after a repaired copy: %v", filepath.Base(p), err)
		}
		if !hasMuxGapRecord(p) {
			t.Errorf("the aside %s has no mux-gap record", filepath.Base(p))
		}
	}

	if again := w.orchestrator.muxStagedAsides(context.Background(), jobCtx, outputDir, "base"); len(again) != 0 {
		t.Errorf("a second pass muxed the kept aside again: %v", again)
	}
	if left := mp4sIn(t, outputDir); len(left) != 1 {
		t.Errorf("output dir holds %v, want the one sibling", left)
	}
	if kept := w.removeVerifiedRecoveredAsides(id, staging); !kept {
		t.Error("the boot sweep reports nothing kept")
	}
	for _, p := range []string{videoAside, audioAside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the aside %s did not survive a later pass and the boot sweep: %v", filepath.Base(p), err)
		}
	}
	if v := decideStagingCleanup(db, job, id, staging); v.keep != keepStagingForMuxGaps {
		t.Errorf("decideStagingCleanup = %v, want keepStagingForMuxGaps", v.keep)
	}
}

// TestRepairRefusedWithoutSpace: a repaired copy needs room on the staging
// volume. Without it the mux fails with the numbers and staging untouched —
// nothing half-written left behind, no archive — for a Mux action once space
// is freed.
//
// Mutant: writeRepairedCopy without its free-space check (the job finishes on
// a volume that, here, has no room for the copy).
func TestRepairRefusedWithoutSpace(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	prev := repairDiskSpace
	repairDiskSpace = func(string) (*disk.DiskSpace, error) { return &disk.DiskSpace{Free: 1 << 20, Total: 1 << 30}, nil }
	t.Cleanup(func() { repairDiskSpace = prev })

	w, db := testWorkerSetup(t)
	const id = "j-gap-full"
	staging, outputDir := muxFixtureJob(t, w, db, id)
	media := filepath.Join(staging, "video_stream")
	stageFragmented(t, media, fragmentedFixture(t, ffmpegPath, "video", 300), 50, damageZeroed)
	before, _ := os.ReadFile(media)

	if err := w.MuxJob(id); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	row, _ := db.GetJob(id)
	if row == nil || row.Status != database.StatusError || !strings.Contains(row.Error, "free") {
		t.Fatalf("job = %v (error %q), want Error naming the free space", statusOf(row), errorOf(row))
	}
	if after, err := os.ReadFile(media); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the staged recording changed or went (err %v)", err)
	}
	if _, err := os.Stat(media + stagedRepairSuffix); !os.IsNotExist(err) {
		t.Errorf("a repaired copy was written without the room for it (stat err %v)", err)
	}
	if left := mp4sIn(t, outputDir); len(left) != 0 {
		t.Errorf("output dir holds %v, want nothing", left)
	}
}

// TestUnmeasurableFragmentedInputKeepsStaging: a fragmented MP4 whose decode
// times run backwards — here its first half appended twice — has no timeline
// to measure a copy by. It is muxed as it stands and checked the way it
// always was, but nothing independent of FFmpeg vouches for the copy, so it
// finishes with staging kept and says why (the auditor's interim rule).
//
// Mutant: walkStagedInput leaving unmeasured empty for an unreliable walk —
// the staging is deleted.
func TestUnmeasurableFragmentedInputKeepsStaging(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	src := fragmentedFixture(t, ffmpegPath, "video", 300)
	twice := fragmentedSource{init: src.init, frags: append(append([][]byte{}, src.frags[:75]...), src.frags[:75]...)}
	w, db := testWorkerSetup(t)
	const id = "j-unmeasured"
	staging, _ := routedRepairJob(t, w, db, id)
	media := filepath.Join(staging, "video_stream")
	stageFragmented(t, media, twice, -1, damageNone)

	if err := w.MuxJob(id); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	row, _ := db.GetJob(id)
	if row == nil || row.Status != database.StatusFinished {
		t.Fatalf("job = %v (error %q), want Finished", statusOf(row), errorOf(row))
	}
	rec, ok := readMuxGapRecord(media)
	if !ok || rec.Unmeasured == "" {
		t.Fatalf("record = %+v (holds %v), want one saying why the copy could not be checked", rec, ok)
	}
	if _, err := os.Stat(media); err != nil {
		t.Errorf("staging was not kept: %v", err)
	}
	if !strings.Contains(strings.Join(db.GetJobLogs(id), "\n"), "muxed without a check against the staged fragments") {
		t.Error("the job's log does not say the copy went unchecked")
	}
}

// TestMuxGapRecordShieldsOnlyItsRecording: a record holds while its recording
// is there at the size the mux read — a resume that appended to it, or a
// set-aside that moved it, leaves a record describing bytes that are not
// there, which shields nothing. And the orphan sweep honours it with no age
// rule, as it does the set-aside shield.
//
// Mutants: readMuxGapRecord without the size check (the appended row still
// shields); jobNeedsStaging without the mux-gap term (the sweep offers the
// dir).
func TestMuxGapRecordShieldsOnlyItsRecording(t *testing.T) {
	_, db := testWorkerSetup(t)
	staging := t.TempDir()
	media := filepath.Join(staging, "video_stream")
	if err := os.WriteFile(media, []byte("staged bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMuxGapRecord(media, muxGapRecord{Size: int64(len("staged bytes")), Dropped: []string{"bytes 1-2"}}); err != nil {
		t.Fatal(err)
	}
	job := &database.Job{ID: "j", Status: database.StatusFinished, UpdatedAt: "2000-01-01T00:00:00Z"}
	if n := len(stagedMuxGaps(staging)); n != 1 {
		t.Fatalf("stagedMuxGaps = %d, want 1", n)
	}
	if !jobNeedsStaging(db, nil, job, staging) {
		t.Error("jobNeedsStaging = false for a Finished job whose staging holds a mux-gap record")
	}
	f, err := os.OpenFile(media, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" appended"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := len(stagedMuxGaps(staging)); n != 0 {
		t.Errorf("stagedMuxGaps = %d after the recording grew, want 0", n)
	}
	if err := os.Remove(media); err != nil {
		t.Fatal(err)
	}
	if n := len(stagedMuxGaps(staging)); n != 0 {
		t.Errorf("stagedMuxGaps = %d with the recording gone, want 0", n)
	}
}

// TestMergeKeepsASupersededDirHoldingAMuxGap: the part merge deletes a
// superseded part's staging dir once its rows are committed — the part's
// media is in the merged file. Not when the part was muxed from a repaired
// copy: the merged file lacks the bytes the repair left out too.
//
// Mutant: the merge cleanup without its mux-gap check.
func TestMergeKeepsASupersededDirHoldingAMuxGap(t *testing.T) {
	pm, stagingDir, segments := mergeFixture(t)
	media := filepath.Join(stagingDir, "seg_1", "video_stream")
	if err := writeMuxGapRecord(media, muxGapRecord{Size: int64(len("raw")), Dropped: []string{"bytes 1-2"}}); err != nil {
		t.Fatal(err)
	}
	pm.merge(context.Background(), "job", stagingDir, segments)
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("the merge cleanup deleted a part's recording its mux could not carry whole: %v", err)
	}
	if n := len(stagedMuxGaps(stagingDir)); n != 1 {
		t.Errorf("stagedMuxGaps = %d after the merge, want 1 — the shield must see the tombstoned dir", n)
	}
}
