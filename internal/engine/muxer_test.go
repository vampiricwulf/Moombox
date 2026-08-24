package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	ctx, cancel := context.WithCancel(context.Background())
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
	cleanupFailedMux(context.Background(), errors.New("ffmpeg: invalid args"), outPath)

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
	cleanupFailedMux(context.Background(), nil, outPath)

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
