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

// TestAsidesSharingAStampAreSeparateRecordings pins the recording half of the
// grouping key. setAsideStagedMedia sets every live-shape capture in a dir
// aside under one stamp, so an HLS video.ts an earlier session left beside a
// DASH capture shares the DASH pair's stamp; grouped by the stamp alone it
// had no slot in the pair's group, and muxStagedAsides removed it with the
// pair, unmuxed. Nor may a video.ts a second away from an audio-only DASH
// aside be taken for that capture's straddled video half.
//
// Mutants: drop asideRecordingOf from groupStagedAsides' key — the video.ts
// joins the DASH pair's group (three groups, not four); compare dirs alone in
// pairStraddledHalves — the video.ts is paired with the lone audio_stream.
func TestAsidesSharingAStampAreSeparateRecordings(t *testing.T) {
	root := t.TempDir()
	aside := func(stem, stamp string) string {
		return filepath.Join(root, stem+engine.StagedRestartSuffix+stamp)
	}
	groups := groupStagedAsides([]string{
		aside("audio_stream", "1700000000"),
		aside("video.ts", "1700000000"),
		aside("video_stream", "1700000000"),
		aside("audio_stream", "1700000500"), // an audio-only DASH restart…
		aside("video.ts", "1700000501"),     // …and an HLS capture a second later
	})
	if len(groups) != 4 {
		t.Fatalf("got %d groups, want 4: %+v", len(groups), groups)
	}
	if g := groups[0]; filepath.Base(g.video) != "video_stream"+engine.StagedRestartSuffix+"1700000000" ||
		filepath.Base(g.audio) != "audio_stream"+engine.StagedRestartSuffix+"1700000000" || len(g.files) != 2 {
		t.Errorf("first group = %+v, want the DASH pair and nothing else", g)
	}
	if g := groups[1]; filepath.Base(g.video) != "video.ts"+engine.StagedRestartSuffix+"1700000000" || g.audio != "" || len(g.files) != 1 {
		t.Errorf("second group = %+v, want the video.ts that shares the pair's stamp, on its own", g)
	}
	if g := groups[2]; g.video != "" || g.audio == "" || len(g.files) != 1 {
		t.Errorf("third group = %+v, want the audio-only restart on its own", g)
	}
	if g := groups[3]; filepath.Base(g.video) != "video.ts"+engine.StagedRestartSuffix+"1700000501" || g.audio != "" || len(g.files) != 1 {
		t.Errorf("fourth group = %+v, want the later video.ts on its own", g)
	}
}
