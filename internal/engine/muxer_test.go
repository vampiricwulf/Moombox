package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestParseFFmpegTime(t *testing.T) {
	tests := []struct {
		name string
		line string
		want float64
	}{
		{"HH:MM:SS format", "frame= 100 fps=25 time=00:01:30.50 bitrate=1000kbits/s", 90.5},
		{"MM:SS format", "time=01:30.00 speed=1x", 90.0},
		{"seconds only", "time=45.25 speed=1x", 45.25},
		{"no time field", "frame= 100 fps=25 bitrate=1000kbits/s", 0},
		{"time=N/A", "time=N/A speed=N/A", 0},
		{"empty time", "time= speed=1x", 0},
		{"zero time", "time=00:00:00.00 speed=1x", 0},
		{"hours", "time=02:30:15.75 speed=1x", 2*3600 + 30*60 + 15.75},
		{"at end of line", "frame= 100 time=10.5", 10.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFFmpegTime(tt.line)
			if got != tt.want {
				t.Errorf("parseFFmpegTime(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestScanFFmpegLines(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		atEOF   bool
		wantAdv int
		wantTok string
	}{
		{"newline split", "hello\nworld", false, 6, "hello"},
		{"carriage return split", "hello\rworld", false, 6, "hello"},
		{"at EOF with data", "hello", true, 5, "hello"},
		{"at EOF no data", "", true, 0, ""},
		{"need more data", "hello", false, 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adv, tok, err := scanFFmpegLines([]byte(tt.data), tt.atEOF)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if adv != tt.wantAdv {
				t.Errorf("advance: got %d, want %d", adv, tt.wantAdv)
			}
			if tok == nil && tt.wantTok != "" {
				t.Errorf("token: got nil, want %q", tt.wantTok)
			} else if tok != nil && string(tok) != tt.wantTok {
				t.Errorf("token: got %q, want %q", string(tok), tt.wantTok)
			}
		})
	}
}

func TestCappedBuffer(t *testing.T) {
	buf := &cappedBuffer{maxSize: 20, keepSize: 10}

	// Write small data
	n, err := buf.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if buf.String() != "hello" {
		t.Errorf("got %q, want %q", buf.String(), "hello")
	}

	// Write more to exceed maxSize
	buf.Write([]byte("1234567890abcdef")) // Total 21 bytes > maxSize 20
	s := buf.String()
	if len(s) != 10 {
		t.Errorf("after truncation: len=%d, want 10 (keepSize)", len(s))
	}
}

func TestCappedBuffer_ExactMax(t *testing.T) {
	buf := &cappedBuffer{maxSize: 10, keepSize: 5}
	buf.Write([]byte("1234567890"))
	if buf.String() != "1234567890" {
		t.Errorf("at exactly maxSize: got %q", buf.String())
	}
	// One more byte triggers truncation
	buf.Write([]byte("A"))
	if len(buf.String()) != 5 {
		t.Errorf("after exceeding maxSize: len=%d, want 5", len(buf.String()))
	}
}

func TestDeriveFFprobePath(t *testing.T) {
	tests := []struct {
		name       string
		ffmpegPath string
		want       string
	}{
		{"empty", "", "ffprobe"},
		{"bare ffmpeg", "ffmpeg", "ffprobe"},
		// Windows paths only since this is a Windows-only project
		{"windows absolute path", `C:\tools\ffmpeg.exe`, `C:\tools\ffprobe.exe`},
		{"windows ffmpeg with suffix", `D:\bin\ffmpeg-6.0.exe`, `D:\bin\ffprobe-6.0.exe`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveFFprobePath(tt.ffmpegPath)
			if got != tt.want {
				t.Errorf("deriveFFprobePath(%q) = %q, want %q", tt.ffmpegPath, got, tt.want)
			}
		})
	}
}

func TestBuildConcatList(t *testing.T) {
	paths := []string{
		`C:\temp\seg_0.mp4`,
		`C:\temp\seg_1.mp4`,
	}
	got := buildConcatList(paths)
	want := "file 'C:/temp/seg_0.mp4'\nfile 'C:/temp/seg_1.mp4'\n"
	if got != want {
		t.Errorf("buildConcatList:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestBuildConcatList_EscapesQuotes pins the CORRECT ffconcat escaping for
// an apostrophe: close the quote, an escaped literal quote OUTSIDE any
// quoting, then reopen the quote (close-backslash-quote-quote) — NOT a
// backslash directly before the quote (`\'`), which is not a valid
// ffconcat escape sequence and breaks parsing (a pre-existing bug fixed
// here).
func TestBuildConcatList_EscapesQuotes(t *testing.T) {
	paths := []string{`C:\temp\it's a file.mp4`}
	got := buildConcatList(paths)
	want := "file 'C:/temp/it'\\''s a file.mp4'\n"
	if got != want {
		t.Errorf("buildConcatList with quote:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestBuildConcatList_ApostropheProducesValidFFConcatSyntax is the I8
// regression test with an explicit apostrophe path, asserting the exact
// close-escape-reopen shape ffconcat requires rather than just diffing
// against a hardcoded want string — a mutation back to the old `\'`
// escaping must fail this.
func TestBuildConcatList_ApostropheProducesValidFFConcatSyntax(t *testing.T) {
	paths := []string{`/tmp/Wendy's Stream - part1.mp4`}
	got := buildConcatList(paths)

	if !strings.Contains(got, `'\''`) {
		t.Fatalf("output does not contain the valid ffconcat escape sequence `'\\''`: %q", got)
	}
	want := "file '/tmp/Wendy'\\''s Stream - part1.mp4'\n"
	if got != want {
		t.Errorf("buildConcatList with apostrophe:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBuildArgs_CopyMode(t *testing.T) {
	logger := &testLogger{}
	m := NewMuxer("ffmpeg", logger)

	args := m.buildArgs("video.mp4", "audio.mp4", "output.mp4", nil, false)

	// Should contain -c copy
	found := false
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && args[i+1] == "copy" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected -c copy in args: %v", args)
	}

	// Should have -y, both -i, output, -movflags faststart
	checkContains(t, args, "-y")
	checkContains(t, args, "video.mp4")
	checkContains(t, args, "audio.mp4")
	checkContains(t, args, "output.mp4")
}

func TestBuildArgs_EncodeMode(t *testing.T) {
	logger := &testLogger{}
	m := NewMuxer("ffmpeg", logger)

	opts := &TrimOptions{
		TrimStartOffset: 10.5,
		TrimDuration:    30.0,
		CRF:             18,
		UsePreciseTrim:  true,
	}
	args := m.buildArgs("video.mp4", "", "output.mp4", opts, true)

	checkContains(t, args, "-ss")
	checkContains(t, args, "10.500")
	checkContains(t, args, "-t")
	checkContains(t, args, "30.000")
	checkContains(t, args, "-c:v")
	checkContains(t, args, "libx264")
	checkContains(t, args, "-crf")
	checkContains(t, args, "18")
}

func TestBuildArgs_ABRMode(t *testing.T) {
	logger := &testLogger{}
	m := NewMuxer("ffmpeg", logger)

	opts := &TrimOptions{
		TrimStartOffset: 5.0,
		TrimDuration:    20.0,
		VideoBitrate:    5000,
		AudioBitrate:    128,
		UsePreciseTrim:  true,
	}
	args := m.buildArgs("video.mp4", "audio.mp4", "output.mp4", opts, true)

	checkContains(t, args, "-b:v")
	checkContains(t, args, "5000k")
	checkContains(t, args, "-b:a")
	checkContains(t, args, "128k")
}

func TestCleanupFailedMux_PreservesOnCtxCancel(t *testing.T) {
	// Create a fake "partial output" file
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "partial.mp4")
	if err := os.WriteFile(outPath, []byte("partial data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cancelled parent ctx: cleanup should NOT remove the file
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cleanupFailedMux(ctx, errors.New("ffmpeg killed"), outPath)

	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("partial output was removed despite ctx cancel: %v", err)
	}
}

func TestCleanupFailedMux_RemovesOnRealError(t *testing.T) {
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "broken.mp4")
	if err := os.WriteFile(outPath, []byte("broken data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh ctx (not cancelled) + real error: file SHOULD be removed
	cleanupFailedMux(t.Context(), errors.New("ffmpeg: invalid args"), outPath)

	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("broken output should have been removed, stat err=%v", err)
	}
}

func TestCleanupFailedMux_NoopOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "good.mp4")
	if err := os.WriteFile(outPath, []byte("good data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// err == nil: file should be left alone
	cleanupFailedMux(t.Context(), nil, outPath)

	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("successful mux output was removed: %v", err)
	}
}

func TestHasTrim(t *testing.T) {
	if hasTrim(nil) {
		t.Error("nil opts should return false")
	}
	if hasTrim(&TrimOptions{}) {
		t.Error("zero opts should return false")
	}
	if !hasTrim(&TrimOptions{TrimStartOffset: 1.0}) {
		t.Error("non-zero offset should return true")
	}
	if !hasTrim(&TrimOptions{TrimDuration: 5.0}) {
		t.Error("non-zero duration should return true")
	}
}

// TestFFmpegPathArg pins ENGINE-12 (report #48): Go opens a >260-character
// path through \\?\, FFmpeg and ffprobe receive the raw string and fail on a
// Windows host without the LongPathsEnabled policy. Short paths are untouched
// so the common case carries no risk at all.
//
// Mutants, one per row:
//   - prefixing unconditionally: every ordinary path grows a \\?\ FFmpeg may
//     not parse.
//   - never prefixing: the long path reaches FFmpeg raw, which is the bug.
//   - prefixing on non-Windows: POSIX paths are corrupted.
//
// Do not add t.Parallel(): mutates the package-level ffmpegPathOS seam (see
// the rule on SegmentTimeout in downloader.go).
func TestFFmpegPathArg(t *testing.T) {
	long := `C:\out\` + strings.Repeat("a", 300) + ".mp4"
	short := `C:\out\clip.mp4`

	prev := ffmpegPathOS
	t.Cleanup(func() { ffmpegPathOS = prev })

	ffmpegPathOS = "windows"
	if got := ffmpegPathArg(short); got != short {
		t.Errorf("ffmpegPathArg(short) = %q, want it unchanged", got)
	}
	if got := ffmpegPathArg(long); !strings.HasPrefix(got, `\\?\`) {
		t.Errorf("ffmpegPathArg(long) = %q, want a \\\\?\\ prefix", got[:12])
	}
	if got := ffmpegPathArg(`\\?\` + long); strings.HasPrefix(got, `\\?\\\?\`) {
		t.Error("ffmpegPathArg double-prefixed an already-prefixed path")
	}

	ffmpegPathOS = "linux"
	if got := ffmpegPathArg(long); got != long {
		t.Errorf("ffmpegPathArg(long) on linux = %q, want it unchanged", got)
	}
}

// utf16Len is what Windows counts against MAX_PATH. Deliberately computed here
// rather than borrowed from the implementation, so a production switch back to
// len() (UTF-8 bytes) is measured against an independent yardstick.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// hostAbsPath builds a path that is already absolute ON THE RUNNING OS, so
// filepath.Abs leaves it alone and the UTF-16 length a row claims is the
// length ffmpegPathArg actually measures — on the Windows dev box and on the
// Linux CI runner alike. (The Windows branch is exercised everywhere via the
// ffmpegPathOS seam, and what the prefixed result looks like for a POSIX root
// does not matter: every assertion here is "prefixed" or "not prefixed".)
func hostAbsPath(t *testing.T, tail string) string {
	t.Helper()
	root := `C:\staging\`
	if runtime.GOOS != "windows" {
		root = "/staging/"
	}
	p := root + tail
	abs, err := filepath.Abs(p)
	if err != nil || abs != p {
		t.Fatalf("fixture %q is not already absolute on %s (Abs = %q, err = %v)", p, runtime.GOOS, abs, err)
	}
	return p
}

// TestFFmpegPathArgMeasuresResolvedUTF16Length pins the two halves of the
// measurement that the first cut of ENGINE-12 got wrong. MAX_PATH counts
// UTF-16 code units, not UTF-8 bytes, and the string FFmpeg has to open is the
// RESOLVED path, not the argument as written:
//
//   - a Japanese title (three bytes per unit) or an emoji-laden one (four
//     bytes per two units) crosses 260 BYTES while sitting nowhere near 260
//     units, and used to be rewritten for nothing — which falsified the row's
//     whole containment claim on this archiver's characteristic filenames;
//   - a relative path shorter than 260 whose absolute form is longer used to
//     escape the prefix it needs.
//
// Mutants, one per group:
//   - measuring len(p) (UTF-8 bytes): the CJK and emoji rows gain a prefix.
//   - measuring before filepath.Abs: the relative row loses its prefix.
//
// Do not add t.Parallel(): mutates the package-level ffmpegPathOS seam.
func TestFFmpegPathArgMeasuresResolvedUTF16Length(t *testing.T) {
	prev := ffmpegPathOS
	t.Cleanup(func() { ffmpegPathOS = prev })
	ffmpegPathOS = "windows"

	// 40x 日本語 = 120 units / 360 bytes; with root + suffix: 140 units on
	// Windows, 138 on a POSIX root — both far below 260 units and far above
	// 260 bytes, which is the whole point of the row.
	cjk := hostAbsPath(t, strings.Repeat("日本語", 40)+".video.ts")
	emoji := hostAbsPath(t, strings.Repeat("🎬", 100)+".video.ts")

	pad := func(units int) string {
		root := `C:\`
		if runtime.GOOS != "windows" {
			root = "/"
		}
		return root + strings.Repeat("a", units-len(root))
	}

	for _, tc := range []struct {
		name          string
		path          string
		wantPrefix    bool
		wantUnder     bool // the row claims "under 260 UTF-16 units"
		wantBytesOver bool // …while being over 260 UTF-8 BYTES
	}{
		{"CJK title: 260+ bytes, far under 260 units", cjk, false, true, true},
		{"emoji title: 260+ bytes, under 260 units", emoji, false, true, true},
		{"259 UTF-16 units", pad(259), false, true, false},
		{"260 UTF-16 units", pad(260), true, false, false},
		{"300 ASCII characters", pad(300), true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture must really have the shape the row names, or the
			// assertion below proves nothing.
			if under := utf16Len(tc.path) < 260; under != tc.wantUnder {
				t.Fatalf("fixture has %d UTF-16 units (%d bytes); row claims under-260 = %v",
					utf16Len(tc.path), len(tc.path), tc.wantUnder)
			}
			if tc.wantBytesOver && len(tc.path) < 260 {
				t.Fatalf("fixture %q is %d bytes — under 260 in BOTH measures, so it cannot discriminate units from bytes",
					tc.path, len(tc.path))
			}
			got := ffmpegPathArg(tc.path)
			if gotPrefix := strings.HasPrefix(got, `\\?\`); gotPrefix != tc.wantPrefix {
				t.Fatalf("ffmpegPathArg prefixed = %v, want %v (%d units, %d bytes)",
					gotPrefix, tc.wantPrefix, utf16Len(tc.path), len(tc.path))
			}
			if !tc.wantPrefix && got != tc.path {
				t.Fatalf("ffmpegPathArg rewrote a path it must leave alone: %q", got)
			}
		})
	}

	// A RELATIVE path under the limit whose resolved form is over it: the
	// string FFmpeg opens is the resolved one, so this needs the prefix.
	rel := strings.Repeat("r", 255) + ".mp4"
	if utf16Len(rel) >= 260 {
		t.Fatalf("relative fixture is %d units, want under 260", utf16Len(rel))
	}
	absRel, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("Abs(rel): %v", err)
	}
	if utf16Len(absRel) < 260 {
		t.Skipf("working directory too short to build the relative-path row (%d units resolved)", utf16Len(absRel))
	}
	if got := ffmpegPathArg(rel); !strings.HasPrefix(got, `\\?\`) {
		t.Fatalf("ffmpegPathArg(relative, resolves to %d units) = %q, want the prefix", utf16Len(absRel), got[:min(12, len(got))])
	}
}

// TestEveryFfmpegArgvBuilderAppliesThePathSeam is the differential half of
// ENGINE-12, over EVERY argv this package hands to FFmpeg — not just the
// archive mux. The row names FFmpeg's path ARGUMENTS in the plural, and the
// two-pass encode and the Trim/concat builders carry the same user-composed
// output path (output dir + channel subdir + title) as the mux does.
//
// For each builder: on the non-Windows OS value the argv is byte-identical to
// what it was before the seam existed, and on Windows exactly that builder's
// path arguments carry the prefix while every other argument and the argv
// length are untouched.
//
// Mutants: leaving any ONE builder's path raw (that builder's row reports the
// unprefixed argument), and applying the seam regardless of ffmpegPathOS (the
// non-Windows argv changes).
//
// Do not add t.Parallel(): mutates the package-level ffmpegPathOS seam.
func TestEveryFfmpegArgvBuilderAppliesThePathSeam(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	stem := strings.Repeat("b", 300)
	video := hostAbsPath(t, stem+".video.ts")
	audio := hostAbsPath(t, stem+".audio.m4a")
	output := hostAbsPath(t, stem+".mp4")
	// The concat list lives in an os.MkdirTemp directory, never in the user's
	// output tree, so it is deliberately NOT a long path here — and it is
	// deliberately not wrapped in production either (see buildConcatArgs).
	list := filepath.Join(t.TempDir(), "concat.txt")
	passLog := filepath.Join(t.TempDir(), "passlog")

	opts := &TrimOptions{
		TrimStartOffset: 1.5,
		TrimDuration:    30,
		VideoBitrate:    5000,
		AudioBitrate:    128,
		UsePreciseTrim:  true,
		TwoPass:         true,
	}
	seg := TrimSegmentInput{InputPath: video, StartTime: 1.5, Duration: 30, NeedScale: true}

	builders := []struct {
		name  string
		build func() []string
		paths []string
	}{
		{"buildArgs", func() []string { return m.buildArgs(video, audio, output, opts, true) },
			[]string{video, audio, output}},
		{"buildTwoPassArgs/pass1", func() []string { a1, _ := m.buildTwoPassArgs(video, audio, output, passLog, opts); return a1 },
			[]string{video}},
		{"buildTwoPassArgs/pass2", func() []string { _, a2 := m.buildTwoPassArgs(video, audio, output, passLog, opts); return a2 },
			[]string{video, audio, output}},
		{"buildTrimSegmentArgs", func() []string { return m.buildTrimSegmentArgs(seg, output, 1920, 1080, 30, 18, 128) },
			[]string{video, output}},
		{"buildConcatArgs", func() []string { return m.buildConcatArgs(list, output) },
			[]string{output}},
	}

	prev := ffmpegPathOS
	t.Cleanup(func() { ffmpegPathOS = prev })

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			ffmpegPathOS = "linux"
			posix := b.build()
			for _, p := range b.paths {
				if !slices.Contains(posix, p) {
					t.Fatalf("non-Windows argv rewrote %q: %v", p, posix)
				}
			}

			ffmpegPathOS = "windows"
			win := b.build()
			if len(win) != len(posix) {
				t.Fatalf("argv length changed with the prefix: %d vs %d", len(win), len(posix))
			}
			prefixed := 0
			for i, raw := range posix {
				if slices.Contains(b.paths, raw) {
					if !strings.HasPrefix(win[i], `\\?\`) {
						t.Errorf("argv[%d] = %q, want the extended-length prefix", i, win[i])
						continue
					}
					if !strings.HasSuffix(win[i], filepath.Base(raw)) {
						t.Errorf("argv[%d] = %q, want it to still end in the original path", i, win[i])
					}
					prefixed++
					continue
				}
				if win[i] != raw {
					t.Errorf("argv[%d] = %q, want the non-path argument %q untouched", i, win[i], raw)
				}
			}
			if prefixed == 0 {
				t.Fatalf("no path argument was prefixed; argv = %v", win)
			}
		})
	}
}

// testLogger is a simple logger for tests.
type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any) {}
func (l *testLogger) Info(msg string, args ...any)  {}
func (l *testLogger) Warn(msg string, args ...any)  {}
func (l *testLogger) Error(msg string, args ...any) {}

func checkContains(t *testing.T, args []string, want string) {
	t.Helper()
	if slices.Contains(args, want) {
		return
	}
	t.Errorf("args %v does not contain %q", args, want)
}
