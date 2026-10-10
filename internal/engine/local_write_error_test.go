package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

// A failure to write the staged recording is marked ErrLocalWrite so callers
// can tell it from anything the stream did — the YouTube live loop read it as
// a possible stream end and finished the job after an hour of re-verifying a
// stream it could not write.
//
// Mutant: the open-output-file error without the sentinel.
func TestOpeningTheOutputFileFailsAsALocalWrite(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "http://127.0.0.1:1/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "gone", "video_stream"), // its directory does not exist
		StartSeq:   -1,
		IsHls:      true,
	})
	d.delays = fastDelays()
	err := d.Start(t.Context())
	if !errors.Is(err, ErrLocalWrite) {
		t.Errorf("Start = %v, want ErrLocalWrite", err)
	}
}
