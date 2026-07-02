package engine

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// mp4box builds a minimal MP4 box: 4-byte big-endian size + 4-byte type + body.
func mp4box(typ string, body []byte) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	b = append(b, []byte(typ)...)
	return append(b, body...)
}

func TestExtractMP4InitBoxes(t *testing.T) {
	ftyp := mp4box("ftyp", []byte("dashiso6avc1mp41"))
	moov := mp4box("moov", bytes.Repeat([]byte("M"), 200))
	init := append(append([]byte{}, ftyp...), moov...)

	tests := []struct {
		name string
		data []byte
		want []byte // nil = expect nil
	}{
		{
			// A real manifestless sq=0: ftyp+moov init, then the first media.
			name: "ftyp+moov before moof returns the init",
			data: append(append(append([]byte{}, init...), mp4box("moof", []byte("frag"))...), mp4box("mdat", []byte("media"))...),
			want: init,
		},
		{
			// Some segments interpose a sidx before the first moof; sidx is
			// media addressing, not init, so it bounds the init too.
			name: "ftyp+moov before sidx returns the init",
			data: append(append([]byte{}, init...), mp4box("sidx", []byte("segidx"))...),
			want: init,
		},
		{
			// A bare media fragment (no init) — cannot derive an init.
			name: "styp-led fragment has no init",
			data: append(mp4box("styp", []byte("msdh")), mp4box("moof", []byte("frag"))...),
			want: nil,
		},
		{
			name: "moof-led fragment has no init",
			data: mp4box("moof", []byte("frag")),
			want: nil,
		},
		{
			name: "too short returns nil",
			data: []byte{0, 0, 0, 4},
			want: nil,
		},
		{
			// A malformed 64-bit (size==1) box claiming a huge size must not
			// overflow int into a negative offset and panic.
			name: "malformed oversized 64-bit box returns nil (no panic)",
			data: append(
				mp4box("ftyp", []byte("dash")),
				append([]byte{0, 0, 0, 1, 'm', 'o', 'o', 'v'},
					[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}...)...,
			),
			want: nil,
		},
		{
			// init-only buffer (no media box at all) is entirely init.
			name: "ftyp+moov with no media box returns whole buffer",
			data: init,
			want: init,
		},
		{
			// A 32-bit box whose size overshoots the buffer must be rejected,
			// not walked past — walking past it would skip the real moof and
			// return the whole (media-contaminated / truncated) buffer as init.
			name: "32-bit box size overshooting buffer returns nil",
			data: func() []byte {
				ftyp := mp4box("ftyp", []byte("dash"))
				hdr := make([]byte, 8)
				binary.BigEndian.PutUint32(hdr, 500) // claims 500 bytes; far more than present
				copy(hdr[4:], "moov")
				oversized := append(hdr, bytes.Repeat([]byte("x"), 8)...)
				return append(append(ftyp, oversized...), mp4box("moof", []byte("f"))...)
			}(),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMP4InitBoxes(tt.data)
			if tt.want == nil {
				if got != nil {
					t.Errorf("extractMP4InitBoxes() = %d bytes, want nil", len(got))
				}
				return
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("extractMP4InitBoxes() = %d bytes, want %d bytes", len(got), len(tt.want))
			}
		})
	}
}
