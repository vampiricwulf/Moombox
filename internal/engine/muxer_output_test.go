package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

	removed, _, err := RemoveStaleMuxTemps([]string{tree, filepath.Join(tree, "missing")}, []string{other})
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

// otherRunToken returns a run token that is not this run's.
func otherRunToken() string {
	if muxRun == "0badc0de" {
		return "0badc0df"
	}
	return "0badc0de"
}

// holdAsAnotherWriter creates a temporary output named with another run's
// token and takes its lock through its own open, as a writer in another
// process does: a second open's lock conflicts with it in this process too,
// on Linux (flock is per open file) and on Windows (LockFileEx is per
// handle). The caller closes the file to let the writer go.
func holdAsAnotherWriter(t *testing.T, dir string) (string, *os.File) {
	t.Helper()
	p := filepath.Join(dir, "Title [live]"+muxTempMarker+otherRunToken()+"-1.mp4")
	f, err := createMuxTempFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockMuxTemp(f); err != nil {
		f.Close()
		t.Skipf("this filesystem cannot lock: %v", err)
	}
	if _, err := f.WriteString("partial"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return p, f
}

// TestRemoveStaleMuxTempsLeavesAFileItsWriterStillHolds is the sweep's half
// of the W30 review's two findings: another run's token is not a gone run. A
// second Moombox writing to the same output tree under its own data
// directory, and a child still shutting down after its launcher was killed
// (the launcher's single-instance lock gone with it), both write under a token
// that is not this run's, and the sweep took their live temporary files —
// FFmpeg's faststart pass reopened its output by name and the mux failed.
// The sweep now takes a file only when it can take its writer's lock: while
// the writer holds it the file stays and is reported live, and once the
// writer is gone — its handle closed, as its death closes it — the file goes.
//
// Mutants: removeStaleMuxTemp not trying the lock (the held file is
// removed); lockMuxTempFile answering nil, or an error that is not
// errMuxTempHeld, for a lock someone else holds (the held file is removed, or
// kept but not reported live); the sweep removing nothing once the lock is
// free (the gone writer's file stays).
func TestRemoveStaleMuxTempsLeavesAFileItsWriterStillHolds(t *testing.T) {
	dir := t.TempDir()
	p, held := holdAsAnotherWriter(t, dir)

	removed, live, err := RemoveStaleMuxTemps(nil, []string{dir})
	if err != nil {
		held.Close()
		t.Fatalf("RemoveStaleMuxTemps: %v", err)
	}
	if len(removed) != 0 || len(live) != 1 || live[0] != p {
		t.Errorf("with its writer alive the sweep removed %v and reported %v live, want nothing removed and %s live", removed, live, p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the sweep removed a temporary file its writer still holds: %v", err)
	}

	held.Close()
	removed, live, err = RemoveStaleMuxTemps(nil, []string{dir})
	if err != nil {
		t.Fatalf("RemoveStaleMuxTemps: %v", err)
	}
	if len(removed) != 1 || len(live) != 0 {
		t.Errorf("with its writer gone the sweep removed %v and reported %v live, want the file removed", removed, live)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("after the writer let go the sweep left %v", names)
	}
}

// TestWriteOutputHoldsItsTemporaryFileUntilTheRename: what the sweep tests is
// a lock writeOutput holds from before FFmpeg opens its temporary file until
// the file is renamed onto the final name or removed. Inside the write the
// file exists already, created by writeOutput (FFmpeg writes over it with
// -y), and another open's lock attempt finds it held; once writeOutput has
// returned, the archive is free.
//
// Mutants: writeOutput closing its handle before write runs, or createMuxTemp
// returning without the lock (the sweep finds the file free mid-write);
// writeOutput never closing it (the archive's lock is still held afterwards —
// a handle leaked per mux).
func TestWriteOutputHoldsItsTemporaryFileUntilTheRename(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	dir := t.TempDir()
	final := filepath.Join(dir, "Title [abc].mp4")
	var verdict muxTempVerdict = -1
	err := m.writeOutput(t.Context(), final, nil, func(tmp string) error {
		if _, err := os.Stat(tmp); err != nil {
			t.Errorf("the temporary file does not exist when the writer is handed it: %v", err)
		}
		verdict = removeStaleMuxTemp(tmp)
		return os.WriteFile(tmp, []byte("whole"), 0o644)
	})
	if err != nil {
		t.Fatalf("writeOutput: %v", err)
	}
	if verdict != muxTempLive {
		t.Errorf("mid-write the sweep's verdict on the temporary file was %d, want live (%d): the writer does not hold its lock", verdict, muxTempLive)
	}
	f, err := openMuxTempForSweep(final)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := lockMuxTemp(f); err != nil {
		t.Errorf("after writeOutput returned the archive's lock is still held (%v): its handle was never closed", err)
	}
}

// TestCreateMuxTempNeverWritesANameItDoesNotHold covers createMuxTemp's two
// retries. A leftover already under the next name (an earlier run whose
// token matched this run's) is left as it is and a new name drawn; and a file
// that a sweep removed between the create and the lock — the moment its
// writer had not yet taken it — is let go, so FFmpeg is never handed a name
// the lock is not on.
//
// Mutants: createMuxTempFile opening a name that exists (the leftover is
// handed out and written over); dropping the sameFileAsPath check (the name
// the sweep removed is handed out).
func TestCreateMuxTempNeverWritesANameItDoesNotHold(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	dir := t.TempDir()
	final := filepath.Join(dir, "Title [abc].mp4")

	// The next sequence number's name, taken by a leftover.
	next := filepath.Join(dir, "Title [abc]"+muxTempMarker+muxRun+"-"+strconv.FormatUint(muxTempSeq.Load()+1, 10)+".mp4")
	if err := os.WriteFile(next, []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp, f, err := m.createMuxTemp(final)
	if err != nil {
		t.Fatalf("createMuxTemp beside a leftover: %v", err)
	}
	f.Close()
	if tmp == next {
		t.Errorf("createMuxTemp handed out %s, which a leftover already holds", next)
	}
	if b, _ := os.ReadFile(next); string(b) != "leftover" {
		t.Errorf("the leftover under %s reads %q, want it untouched", next, b)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}

	// A sweep removes the first file before the writer's lock lands.
	real := lockMuxTemp
	t.Cleanup(func() { lockMuxTemp = real })
	var swept []string
	lockMuxTemp = func(f *os.File) error {
		if len(swept) == 0 {
			swept = append(swept, f.Name())
			if err := os.Remove(f.Name()); err != nil {
				t.Fatal(err)
			}
		}
		return real(f)
	}
	tmp, f, err = m.createMuxTemp(final)
	if err != nil {
		t.Fatalf("createMuxTemp after a sweep took its first file: %v", err)
	}
	defer f.Close()
	lockMuxTemp = real
	if len(swept) != 1 || tmp == swept[0] {
		t.Fatalf("createMuxTemp handed out %s, the name the sweep removed (%v)", tmp, swept)
	}
	if !sameFileAsPath(f, tmp) {
		t.Errorf("createMuxTemp handed out %s, which does not name the file it holds", tmp)
	}
	if v := removeStaleMuxTemp(tmp); v != muxTempLive {
		t.Errorf("the sweep's verdict on the file createMuxTemp holds is %d, want live", v)
	}
}

// TestRemoveStaleMuxTempsWithoutLocksTakesOnlyAFileLongIdle: where the lock
// cannot be tried at all (a filesystem that answers flock with an error), a
// live writer cannot be told from a gone one by it. The writer writes
// unlocked rather than failing the mux, and the sweep takes such a file only
// once it has sat untouched for muxTempIdleAge — a live FFmpeg rewrites its
// output as it goes.
//
// Mutants: dropping the age check (the fresh file is removed); treating the
// error as a held lock (the day-old file is never removed); createMuxTemp
// failing when the lock cannot be taken (writeOutput errors).
func TestRemoveStaleMuxTempsWithoutLocksTakesOnlyAFileLongIdle(t *testing.T) {
	real := lockMuxTemp
	t.Cleanup(func() { lockMuxTemp = real })
	lockMuxTemp = func(*os.File) error { return errors.New("locks are not supported here") }

	dir := t.TempDir()
	fresh := filepath.Join(dir, "Title [a]"+muxTempMarker+otherRunToken()+"-1.mp4")
	idle := filepath.Join(dir, "Title [b]"+muxTempMarker+otherRunToken()+"-2.mp4")
	for _, p := range []string{fresh, idle} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-muxTempIdleAge - time.Hour)
	if err := os.Chtimes(idle, old, old); err != nil {
		t.Fatal(err)
	}
	removed, _, err := RemoveStaleMuxTemps(nil, []string{dir})
	if err != nil {
		t.Fatalf("RemoveStaleMuxTemps: %v", err)
	}
	if len(removed) != 1 || removed[0] != idle {
		t.Errorf("removed %v, want only the file idle past muxTempIdleAge", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the sweep removed a fresh temporary file it could not lock: %v", err)
	}

	final := filepath.Join(dir, "Title [c].mp4")
	if err := NewMuxer("ffmpeg", &testLogger{}).writeOutput(t.Context(), final, nil, func(tmp string) error {
		return os.WriteFile(tmp, []byte("whole"), 0o644)
	}); err != nil {
		t.Fatalf("writeOutput where the lock cannot be taken: %v", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "whole" {
		t.Errorf("the final name holds %q, want the finished file", b)
	}
}
