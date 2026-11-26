package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renditionURL is a live manifest-free video URL of itag on one video.
func renditionURL(itag string) string {
	return "http://" + deadAddress + "/videoplayback?id=abcdefghijk.1&itag=" + itag
}

// TestStartRefusesToAppendAnotherRendition pins owner ruling R4-1 in the
// engine: a restarted live capture whose chosen rendition (itag 298) is not
// the one its staged file was recorded in (itag 299) must not append to it —
// on the orchestrator's forced seq or on the DB's last seq — whether the
// sidecar says so (it was saved) or only the rendition marker does (the
// restart came before the first sidecar save, or the sidecar is gone). Start
// returns ErrRenditionChanged with the file as it was and CurrentSeq() at the
// next segment, which is where the live loop starts the new part. The same
// rendition, a file staged with no marker by an older build, and a downloader
// whose caller cannot split all append exactly as before.
//
// Mutants: drop the check (every "refuse" row appends); compare only the
// sidecar's verdict (the marker rows append); leave the DB seq at the last
// written one (CurrentSeq is 42 on the DB rows, the new part re-fetches it);
// write the marker only on a resume (the fresh-file test finds none).
func TestStartRefusesToAppendAnotherRendition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sidecar string // the itag the resume sidecar was saved for; "" = none
		marker  string // the rendition marker's content; "" = none
		forced  bool
		optOut  bool
		refuse  bool
		nextSeq int
	}{
		{name: "sidecar names another itag, DB seq", sidecar: "299", refuse: true, nextSeq: 43},
		{name: "sidecar names another itag, forced seq", sidecar: "299", forced: true, refuse: true, nextSeq: 42},
		{name: "no sidecar, marker names another itag, DB seq", marker: "abcdefghijk/299", refuse: true, nextSeq: 43},
		{name: "no sidecar, marker names another itag, forced seq", marker: "abcdefghijk/299", forced: true, refuse: true, nextSeq: 42},
		{name: "no sidecar, marker names this itag", marker: "abcdefghijk/298"},
		{name: "no sidecar, no marker (an older build's staging)"},
		{name: "a caller that cannot split", marker: "abcdefghijk/299", optOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stagedFile(t, 4096)
			if tc.sidecar != "" {
				state, _ := json.Marshal(ResumeState{LastSeq: 41, BytesWritten: 4096, Timestamp: time.Now().Unix(), BaseURL: renditionURL(tc.sidecar)})
				if err := os.WriteFile(path+resumeFileSuffix, state, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker != "" {
				if err := os.WriteFile(path+renditionFileSuffix, []byte(tc.marker+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:       renditionURL("298"),
				OutputFile:    path,
				StartSeq:      42,
				ForceStartSeq: tc.forced,
				MaxRetries:    1,
			})
			d.SplitOnRenditionChange = !tc.optOut

			err := startAgainstDeadAddress(t, d)
			if got := errors.Is(err, ErrRenditionChanged); got != tc.refuse {
				t.Fatalf("Start = %v; refused as another rendition %v, want %v", err, got, tc.refuse)
			}
			size := sizeOf(t, path)
			if tc.refuse {
				if size != 4096 {
					t.Errorf("staged file is %d bytes after the refusal, want the 4096 it held", size)
				}
				if got := d.CurrentSeq(); got != tc.nextSeq {
					t.Errorf("CurrentSeq() = %d after the refusal, want the next segment %d", got, tc.nextSeq)
				}
				// The live loop may run the refused downloader again (a
				// look-refresh that waits): the refusal names the same segment.
				d.Cancel()
				if err := startAgainstDeadAddress(t, d); !errors.Is(err, ErrRenditionChanged) {
					t.Errorf("second Start = %v, want the same refusal", err)
				}
				if got := d.CurrentSeq(); got != tc.nextSeq {
					t.Errorf("CurrentSeq() = %d after a second refusal, want the same next segment %d", got, tc.nextSeq)
				}
				if tc.sidecar != "" {
					if _, err := os.Stat(path + resumeFileSuffix); err != nil {
						t.Errorf("the refusal removed the sidecar: %v", err)
					}
				}
				return
			}
			if size < 4096 {
				t.Errorf("staged file is %d bytes, want the 4096 it held kept and appended to", size)
			}
		})
	}
}

// TestStartMarksAFreshFileWithItsRendition: a SplitOnRenditionChange
// downloader that opens its file fresh records the rendition beside it before
// any media — the marker a restart with no sidecar compares against — and
// replaces one an earlier file left in the same directory.
func TestStartMarksAFreshFileWithItsRendition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video_stream")
	if err := os.WriteFile(path+renditionFileSuffix, []byte("abcdefghijk/299\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: renditionURL("298"), OutputFile: path, MaxRetries: 1})
	d.SplitOnRenditionChange = true
	_ = startAgainstDeadAddress(t, d)

	data, err := os.ReadFile(path + renditionFileSuffix)
	if err != nil {
		t.Fatalf("no rendition marker beside a fresh file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "abcdefghijk/298" {
		t.Errorf("rendition marker = %q, want abcdefghijk/298", got)
	}
}
