package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// argsLogger keeps the argv each FFmpeg run logs ahead of starting it.
type argsLogger struct {
	testLogger
	mu   sync.Mutex
	args []string
}

func (l *argsLogger) Debug(msg string, args ...any) {
	if msg != "ffmpeg" || len(args) < 2 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.args = append(l.args, args[1].(string))
}

// TestCopyProgressArgvIsTheCopyArgvWithThreeOptionsAhead pins the argv of
// both shapes of the archive copy, read off the Debug line mux logs before it
// starts FFmpeg (the binary named here does not exist, so nothing runs): a
// copy with no callback is run with exactly the arguments it always was, and
// one with a callback gets `-nostats -progress pipe:1` ahead of the same.
//
// Mutants: mux taking the progress runner whenever opts is nil (the plain
// copy gains the three options); withCopyProgress appending after the output
// path (the suffix no longer equals the plain argv).
func TestCopyProgressArgvIsTheCopyArgvWithThreeOptionsAhead(t *testing.T) {
	dir := t.TempDir()
	video, audio := filepath.Join(dir, "v.mp4"), filepath.Join(dir, "a.m4a")
	for _, p := range []string{video, audio} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := &argsLogger{}
	m := NewMuxer(filepath.Join(dir, "no-such-ffmpeg"), log)
	out := filepath.Join(dir, "out.mp4")
	off := InputOffsets{Audio: 1.5}

	if err := m.MuxCopyArchive(context.Background(), video, audio, out, off, "1.777778", ""); err == nil {
		t.Fatal("MuxCopyArchive ran a binary that does not exist")
	}
	if err := m.MuxCopyArchiveWithProgress(context.Background(), video, audio, out, off, "1.777778", "", nil); err == nil {
		t.Fatal("MuxCopyArchiveWithProgress(nil) ran a binary that does not exist")
	}
	if err := m.MuxCopyArchiveWithProgress(context.Background(), video, audio, out, off, "1.777778", "", func(CopyProgress) {}); err == nil {
		t.Fatal("MuxCopyArchiveWithProgress ran a binary that does not exist")
	}
	if len(log.args) != 3 {
		t.Fatalf("logged %d argvs, want 3: %q", len(log.args), log.args)
	}
	// The output is a temporary name of each run's own: compare without it.
	body := func(argv string) string {
		i := strings.LastIndex(argv, " ")
		return argv[:i]
	}
	want := "-y -i " + video + " -itsoffset 1.500000 -i " + audio + " -c copy -movflags faststart -aspect 1.777778"
	if got := body(log.args[0]); got != want {
		t.Errorf("plain copy argv:\n got %s\nwant %s", got, want)
	}
	if got := body(log.args[1]); got != want {
		t.Errorf("nil-callback copy argv:\n got %s\nwant %s", got, want)
	}
	if got := body(log.args[2]); got != "-nostats -progress pipe:1 "+want {
		t.Errorf("progress copy argv:\n got %s\nwant -nostats -progress pipe:1 %s", got, want)
	}
}

func TestCopyProgressPercent(t *testing.T) {
	cases := []struct {
		out, in int64
		want    int
	}{
		{0, 1000, 0}, {420, 1000, 42}, {999, 1000, 99}, {1000, 1000, 99}, {1500, 1000, 99},
		{500, 0, 0}, {-1, 1000, 0},
	}
	for _, c := range cases {
		if got := (CopyProgress{OutputBytes: c.out, InputBytes: c.in}).Percent(); got != c.want {
			t.Errorf("Percent(%d of %d) = %d, want %d", c.out, c.in, got, c.want)
		}
	}
}

// TestPumpCopyProgressReportsBlocksThenTheStall feeds pumpCopyProgress the
// -progress stream by hand: each closed block is one report with FFmpeg's
// total_size, a block whose size is still N/A reports nothing new, a pipe
// that goes quiet after a report is reported as finalizing, and a report
// after that clears it again.
//
// Mutants: reporting on the stall timer before any block (a leading
// Finalizing); not clearing Finalizing on the next block.
func TestPumpCopyProgressReportsBlocksThenTheStall(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	pr, pw := io.Pipe()
	got := make(chan CopyProgress, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.pumpCopyProgress(pr, 1000, 40*time.Millisecond, func(p CopyProgress) { got <- p })
	}()
	next := func() CopyProgress {
		t.Helper()
		select {
		case p := <-got:
			return p
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return CopyProgress{}
		}
	}

	// Quiet before the first block: longer than the stall, and no report.
	time.Sleep(120 * time.Millisecond)
	io.WriteString(pw, "frame=0\ntotal_size=N/A\nout_time_us=0\nprogress=continue\n")
	io.WriteString(pw, "frame=10\ntotal_size=420\nout_time_us=1000\nprogress=continue\n")
	if p := next(); p.OutputBytes != 420 || p.InputBytes != 1000 || p.Finalizing || p.Percent() != 42 {
		t.Fatalf("first report = %+v, want 420 of 1000, not finalizing", p)
	}
	if p := next(); !p.Finalizing || p.OutputBytes != 420 {
		t.Fatalf("after a quiet pipe: %+v, want the same size, finalizing", p)
	}
	io.WriteString(pw, "garbage line\ntotal_size=980\nprogress=continue\n")
	for {
		p := next()
		if p.Finalizing {
			continue // a second stall tick ahead of the write
		}
		if p.OutputBytes != 980 || p.Percent() != 98 {
			t.Fatalf("report after the stall = %+v, want 980 of 1000", p)
		}
		break
	}
	// FFmpeg's last report comes once the trailer is written.
	io.WriteString(pw, "total_size=1000\nprogress=end\n")
	for {
		p := next()
		if p.OutputBytes != 1000 {
			continue // a stall tick ahead of the write
		}
		if !p.Finalizing {
			t.Fatalf("the end report = %+v, want finalizing", p)
		}
		break
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pumpCopyProgress did not return at the end of the stream")
	}
}

// TestPumpCopyProgressSurvivesAPanickingCallback: a callback that panics is
// dropped after its first call, and the pump still reads the stream to its
// end and returns, so the caller's Wait runs and FFmpeg is never left blocked
// on a full pipe.
//
// Mutant: calling fn with no recover (the panic leaves the pump, and the
// writer below blocks once the pipe is no longer read).
func TestPumpCopyProgressSurvivesAPanickingCallback(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	pr, pw := io.Pipe()
	calls := 0
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		m.pumpCopyProgress(pr, 1000, time.Hour, func(CopyProgress) {
			calls++
			panic("callback broke")
		})
	}()
	wrote := make(chan error, 1)
	go func() {
		var err error
		// Far more blocks than the report buffer holds.
		for i := 1; i <= 200 && err == nil; i++ {
			_, err = fmt.Fprintf(pw, "total_size=%d\nprogress=continue\n", i)
		}
		pw.Close()
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the stream was not read to its end: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pump stopped reading after the callback panicked")
	}
	select {
	case rec := <-done:
		if rec != nil {
			t.Fatalf("the callback's panic left the pump: %v", rec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pumpCopyProgress did not return at the end of the stream")
	}
	if calls != 1 {
		t.Errorf("the callback ran %d times, want once", calls)
	}
}

// TestMuxCopyArchiveWithProgressWritesTheSameArchive runs the real FFmpeg:
// the copy with a callback reports what it wrote, and leaves an archive whose
// streams hash the same as the plain copy's. Skipped where FFmpeg is not
// installed.
func TestMuxCopyArchiveWithProgressWritesTheSameArchive(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	video, audio := filepath.Join(dir, "v.mp4"), filepath.Join(dir, "a.m4a")
	gen := func(out string, args ...string) {
		t.Helper()
		argv := append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...), out)
		if b, err := exec.Command(ffmpegPath, argv...).CombinedOutput(); err != nil {
			t.Skipf("this FFmpeg cannot make the fixture: %v\n%s", err, b)
		}
	}
	gen(video, "-f", "lavfi", "-i", "testsrc=duration=20:size=640x360:rate=30", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	gen(audio, "-f", "lavfi", "-i", "sine=f=440:d=20", "-c:a", "aac")

	m := NewMuxer(ffmpegPath, &testLogger{})
	plain, withFn := filepath.Join(dir, "plain.mp4"), filepath.Join(dir, "progress.mp4")
	if err := m.MuxCopyArchive(t.Context(), video, audio, plain, InputOffsets{}, "", ""); err != nil {
		t.Fatalf("MuxCopyArchive: %v", err)
	}
	var reports []CopyProgress
	if err := m.MuxCopyArchiveWithProgress(t.Context(), video, audio, withFn, InputOffsets{}, "", "",
		func(p CopyProgress) { reports = append(reports, p) }); err != nil {
		t.Fatalf("MuxCopyArchiveWithProgress: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("the copy reported nothing")
	}
	want := inputBytes(video, audio)
	lastReport := reports[len(reports)-1]
	if lastReport.InputBytes != want || lastReport.OutputBytes <= 0 || !lastReport.Finalizing {
		t.Errorf("last report = %+v, want output bytes against %d input bytes, finalizing", lastReport, want)
	}
	if !slices.IsSortedFunc(reports, func(a, b CopyProgress) int { return int(a.OutputBytes - b.OutputBytes) }) {
		t.Errorf("output bytes went backwards: %+v", reports)
	}
	hash := func(p string) string {
		t.Helper()
		b, err := exec.Command(ffmpegPath, "-v", "error", "-i", p, "-map", "0", "-c", "copy", "-f", "streamhash", "-hash", "md5", "-").Output()
		if err != nil {
			t.Fatalf("streamhash %s: %v", p, err)
		}
		return string(b)
	}
	if a, b := hash(plain), hash(withFn); a != b || a == "" {
		t.Errorf("stream hashes differ:\nplain:\n%s\nwith progress:\n%s", a, b)
	}
	pi, _ := os.Stat(plain)
	wi, _ := os.Stat(withFn)
	if pi == nil || wi == nil || pi.Size() != wi.Size() {
		t.Errorf("archive sizes differ: %v vs %v", pi, wi)
	}

	// A failed copy's error still carries FFmpeg's own words from stderr.
	bad := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(bad, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.MuxCopyArchiveWithProgress(t.Context(), bad, "", filepath.Join(dir, "never.mp4"), InputOffsets{}, "", "", func(CopyProgress) {})
	if err == nil || !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("failed copy's error = %v, want FFmpeg's stderr in it", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "never.mp4")); statErr == nil {
		t.Error("a failed copy left its output")
	}
}
