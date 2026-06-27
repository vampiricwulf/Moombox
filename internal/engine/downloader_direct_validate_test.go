package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// box builds a minimal MP4 box: 4-byte big-endian size, 4-byte type, then body.
func box(boxType string, body []byte) []byte {
	size := 8 + len(body)
	b := []byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
	b = append(b, []byte(boxType)...)
	return append(b, body...)
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func TestValidateDownloadedMP4(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{
			// A complete MP4/M4A always begins with an ftyp box.
			name:    "ftyp-led complete file passes",
			data:    append(box("ftyp", []byte("isom\x00\x00\x02\x00")), box("moov", []byte("metadata-here"))...),
			wantErr: false,
		},
		{
			// The post-live failure: a bare media fragment (no ftyp/moov init).
			name:    "moof-led fragment is rejected",
			data:    append(box("styp", []byte("msdh")), box("moof", []byte("fragment-data"))...),
			wantErr: true,
		},
		{
			name:    "sidx-led fragment is rejected",
			data:    box("sidx", []byte("segment-index-data-padding")),
			wantErr: true,
		},
		{
			// A 403/HTML error body saved as .mp4.
			name:    "html error body is rejected",
			data:    []byte("<!DOCTYPE html><html><body>403 Forbidden</body></html>"),
			wantErr: true,
		},
		{
			name:    "tiny file is rejected",
			data:    []byte("abc"),
			wantErr: true,
		},
		{
			name:    "empty file is rejected",
			data:    []byte{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, "video.mp4", tt.data)
			err := validateDownloadedMP4(path)
			if tt.wantErr && err == nil {
				t.Errorf("validateDownloadedMP4() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateDownloadedMP4() = %v, want nil", err)
			}
		})
	}
}

func TestValidateDownloadedMP4MissingFile(t *testing.T) {
	if err := validateDownloadedMP4(filepath.Join(t.TempDir(), "nope.mp4")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
