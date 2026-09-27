package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// assertNoTempSurvives fails if any *.tmp entry is left in dir. The twin of
// internal/utils/chatfile_test.go's helper of the same name — copied rather
// than shared because a _test.go symbol is invisible outside its package, and
// the alternatives (a testing-dependent export from internal/utils, or a
// utilstest package for nine lines) both cost more than the duplication. Same
// name on both sides so they read as one idea.
//
// It replaces the fixed-name `os.Stat(path + ".tmp")` check the adopting
// writer used to be pinned by: once the temp name comes from os.CreateTemp, a
// stat of one hard-coded name proves nothing, while the glob still proves the
// real property — the directory is left with the target and nothing else.
func assertNoTempSurvives(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("glob temps in %s: %v", dir, err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

// TestWriteDescriptionAtomicUsesAUniqueTempName pins the adopt: the
// description writer now goes through utils.WriteFileAtomic, whose temp name
// comes from os.CreateTemp, so a writer already mid-write on the FIXED
// `finalPath + ".tmp"` name can no longer be clobbered. Two jobs whose
// resolved filename base collides in one output directory — a re-download, or
// a restart muxed beside its predecessor — aim at one .description and shared
// that single temp.
//
// Checked directly rather than by racing two writers: the directory is
// pre-seeded with the fixed name the old writer would have used, and it must
// come back untouched.
//
// Mutant this kills: restoring the local `tmpPath := finalPath + ".tmp"`
// writer (the pre-adopt code) — it truncates the seeded file and renames it
// onto the target, so the ReadFile below fails outright.
func TestWriteDescriptionAtomicUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")
	fixed := path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}
	if err := writeDescriptionAtomic(path, "the description body"); err != nil {
		t.Fatalf("writeDescriptionAtomic: %v", err)
	}

	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}

// descriptionGoldenBody is chosen to catch any transform an adopt could
// smuggle in: a CRLF, a lone LF, non-ASCII, and a trailing blank line.
const descriptionGoldenBody = "line one\r\nline two\nhttps://example.test/ünïcode ✓\n\n"

// TestWriteDescriptionAtomicWritesTheBodyVerbatim is a REGRESSION pin, not a
// red-first test: green before AND after the adopt, which is exactly its job.
// There is no encoder anywhere on this path — the writer takes a string and
// puts those bytes on disk — and orphan adoption (orphans.go, `ext ==
// ".description"`) reads files an earlier build wrote, so a byte of drift
// would be invisible until someone diffed an archive.
//
// Mutants this kills: appending a trailing newline; writing
// strings.TrimSpace(body); routing the body through a fmt.Fprintf that
// reinterprets a % in a description.
func TestWriteDescriptionAtomicWritesTheBodyVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")

	if err := writeDescriptionAtomic(path, descriptionGoldenBody); err != nil {
		t.Fatalf("writeDescriptionAtomic: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != descriptionGoldenBody {
		t.Errorf("the body was transformed.\n got: %q\nwant: %q", string(raw), descriptionGoldenBody)
	}

	// No temp of ANY name may survive a successful write. The fixed-name stat
	// the pre-adopt writer could have been pinned by is retired because the
	// name is now unpredictable; the glob is its honest replacement on this
	// SUCCESS path.
	assertNoTempSurvives(t, dir)
}

// TestWriteDescriptionAtomicRenameFailureLeavesNoTemp drives the one failure
// path reachable from this package. utils.WriteFileAtomic's syncFile seam is
// package-private to internal/utils, so the RENAME is what gets injected here:
// a directory standing where the .description belongs. os.Rename refuses to
// replace a directory with a file on every platform Moombox ships on, and
// whatever stood at the target path is still there afterwards.
//
// Green before AND after the adopt — the old writer hand-removed its temp on
// this branch too — so this is a regression pin rather than a red-first test.
// What it guards is THIS package re-diverging: an edit that open-codes
// os.CreateTemp + write + rename here and forgets the cleanup fails it, and so
// does deleting utils.WriteFileAtomic's single deferred os.Remove.
//
// Windows note, measured: MoveFileEx reports ERROR_ACCESS_DENIED for a
// directory in the target's place, which utils.transientReplaceError
// classifies as transient, so ReplaceFile spends its whole retry ladder
// (10+20+40+80+160+320+640 ms) before returning the error. This one test
// therefore costs about 1.3 s on Windows and nothing elsewhere. Expected, not
// a hang. (utils/replacefile_windows.go's own comment calls a directory in the
// way "permanent and must not be retried" — right about the intent, wrong
// about the Windows error code. Recorded as an observation; internal/utils is
// outside this arc's file set.)
func TestWriteDescriptionAtomicRenameFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")
	marker := filepath.Join(path, "occupied")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed the blocking directory: %v", err)
	}
	if err := os.WriteFile(marker, []byte("still here"), 0o644); err != nil {
		t.Fatalf("seed the marker: %v", err)
	}

	if err := writeDescriptionAtomic(path, "a body that cannot land"); err == nil {
		t.Fatal("writeDescriptionAtomic: want the rename to fail against a directory, got nil")
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("what stood at the target path is gone: %v", err)
	}
	if string(got) != "still here" {
		t.Errorf("the previous target was disturbed: %q", string(got))
	}
	assertNoTempSurvives(t, dir)
}

// statByHandle returns a FileInfo whose identity is resolved NOW. os.Stat on
// Windows defers the file-ID lookup to os.SameFile and resolves it by PATH,
// so a pre-write os.Stat compared after the write would describe the
// post-write file and the assertion below could never fail. (*os.File).Stat
// fills the identity from the open handle.
func statByHandle(t *testing.T, path string) os.FileInfo {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// TestWriteDescriptionAtomicReplacesTheTargetRatherThanRewritingIt pins the
// property the three tests above take for granted: the description lands as
// a NEW file renamed over the old one, never as an in-place rewrite of the
// old one. A rewrite truncates first, so a crash between the truncate and the
// write leaves the torn file the doc comment promises is impossible — and a
// direct os.WriteFile(finalPath, …) passes every other test in this file (no
// fixed temp to clobber, nothing to leak, the body verbatim). The file
// identity is what tells the two apart: the NTFS file index / the inode
// changes across a rename and survives a rewrite.
//
// Green before AND after the adopt (the old tmp+rename writer replaced too),
// so a regression pin. Mutant this kills: `return os.WriteFile(finalPath,
// []byte(body), 0o644)`.
func TestWriteDescriptionAtomicReplacesTheTargetRatherThanRewritingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")
	if err := os.WriteFile(path, []byte("the previous description"), 0o644); err != nil {
		t.Fatalf("seed the previous target: %v", err)
	}
	before := statByHandle(t, path)

	if err := writeDescriptionAtomic(path, "the new description"); err != nil {
		t.Fatalf("writeDescriptionAtomic: %v", err)
	}

	after := statByHandle(t, path)
	if os.SameFile(before, after) {
		t.Error("the target was rewritten in place: the same file identity survived the write, " +
			"so the body went through a truncate-then-write rather than a temp renamed over the target")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "the new description" {
		t.Errorf("content after the write = %q, want the new body", got)
	}
	assertNoTempSurvives(t, dir)
}

// TestFinishedImageIsDroppedForTwitch pins the §0 ruling. A Twitch preview URL
// 404s the moment the broadcast ends, and "Download Finished" is sent after it
// did, so the full-width image on every Twitch finished embed was permanently
// broken. YouTube thumbnails outlive the stream and keep theirs.
//
// THE MUTANT: reverting either call site to jobCtx.Job.ThumbnailURL.
func TestFinishedImageIsDroppedForTwitch(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     string
	}{
		{"youtube", "https://i.ytimg.com/vi/x/maxresdefault.jpg"},
		{"twitch", ""},
		{"", "https://i.ytimg.com/vi/x/maxresdefault.jpg"},
	} {
		job := &database.Job{
			Platform:     tc.platform,
			ThumbnailURL: "https://i.ytimg.com/vi/x/maxresdefault.jpg",
		}
		if got := finishedImage(job); got != tc.want {
			t.Errorf("finishedImage(platform=%q) = %q, want %q", tc.platform, got, tc.want)
		}
	}
	if got := finishedImage(nil); got != "" {
		t.Errorf("finishedImage(nil) = %q, want \"\"", got)
	}
}

// TestDescriptionExcerptCutsOnARuneBoundary is the fix for the byte slice at
// the Description excerpt. A Japanese description — the norm for this project's
// archives — was cut mid-rune, and encoding/json then replaced the broken tail
// with U+FFFD.
//
// WHAT THIS PINS: the helper COMPOSITION and its 300-rune budget, not the call
// site — it calls the helpers directly and never reaches
// sendFinishedNotification, so reverting that line to desc[:descMaxLen-3] would
// not fail here. The call site is pinned separately, by the "Description" field
// a notificationtest.Recorder reads off a finished send in Task 6's fixture;
// ClampRunes' own boundary behaviour is pinned exhaustively in
// internal/notifications/limits_test.go.
func TestDescriptionExcerptCutsOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("あ", 500)
	got := notifications.ClampRunes(notifications.EscapeMarkdown(long), 300)
	if !utf8.ValidString(got) {
		t.Fatalf("the excerpt is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 300 {
		t.Errorf("excerpt = %d runes, want 300", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt does not end with the clamp marker: %q", got[len(got)-12:])
	}
}
