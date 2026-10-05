package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// The two halves of one DASH restart are stamped by two downloaders, each
// reading the clock itself; a restart that straddles a second boundary
// stamped them a second apart, and grouping by the exact stamp made that one
// recording two single-stream siblings. A video-only and an audio-only group
// of the same dir a second apart are one restart.
//
// Mutant: pairStraddledHalves returning its input — three groups, not two.
func TestAsideHalvesAcrossASecondBoundaryAreOneRecording(t *testing.T) {
	root := t.TempDir()
	seg := filepath.Join(root, "seg_1")
	aside := func(dir, stem, stamp string) string {
		return filepath.Join(dir, stem+engine.StagedRestartSuffix+stamp)
	}
	groups := groupStagedAsides([]string{
		aside(root, "video_stream", "1700000000"),
		aside(root, "audio_stream", "1700000001"), // the same restart, a second later
		aside(seg, "audio_stream", "1700000000"),  // another dir: never paired
		aside(root, "video_stream", "1700000500"), // a later restart, video only
	})
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.stamp != "1700000000" || filepath.Base(g.video) != "video_stream"+engine.StagedRestartSuffix+"1700000000" ||
		filepath.Base(g.audio) != "audio_stream"+engine.StagedRestartSuffix+"1700000001" || len(g.files) != 2 {
		t.Errorf("first group = %+v, want the straddled pair under the video's stamp", g)
	}
	if groups[1].audio == "" || groups[1].video != "" || filepath.Dir(groups[1].audio) != seg {
		t.Errorf("second group = %+v, want seg_1's audio-only aside on its own", groups[1])
	}
	if groups[2].stamp != "1700000500" || groups[2].audio != "" {
		t.Errorf("third group = %+v, want the later video-only restart on its own", groups[2])
	}
}
