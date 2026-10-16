package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// dirNames lists dir's entries by name.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestWriteOutputRenamesOnlyAWholeVerifiedFile pins writeOutput's contract,
// the half of W28-A01 that makes a crash survivable: the writer is handed a
// temporary name of this run's own beside the final one, the final name
// appears only once the write returned nil and every check passed, and every
// other outcome leaves the directory as it was — no final file, no temporary
// one, and an existing final file untouched.
//
// Mutants: writeOutput handing write `final` instead of tmp (the writer is
// given the final name); dropping the check loop (the rejected file is
// renamed into place); dropping the os.Remove(tmp) on failure (the
// temporary file stays); renaming even when write failed (a final appears
// after the failed write).
func TestWriteOutputRenamesOnlyAWholeVerifiedFile(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	errWrite := errors.New("Conversion failed!")
	errShort := errors.New("mux produced 10s from a 60s input")

	cases := []struct {
		name    string
		before  string // the final file's content beforehand; "" = absent
		write   error
		check   error
		wantErr error
		want    string // the final file's content afterwards; "" = absent
	}{
		{name: "success", write: nil, want: "whole"},
		{name: "success replaces a final that exists", before: "old", want: "whole"},
		{name: "FFmpeg fails", write: errWrite, wantErr: errWrite},
		{name: "FFmpeg fails beside a final that exists", before: "old", write: errWrite, wantErr: errWrite, want: "old"},
		{name: "a check rejects the file", check: errShort, wantErr: errShort},
		{name: "a check rejects beside a final that exists", before: "old", check: errShort, wantErr: errShort, want: "old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			final := filepath.Join(dir, "Title [abc].mp4")
			if tc.before != "" {
				if err := os.WriteFile(final, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var handed, checked string
			err := m.writeOutput(t.Context(), final, []OutputCheck{func(_ context.Context, p string) error {
				checked = p
				if b, _ := os.ReadFile(p); string(b) != "whole" {
					t.Errorf("the check read %q from %s, want the finished write", b, p)
				}
				return tc.check
			}}, func(tmp string) error {
				handed = tmp
				if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
					return err
				}
				if tc.write != nil {
					return tc.write
				}
				return os.WriteFile(tmp, []byte("whole"), 0o644)
			})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("writeOutput = %v, want %v", err, tc.wantErr)
			}

			if handed == final || filepath.Dir(handed) != dir || !IsMuxTempPath(handed) || muxTempRunOf(filepath.Base(handed)) != muxRun {
				t.Errorf("the writer was handed %q, want a temporary name of this run's beside %q", handed, final)
			}
			if tc.write == nil && checked != handed {
				t.Errorf("the check read %q, want the temporary file %q", checked, handed)
			}
			got, rerr := os.ReadFile(final)
			switch {
			case tc.want == "" && rerr == nil:
				t.Errorf("the final name holds %q after a run that failed; nothing should carry it", got)
			case tc.want != "" && string(got) != tc.want:
				t.Errorf("the final name holds %q (err %v), want %q", got, rerr, tc.want)
			}
			wantNames := 0
			if tc.want != "" {
				wantNames = 1
			}
			if names := dirNames(t, dir); len(names) != wantNames {
				t.Errorf("the directory holds %v, want the final file alone or nothing", names)
			}
		})
	}
}

// TestWriteOutputRemovesTheTemporaryFileOfACancelledRun: a cancel kills
// FFmpeg before its trailer, and what it leaves — an MP4 with no moov — used
// to be kept under the final name "for a salvage" (cleanupFailedMux). Nothing
// plays it, the staging it came from is kept for the next mux, and under the
// final name it read as the archive. Now the cancelled run leaves nothing.
//
// Mutant: keeping the temporary file when ctx is cancelled (the
// cleanupFailedMux rule) — the temporary file stays.
func TestWriteOutputRemovesTheTemporaryFileOfACancelledRun(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	err := m.writeOutput(ctx, filepath.Join(dir, "Title.mp4"), nil, func(tmp string) error {
		if err := os.WriteFile(tmp, []byte("moov-less"), 0o644); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeOutput = %v, want the cancel", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("a cancelled run left %v", names)
	}
}

// TestMuxTempPathStaysBesideItsFinalNameAndUnderTheNameLimit: the temporary
// name is in the final file's directory (the move is a rename on one
// volume), keeps the extension last (FFmpeg picks the format from it), starts
// with the final stem (the finalize's stem claim covers it), is new on every
// call, and never passes muxTempNameMax — a stem at the template's budget
// plus a set-aside sibling's suffix plus the marker would, and the stem is
// cut on a rune boundary instead.
//
// Mutants: dropping the cut (the long name passes 255 bytes); cutting
// without the rune-boundary walk (the name is not valid UTF-8); a fixed
// sequence number (two calls return one name).
func TestMuxTempPathStaysBesideItsFinalNameAndUnderTheNameLimit(t *testing.T) {
	dir := filepath.Join("out", "chan")
	final := filepath.Join(dir, "Title [abc].mp4")
	a, b := muxTempPath(final), muxTempPath(final)
	if a == b {
		t.Errorf("two temporary names for one final name are both %q", a)
	}
	for _, p := range []string{a, b} {
		base := filepath.Base(p)
		if filepath.Dir(p) != dir || filepath.Ext(p) != ".mp4" || !strings.HasPrefix(base, "Title [abc]"+muxTempMarker) {
			t.Errorf("muxTempPath(%q) = %q, want <stem>%s<run>-<n>.mp4 beside it", final, p, muxTempMarker)
		}
		if muxTempRunOf(base) != muxRun {
			t.Errorf("muxTempPath(%q) = %q does not carry this run's token %q", final, p, muxRun)
		}
	}

	// 70 three-byte runes (210 bytes, the stem budget) and a set-aside
	// sibling's ".restart-<ts>-99", cut inside the ASCII suffix; and names of
	// three-byte runes alone, two extensions a byte apart, so that whatever
	// length the sequence number gives the marker one of the cuts lands
	// inside a rune.
	for _, final := range []string{
		strings.Repeat("日", 70) + ".restart-1700000000-99.mp4",
		strings.Repeat("日", 82) + ".mp4",
		strings.Repeat("日", 82) + ".ts",
	} {
		checkLongTempName(t, muxTempPath(filepath.Join(dir, final)), filepath.Ext(final))
	}
}

// checkLongTempName is the length half of
// TestMuxTempPathStaysBesideItsFinalNameAndUnderTheNameLimit for one name.
func checkLongTempName(t *testing.T, p, ext string) {
	t.Helper()
	base := filepath.Base(p)
	if len(base) > muxTempNameMax {
		t.Errorf("the temporary name is %d bytes, over the %d a file name may have", len(base), muxTempNameMax)
	}
	if !utf8.ValidString(base) {
		t.Errorf("the cut temporary name %q is not valid UTF-8", base)
	}
	if filepath.Ext(base) != ext || muxTempRunOf(base) != muxRun || !strings.HasPrefix(base, strings.Repeat("日", 70)) {
		t.Errorf("the cut temporary name %q lost its extension, its token or the archive's stem", base)
	}
}

// TestRemoveStaleMuxTempsTakesOnlyAGoneRunsTemporaryFiles is the boot sweep:
// every temporary output another run left — at the top of an output tree, in
// a channel directory below it, in a directory named on its own — goes, and
// nothing else does: not this run's (a mux of this run may be writing it), not
// an archive, not a name that only resembles the marker. A tree that does not
// exist is no error.
//
// Mutants: dropping `run != muxRun` (this run's temporary file is removed);
// reading the tree one level deep (the channel directory's leftover stays);
// skipping dirs (the named directory's leftover stays).
func TestRemoveStaleMuxTempsTakesOnlyAGoneRunsTemporaryFiles(t *testing.T) {
	tree, other := t.TempDir(), t.TempDir()
	gone := "0badc0de"
	if gone == muxRun {
		gone = "0badc0df"
	}
	write := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := []string{
		write(filepath.Join(tree, "Title [a]"+muxTempMarker+gone+"-1.mp4")),
		write(filepath.Join(tree, "chan", "Title [b] - part2"+muxTempMarker+gone+"-7.mp4")),
		write(filepath.Join(other, "Title [c]"+muxTempMarker+gone+"-3.mp4")),
	}
	kept := []string{
		write(filepath.Join(tree, "chan", "Title [d]"+muxTempMarker+muxRun+"-2.mp4")),
		write(filepath.Join(tree, "chan", "Title [d].mp4")),
		write(filepath.Join(tree, "Title [e]"+muxTempMarker+"notahex1-1.mp4")),
		write(filepath.Join(tree, "Title [f].partial.mp4")),
	}

	removed, err := RemoveStaleMuxTemps([]string{tree, filepath.Join(tree, "missing")}, []string{other})
	if err != nil {
		t.Fatalf("RemoveStaleMuxTemps: %v", err)
	}
	if len(removed) != len(stale) {
		t.Errorf("removed %v, want the %d stale temporary files", removed, len(stale))
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the stale temporary file %s survived the sweep", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the sweep removed %s, which is not a gone run's temporary file", p)
		}
	}
}
