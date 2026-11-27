package bgutils

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
)

// poTokenField is the one field a minted PO token holds: protobuf field 6,
// wire type 2 (length-delimited), so the tag byte is 6<<3|2.
const poTokenField = 0x32

// poTokenOverhead is what a minted token's field-6 payload holds beyond the
// integrity token and the content binding: 12 bytes, whatever the binding or
// the integrity token. The payload itself is opaque (it differs in every byte
// from one mint to the next, even for the same minter and binding), so these
// bytes cannot be read out of it. The 12 was measured on goja mints, whose
// integrity token length GenerateIT reports, over 11-byte video-ID bindings
// (2026-11-26 research). That it does not move with the binding was checked
// on 2026-11-27: three fresh sidecar minters each minted under bindings of
// 11, 42, 57, 66, 102 and 150 bytes, and every token was one field-6 record
// whose varint length (one byte up to 127, two above) was the binding plus
// the same 78 bytes (an integrity token of 66, the sidecar's usual figure).
const poTokenOverhead = 12

// ITBytes reports the byte length of the integrity token a minted PO token
// was made from, given the content binding the token was minted under (a
// video ID, visitor data or a DataSyncID: its UTF-8 bytes, which is how the
// minter receives it). It reads the token's own framing: one field-6 record
// whose varint length must cover exactly the rest of the token. The payload
// is that length less the binding and poTokenOverhead.
//
// ok is false for a token that is not one minted record (bad base64, another
// tag such as a cold-start token's field 4, a length that does not match, a
// payload too short for its binding). It never panics, and it returns only a
// length: nothing of the token or the binding is kept or logged.
func ITBytes(token, binding string) (n int, ok bool) {
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
		if err != nil {
			return 0, false
		}
	}
	if len(raw) < 2 || raw[0] != poTokenField {
		return 0, false
	}
	payload, k := binary.Uvarint(raw[1:])
	if k <= 0 || payload != uint64(len(raw)-1-k) { // k <= 0: no varint (too short, or past 64 bits)
		return 0, false
	}
	it := int(payload) - len(binding) - poTokenOverhead
	if it <= 0 {
		return 0, false
	}
	return it, true
}
