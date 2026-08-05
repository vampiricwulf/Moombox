package engine

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Note: box(typ, body) is already defined in downloader_direct_validate_test.go
// (same package) with the identical signature used by these tests — reused
// here rather than redeclared.

func TestInspectSegmentInitCarrying(t *testing.T) {
	data := append(box("ftyp", []byte("iso6....")), append(box("moov", make([]byte, 32)), box("moof", make([]byte, 16))...)...)
	got := InspectSegment(data)
	if !got.HasFtyp || !got.HasMoov || got.FirstMediaBox != "moof" {
		t.Errorf("init-carrying segment misread: %+v", got)
	}
}

func TestInspectSegmentBareFragmentWithSidx(t *testing.T) {
	sidxBody := make([]byte, 24)
	binary.BigEndian.PutUint32(sidxBody[8:], 90000) // fullbox(4) + reference_ID(4), then timescale
	data := append(box("styp", []byte("msdh....")), append(box("sidx", sidxBody), append(box("moof", make([]byte, 16)), box("mdat", []byte{0, 0, 0, 1, 0x67, 0xAA})...)...)...)
	got := InspectSegment(data)
	if got.HasMoov || !got.HasSidx || got.SidxTimescale != 90000 {
		t.Errorf("bare fragment misread: %+v", got)
	}
	if got.SPSPPSHeuristic != "annexb" {
		t.Errorf("SPS heuristic = %q, want annexb (buffer contains 00 00 01 + NAL 7)", got.SPSPPSHeuristic)
	}
}

func TestInspectSegmentMalformed(t *testing.T) {
	got := InspectSegment([]byte{0, 0}) // too short for any box
	if got.HasFtyp || got.HasMoov || len(got.Boxes) != 0 {
		t.Errorf("malformed input must yield empty inspection: %+v", got)
	}
}

// --- Additional cases beyond the brief's verbatim set ---

func TestInspectSegmentPartialResultsOnMalformedTail(t *testing.T) {
	ftyp := box("ftyp", []byte("iso6...."))
	moov := box("moov", make([]byte, 16))
	// Malformed trailing box header: claims a size that overshoots the buffer.
	tail := []byte{0, 0, 0, 200, 'm', 'o', 'o', 'f'}
	data := append(append(append([]byte{}, ftyp...), moov...), tail...)

	got := InspectSegment(data)
	if !got.HasFtyp || !got.HasMoov {
		t.Fatalf("expected boxes collected before the malformed tail to survive: %+v", got)
	}
	if len(got.Boxes) != 2 || got.Boxes[0] != "ftyp" || got.Boxes[1] != "moov" {
		t.Errorf("expected walk to stop before recording the malformed box: %+v", got.Boxes)
	}
	if got.FirstMediaBox != "" {
		t.Errorf("malformed moof must not be recorded as FirstMediaBox: %q", got.FirstMediaBox)
	}
}

func TestInspectSegmentPossibleAVCCHeuristic(t *testing.T) {
	// No Annex-B start codes anywhere, but an AVCC-plausible 4-byte length
	// (5) immediately followed by a NAL-type-7 (SPS) byte.
	mdatBody := []byte{0, 0, 0, 5, 0x67, 0x00}
	data := box("mdat", mdatBody)

	got := InspectSegment(data)
	if got.SPSPPSHeuristic != "possible-avcc" {
		t.Errorf("SPS heuristic = %q, want possible-avcc", got.SPSPPSHeuristic)
	}
}

func TestInspectSegmentNoHeuristicMatch(t *testing.T) {
	data := append(box("ftyp", bytes.Repeat([]byte{0x41}, 8)), box("moov", bytes.Repeat([]byte{0x41}, 16))...)

	got := InspectSegment(data)
	if got.SPSPPSHeuristic != "none" {
		t.Errorf("SPS heuristic = %q, want none", got.SPSPPSHeuristic)
	}
}
