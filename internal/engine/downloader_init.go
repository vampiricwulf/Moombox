package engine

import "encoding/binary"

// extractMP4InitBoxes returns the leading ftyp+moov initialization boxes from a
// fragmented-MP4 segment — everything before the first media box (moof / mdat /
// styp / sidx). Returns nil when the data is too short, malformed, or has no
// ftyp box (i.e. it is a bare media fragment, not an init-carrying segment).
//
// Manifest-free DASH parts carry their init only inline at sq=0 (no separate
// init URL), so a part that force-starts at sq>0 must derive its ftyp+moov from
// a one-shot sq=0 fetch. Extracting just the init — rather than prepending the
// whole sq=0 segment — keeps segment 0's media from leaking into a later part.
func extractMP4InitBoxes(data []byte) []byte {
	off := 0
	sawFtyp := false
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		boxType := string(data[off+4 : off+8])
		switch boxType {
		case "moof", "mdat", "styp", "sidx":
			// First media box — the init is everything before it.
			if sawFtyp && off > 0 {
				return data[:off]
			}
			return nil
		case "ftyp":
			sawFtyp = true
		}
		switch {
		case size == 1:
			// 64-bit extended size in the 8 bytes following the box header.
			if off+16 > len(data) {
				return nil
			}
			size64 := binary.BigEndian.Uint64(data[off+8 : off+16])
			if size64 < 16 {
				return nil
			}
			off += int(size64)
		case size < 8:
			// size 0 means "to EOF"; anything < 8 is a malformed header.
			return nil
		default:
			off += size
		}
	}
	// No media box encountered — if an ftyp was seen the whole buffer is init.
	if sawFtyp {
		return data
	}
	return nil
}
