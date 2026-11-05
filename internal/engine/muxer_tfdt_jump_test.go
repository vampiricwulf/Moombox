package engine

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMuxCopyKeepsAHoleInAFragmentedFile is the check wave W12 of the
// 2026-10-31 audit asks for before a refused segment may be skipped in place
// (refuseSegment, owner decision D8): FFmpeg must accept a fragmented MP4
// whose decode times jump where a fragment is missing — the file a skip in
// place leaves, sq N-1's fragment followed by sq N+1's — and the copy must
// keep the jump as a hole on the timeline rather than fail, or close it up and
// pull everything after the hole one segment early (which would put the
// track out of step with its sibling in the mux).
//
// The fixture is ten one-second fragments rendered by FFmpeg, its sixth
// fragment cut out — exactly what the DASH loop appends past a skipped
// sequence, since every googlevideo fragment carries its own tfdt. The copy
// keeps the full ten seconds and the 45 frames that are left.
func TestMuxCopyKeepsAHoleInAFragmentedFile(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", "5", "-keyint_min", "5", "-sc_threshold", "0",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate the fragmented fixture: %v\n%s", err, out)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Split it the way googlevideo serves it: the init, then one moof+mdat
	// per segment. Anything else (the trailing mfra) is left out.
	var init []byte
	var frags [][]byte
	var moof []byte
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off:]))
		if size < 8 || off+size > len(data) {
			t.Fatalf("fixture box at %d has size %d", off, size)
		}
		box := data[off : off+size]
		switch string(data[off+4 : off+8]) {
		case "ftyp", "moov":
			init = append(init, box...)
		case "moof":
			moof = box
		case "mdat":
			frags = append(frags, append(append([]byte(nil), moof...), box...))
		}
		off += size
	}
	if len(frags) != 10 {
		t.Fatalf("fixture has %d fragments, want 10", len(frags))
	}
	holed := filepath.Join(dir, "video_stream")
	file := append([]byte(nil), init...)
	for i, f := range frags {
		if i != 5 {
			file = append(file, f...)
		}
	}
	if err := os.WriteFile(holed, file, 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMuxer(ffmpegPath, &testLogger{})
	out := filepath.Join(dir, "out.mp4")
	if err := m.MuxCopy(t.Context(), holed, "", out); err != nil {
		t.Fatalf("MuxCopy over the hole: %v", err)
	}
	probe := exec.Command(m.FFprobePath(), "-v", "error", "-select_streams", "v:0",
		"-count_packets", "-show_entries", "stream=duration,nb_read_packets", "-of", "csv=p=0", out)
	raw, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe the copy: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(raw)), ",")
	if len(fields) != 2 {
		t.Fatalf("ffprobe answered %q", raw)
	}
	duration, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("duration %q: %v", fields[0], err)
	}
	packets, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("packets %q: %v", fields[1], err)
	}
	if packets != 45 {
		t.Errorf("copy holds %d frames, want 45 (ten fragments of five, one cut out)", packets)
	}
	if duration < 9.9 || duration > 10.1 {
		t.Errorf("copy lasts %.3f s, want 10 s — the hole kept on the timeline, not closed up", duration)
	}
}
