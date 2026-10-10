package routes

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// W25-04. The import took the first video in the zip and, separately, the
// first chat: a split recording ("<name> - part1.mp4", "<name> - part2.mp4")
// imported as part 1 alone, titled "<name> - part1", with part 2 never
// mentioned; and a zip of two recordings imported the first under the
// second's id, title and chat. A split recording is now one job with its
// parts, stored as the finalize stores one; any other zip of several videos
// is refused with the videos named; and a chat belongs only to the video its
// name matches.

// stubImportDurations stands in for ffprobe: each part's duration is its
// size in seconds times ten.
func stubImportDurations(t *testing.T) {
	t.Helper()
	orig := importProbeDuration
	importProbeDuration = func(_ context.Context, _, file string) float64 {
		info, err := os.Stat(file)
		if err != nil {
			return 0
		}
		return float64(info.Size()) * 10
	}
	t.Cleanup(func() { importProbeDuration = orig })
}

// A split recording zipped as the finalize wrote it — listed out of order —
// is one job: filename the parts' shared name, output_file part 1, the size
// and length of both, the recording's chat on the row, and a segment row per
// part in part order with its own file, size and duration.
//
// Mutants: refusing every zip of several videos (400); taking the first video
// as before; not sorting the parts (part 2 becomes the output file and index
// 0 is part 2); SegmentIndex the part number rather than one less;
// videoOutName kept as part 1's file name; the size of the first part only;
// no LengthSeconds from the durations; the duration not probed; pairing the
// zip's only chat whatever its name.
func TestImportSplitRecordingIsOneJob(t *testing.T) {
	stubImportDurations(t)
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ] - part2.mp4", data: []byte("PART-TWO-LONGER")},
		importEntry{name: "Stream [dQw4w9WgXcQ] - part1.mp4", data: []byte("PART-ONE")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: chatJSONFor(t, map[string]any{
			"videoId": "dQw4w9WgXcQ", "videoTitle": "Stream", "channelName": "Chan",
		})},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	imports := filepath.Join(f.outputDir, "imports")
	if job.ID != "dQw4w9WgXcQ" || job.Title != "Stream" || job.ChannelName != "Chan" {
		t.Errorf("id %q title %q channel %q", job.ID, job.Title, job.ChannelName)
	}
	if job.Filename != "imports/Stream [dQw4w9WgXcQ]" {
		t.Errorf("filename %q, want the parts' shared name", job.Filename)
	}
	if want := filepath.Join(imports, "Stream [dQw4w9WgXcQ] - part1.mp4"); job.OutputFile != want {
		t.Errorf("outputFile %q, want part 1 %q", job.OutputFile, want)
	}
	if job.FileSize == nil || *job.FileSize != int64(len("PART-ONE")+len("PART-TWO-LONGER")) {
		t.Errorf("fileSize %v, want both parts'", job.FileSize)
	}
	if job.LengthSeconds == nil || *job.LengthSeconds != 230 {
		t.Errorf("lengthSeconds %v, want 230", job.LengthSeconds)
	}
	if job.ChatFilename != "imports/Stream [dQw4w9WgXcQ].chat.json" {
		t.Errorf("chatFilename %q", job.ChatFilename)
	}

	segs, err := f.db.GetSegments(job.ID)
	if err != nil || len(segs) != 2 {
		t.Fatalf("segments %+v (%v), want two", segs, err)
	}
	for i, want := range []struct {
		name     string
		size     int64
		duration float64
	}{
		{"Stream [dQw4w9WgXcQ] - part1.mp4", 8, 80},
		{"Stream [dQw4w9WgXcQ] - part2.mp4", 15, 150},
	} {
		s := segs[i]
		if s.SegmentIndex != i || s.Filename != want.name || s.FilePath != filepath.Join(imports, want.name) ||
			s.FileSize == nil || *s.FileSize != want.size || s.DurationSeconds != want.duration || s.ChatFile != "" {
			t.Errorf("segment %d = %+v, want %s (%d bytes, %vs)", i, s, want.name, want.size, want.duration)
		}
	}
	assertNoLeftovers(t, f, "Stream [dQw4w9WgXcQ] - part1.mp4", "Stream [dQw4w9WgXcQ] - part2.mp4", "Stream [dQw4w9WgXcQ].chat.json")
}

// A split Twitch capture carries a chat per part ("<name> - partN.chat.json").
// Each part's chat goes on its segment row, the job's is part 1's (the
// finalize's rule), and the metadata comes from part 1's chat.
//
// Mutants: metaChat reading only the recording's own chat (a youtube row);
// importJobChat taking only a whole-recording chat (no chat on the row);
// importSegment's chat not matched by part (each segment gets the last).
func TestImportSplitTwitchCaptureKeepsItsPartChats(t *testing.T) {
	stubImportDurations(t)
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Late [tw_316543210987] - part1.mp4", data: []byte("one")},
		importEntry{name: "Late [tw_316543210987] - part2.mp4", data: []byte("two")},
		importEntry{name: "Late [tw_316543210987] - part1.chat.json", data: twitchChatJSON(t, "streamer", "Streamer", "316543210987")},
		importEntry{name: "Late [tw_316543210987] - part2.chat.json", data: twitchChatJSON(t, "streamer", "Streamer", "316543210987")},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Platform != "twitch" || job.ChannelName != "Streamer" {
		t.Errorf("platform %q channel %q, want the part chats' twitch / Streamer", job.Platform, job.ChannelName)
	}
	imports := filepath.Join(f.outputDir, "imports")
	if want := filepath.Join(imports, "Late [tw_316543210987] - part1.chat.json"); job.ChatFile != want {
		t.Errorf("chatFile %q, want part 1's %q", job.ChatFile, want)
	}
	segs, _ := f.db.GetSegments(job.ID)
	if len(segs) != 2 {
		t.Fatalf("segments %+v", segs)
	}
	for i, s := range segs {
		want := filepath.Join(imports, "Late [tw_316543210987] - part"+string(rune('1'+i))+".chat.json")
		if s.ChatFile != want {
			t.Errorf("segment %d chat %q, want %q", i, s.ChatFile, want)
		}
	}
}

// A zip of videos that are not the parts of one recording is refused, the
// videos named, and nothing is written.
//
// Mutants: dropping the shared-name check (two names' parts become one job);
// dropping the duplicate-number check (two part 1s become one job); taking
// the first video as before (201).
func TestImportRefusesAZipOfSeveralRecordings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
	}{
		{"two recordings", []string{"Recording A.mp4", "Recording B.mp4"}},
		{"two names' parts", []string{"X - part1.mp4", "Y - part2.mp4"}},
		{"one part twice", []string{"X - part1.mp4", "X - part1.mkv"}},
		{"a part and a whole", []string{"X - part1.mp4", "X.mp4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			var entries []importEntry
			for _, n := range tc.names {
				entries = append(entries, importEntry{name: n, data: []byte("v")})
			}
			entries = append(entries, importEntry{name: "Recording B.chat.json", data: chatJSONFor(t, map[string]any{"videoId": "BBBBBBBBBBB"})})
			rec, _ := importZip(t, f, orderedImportZip(t, entries...))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("import: %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			for _, n := range tc.names {
				if !strings.Contains(rec.Body.String(), n) {
					t.Errorf("the refusal does not name %q: %s", n, rec.Body.String())
				}
			}
			if names := importsDirNames(t, f); len(names) != 0 {
				t.Errorf("a refused import wrote %v", names)
			}
		})
	}
}

// A chat is paired by name only. "Recording A.mp4" beside "Recording B
// .chat.json" is Recording A, imported without B's id, title or chat — and
// the response names the chat it left out. A "<name>.json" holding messages
// still pairs with "<name>.mp4".
//
// Mutants: pairing the zip's only chat whatever its name (A imported as
// BBBBBBBBBBB with B's chat); not reporting the unpaired chat; dropping the
// .json fallback.
func TestImportPairsAChatByNameOnly(t *testing.T) {
	f := newImportFixture(t)
	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Recording A.mp4", data: []byte("AAAA")},
		importEntry{name: "Recording B.chat.json", data: chatJSONFor(t, map[string]any{
			"videoId": "BBBBBBBBBBB", "videoTitle": "Recording B", "channelName": "Chan",
		})},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if r.ID == "BBBBBBBBBBB" || r.Title != "Recording A" || r.ChatFilename != "" || r.ChannelName != "Import" {
		t.Errorf("id %q title %q chat %q channel %q: another video's chat was taken for this one", r.ID, r.Title, r.ChatFilename, r.ChannelName)
	}
	if !slices.Equal(r.Import.UnpairedChats, []string{"Recording B.chat.json"}) || !strings.Contains(r.Import.Note, "Recording B.chat.json") {
		t.Errorf("outcome %+v, want the left-out chat named", r.Import)
	}

	f2 := newImportFixture(t)
	rec, job := importZip(t, f2, orderedImportZip(t,
		importEntry{name: "plain-video.mp4", data: []byte("v")},
		importEntry{name: "plain-video.json", data: chatJSONFor(t, map[string]any{"videoTitle": "From JSON"})},
	))
	if rec.Code != http.StatusCreated || job.ChatFilename == "" || job.Title != "From JSON" {
		t.Errorf("a <name>.json chat: %d, chat %q, title %q", rec.Code, job.ChatFilename, job.Title)
	}
}

// A part row the database refuses takes the job back out — its segment rows
// with it — and nothing is placed.
//
// Mutant: dropping the DeleteJob call on the AddSegment error path (a row
// with no parts names files that were never placed).
func TestImportSplitRecordingWithARefusedPartRowLeavesNothing(t *testing.T) {
	f := newImportFixture(t)
	raw, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.outputDir), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_parts BEFORE INSERT ON segments BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ] - part1.mp4", data: []byte("one")},
		importEntry{name: "Stream [dQw4w9WgXcQ] - part2.mp4", data: []byte("two")},
	))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("import: %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Error("the job survived its refused part rows")
	}
	if names := importsDirNames(t, f); len(names) != 0 {
		t.Errorf("imports/ holds %v", names)
	}
}

// The real probe: ffprobe's format duration, 0 for what it cannot read. The
// media half runs where FFmpeg is installed, and reads the part through its
// temporary ".partial" name, as the import does.
//
// Mutants: answering anything but 0 for a probe that failed; reading a field
// other than format.duration.
func TestImportProbeDurationReadsFFprobe(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-media")
	os.WriteFile(bad, []byte("not media"), 0o644)
	if d := importProbeDuration(context.Background(), "ffprobe", bad); d != 0 {
		t.Errorf("a file ffprobe cannot read: %v, want 0", d)
	}
	if d := importProbeDuration(context.Background(), filepath.Join(dir, "no-ffprobe"), bad); d != 0 {
		t.Errorf("no ffprobe: %v, want 0", d)
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	media := filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=2", "-c:v", "mpeg4", media).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not make a clip: %v %s", err, out)
	}
	partial := filepath.Join(dir, importTempPrefix+"1"+importPartialExt)
	if err := os.Rename(media, partial); err != nil {
		t.Fatal(err)
	}
	if d := importProbeDuration(context.Background(), "ffprobe", partial); d < 1.9 || d > 2.1 {
		t.Errorf("a 2 s clip probed as %v s", d)
	}
}
