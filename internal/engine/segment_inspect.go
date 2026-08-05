package engine

import "encoding/binary"

// SegmentInspection is a diagnostic box-inventory of a raw MP4/CMAF segment.
// It is used exclusively for eviction-diagnosis logging (Task 9) and, later,
// Phase D's init resolver — nothing here drives a muxing decision.
type SegmentInspection struct {
	HasFtyp, HasMoov, HasSidx bool
	SidxTimescale             uint32   // 0 if no sidx / unparsed
	FirstMediaBox             string   // "moof", "mdat", "styp", or "" if none seen
	Boxes                     []string // top-level box types in order (diagnosis logging)
	// SPSPPSHeuristic: "annexb" (00 00 01 / 00 00 00 01 start codes followed
	// by a NAL type 7/8 byte, found anywhere in the buffer), "possible-avcc"
	// (a plausible AVCC-style 4-byte length prefix followed by such a NAL
	// type byte — checked only when no Annex-B start code was found), or
	// "none". Both are heuristics: cheap byte-pattern scans, not a real
	// bitstream parse. Diagnosis-only — no trun-guided sample walking, and no
	// muxing decision rides on this value.
	SPSPPSHeuristic string
}

// InspectSegment walks the top-level boxes of a raw MP4/CMAF segment and
// records everything useful for diagnosing an evicted/discontinuous
// segment: which init boxes are present, which box leads the media, the
// full top-level box order, and a cheap heuristic for whether H.264
// parameter sets are visible in the buffer.
//
// The walk loop is cloned from extractMP4InitBoxes's skeleton
// (downloader_init.go) — 4-byte size + type, 64-bit extended-size form,
// overshoot guards — but, unlike that function, InspectSegment does not stop
// at the first media box: it records every top-level box and keeps walking.
// sidx is indexing, not media, so — deliberately deviating from
// extractMP4InitBoxes' stop-set, which treats sidx as a stopping point —
// InspectSegment lets sidx set HasSidx without ending the walk or counting
// as FirstMediaBox; only moof/mdat/styp count as media.
//
// On a malformed box (short header, bad extended size, or a box that
// overshoots the buffer) the walk stops and whatever was collected so far is
// returned — inspection is best-effort diagnostic data, not a correctness
// gate, so it never discards prior progress because of a later malformed box.
func InspectSegment(data []byte) SegmentInspection {
	var result SegmentInspection

	off := 0
walk:
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		boxType := string(data[off+4 : off+8])

		headerLen := 8
		boxSize := size
		switch {
		case size == 1:
			// 64-bit extended size in the 8 bytes following the box header.
			if off+16 > len(data) {
				break walk
			}
			size64 := binary.BigEndian.Uint64(data[off+8 : off+16])
			// Same reasoning as extractMP4InitBoxes: the absolute cap
			// (size64 > len(data)) also guards int(size64) against
			// overflowing into a negative offset, and with that cap making
			// off+int(size64) safe from wraparound, the off-relative check
			// rejects a box claiming to extend past the fetched buffer.
			if size64 < 16 || size64 > uint64(len(data)) || off+int(size64) > len(data) {
				break walk
			}
			boxSize = int(size64)
			headerLen = 16
		case size < 8:
			// size 0 means "to EOF"; anything < 8 is a malformed header.
			break walk
		case off+size > len(data):
			// Box overshoots the fetched buffer — malformed for our purposes.
			break walk
		}

		result.Boxes = append(result.Boxes, boxType)

		switch boxType {
		case "ftyp":
			result.HasFtyp = true
		case "moov":
			result.HasMoov = true
		case "sidx":
			result.HasSidx = true
			// Fullbox header (version+flags, 4 bytes) + reference_ID
			// (4 bytes), then the big-endian uint32 timescale
			// (ISO/IEC 14496-12 8.16.3) — byte offset 8 within the box body.
			tsOff := off + headerLen + 8
			if tsOff+4 <= off+boxSize {
				result.SidxTimescale = binary.BigEndian.Uint32(data[tsOff : tsOff+4])
			}
		case "moof", "mdat", "styp":
			if result.FirstMediaBox == "" {
				result.FirstMediaBox = boxType
			}
		}

		off += boxSize
	}

	result.SPSPPSHeuristic = detectSPSPPSHeuristic(data)
	return result
}

// detectSPSPPSHeuristic scans the whole raw buffer for signs of H.264
// SPS/PPS NAL units. Diagnosis-only: a cheap byte-pattern scan, not a real
// bitstream parse — no muxing decision may depend on its result.
func detectSPSPPSHeuristic(data []byte) string {
	if hasAnnexBSPSOrPPS(data) {
		return "annexb"
	}
	if hasPossibleAVCCSPSOrPPS(data) {
		return "possible-avcc"
	}
	return "none"
}

// isSPSOrPPSNALType reports whether b's low 5 bits identify an H.264 SPS (7)
// or PPS (8) NAL unit type.
func isSPSOrPPSNALType(b byte) bool {
	t := b & 0x1F
	return t == 7 || t == 8
}

// hasAnnexBSPSOrPPS scans for an Annex-B start code (00 00 01 — which also
// matches the trailing three bytes of a 00 00 00 01 four-byte start code)
// immediately followed by an SPS/PPS NAL type byte, anywhere in the buffer.
func hasAnnexBSPSOrPPS(data []byte) bool {
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && isSPSOrPPSNALType(data[i+3]) {
			return true
		}
	}
	return false
}

// hasPossibleAVCCSPSOrPPS scans for a plausible AVCC-style NAL length prefix
// (4-byte big-endian length in [2, 1<<20]) immediately followed by an
// SPS/PPS NAL type byte. A far weaker signal than Annex-B — only consulted
// when no Annex-B start code was found.
func hasPossibleAVCCSPSOrPPS(data []byte) bool {
	for i := 0; i+4 < len(data); i++ {
		length := binary.BigEndian.Uint32(data[i : i+4])
		if length >= 2 && length <= 1<<20 && isSPSOrPPSNALType(data[i+4]) {
			return true
		}
	}
	return false
}
