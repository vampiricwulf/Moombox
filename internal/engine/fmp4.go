package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// A live capture is staged as one fragmented-MP4 stream: the init segment
// (ftyp+moov), then every fragment appended as it arrived (downloader_init.go,
// downloader_dash.go, downloader_parallel.go, and the HLS loop's EXT-X-MAP
// init for Twitch fMP4). No fragment's box structure is checked on the way
// in, so one damaged fragment mid-file — zeroed, torn, an HTML error body a
// CDN served with 200 — sits in the stream like any other. FFmpeg's mov
// demuxer stops at it, `-c copy` exits 0 having written everything before it,
// and ffprobe, which reads the input with the same demuxer, agrees with the
// short copy: nothing that asks FFmpeg can see the fragments after the damage
// (W30-A11).
//
// WalkFMP4 reads the stream's boxes itself. It reads every moof (a few KB)
// and seeks over every mdat, so a healthy multi-GB recording costs a few
// small reads per fragment and none of its media bytes. Where a box cannot be
// read it resyncs on the next fragment that parses and records the bytes in
// between as a drop, with the hole each track's timeline has there;
// WriteFMP4Repair writes the stream without them, so a mux carries every
// intact fragment after the damage across. Each track's own timeline — the
// first readable fragment's decode time to the end of the last one's samples
// — is what the copy is measured against afterwards, independently of the
// demuxer that stopped.

// ErrNotFMP4 is WalkFMP4's answer for a file that is not a fragmented MP4: an
// MPEG-TS or WebM capture, or an MP4 whose moov describes every sample (no
// mvex). The walk has nothing to say about such a file.
var ErrNotFMP4 = errors.New("not a fragmented MP4")

// ErrUnreadableFMP4 is WalkFMP4's answer for a file shaped like a fragmented
// MP4 that the walk cannot read: fragments with no init segment ahead of
// them, an init that does not parse, or bytes after the init of which not
// one fragment is readable. Such a file is never repaired, and nothing
// independent of FFmpeg measures it.
var ErrUnreadableFMP4 = errors.New("fragmented MP4 the walk cannot read")

// FMP4Track is one track of a fragmented MP4 as its init declares it and its
// fragments fill it.
type FMP4Track struct {
	ID uint32
	// Handler is the hdlr handler_type: "vide", "soun", or whatever else a
	// writer put there.
	Handler string
	// Timescale is the mdhd timescale: decode-time units per second.
	Timescale uint32
	// First is the decode time of the first readable fragment's first sample,
	// End the decode time just past the last readable fragment's last one,
	// both in Timescale units. A hole a drop leaves stays inside the span:
	// the fragments after it keep their own decode times, and so does a copy
	// of them.
	First, End uint64
	// Fragments is how many readable fragments carried samples of this track.
	Fragments int

	seen            bool
	defaultDuration uint32 // trex default_sample_duration
}

// SpanSeconds is the length of the track's timeline in seconds, from its
// first readable fragment to the end of its last — what a whole copy of the
// track lasts. 0 for a track no readable fragment carried.
func (t *FMP4Track) SpanSeconds() float64 {
	if !t.seen || t.Timescale == 0 || t.End < t.First {
		return 0
	}
	return float64(t.End-t.First) / float64(t.Timescale)
}

// seconds places a decode time on the track's timeline, in seconds from its
// first readable fragment.
func (t *FMP4Track) seconds(dt uint64) float64 {
	if t.Timescale == 0 || dt < t.First {
		return 0
	}
	return float64(dt-t.First) / float64(t.Timescale)
}

// FMP4TrackGap is the hole one drop leaves in one track's timeline.
type FMP4TrackGap struct {
	TrackID uint32
	Handler string
	// From is where the track's last readable fragment before the drop ends,
	// To where its first readable fragment after it begins, in seconds on the
	// track's timeline (counted from its first readable fragment). HasFrom is
	// false when the damage came before the track's first readable fragment,
	// HasTo when no readable fragment of the track follows it.
	From, To       float64
	HasFrom, HasTo bool

	fromDT, toDT uint64
}

// FMP4Drop is one stretch of the file the walk could not read, as the byte
// range [Start, End) a repair leaves out.
type FMP4Drop struct {
	Start, End int64
	// Reason says what the walk found where the damage begins.
	Reason string
	// Gaps has one entry per track the init declares, in its order.
	Gaps []FMP4TrackGap
}

// FMP4Walk is what WalkFMP4 found in one file.
type FMP4Walk struct {
	Size   int64
	Tracks []*FMP4Track
	// Drops is every stretch the walk could not read, in file order. Empty
	// for a healthy file, which is muxed as it stands.
	Drops []FMP4Drop
	// Fragments is how many readable fragments the walk accepted.
	Fragments int
	// TornTail is the offset of a last fragment the end of the file cuts
	// short — a capture stopped mid-write. It is not a drop: nothing follows
	// it for a repair to keep, and FFmpeg reads what of it is there. 0 when
	// the file ends on a whole fragment.
	TornTail int64
	// Unreliable names why the fragments' decode times are not the measure of
	// the file — a track whose decode times run backwards, as they do when a
	// second init mid-file starts its timeline over, or when fragments were
	// appended twice — and is "" when they are.
	Unreliable string

	// indexBoxes are the offsets of the top-level sidx and mfra boxes the walk
	// kept. Both address fragments by byte offset, and a repair that cuts a
	// stretch out turns those offsets into lies, so it renames them free.
	indexBoxes []int64
}

// Damaged reports whether the walk dropped anything.
func (w *FMP4Walk) Damaged() bool { return len(w.Drops) > 0 }

// DroppedBytes is how many bytes the walk's drops cover.
func (w *FMP4Walk) DroppedBytes() int64 {
	var n int64
	for _, d := range w.Drops {
		n += d.End - d.Start
	}
	return n
}

const (
	// fmp4MaxMoof bounds the moof the walk reads into memory. A fragment's
	// moof is a few KB (a sample's trun entry is at most 16 bytes); a size
	// past this is damage, not a fragment.
	fmp4MaxMoof = 16 << 20
	// fmp4MaxInit bounds the moov the walk reads. An init segment's is a few
	// KB; one this size is not an init segment.
	fmp4MaxInit = 64 << 20
	// fmp4ScanChunk is how much the resync scan reads at a time.
	fmp4ScanChunk = 1 << 20
	// fmp4MaxLeading bounds how many boxes may lead a fragment's moof (styp,
	// sidx, prft, emsg...) in the resync's candidate check.
	fmp4MaxLeading = 16
)

// fmp4Leading are the top-level boxes that may sit between two fragments, or
// ahead of a fragment's moof, without belonging to its media.
var fmp4Leading = map[string]bool{
	"styp": true, "sidx": true, "ssix": true, "prft": true, "emsg": true,
	"free": true, "skip": true, "uuid": true, "wide": true,
}

// fmp4TopLevel are the other top-level boxes the walk reads past: an inline
// init (ftyp, moov), an mdat no moof describes, the random-access index at
// the end of a file, and two that only ever lead a file.
var fmp4TopLevel = map[string]bool{
	"moof": true, "mdat": true, "ftyp": true, "moov": true, "mfra": true, "meta": true, "pdin": true,
}

// fmp4ResyncTypes are the box types a resync looks for: what a fragment can
// begin with.
var fmp4ResyncTypes = [][]byte{[]byte("moof"), []byte("styp"), []byte("sidx"), []byte("prft"), []byte("emsg")}

// errBoxShort is readBox's answer when fewer bytes are left than a box
// header needs: the end of the file cuts it.
var errBoxShort = errors.New("box header cut short by the end of the file")

// fmp4BadBox is a box that cannot be read where it stands: damage, as against
// an error reading the file.
type fmp4BadBox struct{ msg string }

func (e *fmp4BadBox) Error() string { return e.msg }

func badBox(format string, args ...any) error { return &fmp4BadBox{msg: fmt.Sprintf(format, args...)} }

// isBadBox reports whether err is damage rather than a failure to read.
func isBadBox(err error) bool {
	var bb *fmp4BadBox
	return errors.As(err, &bb)
}

// fmp4Box is one box header.
type fmp4Box struct {
	typ   string
	start int64
	size  int64 // the whole box, header included
	hdr   int64 // header length: 8, or 16 with a 64-bit size
	// pastEOF is set when the box claims more bytes than the file holds.
	pastEOF bool
}

func (b fmp4Box) end() int64 { return b.start + b.size }

// fmp4Child is one child box inside a parent read into memory.
type fmp4Child struct {
	typ     string
	payload []byte
}

// fmp4Walker carries one walk's state.
type fmp4Walker struct {
	ctx  context.Context
	r    io.ReaderAt
	size int64
	walk *FMP4Walk
	byID map[uint32]int // track ID -> index in walk.Tracks

	// pending lists, per track index, the drops whose To that track's next
	// readable fragment fills in.
	pending map[int][]int
	// last is the fragment accepted most recently, kept until what follows
	// it shows it was whole: a torn fragment's mdat claims the bytes after
	// it, and only the damage it lands in gives that away.
	last *fmp4Accepted
}

// fmp4Accepted is what accepting one fragment changed, so it can be undone.
type fmp4Accepted struct {
	start, mdatPayload, end int64
	before                  []FMP4Track
	filled                  [][2]int // (drop, track) pairs whose To it set
}

// WalkFMP4 walks the top-level boxes of a staged fragmented MP4. See the
// comment at the top of this file for what it records; ErrNotFMP4 and
// ErrUnreadableFMP4 are its two refusals, and any other error is a failure
// to read the file (or ctx's).
func WalkFMP4(ctx context.Context, path string) (*FMP4Walk, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return WalkFMP4At(ctx, f, info.Size())
}

// WalkFMP4At is WalkFMP4 over any ReaderAt of the given size.
func WalkFMP4At(ctx context.Context, r io.ReaderAt, size int64) (*FMP4Walk, error) {
	wk := &fmp4Walker{
		ctx:     ctx,
		r:       r,
		size:    size,
		walk:    &FMP4Walk{Size: size},
		byID:    map[uint32]int{},
		pending: map[int][]int{},
	}
	pos, err := wk.readInit()
	if err != nil {
		return nil, err
	}
	if err := wk.walkFragments(pos); err != nil {
		return nil, err
	}
	// Bytes after the init and not one readable fragment among them: there
	// is no timeline to measure anything by, and nothing worth a repair. An
	// init with nothing after it is a capture that caught nothing, which is
	// a walk of its own (every span 0).
	if wk.walk.Fragments == 0 && wk.walk.Damaged() {
		return nil, fmt.Errorf("%w: no readable fragment after the init segment (%s)", ErrUnreadableFMP4, wk.walk.Drops[0].Reason)
	}
	wk.settleGaps()
	return wk.walk, nil
}

// readBox reads the box header at off. A header that cannot be a box is a
// *fmp4BadBox; one the end of the file cuts is errBoxShort.
func (wk *fmp4Walker) readBox(off int64) (fmp4Box, error) {
	if wk.size-off < 8 {
		return fmp4Box{}, errBoxShort
	}
	var h [16]byte
	if _, err := wk.r.ReadAt(h[:8], off); err != nil {
		return fmp4Box{}, err
	}
	b := fmp4Box{typ: string(h[4:8]), start: off, hdr: 8}
	switch size32 := binary.BigEndian.Uint32(h[:4]); {
	case size32 == 1:
		if wk.size-off < 16 {
			return fmp4Box{}, errBoxShort
		}
		if _, err := wk.r.ReadAt(h[8:16], off+8); err != nil {
			return fmp4Box{}, err
		}
		size64 := binary.BigEndian.Uint64(h[8:16])
		if size64 < 16 || size64 > 1<<62 {
			return fmp4Box{}, badBox("box %q at byte %d has a 64-bit size of %d", b.typ, off, size64)
		}
		b.size, b.hdr = int64(size64), 16
	case size32 == 0:
		// "To the end of the file": legal for a last mdat only.
		if b.typ != "mdat" {
			return fmp4Box{}, badBox("box %q at byte %d has size 0", b.typ, off)
		}
		b.size = wk.size - off
	case size32 < 8:
		return fmp4Box{}, badBox("box %q at byte %d has a size of %d", b.typ, off, size32)
	default:
		b.size = int64(size32)
	}
	b.pastEOF = b.size > wk.size-off
	return b, nil
}

// readInit walks the boxes ahead of the first fragment and parses the moov
// among them. It returns where the fragments begin.
func (wk *fmp4Walker) readInit() (int64, error) {
	var pos int64
	for first := true; ; first = false {
		b, err := wk.readBox(pos)
		switch {
		case err != nil && !isBadBox(err) && !errors.Is(err, errBoxShort):
			return 0, err
		case err != nil && first:
			return 0, ErrNotFMP4
		case err != nil:
			return 0, fmt.Errorf("%w: %v inside the init segment", ErrUnreadableFMP4, err)
		}
		switch b.typ {
		case "ftyp", "free", "skip", "wide", "uuid", "pdin", "meta":
			if b.pastEOF {
				return 0, fmt.Errorf("%w: the file ends inside its init segment", ErrUnreadableFMP4)
			}
		case "mdat":
			// A progressive MP4 with its moov after its media: every sample
			// is in that moov, and there are no fragments to walk.
			return 0, ErrNotFMP4
		case "moov":
			if b.pastEOF || b.size > fmp4MaxInit {
				return 0, fmt.Errorf("%w: the moov at byte %d does not fit in the file", ErrUnreadableFMP4, pos)
			}
			fragmented, err := wk.parseMoov(b)
			if err != nil {
				if isBadBox(err) {
					return 0, fmt.Errorf("%w: %v", ErrUnreadableFMP4, err)
				}
				return 0, err
			}
			if !fragmented {
				return 0, ErrNotFMP4
			}
			if len(wk.walk.Tracks) == 0 {
				return 0, fmt.Errorf("%w: the init segment declares no track", ErrUnreadableFMP4)
			}
			return b.end(), nil
		case "moof", "styp", "sidx", "prft", "emsg":
			return 0, fmt.Errorf("%w: fragments with no init segment ahead of them", ErrUnreadableFMP4)
		default:
			if first {
				return 0, ErrNotFMP4
			}
			return 0, fmt.Errorf("%w: box %q at byte %d inside the init segment", ErrUnreadableFMP4, b.typ, pos)
		}
		pos = b.end()
	}
}

// readBody reads a whole box into memory.
func (wk *fmp4Walker) readBody(b fmp4Box) ([]byte, error) {
	buf := make([]byte, b.size)
	if _, err := wk.r.ReadAt(buf, b.start); err != nil {
		return nil, err
	}
	return buf, nil
}

// childBoxes splits a box's payload into its child boxes. A child that does
// not fit inside its parent is damage.
func childBoxes(payload []byte) ([]fmp4Child, error) {
	var out []fmp4Child
	off := 0
	for off < len(payload) {
		if len(payload)-off < 8 {
			return nil, badBox("%d stray bytes at the end of a box", len(payload)-off)
		}
		size := int(binary.BigEndian.Uint32(payload[off : off+4]))
		typ := string(payload[off+4 : off+8])
		hdr := 8
		switch size {
		case 1:
			if len(payload)-off < 16 {
				return nil, badBox("child %q cut short", typ)
			}
			s64 := binary.BigEndian.Uint64(payload[off+8 : off+16])
			if s64 < 16 || s64 > uint64(len(payload)-off) {
				return nil, badBox("child %q has a size of %d", typ, s64)
			}
			size, hdr = int(s64), 16
		case 0:
			size = len(payload) - off
		}
		if size < hdr || size > len(payload)-off {
			return nil, badBox("child %q has a size of %d", typ, size)
		}
		out = append(out, fmp4Child{typ: typ, payload: payload[off+hdr : off+size]})
		off += size
	}
	return out, nil
}

// parseMoov reads the tracks a moov declares — tkhd's track ID, mdhd's
// timescale, hdlr's handler type — and mvex's trex default durations.
// fragmented is false for a moov without mvex: its samples are all in its
// own tables.
func (wk *fmp4Walker) parseMoov(b fmp4Box) (fragmented bool, err error) {
	body, err := wk.readBody(b)
	if err != nil {
		return false, err
	}
	children, err := childBoxes(body[b.hdr:])
	if err != nil {
		return false, err
	}
	defaults := map[uint32]uint32{}
	for _, c := range children {
		switch c.typ {
		case "trak":
			t, err := parseTrak(c.payload)
			if err != nil {
				return false, err
			}
			if _, dup := wk.byID[t.ID]; dup {
				// A second init repeating a track: the first declaration
				// stands, as it does for FFmpeg.
				continue
			}
			wk.byID[t.ID] = len(wk.walk.Tracks)
			wk.walk.Tracks = append(wk.walk.Tracks, t)
		case "mvex":
			fragmented = true
			kids, err := childBoxes(c.payload)
			if err != nil {
				return false, err
			}
			for _, k := range kids {
				if k.typ != "trex" {
					continue
				}
				// version/flags, track_ID, default_sample_description_index,
				// default_sample_duration, default_sample_size,
				// default_sample_flags.
				if len(k.payload) < 24 {
					return false, badBox("a trex of %d bytes", len(k.payload))
				}
				defaults[binary.BigEndian.Uint32(k.payload[4:8])] = binary.BigEndian.Uint32(k.payload[12:16])
			}
		}
	}
	for id, d := range defaults {
		if i, ok := wk.byID[id]; ok && wk.walk.Tracks[i].defaultDuration == 0 {
			wk.walk.Tracks[i].defaultDuration = d
		}
	}
	return fragmented, nil
}

// parseTrak reads one trak's track ID, timescale and handler type.
func parseTrak(payload []byte) (*FMP4Track, error) {
	t := &FMP4Track{}
	kids, err := childBoxes(payload)
	if err != nil {
		return nil, err
	}
	sawTkhd := false
	for _, k := range kids {
		switch k.typ {
		case "tkhd":
			// version 1: creation(8) modification(8) track_ID; version 0:
			// creation(4) modification(4) track_ID.
			idOff := 12
			if len(k.payload) > 0 && k.payload[0] == 1 {
				idOff = 20
			}
			if len(k.payload) < idOff+4 {
				return nil, badBox("a tkhd of %d bytes", len(k.payload))
			}
			t.ID = binary.BigEndian.Uint32(k.payload[idOff : idOff+4])
			sawTkhd = true
		case "mdia":
			mk, err := childBoxes(k.payload)
			if err != nil {
				return nil, err
			}
			for _, m := range mk {
				switch m.typ {
				case "mdhd":
					tsOff := 12
					if len(m.payload) > 0 && m.payload[0] == 1 {
						tsOff = 20
					}
					if len(m.payload) < tsOff+4 {
						return nil, badBox("an mdhd of %d bytes", len(m.payload))
					}
					t.Timescale = binary.BigEndian.Uint32(m.payload[tsOff : tsOff+4])
				case "hdlr":
					// version/flags, pre_defined, handler_type.
					if len(m.payload) < 12 {
						return nil, badBox("an hdlr of %d bytes", len(m.payload))
					}
					t.Handler = string(m.payload[8:12])
				}
			}
		}
	}
	if !sawTkhd {
		return nil, badBox("a trak without tkhd")
	}
	return t, nil
}

// fmp4Traf is one traf of a moof, reduced to what the timeline needs.
type fmp4Traf struct {
	trackID  uint32
	hasTfdt  bool
	tfdt     uint64
	duration uint64
}

// parseMoof reads a moof's trafs. strict asks for what a resync demands of a
// candidate on top of a moof that parses: an mfhd, and a traf of a track the
// init declares — bytes that happen to spell "moof" inside damage are not a
// fragment.
func (wk *fmp4Walker) parseMoof(b fmp4Box, strict bool) ([]fmp4Traf, error) {
	if b.size > fmp4MaxMoof {
		return nil, badBox("a moof of %d bytes", b.size)
	}
	body, err := wk.readBody(b)
	if err != nil {
		return nil, err
	}
	kids, err := childBoxes(body[b.hdr:])
	if err != nil {
		return nil, err
	}
	var trafs []fmp4Traf
	sawMfhd, known := false, false
	for _, k := range kids {
		switch k.typ {
		case "mfhd":
			if len(k.payload) < 8 {
				return nil, badBox("an mfhd of %d bytes", len(k.payload))
			}
			sawMfhd = true
		case "traf":
			tr, err := wk.parseTraf(k.payload)
			if err != nil {
				return nil, err
			}
			if _, ok := wk.byID[tr.trackID]; ok {
				known = true
			}
			trafs = append(trafs, tr)
		}
	}
	if len(trafs) == 0 {
		return nil, badBox("a moof with no traf")
	}
	if strict && (!sawMfhd || !known) {
		return nil, badBox("a moof of no declared track")
	}
	return trafs, nil
}

// parseTraf reads one traf: tfhd's track ID and default sample duration,
// tfdt's base decode time, and every trun's sample durations.
func (wk *fmp4Walker) parseTraf(payload []byte) (fmp4Traf, error) {
	var tr fmp4Traf
	kids, err := childBoxes(payload)
	if err != nil {
		return tr, err
	}
	sawTfhd := false
	var defaultDuration uint32
	var truns [][]byte
	for _, k := range kids {
		p := k.payload
		switch k.typ {
		case "tfhd":
			if len(p) < 8 {
				return tr, badBox("a tfhd of %d bytes", len(p))
			}
			flags := binary.BigEndian.Uint32(p[0:4]) & 0xffffff
			tr.trackID = binary.BigEndian.Uint32(p[4:8])
			off := 8
			if flags&0x01 != 0 { // base_data_offset
				off += 8
			}
			if flags&0x02 != 0 { // sample_description_index
				off += 4
			}
			if flags&0x08 != 0 { // default_sample_duration
				if len(p) < off+4 {
					return tr, badBox("a tfhd of %d bytes", len(p))
				}
				defaultDuration = binary.BigEndian.Uint32(p[off : off+4])
				off += 4
			}
			if flags&0x10 != 0 { // default_sample_size
				off += 4
			}
			if flags&0x20 != 0 { // default_sample_flags
				off += 4
			}
			if len(p) < off {
				return tr, badBox("a tfhd of %d bytes", len(p))
			}
			sawTfhd = true
		case "tfdt":
			switch {
			case len(p) >= 12 && p[0] == 1:
				tr.tfdt = binary.BigEndian.Uint64(p[4:12])
			case len(p) >= 8 && p[0] == 0:
				tr.tfdt = uint64(binary.BigEndian.Uint32(p[4:8]))
			default:
				return tr, badBox("a tfdt of %d bytes", len(p))
			}
			tr.hasTfdt = true
		case "trun":
			truns = append(truns, p)
		}
	}
	if !sawTfhd {
		return tr, badBox("a traf without tfhd")
	}
	if defaultDuration == 0 {
		if i, ok := wk.byID[tr.trackID]; ok {
			defaultDuration = wk.walk.Tracks[i].defaultDuration
		}
	}
	for _, p := range truns {
		d, err := trunDuration(p, defaultDuration)
		if err != nil {
			return tr, err
		}
		tr.duration += d
	}
	return tr, nil
}

// trunDuration sums one trun's sample durations, checking that its sample
// table fits in the box.
func trunDuration(p []byte, defaultDuration uint32) (uint64, error) {
	if len(p) < 8 {
		return 0, badBox("a trun of %d bytes", len(p))
	}
	flags := binary.BigEndian.Uint32(p[0:4]) & 0xffffff
	count := uint64(binary.BigEndian.Uint32(p[4:8]))
	off := 8
	if flags&0x01 != 0 { // data_offset
		off += 4
	}
	if flags&0x04 != 0 { // first_sample_flags
		off += 4
	}
	per := 0
	for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
		if flags&f != 0 {
			per += 4
		}
	}
	if uint64(len(p)) < uint64(off)+count*uint64(per) {
		return 0, badBox("a trun of %d samples in %d bytes", count, len(p))
	}
	if flags&0x100 == 0 {
		return count * uint64(defaultDuration), nil
	}
	var sum uint64
	for range count {
		sum += uint64(binary.BigEndian.Uint32(p[off : off+4]))
		off += per
	}
	return sum, nil
}

// walkFragments walks every top-level box from pos to the end of the file.
// fragStart is where the fragment being read begins, its leading boxes
// included: the start of whatever a drop takes when that fragment fails.
func (wk *fmp4Walker) walkFragments(pos int64) error {
	fragStart := pos
	for pos < wk.size {
		if err := wk.ctx.Err(); err != nil {
			return err
		}
		b, err := wk.readBox(pos)
		if errors.Is(err, errBoxShort) {
			wk.walk.TornTail = fragStart
			return nil
		}
		if err == nil && !fmp4Leading[b.typ] && !fmp4TopLevel[b.typ] {
			err = badBox("box %q at byte %d cannot be there", b.typ, pos)
		}
		if err != nil {
			if !isBadBox(err) {
				return err
			}
			if pos, err = wk.damage(pos, fragStart, err.Error()); err != nil {
				return err
			}
			fragStart = pos
			continue
		}
		if b.pastEOF {
			// A box the end of the file cuts: a capture stopped mid-write —
			// unless a fragment begins inside the bytes it claims, and then it
			// is a torn box with more after it.
			if pos, err = wk.tornBox(b.start+b.hdr, fragStart, fmt.Sprintf("box %q at byte %d claims %d bytes, past the next fragment", b.typ, pos, b.size)); err != nil || pos < 0 {
				return err
			}
			fragStart = pos
			continue
		}
		switch b.typ {
		case "moof":
			next, err := wk.readFragment(b, fragStart)
			if err != nil || next < 0 {
				return err
			}
			pos, fragStart = next, next
		case "moov":
			// A second init segment mid-file: the HLS loop writes one inline
			// when a VOD's init changes (ensureHlsInit). FFmpeg keeps the
			// first one's tracks, and so does parseMoov, adding only a track
			// the first did not declare. Decode times that start over after
			// it are caught where they run backwards (accept).
			if _, err := wk.parseMoov(b); err != nil {
				if !isBadBox(err) {
					return err
				}
				if pos, err = wk.damage(pos, fragStart, fmt.Sprintf("the moov at byte %d does not parse: %v", pos, err)); err != nil {
					return err
				}
				fragStart = pos
				continue
			}
			pos = b.end()
			fragStart, wk.last = pos, nil
		case "mdat":
			// An mdat no moof describes: no sample of it is read, by FFmpeg
			// or here.
			pos = b.end()
			fragStart, wk.last = pos, nil
		default:
			if b.typ == "sidx" || b.typ == "mfra" {
				wk.walk.indexBoxes = append(wk.walk.indexBoxes, pos)
			}
			pos = b.end()
		}
	}
	return nil
}

// readFragment reads the fragment whose moof is b: the moof, then the mdat
// right after it. It returns where the walk goes on, or -1 when the file ends
// in this fragment.
func (wk *fmp4Walker) readFragment(b fmp4Box, fragStart int64) (int64, error) {
	trafs, err := wk.parseMoof(b, false)
	if err != nil {
		if !isBadBox(err) {
			return 0, err
		}
		return wk.damage(b.start, fragStart, fmt.Sprintf("the moof at byte %d does not parse: %v", b.start, err))
	}
	m, err := wk.readBox(b.end())
	if errors.Is(err, errBoxShort) {
		wk.walk.TornTail = fragStart
		return -1, nil
	}
	if err == nil && m.typ != "mdat" {
		err = badBox("the moof at byte %d is followed by %q, not its mdat", b.start, m.typ)
	}
	if err != nil {
		if !isBadBox(err) {
			return 0, err
		}
		return wk.damage(b.end(), fragStart, err.Error())
	}
	if m.pastEOF {
		return wk.tornBox(m.start+m.hdr, fragStart, fmt.Sprintf("the mdat at byte %d claims %d bytes, past the next fragment", m.start, m.size))
	}
	wk.accept(fragStart, m.start+m.hdr, m.end(), trafs)
	return m.end(), nil
}

// tornBox handles a box whose claimed size runs past the end of the file,
// with its payload starting at payload: when a fragment begins inside those
// bytes, the box was torn and the walk goes on from that fragment with the
// torn one dropped; otherwise the file simply ends mid-fragment (TornTail)
// and tornBox returns -1.
func (wk *fmp4Walker) tornBox(payload, fragStart int64, reason string) (int64, error) {
	q, ok, err := wk.resync(payload)
	if err != nil {
		return 0, err
	}
	if !ok {
		wk.walk.TornTail = fragStart
		return -1, nil
	}
	wk.dropIndexBoxesFrom(fragStart)
	wk.recordDrop(fragStart, q, reason)
	wk.last = nil
	return q, nil
}

// damage handles the unreadable box at p, in the fragment that began at
// fragStart: it finds the next fragment that parses, records the bytes before
// it as a drop and returns where the walk goes on. When the damage sits right
// where the last accepted fragment ended, that fragment may itself be torn —
// its mdat claiming bytes it never had, which carried the walk over the start
// of the next fragment into the middle of a later one — so the search starts
// inside its mdat, and a fragment found there takes the torn one into the
// drop with it.
func (wk *fmp4Walker) damage(p, fragStart int64, reason string) (int64, error) {
	scanFrom := p
	if wk.last != nil && wk.last.end == p {
		scanFrom = wk.last.mdatPayload
	}
	q, ok, err := wk.resync(scanFrom)
	if err != nil {
		return 0, err
	}
	dropStart := fragStart
	if ok && wk.last != nil && q < p {
		dropStart = wk.last.start
		wk.rollback()
	}
	if !ok {
		q = wk.size
	}
	wk.dropIndexBoxesFrom(dropStart)
	wk.recordDrop(dropStart, q, reason)
	wk.last = nil
	return q, nil
}

// dropIndexBoxesFrom forgets the index boxes at or after off: they are in a
// drop, and a repair never writes them.
func (wk *fmp4Walker) dropIndexBoxesFrom(off int64) {
	wk.walk.indexBoxes = slices.DeleteFunc(wk.walk.indexBoxes, func(ib int64) bool { return ib >= off })
}

// resync returns the offset of the first fragment at or after from that
// parses: its leading boxes (styp, sidx...), then a moof that passes the
// strict parse, then an mdat header. ok is false when there is none.
func (wk *fmp4Walker) resync(from int64) (int64, bool, error) {
	// Eight bytes of overlap, so a type that straddles a chunk's end is seen
	// whole; a match is owned by the chunk its type field starts in.
	buf := make([]byte, fmp4ScanChunk+8)
	for base := from; base < wk.size; base += fmp4ScanChunk {
		if err := wk.ctx.Err(); err != nil {
			return 0, false, err
		}
		n, err := wk.r.ReadAt(buf, base)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, false, err
		}
		chunk := buf[:n]
		limit := min(n, fmp4ScanChunk)
		for i := 0; i < limit; {
			at := -1
			for _, t := range fmp4ResyncTypes {
				if j := bytes.Index(chunk[i:], t); j >= 0 && (at < 0 || i+j < at) {
					at = i + j
				}
			}
			if at < 0 || at >= limit {
				break
			}
			start := base + int64(at) - 4
			if start >= from {
				ok, err := wk.fragmentAt(start)
				if err != nil {
					return 0, false, err
				}
				if ok {
					return start, true, nil
				}
			}
			i = at + 1
		}
	}
	return 0, false, nil
}

// fragmentAt reports whether a fragment that parses begins at off.
func (wk *fmp4Walker) fragmentAt(off int64) (bool, error) {
	pos := off
	for range fmp4MaxLeading {
		b, err := wk.readBox(pos)
		if err != nil {
			if isBadBox(err) || errors.Is(err, errBoxShort) {
				return false, nil
			}
			return false, err
		}
		if b.pastEOF {
			return false, nil
		}
		if b.typ == "moof" {
			if _, err := wk.parseMoof(b, true); err != nil {
				if isBadBox(err) {
					return false, nil
				}
				return false, err
			}
			m, err := wk.readBox(b.end())
			if err != nil && !isBadBox(err) && !errors.Is(err, errBoxShort) {
				return false, err
			}
			return err == nil && m.typ == "mdat", nil
		}
		if !fmp4Leading[b.typ] {
			return false, nil
		}
		pos = b.end()
	}
	return false, nil
}

// accept adds one readable fragment to the tracks' timelines.
func (wk *fmp4Walker) accept(start, mdatPayload, end int64, trafs []fmp4Traf) {
	a := &fmp4Accepted{start: start, mdatPayload: mdatPayload, end: end}
	a.before = make([]FMP4Track, len(wk.walk.Tracks))
	for i, t := range wk.walk.Tracks {
		a.before[i] = *t
	}
	touched := map[int]bool{}
	for _, tr := range trafs {
		i, ok := wk.byID[tr.trackID]
		if !ok {
			continue // a track the init does not declare; FFmpeg skips it too
		}
		t := wk.walk.Tracks[i]
		begin := t.End
		if tr.hasTfdt {
			begin = tr.tfdt
		}
		switch {
		case !t.seen:
			t.First, t.End, t.seen = begin, begin, true
		case begin+uint64(t.Timescale) < t.End && wk.walk.Unreliable == "":
			// More than a second behind where the track had got to.
			wk.walk.Unreliable = fmt.Sprintf("the %s track's decode times run backwards at byte %d", t.Handler, start)
		}
		t.End = max(t.End, begin+tr.duration)
		if touched[i] {
			continue
		}
		touched[i] = true
		t.Fragments++
		for _, d := range wk.pending[i] {
			g := &wk.walk.Drops[d].Gaps[i]
			g.toDT, g.HasTo = begin, true
			a.filled = append(a.filled, [2]int{d, i})
		}
		delete(wk.pending, i)
	}
	wk.walk.Fragments++
	wk.last = a
}

// rollback undoes the last accepted fragment: it turned out to be torn.
func (wk *fmp4Walker) rollback() {
	a := wk.last
	for i := range a.before {
		*wk.walk.Tracks[i] = a.before[i]
	}
	for _, f := range a.filled {
		d, i := f[0], f[1]
		g := &wk.walk.Drops[d].Gaps[i]
		g.toDT, g.HasTo = 0, false
		wk.pending[i] = append(wk.pending[i], d)
	}
	wk.walk.Fragments--
	wk.last = nil
}

// recordDrop records [start, end) as dropped, folding it into the drop before
// it when the two touch.
func (wk *fmp4Walker) recordDrop(start, end int64, reason string) {
	if end <= start {
		return
	}
	if n := len(wk.walk.Drops); n > 0 && wk.walk.Drops[n-1].End == start {
		wk.walk.Drops[n-1].End = end
		return
	}
	d := FMP4Drop{Start: start, End: end, Reason: reason, Gaps: make([]FMP4TrackGap, len(wk.walk.Tracks))}
	idx := len(wk.walk.Drops)
	for i, t := range wk.walk.Tracks {
		d.Gaps[i] = FMP4TrackGap{TrackID: t.ID, Handler: t.Handler, fromDT: t.End, HasFrom: t.seen}
		wk.pending[i] = append(wk.pending[i], idx)
	}
	wk.walk.Drops = append(wk.walk.Drops, d)
}

// settleGaps turns every gap's decode times into seconds on its track's
// timeline, now that every track's first readable fragment is known. A track
// a later init declared has no slot in the drops before it.
func (wk *fmp4Walker) settleGaps() {
	for di := range wk.walk.Drops {
		for gi := range wk.walk.Drops[di].Gaps {
			g := &wk.walk.Drops[di].Gaps[gi]
			t := wk.walk.Tracks[gi]
			if g.HasFrom {
				g.From = t.seconds(g.fromDT)
			}
			if g.HasTo {
				g.To = t.seconds(g.toDT)
			}
		}
	}
}

// WriteFMP4Repair writes the walked file at src without the walk's drops to
// dst — created, or truncated if it exists — and returns how many bytes it
// wrote. The top-level sidx and mfra boxes it keeps are renamed free: both
// address fragments by byte offset, every offset past a cut is wrong, and the
// fragments themselves carry everything a reader needs. src is only read; a
// failed write removes dst.
func WriteFMP4Repair(ctx context.Context, src string, walk *FMP4Walk, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	written, werr := copyKeptRanges(ctx, in, out, walk)
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(dst)
		return 0, werr
	}
	return written, nil
}

// RepairedSize is how many bytes WriteFMP4Repair writes for this walk.
func (w *FMP4Walk) RepairedSize() int64 { return w.Size - w.DroppedBytes() }

// copyKeptRanges copies every byte of the walk's file outside its drops,
// writing "free" over the type of each kept index box.
func copyKeptRanges(ctx context.Context, in io.ReaderAt, out io.Writer, walk *FMP4Walk) (int64, error) {
	var kept [][2]int64
	var pos int64
	for _, d := range walk.Drops {
		if d.Start > pos {
			kept = append(kept, [2]int64{pos, d.Start})
		}
		pos = d.End
	}
	if pos < walk.Size {
		kept = append(kept, [2]int64{pos, walk.Size})
	}
	patches := slices.Clone(walk.indexBoxes)
	slices.Sort(patches)
	buf := make([]byte, 1<<20)
	var written int64
	copySpan := func(from, to int64) error {
		for from < to {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(int64(len(buf)), to-from)
			if _, err := in.ReadAt(buf[:n], from); err != nil {
				return err
			}
			w, err := out.Write(buf[:n])
			written += int64(w)
			if err != nil {
				return err
			}
			from += n
		}
		return nil
	}
	for _, r := range kept {
		from := r[0]
		for _, p := range patches {
			if p < from || p+8 > r[1] {
				continue
			}
			if err := copySpan(from, p+4); err != nil {
				return written, err
			}
			w, err := io.WriteString(out, "free")
			written += int64(w)
			if err != nil {
				return written, err
			}
			from = p + 8
		}
		if err := copySpan(from, r[1]); err != nil {
			return written, err
		}
	}
	return written, nil
}

// FormatFMP4Drop renders one drop for a log line: its byte range and, per
// track, the stretch of the track's timeline it leaves empty.
func FormatFMP4Drop(d FMP4Drop) string {
	var parts []string
	for _, g := range d.Gaps {
		from, to := "the start", "the end"
		if g.HasFrom {
			from = FormatTimelineSeconds(g.From)
		}
		if g.HasTo {
			to = FormatTimelineSeconds(g.To)
		}
		gap := ""
		if g.HasFrom && g.HasTo {
			gap = fmt.Sprintf(" (%.1fs)", g.To-g.From)
		}
		parts = append(parts, fmt.Sprintf("%s %s to %s%s", HandlerName(g.Handler), from, to, gap))
	}
	return fmt.Sprintf("bytes %d-%d (%d bytes): %s", d.Start, d.End, d.End-d.Start, strings.Join(parts, ", "))
}

// HandlerName is how a log line or a message names a track by its handler
// type: "video" and "audio" for the two every capture has, the type itself
// for anything else.
func HandlerName(handler string) string {
	switch handler {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	}
	return fmt.Sprintf("%q", handler)
}

// FormatTimelineSeconds renders seconds on a track's timeline as H:MM:SS.s.
func FormatTimelineSeconds(s float64) string {
	if s < 0 {
		s = 0
	}
	h := int(s) / 3600
	m := int(s) / 60 % 60
	sec := s - float64(h*3600+m*60)
	return fmt.Sprintf("%d:%02d:%04.1f", h, m, sec)
}
