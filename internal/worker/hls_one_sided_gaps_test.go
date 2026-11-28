package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestOneSidedGaps: the sequences one half of a demuxed HLS pair skipped
// that the other did not, read from the job's gap rows, each stream's from
// its own first sequence and both up to the pair's end.
//
// Mutants: the lower bounds ignored (an earlier part's gap, or a leading
// gap before a half's first segment, is reported); the upper bound ignored
// (a later part's gap is reported); a range not split around the other
// stream's (40-44 less 42 stays whole); the two streams swapped.
func TestOneSidedGaps(t *testing.T) {
	g := func(from, to int, stream string) database.Gap {
		return database.Gap{From: from, To: to, Stream: stream}
	}
	for _, tc := range []struct {
		name                 string
		gaps                 []database.Gap
		vFrom, aFrom, upTo   int
		wantVideo, wantAudio [][2]int
	}{
		{"none", nil, 0, 0, -1, nil, nil},
		{"both skipped the same", []database.Gap{g(40, 44, "video"), g(40, 44, "audio")}, 0, 0, -1, nil, nil},
		{"video alone", []database.Gap{g(40, 44, "video")}, 0, 0, -1, [][2]int{{40, 44}}, nil},
		{"audio alone", []database.Gap{g(50, 50, "audio")}, 0, 0, -1, nil, [][2]int{{50, 50}}},
		{"video less a middle audio gap", []database.Gap{g(40, 44, "video"), g(42, 42, "audio")}, 0, 0, -1, [][2]int{{40, 41}, {43, 44}}, nil},
		{"overlapping", []database.Gap{g(40, 44, "video"), g(43, 47, "audio")}, 0, 0, -1, [][2]int{{40, 42}}, [][2]int{{45, 47}}},
		{"an earlier part's gap", []database.Gap{g(10, 12, "video"), g(98, 102, "audio")}, 100, 100, -1, nil, [][2]int{{100, 102}}},
		// A forced start below the window: each half's gap row ends before
		// its own first segment, one a segment later than the other.
		{"leading gaps before each half's start", []database.Gap{g(90, 99, "video"), g(90, 100, "audio")}, 100, 101, -1, nil, nil},
		{"a later part's gap", []database.Gap{g(40, 41, "video"), g(300, 310, "audio")}, 0, 0, 200, [][2]int{{40, 41}}, nil},
	} {
		v, a := oneSidedGaps(tc.gaps, tc.vFrom, tc.aFrom, tc.upTo)
		if !reflect.DeepEqual(v, tc.wantVideo) || !reflect.DeepEqual(a, tc.wantAudio) {
			t.Errorf("%s: video-only %v audio-only %v, want %v and %v", tc.name, v, a, tc.wantVideo, tc.wantAudio)
		}
	}
}

// TestCopyStagedWarnsOnAOneSidedHlsGap: the copy of a demuxed HLS pair
// says in a Warn naming the job when one half skipped segments the other
// has, and stays quiet when both skipped the same ones, when the gap lies
// past the pair's saved end (a later part's), and when the pair's starts
// are not recorded (an aside's records stay with the live names).
//
// Mutants: warnOneSidedHlsGaps not called; the unrecorded-start skip
// dropped; the sidecars' end not read.
func TestCopyStagedWarnsOnAOneSidedHlsGap(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	const msg = "one half of an HLS capture skipped segments the other half has; a skip longer than about 10 s makes everything after it in that half play early, out of step with the other, for the rest of the part"
	for _, tc := range []struct {
		name       string
		audioGap   bool
		unrecorded bool
		endsAt     int // both halves' sidecar LastSeq; 0: no sidecars
		want       bool
	}{
		{name: "video alone skipped", want: true},
		{name: "video alone skipped, inside the pair's end", endsAt: 60, want: true},
		{name: "both skipped", audioGap: true},
		{name: "a later part's gap", endsAt: 30},
		{name: "starts not recorded", unrecorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			if err := o.db.AddGap(jobCtx.Job.ID, 40, 44, "video"); err != nil {
				t.Fatal(err)
			}
			if tc.audioGap {
				if err := o.db.AddGap(jobCtx.Job.ID, 40, 44, "audio"); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			video, audio := filepath.Join(dir, "video.ts"), filepath.Join(dir, "audio.ts")
			writeOffsetTSFixture(t, ffmpegPath, video, 4, 17000)
			writeOffsetAudioTSFixture(t, ffmpegPath, audio, 4, 3)
			if !tc.unrecorded {
				noteRecordingStart(video, 20)
				noteRecordingStart(audio, 20)
			}
			if tc.endsAt > 0 {
				for _, p := range []string{video, audio} {
					state, _ := json.Marshal(engine.ResumeState{LastSeq: tc.endsAt, BytesWritten: 1, Timestamp: time.Now().Unix()})
					if err := os.WriteFile(p+".resume.json", state, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			logs := &captureLogger{}
			o.logger = logs
			if _, err := o.copyStaged(context.Background(), jobCtx.Job.ID, video, audio, filepath.Join(dir, "out.mp4"), "", nil); err != nil {
				t.Fatalf("copyStaged: %v", err)
			}
			got, warned := logs.field(msg, "jobID")
			if warned != tc.want {
				t.Fatalf("one-sided gap Warn logged %v, want %v", warned, tc.want)
			}
			if warned && got != jobCtx.Job.ID {
				t.Errorf("Warn names job %v, want %s", got, jobCtx.Job.ID)
			}
			if v, _ := logs.field(msg, "videoOnlyGaps"); warned && v != "40-44" {
				t.Errorf("videoOnlyGaps = %v, want 40-44", v)
			}
		})
	}
}
