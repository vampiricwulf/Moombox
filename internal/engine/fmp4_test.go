package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- a synthetic fragmented MP4, box by box ---------------------------------

func synthBox(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

func synthFull(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	vf := make([]byte, 4)
	binary.BigEndian.PutUint32(vf, flags)
	vf[0] = version
	return synthBox(typ, append([][]byte{vf}, payload...)...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

type synthTrack struct {
	id        uint32
	handler   string
	timescale uint32
}

// synthInit is an init segment the way the engine stages one: ftyp, then a
// moov with one trak per track and an mvex.
func synthInit(tracks ...synthTrack) []byte {
	var traks, trexes [][]byte
	for _, t := range tracks {
		tkhd := synthFull("tkhd", 0, 3, u32(0), u32(0), u32(t.id), u32(0), u32(0))
		mdhd := synthFull("mdhd", 0, 0, u32(0), u32(0), u32(t.timescale), u32(0), u32(0))
		hdlr := synthFull("hdlr", 0, 0, u32(0), []byte(t.handler), make([]byte, 12), []byte("h\x00"))
		traks = append(traks, synthBox("trak", tkhd, synthBox("mdia", mdhd, hdlr)))
		trexes = append(trexes, synthFull("trex", 0, 0, u32(t.id), u32(1), u32(0), u32(0), u32(0)))
	}
	mvhd := synthFull("mvhd", 0, 0, make([]byte, 96))
	moov := synthBox("moov", append(append([][]byte{mvhd}, traks...), synthBox("mvex", trexes...))...)
	return append(synthBox("ftyp", []byte("iso6"), u32(0), []byte("iso6dash")), moov...)
}

type synthTraf struct {
	track   uint32
	tfdt    uint64
	samples int
	dur     uint32 // per sample
	size    uint32 // per sample
}

// synthFragment is one media segment as a DASH server sends it: styp, sidx,
// moof, mdat. The mdat payload is a byte pattern that spells no box type.
func synthFragment(seq uint32, trafs ...synthTraf) []byte {
	var trafBoxes [][]byte
	var payload int
	for _, tr := range trafs {
		var table [][]byte
		for range tr.samples {
			table = append(table, u32(tr.dur), u32(tr.size))
		}
		trun := synthFull("trun", 0, 0x301, append([][]byte{u32(uint32(tr.samples)), u32(0)}, table...)...)
		trafBoxes = append(trafBoxes, synthBox("traf",
			synthFull("tfhd", 0, 0x020000, u32(tr.track)),
			synthFull("tfdt", 1, 0, u64(tr.tfdt)),
			trun))
		payload += tr.samples * int(tr.size)
	}
	moof := synthBox("moof", append([][]byte{synthFull("mfhd", 0, 0, u32(seq))}, trafBoxes...)...)
	media := bytes.Repeat([]byte{0xa5, 0x5a, 0x33}, payload/3+1)[:payload]
	styp := synthBox("styp", []byte("msdh"), u32(0), []byte("msdhmsix"))
	sidx := synthFull("sidx", 1, 0, u32(1), u32(90000), u64(0), u64(0), u32(0), u32(0), u32(0), u32(0))
	return bytes.Join([][]byte{styp, sidx, moof, synthBox("mdat", media)}, nil)
}

// synthStream is an init plus n two-second fragments of one video track
// (timescale 90000, 50 samples of 3600), the first one at startSec. It
// returns the stream and where each fragment begins (plus the end).
func synthStream(n int, startSec uint64) ([]byte, []int) {
	data := synthInit(synthTrack{1, "vide", 90000})
	offs := []int{}
	for i := range n {
		offs = append(offs, len(data))
		data = append(data, synthFragment(uint32(i+1), synthTraf{1, (startSec + uint64(2*i)) * 90000, 50, 3600, 200})...)
	}
	return data, append(offs, len(data))
}

func walkBytes(t *testing.T, data []byte) (*FMP4Walk, error) {
	t.Helper()
	return WalkFMP4At(context.Background(), bytes.NewReader(data), int64(len(data)))
}

func mustWalk(t *testing.T, data []byte) *FMP4Walk {
	t.Helper()
	w, err := walkBytes(t, data)
	if err != nil {
		t.Fatalf("WalkFMP4At: %v", err)
	}
	return w
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// splice replaces data[from:to] with repl.
func splice(data []byte, from, to int, repl []byte) []byte {
	out := append([]byte{}, data[:from]...)
	out = append(out, repl...)
	return append(out, data[to:]...)
}

// TestWalkFMP4ReadsAHealthyStream: every fragment is accepted, nothing is
// dropped, and the track's span is the fragments' own timeline.
//
// Mutant: summing durations from tfhd/trex when trun carries them (the span
// reads 0).
func TestWalkFMP4ReadsAHealthyStream(t *testing.T) {
	data, _ := synthStream(10, 0)
	w := mustWalk(t, data)
	if w.Damaged() || w.TornTail != 0 || w.Unreliable != "" {
		t.Fatalf("healthy stream: drops=%v tornTail=%d unreliable=%q", w.Drops, w.TornTail, w.Unreliable)
	}
	if w.Fragments != 10 || len(w.Tracks) != 1 || w.Tracks[0].Fragments != 10 {
		t.Fatalf("fragments = %d (track %d), want 10", w.Fragments, w.Tracks[0].Fragments)
	}
	if got := w.Tracks[0].SpanSeconds(); !near(got, 20) {
		t.Errorf("span = %.3fs, want 20s", got)
	}
	if w.Tracks[0].Handler != "vide" || w.Tracks[0].Timescale != 90000 {
		t.Errorf("track = %+v, want vide at 90000", *w.Tracks[0])
	}
}

// TestWalkFMP4ReadsPastAForeignBox: a box of a type the walk does not know,
// which a writer put between fragments — whole, printable, followed by a box
// the walk does know — is read past as FFmpeg reads past it. Treated as
// damage, every capture whose server adds one would be "repaired" and keep
// its staging.
//
// Mutant: foreignBoxFits answering false (ten drops, one per foreign box).
func TestWalkFMP4ReadsPastAForeignBox(t *testing.T) {
	data := synthInit(synthTrack{1, "vide", 90000})
	for i := range 10 {
		data = append(data, synthBox("ytmd", []byte("Sequence-Number: "+strings.Repeat("7", i+1)))...)
		data = append(data, synthFragment(uint32(i+1), synthTraf{1, uint64(2*i) * 90000, 50, 3600, 100})...)
	}
	data = append(data, synthBox("ytmd", []byte("trailing"))...)
	w := mustWalk(t, data)
	if w.Damaged() || w.Fragments != 10 || !near(w.Tracks[0].SpanSeconds(), 20) {
		t.Errorf("drops=%v fragments=%d span=%.3f, want a clean 20 s walk of 10 fragments", w.Drops, w.Fragments, w.Tracks[0].SpanSeconds())
	}
}

// TestWalkFMP4ForeignBoxMustFit: damage whose first bytes read as a header of
// an unknown type is not a foreign box. Here the header claims 16 KB — past
// the start of the intact fragment right after the damage — and where that
// lands is no box; taken as a foreign box, the walk jumped over that fragment
// and the drop took it. A type that is not four printable characters is
// damage even when a box follows it.
//
// Mutants: foreignBoxFits not looking at what follows the box (the first row
// drops fragment 5); foreignBoxFits taking any type (the second row has no
// drop).
func TestWalkFMP4ForeignBoxMustFit(t *testing.T) {
	data, offs := synthStream(10, 0)
	garbage := append(append(u32(16<<10), []byte("<htm")...), bytes.Repeat([]byte("l><head>"), 12)...)
	w := mustWalk(t, splice(data, offs[4], offs[5], garbage))
	if len(w.Drops) != 1 || w.Drops[0].End != int64(offs[4]+len(garbage)) || w.Fragments != 9 {
		t.Errorf("drops=%+v fragments=%d, want only the %d damaged bytes dropped and 9 fragments", w.Drops, w.Fragments, len(garbage))
	}
	frag4 := data[offs[4]:offs[5]]
	typeless := append(append(u32(uint32(len(frag4))), 0x01, 0x02, 0x03, 0x04), make([]byte, len(frag4)-8)...)
	w = mustWalk(t, splice(data, offs[4], offs[5], typeless))
	if len(w.Drops) != 1 || w.Drops[0].Start != int64(offs[4]) || w.Drops[0].End != int64(offs[5]) {
		t.Errorf("drops=%+v, want the box of a non-printable type dropped", w.Drops)
	}
}

// TestWalkFMP4SpanStartsAtTheFirstFragment: a part that began 208 s into the
// broadcast carries 20 s of fragments, not 228.
//
// Mutant: SpanSeconds measuring End from zero.
func TestWalkFMP4SpanStartsAtTheFirstFragment(t *testing.T) {
	data, _ := synthStream(10, 208)
	if got := mustWalk(t, data).Tracks[0].SpanSeconds(); !near(got, 20) {
		t.Errorf("span = %.3fs, want 20s", got)
	}
}

// TestWalkFMP4DropsADamagedFragment is W30-A11's three shapes of one bad
// fragment mid-stream — zeroed, an HTML error body served in its place, and
// torn (half its bytes, then the next fragment appended). Each must be
// dropped as exactly the bad fragment's bytes, with every fragment after it
// kept, and the gap it leaves at 8-10 s on the track's timeline.
//
// Mutants:
//   - damage() resyncing from the damage instead of from inside the last
//     accepted fragment's mdat: the torn row drops fragment 5 too (its mdat
//     claims fragment 5's bytes) and keeps the torn fragment 4.
//   - no rollback of that fragment: the torn row counts 10 fragments.
func TestWalkFMP4DropsADamagedFragment(t *testing.T) {
	data, offs := synthStream(10, 0)
	frag4 := data[offs[4]:offs[5]]
	html := []byte("<html><head><title>502 Bad Gateway</title></head><body>Bad Gateway</body></html>\n")
	for _, tc := range []struct {
		name     string
		stream   []byte
		dropFrom int
		dropTo   int
	}{
		{"zeroed", splice(data, offs[4], offs[5], make([]byte, len(frag4))), offs[4], offs[5]},
		{"html body", splice(data, offs[4], offs[5], html), offs[4], offs[4] + len(html)},
		{"torn", splice(data, offs[4], offs[5], frag4[:len(frag4)/2]), offs[4], offs[4] + len(frag4)/2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWalk(t, tc.stream)
			if len(w.Drops) != 1 {
				t.Fatalf("drops = %+v, want exactly one", w.Drops)
			}
			d := w.Drops[0]
			if d.Start != int64(tc.dropFrom) || d.End != int64(tc.dropTo) {
				t.Errorf("drop = bytes %d-%d, want %d-%d — only the bad fragment's bytes", d.Start, d.End, tc.dropFrom, tc.dropTo)
			}
			if w.Fragments != 9 {
				t.Errorf("fragments = %d, want 9 — every intact fragment after the damage", w.Fragments)
			}
			if got := w.Tracks[0].SpanSeconds(); !near(got, 20) {
				t.Errorf("span = %.3fs, want 20s — the gap stays inside the timeline", got)
			}
			g := d.Gaps[0]
			if !g.HasFrom || !g.HasTo || !near(g.From, 8) || !near(g.To, 10) {
				t.Errorf("gap = %+v, want 8s to 10s", g)
			}
			if !strings.Contains(FormatFMP4Drop(d), "video 0:00:08.0 to 0:00:10.0 (2.0s)") {
				t.Errorf("FormatFMP4Drop = %q, want the video gap named", FormatFMP4Drop(d))
			}
		})
	}
}

// TestWalkFMP4ResyncWantsADeclaredTrack: bytes inside the damage that spell a
// moof which parses — here a fragment of a track this init never declared,
// with no mfhd — are not where the stream picks up again. The drop runs on
// to the next fragment of the stream's own track.
//
// Mutant: resync's candidate check parsing the moof non-strictly — the
// foreign fragment becomes the resync point, the walk accepts it, and the
// drop stops short of it.
func TestWalkFMP4ResyncWantsADeclaredTrack(t *testing.T) {
	data, offs := synthStream(10, 0)
	foreign := synthBox("moof", synthBox("traf",
		synthFull("tfhd", 0, 0x020000, u32(99)),
		synthFull("trun", 0, 0x301, u32(1), u32(0), u32(3600), u32(16))))
	foreign = append(foreign, synthBox("mdat", make([]byte, 16))...)
	damage := append(make([]byte, 300), foreign...)
	damage = append(damage, make([]byte, 300)...)
	w := mustWalk(t, splice(data, offs[4], offs[5], damage))
	if len(w.Drops) != 1 || w.Drops[0].Start != int64(offs[4]) || w.Drops[0].End != int64(offs[4]+len(damage)) {
		t.Fatalf("drops = %+v, want one drop over all %d damaged bytes", w.Drops, len(damage))
	}
	if w.Fragments != 9 {
		t.Errorf("fragments = %d, want 9 — the foreign fragment is not one of the stream's", w.Fragments)
	}
}

// TestWalkFMP4DamageRightAfterTheInit: the first fragment is the bad one.
// Nothing precedes the gap, and the timeline starts at the first readable
// fragment.
//
// Mutant: recordDrop setting HasFrom unconditionally — the gap claims to
// start at 0 s of a track that had no fragment yet.
func TestWalkFMP4DamageRightAfterTheInit(t *testing.T) {
	data, offs := synthStream(10, 0)
	w := mustWalk(t, splice(data, offs[0], offs[1], make([]byte, offs[1]-offs[0])))
	if len(w.Drops) != 1 || w.Drops[0].Start != int64(offs[0]) || w.Drops[0].End != int64(offs[1]) {
		t.Fatalf("drops = %+v, want the first fragment's bytes", w.Drops)
	}
	if g := w.Drops[0].Gaps[0]; g.HasFrom || !g.HasTo || !near(g.To, 0) {
		t.Errorf("gap = %+v, want no start and the timeline's own 0 s as its end", g)
	}
	if got := w.Tracks[0].SpanSeconds(); !near(got, 18) || w.Fragments != 9 {
		t.Errorf("span = %.3fs over %d fragments, want 18s over 9", got, w.Fragments)
	}
}

// TestWalkFMP4DamageInTheLastFragment: a bad last fragment runs to the end of
// the file; no fragment follows it.
//
// Mutant: damage() dropping nothing when the resync finds no fragment — the
// walk records no drop and the repair keeps the garbage.
func TestWalkFMP4DamageInTheLastFragment(t *testing.T) {
	data, offs := synthStream(10, 0)
	w := mustWalk(t, splice(data, offs[9], offs[10], make([]byte, offs[10]-offs[9])))
	if len(w.Drops) != 1 || w.Drops[0].Start != int64(offs[9]) || w.Drops[0].End != int64(offs[10]) {
		t.Fatalf("drops = %+v, want the last fragment's bytes to the end of the file", w.Drops)
	}
	if g := w.Drops[0].Gaps[0]; !g.HasFrom || g.HasTo || !near(g.From, 18) {
		t.Errorf("gap = %+v, want 18 s to the end", g)
	}
	if got := w.Tracks[0].SpanSeconds(); !near(got, 18) {
		t.Errorf("span = %.3fs, want 18s", got)
	}
}

// TestWalkFMP4TornTailIsNotADrop: a capture stopped mid-write leaves its last
// fragment cut short by the end of the file. Nothing follows it to keep, and
// FFmpeg reads what of it is there, so it is not damage a repair would have
// to drop — a drop would keep staging for every interrupted capture.
//
// Mutant: tornBox recording a drop to the end of the file.
func TestWalkFMP4TornTailIsNotADrop(t *testing.T) {
	data, offs := synthStream(10, 0)
	for _, cut := range []int{offs[9] + 30, offs[9] + (offs[10]-offs[9])/2, offs[9] + 5} {
		w := mustWalk(t, data[:cut])
		if w.Damaged() {
			t.Errorf("cut at %d: drops = %+v, want none", cut, w.Drops)
		}
		if w.TornTail != int64(offs[9]) || w.Fragments != 9 {
			t.Errorf("cut at %d: tornTail = %d, fragments = %d; want %d and 9", cut, w.TornTail, w.Fragments, offs[9])
		}
	}
}

// TestWalkFMP4TwoTracksInOneFile is the Twitch fMP4 shape: a muxed variant
// whose every moof carries a video traf and an audio traf. A damaged fragment
// leaves a gap in both.
//
// Mutants: accept() taking only the first traf of a moof (the audio track has
// no fragments and no span); counting a fragment per traf rather than per
// moof (18 fragments).
func TestWalkFMP4TwoTracksInOneFile(t *testing.T) {
	data := synthInit(synthTrack{1, "vide", 90000}, synthTrack{2, "soun", 48000})
	var offs []int
	for i := range 10 {
		offs = append(offs, len(data))
		data = append(data, synthFragment(uint32(i+1),
			synthTraf{1, uint64(2*i) * 90000, 50, 3600, 100},
			synthTraf{2, uint64(2*i) * 48000, 96, 1000, 20})...)
	}
	offs = append(offs, len(data))
	w := mustWalk(t, splice(data, offs[3], offs[4], make([]byte, offs[4]-offs[3])))
	if len(w.Tracks) != 2 || len(w.Drops) != 1 || w.Fragments != 9 {
		t.Fatalf("tracks = %d, drops = %+v, fragments = %d; want 2 tracks, one drop and 9 fragments", len(w.Tracks), w.Drops, w.Fragments)
	}
	for i, want := range []string{"vide", "soun"} {
		tr := w.Tracks[i]
		if tr.Handler != want || !near(tr.SpanSeconds(), 20) || tr.Fragments != 9 {
			t.Errorf("track %d = %s span %.3fs over %d fragments, want %s 20s over 9", i, tr.Handler, tr.SpanSeconds(), tr.Fragments, want)
		}
		if g := w.Drops[0].Gaps[i]; !near(g.From, 6) || !near(g.To, 8) {
			t.Errorf("track %d gap = %+v, want 6s to 8s", i, g)
		}
	}
}

// TestWalkFMP4AdjacentDamageIsOneDrop: two bad fragments in a row are one
// stretch, one log line, one cut. The case that needs the fold: a zeroed
// fragment, then a torn one the resync lands on — it parses, so the walk
// accepts it, and only the damage its oversized mdat carries the walk into
// shows it was torn, which drops it from where the first drop ended.
//
// Mutant: recordDrop without the fold — two drops for one hole.
func TestWalkFMP4AdjacentDamageIsOneDrop(t *testing.T) {
	data, offs := synthStream(10, 0)
	frag5 := data[offs[5]:offs[6]]
	bad := splice(data, offs[5], offs[6], frag5[:len(frag5)/2])
	bad = splice(bad, offs[4], offs[5], make([]byte, offs[5]-offs[4]))
	end := offs[5] + len(frag5)/2
	w := mustWalk(t, bad)
	if len(w.Drops) != 1 || w.Drops[0].Start != int64(offs[4]) || w.Drops[0].End != int64(end) {
		t.Fatalf("drops = %+v, want one drop over both fragments (%d-%d)", w.Drops, offs[4], end)
	}
	if g := w.Drops[0].Gaps[0]; !near(g.From, 8) || !near(g.To, 12) {
		t.Errorf("gap = %+v, want 8s to 12s", g)
	}
	if w.Fragments != 8 {
		t.Errorf("fragments = %d, want 8", w.Fragments)
	}
}

// TestWalkFMP4Refusals: a file that is not a fragmented MP4 is not the walk's
// business; one that looks like one and cannot be read is a refusal of its
// own, which the mux treats as unmeasured.
//
// Mutants: readInit answering ErrNotFMP4 for fragments with no init (the
// worker then measures them as it measures a TS); WalkFMP4At returning an
// all-garbage walk with no fragments as readable.
func TestWalkFMP4Refusals(t *testing.T) {
	ts := bytes.Repeat(append([]byte{0x47, 0x40, 0x00, 0x10}, make([]byte, 184)...), 20)
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3, 0x01, 0, 0, 0}, make([]byte, 64)...)
	progressive := append(synthBox("ftyp", []byte("isom"), u32(0)),
		synthBox("moov", synthBox("trak", synthFull("tkhd", 0, 3, u32(0), u32(0), u32(1), u32(0), u32(0))))...)
	progressive = append(progressive, synthBox("mdat", make([]byte, 64))...)
	moovAtEnd := append(synthBox("ftyp", []byte("isom"), u32(0)), synthBox("mdat", make([]byte, 64))...)
	_, offs := synthStream(3, 0)
	stream, _ := synthStream(3, 0)
	initOnly := stream[:offs[0]]
	bare := stream[offs[0]:]
	garbage := append(append([]byte{}, initOnly...), make([]byte, 4096)...)
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"mpeg-ts", ts, ErrNotFMP4},
		{"webm", webm, ErrNotFMP4},
		{"progressive mp4, moov up front", progressive, ErrNotFMP4},
		{"progressive mp4, moov at the end", moovAtEnd, ErrNotFMP4},
		{"empty", nil, ErrNotFMP4},
		{"fragments with no init", bare, ErrUnreadableFMP4},
		{"an init and garbage", garbage, ErrUnreadableFMP4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := walkBytes(t, tc.data); !errors.Is(err, tc.want) {
				t.Errorf("WalkFMP4At = %v, want %v", err, tc.want)
			}
		})
	}
	// An init with nothing after it is a capture that caught nothing: a walk
	// of its own, with nothing to measure.
	w := mustWalk(t, initOnly)
	if w.Fragments != 0 || w.Damaged() || w.Tracks[0].SpanSeconds() != 0 {
		t.Errorf("init only: %+v, want a walk with no fragments and no drops", w)
	}
}

// TestWalkFMP4FlagsAnUnreliableTimeline: decode times that run backwards — a
// second init mid-file that starts the timeline over, fragments appended
// twice — make the timeline no measure of the file. A second init the
// timeline runs straight on through (the HLS loop's inline init, which FFmpeg
// reads past) does not.
//
// Mutants: the backwards check in accept() removed; damage() treating an
// inline moov as damage (the inline row drops the init and the fragments
// after it).
func TestWalkFMP4FlagsAnUnreliableTimeline(t *testing.T) {
	data, offs := synthStream(6, 0)
	inline := splice(data, offs[3], offs[3], synthInit(synthTrack{1, "vide", 90000}))
	if w := mustWalk(t, inline); w.Unreliable != "" || w.Damaged() || w.Fragments != 6 || !near(w.Tracks[0].SpanSeconds(), 12) {
		t.Errorf("inline init: unreliable=%q drops=%v fragments=%d span=%.3f, want a clean 12 s walk of 6 fragments",
			w.Unreliable, w.Drops, w.Fragments, w.Tracks[0].SpanSeconds())
	}
	back := synthInit(synthTrack{1, "vide", 90000})
	for i, sec := range []uint64{0, 2, 4, 0, 2} {
		back = append(back, synthFragment(uint32(i+1), synthTraf{1, sec * 90000, 50, 3600, 100})...)
	}
	if w := mustWalk(t, back); !strings.Contains(w.Unreliable, "backwards") {
		t.Errorf("backwards decode times: unreliable=%q, want a backwards reason", w.Unreliable)
	}
	if w := mustWalk(t, data); w.Unreliable != "" {
		t.Errorf("healthy stream: unreliable=%q", w.Unreliable)
	}
}

// TestWriteFMP4RepairKeepsEveryIntactFragment: the repaired copy is the
// original without the drop — every fragment after the damage in it, the
// timeline unchanged — and its index boxes no longer address offsets the
// cut moved. The original is not touched.
//
// Mutants: copyKeptRanges copying the drop too (the repaired copy walks
// damaged); skipping the index patch (a sidx is still in the copy);
// WriteFMP4Repair appending to an existing dst rather than truncating it.
func TestWriteFMP4RepairKeepsEveryIntactFragment(t *testing.T) {
	data, offs := synthStream(10, 0)
	damaged := splice(data, offs[4], offs[5], make([]byte, offs[5]-offs[4]))
	dir := t.TempDir()
	src := filepath.Join(dir, "video_stream")
	if err := os.WriteFile(src, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	walk, err := WalkFMP4(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "video_stream.repair")
	if err := os.WriteFile(dst, bytes.Repeat([]byte("stale"), 100000), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := WriteFMP4Repair(context.Background(), src, walk, dst)
	if err != nil {
		t.Fatalf("WriteFMP4Repair: %v", err)
	}
	repaired, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(repaired)) || n != walk.RepairedSize() {
		t.Errorf("wrote %d bytes, file holds %d, RepairedSize %d", n, len(repaired), walk.RepairedSize())
	}
	if bytes.Contains(repaired, []byte("sidx")) {
		t.Error("the repaired copy still holds a sidx addressing offsets the cut moved")
	}
	rw := mustWalk(t, repaired)
	if rw.Damaged() || rw.Fragments != 9 || !near(rw.Tracks[0].SpanSeconds(), 20) {
		t.Errorf("repaired copy walks as drops=%v fragments=%d span=%.3f, want clean, 9, 20s", rw.Drops, rw.Fragments, rw.Tracks[0].SpanSeconds())
	}
	if orig, _ := os.ReadFile(src); !bytes.Equal(orig, damaged) {
		t.Error("the staged original was modified")
	}
}

// TestWalkFMP4HonoursCancellation: a walk of a multi-GB file is cancellable.
func TestWalkFMP4HonoursCancellation(t *testing.T) {
	data, _ := synthStream(10, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := WalkFMP4At(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Errorf("WalkFMP4At on a cancelled context = %v, want context.Canceled", err)
	}
}
