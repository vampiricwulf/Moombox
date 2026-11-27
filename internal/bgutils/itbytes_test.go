package bgutils

import (
	"encoding/base64"
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"testing"
)

// mintShaped builds a token framed the way a minted PO token is (one field-6
// record whose payload is the integrity token, the binding and 12 more
// bytes, all opaque), with random payload bytes: no real token is used.
func mintShaped(r *rand.Rand, itBytes int, binding string) []byte {
	payload := make([]byte, itBytes+len(binding)+12) // 12: the measured overhead, not the constant under test
	for i := range payload {
		payload[i] = byte(r.IntN(256))
	}
	out := []byte{0x32}
	out = binary.AppendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

// coldStartShaped builds a token as bgutils-js createColdStartToken does: a
// field-4 record (tag 34) with a one-byte length.
func coldStartShaped(binding string) []byte {
	header := []byte{7, 9, 0, 1, 0x65, 0x4f, 0x2a, 0x11}
	payload := append(header, binding...)
	return append([]byte{34, byte(len(payload))}, payload...)
}

func TestITBytes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	videoID := "dQw4w9WgXcQ"
	visitorData := "CgtTWVJ6cVhkVjhKNCi2qZjBBjIKCgJVUxIEGgAgOQ%3D%3D"
	dataSyncID := "112233445566778899001||"
	longVisitor := strings.Repeat("Cgt", 40) // payload past 127 bytes: a 2-byte length varint

	padded := func(b []byte) string { return base64.URLEncoding.EncodeToString(b) }
	unpadded := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

	cases := []struct {
		name    string
		token   string
		binding string
		want    int
		ok      bool
	}{
		{"video id, IT 66", padded(mintShaped(r, 66, videoID)), videoID, 66, true},
		{"video id, IT 68, unpadded", unpadded(mintShaped(r, 68, videoID)), videoID, 68, true},
		{"visitor data, IT 67", padded(mintShaped(r, 67, visitorData)), visitorData, 67, true},
		{"DataSyncID, IT 68", padded(mintShaped(r, 68, dataSyncID)), dataSyncID, 68, true},
		{"long visitor data, 2-byte length", padded(mintShaped(r, 66, longVisitor)), longVisitor, 66, true},
		{"goja-sized IT 75", unpadded(mintShaped(r, 75, videoID)), videoID, 75, true},
		{"cold-start token", padded(coldStartShaped(videoID)), videoID, 0, false},
		{"another field's record", padded(append([]byte{0x0a}, mintShaped(r, 66, videoID)[1:]...)), videoID, 0, false},
		{"truncated", padded(mintShaped(r, 66, videoID)[:60]), videoID, 0, false},
		{"trailing bytes", padded(append(mintShaped(r, 66, videoID), 0x00, 0x01)), videoID, 0, false},
		{"payload too short for its binding", padded(mintShaped(r, 0, videoID)), videoID, 0, false},
		{"not base64", "this is not*a token", videoID, 0, false},
		{"empty", "", videoID, 0, false},
		{"tag only", padded([]byte{0x32}), videoID, 0, false},
		{"unterminated varint", padded([]byte{0x32, 0xff, 0xff}), videoID, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ITBytes(tc.token, tc.binding)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ITBytes = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
